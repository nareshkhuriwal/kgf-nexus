// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// These tests drive the HTTP surface, because the Idempotency-Key contract is a
// property of the HTTP layer: which STATUS CODE a client sees is the whole point
// (200 replay vs 201 created vs 409 conflict), and asserting it at the store level
// would leave the mapping untested.

// ── doubles ───────────────────────────────────────────────────────────────

// restDB models api_keys and idempotency_keys, the two tables the gateway owns and
// the only ones these tests touch.
type restDB struct {
	mu sync.Mutex
	// keyHash -> principal fields
	keys map[string][4]string
	idem map[string]*idemRow
}

type idemRow struct {
	requestHash string
	response    string
}

func newRestDB() *restDB {
	return &restDB{keys: map[string][4]string{}, idem: map[string]*idemRow{}}
}

func (d *restDB) addKey(plaintext, ns, name, role string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys[store.HashAPIKey(plaintext)] = [4]string{"key-" + name, ns, name, role}
}

func (d *restDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(sql, "INSERT INTO idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		if _, exists := d.idem[k]; exists {
			return pgconn.NewCommandTag("INSERT 0 0"), nil
		}
		d.idem[k] = &idemRow{requestHash: args[2].(string), response: "null"}
		return pgconn.NewCommandTag("INSERT 0 1"), nil
	case strings.Contains(sql, "UPDATE idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		if row, ok := d.idem[k]; ok {
			row.response = args[2].(string)
		}
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(sql, "DELETE FROM idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		if row, ok := d.idem[k]; ok && row.response == "null" {
			delete(d.idem, k)
			return pgconn.NewCommandTag("DELETE 1"), nil
		}
		return pgconn.NewCommandTag("DELETE 0"), nil
	}
	return pgconn.NewCommandTag(""), fmt.Errorf("restDB: unhandled Exec: %s", sql)
}

func (d *restDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(sql, "FROM api_keys"):
		p, ok := d.keys[args[0].(string)]
		if !ok {
			return errRow{pgx.ErrNoRows}
		}
		return valueRow{[]any{p[0], p[1], p[2], p[3]}}
	case strings.Contains(sql, "FROM idempotency_keys"):
		k := args[0].(string) + "\x00" + args[1].(string)
		row, ok := d.idem[k]
		if !ok {
			return errRow{pgx.ErrNoRows}
		}
		return valueRow{[]any{row.requestHash, []byte(row.response)}}
	}
	return errRow{fmt.Errorf("restDB: unhandled QueryRow: %s", sql)}
}

func (d *restDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, fmt.Errorf("restDB: unhandled Query: %s", sql)
}

func (d *restDB) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, fmt.Errorf("restDB: transactions not modelled")
}

type errRow struct{ err error }

func (r errRow) Scan(dest ...any) error { return r.err }

type valueRow struct{ vals []any }

func (r valueRow) Scan(dest ...any) error {
	if len(dest) != len(r.vals) {
		return fmt.Errorf("scan wants %d destinations, row has %d", len(dest), len(r.vals))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = r.vals[i].(string)
		case *[]byte:
			*p = r.vals[i].([]byte)
		default:
			return fmt.Errorf("unsupported destination %T", d)
		}
	}
	return nil
}

// fakeWFM counts CreateRun calls, which is how "the work happened only once" is
// asserted: a replay that reaches the workflow-manager would create a second run.
type fakeWFM struct {
	mu       sync.Mutex
	creates  int
	nextID   int
	createFn func() (store.RunRow, bool, error)
}

func (f *fakeWFM) PutSpec(ctx context.Context, namespace, name, hash string, canonicalJSON []byte, createdBy string) (store.SpecRow, bool, error) {
	return store.SpecRow{
		ID: "spec-1", Namespace: namespace, Name: name, Version: 1, SpecHash: hash,
		CreatedBy: createdBy, CreatedAt: time.Now(),
	}, true, nil
}

func (f *fakeWFM) CreateRun(ctx context.Context, req CreateRunRequest) (store.RunRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createFn != nil {
		return f.createFn()
	}
	f.nextID++
	return store.RunRow{
		ID: fmt.Sprintf("run-%d", f.nextID), Namespace: req.Namespace,
		SpecID: "spec-1", SpecName: "train", SpecVersion: 1,
		State: "PENDING", TriggerKind: req.TriggerKind, CreatedAt: time.Now(),
	}, true, nil
}

