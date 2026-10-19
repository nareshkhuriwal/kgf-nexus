// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// fakeWorkspaces is an in-memory WorkspaceStore: active workspaces by
// (organization, normalised resource id).
type fakeWorkspaces struct {
	mu     sync.Mutex
	active map[string]bool
}

func (f *fakeWorkspaces) set(org, res string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.active == nil {
		f.active = map[string]bool{}
	}
	f.active[org+"\x00"+res] = on
}

func (f *fakeWorkspaces) ActiveKGFWorkspace(_ context.Context, w store.KGFWorkspace) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.active[w.Organization+"\x00"+w.ResourceID] {
		return "", store.ErrKGFWorkspaceNotProvisioned
	}
	return store.KGFWorkspaceNamespace(w.Organization, w.ResourceID), nil
}

func TestKGFWorkspaceAuthenticate(t *testing.T) {
	gwKey, frontKey := newTestKey(t), newTestKey(t)
	keys := kgfctx.StaticKeys{}
	for _, k := range []ed25519.PrivateKey{gwKey, frontKey} {
		pub := k.Public().(ed25519.PublicKey)
		keys[kgfctx.KeyID(pub)] = pub
	}
	gw, front := kgfctx.NewSigner("kgf-gateway", gwKey), kgfctx.NewSigner("kgf-nexus", frontKey)
	now := time.Now()
	ws := &fakeWorkspaces{}
	prov := &fakeProvisioner{}
	k := &KGFAuthenticator{
		Verifier: kgfctx.Verifier{Keys: keys, Audience: "kgf-nexus-engine", UserIssuers: []string{"kgf-gateway"},
			FrontIssuers: []string{"kgf-nexus"}, Seen: kgfctx.NewMemoryReplayGuard()},
		Provisioner: prov,
		Workspaces:  ws,
		Now:         func() time.Time { return now },
	}
	mint := func(s kgfctx.Signer, c kgfctx.Context) string {
		t.Helper()
		tok, err := s.MintFor("kgf-nexus-engine", c, time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	const res = "/subscriptions/Sub-1/resourceGroups/RG-1/providers/KGF.Nexus/workspaces/etl-prod"
	const norm = "/subscriptions/sub-1/resourcegroups/rg-1/providers/kgf.nexus/workspaces/etl-prod"
	wsNS := store.KGFWorkspaceNamespace("org-a", norm)
	delegated := func(role string) kgfctx.Context {
		return kgfctx.Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "svc:kgf-nexus",
			Role: kgfctx.RoleService, OnBehalfOf: "user:bob@a.test", OnBehalfOfRole: role, RequestID: "req-ws", Resource: res}
	}
	provisioner := kgfctx.Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1",
		Principal: "svc:kgf-nexus", Role: kgfctx.RoleService, RequestID: "req-prov", Resource: res}

	// Not provisioned: 404-class error, and NOTHING is provisioned lazily.
	if _, err := k.Authenticate(context.Background(), mint(front, delegated(kgfctx.RoleOperator))); !errors.Is(err, ErrWorkspaceNotProvisioned) {
		t.Fatalf("unprovisioned workspace: %v", err)
	}
	if prov.calls.Load() != 0 {
		t.Fatal("a workspace context provisioned a namespace on first use")
	}

	// The provisioner authenticates before the workspace exists, with no
	// Nexus permission at all.
	pp, err := k.Authenticate(context.Background(), mint(front, provisioner))
	if err != nil {
		t.Fatal(err)
	}
	if pp.KGF == nil || !pp.KGF.Provisioner || pp.Namespace != wsNS || pp.KGF.ResourceID != norm || pp.KGF.Workspace != "etl-prod" ||
		pp.Grants == nil || len(pp.Grants) != 0 || pp.Role == RoleAdmin || pp.Name != "svc:kgf-nexus" {
		t.Fatalf("provisioner principal %+v %+v", pp, pp.KGF)
	}

	ws.set("org-a", norm, true)

	// a. Delegated operator: the person, the operator grants, the v2
	// namespace, and both identities for audit.
	po, err := k.Authenticate(context.Background(), mint(front, delegated(kgfctx.RoleOperator)))
	if err != nil {
		t.Fatal(err)
	}
	if po.Namespace != wsNS || po.Name != "user:bob@a.test" || po.KGF.Via != "svc:kgf-nexus" || po.KGF.Principal != "user:bob@a.test" ||
		po.KGF.Role != kgfctx.RoleOperator || po.KGF.Provisioner || po.KGF.RequestID != "req-ws" || po.KGF.ResourceID != norm {
		t.Fatalf("delegated operator %+v %+v", po, po.KGF)
	}
	want := KGFGrants(kgfctx.RoleOperator)
	if len(po.Grants) != len(want) || !po.Allows("runs.cancel") || po.Allows("users.manage") {
		t.Fatalf("delegated operator grants differ from a gateway operator's: %v", po.Grants)
	}
	d := po.KGF.AuditDetail()
	if d["via"] != "svc:kgf-nexus" || d["principal"] != "user:bob@a.test" || d["resource_id"] != norm {
		t.Fatalf("audit detail %v", d)
	}
	// Delegated viewer: reads only.
	pv, err := k.Authenticate(context.Background(), mint(front, delegated(kgfctx.RoleViewer)))
	if err != nil || !pv.Allows("runs.view") || pv.Allows("runs.create") || pv.Namespace != wsNS {
		t.Fatalf("delegated viewer %+v %v", pv, err)
	}
	// The workspace principal is bound to its namespace only: not the
	// resource group's v1 namespace, not a sibling workspace.
	v1 := store.KGFNamespace(store.KGFScope{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1"})
	sibling := store.KGFWorkspaceNamespace("org-a", "/subscriptions/sub-1/resourcegroups/rg-1/providers/kgf.nexus/workspaces/etl-dev")
	for _, other := range []string{v1, sibling} {
		if po.Authorize(other, ActionRead) == nil {
			t.Fatalf("workspace principal authorized for %s", other)
		}
	}

	// Deleted (inactive again): 404-class, immediately.
	ws.set("org-a", norm, false)
	if _, err := k.Authenticate(context.Background(), mint(front, delegated(kgfctx.RoleOperator))); !errors.Is(err, ErrWorkspaceNotProvisioned) {
		t.Fatalf("deleted workspace: %v", err)
	}

	// Refusals with their own codes.
	gwRes := kgfctx.Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "user:eve@a.test",
		Role: kgfctx.RoleOperator, Resource: res}
	noRes := delegated(kgfctx.RoleOperator)
	noRes.Resource = ""
	mismatch := delegated(kgfctx.RoleOperator)
	mismatch.ResourceGroup = "rg-2"
	for name, tc := range map[string]struct {
		tok  string
		want error
	}{
		"gateway context with res":    {mint(gw, gwRes), ErrKGFResourceNotAllowed},
		"front context without res":   {mint(front, noRes), ErrKGFResourceRequired},
		"res outside the token scope": {mint(front, mismatch), ErrKGFResourceInvalid},
		"front as a human": {mint(front, kgfctx.Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1",
			Principal: "user:eve@a.test", Role: kgfctx.RoleOperator, Resource: res}), ErrKGFFrontShape},
	} {
		if _, err := k.Authenticate(context.Background(), tc.tok); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}

	// The v1 path is unchanged beside it: a gateway context without res is
	// lazily provisioned per resource group, as before.
	pg, err := k.Authenticate(context.Background(), mint(gw, kgfctx.Context{Organization: "org-a", Subscription: "sub-1",
		ResourceGroup: "rg-1", Principal: "user:alice@a.test", Role: kgfctx.RoleViewer}))
	if err != nil || pg.Namespace != v1 || prov.calls.Load() != 1 || pg.KGF.ResourceID != "" {
		t.Fatalf("v1 context %+v %v (provisioned %d)", pg, err, prov.calls.Load())
	}
}

