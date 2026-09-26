// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

const (
	asgRun     = "11111111-1111-1111-1111-111111111111"
	asgAttempt = "22222222-2222-2222-2222-222222222222"
)

// fakeAssignments models the namespace-scoped read: a row is returned only for
// the namespace (and, when given, the run) it belongs to -- as the SQL
// predicate does -- and every call is recorded.
type fakeAssignments struct {
	rows  map[string]store.AttemptAssignmentRow // attempt id -> row
	ns    map[string]string                     // attempt id -> namespace
	calls []string
}

func (f *fakeAssignments) GetAttemptAssignment(_ context.Context, ns, runID, attemptID string) (store.AttemptAssignmentRow, error) {
	f.calls = append(f.calls, ns+"/"+runID+"/"+attemptID)
	row, ok := f.rows[attemptID]
	if !ok || f.ns[attemptID] != ns || (runID != "" && row.RunID != runID) {
		return store.AttemptAssignmentRow{}, store.ErrNotFound
	}
	return row, nil
}

func newAssignmentAPI(t *testing.T) (http.Handler, *restDB, *fakeAssignments) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	db.addKey("nx_other_key", "team-other", "other", "admin")
	db.addKey("nx_worker_key", "team-ml", "agent", "worker")
	db.addRun("team-ml", asgRun, "FAILED", map[string]string{"load": "FAILED"})
	mi := int32(3)
	parent := "load"
	fa := &fakeAssignments{
		rows: map[string]store.AttemptAssignmentRow{asgAttempt: {
			AttemptID: asgAttempt, RunID: asgRun, NodeID: "load[3]", AttemptNumber: 2, State: "FAILED",
			IdempotencyKey: "idem-abc",
			SpecJSON: []byte(`{"id":"load[3]","task_ref":"etl.load","version":1,` +
				`"timeouts":{"start_to_close_ms":"600000"},` +
				`"runtime":{"executor":"subprocess","command":["sh","-c","echo"],` +
				`"secrets":[{"name":"db","ref":"vault://kv/db","value":"hunter2"},{"name":"tls.key","ref":"file:///k","inject":"file"}]}}`),
			InputRefsJSON: []byte(`{"summary":{"inline":"eyJ0b3RhbCI6MTM1fQ==","content_type":"application/json"},` +
				`"big":{"blob":{"bucket":"b","key":"team-ml/ab","sha256":"ab"},"size_bytes":"900000"}}`),
			ParentNodeID: &parent, MapIndex: &mi, SpecName: "etl", SpecVersion: 4,
		}},
		ns: map[string]string{asgAttempt: "team-ml"},
	}
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithAssignmentReader(fa).WithInlineThreshold(4096)
	return api.Routes(), db, fa
}

