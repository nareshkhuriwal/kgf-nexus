// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

// The RBAC gaps closed by ADR-0024: delegated identity administration,
// service accounts and token ceilings, cross-namespace owners, users
// pagination, and the cluster permissions.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func (e *ssoEnv) session(ns, email, role string) (store.UserRow, string) {
	e.t.Helper()
	ctx := context.Background()
	u, err := store.GatewayCreateUser(ctx, e.pool, ns, email, email, role, "a-long-password-1234")
	if err != nil {
		e.t.Fatal(err)
	}
	tok, _, err := store.GatewayCreateSession(ctx, e.pool, u.ID, time.Hour, "test", "")
	if err != nil {
		e.t.Fatal(err)
	}
	return u, tok
}

func TestIntegrationDelegatedIdentityAdministration(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("rbac-deleg")
	base := "/v1/namespaces/" + ns
	admin, _ := e.session(ns, "admin@d.test", "admin")
	viewer, _ := e.session(ns, "viewer@d.test", "reader")
	delegate, dtok := e.session(ns, "delegate@d.test", "reader")

	// A custom role that delegates part of user administration. Identity
	// permissions are now storable in custom roles...
	e.call("PUT", base+"/roles/User%20admin", `{"permissions":["users.view","users.disable","users.enable","users.password.reset",
		"users.sessions.revoke","users.roles.assign","roles.view","runs.view"],"revision":0}`, key, 200)
	// ...but owners.grant is never delegable, not even by an administrator.
	e.call("PUT", base+"/roles/Owner%20maker", `{"permissions":["owners.view","owners.grant"],"revision":0}`, key, 403)
	e.call("PUT", base+"/roles/Run%20viewer", `{"permissions":["runs.view"],"revision":0}`, key, 200)
	e.call("PUT", base+"/users/"+delegate.ID+"/roles", `{"roles":["User admin"],"revision":0}`, key, 200)
	e.call("PUT", base+"/users/"+viewer.ID+"/roles", `{"roles":["Run viewer"],"revision":0}`, key, 200)

	// The delegate can administer an account no more powerful than itself.
	e.call("GET", base+"/users", "", dtok, 200)
	e.call("POST", base+"/users/"+viewer.ID+"/sessions/revoke", "", dtok, 200)
	e.call("POST", base+"/users/"+viewer.ID+"/password", `{"password":"another-long-password"}`, dtok, 200)
	e.call("POST", base+"/users/"+viewer.ID+"/disable", "", dtok, 200)
	e.call("POST", base+"/users/"+viewer.ID+"/enable", "", dtok, 200)
	e.call("PUT", base+"/users/"+viewer.ID+"/roles", `{"roles":["Run viewer"],"revision":1}`, dtok, 200)

	// Target check: never an Administrator (takeover via password reset).
	for _, path := range []string{"/password", "/disable", "/sessions/revoke"} {
		w := e.call("POST", base+"/users/"+admin.ID+path, `{"password":"another-long-password"}`, dtok, 403)
		if !contains(w.Body.String(), "target_above_actor") {
			t.Fatalf("%s on an administrator: %s", path, w.Body.String())
		}
	}
	// Content check: cannot hand out grants it does not hold.
	e.call("PUT", base+"/users/"+viewer.ID+"/roles", `{"roles":["Workflow Operator"],"revision":2}`, dtok, 403)
	// Not granted: creating users, editing profiles, roles, tokens.
	e.call("POST", base+"/users", `{"email":"x@d.test","password":"a-long-password-1234","role_name":"Run viewer"}`, dtok, 403)
	e.call("PUT", base+"/roles/Mine", `{"permissions":["runs.view"],"revision":0}`, dtok, 403)
	e.call("POST", base+"/tokens", `{"name":"t","scopes":["read"]}`, dtok, 403)
	// Self-assignment stays refused.
	e.call("PUT", base+"/users/"+delegate.ID+"/roles", `{"roles":["User admin"],"revision":1}`, dtok, 409)

	// A delegate with tokens.create must set a ceiling within its own grants,
	// and may not mint an administrator account.
	e.call("PUT", base+"/roles/Token%20admin", `{"permissions":["tokens.view","tokens.create","users.view","users.create","users.roles.assign","roles.view","runs.view"],"revision":0}`, key, 200)
	_, ttok := e.session(ns, "tokens@d.test", "reader")
	var tokUser string
	_ = e.pool.QueryRow(context.Background(), `SELECT id::text FROM users WHERE namespace=$1 AND email='tokens@d.test'`, ns).Scan(&tokUser)
	e.call("PUT", base+"/users/"+tokUser+"/roles", `{"roles":["Token admin"],"revision":0}`, key, 200)
	w := e.call("POST", base+"/tokens", `{"name":"legacy","role":"admin","scopes":["read","write","admin_write"]}`, ttok, 403)
	if !contains(w.Body.String(), "ceiling_required") {
		t.Fatal(w.Body.String())
	}
	e.call("POST", base+"/tokens", `{"name":"ceil","role":"reader","scopes":["read"],"permissions":["runs.view","users.view"]}`, ttok, 201)
	e.call("POST", base+"/tokens", `{"name":"over","role":"reader","scopes":["read"],"permissions":["runs.cancel"]}`, ttok, 403)
	e.call("POST", base+"/users", `{"email":"boss@d.test","password":"a-long-password-1234","role_name":"Administrator"}`, ttok, 403)
	e.call("POST", base+"/users", `{"email":"boss@d.test","password":"a-long-password-1234","role":"admin"}`, ttok, 403)
	e.call("POST", base+"/users", `{"email":"run@d.test","password":"a-long-password-1234","role_name":"Run viewer"}`, ttok, 201)

	// Every refusal of a target was audited.
	if e.auditActions(ns)["user.password_reset:denied"] == 0 {
		t.Error("denied delegated password reset was not audited")
	}
}

