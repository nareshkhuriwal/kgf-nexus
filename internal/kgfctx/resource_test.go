// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

const wsID = "/subscriptions/sub-1/resourceGroups/rg-1/providers/KGF.Nexus/workspaces/etl-prod"

func TestParseWorkspaceID(t *testing.T) {
	id, err := ParseWorkspaceID(wsID)
	if err != nil || id.Subscription != "sub-1" || id.ResourceGroup != "rg-1" || id.Workspace != "etl-prod" {
		t.Fatalf("%+v %v", id, err)
	}
	if id.String() != wsID {
		t.Fatalf("canonical %q", id.String())
	}
	if id.Normalized() != strings.ToLower(wsID) {
		t.Fatalf("normalized %q", id.Normalized())
	}
	// Fixed segments and the provider are case-insensitive, as in ARM.
	mixed, err := ParseWorkspaceID("/SUBSCRIPTIONS/Sub-1/resourcegroups/RG-1/Providers/kgf.nexus/Workspaces/etl-prod")
	if err != nil || mixed.Normalized() != id.Normalized() {
		t.Fatalf("case-insensitive segments: %+v %v", mixed, err)
	}
	if !mixed.Matches("SUB-1", " rg-1 ") {
		t.Fatal("scope comparison must be case-insensitive")
	}
	if mixed.Matches("sub-2", "rg-1") || mixed.Matches("sub-1", "rg-2") {
		t.Fatal("matched another scope")
	}
	for name, bad := range map[string]string{
		"empty":             "",
		"no leading slash":  strings.TrimPrefix(wsID, "/"),
		"trailing slash":    wsID + "/",
		"extra segment":     wsID + "/runs/x",
		"other provider":    strings.Replace(wsID, "KGF.Nexus", "KGF.Compute", 1),
		"other type":        strings.Replace(wsID, "workspaces", "pipelines", 1),
		"missing rg":        "/subscriptions/sub-1/resourceGroups//providers/KGF.Nexus/workspaces/etl-prod",
		"upper-case ws":     strings.Replace(wsID, "etl-prod", "ETL-prod", 1),
		"ws too short":      strings.Replace(wsID, "etl-prod", "ab", 1),
		"ws starts digit":   strings.Replace(wsID, "etl-prod", "1etl", 1),
		"ws underscore":     strings.Replace(wsID, "etl-prod", "etl_prod", 1),
		"ws too long":       strings.Replace(wsID, "etl-prod", "a"+strings.Repeat("b", 48), 1),
		"space in rg":       strings.Replace(wsID, "rg-1", "rg 1", 1),
		"query":             wsID + "?x=1",
		"encoded slash":     strings.Replace(wsID, "rg-1", "rg%2F1", 1),
		"too long":          "/subscriptions/" + strings.Repeat("s", 600) + "/resourceGroups/rg/providers/KGF.Nexus/workspaces/etl",
		"resourcegroup key": "/subscriptions/sub-1/resourceGroup/rg-1/providers/KGF.Nexus/workspaces/etl-prod",
	} {
		if _, err := ParseWorkspaceID(bad); !errors.Is(err, ErrMalformedResourceID) {
			t.Errorf("%s: accepted %q", name, bad)
		}
	}
	// 48 characters is the maximum name.
	if _, err := ParseWorkspaceID(strings.Replace(wsID, "etl-prod", "a"+strings.Repeat("b", 47), 1)); err != nil {
		t.Errorf("48-character name refused: %v", err)
	}
}

func frontVerifier(keys ...ed25519.PrivateKey) Verifier {
	v := verifierFor(keys...)
	v.FrontIssuers = []string{"kgf-nexus"}
	return v
}

