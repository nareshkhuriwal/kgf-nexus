// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package authn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/kgfctx"
)

// Every catalog permission is either mapped to a KGF.Nexus action or on this
// list of permissions a KGF principal must never hold. A new permission
// therefore fails this test until someone decides which it is.
var kgfUnmapped = map[string]bool{"users.view": true, "users.manage": true}

func TestKGFActionTableCoversTheCatalog(t *testing.T) {
	for _, perm := range Catalog() {
		action, mapped := KGFActions[perm]
		if mapped == kgfUnmapped[perm] {
			t.Errorf("%s: mapped=%v; every permission is mapped or explicitly unmapped", perm, mapped)
		}
		if mapped && !strings.HasPrefix(action, "KGF.Nexus/") {
			t.Errorf("%s maps outside KGF.Nexus: %s", perm, action)
		}
	}
	for perm := range KGFActions {
		if !Known(perm) {
			t.Errorf("KGFActions maps unknown permission %s", perm)
		}
	}
}

func TestKGFRoleMapping(t *testing.T) {
	op, viewer := KGFGrants(kgfctx.RoleOperator), KGFGrants(kgfctx.RoleViewer)
	for perm := range KGFActions {
		if !op[perm] {
			t.Errorf("operator (Contributor) lacks %s", perm)
		}
	}
	for _, want := range []string{"runs.view", "workflows.view", "tasks.logs.view", "audit.view", "workers.view", "schedules.view"} {
		if !viewer[want] {
			t.Errorf("viewer (Reader) lacks %s", want)
		}
	}
	for _, deny := range []string{"runs.create", "runs.cancel", "workflows.publish", "workflows.delete", "workers.drain",
		"schedules.create", "service_accounts.manage", "tokens.manage", "roles.manage", "quotas.manage", "infrastructure.configure"} {
		if viewer[deny] {
			t.Errorf("viewer holds %s", deny)
		}
	}
	for _, g := range []map[string]bool{op, viewer} {
		if g["users.view"] || g["users.manage"] {
			t.Fatal("a KGF role granted Nexus user administration; KGF owns people")
		}
	}
	if len(KGFGrants(kgfctx.RoleService)) != 0 || len(KGFGrants("admin")) != 0 {
		t.Fatal("an unknown or service role granted something")
	}
	// KGF's pattern matcher semantics (pkg/authz.matchPattern).
	for _, tc := range []struct {
		p, a string
		ok   bool
	}{
		{"*", "KGF.Nexus/pipelines/read", true},
		{"*/read", "KGF.Nexus/pipelines/runs/read", true},
		{"*/read", "KGF.Nexus/pipelines/runs/cancel/action", false},
		{"KGF.Nexus/*", "KGF.Nexus/workers/drain/action", true},
		{"KGF.Compute/*", "KGF.Nexus/pipelines/read", false},
		{"kgf.nexus/PIPELINES/read", "KGF.Nexus/pipelines/read", true},
	} {
		if kgfMatch(tc.p, tc.a) != tc.ok {
			t.Errorf("match(%q, %q) != %v", tc.p, tc.a, tc.ok)
		}
	}
	if KGFRoleAllows(kgfctx.RoleOperator, "KGF.Identity/roleAssignments/write") {
		t.Fatal("Contributor must not grant access-changing KGF.Identity actions")
	}
}

type fakeProvisioner struct{ calls atomic.Int32 }

func (f *fakeProvisioner) ProvisionKGFNamespace(_ context.Context, s store.KGFScope) (string, error) {
	f.calls.Add(1)
	return store.KGFNamespace(s), nil
}