func TestIntegrationServiceAccountsAndTokenCeilings(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("rbac-sa")
	nsOther, _ := e.tenant("rbac-sa-other")
	base := "/v1/namespaces/" + ns
	e.call("PUT", base+"/roles/Run%20monitor", `{"permissions":["runs.view","workers.view"],"revision":0}`, key, 200)

	sa := decodeAs[map[string]any](t, e.call("POST", base+"/service-accounts", `{"name":"ci-bot","description":"CI","roles":["Run monitor"]}`, key, 201))
	saID := sa["id"].(string)
	e.call("POST", base+"/service-accounts", `{"name":"ci-bot"}`, key, 409)
	e.call("POST", base+"/service-accounts", `{"name":"Bad Name!"}`, key, 422)
	e.call("POST", base+"/service-accounts", `{"name":"root","roles":["Administrator"]}`, key, 409)

	// A ceiling outside the account's roles is refused at mint time.
	e.call("POST", base+"/tokens", `{"name":"t","service_account_id":"`+saID+`","permissions":["runs.cancel"]}`, key, 422)
	e.call("POST", base+"/tokens", `{"name":"t","service_account_id":"`+saID+`"}`, key, 422)
	tok := decodeAs[map[string]any](t, e.call("POST", base+"/tokens", `{"name":"ci","service_account_id":"`+saID+`","permissions":["runs.view"]}`, key, 201))
	secret := tok["token"].(string)
	if tok["service_account_id"] != saID {
		t.Fatalf("token payload %+v", tok)
	}
	// Stored with no legacy scopes: an old replica would grant it nothing.
	var scopes string
	_ = e.pool.QueryRow(context.Background(), `SELECT scopes::text FROM scoped_tokens WHERE id=$1`, tok["id"]).Scan(&scopes)
	if scopes != "[]" {
		t.Fatalf("service-account token stored with legacy scopes %s", scopes)
	}

	e.call("GET", base+"/runs", "", secret, 200)
	e.call("GET", base+"/workers", "", secret, 403) // in the account's roles, outside the ceiling
	e.call("POST", base+"/runs", `{}`, secret, 403)
	e.call("GET", base+"/users", "", secret, 403)
	e.call("GET", "/v1/namespaces/"+nsOther+"/runs", "", secret, 403)
	who := decodeAs[whoami](t, e.call("GET", "/v1/auth/whoami", "", secret, 200))
	if len(who.Effective) != 1 || who.Effective[0] != "runs.view" {
		t.Fatalf("effective permissions %v", who.Effective)
	}

	// Disabling the account kills its tokens at once; enabling restores them.
	e.call("POST", base+"/service-accounts/"+saID+"/disable", "", key, 200)
	e.call("GET", base+"/runs", "", secret, 401)
	e.call("POST", base+"/tokens", `{"name":"x","service_account_id":"`+saID+`","permissions":["runs.view"]}`, key, 409)
	e.call("POST", base+"/service-accounts/"+saID+"/enable", "", key, 200)
	e.call("GET", base+"/runs", "", secret, 200)
	// Narrowing the account's roles narrows the token immediately.
	e.call("PUT", base+"/service-accounts/"+saID+"/roles", `{"roles":[],"revision":1}`, key, 200)
	e.call("GET", base+"/runs", "", secret, 403)
	// A role bound to a service account cannot be deleted.
	e.call("PUT", base+"/service-accounts/"+saID+"/roles", `{"roles":["Run monitor"],"revision":2}`, key, 200)
	e.call("DELETE", base+"/roles/Run%20monitor?revision=1", "", key, 409)

	// Legacy token with a ceiling: admin role, but only runs.view.
	legacy := decodeAs[map[string]any](t, e.call("POST", base+"/tokens", `{"name":"ceil","role":"admin","scopes":["read","write","admin_write"],"permissions":["runs.view"]}`, key, 201))
	lt := legacy["token"].(string)
	e.call("GET", base+"/runs", "", lt, 200)
	e.call("GET", base+"/users", "", lt, 403)
	e.call("DELETE", base+"/runs/00000000-0000-0000-0000-000000000000", "", lt, 403)
	if s := fmt.Sprint(legacy["scopes"]); s != "[read]" {
		t.Fatalf("ceilinged token stored broader legacy scopes: %s", s)
	}
	// Legacy tokens without a ceiling are unchanged.
	plain := decodeAs[map[string]any](t, e.call("POST", base+"/tokens", `{"name":"plain","role":"reader","scopes":["read"]}`, key, 201))
	e.call("GET", base+"/runs", "", plain["token"].(string), 200)
	list := decodeAs[map[string][]map[string]any](t, e.call("GET", base+"/service-accounts", "", key, 200))["service_accounts"]
	if len(list) != 1 || list[0]["live_tokens"].(float64) != 1 {
		t.Fatalf("service accounts %+v", list)
	}
}

