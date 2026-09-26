// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/notify"
	"github.com/khuriwalgroup/kgf-nexus/internal/secretbox"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// fakeRuns stands in for the workflow-manager: it deduplicates on the
// idempotency key as the run layer does, and can be told to fail.
type fakeRuns struct {
	mu    sync.Mutex
	byKey map[string]string
	calls []TriggeredRunRequest
	fail  error
}

func (f *fakeRuns) CreateTriggeredRun(_ context.Context, req TriggeredRunRequest) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.fail != nil {
		err := f.fail
		f.fail = nil
		return "", false, err
	}
	if id, ok := f.byKey[req.IdempotencyKey]; ok {
		return id, false, nil
	}
	id := uuid.NewString()
	f.byKey[req.IdempotencyKey] = id
	return id, true, nil
}

type eventsHarness struct {
	t       *testing.T
	ctx     context.Context
	tx      pgx.Tx
	h       http.Handler
	nsA     string
	nsB     string
	adminA  string
	readerA string
	adminB  string
	runs    *fakeRuns
}

func newEventsHarness(t *testing.T, withKey bool) *eventsHarness {
	ctx := context.Background()
	tx, err := attentionPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	stamp := time.Now().UnixNano()
	e := &eventsHarness{t: t, ctx: ctx, tx: tx, nsA: fmt.Sprintf("ev-a-%d", stamp), nsB: fmt.Sprintf("ev-b-%d", stamp),
		runs: &fakeRuns{byKey: map[string]string{}}}
	for _, ns := range []string{e.nsA, e.nsB} {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'events test')`, ns); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ($1, 'etl', 1, $2, '{}', 'test')`, ns, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	e.adminA, e.readerA, e.adminB = "nx_ev_admin_a_"+e.nsA, "nx_ev_reader_a_"+e.nsA, "nx_ev_admin_b_"+e.nsB
	for _, k := range []struct{ ns, role, key string }{{e.nsA, "admin", e.adminA}, {e.nsA, "reader", e.readerA}, {e.nsB, "admin", e.adminB}} {
		if _, err := store.GatewayCreateAPIKey(ctx, tx, k.ns, "test", k.role, k.key); err != nil {
			t.Fatal(err)
		}
	}
	var box *secretbox.Box
	if withKey {
		box, _ = secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	}
	auth := authn.New(tx).WithRBAC()
	e.h = New(tx, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(tx)).
		WithEvents(EventsConfig{Secrets: box, TriggeredRuns: e.runs, AllowPrivateTargets: true, PublicURL: "https://nexus.example"}).
		Routes()
	return e
}

