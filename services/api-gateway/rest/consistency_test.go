// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/auditexport"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// These tests pin behaviour that has to be the SAME on every route rather than
// correct on one: how a request body that cannot be read is answered, how a
// list endpoint reads `limit`, and how a revisioned write reads `revision`. A
// rule that holds on the route somebody last touched and nowhere else is how
// the gateway came to answer an oversized body with 413 on one route and 400
// on the next.

// ── request bodies ──────────────────────────────────────────────────────────

// TestReadBodyDistinguishesOversizeFromAFailedRead: the two failures have
// different fixes -- send less, or retry the transport -- and used to share
// one error, which every caller then reported as 413.
func TestReadBodyDistinguishesOversizeFromAFailedRead(t *testing.T) {
	big := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 11)))
	_, err := readBody(big, 10)
	if !isBodyTooLarge(err) {
		t.Fatalf("an 11-byte body against a 10-byte limit: err = %v, want a too-large error", err)
	}

	broken := httptest.NewRequest(http.MethodPost, "/", iotest.ErrReader(errors.New("connection reset")))
	_, err = readBody(broken, 10)
	if err == nil || isBodyTooLarge(err) {
		t.Fatalf("a failed read: err = %v, want a read error that is NOT too-large", err)
	}

	exact := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 10)))
	if b, err := readBody(exact, 10); err != nil || len(b) != 10 {
		t.Fatalf("a body at exactly the limit: %d bytes, err %v; want it accepted", len(b), err)
	}
}

// TestSpecSubmitAnswers413OnlyForOversize: a body that fails to arrive is not
// a body that is too large, and telling a client it sent too much when it sent
// nothing sends it hunting for a size problem that does not exist.
func TestSpecSubmitAnswers413OnlyForOversize(t *testing.T) {
	h, _, _ := newTestAPI(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/namespaces/team-ml/specs", iotest.ErrReader(errors.New("unexpected EOF")))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || decode(t, rec)["error"] != "unreadable_body" {
		t.Errorf("a body that could not be read: %d %s; want 400 unreadable_body", rec.Code, rec.Body)
	}

	huge := strings.Repeat("x", maxSpecBytes+1)
	rec = post(t, h, "/v1/namespaces/team-ml/specs", adminKey, "", huge)
	if rec.Code != http.StatusRequestEntityTooLarge || decode(t, rec)["error"] != "spec_too_large" {
		t.Errorf("an oversized spec: %d %s; want 413 spec_too_large", rec.Code, rec.Body)
	}
}

