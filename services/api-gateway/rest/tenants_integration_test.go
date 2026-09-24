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

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func TestIntegrationTenantRegistrationIsolation(t *testing.T) {
	db := attentionPool(t)
	ctx := context.Background()
	h := New(db, authn.New(db).WithRBAC().WithIdentity(authn.StoreIdentity{DB: db}), nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(db)).Routes()
	call := func(path, body, token string, status int) *httptest.ResponseRecorder {
		t.Helper()
		method := "POST"
		if body == "" {
			method = "GET"
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	call("/v1/auth/register", `{"name":"Invalid","email":"bad","password":"short"}`, "", 422)
	register := func(email string) string {
		w := call("/v1/auth/register", `{"name":"Test customer","email":"`+email+`","password":"strong-test-password"}`, "", 201)
		var response struct {
			Tenant store.TenantRow `json:"tenant"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Tenant.ID == "" || response.Tenant.ID != response.Tenant.Namespace {
			t.Fatal("tenant identity missing")
		}
		return response.Tenant.ID
	}
	// Unique per run: registration emails are globally unique, so fixed
	// addresses made this test pass exactly once per database.
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	emailA, emailB := "tenant-a-"+run+"@example.test", "tenant-b-"+run+"@example.test"
	a := register(emailA)
	b := register(emailB)
	call("/v1/auth/register", `{"name":"Duplicate","email":"`+emailA+`","password":"strong-test-password"}`, "", 409)
	// Scoped to this run's addresses: the database is shared with every other
	// test that registers a tenant.
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE registration_email = ANY($1)`,
		[]string{emailA, emailB}).Scan(&count); err != nil || count != 2 {
		t.Fatalf("registration atomicity: count=%d err=%v", count, err)
	}
	w := call("/v1/auth/login", `{"namespace":"`+a+`","email":"`+emailA+`","password":"strong-test-password"}`, "", 200)
	var login struct {
		Token string `json:"token"`
	}
	json.Unmarshal(w.Body.Bytes(), &login)
	call("/v1/account/tenant", "", login.Token, 200)
	call("/v1/namespaces/"+a+"/users", "", login.Token, 200)
	call("/v1/namespaces/"+b+"/users", "", login.Token, 403)
	call("/v1/namespaces/"+b+"/runs", "", login.Token, 403)
	call("/v1/platform/tenants", "", login.Token, 403)
	var owner string
	if err := db.QueryRow(ctx, `SELECT u.role FROM tenants t JOIN users u ON u.id=t.owner_user_id WHERE t.id=$1 AND u.namespace=t.namespace`, a).Scan(&owner); err != nil || owner != "admin" {
		t.Fatalf("owner: %s %v", owner, err)
	}
}