func (f *fakeWFM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

func (f *fakeWFM) CancelRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return store.RunRow{ID: id, State: "CANCELLING"}, nil
}
func (f *fakeWFM) PauseRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return store.RunRow{ID: id, State: "PAUSING"}, nil
}
func (f *fakeWFM) ResumeRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return store.RunRow{ID: id, State: "RUNNING"}, nil
}
func (f *fakeWFM) RetryRun(ctx context.Context, ns, id string) (store.RunRow, error) {
	return store.RunRow{}, store.ErrRunStateConflict
}

type fakeRM struct{}

func (fakeRM) DrainWorker(ctx context.Context, ns, id string) error { return nil }
func (fakeRM) Capacity(ctx context.Context, ns string) (CapacityReport, error) {
	return CapacityReport{Namespace: ns, Workers: 2}, nil
}

const adminKey = "nx_admin_key"
const readerKey = "nx_reader_key"

func newTestAPI(t *testing.T) (http.Handler, *restDB, *fakeWFM) {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	db.addKey(readerKey, "team-ml", "dashboard", "reader")
	wfm := &fakeWFM{}
	api := New(db, authn.New(db), wfm, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return api.Routes(), db, wfm
}

func post(t *testing.T, h http.Handler, path, key, idemKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ── idempotency over HTTP ─────────────────────────────────────────────────

// TestCreateRunIdempotentReplay: the same key with the same body must return the
// CACHED response and must NOT create a second run.
func TestCreateRunIdempotentReplay(t *testing.T) {
	h, _, wfm := newTestAPI(t)
	body := `{"spec_name":"train","params":{"epochs":10}}`

	first := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201. body=%s", first.Code, first.Body)
	}

	second := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-1", body)
	if second.Code != http.StatusOK {
		t.Fatalf("replay: status %d, want 200. body=%s", second.Code, second.Body)
	}
	if second.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("a replayed response was not marked with Idempotency-Replayed")
	}
	if n := wfm.callCount(); n != 1 {
		t.Errorf("CreateRun was called %d times for one idempotency key; the work was done twice", n)
	}

	// The replay must be the SAME run, byte-for-byte: a client treating the retry as
	// the original call needs the original answer.
	var a, b map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	if a["id"] != b["id"] {
		t.Errorf("replay returned run %v, want the original %v", b["id"], a["id"])
	}
}

// TestCreateRunIdempotencyConflict is the 409 the task calls out: the same key with a
// DIFFERENT body catches a client bug instead of silently doing the wrong thing.
func TestCreateRunIdempotencyConflict(t *testing.T) {
	h, _, wfm := newTestAPI(t)

	first := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-2",
		`{"spec_name":"train","params":{"epochs":10}}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201. body=%s", first.Code, first.Body)
	}

	// Same key, different params — a retry loop whose body changed underneath it.
	conflict := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-2",
		`{"spec_name":"train","params":{"epochs":500}}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same key + different body: status %d, want 409. body=%s", conflict.Code, conflict.Body)
	}
	if !strings.Contains(conflict.Body.String(), "idempotency_key_reused") {
		t.Errorf("the 409 does not name the cause: %s", conflict.Body)
	}
	if n := wfm.callCount(); n != 1 {
		t.Errorf("CreateRun was called %d times; a conflicting request must not create anything", n)
	}
}

