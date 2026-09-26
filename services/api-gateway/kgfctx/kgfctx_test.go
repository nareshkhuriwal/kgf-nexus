// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var now = time.Unix(1790380800, 0)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func user() Context {
	return Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1",
		Principal: "user:alice@a.test", Role: RoleOperator, RequestID: "req-1"}
}

func verifierFor(keys ...ed25519.PrivateKey) Verifier {
	s := StaticKeys{}
	for _, k := range keys {
		pub := k.Public().(ed25519.PublicKey)
		s[KeyID(pub)] = pub
	}
	return Verifier{Keys: s, Audience: "kgf-nexus", UserIssuers: []string{"kgf-gateway"}, Seen: NewMemoryReplayGuard()}
}

// TestConformanceWithKGFTenantctx verifies tokens minted by kgf-cloud's own
// pkg/tenantctx (testdata, generated from that code), and checks this
// package's Signer produces byte-identical tokens from the same claims and
// seed, so the two formats cannot drift apart silently.
func TestConformanceWithKGFTenantctx(t *testing.T) {
	raw, err := os.ReadFile("testdata/tenantctx_conformance.json")
	if err != nil {
		t.Fatal(err)
	}
	var vec struct {
		PublicKey, Kid, Audience, UserToken, DelegatedToken string
		Now                                                 int64
	}
	if err := json.Unmarshal(raw, &struct {
		PublicKey      *string `json:"public_key"`
		Kid            *string `json:"kid"`
		Audience       *string `json:"audience"`
		UserToken      *string `json:"user_token"`
		DelegatedToken *string `json:"delegated_token"`
		Now            *int64  `json:"now"`
	}{&vec.PublicKey, &vec.Kid, &vec.Audience, &vec.UserToken, &vec.DelegatedToken, &vec.Now}); err != nil {
		t.Fatal(err)
	}
	keys, err := ParseStaticKeys(vec.PublicKey)
	if err != nil || keys[vec.Kid] == nil {
		t.Fatalf("kid derivation differs from tenantctx: %v %v", keys, err)
	}
	v := Verifier{Keys: keys, Audience: vec.Audience, UserIssuers: []string{"kgf-gateway"}}
	at := time.Unix(vec.Now, 0).Add(10 * time.Second)
	c, err := v.Verify(vec.UserToken, at)
	if err != nil {
		t.Fatalf("a kgf-cloud token did not verify: %v", err)
	}
	if c.Organization != "org-acme" || c.Subscription != "Sub-0001" || c.ResourceGroup != "Data-RG" ||
		c.Principal != "user:alice@acme.test" || c.Role != RoleOperator || c.RequestID != "req-7f3a" ||
		c.Issuer != "kgf-gateway" || c.Audience != "kgf-nexus" || c.TokenID == "" || c.Expires-c.IssuedAt != 60 {
		t.Fatalf("claims %+v", c)
	}
	d, err := v.Verify(vec.DelegatedToken, at)
	if err != nil {
		t.Fatal(err)
	}
	if p, r := d.Effective(); p != "user:bob@acme.test" || r != RoleViewer || !d.IsDelegated() {
		t.Fatalf("delegation %+v", d)
	}

	// Re-mint the same claims with the same seed: Ed25519 is deterministic,
	// so a conforming encoder must reproduce the token byte for byte.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	s := NewSigner("kgf-gateway", ed25519.NewKeyFromSeed(seed))
	for _, tok := range []string{vec.UserToken, vec.DelegatedToken} {
		payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[1])
		var cl Claims
		_ = json.Unmarshal(payload, &cl)
		again, _ := s.MintClaims(cl)
		if again != tok {
			t.Fatalf("encoding drift from tenantctx:\n kgf:   %s\n nexus: %s", tok, again)
		}
	}
}

