// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package sso implements the protocol half of tenant single sign-on
// (ADR-0024): OIDC authorization code + PKCE relying party, SAML 2.0 service
// provider, secret-reference resolution, an SSRF-bounded HTTP client and
// return-URL validation. It holds no database state; the rest package owns
// the flow and the store owns persistence.
package sso

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is the installation-wide SSO policy. Tenants configure providers;
// only the operator configures this.
type Config struct {
	// PublicURL is the console's external origin (NEXUS_PUBLIC_URL). Redirect
	// URIs, the SAML ACS and entity IDs are built from it, and the gateway only
	// ever redirects browsers to fixed paths under it.
	PublicURL string
	// SecretDir holds per-namespace secret files: <dir>/<namespace>/<name>.
	SecretDir string
	// AllowEnvSecrets permits env:NEXUS_SSO_SECRET_* references. Off by
	// default: in a multi-tenant installation every tenant could name every
	// other tenant's variable.
	AllowEnvSecrets bool
	// AllowInsecureIssuers permits http:// issuers (development, tests).
	AllowInsecureIssuers bool
	// AllowPrivateIssuers permits issuers on loopback/private addresses.
	AllowPrivateIssuers bool
	// ClockSkew is the leeway applied to ID-token exp/iat.
	ClockSkew time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Getenv reads the environment for env: references; nil means os.Getenv.
	Getenv func(string) string
}

// DefaultClockSkew is the ID-token leeway: enough for unsynchronised VMs,
// small against a 5–60 minute token lifetime.
const DefaultClockSkew = 2 * time.Minute

// MaxIDTokenAge bounds how old an ID token's iat may be. A code is redeemed
// seconds after it is issued; an older token is a replay or a broken clock.
const MaxIDTokenAge = 10 * time.Minute

// Validate checks the configuration an operator supplied.
func (c *Config) Validate() error {
	if c.PublicURL == "" {
		return nil // SSO disabled; the routes answer 503 sso_unavailable.
	}
	u, err := url.Parse(c.PublicURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("NEXUS_PUBLIC_URL %q must be an absolute http(s) origin", c.PublicURL)
	}
	if u.Scheme == "http" && !isLocalHost(u.Hostname()) && !c.AllowInsecureIssuers {
		return errors.New("NEXUS_PUBLIC_URL must be https outside localhost: SSO cookies and redirects need a secure origin")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.ClockSkew <= 0 {
		c.ClockSkew = DefaultClockSkew
	}
	return nil
}

// Enabled reports whether SSO can run at all.
func (c *Config) Enabled() bool { return c != nil && c.PublicURL != "" }

// SecureCookies reports whether cookies must carry the Secure attribute.
func (c *Config) SecureCookies() bool { return strings.HasPrefix(c.PublicURL, "https://") }

func (c *Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func isLocalHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// Paths, relative to PublicURL. Per-provider callback and ACS URLs are the
// RFC 9700 mix-up defence: a response can only be processed as the provider
// its URL names.
func (c *Config) OIDCRedirectURI(providerID string) string {
	return c.PublicURL + "/v1/auth/sso/oidc/" + providerID + "/callback"
}
func (c *Config) SAMLACSURL(providerID string) string {
	return c.PublicURL + "/v1/auth/sso/saml/" + providerID + "/acs"
}
func (c *Config) SAMLEntityID(providerID string) string {
	return c.PublicURL + "/v1/auth/sso/saml/" + providerID + "/metadata"
}
func (c *Config) SAMLSLOURL(providerID string) string {
	return c.PublicURL + "/v1/auth/sso/saml/" + providerID + "/slo"
}
func (c *Config) CompleteURL() string        { return c.PublicURL + "/sso/complete" }
func (c *Config) LoginURL() string           { return c.PublicURL + "/login" }
func (c *Config) PostLogoutRedirect() string { return c.PublicURL + "/login?signed_out=1" }