func TestKGFAuthenticate(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	pub := key.Public().(ed25519.PublicKey)
	gw := kgfctx.NewSigner("kgf-gateway", key)
	prov := &fakeProvisioner{}
	now := time.Now()
	k := &KGFAuthenticator{
		Verifier: kgfctx.Verifier{Keys: kgfctx.StaticKeys{kgfctx.KeyID(pub): pub}, Audience: "kgf-nexus",
			UserIssuers: []string{"kgf-gateway"}, Seen: kgfctx.NewMemoryReplayGuard()},
		Provisioner:       prov,
		ServicePrincipals: map[string]string{"svc:kgf-billing": kgfctx.RoleViewer},
		Now:               func() time.Time { return now },
	}
	mint := func(c kgfctx.Context) string {
		tok, err := gw.MintFor("kgf-nexus", c, time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	alice := kgfctx.Context{Organization: "org-a", Subscription: "Sub-1", ResourceGroup: "RG-1", Principal: "user:alice", Role: kgfctx.RoleViewer, RequestID: "req-1"}
	p, err := k.Authenticate(context.Background(), mint(alice))
	if err != nil {
		t.Fatal(err)
	}
	if p.Kind != KindKGF || p.Namespace != store.KGFNamespace(store.KGFScope{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1"}) ||
		p.Name != "user:alice" || p.Role == RoleAdmin || p.KGF.RequestID != "req-1" || !p.Allows("runs.view") || p.Allows("runs.cancel") {
		t.Fatalf("principal %+v", p)
	}
	if p.Authorize(p.Namespace, ActionWorkerRPC) == nil {
		t.Fatal("a KGF principal may never speak the worker protocol")
	}
	// Provisioning is cached after the first request for a scope.
	_, _ = k.Authenticate(context.Background(), mint(alice))
	if prov.calls.Load() != 1 {
		t.Fatalf("provisioned %d times", prov.calls.Load())
	}

	// Cross-organization: identical subscription and group names in another
	// organization are another namespace.
	other := alice
	other.Organization = "org-b"
	pb, err := k.Authenticate(context.Background(), mint(other))
	if err != nil || pb.Namespace == p.Namespace {
		t.Fatalf("org-b mapped onto org-a's namespace: %v", err)
	}
	if p.Authorize(pb.Namespace, ActionRead) == nil {
		t.Fatal("org-a principal authorized for org-b's namespace")
	}

	// A delegated context is decided against the person.
	del := kgfctx.Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "svc:kgf-gateway",
		Role: kgfctx.RoleService, OnBehalfOf: "user:bob", OnBehalfOfRole: kgfctx.RoleOperator}
	pd, err := k.Authenticate(context.Background(), mint(del))
	if err != nil || pd.Name != "user:bob" || pd.KGF.Via != "svc:kgf-gateway" || !pd.Allows("runs.cancel") {
		t.Fatalf("delegated %+v %v", pd, err)
	}

	// Pure service contexts: only listed services, at their configured role.
	svc := kgfctx.Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "svc:kgf-billing", Role: kgfctx.RoleService}
	ps, err := k.Authenticate(context.Background(), mint(svc))
	if err != nil || !ps.Allows("usage.view") || ps.Allows("runs.create") {
		t.Fatalf("listed service %+v %v", ps, err)
	}
	svc.Principal = "svc:kgf-compute"
	if _, err := k.Authenticate(context.Background(), mint(svc)); !errors.Is(err, ErrKGFServiceNotAllowed) {
		t.Fatalf("unlisted service: %v", err)
	}

	// No workload scope.
	noScope := alice
	noScope.ResourceGroup = ""
	if _, err := k.Authenticate(context.Background(), mint(noScope)); !errors.Is(err, ErrKGFScopeRequired) {
		t.Fatalf("missing resource group: %v", err)
	}

	// Tampered, expired, wrong key and replayed contexts are one 401.
	tok := mint(alice)
	parts := strings.Split(tok, ".")
	_, stranger, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey, _ := kgfctx.Signer{KeyID: gw.KeyID, Key: stranger, Issuer: "kgf-gateway"}.MintFor("kgf-nexus", alice, time.Minute, now)
	expired, _ := gw.MintFor("kgf-nexus", alice, time.Minute, now.Add(-2*time.Minute))
	for name, bad := range map[string]string{
		"tampered":  parts[0] + "." + parts[1][:len(parts[1])-2] + "AA." + parts[2],
		"wrong key": wrongKey,
		"expired":   expired,
		"garbage":   "not-a-token",
	} {
		if _, err := k.Authenticate(context.Background(), bad); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := k.Authenticate(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Authenticate(context.Background(), tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replayed context: %v", err)
	}
}

func TestParseServicePrincipals(t *testing.T) {
	m, err := ParseServicePrincipals(" svc:kgf-billing=viewer , svc:kgf-orchestrator=operator ")
	if err != nil || m["svc:kgf-billing"] != "viewer" || m["svc:kgf-orchestrator"] != "operator" {
		t.Fatalf("%v %v", m, err)
	}
	for _, bad := range []string{"kgf-billing=viewer", "svc:x=service", "svc:x", "user:alice=operator"} {
		if _, err := ParseServicePrincipals(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