func TestIntegrationCrossNamespaceOwners(t *testing.T) {
	e := newSSOEnv(t)
	nsA, keyA := e.tenant("own-a")
	nsB, _ := e.tenant("own-b")
	_, adminATok := e.session(nsA, "admin@a.test", "admin")
	owner, ownerTok := e.session(nsB, "platform@b.test", "reader")
	_ = keyA

	e.call("GET", "/v1/namespaces/"+nsA+"/runs", "", ownerTok, 403)
	// An API key (even admin) cannot grant ownership; a human administrator can.
	e.call("POST", "/v1/namespaces/"+nsA+"/owners", `{"user_id":"`+owner.ID+`","email":"platform@b.test"}`, keyA, 403)
	// The email must match the id: no probing another tenant's accounts.
	e.call("POST", "/v1/namespaces/"+nsA+"/owners", `{"user_id":"`+owner.ID+`","email":"someone@b.test"}`, adminATok, 404)
	e.call("POST", "/v1/namespaces/"+nsA+"/owners", `{"user_id":"`+owner.ID+`","email":"platform@b.test"}`, adminATok, 201)
	e.call("POST", "/v1/namespaces/"+nsA+"/owners", `{"user_id":"`+owner.ID+`","email":"platform@b.test"}`, adminATok, 409)

	e.call("GET", "/v1/namespaces/"+nsA+"/runs", "", ownerTok, 200)
	e.call("GET", "/v1/namespaces/"+nsA+"/users", "", ownerTok, 200)
	// Still bound by its own namespace's roles at home (legacy reader here).
	e.call("GET", "/v1/namespaces/"+nsB+"/users", "", ownerTok, 403)
	mem := decodeAs[map[string][]map[string]any](t, e.call("GET", "/v1/auth/memberships", "", ownerTok, 200))["memberships"]
	if len(mem) != 2 || mem[1]["namespace"] != nsA || mem[1]["relation"] != "owner" {
		t.Fatalf("memberships %+v", mem)
	}
	// Acting in A is attributed to the real person in A's audit trail.
	e.call("PUT", "/v1/namespaces/"+nsA+"/roles/By%20owner", `{"permissions":["runs.view"],"revision":0}`, ownerTok, 200)
	var actor, kind string
	_ = e.pool.QueryRow(context.Background(), `SELECT actor_id::text, actor_kind FROM audit_log WHERE namespace=$1 AND action='role.save' ORDER BY id DESC LIMIT 1`, nsA).Scan(&actor, &kind)
	if actor != owner.ID || kind != "session" {
		t.Fatalf("owner action attributed to %q/%q", actor, kind)
	}
	// Revocation takes effect on the next request.
	e.call("DELETE", "/v1/namespaces/"+nsA+"/owners/"+owner.ID, "", adminATok, 200)
	e.call("GET", "/v1/namespaces/"+nsA+"/runs", "", ownerTok, 403)
	// A third namespace the owner was never granted stays closed.
	nsC, _ := e.tenant("own-c")
	e.call("GET", "/v1/namespaces/"+nsC+"/runs", "", ownerTok, 403)
}

