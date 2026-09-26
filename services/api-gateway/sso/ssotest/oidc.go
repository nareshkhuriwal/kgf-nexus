// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package ssotest provides in-process identity providers for tests: an OIDC
// provider that signs real ID tokens with a real JWKS, and a SAML signer that
// produces real XML-DSig signatures. They exist so the gateway's verification
// is exercised against genuine cryptography, including deliberately tampered,
// expired, mis-addressed and replayed messages. Tests only; never imported by
// production code.
package ssotest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// OIDCProvider is a minimal but real OpenID provider.
type OIDCProvider struct {
	Server   *httptest.Server
	Key      *rsa.PrivateKey
	KeyID    string
	ClientID string
	Secret   string

	mu sync.Mutex
	// codes maps an issued code to the authorization request it answers.
	codes map[string]authz
	// Claims returns the ID token claims for a code; tests mutate the map it
	// returns to tamper. Defaults to DefaultClaims.
	Claims func(nonce string) map[string]any
	// Sign overrides how a claims set becomes a token (e.g. alg=none,
	// another key). Defaults to RS256 with Key.
	Sign func(claims map[string]any) string
	// SendIss controls the RFC 9207 iss parameter on the authorization
	// response; IssOverride replaces its value.
	SendIss     bool
	IssOverride string
	// EndSession, when set, is advertised as end_session_endpoint.
	EndSession bool
	// Now is the IdP clock.
	Now func() time.Time
}

type authz struct {
	redirectURI string
	nonce       string
	challenge   string
}

// NewOIDCProvider starts a provider on a loopback httptest server.
func NewOIDCProvider(clientID, secret string) *OIDCProvider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p := &OIDCProvider{Key: key, KeyID: "k1", ClientID: clientID, Secret: secret, codes: map[string]authz{}, SendIss: true, EndSession: true, Now: time.Now}
	mux := http.NewServeMux()
	p.Server = httptest.NewServer(mux)
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /jwks", p.jwks)
	mux.HandleFunc("GET /authorize", p.authorize)
	mux.HandleFunc("POST /token", p.token)
	p.Claims = p.DefaultClaims
	p.Sign = p.SignRS256
	return p
}

// Issuer is the provider's issuer URL.
func (p *OIDCProvider) Issuer() string { return p.Server.URL }

// Close stops the server.
func (p *OIDCProvider) Close() { p.Server.Close() }

func (p *OIDCProvider) discovery(w http.ResponseWriter, _ *http.Request) {
	d := map[string]any{
		"issuer":                                p.Issuer(),
		"authorization_endpoint":                p.Issuer() + "/authorize",
		"token_endpoint":                        p.Issuer() + "/token",
		"jwks_uri":                              p.Issuer() + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"authorization_response_iss_parameter_supported": p.SendIss,
	}
	if p.EndSession {
		d["end_session_endpoint"] = p.Issuer() + "/logout"
	}
	_ = json.NewEncoder(w).Encode(d)
}

func (p *OIDCProvider) jwks(w http.ResponseWriter, _ *http.Request) {
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.Key.PublicKey, KeyID: p.KeyID, Algorithm: "RS256", Use: "sig"}}}
	_ = json.NewEncoder(w).Encode(set)
}

// authorize immediately "logs the user in" and redirects back with a code.
func (p *OIDCProvider) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := RandomString()
	p.mu.Lock()
	p.codes[code] = authz{redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	p.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	back := u.Query()
	back.Set("code", code)
	back.Set("state", q.Get("state"))
	if p.SendIss {
		iss := p.Issuer()
		if p.IssOverride != "" {
			iss = p.IssOverride
		}
		back.Set("iss", iss)
	}
	u.RawQuery = back.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (p *OIDCProvider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.Secret {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	a, found := p.codes[code]
	delete(p.codes, code) // codes are single use
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || a.redirectURI != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != a.challenge {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": RandomString(), "token_type": "Bearer", "expires_in": 300,
		"id_token": p.Sign(p.Claims(a.nonce)),
	})
}

// DefaultClaims is a valid claim set for alice.
func (p *OIDCProvider) DefaultClaims(nonce string) map[string]any {
	now := p.Now()
	return map[string]any{
		"iss": p.Issuer(), "sub": "alice-subject", "aud": p.ClientID, "azp": p.ClientID,
		"exp": now.Add(5 * time.Minute).Unix(), "iat": now.Unix(), "nonce": nonce,
		"email": "alice@acme.test", "email_verified": true, "name": "Alice Example",
		"groups": []string{"nexus-operators"},
	}
}

// SignRS256 signs claims with the provider key.
func (p *OIDCProvider) SignRS256(claims map[string]any) string {
	return SignJWT(p.Key, p.KeyID, jose.RS256, claims)
}

// SignJWT signs a claims set as a compact JWS.
func SignJWT(key crypto.Signer, kid string, alg jose.SignatureAlgorithm, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid))
	if err != nil {
		panic(err)
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		panic(err)
	}
	s, _ := jws.CompactSerialize()
	return s
}

// UnsignedJWT builds an alg=none token, which must always be refused.
func UnsignedJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + "."
}

// HS256JWT builds a token MAC'd with a shared secret (e.g. the client secret),
// which must always be refused.
func HS256JWT(secret []byte, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: secret}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		panic(err)
	}
	payload, _ := json.Marshal(claims)
	jws, _ := signer.Sign(payload)
	s, _ := jws.CompactSerialize()
	return s
}

// RandomString is 32 random bytes, base64url.
func RandomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