func TestVerifyWorkspaceContexts(t *testing.T) {
	gwKey, frontKey := newKey(t), newKey(t)
	v := frontVerifier(gwKey, frontKey)
	gw, front := NewSigner("kgf-gateway", gwKey), NewSigner("kgf-nexus", frontKey)
	mint := func(s Signer, c Context) string {
		t.Helper()
		tok, err := s.MintFor("kgf-nexus", c, time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	delegated := Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "svc:kgf-nexus",
		Role: RoleService, OnBehalfOf: "user:alice@a.test", OnBehalfOfRole: RoleViewer, Resource: wsID}
	provisioner := delegated
	provisioner.OnBehalfOf, provisioner.OnBehalfOfRole = "", ""

	// a. delegated user; b. provisioner.
	for name, c := range map[string]Context{"delegated": delegated, "provisioner": provisioner} {
		got, err := v.Verify(mint(front, c), now)
		if err != nil || got.Resource != wsID {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
	}
	// The claim round-trips under its tenantctx-style name.
	if !strings.Contains(string(mustPayload(t, mint(front, delegated))), `"res":"`+wsID+`"`) {
		t.Fatal("res is not serialised as the `res` claim")
	}

	for name, tc := range map[string]struct {
		s    Signer
		c    Context
		want error
	}{
		// Only fronts assert res.
		"gateway user with res":    {gw, func() Context { c := user(); c.Resource = wsID; return c }(), ErrResourceNotAllowed},
		"gateway service with res": {gw, provisioner, ErrResourceNotAllowed},
		// A front's token always carries res and is a service context.
		"front without res": {front, func() Context { c := delegated; c.Resource = ""; return c }(), ErrResourceRequired},
		"front provisioner without res": {front, func() Context {
			c := provisioner
			c.Resource = ""
			return c
		}(), ErrResourceRequired},
		"front as operator": {front, func() Context {
			c := user()
			c.Resource = wsID
			return c
		}(), ErrFrontShape},
		// res must be well-formed and in the token's own scope.
		"malformed res":    {front, func() Context { c := delegated; c.Resource = "/subscriptions/sub-1"; return c }(), ErrResourceInvalid},
		"res in other rg":  {front, func() Context { c := delegated; c.ResourceGroup = "rg-2"; return c }(), ErrResourceInvalid},
		"res in other sub": {front, func() Context { c := delegated; c.Subscription = "sub-2"; return c }(), ErrResourceInvalid},
		"no scope":         {front, func() Context { c := delegated; c.Subscription, c.ResourceGroup = "", ""; return c }(), ErrResourceInvalid},
	} {
		if _, err := v.Verify(mint(tc.s, tc.c), now); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}

	// Without configured fronts nobody may assert res, and a front issuer's
	// delegated context is an ordinary user-issuer question (refused).
	plain := verifierFor(gwKey, frontKey)
	if _, err := plain.Verify(mint(front, delegated), now); !errors.Is(err, ErrResourceNotAllowed) {
		t.Fatalf("res accepted with no fronts configured: %v", err)
	}
	// Existing gateway contexts are unaffected by configuring a front.
	if _, err := v.Verify(mint(gw, user()), now); err != nil {
		t.Fatalf("gateway user context: %v", err)
	}
	// A front is not a user issuer: it cannot mint a plain human context.
	if _, err := v.Verify(mint(front, user()), now); !errors.Is(err, ErrResourceRequired) {
		t.Fatalf("front minted a plain user context: %v", err)
	}
}

func mustPayload(t *testing.T, tok string) []byte {
	t.Helper()
	b, err := enc.DecodeString(strings.Split(tok, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseKeyURLs(t *testing.T) {
	got := ParseKeyURLs(" https://kgf.localhost/.well-known/kgf-context.json, ,https://front/.well-known/kgf-context.json,https://kgf.localhost/.well-known/kgf-context.json ")
	if len(got) != 2 || got[0] != "https://kgf.localhost/.well-known/kgf-context.json" || got[1] != "https://front/.well-known/kgf-context.json" {
		t.Fatalf("%q", got)
	}
	if len(ParseKeyURLs("")) != 0 {
		t.Fatal("empty list")
	}
}

// TestMultiKeysMergesSources: the gateway's and a front's key documents and
// static keys are one trust set.
func TestMultiKeysMergesSources(t *testing.T) {
	gwKey, frontKey, staticKey, stranger := newKey(t), newKey(t), newKey(t), newKey(t)
	pub := func(k ed25519.PrivateKey) ed25519.PublicKey { return k.Public().(ed25519.PublicKey) }
	gwDoc, frontDoc := newKeyServer(t, pub(gwKey)), newKeyServer(t, pub(frontKey))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	static := StaticKeys{KeyID(pub(staticKey)): pub(staticKey)}
	m := MultiKeys{static, NewRemoteKeys(gwDoc.srv.URL, nil, log), NewRemoteKeys(frontDoc.srv.URL, nil, log)}
	for _, k := range []ed25519.PrivateKey{gwKey, frontKey, staticKey} {
		got, err := m.Key(KeyID(pub(k)))
		if err != nil || !got.Equal(pub(k)) {
			t.Fatalf("key %s: %v", KeyID(pub(k)), err)
		}
	}
	if _, err := m.Key(KeyID(pub(stranger))); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown kid: %v", err)
	}
	if err := m.Ready(context.Background()); err != nil {
		t.Fatalf("ready: %v", err)
	}

	// One document down: the other issuer's contexts still verify, and the
	// gateway stays ready.
	frontDoc.fail.Store(true)
	partial := MultiKeys{NewRemoteKeys(frontDoc.srv.URL, nil, log), NewRemoteKeys(gwDoc.srv.URL, nil, log)}
	if _, err := partial.Key(KeyID(pub(gwKey))); err != nil {
		t.Fatalf("gateway key with the front's document down: %v", err)
	}
	if err := partial.Ready(context.Background()); err != nil {
		t.Fatalf("ready with one document: %v", err)
	}
	// Nothing anywhere: fail closed.
	gwDoc.fail.Store(true)
	none := MultiKeys{NewRemoteKeys(frontDoc.srv.URL, nil, log), NewRemoteKeys(gwDoc.srv.URL, nil, log)}
	if _, err := none.Key(KeyID(pub(gwKey))); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("no keys anywhere: %v", err)
	}
	if err := none.Ready(context.Background()); err == nil {
		t.Fatal("ready with no keys")
	}
}

func TestVerifyAudienceList(t *testing.T) {
	key := newKey(t)
	v := verifierFor(key)
	v.Audiences = []string{"kgf-nexus-engine"}
	s := NewSigner("kgf-gateway", key)
	for aud, ok := range map[string]bool{"kgf-nexus": true, "kgf-nexus-engine": true, "kgf-compute": false, "*": false} {
		tok, err := s.MintFor(aud, user(), time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v.Verify(tok, now); (err == nil) != ok || (!ok && !errors.Is(err, ErrWrongAudience)) {
			t.Errorf("audience %q: %v", aud, err)
		}
	}
}