func (e *eventsHarness) call(method, path, body, bearer string, want int) map[string]any {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != want {
		e.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body)
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

func (e *eventsHarness) hook(path string, body []byte, secret string, ts time.Time) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	tsS := strconv.FormatInt(ts.Unix(), 10)
	r.Header.Set("X-Nexus-Timestamp", tsS)
	r.Header.Set("X-Nexus-Signature", notify.Signature([]byte(secret), tsS, body))
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *eventsHarness) auditCount(action, outcome string) int {
	var n int
	if err := e.tx.QueryRow(e.ctx, `SELECT count(*) FROM audit_log WHERE namespace = $1 AND action = $2 AND outcome = $3`,
		e.nsA, action, outcome).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestIntegrationNotificationChannelsNeverExposeSecrets(t *testing.T) {
	e := newEventsHarness(t, true)
	base := "/v1/namespaces/" + e.nsA + "/notification-channels"
	created := e.call("POST", base, `{"name":"ops-hook","kind":"webhook","config":{"url":"https://hooks.example/nexus"}}`, e.adminA, 201)
	signing, _ := created["signing_secret"].(string)
	if len(signing) != 64 {
		t.Fatalf("a generated signing secret should be shown once: %v", created)
	}
	slack := e.call("POST", base, `{"name":"slack","kind":"slack_webhook","secret":"https://hooks.slack.com/services/T0/B0/SUPERSECRET"}`, e.adminA, 201)
	if slack["secret"].(map[string]any)["hint"] != "hooks.slack.com/…" {
		t.Fatalf("hint %v", slack["secret"])
	}
	for _, path := range []string{base, base + "/slack", base + "/ops-hook"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+e.readerA)
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("reader GET %s: %d", path, w.Code)
		}
		if strings.Contains(w.Body.String(), "SUPERSECRET") || strings.Contains(w.Body.String(), signing) ||
			strings.Contains(w.Body.String(), "signing_secret") {
			t.Fatalf("GET %s leaks a secret: %s", path, w.Body)
		}
	}
	// Sealed at rest.
	var ct []byte
	if err := e.tx.QueryRow(e.ctx, `SELECT secret_ciphertext FROM notification_channels WHERE namespace = $1 AND name = 'slack'`, e.nsA).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, []byte("SUPERSECRET")) {
		t.Fatal("secret stored in clear")
	}
	// References, validation and authorization.
	e.call("POST", base, `{"name":"pd","kind":"pagerduty","secret_ref":"env:PD_ROUTING_KEY","config":{"severity":"critical"}}`, e.adminA, 201)
	e.call("POST", base, `{"name":"pd2","kind":"pagerduty","secret_ref":"env:NEXUS_SECRET_KEY"}`, e.adminA, 422)
	e.call("POST", base, `{"name":"bad","kind":"slack_webhook"}`, e.adminA, 422)
	e.call("POST", base, `{"name":"bad","kind":"email","config":{"to":["x@example.com"]},"secret":"no"}`, e.adminA, 422)
	e.call("POST", base, `{"name":"bad","kind":"webhook","config":{"url":"https://h.example"},"token":"x"}`, e.adminA, 400)
	e.call("POST", base, `{"name":"x","kind":"webhook","config":{"url":"https://h.example"}}`, e.readerA, 403)
	e.call("GET", "/v1/namespaces/"+e.nsB+"/notification-channels", "", e.adminA, 403)
	e.call("POST", base, `{"name":"ops-hook","kind":"webhook","config":{"url":"https://h.example"}}`, e.adminA, 409)
	// Update keeps the secret unless replaced. The revision rule (F155): an
	// update names the revision it read; omitting it (0, "create only if
	// absent") on a channel that exists is a conflict, not an overwrite.
	e.call("PUT", base+"/slack", `{"enabled":false,"rate_limit_per_minute":5}`, e.adminA, 409)
	upd := e.call("PUT", base+"/slack", `{"enabled":false,"rate_limit_per_minute":5,"revision":1}`, e.adminA, 200)
	if upd["enabled"] != false || upd["secret"].(map[string]any)["configured"] != true {
		t.Fatalf("update %v", upd)
	}
	if e.auditCount("notification.channel.create", "success") != 3 || e.auditCount("notification.channel.update", "success") != 1 {
		t.Fatal("channel changes were not audited")
	}
}

func TestIntegrationInlineSecretsNeedAKey(t *testing.T) {
	e := newEventsHarness(t, false)
	base := "/v1/namespaces/" + e.nsA + "/notification-channels"
	e.call("POST", base, `{"name":"s","kind":"slack_webhook","secret":"https://hooks.slack.com/x"}`, e.adminA, 503)
	e.call("POST", base, `{"name":"s","kind":"slack_webhook","secret_ref":"file:/run/secrets/slack"}`, e.adminA, 201)
	e.call("POST", "/v1/namespaces/"+e.nsA+"/triggers", `{"name":"t","workflow":"etl"}`, e.adminA, 503)
}

