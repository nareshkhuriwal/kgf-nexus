// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

// KGF.Nexus workspace contexts through the real guard, without a database
// (ADR-0034): the refusals, the provisioner's route restriction and the
// unprovisioned 404 are all decided before any handler runs. The provisioning
// round trip itself is kgf_workspace_integration_test.go.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/kgfctx"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

type unitProvisioner struct{}

func (unitProvisioner) ProvisionKGFNamespace(_ context.Context, s store.KGFScope) (string, error) {
	return store.KGFNamespace(s), nil
}

type unitWorkspaces map[string]bool

func (u unitWorkspaces) ActiveKGFWorkspace(_ context.Context, w store.KGFWorkspace) (string, error) {
	if !u[w.ResourceID] {
		return "", store.ErrKGFWorkspaceNotProvisioned
	}
	return store.KGFWorkspaceNamespace(w.Organization, w.ResourceID), nil
}

const unitWS = "/subscriptions/sub-1/resourceGroups/rg-1/providers/KGF.Nexus/workspaces/etl-prod"

func TestKGFWorkspaceGuard(t *testing.T) {
	var gwKey, frontKey ed25519.PrivateKey
	_, gwKey, _ = ed25519.GenerateKey(rand.Reader)
	_, frontKey, _ = ed25519.GenerateKey(rand.Reader)
	keys := kgfctx.StaticKeys{}
	for _, k := range []ed25519.PrivateKey{gwKey, frontKey} {
		pub := k.Public().(ed25519.PublicKey)
		keys[kgfctx.KeyID(pub)] = pub
	}
	gw, front := kgfctx.NewSigner("kgf-gateway", gwKey), kgfctx.NewSigner("kgf-nexus", frontKey)
	active := "/subscriptions/sub-1/resourcegroups/rg-1/providers/kgf.nexus/workspaces/etl-live"
	kgfAuth := &authn.KGFAuthenticator{
		Verifier: kgfctx.Verifier{Keys: keys, Audience: "kgf-nexus-engine", UserIssuers: []string{"kgf-gateway"},
			FrontIssuers: []string{"kgf-nexus"}, Seen: kgfctx.NewMemoryReplayGuard()},
		Provisioner: unitProvisioner{},
		Workspaces:  unitWorkspaces{active: true},
	}
	db := newRestDB()
	db.addKey(adminKey, "team-ml", "ci", "admin")
	h := New(db, authn.New(db), &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithKGF(AuthKGF, kgfAuth).Routes()

	mint := func(s kgfctx.Signer, c kgfctx.Context) string {
		t.Helper()
		tok, err := s.MintFor("kgf-nexus-engine", c, time.Minute, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	do := func(method, path, ctxTok, bearer string) (int, string) {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if ctxTok != "" {
			r.Header.Set(kgfctx.Header, ctxTok)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var body struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body.Error
	}
	scope := func(c kgfctx.Context) kgfctx.Context {
		c.Organization, c.Subscription, c.ResourceGroup = "org-a", "sub-1", "rg-1"
		return c
	}
	provisioner := scope(kgfctx.Context{Principal: "svc:kgf-nexus", Role: kgfctx.RoleService, Resource: unitWS})
	delegated := func(res string) kgfctx.Context {
		return scope(kgfctx.Context{Principal: "svc:kgf-nexus", Role: kgfctx.RoleService,
			OnBehalfOf: "user:bob@a.test", OnBehalfOfRole: kgfctx.RoleOperator, Resource: res})
	}
	unprovNS := store.KGFWorkspaceNamespace("org-a", "/subscriptions/sub-1/resourcegroups/rg-1/providers/kgf.nexus/workspaces/etl-prod")
	activeNS := store.KGFWorkspaceNamespace("org-a", active)

	for name, tc := range map[string]struct {
		method, path, ctx, bearer string
		status                    int
		code                      string
	}{
		// The provisioner reaches the two provisioning routes and nothing else.
		"provisioner lists runs": {"GET", "/v1/namespaces/" + unprovNS + "/runs", mint(front, provisioner), "", 403, "kgf_provisioner_restricted"},
		"provisioner schedules":  {"GET", "/v1/namespaces/" + unprovNS + "/schedules", mint(front, provisioner), "", 403, "kgf_provisioner_restricted"},
		"provisioner workers":    {"GET", "/v1/namespaces/" + unprovNS + "/workers", mint(front, provisioner), "", 403, "kgf_provisioner_restricted"},
		// Delegated contexts for a workspace that is not provisioned: 404.
		"delegated unprovisioned":       {"GET", "/v1/namespaces/" + unprovNS + "/runs", mint(front, delegated(unitWS)), "", 404, "workspace_not_provisioned"},
		"delegated unprovisioned write": {"POST", "/v1/namespaces/" + unprovNS + "/runs", mint(front, delegated(unitWS)), "", 404, "workspace_not_provisioned"},
		// Delegated contexts reach only their own namespace.
		"delegated other namespace": {"GET", "/v1/namespaces/" + unprovNS + "/runs", mint(front, delegated(active)), "", 403, "forbidden"},
		// Only fronts assert res; fronts always do.
		"gateway with res": {"GET", "/v1/namespaces/" + activeNS + "/runs",
			mint(gw, scope(kgfctx.Context{Principal: "user:eve@a.test", Role: kgfctx.RoleOperator, Resource: active})), "", 403, "kgf_resource_not_allowed"},
		"front without res": {"GET", "/v1/namespaces/" + activeNS + "/runs", mint(front, delegated("")), "", 403, "kgf_resource_required"},
		"res outside scope": {"GET", "/v1/namespaces/" + activeNS + "/runs",
			mint(front, func() kgfctx.Context { c := delegated(active); c.ResourceGroup = "rg-2"; return c }()), "", 403, "kgf_resource_invalid"},
		"front as a human": {"GET", "/v1/namespaces/" + activeNS + "/runs",
			mint(front, scope(kgfctx.Context{Principal: "user:eve@a.test", Role: kgfctx.RoleOperator, Resource: active})), "", 403, "kgf_front_context_invalid"},
		// The provisioning routes serve only the provisioner: a delegated
		// person and a Nexus admin key are refused.
		"delegated provisions": {"PUT", "/v1/kgf/workspace", mint(front, delegated(active)), "", 403, "kgf_provisioner_required"},
		"delegated deletes":    {"DELETE", "/v1/kgf/workspace", mint(front, delegated(active)), "", 403, "kgf_provisioner_required"},
		"admin key provisions": {"PUT", "/v1/kgf/workspace", "", adminKey, 403, "kgf_provisioner_required"},
		"no credential":        {"PUT", "/v1/kgf/workspace", "", "", 401, "unauthenticated"},
		"gateway user provisions": {"PUT", "/v1/kgf/workspace",
			mint(gw, scope(kgfctx.Context{Principal: "user:eve@a.test", Role: kgfctx.RoleOperator})), "", 403, "kgf_provisioner_required"},
	} {
		status, code := do(tc.method, tc.path, tc.ctx, tc.bearer)
		if status != tc.status || code != tc.code {
			t.Errorf("%s: got %d %s, want %d %s", name, status, code, tc.status, tc.code)
		}
	}

	// A delegated person in an active workspace is an ordinary principal of
	// its namespace (whoami and the full flow: the integration test).
	if status, code := do("GET", "/v1/namespaces/"+activeNS+"/runs", mint(front, delegated(active)), ""); status != http.StatusOK {
		t.Fatalf("delegated operator in an active workspace: %d %s", status, code)
	}
}