// With no workspace store at all, a delegated workspace context fails closed.
func TestKGFWorkspaceWithoutStoreFailsClosed(t *testing.T) {
	key := newTestKey(t)
	pub := key.Public().(ed25519.PublicKey)
	front := kgfctx.NewSigner("kgf-nexus", key)
	now := time.Now()
	k := &KGFAuthenticator{
		Verifier: kgfctx.Verifier{Keys: kgfctx.StaticKeys{kgfctx.KeyID(pub): pub}, Audience: "kgf-nexus",
			FrontIssuers: []string{"kgf-nexus"}, UserIssuers: []string{"kgf-gateway"}},
		Provisioner: &fakeProvisioner{},
		Now:         func() time.Time { return now },
	}
	tok, _ := front.MintFor("kgf-nexus", kgfctx.Context{Organization: "o", Subscription: "s", ResourceGroup: "r",
		Principal: "svc:kgf-nexus", Role: kgfctx.RoleService, OnBehalfOf: "user:a", OnBehalfOfRole: kgfctx.RoleOperator,
		Resource: "/subscriptions/s/resourceGroups/r/providers/KGF.Nexus/workspaces/abc"}, time.Minute, now)
	if _, err := k.Authenticate(context.Background(), tok); !errors.Is(err, ErrWorkspaceNotProvisioned) {
		t.Fatalf("no workspace store: %v", err)
	}
}

func newTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