// TestCreateRunIdempotencyIgnoresFormatting: a client that re-serializes its request
// with different key order or whitespace is NOT making a different request, and
// 409-ing it would be a false positive that breaks well-behaved clients.
func TestCreateRunIdempotencyIgnoresFormatting(t *testing.T) {
	h, _, wfm := newTestAPI(t)

	if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-3",
		`{"spec_name":"train","params":{"epochs":10}}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create: status %d, want 201", rec.Code)
	}
	// Same content, reordered keys and extra whitespace.
	replay := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-3",
		"{\n  \"params\" : { \"epochs\" : 10 },\n  \"spec_name\" : \"train\"\n}")
	if replay.Code != http.StatusOK {
		t.Errorf("a semantically identical body returned %d, want 200 replay. body=%s", replay.Code, replay.Body)
	}
	if n := wfm.callCount(); n != 1 {
		t.Errorf("CreateRun called %d times, want 1", n)
	}
}

// TestCreateRunWithoutIdempotencyKey: the header is optional, and two unkeyed
// requests are two genuine runs.
func TestCreateRunWithoutIdempotencyKey(t *testing.T) {
	h, _, wfm := newTestAPI(t)
	body := `{"spec_name":"train"}`

	for i := 0; i < 2; i++ {
		if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", body); rec.Code != http.StatusCreated {
			t.Fatalf("create %d: status %d, want 201. body=%s", i, rec.Code, rec.Body)
		}
	}
	if n := wfm.callCount(); n != 2 {
		t.Errorf("CreateRun called %d times, want 2: unkeyed requests must not dedup", n)
	}
}

// TestFailedCreateReleasesIdempotencyClaim: without release, a transient failure would
// poison the key permanently and the client's correct retry could never succeed.
func TestFailedCreateReleasesIdempotencyClaim(t *testing.T) {
	h, db, wfm := newTestAPI(t)

	wfm.createFn = func() (store.RunRow, bool, error) {
		return store.RunRow{}, false, errors.New("database briefly unavailable")
	}
	if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-4",
		`{"spec_name":"train"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a failed creation returned %d, want 500", rec.Code)
	}

	db.mu.Lock()
	_, stillClaimed := db.idem["team-ml\x00client-key-4"]
	db.mu.Unlock()
	if stillClaimed {
		t.Error("the idempotency claim survived a failed creation; the key is poisoned and the client can never retry")
	}

	// The retry, once the dependency recovers, must succeed.
	wfm.createFn = nil
	if rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "client-key-4",
		`{"spec_name":"train"}`); rec.Code != http.StatusCreated {
		t.Errorf("the retry after recovery returned %d, want 201. body=%s", rec.Code, rec.Body)
	}
}

func TestCreateRunRequiresSpec(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a run with neither spec_id nor spec_name returned %d, want 400", rec.Code)
	}
}

