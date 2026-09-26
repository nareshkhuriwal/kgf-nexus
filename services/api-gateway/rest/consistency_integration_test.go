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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// consistencyHarness is one namespace, an admin API key, and the gateway, all
// inside a transaction that is rolled back -- including the fault-injection
// trigger below, since DDL is transactional in PostgreSQL.
type consistencyHarness struct {
	t   *testing.T
	ctx context.Context
	tx  pgx.Tx
	h   http.Handler
	ns  string
	key string
}

func newConsistencyHarness(t *testing.T) *consistencyHarness {
	t.Helper()
	ctx := context.Background()
	tx, err := attentionPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	c := &consistencyHarness{t: t, ctx: ctx, tx: tx, ns: fmt.Sprintf("consistency-%d", time.Now().UnixNano())}
	if _, err := tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'consistency test')`, c.ns); err != nil {
		t.Fatal(err)
	}
	c.key = "nx_consistency_" + c.ns
	if _, err := store.GatewayCreateAPIKey(ctx, tx, c.ns, "test", "admin", c.key); err != nil {
		t.Fatal(err)
	}
	auth := authn.New(tx).WithRBAC().WithIdentity(authn.StoreIdentity{DB: tx})
	c.h = New(tx, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(tx)).Routes()
	return c
}

func (c *consistencyHarness) call(method, path, body, bearer string, want int) map[string]any {
	c.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+bearer)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	if w.Code != want {
		c.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body)
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return out
}

// failAuditWrites makes every audit_log insert for this harness's namespace
// fail, for the rest of the transaction.
func (c *consistencyHarness) failAuditWrites() {
	c.t.Helper()
	fn := fmt.Sprintf("fail_audit_%d", time.Now().UnixNano())
	if _, err := c.tx.Exec(c.ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.namespace = '%[2]s' THEN RAISE EXCEPTION 'injected audit write failure'; END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER %[1]s BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION %[1]s();`, fn, c.ns)); err != nil {
		c.t.Fatalf("install audit fault: %v", err)
	}
}