// bodyCheckedAPI is the gateway with every optional route family wired --
// identity, email flows, events, audit sinks, schedules -- so the oversized-body
// sweep reaches every handler that takes a body.
func bodyCheckedAPI(t *testing.T) http.Handler {
	t.Helper()
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	ids := newFakeIdentityAdmin()
	keys, err := auditexport.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	api := New(db, authn.New(db).WithIdentity(sessionBridge{ids}), &fakeWFM{}, fakeRM{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(ids).
		WithEmailFlows(DefaultEmailFlowConfig()).
		WithEvents(EventsConfig{}).
		WithAuditExport(keys, auditexport.URLPolicy{}).
		WithSchedules(&fakeSchedules{}).
		WithBackfills(&fakeBackfills{}).
		WithCacheInvalidator(&fakeInvalidator{})
	t.Cleanup(func() { _ = api.CloseEmailFlows(context.Background()) })
	return api.Routes()
}

// readTracker is a request body that remembers whether the handler read it.
type readTracker struct {
	r    io.Reader
	read bool
}

func (t *readTracker) Read(p []byte) (int, error) {
	t.read = true
	return t.r.Read(p)
}

// TestOversizedBodiesAre413OnEveryRoute sends a body larger than any route's
// limit to every POST, PUT and PATCH route and requires 413 from every handler
// that reads it. Before, the routes that decoded straight from the request
// (roles, service accounts, audit, the ADR-0028 surfaces) reported the size as
// a malformed body, 400, and the schedule routes called it unreadable. A route
// that answers without reading the body (a lifecycle action, a refusal made
// before the body matters) is not held to it: its answer does not depend on
// the body at all.
func TestOversizedBodiesAre413OnEveryRoute(t *testing.T) {
	h := bodyCheckedAPI(t)
	huge := `{"name":"` + strings.Repeat("x", maxSpecBytes+1) + `"}`
	checked := 0
	for _, rt := range parsedRoutes(t) {
		method, path, _ := strings.Cut(rt.pattern, " ")
		if method != http.MethodPost && method != http.MethodPut && method != http.MethodPatch {
			continue
		}
		concrete := strings.NewReplacer("{ns}", "team-ml", "{id}", "00000000-0000-0000-0000-000000000001",
			"{name}", "thing", "{run}", "-", "{trigger}", "thing").Replace(path)
		body := &readTracker{r: strings.NewReader(huge)}
		req := httptest.NewRequest(method, concrete, body)
		req.Header.Set("Authorization", "Bearer "+adminKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !body.read {
			continue
		}
		checked++
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: an oversized body answered %d %s; want 413", rt.pattern, rec.Code, truncateForLog(rec.Body.String()))
		}
	}
	if checked < 35 {
		t.Fatalf("only %d routes read the oversized body; the sweep is not reaching the handlers", checked)
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// ── revisions ───────────────────────────────────────────────────────────────

// TestNegativeRevisionIsRejectedEverywhere: under the revision rule a
// revision is the value the client last read, or 0 for "create only if
// absent". A negative one names no state the server ever had; every
// revisioned write refuses it with 400 before touching anything, where some
// used to answer 409 (as if it were merely stale) and others 200.
func TestNegativeRevisionIsRejectedEverywhere(t *testing.T) {
	h := bodyCheckedAPI(t)
	const id = "00000000-0000-0000-0000-000000000001"
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", "/v1/namespaces/team-ml/quotas", `{"revision":-1,"quotas":{}}`},
		{"PUT", "/v1/namespaces/team-ml/platform/config", `{"revision":-1,"scheduler":{"min_replicas":1,"max_replicas":4,"target_cpu_percent":65,"scale_down_seconds":300,"fair_share_weight":1}}`},
		{"PUT", "/v1/namespaces/team-ml/platform/clusters/prod", `{"desired_revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/roles/Run%20monitor", `{"permissions":["runs.view"],"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/users/" + id + "/roles", `{"roles":[],"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/service-accounts/" + id, `{"description":"x","revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/service-accounts/" + id + "/roles", `{"roles":[],"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/audit/policy", `{"retention_days":30,"revision":-1}`},
		{"POST", "/v1/namespaces/team-ml/audit/legal-hold", `{"hold":false,"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/audit/sinks/" + id, `{"enabled":false,"revision":-1}`},
		{"POST", "/v1/namespaces/team-ml/audit/sinks/" + id + "/rotate-secret", `{"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/notification-channels/thing", `{"enabled":false,"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/notification-rules/thing", `{"events":["run.failed"],"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/triggers/thing", `{"enabled":false,"revision":-1}`},
		{"PUT", "/v1/namespaces/team-ml/sensors/thing", `{"enabled":false,"revision":-1}`},
	} {
		rec := doJSON(t, h, tc.method, tc.path, adminKey, tc.body)
		want := "invalid_revision"
		if strings.HasSuffix(tc.path, "/platform/config") {
			want = "invalid_policy" // the document validates its own revision
		}
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"error":"`+want+`"`) {
			t.Errorf("%s %s with revision -1: %d %s; want 400 %s", tc.method, tc.path, rec.Code, truncateForLog(rec.Body.String()), want)
		}
	}
}

// ── problem bodies ──────────────────────────────────────────────────────────

// TestInvalidAuditSinkNamesTheField: the 422 for a refused sink was the one
// error body without a `message`, so a client printing `message` printed
// nothing, and nothing said WHICH field to fix except by parsing prose.
func TestInvalidAuditSinkNamesTheField(t *testing.T) {
	h := bodyCheckedAPI(t)
	rec := doJSON(t, h, "POST", "/v1/namespaces/team-ml/audit/sinks", adminKey,
		`{"name":"siem","url":"ftp://siem.example/in","batch_size":5000}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d %s; want 422", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	msg, _ := body["message"].(string)
	if body["error"] != "invalid_sink" || !strings.Contains(msg, "url") || !strings.Contains(msg, "batch_size") {
		t.Errorf("body %v; want invalid_sink with a message naming url and batch_size", body)
	}
	fields, _ := body["fields"].([]any)
	if len(fields) != 2 || fields[0] != "url" || fields[1] != "batch_size" {
		t.Errorf("fields = %v; want [url batch_size]", body["fields"])
	}
	if p, _ := body["problems"].([]any); len(p) != 2 {
		t.Errorf("problems = %v; want one entry per invalid field", body["problems"])
	}

	rec = doJSON(t, h, "PUT", "/v1/namespaces/team-ml/audit/sinks/00000000-0000-0000-0000-000000000001", adminKey,
		`{"batch_size":0,"revision":1}`)
	if body := decode(t, rec); rec.Code != http.StatusUnprocessableEntity || body["field"] != "batch_size" {
		t.Errorf("update with batch_size 0: %d %v; want 422 naming field batch_size", rec.Code, body)
	}
}

// ── empty lists ─────────────────────────────────────────────────────────────

// TestEmptyClusterListIsAnArray: a namespace with no registered clusters
// answers `"clusters": []`. A null makes every client test for two kinds of
// "none", and the console's registry table rendered a null as a load failure.
func TestEmptyClusterListIsAnArray(t *testing.T) {
	h, _, _ := newTestAPI(t)
	rec := get(t, h, "/v1/namespaces/team-ml/platform/clusters", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"clusters":[]`) {
		t.Errorf("an empty registry: %s; want \"clusters\":[]", rec.Body)
	}
}

// ── authorization ───────────────────────────────────────────────────────────

// mutatingOnCoarseAction lists the mutating routes that carry NO fine-grained
// permission and may nevertheless be guarded on read, with the reason.
var mutatingOnCoarseAction = map[string]string{
	// Any authenticated principal may end its own session.
	"POST /v1/auth/logout": "ends the caller's own session",
}

// TestMutatingRoutesWithoutAPermissionAreNotGuardedOnRead is F154's class
// check. Most routes are guarded on a coarse action AND a routePermissions
// entry, and the fine permission is what really decides. A route whose entry
// is EMPTY is decided by the coarse action alone (plus whatever its handler
// checks), so a write guarded on `read` lets every read-only credential
// through the guard and leaves the handler as the only line of defence.
// POST /v1/platform/tenants was exactly that.
func TestMutatingRoutesWithoutAPermissionAreNotGuardedOnRead(t *testing.T) {
	for _, rt := range parsedRoutes(t) {
		method, _, _ := strings.Cut(rt.pattern, " ")
		if method == http.MethodGet || method == http.MethodHead || rt.action == "" {
			continue
		}
		if perms, ok := routePermissions[rt.pattern]; !ok || len(perms) > 0 {
			continue
		}
		if _, allowed := mutatingOnCoarseAction[rt.pattern]; allowed {
			continue
		}
		if rt.action == string(authn.ActionRead) {
			t.Errorf("%s changes state, has no fine-grained permission, and is guarded on read (%s)", rt.pattern, rt.pos)
		}
	}
}

// TestCreatingATenantNeedsAdminWrite pins F154 on the route itself: creating a
// tenant is a platform-operator WRITE, and the guard says so before the
// handler's operator check runs.
func TestCreatingATenantNeedsAdminWrite(t *testing.T) {
	for _, rt := range parsedRoutes(t) {
		if rt.pattern == "POST /v1/platform/tenants" {
			if rt.action != string(authn.ActionAdminWrite) {
				t.Fatalf("POST /v1/platform/tenants is guarded on %q; want admin_write", rt.action)
			}
			return
		}
	}
	t.Fatal("POST /v1/platform/tenants is not registered")
}

// TestAReadOnlyCredentialCannotReachTenantCreation: a reader key is refused by
// the guard itself, and the body -- a new tenant and its administrator's
// password -- is never read.
func TestAReadOnlyCredentialCannotReachTenantCreation(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	body := &readTracker{r: strings.NewReader(`{"name":"Acme","email":"owner@acme.example","password":"a-long-enough-password"}`)}
	req := httptest.NewRequest(http.MethodPost, "/v1/platform/tenants", body)
	req.Header.Set("Authorization", "Bearer "+readerKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader key: %d %s; want 403", rec.Code, rec.Body)
	}
	if body.read {
		t.Error("the handler read the request body of a refused tenant creation")
	}
}

// ── missing things are 404 ──────────────────────────────────────────────────

// TestCurrentTenantWithoutATenantRowIs404: a principal whose namespace has no
// tenants row -- a namespace that predates tenancy, or a KGF principal whose
// organization was never onboarded as a Nexus tenant -- used to get 500,
// which reads as "the server broke" for what is an absent record.
func TestCurrentTenantWithoutATenantRowIs404(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	rec := get(t, h, "/v1/account/tenant", adminKey)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d %s; want 404", rec.Code, rec.Body)
	}
	body := decode(t, rec)
	if body["error"] != "tenant_not_found" || body["message"] == "" {
		t.Errorf("body %v; want a tenant_not_found problem with a message", body)
	}
}

// ── list limits ─────────────────────────────────────────────────────────────

func TestListLimitParsing(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		want   int
		status int
	}{
		{"", 100, 0},
		{"1", 1, 0},
		{"250", 250, 0},
		{"500", 500, 0},
		{"501", 500, 0}, // capped, not reset to the default
		{"1000000", 500, 0},
		{"0", 0, http.StatusBadRequest},
		{"-1", 0, http.StatusBadRequest},
		{"ten", 0, http.StatusBadRequest},
		{"1.5", 0, http.StatusBadRequest},
		{"99999999999999999999", 0, http.StatusBadRequest},
	} {
		target := "/x"
		if tc.raw != "" {
			target += "?limit=" + url.QueryEscape(tc.raw)
		}
		rec := httptest.NewRecorder()
		got, ok := listLimit(rec, httptest.NewRequest(http.MethodGet, target, nil), 100, 500)
		if tc.status != 0 {
			if ok || rec.Code != tc.status {
				t.Errorf("limit=%q: ok=%v status=%d; want refused with %d", tc.raw, ok, rec.Code, tc.status)
			} else if decode(t, rec)["error"] != "invalid_limit" {
				t.Errorf("limit=%q: body %s; want error invalid_limit", tc.raw, rec.Body)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("limit=%q: got %d ok=%v; want %d", tc.raw, got, ok, tc.want)
		}
	}
}

// TestListEndpointsRejectAMalformedLimit: every list endpoint answers a limit
// that is not a positive integer with 400 before it reads anything, rather than
// silently substituting its default (which hid client bugs behind a page that
// was merely the wrong size).
func TestListEndpointsRejectAMalformedLimit(t *testing.T) {
	h := bodyCheckedAPI(t)
	for _, path := range []string{
		"/v1/namespaces/team-ml/runs",
		"/v1/namespaces/team-ml/specs",
		"/v1/namespaces/team-ml/specs/etl/versions",
		"/v1/namespaces/team-ml/schedules",
		"/v1/namespaces/team-ml/schedules/nightly/backfills",
		"/v1/namespaces/team-ml/attention",
		"/v1/namespaces/team-ml/users",
		"/v1/namespaces/team-ml/users/00000000-0000-0000-0000-000000000001/sessions",
		"/v1/namespaces/team-ml/tokens",
		"/v1/namespaces/team-ml/audit",
		"/v1/namespaces/team-ml/notification-deliveries",
		"/v1/namespaces/team-ml/triggers/thing/deliveries",
		"/v1/namespaces/team-ml/sensors/thing/observations",
		"/v1/namespaces/team-ml/assets",
		"/v1/namespaces/team-ml/usage",
	} {
		for _, bad := range []string{"-5", "abc", "0"} {
			rec := get(t, h, path+"?limit="+bad, adminKey)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s?limit=%s: %d %s; want 400", path, bad, rec.Code, truncateForLog(rec.Body.String()))
			}
		}
	}
	rec := get(t, h, "/v1/namespaces/team-ml/workers/00000000-0000-0000-0000-000000000001?recent_limit=-1", adminKey)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET worker?recent_limit=-1: %d; want 400", rec.Code)
	}
}

// TestRunListCapsAnOversizedLimit: limit=600 used to fall back to 100, so a
// client asking for more than the cap got FEWER rows than it would have for a
// limit of 101. It is capped at 500 instead.
func TestRunListCapsAnOversizedLimit(t *testing.T) {
	h, db, _ := newTestAPI(t)
	rec := get(t, h, "/v1/namespaces/team-ml/runs?limit=600", adminKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	// One more than the page, for the has-more lookahead.
	if got := db.lastRunListLimit(); got != 501 {
		t.Errorf("the store was asked for %d rows; want 501 (the 500 cap plus the lookahead)", got)
	}
	var body struct {
		Runs []any `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Runs == nil {
		t.Errorf("runs = %v (%v); want an empty list, never null", body.Runs, err)
	}
}