func TestCreateRunRejectsMalformedJSON(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"spec_name":`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON returned %d, want 400", rec.Code)
	}
}

// ── spec submission ───────────────────────────────────────────────────────

func TestSubmitSpecAcceptsJSONAndYAML(t *testing.T) {
	h, _, _ := newTestAPI(t)

	jsonSpec := `{"api_version":"nexus.dev/v1","namespace":"team-ml","name":"train",
	  "tasks":[{"id":"a","task_ref":"etl.extract:v1"}]}`
	rec := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "", jsonSpec)
	if rec.Code != http.StatusCreated {
		t.Fatalf("JSON spec: status %d, want 201. body=%s", rec.Code, rec.Body)
	}

	yamlSpec := "api_version: nexus.dev/v1\nnamespace: team-ml\nname: train\ntasks:\n  - id: a\n    task_ref: etl.extract:v1\n"
	req := httptest.NewRequest(http.MethodPost, "/v1/namespaces/team-ml/specs", strings.NewReader(yamlSpec))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	req.Header.Set("Content-Type", "application/yaml")
	yrec := httptest.NewRecorder()
	h.ServeHTTP(yrec, req)
	if yrec.Code != http.StatusCreated {
		t.Fatalf("YAML spec: status %d, want 201. body=%s", yrec.Code, yrec.Body)
	}

	// The two surfaces must produce the SAME content address: ADR-0004's "neither
	// surface is second-class", visible at the API boundary.
	var a, b map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	_ = json.Unmarshal(yrec.Body.Bytes(), &b)
	if a["spec_hash"] != b["spec_hash"] {
		t.Errorf("JSON hash %v != YAML hash %v for the same workflow", a["spec_hash"], b["spec_hash"])
	}
}

// TestSubmitInvalidSpecReturns422WithProblems: 422 rather than 400 because the request
// was well-formed and the DOCUMENT is unacceptable, and the problems list is what makes
// the error actionable rather than just a rejection.
func TestSubmitInvalidSpecReturns422WithProblems(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "",
		`{"api_version":"nexus.dev/v1","namespace":"team-ml","name":"train","tasks":[]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422. body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Error    string `json:"error"`
		Problems []struct {
			Path    string `json:"path"`
			Message string `json:"message"`
			Stage   string `json:"stage"`
		} `json:"problems"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error != "spec_invalid" || len(body.Problems) == 0 {
		t.Errorf("the rejection carries no actionable problems: %s", rec.Body)
	}
}

// TestSubmitSpecRejectsNamespaceMismatch: silently preferring one would let a caller
// authenticated for namespace A store a spec claiming namespace B.
func TestSubmitSpecRejectsNamespaceMismatch(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "",
		`{"api_version":"nexus.dev/v1","namespace":"other-team","name":"train",
		  "tasks":[{"id":"a","task_ref":"x"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "namespace_mismatch") {
		t.Errorf("the rejection does not name the cause: %s", rec.Body)
	}
}

// ── authn / authz over HTTP ───────────────────────────────────────────────

func TestUnauthenticatedRequestsRejected(t *testing.T) {
	h, _, _ := newTestAPI(t)

	if rec := post(t, h, "/v1/namespaces/team-ml/runs", "", "", `{"spec_name":"t"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("no credentials: status %d, want 401", rec.Code)
	}
	if rec := post(t, h, "/v1/namespaces/team-ml/runs", "nx_not_a_real_key", "", `{"spec_name":"t"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("a bad key: status %d, want 401", rec.Code)
	}
}

// TestUnauthorizedResponsesLeakNothing: distinguishing "no such key" from "revoked"
// would make this endpoint an oracle for probing valid credentials.
func TestUnauthorizedResponsesLeakNothing(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", "nx_wrong", "", `{"spec_name":"t"}`)
	body := rec.Body.String()
	if strings.Contains(body, "nx_wrong") {
		t.Error("the response echoed the presented key material back to the caller")
	}
	for _, leak := range []string{"revoked", "no such", "not found", "hash"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the 401 distinguishes the failure mode (%q), making it an oracle: %s", leak, body)
		}
	}
}

// TestReaderCannotMutate: a read-only credential must not be able to create runs or
// submit specs.
func TestReaderCannotMutate(t *testing.T) {
	h, _, wfm := newTestAPI(t)

	if rec := post(t, h, "/v1/namespaces/team-ml/runs", readerKey, "", `{"spec_name":"t"}`); rec.Code != http.StatusForbidden {
		t.Errorf("reader creating a run: status %d, want 403", rec.Code)
	}
	if rec := post(t, h, "/v1/namespaces/team-ml/specs", readerKey, "", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("reader submitting a spec: status %d, want 403", rec.Code)
	}
	if n := wfm.callCount(); n != 0 {
		t.Errorf("a forbidden request reached the workflow-manager %d times", n)
	}
}

// TestCrossNamespaceAccessForbidden: namespaces are the tenancy boundary, and a
// boundary that is not checked on every request is not a boundary.
func TestCrossNamespaceAccessForbidden(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/other-team/runs", adminKey, "", `{"spec_name":"t"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a team-ml key acting on other-team: status %d, want 403", rec.Code)
	}
}

// TestAdminOnlyDrain: draining a worker is destructive to in-flight capacity planning,
// so it requires admin rather than any writer.
func TestAdminOnlyDrain(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/workers/w1/drain", readerKey, "", "")
	if rec.Code != http.StatusForbidden {
		t.Errorf("reader draining a worker: status %d, want 403", rec.Code)
	}
}

// ── lifecycle status mapping ──────────────────────────────────────────────

// TestLifecycleEndpointsMapStates checks that pause reports PAUSING rather than
// PAUSED: the run has not finished draining, and reporting PAUSED early would tell an
// operator that in-flight work had stopped when it has not.
func TestLifecycleEndpointsMapStates(t *testing.T) {
	h, _, _ := newTestAPI(t)
	cases := []struct{ path, wantState string }{
		{"/v1/namespaces/team-ml/runs/run-1/pause", "PAUSING"},
		{"/v1/namespaces/team-ml/runs/run-1/cancel", "CANCELLING"},
		{"/v1/namespaces/team-ml/runs/run-1/resume", "RUNNING"},
	}
	for _, c := range cases {
		rec := post(t, h, c.path, adminKey, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200. body=%s", c.path, rec.Code, rec.Body)
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: decode: %v", c.path, err)
			continue
		}
		if body["state"] != c.wantState {
			t.Errorf("%s: state %v, want %s", c.path, body["state"], c.wantState)
		}
	}
}

// TestLifecycleConflictIs409: an operator who retries a run that is not retryable must
// be told it did not apply, not given a silent success.
func TestLifecycleConflictIs409(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := post(t, h, "/v1/namespaces/team-ml/runs/run-1/retry", adminKey, "", "")
	if rec.Code != http.StatusConflict {
		t.Errorf("retrying a non-retryable run: status %d, want 409. body=%s", rec.Code, rec.Body)
	}
}

// ── schema endpoint ───────────────────────────────────────────────────────

// TestServeSchema: publishing the grammar the gateway actually enforces, so authors
// validate locally against the same rules rather than a documented copy.
func TestServeSchema(t *testing.T) {
	h, _, _ := newTestAPI(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/schemas/workflow-spec-v1.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/schema+json" {
		t.Errorf("Content-Type = %q, want application/schema+json", ct)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("the served schema is not valid JSON: %v", err)
	}
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Errorf("$schema = %v, want draft 2020-12", doc["$schema"])
	}
}

