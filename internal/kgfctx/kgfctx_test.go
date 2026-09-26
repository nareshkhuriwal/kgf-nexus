// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package kgfctx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

// mint reproduces kgf-cloud's tenantctx.Signer.MintFor wire format, so this
// test pins compatibility with the tokens KGF's gateway sends.
func mint(t *testing.T, key ed25519.PrivateKey, kid string, c map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": kid})
	p, _ := json.Marshal(c)
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(p)
	return signing + "." + enc.EncodeToString(ed25519.Sign(key, []byte(signing)))
}

func TestVerifyIsCompatibleWithKGFTenantctx(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	env := map[string]string{EnvPublicKeys: enc.EncodeToString(pub), EnvUserIssuers: "gateway"}
	v, err := FromEnv(func(k string) string { return env[k] })
	if err != nil || v == nil || v.Audience != "nexus" {
		t.Fatalf("FromEnv: %v %v", v, err)
	}
	now := time.Now()
	base := func() map[string]any {
		return map[string]any{"org": "org-1", "sub": "sub-1", "rg": "rg-1", "principal": "user:ana", "role": "operator",
			"request_id": "req_1", "iss": "gateway", "aud": "nexus", "jti": "x", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	}
	kid := KeyID(pub)
	c, err := v.Verify(mint(t, priv, kid, base()), now)
	if err != nil || c.Organization != "org-1" || c.Principal != "user:ana" || c.RequestID != "req_1" || c.ResourceGroup != "rg-1" {
		t.Fatalf("verify: %+v %v", c, err)
	}
	bad := map[string]func() string{
		"wrong key":        func() string { return mint(t, otherPriv, kid, base()) },
		"unknown kid":      func() string { return mint(t, priv, "kgf-000000000000", base()) },
		"expired":          func() string { m := base(); m["exp"] = now.Add(-time.Second).Unix(); return mint(t, priv, kid, m) },
		"other audience":   func() string { m := base(); m["aud"] = "billing"; return mint(t, priv, kid, m) },
		"worker as a user": func() string { m := base(); m["iss"] = "inventory"; return mint(t, priv, kid, m) },
		"no organization":  func() string { m := base(); delete(m, "org"); return mint(t, priv, kid, m) },
		"garbage":          func() string { return "a.b.c" },
	}
	for name, tok := range bad {
		if _, err := v.Verify(tok(), now); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	// A service context from a non-gateway issuer is fine (it speaks for itself).
	m := base()
	m["iss"], m["role"], m["principal"] = "inventory", "service", "svc:inventory"
	if _, err := v.Verify(mint(t, priv, kid, m), now); err != nil {
		t.Errorf("service context: %v", err)
	}
	if v, err := FromEnv(func(string) string { return "" }); v != nil || err != nil {
		t.Errorf("unconfigured: %v %v", v, err)
	}
}
