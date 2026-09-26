// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package kgfctx verifies the KGF context: the Ed25519-signed token KGF Cloud's
// gateway mints for every request it forwards to a resource provider, in the
// X-KGF-Context header (ADR-0024).
//
// The format and every check mirror kgf-cloud's pkg/tenantctx exactly: same
// header, same claim names, same validity window, same audience and issuer
// rules. Nexus cannot import that module (a separate repository and module
// path), so the code is reproduced here and held to it by a conformance test
// that verifies a token minted by kgf-cloud's own Signer
// (testdata/tenantctx_conformance.json). Where Nexus is stricter, the
// difference is named at the check.
package kgfctx

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Header carries the signed context between services (tenantctx.Header).
const Header = "X-KGF-Context"

// RequestIDHeader is the header KGF services report the request id in
// (kgf-cloud pkg/httpkit.RequestIDHeader).
const RequestIDHeader = "X-Request-ID"

// Roles recognised across KGF (tenantctx.Role*).
const (
	RoleOperator = "operator"
	RoleViewer   = "viewer"
	RoleService  = "service"
)

// Context is the verified scope of a request. JSON names are tenantctx's.
type Context struct {
	Organization   string `json:"org"`
	Subscription   string `json:"sub,omitempty"`
	ResourceGroup  string `json:"rg,omitempty"`
	Principal      string `json:"principal"`
	Role           string `json:"role"`
	RequestID      string `json:"request_id,omitempty"`
	OnBehalfOf     string `json:"obo,omitempty"`
	OnBehalfOfRole string `json:"obo_role,omitempty"`
}

// Validate checks the fields every consumer relies on (tenantctx.Validate).
func (c Context) Validate() error {
	if c.Organization == "" {
		return errors.New("organization is required")
	}
	if c.Principal == "" {
		return errors.New("principal is required")
	}
	if c.OnBehalfOf != "" {
		if c.Role != RoleService {
			return errors.New("only a service context may act on behalf of a principal")
		}
		if c.OnBehalfOfRole != RoleOperator && c.OnBehalfOfRole != RoleViewer {
			return errors.New("a delegated context must name the role it acts with")
		}
	} else if c.OnBehalfOfRole != "" {
		return errors.New("a delegated role without a principal is not a delegation")
	}
	switch c.Role {
	case RoleOperator, RoleViewer, RoleService:
		return nil
	}
	return errors.New("unknown role")
}

// IsService reports whether the caller is a KGF service acting for itself.
func (c Context) IsService() bool { return c.Role == RoleService && c.OnBehalfOf == "" }

// IsDelegated reports whether a service is acting for a person.
func (c Context) IsDelegated() bool { return c.Role == RoleService && c.OnBehalfOf != "" }

