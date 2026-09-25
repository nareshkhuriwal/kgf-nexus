// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package workergrpc

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

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

// TestIntegrationWorkerTokenFromTheAPIAuthenticatesTheWorkerProtocol is F114
// end to end against Postgres: an admin mints a worker token through
// POST /v1/namespaces/{ns}/tokens, and the worker gRPC interceptor -- built
// exactly as the gateway's main builds it -- accepts it for the token's
// namespace. Revoking it through the API refuses it again, a reader token is
// refused, and a legacy api_keys worker key keeps working.
func TestIntegrationWorkerTokenFromTheAPIAuthenticatesTheWorkerProtocol(t *testing.T) {
	dsn := testdb.DSN(t)
	ctx := context.Background()
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := store.GWOpen(openCtx, dsn, 4)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)

	ns := fmt.Sprintf("f114-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	adminKey := "nx_f114_admin_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, pool, ns, "f114-admin", "admin", adminKey); err != nil {
		t.Fatal(err)
	}
	legacyWorkerKey := "nxs_f114_worker_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, pool, ns, "f114-worker", "worker", legacyWorkerKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM audit_log WHERE namespace=$1`, `DELETE FROM scoped_tokens WHERE namespace=$1`, `DELETE FROM api_keys WHERE namespace=$1`} {
			if _, e := pool.Exec(ctx, q, ns); e != nil {
				t.Logf("cleanup: %v", e)
			}
		}
	})

	// The gateway's own wiring (cmd/api-gateway/main.go).
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	api := rest.New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(rest.NewStoreIdentityAdmin(pool)).Routes()
	post := func(path, body string, want int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+adminKey)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, w.Code, want, w.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	base := "/v1/namespaces/" + ns
	minted := post(base+"/tokens", `{"name":"agents","role":"worker","scopes":["worker_rpc"]}`, http.StatusCreated)
	workerToken, _ := minted["token"].(string)
	tokenID, _ := minted["id"].(string)
	if !strings.HasPrefix(workerToken, "nxtok_") || tokenID == "" {
		t.Fatalf("minted token = %v", minted)
	}
	reader := post(base+"/tokens", `{"name":"dash","role":"reader","scopes":["read"]}`, http.StatusCreated)
	readerToken, _ := reader["token"].(string)

	call := func(credential, namespace string) (authn.Principal, error) {
		md := metadata.Pairs("authorization", "Bearer "+credential)
		var got authn.Principal
		_, err := AuthUnaryInterceptor(auth)(metadata.NewIncomingContext(ctx, md), nil,
			&grpc.UnaryServerInfo{FullMethod: "/nexus.v1.WorkerService/Register"},
			func(ctx context.Context, _ any) (any, error) {
				p, err := (&Server{}).requirePrincipal(ctx, namespace)
				got = p
				return nil, err
			})
		return got, err
	}

	p, err := call(workerToken, ns)
	if err != nil {
		t.Fatalf("the worker token the API minted was refused by the worker protocol: %v", err)
	}
	if p.Role != authn.RoleWorker || p.Namespace != ns || p.KeyID != tokenID {
		t.Errorf("principal = %+v, want the minted worker token", p)
	}
	if _, err := call(workerToken, ns+"-other"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("worker token in another namespace = %v, want PermissionDenied", err)
	}
	if _, err := call(readerToken, ns); status.Code(err) != codes.PermissionDenied {
		t.Errorf("reader token = %v, want PermissionDenied", err)
	}
	if p, err := call(legacyWorkerKey, ns); err != nil || p.Role != authn.RoleWorker {
		t.Errorf("legacy api_keys worker key = %+v, %v; it must keep working", p, err)
	}

	post(base+"/tokens/"+tokenID+"/revoke", `{}`, http.StatusOK)
	if _, err := call(workerToken, ns); status.Code(err) != codes.Unauthenticated {
		t.Errorf("revoked worker token = %v, want Unauthenticated", err)
	}
}
