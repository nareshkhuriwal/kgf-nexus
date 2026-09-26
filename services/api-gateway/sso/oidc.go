// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sso

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Errors a callback can fail with. Each is shown to the user only as an opaque
// code; the detail goes to the audit log.
var (
	ErrIssuerMismatch = errors.New("oidc: authorization response iss does not match the provider")
	ErrNonceMismatch  = errors.New("oidc: id_token nonce does not match the login")
	ErrAZPMismatch    = errors.New("oidc: id_token azp is not this client")
	ErrTokenTooOld    = errors.New("oidc: id_token iat is too old")
	ErrTokenFuture    = errors.New("oidc: id_token iat is in the future")
	ErrNoIDToken      = errors.New("oidc: token response has no id_token")
	ErrMissingSubject = errors.New("oidc: id_token has no subject")
)

// asymmetricAlgs are the only ID-token algorithms accepted: never "none",
// never HMAC (whose key would be the client secret, i.e. forgeable by anyone
// holding it).
var asymmetricAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA,
}

// discoveryTTL bounds how long a discovered provider (endpoints, JWKS URL) is
// cached per replica. JWKS rotation does not wait for it: go-oidc refetches
// keys on an unknown kid.
const discoveryTTL = time.Hour

// OIDC is the relying party. Safe for concurrent use.
type OIDC struct {
	cfg *Config
	mu  sync.Mutex
	// providers caches discovery per provider id + revision, so an edit to the
	// issuer is never served from a stale entry.
	providers map[string]cachedProvider
}

type cachedProvider struct {
	p       *oidc.Provider
	fetched time.Time
	meta    discoveryMeta
}

type discoveryMeta struct {
	EndSessionEndpoint string   `json:"end_session_endpoint"`
	IssParamSupported  bool     `json:"authorization_response_iss_parameter_supported"`
	CodeChallenge      []string `json:"code_challenge_methods_supported"`
}

// NewOIDC builds the relying party.
func NewOIDC(cfg *Config) *OIDC {
	return &OIDC{cfg: cfg, providers: map[string]cachedProvider{}}
}

func (o *OIDC) provider(ctx context.Context, p store.SSOProviderRow) (cachedProvider, error) {
	key := fmt.Sprintf("%s@%d", p.ID, p.Revision)
	o.mu.Lock()
	c, ok := o.providers[key]
	o.mu.Unlock()
	if ok && o.cfg.now().Sub(c.fetched) < discoveryTTL {
		return c, nil
	}
	if err := o.cfg.ValidateIssuer(ctx, p.Issuer); err != nil {
		return cachedProvider{}, err
	}
	// Discovery runs on a background context with the SSRF-bounded client:
	// the provider is cached beyond this request, and go-oidc keeps the
	// context for later JWKS refreshes. The client carries its own timeouts.
	dctx := oidc.ClientContext(context.Background(), o.cfg.HTTPClient())
	prov, err := oidc.NewProvider(dctx, p.Issuer)
	if err != nil {
		return cachedProvider{}, fmt.Errorf("oidc discovery for %s: %w", p.Issuer, err)
	}
	var meta discoveryMeta
	if err := prov.Claims(&meta); err != nil {
		return cachedProvider{}, fmt.Errorf("oidc discovery metadata: %w", err)
	}
	c = cachedProvider{p: prov, fetched: o.cfg.now(), meta: meta}
	o.mu.Lock()
	for k := range o.providers {
		if strings.HasPrefix(k, p.ID+"@") {
			delete(o.providers, k)
		}
	}
	o.providers[key] = c
	o.mu.Unlock()
	return c, nil
}

func scopesWithOpenID(scopes []string) []string {
	out := []string{"openid"}
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s != "" && s != "openid" {
			out = append(out, s)
		}
	}
	return out
}

func (o *OIDC) oauthConfig(c cachedProvider, p store.SSOProviderRow, secret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.ClientID,
		ClientSecret: secret,
		Endpoint:     c.p.Endpoint(),
		RedirectURL:  o.cfg.OIDCRedirectURI(p.ID),
		Scopes:       scopesWithOpenID(p.Scopes),
	}
}

// AuthURL builds the authorization request: code flow, PKCE S256, state and
// nonce. The verifier and nonce are the caller's to store with the state.
func (o *OIDC) AuthURL(ctx context.Context, p store.SSOProviderRow, state, nonce, verifier string) (string, error) {
	c, err := o.provider(ctx, p)
	if err != nil {
		return "", err
	}
	cfg := o.oauthConfig(c, p, "")
	return cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oauth2.SetAuthURLParam("nonce", nonce)), nil
}

// CallbackParams are the authorization response parameters.
type CallbackParams struct {
	Code string
	// Iss is the RFC 9207 issuer identifier, when the IdP sends one.
	Iss string
}

