// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIntegrationRBACAssignmentsAndRevocation(t *testing.T) {
	pool := attentionPool(t)
	ctx := context.Background()
	ns := fmt.Sprintf("rbac-test-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	key := "nx_rbac_test_" + ns
	_, err := store.GatewayCreateAPIKey(ctx, pool, ns, "test-admin", "admin", key)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.GatewayCreateUserWithoutPassword(ctx, pool, ns, "rbac@example.test", "RBAC test", "reader")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.GatewayCreateSession(ctx, pool, user.ID, time.Hour, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// audit_log rows stay: since 0033 they are removed only by audit_purge(),
		// and the namespace is unique to this test.
		for _, sql := range []string{`DELETE FROM rbac_user_access WHERE namespace=$1`, `DELETE FROM rbac_roles WHERE namespace=$1`, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE namespace=$1)`, `DELETE FROM users WHERE namespace=$1`, `DELETE FROM api_keys WHERE namespace=$1`} {
			if _, e := pool.Exec(ctx, sql, ns); e != nil {
				t.Errorf("cleanup: %v", e)
			}
		}
	})
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	h := New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(pool)).Routes()
	call := func(method, path, body, bearer string, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d expected %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	base := "/v1/namespaces/" + ns
	admin, err := store.GatewayCreateUserWithoutPassword(ctx, pool, ns, "admin@example.test", "Recovery admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	adminToken, _, err := store.GatewayCreateSession(ctx, pool, admin.ID, time.Hour, "setup-test", "")
	if err != nil {
		t.Fatal(err)
	}
	who := call("GET", "/v1/auth/whoami", "", adminToken, 200)
	var adminIdentity struct {
		Roles       []string `json:"roles"`
		Permissions []string `json:"effective_permissions"`
	}
	if err = json.Unmarshal(who.Body.Bytes(), &adminIdentity); err != nil {
		t.Fatal(err)
	}
	if len(adminIdentity.Roles) != 1 || adminIdentity.Roles[0] != authn.AdministratorRole || len(adminIdentity.Permissions) != len(authn.Catalog()) {
		t.Fatal("new administrator did not automatically receive the system role")
	}
	catalog := call("GET", base+"/roles", "", adminToken, 200)
	var catalogBody struct {
		Roles []struct {
			Name    string `json:"name"`
			Builtin bool   `json:"builtin"`
		} `json:"roles"`
	}
	if err = json.Unmarshal(catalog.Body.Bytes(), &catalogBody); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, role := range catalogBody.Roles {
		if role.Name == authn.AdministratorRole {
			count++
			if !role.Builtin {
				t.Fatal("administrator role is editable")
			}
		}
	}
	if count != 1 {
		t.Fatal("expected exactly one Administrator role")
	}
	call("PUT", base+"/roles/Administrator", `{"permissions":[],"revision":0}`, adminToken, 409)
	call("PUT", base+"/roles/administrator", `{"permissions":[],"revision":0}`, adminToken, 409)
	call("DELETE", base+"/roles/Administrator?revision=0", "", adminToken, 409)
	call("PUT", base+"/users/"+user.ID+"/roles", `{"roles":["Administrator"],"revision":0}`, adminToken, 409)
	call("POST", base+"/users/"+admin.ID+"/disable", `{}`, key, 409)
	call("PUT", base+"/users/"+admin.ID+"/roles", `{"roles":[],"revision":0}`, key, 409)
	call("PUT", base+"/roles/Run%20monitor", `{"permissions":["runs.view"],"revision":0}`, key, 200)
	call("PUT", base+"/roles/Run%20monitor", `{"permissions":["runs.view"],"revision":0}`, key, 409)
	created := call("POST", base+"/users", `{"email":"created@example.test","password":"test-password-12345","role_name":"Run monitor"}`, key, 201)
	var createdUser struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(created.Body.Bytes(), &createdUser); err != nil {
		t.Fatal(err)
	}
	createdToken, _, err := store.GatewayCreateSession(ctx, pool, createdUser.ID, time.Hour, "create-test", "")
	if err != nil {
		t.Fatal(err)
	}
	call("GET", base+"/runs", "", createdToken, 200)
	call("GET", base+"/specs", "", createdToken, 403)
	call("PUT", base+"/users/"+createdUser.ID+"/roles", `{"roles":[],"revision":1}`, key, 200)
	call("POST", base+"/users", `{"email":"invalid@example.test","password":"test-password-12345","role_name":"Does not exist"}`, key, 422)
	var invalidCount int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE namespace=$1 AND email='invalid@example.test'`, ns).Scan(&invalidCount); err != nil || invalidCount != 0 {
		t.Fatal("failed creation left an account behind")
	}
	call("POST", base+"/users", `{"email":"new-admin@example.test","password":"test-password-12345","role_name":"Administrator"}`, key, 201)
	call("PUT", base+"/users/"+user.ID+"/roles", `{"roles":["Run monitor"],"revision":0}`, key, 200)
	call("GET", base+"/runs", "", token, 200)
	call("GET", base+"/specs", "", token, 403)
	call("GET", "/v1/namespaces/another/runs", "", token, 403)
	call("POST", base+"/runs", `{}`, token, 403)
	call("PUT", base+"/roles/Escalation", `{"permissions":["runs.view"]}`, token, 403)
	call("DELETE", base+"/roles/Run%20monitor?revision=1", "", key, 409)
	call("PUT", base+"/users/"+user.ID+"/roles", `{"roles":[],"revision":0}`, key, 409)
	call("PUT", base+"/users/"+user.ID+"/roles", `{"roles":[],"revision":1}`, key, 200)
	call("GET", base+"/runs", "", token, 403)
	w := call("GET", "/v1/auth/whoami", "", token, 200)
	var me struct {
		Permissions []string `json:"effective_permissions"`
	}
	if json.Unmarshal(w.Body.Bytes(), &me) != nil || len(me.Permissions) != 0 {
		t.Fatal("empty roles must expose no module grants")
	}
	call("DELETE", base+"/roles/Run%20monitor?revision=1", "", key, 204)
}