func getWith(t *testing.T, h http.Handler, path, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAttemptAssignmentReturnsInputsAndSpecWithoutCredentials(t *testing.T) {
	h, _, _ := newAssignmentAPI(t)
	for _, run := range []string{asgRun, "-"} {
		rec := getWith(t, h, "/v1/namespaces/team-ml/runs/"+run+"/attempts/"+asgAttempt+"/assignment", readerKey)
		if rec.Code != http.StatusOK {
			t.Fatalf("run %s: status %d body=%s", run, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		for _, forbidden := range []string{"hunter2", "lease_token\":\"", "\"value\""} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("response leaks %q: %s", forbidden, body)
			}
		}
		var got struct {
			Assignment struct {
				NodeID         string                     `json:"node_id"`
				MapIndex       int                        `json:"map_index"`
				IdempotencyKey string                     `json:"idempotency_key"`
				Inputs         map[string]json.RawMessage `json:"inputs"`
				Spec           map[string]any             `json:"spec"`
			} `json:"assignment"`
			Secrets     []map[string]string `json:"secrets"`
			Encoding    string              `json:"encoding"`
			ResultStore struct {
				Inline int64 `json:"inline_threshold_bytes"`
			} `json:"result_store"`
			Omitted  []string       `json:"omitted"`
			Workflow map[string]any `json:"workflow"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		a := got.Assignment
		if a.NodeID != "load[3]" || a.MapIndex != 3 || a.IdempotencyKey != "idem-abc" {
			t.Errorf("assignment identity = %+v", a)
		}
		if string(a.Inputs["summary"]) != `{"inline":"eyJ0b3RhbCI6MTM1fQ==","content_type":"application/json"}` {
			t.Errorf("inline input not passed through: %s", a.Inputs["summary"])
		}
		if !strings.Contains(string(a.Inputs["big"]), `"blob"`) {
			t.Errorf("blob input must stay a reference: %s", a.Inputs["big"])
		}
		if len(got.Secrets) != 2 || got.Secrets[0]["name"] != "db" || got.Secrets[0]["ref"] != "vault://kv/db" || got.Secrets[0]["value"] != "" ||
			got.Secrets[1]["inject"] != "file" || got.Secrets[0]["inject"] != "" {
			t.Errorf("secrets = %v, want the reference only", got.Secrets)
		}
		if got.Encoding != "protojson" || got.ResultStore.Inline != 4096 || got.Workflow["name"] != "etl" {
			t.Errorf("envelope = %+v", got)
		}
		if len(got.Omitted) == 0 || got.Omitted[0] != "lease_token" {
			t.Errorf("omitted = %v", got.Omitted)
		}
	}
}

// F101: the namespace is checked BEFORE the read. A principal of another
// namespace naming this run through its own namespace gets 404 and the reader
// is never asked about the run; through this namespace's path it gets 403.
func TestAttemptAssignmentIsNamespaceScoped(t *testing.T) {
	h, _, fa := newAssignmentAPI(t)
	rec := getWith(t, h, "/v1/namespaces/team-other/runs/"+asgRun+"/attempts/"+asgAttempt+"/assignment", "nx_other_key")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-namespace run: status %d, want 404", rec.Code)
	}
	if len(fa.calls) != 0 {
		t.Fatalf("the assignment was read before the run's namespace was checked: %v", fa.calls)
	}
	// The wildcard reaches the reader, which scopes by namespace: still 404.
	rec = getWith(t, h, "/v1/namespaces/team-other/runs/-/attempts/"+asgAttempt+"/assignment", "nx_other_key")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-namespace wildcard: status %d, want 404", rec.Code)
	}
	if rec := getWith(t, h, "/v1/namespaces/team-ml/runs/-/attempts/"+asgAttempt+"/assignment", "nx_other_key"); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign namespace in the path: status %d, want 403", rec.Code)
	}
}

func TestAttemptAssignmentNotFoundCases(t *testing.T) {
	h, _, fa := newAssignmentAPI(t)
	for name, path := range map[string]string{
		"malformed attempt": "/v1/namespaces/team-ml/runs/-/attempts/not-a-uuid/assignment",
		"malformed run":     "/v1/namespaces/team-ml/runs/nope/attempts/" + asgAttempt + "/assignment",
		"unknown attempt":   "/v1/namespaces/team-ml/runs/-/attempts/33333333-3333-3333-3333-333333333333/assignment",
	} {
		if rec := getWith(t, h, path, adminKey); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rec.Code)
		}
	}
	// A run of this namespace that the attempt does not belong to.
	other, _ := fa.GetAttemptAssignment(context.Background(), "team-ml", "44444444-4444-4444-4444-444444444444", asgAttempt)
	if other.AttemptID != "" {
		t.Fatal("fake reader ignored the run predicate")
	}
}

func TestAttemptAssignmentPermissions(t *testing.T) {
	h, _, _ := newAssignmentAPI(t)
	path := "/v1/namespaces/team-ml/runs/-/attempts/" + asgAttempt + "/assignment"
	if rec := getWith(t, h, path, "nx_worker_key"); rec.Code != http.StatusForbidden {
		t.Errorf("a worker credential read an assignment: status %d", rec.Code)
	}
	if rec := getWith(t, h, path, "nx_no_such_key"); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: status %d, want 401", rec.Code)
	}
	want := []string{"runs.view", "tasks.view", "tasks.results.view"}
	got := routePermissions[assignmentRoute]
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("route permissions = %v, want %v", got, want)
	}
	// A role that can see runs and tasks but not results cannot read inputs.
	p := authn.Principal{Grants: map[string]bool{"runs.view": true, "tasks.view": true}}
	allowed := true
	for _, perm := range got {
		allowed = allowed && p.Allows(perm)
	}
	if allowed {
		t.Fatal("a principal without tasks.results.view would be able to read input values")
	}
}