func (c *consistencyHarness) count(sql string, args ...any) int {
	c.t.Helper()
	var n int
	if err := c.tx.QueryRow(c.ctx, sql, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// TestIntegrationRoleChangeAuditFailureIs503: every RBAC change commits with
// its audit row or not at all, and when the row cannot be written the caller
// is told 503 audit_unavailable -- the F139 fail-closed answer that every
// other security grant gives -- rather than a 500 that reads as a server
// fault. Nothing may have changed.
func TestIntegrationRoleChangeAuditFailureIs503(t *testing.T) {
	c := newConsistencyHarness(t)
	base := "/v1/namespaces/" + c.ns
	reader, err := store.GatewayCreateUserWithoutPassword(c.ctx, c.tx, c.ns, "reader@"+c.ns+".test", "Reader", "reader")
	if err != nil {
		t.Fatal(err)
	}
	// A role that exists before the audit store "fails", to delete later.
	c.call("PUT", base+"/roles/Keep", `{"permissions":["runs.view"],"revision":0}`, c.key, 200)

	c.failAuditWrites()
	for _, tc := range []struct{ method, path, body string }{
		{"PUT", base + "/roles/Run%20monitor", `{"permissions":["runs.view"],"revision":0}`},
		{"DELETE", base + "/roles/Keep?revision=1", ``},
		{"PUT", base + "/users/" + reader.ID + "/roles", `{"roles":["Keep"],"revision":0}`},
		{"PUT", base + "/users/" + reader.ID, fmt.Sprintf(`{"email":"moved@%s.test","display_name":"Reader","previous_email":%q,"previous_display_name":"Reader"}`, c.ns, reader.Email)},
	} {
		got := c.call(tc.method, tc.path, tc.body, c.key, http.StatusServiceUnavailable)
		if got["error"] != "audit_unavailable" {
			t.Errorf("%s %s: error %v; want audit_unavailable", tc.method, tc.path, got["error"])
		}
	}
	if n := c.count(`SELECT count(*) FROM rbac_roles WHERE namespace=$1 AND name='Run monitor'`, c.ns); n != 0 {
		t.Error("the role was saved although its audit row was not")
	}
	if n := c.count(`SELECT count(*) FROM rbac_roles WHERE namespace=$1 AND name='Keep'`, c.ns); n != 1 {
		t.Error("the role was deleted although its audit row was not")
	}
	if n := c.count(`SELECT count(*) FROM rbac_user_access WHERE user_id=$1`, reader.ID); n != 0 {
		t.Error("the assignment was saved although its audit row was not")
	}
	if n := c.count(`SELECT count(*) FROM users WHERE id=$1 AND email=$2`, reader.ID, reader.Email); n != 1 {
		t.Error("the profile edit was saved although its audit row was not")
	}
}

// TestIntegrationServiceAccountUpdateSaysWhichFailure: a missing account is
// 404 whatever revision is sent; only an account that exists can be stale
// (409). One conditional UPDATE used to answer 409 for both.
func TestIntegrationServiceAccountUpdateSaysWhichFailure(t *testing.T) {
	c := newConsistencyHarness(t)
	base := "/v1/namespaces/" + c.ns + "/service-accounts"
	sa := c.call("POST", base, `{"name":"deployer","description":"CI","roles":[]}`, c.key, 201)
	id := sa["id"].(string)

	c.call("PUT", base+"/00000000-0000-0000-0000-00000000dead", `{"description":"x","revision":1}`, c.key, 404)
	c.call("PUT", base+"/not-a-uuid", `{"description":"x","revision":1}`, c.key, 404)
	c.call("PUT", base+"/"+id, `{"description":"x","revision":0}`, c.key, 409) // 0: it exists
	c.call("PUT", base+"/"+id, `{"description":"x","revision":7}`, c.key, 409) // stale
	c.call("PUT", base+"/"+id, `{"description":"updated","revision":1}`, c.key, 200)
	c.call("PUT", base+"/"+id, `{"description":"again","revision":1}`, c.key, 409) // now stale
	if n := c.count(`SELECT count(*) FROM service_accounts WHERE id=$1 AND description='updated' AND revision=2`, id); n != 1 {
		t.Error("the accepted update was not stored exactly once")
	}
}

// TestIntegrationTenantCreationAuthorization is F154. POST /v1/platform/tenants
// creates a tenant and its administrator -- the most privileged write in the
// product -- and was guarded on `read`, so the handler's operator check was
// the only thing between any credential and a new tenant. The guard is now
// admin_write, and every principal short of a platform operator's session is
// refused without a tenant being created.
func TestIntegrationTenantCreationAuthorization(t *testing.T) {
	c := newConsistencyHarness(t)
	mk := func(email, role string, operator bool) string {
		u, err := store.GatewayCreateUserWithoutPassword(c.ctx, c.tx, c.ns, email, email, role)
		if err != nil {
			t.Fatal(err)
		}
		if operator {
			if _, err := c.tx.Exec(c.ctx, `INSERT INTO platform_operators(user_id) VALUES($1)`, u.ID); err != nil {
				t.Fatal(err)
			}
		}
		tok, _, err := store.GatewayCreateSession(c.ctx, c.tx, u.ID, time.Hour, "test", "")
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	operator := mk("op@"+c.ns+".test", "admin", true)
	adminSession := mk("admin@"+c.ns+".test", "admin", false)
	readerSession := mk("reader@"+c.ns+".test", "reader", false)
	readerKey := "nx_consistency_reader_" + c.ns
	if _, err := store.GatewayCreateAPIKey(c.ctx, c.tx, c.ns, "reader", "reader", readerKey); err != nil {
		t.Fatal(err)
	}
	// An operator's account bound to a role is not an unbound administrator.
	boundOp, err := store.GatewayCreateUserWithoutPassword(c.ctx, c.tx, c.ns, "bound@"+c.ns+".test", "Bound", "reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.tx.Exec(c.ctx, `INSERT INTO rbac_user_access(user_id,namespace,roles) VALUES($1,$2,'["Viewer"]')`, boundOp.ID, c.ns); err != nil {
		t.Fatal(err)
	}
	boundTok, _, err := store.GatewayCreateSession(c.ctx, c.tx, boundOp.ID, time.Hour, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	scoped, _, err := store.GatewayCreateScopedToken(c.ctx, c.tx, c.ns, "admin-token", "admin", []string{"read", "write", "admin_write"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	body := func(tag string) string {
		return fmt.Sprintf(`{"name":"Acme %[1]s","email":"owner-%[1]s@%[2]s.test","password":"a-long-enough-password"}`, tag, c.ns)
	}
	for name, cred := range map[string]string{
		"reader API key":             readerKey,
		"reader session":             readerSession,
		"role-bound session":         boundTok,
		"admin API key":              c.key,
		"admin scoped token":         scoped,
		"admin session, no operator": adminSession,
	} {
		c.call("POST", "/v1/platform/tenants", body(strings.ReplaceAll(name, " ", "-")), cred, http.StatusForbidden)
	}
	if n := c.count(`SELECT count(*) FROM tenants WHERE contact_email LIKE $1`, "owner-%@"+c.ns+".test"); n != 0 {
		t.Fatalf("%d tenants were created by refused callers", n)
	}

	out := c.call("POST", "/v1/platform/tenants", body("op"), operator, http.StatusCreated)
	tenantID := out["tenant"].(map[string]any)["id"].(string)
	// Audited in both namespaces with the outcome every filter uses (it was
	// "OK", which none matched).
	if n := c.count(`SELECT count(*) FROM audit_log WHERE action='tenant.create' AND target_id=$1 AND outcome='success'`, tenantID); n != 2 {
		t.Errorf("tenant.create audit rows with outcome success: %d, want 2", n)
	}
}
