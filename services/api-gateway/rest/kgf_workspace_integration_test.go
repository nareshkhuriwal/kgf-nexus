// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

// KGF.Nexus workspaces end to end through the real handlers and a real
// PostgreSQL (ADR-0034, migration 0040): the front's provisioner creates and
// deletes a workspace, delegated people act in it, and nothing is ever
// provisioned lazily.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

type wsEnv struct {
	*kgfEnv
	front kgfctx.Signer
	org   string
}

func newWSEnv(t *testing.T) *wsEnv {
	t.Helper()
	pool := attentionPool(t)
	_, gwKey, _ := ed25519.GenerateKey(rand.Reader)
	_, frontKey, _ := ed25519.GenerateKey(rand.Reader)
	keys := kgfctx.StaticKeys{}
	for _, k := range []ed25519.PrivateKey{gwKey, frontKey} {
		pub := k.Public().(ed25519.PublicKey)
		keys[kgfctx.KeyID(pub)] = pub
	}
	kgfAuth := &authn.KGFAuthenticator{
		Verifier: kgfctx.Verifier{Keys: keys, Audience: "kgf-nexus", UserIssuers: []string{"kgf-gateway"},
			FrontIssuers: []string{"kgf-nexus"}, Seen: kgfctx.NewMemoryReplayGuard()},
		Provisioner: authn.StoreProvisioner{DB: pool},
	}
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	h := New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(pool)).WithKGF(AuthKGF, kgfAuth).Routes()
	prefix := fmt.Sprintf("t%d", time.Now().UnixNano())
	return &wsEnv{
		kgfEnv: &kgfEnv{t: t, pool: pool, signer: kgfctx.NewSigner("kgf-gateway", gwKey), h: h, prefix: prefix},
		front:  kgfctx.NewSigner("kgf-nexus", frontKey),
		org:    prefix + "acme",
	}
}

func wsResource(ws string) string {
	return "/subscriptions/Sub-1/resourceGroups/RG-Data/providers/KGF.Nexus/workspaces/" + ws
}

