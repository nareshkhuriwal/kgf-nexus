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

func TestIntegrationEditUser(t *testing.T) {
	ctx := context.Background()
	tx, err := attentionPool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ns := fmt.Sprintf("edit-user-%d", time.Now().UnixNano())
	if _, err = tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'edit test')`, ns); err != nil {
		t.Fatal(err)
	}
	key := "nx_edit_test_" + ns
	readerKey := "nx_edit_reader_" + ns
	for _, v := range []struct{ key, role string }{{key, "admin"}, {readerKey, "reader"}} {
		if _, err = store.GatewayCreateAPIKey(ctx, tx, ns, "test", v.role, v.key); err != nil {
			t.Fatal(err)
		}
	}
	user, err := store.GatewayCreateUserWithoutPassword(ctx, tx, ns, "old-"+ns+"@example.test", "Original", "reader")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.GatewayCreateUserWithoutPassword(ctx, tx, ns, "taken-"+ns+"@example.test", "Other", "reader")
	if err != nil {
		t.Fatal(err)
	}
	auth := authn.New(tx).WithRBAC().WithIdentity(authn.StoreIdentity{DB: tx})
	h := New(tx, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(tx)).Routes()
	body := map[string]any{"email": "new-" + ns + "@example.test", "display_name": "Updated", "previous_email": user.Email, "previous_display_name": user.DisplayName}
	path := "/v1/namespaces/" + ns + "/users/" + user.ID
	call := func(target, bearer string, b map[string]any, want int) {
		t.Helper()
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest("PUT", target, strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("got %d want %d: %s", w.Code, want, w.Body)
		}
	}
	call(path, readerKey, body, 403)
	body["email"] = "invalid"
	call(path, key, body, 400)
	body["email"] = other.Email
	call(path, key, body, 409)
	body["email"] = "new-" + ns + "@example.test"
	call(path, key, body, 200)
	call(path, key, body, 409) // stale original profile cannot overwrite the update
	updated, err := store.GatewayGetUser(ctx, tx, ns, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Email != body["email"] || updated.DisplayName != "Updated" || updated.Role != user.Role || updated.ID != user.ID || !updated.CreatedAt.Equal(user.CreatedAt) {
		t.Fatal("incorrect profile update")
	}
	var audit int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE namespace=$1 AND action='user.update'`, ns).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("audit=%d err=%v", audit, err)
	}
	// A valid ID from another namespace is not editable through this namespace.
	ns2 := ns + "-other"
	if _, err = tx.Exec(ctx, `INSERT INTO tenants(namespace,name) VALUES($1,'other')`, ns2); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.GatewayCreateUserWithoutPassword(ctx, tx, ns2, "foreign-"+ns+"@example.test", "Foreign", "reader")
	if err != nil {
		t.Fatal(err)
	}
	call("/v1/namespaces/"+ns+"/users/"+foreign.ID, key, body, 404)
	body["role"] = "admin"
	call(path, key, body, 400) // no profile-based privilege change
}
