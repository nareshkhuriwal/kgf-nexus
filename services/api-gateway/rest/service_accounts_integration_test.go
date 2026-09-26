// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

func decodeMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return out
}

func TestIntegrationServiceAccountsAndTokenCeilings(t *testing.T) {
	e := newKGFEnv(t, AuthStandalone)
	ctx := context.Background()
	ns := fmt.Sprintf("sa-%d", time.Now().UnixNano())
	other := ns + "-other"
	testdb.RegisterTenant(t, e.pool, ns)
	testdb.RegisterTenant(t, e.pool, other)
	key := "nx_sa_test_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, e.pool, ns, "sa-admin", "admin", key); err != nil {
		t.Fatal(err)
	}
	base := "/v1/namespaces/" + ns
	call := func(method, path, body, bearer string, want int) map[string]any {
		return decodeMap(t, e.do(method, path, body, "", bearer, want).Body.Bytes())
	}
	e.do("PUT", base+"/roles/Run%20monitor", `{"permissions":["runs.view","workers.view"],"revision":0}`, "", key, 200)
	// Reserved identity grants cannot be put in a custom role.
	e.do("PUT", base+"/roles/Sneaky", `{"permissions":["service_accounts.view","service_accounts.manage"],"revision":0}`, "", key, 403)

	sa := call("POST", base+"/service-accounts", `{"name":"ci-bot","description":"CI","roles":["Run monitor"]}`, key, 201)
	saID := sa["id"].(string)
	e.do("POST", base+"/service-accounts", `{"name":"ci-bot"}`, "", key, 409)
	e.do("POST", base+"/service-accounts", `{"name":"Bad Name!"}`, "", key, 422)
	e.do("POST", base+"/service-accounts", `{"name":"root","roles":["Administrator"]}`, "", key, 409)

	e.do("POST", base+"/tokens", `{"name":"t","service_account_id":"`+saID+`","permissions":["runs.cancel"]}`, "", key, 422)
	e.do("POST", base+"/tokens", `{"name":"t","service_account_id":"`+saID+`"}`, "", key, 422)
	e.do("POST", base+"/tokens", `{"name":"t","service_account_id":"`+saID+`","permissions":["service_accounts.manage"]}`, "", key, 403)
	tok := call("POST", base+"/tokens", `{"name":"ci","service_account_id":"`+saID+`","permissions":["runs.view"]}`, key, 201)
	secret := tok["token"].(string)
	var scopes string
	_ = e.pool.QueryRow(ctx, `SELECT scopes::text FROM scoped_tokens WHERE id=$1`, tok["id"]).Scan(&scopes)
	if scopes != "[]" {
		t.Fatalf("service-account token stored with legacy scopes %s (an old replica would honour them)", scopes)
	}

	e.do("GET", base+"/runs", "", "", secret, 200)
	e.do("GET", base+"/workers", "", "", secret, 403) // in the account's roles, outside the ceiling
	e.do("GET", base+"/users", "", "", secret, 403)
	e.do("GET", "/v1/namespaces/"+other+"/runs", "", "", secret, 403)
	who := call("GET", "/v1/auth/whoami", "", secret, 200)
	if fmt.Sprint(who["effective_permissions"]) != "[runs.view]" || who["service_account_id"] != saID {
		t.Fatalf("whoami %+v", who)
	}

	// Disabling the account stops its tokens at once; enabling restores them.
	e.do("POST", base+"/service-accounts/"+saID+"/disable", "", "", key, 200)
	e.do("GET", base+"/runs", "", "", secret, 401)
	e.do("POST", base+"/tokens", `{"name":"x","service_account_id":"`+saID+`","permissions":["runs.view"]}`, "", key, 409)
	e.do("POST", base+"/service-accounts/"+saID+"/enable", "", "", key, 200)
	e.do("GET", base+"/runs", "", "", secret, 200)
	// Narrowing the account's roles narrows the token immediately.
	e.do("PUT", base+"/service-accounts/"+saID+"/roles", `{"roles":[],"revision":1}`, "", key, 200)
	e.do("GET", base+"/runs", "", "", secret, 403)
	e.do("PUT", base+"/service-accounts/"+saID+"/roles", `{"roles":["Run monitor"],"revision":2}`, "", key, 200)
	e.do("GET", base+"/runs", "", "", secret, 200)

	// A ceilinged legacy token: admin role, only runs.view.
	legacy := call("POST", base+"/tokens", `{"name":"ceil","role":"admin","scopes":["read","write","admin_write"],"permissions":["runs.view"]}`, key, 201)
	lt := legacy["token"].(string)
	e.do("GET", base+"/runs", "", "", lt, 200)
	e.do("GET", base+"/users", "", "", lt, 403)
	e.do("DELETE", base+"/runs/00000000-0000-0000-0000-000000000000", "", "", lt, 403)
	if s := fmt.Sprint(legacy["scopes"]); s != "[read]" {
		t.Fatalf("ceilinged token stored broader legacy scopes: %s", s)
	}
	// Unceilinged legacy tokens are unchanged.
	plain := call("POST", base+"/tokens", `{"name":"plain","role":"reader","scopes":["read"]}`, key, 201)
	e.do("GET", base+"/runs", "", "", plain["token"].(string), 200)
	list := call("GET", base+"/service-accounts", "", key, 200)["service_accounts"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["live_tokens"].(float64) != 1 {
		t.Fatalf("service accounts %+v", list)
	}
	// A reader credential cannot see or manage service accounts.
	e.do("GET", base+"/service-accounts", "", "", plain["token"].(string), 403)
}

func TestIntegrationUsersCursorPagination(t *testing.T) {
	e := newKGFEnv(t, AuthStandalone)
	ctx := context.Background()
	ns := fmt.Sprintf("page-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, e.pool, ns)
	key := "nx_page_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, e.pool, ns, "k", "admin", key); err != nil {
		t.Fatal(err)
	}
	base := "/v1/namespaces/" + ns
	for i := 0; i < 7; i++ {
		if _, err := store.GatewayCreateUser(ctx, e.pool, ns, fmt.Sprintf("user%d@p.test", i), fmt.Sprintf("User %d", i), "reader", "a-long-password-1234"); err != nil {
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
		var page struct {
			Users []map[string]any `json:"users"`
			Next  *string          `json:"next_cursor"`
		}
		_ = json.Unmarshal(e.do("GET", path, "", "", key, 200).Body.Bytes(), &page)
		pages++
		for _, u := range page.Users {
			id := u["id"].(string)
			if seen[id] {
				t.Fatalf("user %s on two pages", id)
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
	var res struct {
		Users []map[string]any `json:"users"`
	}
	_ = json.Unmarshal(e.do("GET", base+"/users?q=USER3", "", "", key, 200).Body.Bytes(), &res)
	if len(res.Users) != 1 || res.Users[0]["email"] != "user3@p.test" {
		t.Fatalf("server-side search %+v", res)
	}
	_ = json.Unmarshal(e.do("GET", base+"/users?q=%25", "", "", key, 200).Body.Bytes(), &res)
	if len(res.Users) != 0 {
		t.Fatal("% matched everything: LIKE metacharacters must be literal")
	}
	e.do("GET", base+"/users?cursor=not-a-cursor", "", "", key, 400)
}