func (e *wsEnv) frontCtx(ws, obo, oboRole, requestID string) string {
	e.t.Helper()
	c := kgfctx.Context{Organization: e.org, Subscription: "sub-1", ResourceGroup: "rg-data", Principal: "svc:kgf-nexus",
		Role: kgfctx.RoleService, OnBehalfOf: obo, OnBehalfOfRole: oboRole, RequestID: requestID, Resource: wsResource(ws)}
	tok, err := e.front.MintFor("kgf-nexus", c, time.Minute, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *wsEnv) provisioner(ws, requestID string) string { return e.frontCtx(ws, "", "", requestID) }

func (e *wsEnv) wsNS(ws string) string {
	return store.KGFWorkspaceNamespace(e.org, strings.ToLower(wsResource(ws)))
}

func errorCode(w *httptest.ResponseRecorder) string {
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return body.Error
}

func TestIntegrationKGFWorkspaceLifecycle(t *testing.T) {
	e := newWSEnv(t)
	ctx := context.Background()
	ns := e.wsNS("etl-prod")
	base := "/v1/namespaces/" + ns
	bob := func(role, req string) string { return e.frontCtx("etl-prod", "user:bob@acme.test", role, req) }

	// Before provisioning: a delegated person gets 404 and nothing is created.
	if w := e.do("GET", base+"/runs", "", bob("operator", "r0"), "", 404); errorCode(w) != "workspace_not_provisioned" {
		t.Fatal(w.Body.String())
	}
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE namespace=$1`, ns).Scan(&n)
	if n != 0 {
		t.Fatal("a delegated workspace context provisioned a tenant")
	}

	// PUT: 201, then 200 (idempotent), same body.
	w := e.do("PUT", "/v1/kgf/workspace", "", e.provisioner("etl-prod", "req-put-1"), "", 201)
	var got struct {
		Namespace  string `json:"namespace"`
		ResourceID string `json:"resource_id"`
		State      string `json:"state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	wantRID := strings.ToLower(wsResource("etl-prod"))
	if got.Namespace != ns || got.ResourceID != wantRID || got.State != "active" {
		t.Fatalf("PUT body %s", w.Body.String())
	}
	w = e.do("PUT", "/v1/kgf/workspace", "", e.provisioner("etl-prod", "req-put-2"), "", 200)
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Namespace != ns || got.State != "active" {
		t.Fatalf("second PUT %s", w.Body.String())
	}
	rec, err := store.GatewayKGFWorkspace(ctx, e.pool, e.org, wantRID)
	if err != nil || rec.Namespace != ns || rec.Workspace != "etl-prod" || rec.Subscription != "sub-1" ||
		rec.ResourceGroup != "rg-data" || rec.State != "active" {
		t.Fatalf("kgf_workspaces row %+v %v", rec, err)
	}
	// Audit: the provisioning is recorded in the workspace's chain, by the front.
	var kind, label, detail string
	if err := e.pool.QueryRow(ctx, `SELECT actor_kind, actor_label, detail::text FROM audit_log
		WHERE namespace=$1 AND action='kgf.workspace.provision' AND request_id='req-put-1'`, ns).Scan(&kind, &label, &detail); err != nil ||
		kind != "kgf" || label != "svc:kgf-nexus" || !strings.Contains(detail, `"created": true`) || !strings.Contains(detail, wantRID) {
		t.Fatalf("provision audit: %v %s %s %s", err, kind, label, detail)
	}

	// The provisioner may do nothing else.
	if w := e.do("GET", base+"/runs", "", e.provisioner("etl-prod", "p1"), "", 403); errorCode(w) != "kgf_provisioner_restricted" {
		t.Fatal(w.Body.String())
	}
	if w := e.do("GET", "/v1/auth/whoami", "", e.provisioner("etl-prod", "p2"), "", 403); errorCode(w) != "kgf_provisioner_restricted" {
		t.Fatal(w.Body.String())
	}

	// Delegated people: whoami names the workspace; roles map as for a
	// gateway context; the audit row names the person AND the front.
	w = e.do("GET", "/v1/auth/whoami", "", bob("operator", "r1"), "", 200)
	var who struct {
		Namespace string             `json:"namespace"`
		Name      string             `json:"name"`
		KGF       *authn.KGFIdentity `json:"kgf"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &who)
	if who.Namespace != ns || who.Name != "user:bob@acme.test" || who.KGF == nil || who.KGF.ResourceID != wantRID ||
		who.KGF.Workspace != "etl-prod" || who.KGF.Via != "svc:kgf-nexus" {
		t.Fatalf("whoami %s", w.Body.String())
	}
	e.do("GET", base+"/runs", "", bob("operator", "r2"), "", 200)
	e.do("PUT", base+"/roles/Run%20monitor", `{"permissions":["runs.view"],"revision":0}`, bob("operator", "req-role"), "", 200)
	e.do("GET", base+"/users", "", bob("operator", "r3"), "", 403)
	e.do("GET", base+"/runs", "", bob("viewer", "r4"), "", 200)
	e.do("PUT", base+"/roles/Nope", `{"permissions":["runs.view"],"revision":0}`, bob("viewer", "r5"), "", 403)
	var onBehalf string
	if err := e.pool.QueryRow(ctx, `SELECT actor_label, kgf_principal, kgf_on_behalf_of, detail::text FROM audit_log
		WHERE namespace=$1 AND request_id='req-role'`, ns).Scan(&label, &kind, &onBehalf, &detail); err != nil ||
		label != "user:bob@acme.test" || kind != "svc:kgf-nexus" || onBehalf != "user:bob@acme.test" ||
		!strings.Contains(detail, `"via": "svc:kgf-nexus"`) || !strings.Contains(detail, wantRID) {
		t.Fatalf("delegated audit: %v label=%s kgf_principal=%s obo=%s %s", err, label, kind, onBehalf, detail)
	}

	// A machine credential minted inside the workspace, to prove DELETE
	// revokes it.
	sa := e.do("POST", base+"/service-accounts", `{"name":"ci-bot","roles":["Run monitor"]}`, bob("operator", "r6"), "", 201)
	var saRow map[string]any
	_ = json.Unmarshal(sa.Body.Bytes(), &saRow)
	tok := e.do("POST", base+"/tokens", `{"name":"ci","service_account_id":"`+saRow["id"].(string)+`","permissions":["runs.view"]}`, bob("operator", "r7"), "", 201)
	var tokRow map[string]any
	_ = json.Unmarshal(tok.Body.Bytes(), &tokRow)
	machine := tokRow["token"].(string)
	e.do("GET", base+"/runs", "", "", machine, 200)

	// Two workspaces in one resource group are two namespaces, and the
	// resource group's v1 namespace is a third; none reaches another.
	e.do("PUT", "/v1/kgf/workspace", "", e.provisioner("etl-dev", "req-put-dev"), "", 201)
	devNS := e.wsNS("etl-dev")
	v1NS := store.KGFNamespace(store.KGFScope{Organization: e.org, Subscription: "sub-1", ResourceGroup: "rg-data"})
	if devNS == ns || v1NS == ns || v1NS == devNS {
		t.Fatal("namespaces collide")
	}
	e.do("GET", "/v1/namespaces/"+devNS+"/runs", "", bob("operator", "x1"), "", 403)
	e.do("GET", "/v1/namespaces/"+v1NS+"/runs", "", bob("operator", "x2"), "", 403)
	// The v1 path still works beside the workspaces (lazy, per resource group).
	gw := func(req string) string {
		tok, err := e.signer.MintFor("kgf-nexus", kgfctx.Context{Organization: e.org, Subscription: "sub-1", ResourceGroup: "rg-data",
			Principal: "user:alice@acme.test", Role: kgfctx.RoleOperator, RequestID: req}, time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	e.do("GET", "/v1/namespaces/"+v1NS+"/runs", "", gw("g1"), "", 200)
	e.do("GET", base+"/runs", "", gw("g2"), "", 403)

	// DELETE: 200 deleted; the workspace is gone for delegated people at once,
	// its machine credentials are revoked, and nothing is hard-deleted.
	w = e.do("DELETE", "/v1/kgf/workspace", "", e.provisioner("etl-prod", "req-del-1"), "", 200)
	if strings.TrimSpace(w.Body.String()) != `{"state":"deleted"}` {
		t.Fatalf("DELETE body %s", w.Body.String())
	}
	if w := e.do("GET", base+"/runs", "", bob("operator", "r8"), "", 404); errorCode(w) != "workspace_not_provisioned" {
		t.Fatal(w.Body.String())
	}
	e.do("GET", base+"/runs", "", "", machine, 401)
	var disabled bool
	_ = e.pool.QueryRow(ctx, `SELECT disabled_at IS NOT NULL FROM service_accounts WHERE namespace=$1 AND name='ci-bot'`, ns).Scan(&disabled)
	if !disabled {
		t.Fatal("service account still enabled after workspace deletion")
	}
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE namespace=$1`, ns).Scan(&n)
	if n != 1 {
		t.Fatal("the tenant was hard-deleted")
	}
	if err := e.pool.QueryRow(ctx, `SELECT detail::text FROM audit_log WHERE namespace=$1 AND action='kgf.workspace.delete'
		AND request_id='req-del-1'`, ns).Scan(&detail); err != nil || !strings.Contains(detail, `"revoked_tokens": 1`) ||
		!strings.Contains(detail, `"disabled_service_accounts": 1`) {
		t.Fatalf("delete audit: %v %s", err, detail)
	}
	// The sibling workspace is untouched.
	e.do("GET", "/v1/namespaces/"+devNS+"/runs", "", e.frontCtx("etl-dev", "user:bob@acme.test", "viewer", "x3"), "", 200)

	// DELETE again: still 200. PUT after delete: 409 workspace_deleted.
	e.do("DELETE", "/v1/kgf/workspace", "", e.provisioner("etl-prod", "req-del-2"), "", 200)
	if w := e.do("PUT", "/v1/kgf/workspace", "", e.provisioner("etl-prod", "req-put-3"), "", 409); errorCode(w) != "workspace_deleted" {
		t.Fatal(w.Body.String())
	}
	if err := e.pool.QueryRow(ctx, `SELECT outcome FROM audit_log WHERE namespace=$1 AND request_id='req-put-3'`, ns).Scan(&kind); err != nil || kind != "denied" {
		t.Fatalf("denied re-create audit: %v %s", err, kind)
	}
	// DELETE of a workspace never provisioned: 200, nothing created.
	e.do("DELETE", "/v1/kgf/workspace", "", e.provisioner("never-made", "req-del-3"), "", 200)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM tenants WHERE namespace=$1`, e.wsNS("never-made")).Scan(&n)
	if n != 0 {
		t.Fatal("DELETE of an unknown workspace created a tenant")
	}
}

func TestIntegrationKGFWorkspaceProvisioningConcurrencyAndConflicts(t *testing.T) {
	pool := attentionPool(t)
	ctx := context.Background()
	org := fmt.Sprintf("org-%d", time.Now().UnixNano())
	rid := "/subscriptions/s/resourcegroups/g/providers/kgf.nexus/workspaces/abc"
	ws := store.KGFWorkspace{Organization: org, Subscription: "S", ResourceGroup: "G", Workspace: "abc", ResourceID: rid}
	var wg sync.WaitGroup
	created := make([]bool, 8)
	errs := make([]error, 8)
	for i := range created {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, created[i], errs[i] = store.GatewayProvisionKGFWorkspace(ctx, pool, ws, "svc:test")
		}(i)
	}
	wg.Wait()
	creators := 0
	for i := range created {
		if errs[i] != nil {
			t.Fatalf("concurrent provisioning %d: %v", i, errs[i])
		}
		if created[i] {
			creators++
		}
	}
	if creators != 1 {
		t.Fatalf("%d callers report creating the workspace", creators)
	}
	// A pre-existing tenant with a workspace's derived name is refused.
	squatOrg := fmt.Sprintf("org-squat-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, store.KGFWorkspaceNamespace(squatOrg, rid))
	sq := ws
	sq.Organization = squatOrg
	if _, _, err := store.GatewayProvisionKGFWorkspace(ctx, pool, sq, "svc:test"); err != store.ErrKGFNamespaceConflict {
		t.Fatalf("squatted workspace namespace: %v", err)
	}
	// The v1 path refuses a workspace namespace too (it has no kgf_scopes row).
	if _, err := store.GatewayActiveKGFWorkspace(ctx, pool, store.KGFWorkspace{Organization: squatOrg, Subscription: "s",
		ResourceGroup: "g", Workspace: "abc", ResourceID: rid}); err != store.ErrKGFWorkspaceNotProvisioned {
		t.Fatalf("squatted namespace resolved as a workspace: %v", err)
	}
	// Deleting twice is idempotent.
	d1, err := store.GatewayDeleteKGFWorkspace(ctx, pool, org, rid, "svc:test")
	if err != nil || !d1.Existed || d1.AlreadyDeleted {
		t.Fatalf("first delete %+v %v", d1, err)
	}
	d2, err := store.GatewayDeleteKGFWorkspace(ctx, pool, org, rid, "svc:test")
	if err != nil || !d2.Existed || !d2.AlreadyDeleted {
		t.Fatalf("second delete %+v %v", d2, err)
	}
	if _, _, err := store.GatewayProvisionKGFWorkspace(ctx, pool, ws, "svc:test"); err != store.ErrKGFWorkspaceDeleted {
		t.Fatalf("re-create after delete: %v", err)
	}
}