func TestIntegrationRulesTestSendAndRedrive(t *testing.T) {
	e := newEventsHarness(t, true)
	ns := "/v1/namespaces/" + e.nsA
	e.call("POST", ns+"/notification-channels", `{"name":"hook","kind":"webhook","config":{"url":"https://hooks.example/x"}}`, e.adminA, 201)
	e.call("POST", ns+"/notification-rules", `{"name":"r","events":["run.exploded"],"channels":["hook"]}`, e.adminA, 422)
	e.call("POST", ns+"/notification-rules", `{"name":"r","events":["run.sla_exceeded"],"channels":["hook"]}`, e.adminA, 422)
	e.call("POST", ns+"/notification-rules", `{"name":"r","events":["run.failed"],"channels":["nope"]}`, e.adminA, 404)
	rule := e.call("POST", ns+"/notification-rules",
		`{"name":"r","events":["run.failed","run.sla_exceeded","attempt.lost"],"sla_seconds":600,"workflows":["etl"],"labels":{"team":"data"},"channels":["hook"]}`, e.adminA, 201)
	if rule["sla_seconds"] != float64(600) || len(rule["channels"].([]any)) != 1 {
		t.Fatalf("rule %v", rule)
	}
	e.call("PUT", ns+"/notification-rules/r", `{"events":["run.failed"],"channels":["hook"],"revision":999}`, e.adminA, 409)
	e.call("PUT", ns+"/notification-rules/r", `{"events":["run.failed"],"channels":["hook"]}`, e.adminA, 409)
	e.call("PUT", ns+"/notification-rules/r", `{"events":["run.failed"],"channels":["hook"],"revision":-1}`, e.adminA, 400)
	e.call("PUT", ns+"/notification-rules/r", `{"events":["run.failed"],"channels":["hook"],"revision":1}`, e.adminA, 200)
	e.call("PUT", ns+"/notification-rules/gone", `{"events":["run.failed"],"channels":["hook"],"revision":1}`, e.adminA, 404)

	test := e.call("POST", ns+"/notification-channels/hook/test", "", e.adminA, 202)
	id := int64(test["delivery_id"].(float64))
	got := e.call("GET", ns+"/notification-deliveries/"+strconv.FormatInt(id, 10), "", e.readerA, 200)
	if got["state"] != "pending" || got["event_type"] != notify.EventTest {
		t.Fatalf("test delivery %v", got)
	}
	e.call("POST", ns+"/notification-deliveries/"+strconv.FormatInt(id, 10)+"/redrive", "", e.adminA, 409)
	if _, err := e.tx.Exec(e.ctx, `UPDATE notification_deliveries SET state = 'dead', dead_at = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	re := e.call("POST", ns+"/notification-deliveries/"+strconv.FormatInt(id, 10)+"/redrive", "", e.adminA, 202)
	newID := int64(re["delivery_id"].(float64))
	nd := e.call("GET", ns+"/notification-deliveries/"+strconv.FormatInt(newID, 10), "", e.adminA, 200)
	if nd["dedup_key"] != got["dedup_key"] || nd["redrive_of"] != float64(id) {
		t.Fatalf("redrive %v", nd)
	}
	dead := e.call("GET", ns+"/notification-deliveries?state=dead", "", e.adminA, 200)
	if len(dead["deliveries"].([]any)) != 1 {
		t.Fatalf("dead listing %v", dead)
	}
	e.call("GET", "/v1/namespaces/"+e.nsB+"/notification-deliveries/"+strconv.FormatInt(id, 10), "", e.adminB, 404)
}

func TestIntegrationWebhookTriggerSignatureReplayAndIdempotency(t *testing.T) {
	e := newEventsHarness(t, true)
	ns := "/v1/namespaces/" + e.nsA
	e.call("POST", ns+"/triggers", `{"name":"t","workflow":"missing"}`, e.adminA, 422)
	e.call("POST", ns+"/triggers", `{"name":"t","workflow":"etl"}`, e.readerA, 403)
	tr := e.call("POST", ns+"/triggers", `{"name":"gh","workflow":"etl","idempotency_key_path":"$.payload.delivery",
	  "params_mapping":{"repo":"$.payload.repository.name","ref":"refs: {{ $.payload.ref }}","env":"prod"}}`, e.adminA, 201)
	secret := tr["secret"].(string)
	path := tr["path"].(string)
	if tr["url"] != "https://nexus.example"+path {
		t.Fatalf("url %v", tr["url"])
	}
	got := e.call("GET", ns+"/triggers/gh", "", e.readerA, 200)
	if _, leaked := got["secret"]; leaked {
		t.Fatal("GET returned the trigger secret")
	}

	body := []byte(`{"delivery":"d-1","ref":"main","repository":{"name":"etl-repo"}}`)
	now := time.Now()
	w := e.hook(path, body, secret, now)
	if w.Code != 202 {
		t.Fatalf("valid delivery: %d %s", w.Code, w.Body)
	}
	e.runs.mu.Lock()
	call := e.runs.calls[0]
	e.runs.mu.Unlock()
	if call.TriggerKind != "webhook" || call.IdempotencyKey != "webhook:"+tr["id"].(string)+":d-1" ||
		call.Params["repo"] != "etl-repo" || call.Params["ref"] != "refs: main" || call.Params["env"] != "prod" {
		t.Fatalf("run request %+v", call)
	}
	// A byte-identical resend is a replay.
	if w := e.hook(path, body, secret, now); w.Code != http.StatusConflict {
		t.Fatalf("replay: %d", w.Code)
	}
	// The sender's retry with a fresh timestamp converges on the same run.
	w = e.hook(path, body, secret, now.Add(time.Second))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 202 || out["created"] != false {
		t.Fatalf("retry: %d %s", w.Code, w.Body)
	}
	if w := e.hook(path, body, "wrong-secret", now.Add(2*time.Second)); w.Code != 401 {
		t.Fatalf("bad signature: %d", w.Code)
	}
	if w := e.hook(path, body, secret, now.Add(-10*time.Minute)); w.Code != 401 {
		t.Fatalf("stale timestamp: %d", w.Code)
	}
	if w := e.hook(path, []byte(`{"ref":"x","repository":{"name":"r"}}`), secret, now.Add(3*time.Second)); w.Code != 422 {
		t.Fatalf("missing idempotency key: %d", w.Code)
	}
	if w := e.hook("/v1/hooks/"+e.nsA+"/"+uuid.NewString(), body, secret, now); w.Code != 404 {
		t.Fatalf("unknown trigger: %d", w.Code)
	}
	// Unsigned.
	r := httptest.NewRequest("POST", path, bytes.NewReader(body))
	rw := httptest.NewRecorder()
	e.h.ServeHTTP(rw, r)
	if rw.Code != 401 {
		t.Fatalf("unsigned: %d", rw.Code)
	}

	// A transient upstream failure marks the receipt rejected, so the identical
	// signed retry is accepted rather than refused as a replay.
	b2 := []byte(`{"delivery":"d-2","ref":"dev","repository":{"name":"r"}}`)
	t2 := now.Add(5 * time.Second)
	e.runs.mu.Lock()
	e.runs.fail = status.Error(codes.ResourceExhausted, "max_active_runs reached")
	e.runs.mu.Unlock()
	if w := e.hook(path, b2, secret, t2); w.Code != http.StatusTooManyRequests {
		t.Fatalf("upstream limit: %d %s", w.Code, w.Body)
	}
	if w := e.hook(path, b2, secret, t2); w.Code != 202 {
		t.Fatalf("retry after rejection: %d %s", w.Code, w.Body)
	}

	// Rotation with a grace period: both secrets verify; with grace 0 the old one stops.
	rot := e.call("POST", ns+"/triggers/gh/rotate-secret", `{"grace_seconds":3600}`, e.adminA, 200)
	newSecret := rot["secret"].(string)
	if w := e.hook(path, []byte(`{"delivery":"d-3","ref":"x","repository":{"name":"r"}}`), secret, now.Add(6*time.Second)); w.Code != 202 {
		t.Fatalf("old secret in grace: %d", w.Code)
	}
	if w := e.hook(path, []byte(`{"delivery":"d-4","ref":"x","repository":{"name":"r"}}`), newSecret, now.Add(7*time.Second)); w.Code != 202 {
		t.Fatalf("new secret: %d", w.Code)
	}
	e.call("POST", ns+"/triggers/gh/rotate-secret", `{"grace_seconds":0}`, e.adminA, 200)
	if w := e.hook(path, []byte(`{"delivery":"d-5","ref":"x","repository":{"name":"r"}}`), newSecret, now.Add(8*time.Second)); w.Code != 401 {
		t.Fatalf("revoked secret still verifies: %d", w.Code)
	}
	// Disabled triggers are indistinguishable from unknown ones.
	e.call("PUT", ns+"/triggers/gh", `{"enabled":false}`, e.adminA, 409) // no revision: not an overwrite
	rev := e.call("GET", ns+"/triggers/gh", "", e.readerA, 200)["revision"].(float64)
	e.call("PUT", ns+"/triggers/gh", fmt.Sprintf(`{"enabled":false,"revision":%d}`, int64(rev)), e.adminA, 200)
	if w := e.hook(path, body, secret, now.Add(9*time.Second)); w.Code != 404 {
		t.Fatalf("disabled: %d", w.Code)
	}
	dl := e.call("GET", ns+"/triggers/gh/deliveries", "", e.readerA, 200)
	if len(dl["deliveries"].([]any)) < 5 {
		t.Fatalf("delivery log %v", dl)
	}
	if e.auditCount("trigger.fire", "success") < 4 || e.auditCount("trigger.fire", "denied") < 5 ||
		e.auditCount("trigger.create", "success") != 1 || e.auditCount("trigger.secret.rotate", "success") != 2 {
		t.Fatal("trigger activity was not audited")
	}
}

func TestIntegrationSensorsAPI(t *testing.T) {
	e := newEventsHarness(t, true)
	ns := "/v1/namespaces/" + e.nsA
	e.call("POST", ns+"/sensors", `{"name":"s","kind":"object_store","workflow":"etl","config":{"uri":"ftp://x/y"}}`, e.adminA, 422)
	e.call("POST", ns+"/sensors", `{"name":"s","kind":"run_succeeded","workflow":"etl","config":{"upstream_workflow":"etl"}}`, e.adminA, 422)
	e.call("POST", ns+"/sensors", `{"name":"s","kind":"object_store","workflow":"etl","config":{"uri":"s3://b/p/"},"params_mapping":{"x":"$.object[*]"}}`, e.adminA, 422)
	s := e.call("POST", ns+"/sensors", `{"name":"arrivals","kind":"object_store","workflow":"etl","config":{"uri":"s3://raw/in/","pattern":"*.csv"},
	  "params_mapping":{"path":"$.object.uri"},"poll_interval_seconds":10}`, e.adminA, 201)
	if s["status"] != "waiting" || s["poll_interval_seconds"] != float64(10) {
		t.Fatalf("sensor %v", s)
	}
	e.call("PUT", ns+"/sensors/arrivals", `{"kind":"run_succeeded"}`, e.adminA, 400)
	e.call("PUT", ns+"/sensors/arrivals", `{"enabled":false}`, e.adminA, 409)
	e.call("PUT", ns+"/sensors/arrivals", `{"enabled":false,"revision":-2}`, e.adminA, 400)
	e.call("PUT", ns+"/sensors/arrivals", `{"enabled":false,"revision":1}`, e.adminA, 200)
	list := e.call("GET", ns+"/sensors", "", e.readerA, 200)
	if len(list["sensors"].([]any)) != 1 || list["sensors"].([]any)[0].(map[string]any)["status"] != "disabled" {
		t.Fatalf("list %v", list)
	}
	e.call("GET", ns+"/sensors/arrivals/observations", "", e.readerA, 200)
	e.call("DELETE", ns+"/sensors/arrivals", "", e.readerA, 403)
	e.call("DELETE", ns+"/sensors/arrivals", "", e.adminA, 204)
}

func TestIntegrationAssetLineageAPI(t *testing.T) {
	e := newEventsHarness(t, true)
	var specID, runID string
	if err := e.tx.QueryRow(e.ctx, `SELECT id::text FROM workflow_specs WHERE namespace = $1 AND name = 'etl'`, e.nsA).Scan(&specID); err != nil {
		t.Fatal(err)
	}
	if err := e.tx.QueryRow(e.ctx, `INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind) VALUES ($1, $2, 'SUCCEEDED', 'test') RETURNING id::text`, specID, e.nsA).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO asset_lineage_edges (run_id, node_id, direction, asset_uri, asset_name, namespace, spec_name) VALUES
		  ($1,'extract','consumes','s3://raw/orders','Raw orders',$2,'etl'), ($1,'extract','produces','pg://dw/orders','',$2,'etl'),
		  ($1,'report','consumes','pg://dw/orders','',$2,'etl')`,
		`INSERT INTO assets (namespace, uri, name) SELECT $2,'s3://raw/orders','Raw orders' WHERE $1::uuid IS NOT NULL UNION ALL SELECT $2,'pg://dw/orders',''`,
		`INSERT INTO asset_materializations (namespace, asset_uri, run_id, node_id, spec_name) VALUES ($2,'pg://dw/orders',$1,'extract','etl')`,
	} {
		if _, err := e.tx.Exec(e.ctx, q, runID, e.nsA); err != nil {
			t.Fatal(err)
		}
	}
	ns := "/v1/namespaces/" + e.nsA
	list := e.call("GET", ns+"/assets", "", e.readerA, 200)
	if len(list["assets"].([]any)) != 2 {
		t.Fatalf("assets %v", list)
	}
	d := e.call("GET", ns+"/assets/detail?uri=pg://dw/orders", "", e.readerA, 200)
	if len(d["producer_tasks"].([]any)) != 1 || len(d["consumer_tasks"].([]any)) != 1 || d["latest_materialization"] == nil {
		t.Fatalf("detail %v", d)
	}
	g := e.call("GET", ns+"/assets/graph?uri=s3://raw/orders&depth=2", "", e.readerA, 200)
	if len(g["nodes"].([]any)) != 4 || len(g["edges"].([]any)) != 3 {
		t.Fatalf("graph %v", g)
	}
	e.call("GET", ns+"/assets/detail?uri=nope://x", "", e.readerA, 404)
	e.call("GET", "/v1/namespaces/"+e.nsB+"/assets/detail?uri=pg://dw/orders", "", e.adminB, 404)
}
