// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// fakeIdentity resolves sessions and scoped tokens without a database, so these
// tests exercise the interceptor's decision, not Postgres.
type fakeIdentity struct {
	tokens   map[string]store.ResolvedToken
	sessions map[string]store.ResolvedSession
	err      error
}

func (f *fakeIdentity) ResolveSession(_ context.Context, presented string) (store.ResolvedSession, error) {
	if f.err != nil {
		return store.ResolvedSession{}, f.err
	}
	if s, ok := f.sessions[presented]; ok {
		return s, nil
	}
	return store.ResolvedSession{}, store.ErrIdentityUnknown
}

func (f *fakeIdentity) ResolveScopedToken(_ context.Context, presented string) (store.ResolvedToken, error) {
	if f.err != nil {
		return store.ResolvedToken{}, f.err
	}
	if t, ok := f.tokens[presented]; ok {
		return t, nil
	}
	return store.ResolvedToken{}, store.ErrIdentityUnknown
}

func (f *fakeIdentity) TouchSession(context.Context, string) error     { return nil }
func (f *fakeIdentity) TouchScopedToken(context.Context, string) error { return nil }

func newFakeIdentity() *fakeIdentity {
	return &fakeIdentity{
		tokens: map[string]store.ResolvedToken{
			// What POST /v1/namespaces/{ns}/tokens mints for an agent.
			"nxtok_worker": {TokenID: "tok-w", Namespace: "team-ml", Name: "agents", Role: "worker", Scopes: []string{"worker_rpc"}},
			"nxtok_reader": {TokenID: "tok-r", Namespace: "team-ml", Name: "dash", Role: "reader", Scopes: []string{"read"}},
			// A worker-role token that was not given the worker_rpc scope.
			"nxtok_worker_unscoped": {TokenID: "tok-wu", Namespace: "team-ml", Name: "odd", Role: "worker", Scopes: []string{"read"}},
			// An admin automation token, scoped away from worker RPCs.
			"nxtok_admin_read": {TokenID: "tok-a", Namespace: "team-ml", Name: "ci", Role: "admin", Scopes: []string{"read", "write"}},
		},
		sessions: map[string]store.ResolvedSession{
			"nxsess_admin": {SessionID: "sess-1", ExpiresAt: time.Now().Add(time.Hour),
				User: store.UserRow{ID: "user-1", Namespace: "team-ml", Email: "a@example.com", Role: "admin"}},
		},
	}
}

// intercept runs one unary call through the interceptor, with the worker
// handlers' own namespace check (requirePrincipal) as the handler.
func intercept(t *testing.T, ids authn.IdentityStore, credential, namespace string) (authn.Principal, error) {
	t.Helper()
	auth := authn.New(nil).WithIdentity(ids)
	ctx := context.Background()
	if credential != "" {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer "+credential))
	} else {
		ctx = metadata.NewIncomingContext(ctx, metadata.MD{})
	}
	var got authn.Principal
	handler := func(ctx context.Context, _ any) (any, error) {
		p, err := (&Server{}).requirePrincipal(ctx, namespace)
		got = p
		return nil, err
	}
	_, err := AuthUnaryInterceptor(auth)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/nexus.v1.WorkerService/Register"}, handler)
	return got, err
}

// TestWorkerTokenMintedByTheAPIAuthenticatesAnAgent is F114: the nxtok_ token
// the tokens API issues with role worker and scope worker_rpc must be accepted
// by the worker protocol, in its own namespace only.
func TestWorkerTokenMintedByTheAPIAuthenticatesAnAgent(t *testing.T) {
	p, err := intercept(t, newFakeIdentity(), "nxtok_worker", "team-ml")
	if err != nil {
		t.Fatalf("worker token refused: %v", err)
	}
	if p.Role != authn.RoleWorker || p.Kind != authn.KindScopedToken || p.KeyID != "tok-w" {
		t.Errorf("principal = %+v, want the worker token", p)
	}
	if _, err := intercept(t, newFakeIdentity(), "nxtok_worker", "other-team"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("worker token in another namespace = %v, want PermissionDenied", err)
	}
}

// TestInterceptorRefusesCredentialsWithoutWorkerRPC: accepting scoped tokens
// and sessions must not widen who may speak the worker protocol.
func TestInterceptorRefusesCredentialsWithoutWorkerRPC(t *testing.T) {
	for _, cred := range []string{"nxtok_reader", "nxtok_worker_unscoped", "nxtok_admin_read", "nxsess_admin"} {
		handled := false
		auth := authn.New(nil).WithIdentity(newFakeIdentity())
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+cred))
		_, err := AuthUnaryInterceptor(auth)(ctx, nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
			handled = true
			return nil, nil
		})
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("%s: %v, want PermissionDenied", cred, err)
		}
		if handled {
			t.Errorf("%s reached the handler; the capability must be checked at the transport", cred)
		}
	}
}

func TestInterceptorAuthenticationFailures(t *testing.T) {
	cases := []struct {
		name, cred string
		ids        *fakeIdentity
		want       codes.Code
	}{
		{"no credential", "", newFakeIdentity(), codes.Unauthenticated},
		{"unknown token", "nxtok_nope", newFakeIdentity(), codes.Unauthenticated},
		{"identity store down", "nxtok_worker", &fakeIdentity{err: errors.New("connection refused")}, codes.Unavailable},
	}
	for _, c := range cases {
		if _, err := intercept(t, c.ids, c.cred, "team-ml"); status.Code(err) != c.want {
			t.Errorf("%s: %v, want %s", c.name, err, c.want)
		}
	}
}