func TestIntegrationUsersCursorPagination(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("rbac-page")
	base := "/v1/namespaces/" + ns
	for i := 0; i < 7; i++ {
		if _, err := store.GatewayCreateUser(context.Background(), e.pool, ns, fmt.Sprintf("user%d@p.test", i), fmt.Sprintf("User %d", i), "reader", "a-long-password-1234"); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor, pages := "", 0
	for {
		path := base + "/users?limit=3"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		page := decodeAs[struct {
			Users []map[string]any `json:"users"`
			Next  *string          `json:"next_cursor"`
		}](t, e.call("GET", path, "", key, 200))
		pages++
		for _, u := range page.Users {
			id := u["id"].(string)
			if seen[id] {
				t.Fatalf("user %s appeared on two pages", id)
			}
			seen[id] = true
		}
		if page.Next == nil {
			break
		}
		cursor = *page.Next
	}
	if len(seen) != 7 || pages != 3 {
		t.Fatalf("saw %d users over %d pages", len(seen), pages)
	}
	res := decodeAs[map[string][]map[string]any](t, e.call("GET", base+"/users?q=USER3", "", key, 200))["users"]
	if len(res) != 1 || res[0]["email"] != "user3@p.test" {
		t.Fatalf("server-side search %+v", res)
	}
	// LIKE metacharacters are literal.
	if res := decodeAs[map[string][]any](t, e.call("GET", base+"/users?q=%25", "", key, 200))["users"]; len(res) != 0 {
		t.Fatal("% matched everything")
	}
	e.call("GET", base+"/users?cursor=not-a-cursor", "", key, 400)
}

func TestIntegrationClusterPermissionsSplit(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("rbac-cluster")
	base := "/v1/namespaces/" + ns
	e.call("PUT", base+"/roles/Infra%20view", `{"permissions":["infrastructure.view"],"revision":0}`, key, 200)
	e.call("PUT", base+"/roles/Cluster%20view", `{"permissions":["clusters.view"],"revision":0}`, key, 200)
	u, tok := e.session(ns, "infra@c.test", "reader")
	e.call("PUT", base+"/users/"+u.ID+"/roles", `{"roles":["Infra view"],"revision":0}`, key, 200)
	e.call("GET", base+"/platform/clusters", "", tok, 403)
	e.call("PUT", base+"/users/"+u.ID+"/roles", `{"roles":["Cluster view"],"revision":1}`, key, 200)
	w := e.call("GET", base+"/platform/clusters", "", tok, 0)
	if w.Code == http.StatusForbidden {
		t.Fatalf("clusters.view did not open the cluster listing: %s", w.Body.String())
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
