// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

// KGF context authentication end to end through the real handlers and a real
// PostgreSQL (ADR-0024). Contexts are minted in tenantctx's exact format
// (kgfctx.Signer, held to kgf-cloud's code by kgfctx's conformance test).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

type kgfEnv struct {
	t      *testing.T
	pool   *pgxpool.Pool
	signer kgfctx.Signer
	h      http.Handler
	prefix string
}

func newKGFEnv(t *testing.T, mode AuthMode) *kgfEnv {
	t.Helper()
	pool := attentionPool(t)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	pub := key.Public().(ed25519.PublicKey)
	signer := kgfctx.NewSigner("kgf-gateway", key)
	kgfAuth := &authn.KGFAuthenticator{
		Verifier: kgfctx.Verifier{Keys: kgfctx.StaticKeys{kgfctx.KeyID(pub): pub}, Audience: "kgf-nexus",
			UserIssuers: []string{"kgf-gateway"}, Seen: kgfctx.NewMemoryReplayGuard()},
		Provisioner: authn.StoreProvisioner{DB: pool},
	}
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	h := New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(pool)).WithKGF(mode, kgfAuth).Routes()
	return &kgfEnv{t: t, pool: pool, signer: signer, h: h, prefix: fmt.Sprintf("t%d", time.Now().UnixNano())}
}

