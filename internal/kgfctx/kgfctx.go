// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package kgfctx verifies the KGF Cloud request context (the X-KGF-Context
// token KGF's gateway signs) so the Nexus audit trail can record which KGF
// organization, subscription, resource group and principal a request came
// from (ADR-0025 §2).
//
// It is a verifier only, wire-compatible with kgf-cloud's pkg/tenantctx: an
// EdDSA JWS whose header names the key (kid) and whose claims carry org,
// sub, rg, principal, role, request_id, obo, iss, aud, iat, exp. Nexus does
// NOT authorize on it -- Nexus's own credentials still decide what a caller
// may do -- it only ATTRIBUTES. A token that does not verify contributes
// nothing to the audit row, so a caller cannot put words in KGF's mouth.
package kgfctx

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Header is KGF's context header (pkg/tenantctx.Header).
const Header = "X-KGF-Context"

// RequestIDHeader is KGF's request id header (pkg/httpkit.RequestIDHeader).
const RequestIDHeader = "X-Request-ID"

// Env names, mirroring kgf-cloud's KGF_CONTEXT_* with a NEXUS_ prefix.
const (
	EnvPublicKeys  = "NEXUS_KGF_CONTEXT_PUBLIC_KEYS"  // comma-separated base64url Ed25519 public keys
	EnvAudience    = "NEXUS_KGF_CONTEXT_AUDIENCE"     // this service's KGF name; default "nexus"
	EnvUserIssuers = "NEXUS_KGF_CONTEXT_USER_ISSUERS" // issuers entitled to assert a human principal, e.g. "gateway"
)

// Context is the verified KGF scope of a request.
type Context struct {
	Organization  string `json:"org"`
	Subscription  string `json:"sub,omitempty"`
	ResourceGroup string `json:"rg,omitempty"`
	Principal     string `json:"principal"`
	Role          string `json:"role"`
	RequestID     string `json:"request_id,omitempty"`
	OnBehalfOf    string `json:"obo,omitempty"`
}

type claims struct {
	Context
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
}

// Verifier checks KGF context tokens.
type Verifier struct {
	Keys        map[string]ed25519.PublicKey
	Audience    string
	UserIssuers []string
}

// KeyID is kgf-cloud's tenantctx.KeyID: "kgf-" + hex(sha256(pub)[:6]).
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "kgf-" + hex.EncodeToString(sum[:6])
}

var enc = base64.RawURLEncoding

// FromEnv builds a verifier; nil when no public keys are configured, which
// leaves KGF attribution off.
func FromEnv(getenv func(string) string) (*Verifier, error) {
	raw := strings.TrimSpace(getenv(EnvPublicKeys))
	if raw == "" {
		return nil, nil
	}
	v := &Verifier{Keys: map[string]ed25519.PublicKey{}, Audience: strings.TrimSpace(getenv(EnvAudience))}
	if v.Audience == "" {
		v.Audience = "nexus"
	}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		b, err := enc.DecodeString(item)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, errors.New(EnvPublicKeys + " contains an invalid Ed25519 public key (base64url)")
		}
		v.Keys[KeyID(b)] = ed25519.PublicKey(b)
	}
	for _, item := range strings.Split(getenv(EnvUserIssuers), ",") {
		if s := strings.TrimSpace(item); s != "" {
			v.UserIssuers = append(v.UserIssuers, s)
		}
	}
	return v, nil
}

// ErrInvalid is any verification failure; the reason is deliberately not
// distinguished to callers.
var ErrInvalid = errors.New("KGF context token did not verify")

// Verify checks signature, lifetime, audience and issuer entitlement, exactly
// as kgf-cloud's Verifier does (minus replay tracking, which only matters
// for authorization).
func (v *Verifier) Verify(token string, now time.Time) (Context, error) {
	if v == nil {
		return Context{}, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 8192 {
		return Context{}, ErrInvalid
	}
	hb, e1 := enc.DecodeString(parts[0])
	pb, e2 := enc.DecodeString(parts[1])
	sig, e3 := enc.DecodeString(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return Context{}, ErrInvalid
	}
	var h struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if json.Unmarshal(hb, &h) != nil || h.Alg != "EdDSA" {
		return Context{}, ErrInvalid
	}
	key, ok := v.Keys[h.Kid]
	if !ok || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Context{}, ErrInvalid
	}
	var c claims
	if json.Unmarshal(pb, &c) != nil || c.Organization == "" || c.Principal == "" {
		return Context{}, ErrInvalid
	}
	if c.Expires <= now.Unix() || c.IssuedAt > now.Unix()+60 {
		return Context{}, ErrInvalid
	}
	if v.Audience != "" && c.Audience != v.Audience && c.Audience != "*" {
		return Context{}, ErrInvalid
	}
	isService := c.Role == "service" && c.OnBehalfOf == ""
	if !isService && len(v.UserIssuers) > 0 {
		entitled := false
		for _, iss := range v.UserIssuers {
			if iss == c.Issuer {
				entitled = true
			}
		}
		if !entitled {
			return Context{}, ErrInvalid
		}
	}
	return c.Context, nil
}

type ctxKey struct{}
type reqKey struct{}

// WithRequestID attaches the request's correlation id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqKey{}, id)
}

// RequestIDFrom returns the correlation id, or "".
func RequestIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(reqKey{}).(string)
	return s
}

// SanitizeRequestID bounds a caller-supplied id: at most 128 printable ASCII
// characters, else "". It is recorded as a correlation id, never trusted.
func SanitizeRequestID(s string) string {
	if len(s) == 0 || len(s) > 128 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}

// With attaches a verified context.
func With(ctx context.Context, c Context) context.Context { return context.WithValue(ctx, ctxKey{}, c) }

// From returns the verified context, if any.
func From(ctx context.Context) (Context, bool) {
	c, ok := ctx.Value(ctxKey{}).(Context)
	return c, ok
}