func TestVerifyValidAndDelegated(t *testing.T) {
	k := newKey(t)
	s := NewSigner("kgf-gateway", k)
	v := verifierFor(k)
	tok, _ := s.MintFor("kgf-nexus", user(), time.Minute, now)
	c, err := v.Verify(tok, now.Add(5*time.Second))
	if err != nil || c.Principal != "user:alice@a.test" || c.RequestID != "req-1" {
		t.Fatalf("%+v %v", c, err)
	}
	// A KGF service delegating for a person, minted by an issuer entitled to
	// carry people (the facade listed in UserIssuers).
	v.UserIssuers = append(v.UserIssuers, "kgf-nexus-facade")
	svc := NewSigner("kgf-nexus-facade", k)
	del := Context{Organization: "org-a", Subscription: "sub-1", ResourceGroup: "rg-1", Principal: "svc:kgf-nexus-facade",
		Role: RoleService, OnBehalfOf: "user:bob@a.test", OnBehalfOfRole: RoleViewer}
	tok, _ = svc.MintFor("kgf-nexus", del, time.Minute, now)
	if _, err := v.Verify(tok, now); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejects(t *testing.T) {
	k, other := newKey(t), newKey(t)
	s := NewSigner("kgf-gateway", k)
	good, _ := s.MintFor("kgf-nexus", user(), time.Minute, now)
	parts := strings.Split(good, ".")
	flip := func(seg string) string {
		b, _ := base64.RawURLEncoding.DecodeString(seg)
		b[len(b)/2] ^= 1
		return base64.RawURLEncoding.EncodeToString(b)
	}
	forged := func(mutate func(*Claims)) string {
		c := Claims{Context: user(), Issuer: "kgf-gateway", Audience: "kgf-nexus", TokenID: "t-" + t.Name(),
			IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix()}
		mutate(&c)
		tok, _ := s.MintClaims(c)
		return tok
	}
	otherSigner := NewSigner("kgf-gateway", other)
	sameKidOtherKey := Signer{KeyID: s.KeyID, Key: other, Issuer: "kgf-gateway"}
	tokOther, _ := otherSigner.MintFor("kgf-nexus", user(), time.Minute, now)
	tokSameKid, _ := sameKidOtherKey.MintFor("kgf-nexus", user(), time.Minute, now)
	claimsPart := func(c Context) string {
		b, _ := json.Marshal(Claims{Context: c, Issuer: "kgf-gateway", Audience: "kgf-nexus", IssuedAt: now.Unix(), Expires: now.Add(time.Minute).Unix()})
		return base64.RawURLEncoding.EncodeToString(b)
	}
	escalated := user()
	escalated.Organization = "org-b"
	cases := []struct {
		name string
		tok  string
		at   time.Time
		want error
	}{
		{"tampered payload", parts[0] + "." + flip(parts[1]) + "." + parts[2], now, ErrInvalid},
		{"payload swapped to another organization", parts[0] + "." + claimsPart(escalated) + "." + parts[2], now, ErrInvalid},
		{"tampered signature", parts[0] + "." + parts[1] + "." + flip(parts[2]), now, ErrInvalid},
		{"unknown key", tokOther, now, ErrUnknownKey},
		{"known kid, other key", tokSameKid, now, ErrInvalid},
		{"expired", good, now.Add(61 * time.Second), ErrExpired},
		{"issued in the future", forged(func(c *Claims) { c.IssuedAt = now.Add(2 * time.Minute).Unix(); c.Expires = now.Add(3 * time.Minute).Unix() }), now, ErrExpired},
		{"lifetime beyond MaxTTL", forged(func(c *Claims) { c.Expires = now.Add(time.Hour).Unix() }), now, ErrInvalid},
		{"wrong audience", forged(func(c *Claims) { c.Audience = "kgf-compute" }), now, ErrWrongAudience},
		{"wildcard audience", forged(func(c *Claims) { c.Audience = AnyAudience }), now, ErrWrongAudience},
		{"issuer not entitled to a user", forged(func(c *Claims) { c.Issuer = "kgf-billing" }), now, ErrIssuerNotEntitled},
		{"unknown role", forged(func(c *Claims) { c.Role = "admin" }), now, ErrInvalid},
		{"delegation by a non-service", forged(func(c *Claims) { c.OnBehalfOf = "user:x"; c.OnBehalfOfRole = RoleOperator }), now, ErrInvalid},
		{"no organization", forged(func(c *Claims) { c.Organization = "" }), now, ErrInvalid},
		{"alg none", base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"`+s.KeyID+`"}`)) + "." + parts[1] + ".", now, ErrInvalid},
		{"two segments", parts[0] + "." + parts[1], now, ErrInvalid},
		{"oversized", strings.Repeat("a", MaxTokenBytes+1), now, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifierFor(k).Verify(tc.tok, tc.at)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	// The wildcard is accepted only when explicitly allowed (tenantctx's default).
	v := verifierFor(k)
	v.AcceptAnyAudience = true
	if _, err := v.Verify(forged(func(c *Claims) { c.Audience = AnyAudience; c.TokenID = "wild" }), now); err != nil {
		t.Fatalf("AcceptAnyAudience: %v", err)
	}
	// A pure service context from a non-user issuer is fine: it asserts no person.
	svcTok := forged(func(c *Claims) { c.Issuer = "kgf-billing"; c.Principal = "svc:kgf-billing"; c.Role = RoleService; c.TokenID = "svc" })
	if _, err := verifierFor(k).Verify(svcTok, now); err != nil {
		t.Fatalf("service context: %v", err)
	}
}

func TestReplayIsRefusedWithinTheWindow(t *testing.T) {
	k := newKey(t)
	v := verifierFor(k)
	guard := NewMemoryReplayGuard()
	clock := now
	guard.Now = func() time.Time { return clock }
	v.Seen = guard
	tok, _ := NewSigner("kgf-gateway", k).MintFor("kgf-nexus", user(), time.Minute, now)
	if _, err := v.Verify(tok, now); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(tok, now.Add(time.Second)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("replay inside the window: %v", err)
	}
	// Outside the window the token is expired, which refuses it regardless of
	// what the guard still remembers.
	clock = now.Add(2 * time.Minute)
	if _, err := v.Verify(tok, clock); !errors.Is(err, ErrExpired) {
		t.Fatalf("replay outside the window: %v", err)
	}
}

type keyServer struct {
	srv   *httptest.Server
	keys  atomic.Value // []ed25519.PublicKey
	hits  atomic.Int32
	fail  atomic.Bool
	extra atomic.Value // raw extra entries
}

func newKeyServer(t *testing.T, keys ...ed25519.PublicKey) *keyServer {
	ks := &keyServer{}
	ks.keys.Store(keys)
	ks.extra.Store([]map[string]string{})
	ks.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ks.hits.Add(1)
		if ks.fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		out := []map[string]string{}
		for _, k := range ks.keys.Load().([]ed25519.PublicKey) {
			out = append(out, map[string]string{"kid": KeyID(k), "alg": "EdDSA", "key": EncodePublicKey(k)})
		}
		out = append(out, ks.extra.Load().([]map[string]string)...)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": out})
	}))
	t.Cleanup(ks.srv.Close)
	return ks
}

func TestRemoteKeysRotation(t *testing.T) {
	k1, k2 := newKey(t), newKey(t)
	pub1, pub2 := k1.Public().(ed25519.PublicKey), k2.Public().(ed25519.PublicKey)
	ks := newKeyServer(t, pub1)
	clock := now
	rk := NewRemoteKeys(ks.srv.URL+"/.well-known/kgf-context.json", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rk.Now = func() time.Time { return clock }
	v := Verifier{Keys: rk, Audience: "kgf-nexus", UserIssuers: []string{"kgf-gateway"}}
	mint := func(k ed25519.PrivateKey) string {
		tok, _ := NewSigner("kgf-gateway", k).MintFor("kgf-nexus", user(), time.Minute, clock)
		return tok
	}
	if _, err := v.Verify(mint(k1), clock); err != nil {
		t.Fatal(err)
	}
	if err := rk.Ready(nil); err != nil {
		t.Fatal(err)
	}

	// Rotation: the gateway now publishes only k2. A k2 token is an unknown
	// kid, which triggers an immediate refetch.
	ks.keys.Store([]ed25519.PublicKey{pub2})
	clock = clock.Add(31 * time.Second)
	if _, err := v.Verify(mint(k2), clock); err != nil {
		t.Fatalf("rotated key not picked up on an unknown kid: %v", err)
	}
	// k1 tokens minted just before the rotation still verify during the grace.
	if _, err := v.Verify(mint(k1), clock.Add(30*time.Second)); err != nil {
		t.Fatalf("retired key refused inside its grace: %v", err)
	}
	// After the grace, k1 is gone.
	clock = clock.Add(MaxTTL + time.Second)
	if _, err := v.Verify(mint(k1), clock); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("retired key still trusted after its grace: %v", err)
	}

	// Unknown kids refetch at most once per MissInterval.
	before := ks.hits.Load()
	for i := 0; i < 20; i++ {
		_, _ = v.Verify(mint(newKey(t)), clock)
	}
	if got := ks.hits.Load() - before; got > 1 {
		t.Fatalf("unknown kids caused %d fetches; want at most 1", got)
	}

	// An outage keeps the keys already held.
	ks.fail.Store(true)
	clock = clock.Add(10 * time.Minute)
	if _, err := v.Verify(mint(k2), clock); err != nil {
		t.Fatalf("a failed refresh dropped the held key: %v", err)
	}
}

func TestRemoteKeysIgnoresMismatchedKids(t *testing.T) {
	k1, evil := newKey(t), newKey(t)
	ks := newKeyServer(t)
	// A document claiming k1's kid for another key must not be trusted.
	ks.extra.Store([]map[string]string{{"kid": KeyID(k1.Public().(ed25519.PublicKey)), "alg": "EdDSA", "key": EncodePublicKey(evil.Public().(ed25519.PublicKey))}})
	rk := NewRemoteKeys(ks.srv.URL, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := rk.Ready(nil); err == nil {
		t.Fatal("a document with only a mismatched kid counted as ready")
	}
	tok, _ := Signer{KeyID: KeyID(k1.Public().(ed25519.PublicKey)), Key: evil, Issuer: "kgf-gateway"}.MintFor("kgf-nexus", user(), time.Minute, time.Now())
	if _, err := (Verifier{Keys: rk, Audience: "kgf-nexus"}).Verify(tok, time.Now()); err == nil {
		t.Fatal("a key published under someone else's kid was trusted")
	}
	// No keys at all: fail closed.
	if _, err := (Verifier{Audience: "kgf-nexus"}).Verify(tok, time.Now()); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("no key source: %v", err)
	}
}