// TestBodySizeLimit: bounded by MaxBytesReader rather than a Content-Length check,
// which a client controls and can lie about.
func TestBodySizeLimit(t *testing.T) {
	h, _, _ := newTestAPI(t)
	huge := strings.Repeat("x", maxRunBytes+1024)
	rec := post(t, h, "/v1/namespaces/team-ml/runs", adminKey, "", `{"pad":"`+huge+`"}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body returned %d, want 413", rec.Code)
	}
}

// fakeFleet is a fixed upstream report, so the handler's behaviour is pinned
// without dialling anything.
type fakeFleet struct{ ups []UpstreamHealth }

func (f fakeFleet) UpstreamHealth() []UpstreamHealth { return f.ups }

// A fleet where every configured upstream is connected is "ok".
func TestFleetHealthReportsOKWhenEverythingIsReady(t *testing.T) {
	rec := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "scheduler", Addr: "scheduler:9090", State: "READY", Configured: true},
		{Name: "dag-engine", Addr: "dag:9090", State: "READY", Configured: true},
	}}), "/v1/health/fleet", adminKey)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status    string `json:"status"`
		Upstreams []struct {
			Name, State string
		} `json:"upstreams"`
		Measures string `json:"measures"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
	if len(body.Upstreams) != 2 {
		t.Errorf("reported %d upstreams, want 2", len(body.Upstreams))
	}
	// The payload must say what it measured, or a client will treat connectivity
	// as readiness and call a failing service healthy.
	if !strings.Contains(body.Measures, "connectivity") {
		t.Errorf("payload does not state what it measures: %q", body.Measures)
	}
}

// A failed upstream degrades the fleet; an IDLE one does NOT, because idle means
// "not dialled yet", which is normal before first use rather than a fault.
func TestFleetHealthDistinguishesFailureFromIdle(t *testing.T) {
	degraded := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "scheduler", State: "TRANSIENT_FAILURE", Configured: true},
	}}), "/v1/health/fleet", adminKey)
	if got := statusField(t, degraded); got != "degraded" {
		t.Errorf("a failed upstream gave status %q, want degraded", got)
	}

	idle := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "scheduler", State: "IDLE", Configured: true},
	}}), "/v1/health/fleet", adminKey)
	if got := statusField(t, idle); got != "ok" {
		t.Errorf("an IDLE upstream gave status %q, want ok (idle is not a fault)", got)
	}
}

// An upstream nobody configured is not a fault either: reporting it as down
// sends an operator chasing a service this deployment does not run.
func TestFleetHealthIgnoresUnconfiguredUpstreams(t *testing.T) {
	rec := get(t, buildFleetAPI(t, fakeFleet{ups: []UpstreamHealth{
		{Name: "dag-engine", State: "UNCONFIGURED", Configured: false},
	}}), "/v1/health/fleet", adminKey)
	if got := statusField(t, rec); got != "ok" {
		t.Errorf("an unconfigured upstream gave status %q, want ok", got)
	}
}

// A gateway assembled without a reporter answers 501, not 500: the feature is
// absent, not broken.
func TestFleetHealthIsNotImplementedWhenUnwired(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := get(t, h, "/v1/health/fleet", adminKey)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501 when no reporter is wired", rec.Code)
	}
}

func statusField(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body.Status
}

// buildFleetAPI is newTestAPI plus a wired health reporter.
func buildFleetAPI(t *testing.T, f FleetHealth) http.Handler {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	api := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithFleetHealth(f)
	return api.Routes()
}
