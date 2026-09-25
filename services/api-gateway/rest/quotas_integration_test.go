// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// TestIntegrationQuotasAndUsageAPI pins ADR-0021's API against real Postgres:
// who may read and change quotas and read usage, that another tenant's
// quotas and usage are unreachable through the path (F101), the lock rules,
// revision conflicts, the audit trail, and that the platform-config route can
// neither erase nor edit quotas.
//
// Everything runs in one transaction that is rolled back, like the fair-share
// weight test, so nothing is left behind.
func TestIntegrationQuotasAndUsageAPI(t *testing.T) {
	ctx := context.Background()
	tx, err := attentionPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	stamp := time.Now().UnixNano()
	nsA, nsB := fmt.Sprintf("quota-a-%d", stamp), fmt.Sprintf("quota-b-%d", stamp)
	for _, ns := range []string{nsA, nsB} {
		if _, err = tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'quota test')`, ns); err != nil {
			t.Fatal(err)
		}
	}
	adminA, readerA, adminB := "nx_q_admin_a_"+nsA, "nx_q_reader_a_"+nsA, "nx_q_admin_b_"+nsB
	for _, k := range []struct{ ns, role, key string }{{nsA, "admin", adminA}, {nsA, "reader", readerA}, {nsB, "admin", adminB}} {
		if _, err = store.GatewayCreateAPIKey(ctx, tx, k.ns, "test", k.role, k.key); err != nil {
			t.Fatal(err)
		}
	}
	// An RBAC-bound Viewer, and a platform operator (a legacy admin session
	// enrolled in platform_operators).
	viewer, err := store.GatewayCreateUserWithoutPassword(ctx, tx, nsA, "viewer@quota.test", "Viewer", "reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO rbac_user_access(user_id,namespace,roles) VALUES($1,$2,'["Viewer"]')`, viewer.ID, nsA); err != nil {
		t.Fatal(err)
	}
	viewerTok, _, err := store.GatewayCreateSession(ctx, tx, viewer.ID, time.Hour, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	op, err := store.GatewayCreateUserWithoutPassword(ctx, tx, nsA, "op@quota.test", "Operator", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO platform_operators(user_id) VALUES($1)`, op.ID); err != nil {
		t.Fatal(err)
	}
	opTok, _, err := store.GatewayCreateSession(ctx, tx, op.ID, time.Hour, "test", "")
	if err != nil {
		t.Fatal(err)
	}

	auth := authn.New(tx).WithRBAC().WithIdentity(authn.StoreIdentity{DB: tx})
	h := New(tx, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(tx)).Routes()
	call := func(method, path, body, bearer string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body)
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	qA, qB := "/v1/namespaces/"+nsA+"/quotas", "/v1/namespaces/"+nsB+"/quotas"
	uA, uB := "/v1/namespaces/"+nsA+"/usage", "/v1/namespaces/"+nsB+"/usage"

	// Defaults: unlimited, revision 0, every dimension listed.
	got := call("GET", qA, "", adminA, 200)
	if got["revision"] != float64(0) || len(got["dimensions"].([]any)) != 7 {
		t.Fatalf("default quotas: %v", got)
	}

	// Who may read.
	call("GET", qA, "", readerA, 200)
	call("GET", qA, "", viewerTok, 200)
	call("GET", uA, "", readerA, 403) // usage is cost data
	call("GET", uA, "", viewerTok, 403)
	call("GET", uA, "", adminA, 200)

	// Cross-namespace, every route and verb: refused before any read.
	call("GET", qB, "", adminA, 403)
	call("PUT", qB, `{"revision":0,"quotas":{"max_cpu_millis":1}}`, adminA, 403)
	call("GET", uB, "", adminA, 403)
	call("GET", qA, "", adminB, 403)

	// Who may change.
	body := `{"revision":0,"quotas":{"max_inflight_attempts":10,"max_cpu_millis":8000,"max_runs_per_hour":100}}`
	call("PUT", qA, body, readerA, 403)
	call("PUT", qA, body, viewerTok, 403)
	saved := call("PUT", qA, body, adminA, 200)
	if saved["revision"] != float64(1) {
		t.Fatalf("saved revision %v", saved["revision"])
	}
	call("PUT", qA, body, adminA, 409)                                                     // stale revision
	call("PUT", qA, `{"revision":1,"quotas":{"max_cpu_millis":-1}}`, adminA, 400)          // invalid
	call("PUT", qA, `{"quotas":{"max_cpu_millis":1}}`, adminA, 400)                        // revision required
	call("PUT", qA, `{"revision":1,"quotas":{"locked":true}}`, adminA, 403)                // only an operator locks
	call("PUT", qA, `{"revision":1,"quotas":{"max_cpu_millis":1},"extra":1}`, adminA, 400) // unknown field

	// The operator locks; the namespace admin may then only tighten.
	call("PUT", qA, `{"revision":1,"quotas":{"locked":true,"max_inflight_attempts":10,"max_cpu_millis":8000,"max_runs_per_hour":100}}`, opTok, 200)
	call("PUT", qA, `{"revision":2,"quotas":{"locked":true,"max_inflight_attempts":5,"max_cpu_millis":8000,"max_runs_per_hour":100}}`, adminA, 200)
	refused := call("PUT", qA, `{"revision":3,"quotas":{"locked":true,"max_inflight_attempts":50,"max_cpu_millis":8000,"max_runs_per_hour":100}}`, adminA, 403)
	if refused["error"] != "quotas_locked" || fmt.Sprint(refused["loosened"]) != "[max_inflight_attempts]" {
		t.Fatalf("locked refusal: %v", refused)
	}

	// The platform-config route: omitting quotas keeps them; changing them is refused.
	cfg := call("GET", "/v1/namespaces/"+nsA+"/platform/config", "", adminA, 200)
	cfgQuotas, _ := json.Marshal(cfg["quotas"])
	delete(cfg, "quotas")
	plain, _ := json.Marshal(cfg)
	call("PUT", "/v1/namespaces/"+nsA+"/platform/config", string(plain), adminA, 200)
	after := call("GET", qA, "", adminA, 200)["quotas"].(map[string]any)
	if after["max_inflight_attempts"] != float64(5) || after["locked"] != true {
		t.Fatalf("platform PUT without quotas erased them: %v", after)
	}
	cfg = call("GET", "/v1/namespaces/"+nsA+"/platform/config", "", adminA, 200)
	cfg["quotas"] = map[string]any{"max_inflight_attempts": 999}
	edited, _ := json.Marshal(cfg)
	call("PUT", "/v1/namespaces/"+nsA+"/platform/config", string(edited), adminA, 400)
	var reread map[string]any
	_ = json.Unmarshal(cfgQuotas, &reread)
	if reread["max_inflight_attempts"] != float64(5) {
		t.Fatalf("platform config GET does not expose the stored quotas: %s", cfgQuotas)
	}

	// Audited: one row per accepted change, none for refusals.
	var n int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE namespace=$1 AND action='quota.update'`, nsA).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("quota.update audit rows = %d, want 3", n)
	}

	// Usage: A's rollup sees A's ledger only.
	for _, row := range []struct {
		ns  string
		cpu int64
	}{{nsA, 1000}, {nsA, 3000}, {nsB, 500000}} {
		if _, err = tx.Exec(ctx, `
INSERT INTO usage_ledger (attempt_id, tenant_id, namespace, run_id, node_id, attempt_number, outcome, queue,
  cpu_millis, dispatched_at, ended_at, wall_seconds, reserved_seconds, cpu_core_seconds, memory_gib_seconds, gpu_seconds)
SELECT $1, id, namespace, $2, 'n', 1, 'SUCCEEDED', 'q', $3::bigint, now() - interval '1 hour', now() - interval '59 minutes',
       60, 60, $3::bigint::numeric * 60 / 1000, 0, 0 FROM tenants WHERE namespace=$4`,
			uuid.New(), uuid.New(), row.cpu, row.ns); err != nil {
			t.Fatal(err)
		}
	}
	usage := call("GET", uA+"?group_by=none", "", adminA, 200)
	buckets := usage["buckets"].([]any)
	if len(buckets) != 1 || buckets[0].(map[string]any)["cpu_core_seconds"] != float64(240) ||
		buckets[0].(map[string]any)["attempts"] != float64(2) {
		t.Fatalf("A's usage: %v", usage)
	}
	call("GET", uA+"?group_by=bogus", "", adminA, 400)
	call("GET", uA+"?from=yesterday", "", adminA, 400)
	call("GET", uA+"?from=2026-01-01&to=2025-01-01", "", adminA, 400)

	// Cross-tenant rollup: a platform operator only.
	call("GET", "/v1/platform/usage", "", adminA, 403)
	plat := call("GET", "/v1/platform/usage?limit=500", "", opTok, 200)
	seen := map[string]bool{}
	for _, b := range plat["buckets"].([]any) {
		seen[b.(map[string]any)["key"].(string)] = true
	}
	if !seen[nsA] || !seen[nsB] {
		t.Errorf("platform usage misses a namespace: %v", seen)
	}
}