func (e *kgfEnv) ctx(org, sub, rg, principal, role, requestID string) string {
	e.t.Helper()
	tok, err := e.signer.MintFor("kgf-nexus", kgfctx.Context{Organization: e.prefix + org, Subscription: sub, ResourceGroup: rg,
		Principal: principal, Role: role, RequestID: requestID}, time.Minute, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *kgfEnv) ns(org, sub, rg string) string {
	return store.KGFNamespace(store.KGFScope{Organization: e.prefix + org, Subscription: sub, ResourceGroup: rg})
}

func (e *kgfEnv) do(method, path, body, kgfToken, bearer string, want int) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if kgfToken != "" {
		r.Header.Set(kgfctx.Header, kgfToken)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if want != 0 && w.Code != want {
		e.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}

func TestIntegrationKGFContextAuthAndAudit(t *testing.T) {
	e := newKGFEnv(t, AuthKGF)
	ns := e.ns("acme", "sub-1", "rg-data")
	base := "/v1/namespaces/" + ns

	// First use provisions the tenant; whoami shows the mapping.
	w := e.do("GET", "/v1/auth/whoami", "", e.ctx("acme", "Sub-1", "RG-Data", "user:alice@acme.test", "operator", "req-a1"), "", 200)
	var who struct {
		Namespace string             `json:"namespace"`
		Name      string             `json:"name"`
		Kind      string             `json:"kind"`
		Effective []string           `json:"effective_permissions"`
		KGF       *authn.KGFIdentity `json:"kgf"`
		Perms     map[string]bool    `json:"permissions"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &who)
	if who.Namespace != ns || who.Name != "user:alice@acme.test" || who.Kind != "kgf" || who.KGF == nil || who.KGF.RequestID != "req-a1" {
		t.Fatalf("whoami %s", w.Body.String())
	}
	if w.Header().Get(kgfctx.RequestIDHeader) != "req-a1" {
		t.Fatal("X-Request-ID not echoed")
	}
	sc, err := store.GatewayKGFScope(context.Background(), e.pool, ns)
	if err != nil || sc.Organization != e.prefix+"acme" || sc.Subscription != "sub-1" || sc.ResourceGroup != "rg-data" {
		t.Fatalf("kgf_scopes %+v %v", sc, err)
	}

	// Operator: reads and Nexus-internal administration; never user admin.
	op := func(req string) string {
		return e.ctx("acme", "sub-1", "rg-data", "user:alice@acme.test", "operator", req)
	}
	e.do("GET", base+"/runs", "", op("r1"), "", 200)
	e.do("PUT", base+"/roles/Run%20monitor", `{"permissions":["runs.view"],"revision":0}`, op("req-role"), "", 200)
	e.do("GET", base+"/users", "", op("r3"), "", 403)
	e.do("POST", base+"/users", `{"email":"x@acme.test","password":"a-long-password-1234"}`, op("r4"), "", 403)
	sa := e.do("POST", base+"/service-accounts", `{"name":"ci-bot","roles":["Run monitor"]}`, op("req-sa"), "", 201)
	var saRow map[string]any
	_ = json.Unmarshal(sa.Body.Bytes(), &saRow)
	e.do("POST", base+"/tokens", `{"name":"legacy","role":"admin","scopes":["read","write","admin_write"]}`, op("r5"), "", 403)
	tok := e.do("POST", base+"/tokens", `{"name":"ci","service_account_id":"`+saRow["id"].(string)+`","permissions":["runs.view"]}`, op("r6"), "", 201)
	var tokRow map[string]any
	_ = json.Unmarshal(tok.Body.Bytes(), &tokRow)
	// The service-account token works on its own (machine credentials stay).
	e.do("GET", base+"/runs", "", "", tokRow["token"].(string), 200)
	e.do("GET", base+"/workers", "", "", tokRow["token"].(string), 403)

	// Viewer: reads only.
	vw := func(req string) string { return e.ctx("acme", "sub-1", "rg-data", "user:vic@acme.test", "viewer", req) }
	e.do("GET", base+"/runs", "", vw("v1"), "", 200)
	e.do("GET", base+"/audit", "", vw("v2"), "", 200)
	e.do("PUT", base+"/roles/Nope", `{"permissions":["runs.view"],"revision":0}`, vw("v3"), "", 403)
	e.do("POST", base+"/service-accounts", `{"name":"nope"}`, vw("v4"), "", 403)
	e.do("POST", base+"/runs/00000000-0000-0000-0000-000000000000/cancel", "", vw("v5"), "", 403)
	e.do("DELETE", base+"/specs/any", "", vw("v6"), "", 403)

	// Audit rows carry the KGF identity and request id, and a support
	// reference resolves through ?request_id=.
	var kind, label, detail string
	err = e.pool.QueryRow(context.Background(), `SELECT actor_kind, actor_label, detail::text FROM audit_log WHERE namespace=$1 AND request_id='req-sa'`, ns).Scan(&kind, &label, &detail)
	if err != nil || kind != "kgf" || label != "user:alice@acme.test" || !strings.Contains(detail, `"organization": "`+e.prefix+`acme"`) || !strings.Contains(detail, `"role": "operator"`) {
		t.Fatalf("audit attribution: %v %s %s %s", err, kind, label, detail)
	}
	// In KGF mode the chained KGF columns (migration 0033) come from the
	// context that AUTHENTICATED the request, verified once by kgfctx.
	var org, sub, rg, principal string
	var seq *int64
	err = e.pool.QueryRow(context.Background(), `SELECT kgf_organization, kgf_subscription, kgf_resource_group,
		kgf_principal, chain_seq FROM audit_log WHERE namespace=$1 AND request_id='req-sa'`, ns).Scan(&org, &sub, &rg, &principal, &seq)
	if err != nil || org != e.prefix+"acme" || sub != "sub-1" || rg != "rg-data" || principal != "user:alice@acme.test" || seq == nil {
		t.Fatalf("KGF columns from the authenticated context: %v %q %q %q %q %v", err, org, sub, rg, principal, seq)
	}
	list := e.do("GET", base+"/audit?request_id=req-role", "", op("r7"), "", 200)
	var entries struct {
		Entries []map[string]any `json:"entries"`
	}
	_ = json.Unmarshal(list.Body.Bytes(), &entries)
	if len(entries.Entries) != 1 || entries.Entries[0]["request_id"] != "req-role" || entries.Entries[0]["action"] != "role.save" {
		t.Fatalf("audit by request id: %s", list.Body.String())
	}
}

func TestIntegrationKGFCrossOrganizationIsolation(t *testing.T) {
	e := newKGFEnv(t, AuthKGF)
	nsA, nsB := e.ns("org-a", "shared", "rg"), e.ns("org-b", "shared", "rg")
	if nsA == nsB {
		t.Fatal("two organizations with equal subscription and group names share a namespace")
	}
	a := func(r string) string { return e.ctx("org-a", "shared", "rg", "user:a@a.test", "operator", r) }
	b := func(r string) string { return e.ctx("org-b", "shared", "rg", "user:b@b.test", "operator", r) }
	e.do("GET", "/v1/namespaces/"+nsA+"/runs", "", a("1"), "", 200)
	e.do("GET", "/v1/namespaces/"+nsB+"/runs", "", b("2"), "", 200)
	// Org A's principal cannot reach org B's namespace by naming it, nor its
	// own organization's other resource group.
	e.do("GET", "/v1/namespaces/"+nsB+"/runs", "", a("3"), "", 403)
	e.do("PUT", "/v1/namespaces/"+nsB+"/roles/X", `{"permissions":["runs.view"],"revision":0}`, a("4"), "", 403)
	e.do("GET", "/v1/namespaces/"+e.ns("org-a", "shared", "other-rg")+"/runs", "", a("5"), "", 403)
	var tA, tB string
	_ = e.pool.QueryRow(context.Background(), `SELECT tenant_id::text FROM kgf_scopes WHERE namespace=$1`, nsA).Scan(&tA)
	_ = e.pool.QueryRow(context.Background(), `SELECT tenant_id::text FROM kgf_scopes WHERE namespace=$1`, nsB).Scan(&tB)
	if tA == "" || tA == tB {
		t.Fatalf("tenants %q %q", tA, tB)
	}
}

func TestIntegrationKGFProvisioningIsIdempotentAndRefusesConflicts(t *testing.T) {
	pool := attentionPool(t)
	ctx := context.Background()
	scope := store.KGFScope{Organization: fmt.Sprintf("org-%d", time.Now().UnixNano()), Subscription: "S", ResourceGroup: "G"}
	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.GatewayProvisionKGFNamespace(ctx, pool, scope)
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != store.KGFNamespace(scope) {
			t.Fatalf("concurrent provisioning %d: %q %v", i, results[i], errs[i])
		}
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE namespace=$1`, results[0]).Scan(&n)
	if n != 1 {
		t.Fatalf("%d tenant rows", n)
	}
	// A pre-existing tenant with the derived name that KGF did not create is
	// refused, never adopted.
	squat := store.KGFScope{Organization: fmt.Sprintf("org-squat-%d", time.Now().UnixNano()), Subscription: "S", ResourceGroup: "G"}
	testdb.RegisterTenant(t, pool, store.KGFNamespace(squat))
	if _, err := store.GatewayProvisionKGFNamespace(ctx, pool, squat); err != store.ErrKGFNamespaceConflict {
		t.Fatalf("squatted namespace: %v", err)
	}
}

func TestIntegrationKGFModesAndCredentialConfusion(t *testing.T) {
	ctx := context.Background()
	pool := attentionPool(t)
	// A Nexus user with a session, and an admin API key, in some namespace.
	ns := fmt.Sprintf("kgf-modes-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	key := "nx_kgf_modes_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, pool, ns, "k", "admin", key); err != nil {
		t.Fatal(err)
	}
	u, _ := store.GatewayCreateUser(ctx, pool, ns, "p@x.test", "P", "admin", "a-long-password-1234")
	session, _, _ := store.GatewayCreateSession(ctx, pool, u.ID, time.Hour, "t", "")

	kgfMode := newKGFEnv(t, AuthKGF)
	both := newKGFEnv(t, AuthBoth)
	std := newKGFEnv(t, AuthStandalone)
	tok := func(e *kgfEnv) string { return e.ctx("o", "s", "g", "user:z", "operator", "") }

	// Both a context and a bearer: refused, in either mode that accepts contexts.
	for _, e := range []*kgfEnv{kgfMode, both} {
		w := e.do("GET", "/v1/auth/whoami", "", tok(e), key, 400)
		if !strings.Contains(w.Body.String(), "ambiguous_credentials") {
			t.Fatal(w.Body.String())
		}
	}
	// Standalone: a context is refused, not ignored.
	if w := std.do("GET", "/v1/auth/whoami", "", tok(std), "", 400); !strings.Contains(w.Body.String(), "kgf_context_not_accepted") {
		t.Fatal(w.Body.String())
	}
	std.do("GET", "/v1/auth/whoami", "", "", session, 200)
	// KGF mode: sessions refused, machine credentials accepted, no password
	// or registration routes.
	kgfMode.do("GET", "/v1/auth/whoami", "", "", session, 401)
	kgfMode.do("GET", "/v1/auth/whoami", "", "", key, 200)
	kgfMode.do("POST", "/v1/auth/login", `{"email":"p@x.test","password":"a-long-password-1234"}`, "", "", 404)
	kgfMode.do("POST", "/v1/auth/register", `{}`, "", "", 404)
	// Both mode: all of them.
	both.do("GET", "/v1/auth/whoami", "", "", session, 200)
	both.do("GET", "/v1/auth/whoami", "", tok(both), "", 200)
	// A context signed by a key this Nexus does not trust, or replayed.
	stranger := newKGFEnv(t, AuthKGF)
	kgfMode.do("GET", "/v1/auth/whoami", "", tok(stranger), "", 401)
	t1 := tok(kgfMode)
	kgfMode.do("GET", "/v1/auth/whoami", "", t1, "", 200)
	kgfMode.do("GET", "/v1/auth/whoami", "", t1, "", 401)
	// No resource group: no workload scope.
	noRG, _ := kgfMode.signer.MintFor("kgf-nexus", kgfctx.Context{Organization: "o", Subscription: "s", Principal: "user:z", Role: "operator"}, time.Minute, time.Now())
	kgfMode.do("GET", "/v1/auth/whoami", "", noRG, "", 403)
	// An unlisted KGF service acting for itself.
	svc, _ := kgfMode.signer.MintFor("kgf-nexus", kgfctx.Context{Organization: "o", Subscription: "s", ResourceGroup: "g", Principal: "svc:kgf-compute", Role: "service"}, time.Minute, time.Now())
	kgfMode.do("GET", "/v1/auth/whoami", "", svc, "", 403)
}

// TestIntegrationKGFKeyRotationThroughWellKnown follows the gateway's
// published key document through a rotation, end to end.
func TestIntegrationKGFKeyRotationThroughWellKnown(t *testing.T) {
	pool := attentionPool(t)
	_, k1, _ := ed25519.GenerateKey(rand.Reader)
	_, k2, _ := ed25519.GenerateKey(rand.Reader)
	var mu sync.Mutex
	published := []ed25519.PrivateKey{k1}
	wellKnown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		keys := []map[string]string{}
		for _, k := range published {
			pub := k.Public().(ed25519.PublicKey)
			keys = append(keys, map[string]string{"kid": kgfctx.KeyID(pub), "alg": "EdDSA", "key": kgfctx.EncodePublicKey(pub)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	defer wellKnown.Close()
	rk := kgfctx.NewRemoteKeys(wellKnown.URL+"/.well-known/kgf-context.json", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rk.MissInterval = 0
	kgfAuth := &authn.KGFAuthenticator{
		Verifier:    kgfctx.Verifier{Keys: rk, Audience: "kgf-nexus", UserIssuers: []string{"kgf-gateway"}, Seen: kgfctx.NewMemoryReplayGuard()},
		Provisioner: authn.StoreProvisioner{DB: pool},
	}
	h := New(pool, authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool}), nil, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithIdentity(NewStoreIdentityAdmin(pool)).WithKGF(AuthKGF, kgfAuth).Routes()
	org := fmt.Sprintf("rot-%d", time.Now().UnixNano())
	call := func(k ed25519.PrivateKey, want int) {
		t.Helper()
		tok, _ := kgfctx.NewSigner("kgf-gateway", k).MintFor("kgf-nexus",
			kgfctx.Context{Organization: org, Subscription: "s", ResourceGroup: "g", Principal: "user:r", Role: "viewer"}, time.Minute, time.Now())
		r := httptest.NewRequest("GET", "/v1/auth/whoami", nil)
		r.Header.Set(kgfctx.Header, tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("got %d want %d: %s", w.Code, want, w.Body.String())
		}
	}
	call(k1, 200)
	call(k2, 401) // not published yet
	mu.Lock()
	published = []ed25519.PrivateKey{k2}
	mu.Unlock()
	call(k2, 200) // unknown kid → refetch → trusted
	call(k1, 200) // retired, still inside its grace
}