// Exchange redeems the code with the PKCE verifier and verifies the ID token:
// signature (discovered JWKS, asymmetric algorithms only), iss, aud, exp with
// skew (go-oidc), then nonce, azp and iat here.
func (o *OIDC) Exchange(ctx context.Context, p store.SSOProviderRow, secret string, cb CallbackParams, nonce, verifier string) (store.SSOAssertedIdentity, error) {
	var id store.SSOAssertedIdentity
	c, err := o.provider(ctx, p)
	if err != nil {
		return id, err
	}
	// RFC 9207: a response carrying iss must name this provider; a provider that
	// advertises support must send it. Defeats IdP mix-up.
	if cb.Iss != "" && cb.Iss != p.Issuer {
		return id, ErrIssuerMismatch
	}
	if cb.Iss == "" && c.meta.IssParamSupported {
		return id, ErrIssuerMismatch
	}
	xctx := context.WithValue(ctx, oauth2.HTTPClient, o.cfg.HTTPClient())
	tok, err := o.oauthConfig(c, p, secret).Exchange(xctx, cb.Code, oauth2.VerifierOption(verifier))
	if err != nil {
		return id, fmt.Errorf("oidc code exchange: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return id, ErrNoIDToken
	}
	return o.VerifyIDToken(xctx, c, p, raw, nonce)
}

// VerifyIDToken verifies a raw ID token for a provider. Exported for tests.
func (o *OIDC) VerifyIDToken(ctx context.Context, c cachedProvider, p store.SSOProviderRow, raw, nonce string) (store.SSOAssertedIdentity, error) {
	var id store.SSOAssertedIdentity
	skew := o.cfg.ClockSkew
	if skew <= 0 {
		skew = DefaultClockSkew
	}
	verifier := c.p.Verifier(&oidc.Config{
		ClientID:             p.ClientID,
		SupportedSigningAlgs: asymmetricAlgs,
		// Leeway on exp: accept a token up to `skew` past its expiry.
		Now: func() time.Time { return o.cfg.now().Add(-skew) },
	})
	tok, err := verifier.Verify(ctx, raw)
	if err != nil {
		return id, fmt.Errorf("oidc id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(tok.Nonce), []byte(nonce)) != 1 || nonce == "" {
		return id, ErrNonceMismatch
	}
	var claims struct {
		AZP               string          `json:"azp"`
		IAT               int64           `json:"iat"`
		Email             string          `json:"email"`
		EmailVerified     json.RawMessage `json:"email_verified"`
		Name              string          `json:"name"`
		PreferredUsername string          `json:"preferred_username"`
	}
	if err := tok.Claims(&claims); err != nil {
		return id, fmt.Errorf("oidc claims: %w", err)
	}
	// OIDC Core 3.1.3.7: with several audiences azp must be present; when
	// present it must be this client.
	if (len(tok.Audience) > 1 && claims.AZP == "") || (claims.AZP != "" && claims.AZP != p.ClientID) {
		return id, ErrAZPMismatch
	}
	now := o.cfg.now()
	iat := time.Unix(claims.IAT, 0)
	if claims.IAT == 0 || iat.Before(now.Add(-MaxIDTokenAge-skew)) {
		return id, ErrTokenTooOld
	}
	if iat.After(now.Add(skew)) {
		return id, ErrTokenFuture
	}
	if tok.Subject == "" {
		return id, ErrMissingSubject
	}
	groups, err := groupsFromClaims(tok, p.GroupsClaim)
	if err != nil {
		return id, err
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	id = store.SSOAssertedIdentity{
		Subject:       tok.Subject,
		Email:         strings.ToLower(strings.TrimSpace(claims.Email)),
		EmailVerified: truthy(claims.EmailVerified),
		DisplayName:   name,
		Groups:        groups,
	}
	return id, nil
}

// truthy accepts JSON true and the string "true" (some IdPs, e.g. older AWS
// Cognito and some Entra configurations, send the latter). Anything else is
// false: an absent email_verified is NOT verified.
func truthy(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "true" || s == `"true"`
}

func groupsFromClaims(tok *oidc.IDToken, claim string) ([]string, error) {
	if claim == "" {
		claim = "groups"
	}
	var all map[string]json.RawMessage
	if err := tok.Claims(&all); err != nil {
		return nil, err
	}
	raw, ok := all[claim]
	if !ok {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return capGroups(list), nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil && one != "" {
		return []string{one}, nil
	}
	return nil, nil
}

// capGroups bounds what one token can make the gateway process.
func capGroups(in []string) []string {
	if len(in) > 1000 {
		in = in[:1000]
	}
	out := make([]string, 0, len(in))
	for _, g := range in {
		if g != "" && len(g) <= 256 {
			out = append(out, g)
		}
	}
	return out
}

// EndSessionURL is the RP-initiated logout URL, or "" when the IdP does not
// advertise an end_session_endpoint. client_id is sent instead of
// id_token_hint (ADR-0024 §11): the ID token is never stored.
func (o *OIDC) EndSessionURL(ctx context.Context, p store.SSOProviderRow) (string, error) {
	c, err := o.provider(ctx, p)
	if err != nil {
		return "", err
	}
	if c.meta.EndSessionEndpoint == "" {
		return "", nil
	}
	u, err := url.Parse(c.meta.EndSessionEndpoint)
	if err != nil || (u.Scheme != "https" && !(u.Scheme == "http" && o.cfg.AllowInsecureIssuers)) {
		return "", nil
	}
	q := u.Query()
	q.Set("client_id", p.ClientID)
	q.Set("post_logout_redirect_uri", o.cfg.PostLogoutRedirect())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Discover validates that an issuer's discovery document is reachable and
// usable, for the administrator's "test configuration" check.
func (o *OIDC) Discover(ctx context.Context, p store.SSOProviderRow) (map[string]any, error) {
	c, err := o.provider(ctx, p)
	if err != nil {
		return nil, err
	}
	s256 := false
	for _, m := range c.meta.CodeChallenge {
		if m == "S256" {
			s256 = true
		}
	}
	return map[string]any{
		"authorization_endpoint":  c.p.Endpoint().AuthURL,
		"token_endpoint":          c.p.Endpoint().TokenURL,
		"end_session_endpoint":    c.meta.EndSessionEndpoint,
		"pkce_s256_advertised":    s256,
		"iss_parameter_supported": c.meta.IssParamSupported,
	}, nil
}