// Effective returns the principal and role a decision is made against: the
// delegated person when there is one, otherwise the caller.
func (c Context) Effective() (principal, role string) {
	if c.IsDelegated() {
		return c.OnBehalfOf, c.OnBehalfOfRole
	}
	return c.Principal, c.Role
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// Claims is the full signed payload.
type Claims struct {
	Context
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	TokenID  string `json:"jti"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

var enc = base64.RawURLEncoding

// MaxTTL bounds how long a minted context stays valid (tenantctx.MaxTTL).
const MaxTTL = 5 * time.Minute

// FutureSkew is how far in the future an iat may be (tenantctx: 60 s).
const FutureSkew = 60 * time.Second

// AnyAudience is tenantctx's wildcard audience.
const AnyAudience = "*"

// MaxTokenBytes bounds a presented token (tenantctx: 8192).
const MaxTokenBytes = 8192

// Errors. Callers answer all of them with one opaque 401; the distinction is
// for logs and tests.
var (
	ErrInvalid           = errors.New("kgf context invalid")
	ErrExpired           = errors.New("kgf context expired or not yet valid")
	ErrWrongAudience     = errors.New("kgf context was minted for another service")
	ErrIssuerNotEntitled = errors.New("kgf context issuer may not assert a user principal")
	ErrReplayed          = errors.New("kgf context has already been used")
	ErrUnknownKey        = errors.New("kgf context signed by an unknown key")
	ErrNoKeys            = errors.New("no KGF context verification keys are loaded")
)

// KeySource resolves a key id to a trusted public key.
type KeySource interface {
	Key(kid string) (ed25519.PublicKey, error)
}

// ReplayGuard remembers token identifiers until they expire.
type ReplayGuard interface {
	Observe(id string, expires time.Time) bool
}

// Verifier accepts contexts signed by any of its keys, for its audience.
type Verifier struct {
	Keys KeySource
	// Audience is this service's name; a token must name it.
	Audience string
	// AcceptAnyAudience admits tenantctx's wildcard audience. Off by default
	// in Nexus (tenantctx accepts it): a wildcard token is replayable against
	// every provider.
	AcceptAnyAudience bool
	// UserIssuers lists issuers entitled to carry a human principal. Empty
	// means every issuer (tenantctx's legacy behaviour); Nexus configures it.
	UserIssuers []string
	Seen        ReplayGuard
}

func (v Verifier) mayAssertUser(issuer string) bool {
	if len(v.UserIssuers) == 0 {
		return true
	}
	for _, candidate := range v.UserIssuers {
		if candidate == issuer {
			return true
		}
	}
	return false
}

// Verify checks the signature, lifetime, audience, issuer and replay, in
// tenantctx's order, and returns the claims.
func (v Verifier) Verify(token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > MaxTokenBytes {
		return Claims{}, ErrInvalid
	}
	hb, e1 := enc.DecodeString(parts[0])
	pb, e2 := enc.DecodeString(parts[1])
	sig, e3 := enc.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return Claims{}, ErrInvalid
	}
	var h header
	if e := json.Unmarshal(hb, &h); e != nil || h.Alg != "EdDSA" {
		return Claims{}, ErrInvalid
	}
	if v.Keys == nil {
		return Claims{}, ErrNoKeys
	}
	key, err := v.Keys.Key(h.Kid)
	if err != nil {
		return Claims{}, err
	}
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if e := json.Unmarshal(pb, &c); e != nil || c.Context.Validate() != nil {
		return Claims{}, ErrInvalid
	}
	if c.Expires <= now.Unix() || c.IssuedAt > now.Add(FutureSkew).Unix() {
		return Claims{}, ErrExpired
	}
	// Stricter than tenantctx: a token claiming a lifetime beyond MaxTTL was
	// not minted by a conforming signer (MintFor clamps to MaxTTL).
	if c.Expires-c.IssuedAt > int64(MaxTTL/time.Second) {
		return Claims{}, ErrInvalid
	}
	if v.Audience != "" && c.Audience != v.Audience && !(v.AcceptAnyAudience && c.Audience == AnyAudience) {
		return Claims{}, ErrWrongAudience
	}
	if !c.Context.IsService() && !v.mayAssertUser(c.Issuer) {
		return Claims{}, ErrIssuerNotEntitled
	}
	if v.Seen != nil && c.TokenID != "" && !v.Seen.Observe(c.TokenID, time.Unix(c.Expires, 0)) {
		return Claims{}, ErrReplayed
	}
	return c, nil
}

// KeyID derives the stable identifier of a public key (tenantctx.KeyID).
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "kgf-" + hex.EncodeToString(sum[:6])
}

// EncodePublicKey renders a public key as tenantctx does.
func EncodePublicKey(k ed25519.PublicKey) string { return enc.EncodeToString(k) }

// DecodePublicKey parses a key rendered by EncodePublicKey.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, e := enc.DecodeString(strings.TrimSpace(s))
	if e != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid public key")
	}
	return ed25519.PublicKey(b), nil
}

// Signer mints contexts in tenantctx's format. Nexus never mints contexts in
// production; this exists so tests and the conformance check produce tokens
// byte-compatible with kgf-gateway's (tenantctx.Signer.MintFor).
type Signer struct {
	KeyID  string
	Key    ed25519.PrivateKey
	Issuer string
}

// NewSigner builds a signer whose kid is derived like tenantctx's.
func NewSigner(issuer string, key ed25519.PrivateKey) Signer {
	return Signer{KeyID: KeyID(key.Public().(ed25519.PublicKey)), Key: key, Issuer: issuer}
}

// MintFor signs c for one audience (tenantctx.Signer.MintFor).
func (s Signer) MintFor(audience string, c Context, ttl time.Duration, now time.Time) (string, error) {
	if e := c.Validate(); e != nil {
		return "", e
	}
	if s.Key == nil || s.KeyID == "" || audience == "" {
		return "", errors.New("signing key and audience required")
	}
	if ttl <= 0 || ttl > MaxTTL {
		ttl = MaxTTL
	}
	id := make([]byte, 16)
	if _, e := rand.Read(id); e != nil {
		return "", e
	}
	return s.MintClaims(Claims{Context: c, Issuer: s.Issuer, Audience: audience, TokenID: enc.EncodeToString(id),
		IssuedAt: now.Unix(), Expires: now.Add(ttl).Unix()})
}

// MintClaims signs an arbitrary claim set, including ones MintFor would
// refuse. For tests that need expired, future or malformed tokens.
func (s Signer) MintClaims(c Claims) (string, error) {
	h, _ := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", Kid: s.KeyID})
	p, _ := json.Marshal(c)
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(p)
	return signing + "." + enc.EncodeToString(ed25519.Sign(s.Key, []byte(signing))), nil
}
