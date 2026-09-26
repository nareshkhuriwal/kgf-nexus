// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package sso_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso/ssotest"
)

func TestSafeReturnTo(t *testing.T) {
	good := []string{"/runs", "/runs/abc?tab=logs", "/admin/users#x", "/workflows/a%20b"}
	bad := []string{"", "runs", "//evil.test", "/\\evil.test", "https://evil.test/", "/%2F%2Fevil.test",
		"/%5Cevil", "/runs\n", "/v1/auth/logout", "/sso/complete", "javascript:alert(1)", "/" + strings.Repeat("a", 600),
		"http:/evil", "\\\\evil"}
	for _, g := range good {
		if got := sso.SafeReturnTo(g); got != g {
			t.Errorf("SafeReturnTo(%q) = %q, want it unchanged", g, got)
		}
	}
	for _, b := range bad {
		if got := sso.SafeReturnTo(b); got != sso.DefaultReturnTo {
			t.Errorf("open redirect: SafeReturnTo(%q) = %q, want %q", b, got, sso.DefaultReturnTo)
		}
	}
}

func TestSecretReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tenant-a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tenant-a", "okta"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top-level"), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"NEXUS_SSO_SECRET_OKTA": "from-env", "NEXUS_DATABASE_URL": "postgres://db"}
	cfg := &sso.Config{SecretDir: dir, Getenv: func(k string) string { return env[k] }}

	if v, err := cfg.ResolveSecret("tenant-a", "file:okta"); err != nil || v != "s3cret" {
		t.Fatalf("file ref: %q %v", v, err)
	}
	// Tenant B cannot name tenant A's secret: resolution is inside B's directory.
	if _, err := cfg.ResolveSecret("tenant-b", "file:okta"); !errors.Is(err, sso.ErrSecretUnavailable) {
		t.Fatalf("cross-tenant secret resolved: %v", err)
	}
	for _, ref := range []string{"file:../tenant-a/okta", "file:/etc/passwd", "file:..", "file:.hidden", "file:a/b", "okta", "vault:x"} {
		if _, err := cfg.ResolveSecret("tenant-a", ref); err == nil {
			t.Errorf("reference %q resolved; it must be refused", ref)
		}
	}
	if _, err := cfg.ResolveSecret("../", "file:top-level"); err == nil {
		t.Error("a namespace with a dot path escaped the secret directory")
	}
	// env: is off unless the operator allows it, and never reaches other variables.
	if _, err := cfg.ResolveSecret("tenant-a", "env:NEXUS_SSO_SECRET_OKTA"); err == nil {
		t.Error("env reference resolved while env secrets are disabled")
	}
	cfg.AllowEnvSecrets = true
	if v, err := cfg.ResolveSecret("tenant-a", "env:NEXUS_SSO_SECRET_OKTA"); err != nil || v != "from-env" {
		t.Fatalf("env ref: %q %v", v, err)
	}
	if _, err := cfg.ResolveSecret("tenant-a", "env:NEXUS_DATABASE_URL"); err == nil {
		t.Error("env reference outside NEXUS_SSO_SECRET_ resolved: a tenant could exfiltrate gateway credentials")
	}
}

func TestIssuerPolicyAndSSRF(t *testing.T) {
	ctx := context.Background()
	strict := &sso.Config{}
	for _, iss := range []string{"http://idp.example.com", "https://127.0.0.1/", "https://10.0.0.5", "https://169.254.169.254",
		"https://localhost", "ftp://idp", "https://user:pw@idp.example.com", "https://idp.example.com?x=1"} {
		if err := strict.ValidateIssuer(ctx, iss); err == nil {
			t.Errorf("issuer %q accepted by the strict policy", iss)
		}
	}
	if err := strict.ValidateIssuer(ctx, "https://login.microsoftonline.com/tid/v2.0"); err != nil {
		t.Errorf("public https issuer refused: %v", err)
	}
	// The dial-time guard: even a public-looking name that resolves to loopback
	// (DNS rebinding) is refused when the connection is made.
	srv := ssotest.NewOIDCProvider("c", "s")
	defer srv.Close()
	_, err := strict.HTTPClient().Get(srv.Issuer() + "/.well-known/openid-configuration")
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("strict client reached a loopback IdP: %v", err)
	}
	// Redirects are never followed.
	redir := &http.Server{}
	_ = redir
	relaxed := &sso.Config{AllowPrivateIssuers: true, AllowInsecureIssuers: true}
	if _, err := relaxed.HTTPClient().Get(srv.Issuer() + "/.well-known/openid-configuration"); err != nil {
		t.Fatalf("relaxed client: %v", err)
	}
}

// ── OIDC ─────────────────────────────────────────────────────────────────────

type oidcFixture struct {
	idp  *ssotest.OIDCProvider
	rp   *sso.OIDC
	cfg  *sso.Config
	prov store.SSOProviderRow
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	idp := ssotest.NewOIDCProvider("nexus-client", testSecret)
	t.Cleanup(idp.Close)
	cfg := &sso.Config{PublicURL: "http://localhost:3000", AllowInsecureIssuers: true, AllowPrivateIssuers: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	prov := store.SSOProviderRow{ID: "11111111-1111-1111-1111-111111111111", Namespace: "acme", Kind: "oidc",
		Issuer: idp.Issuer(), ClientID: "nexus-client", Scopes: []string{"openid", "email"}, GroupsClaim: "groups", Revision: 1}
	return &oidcFixture{idp: idp, rp: sso.NewOIDC(cfg), cfg: cfg, prov: prov}
}

// login runs the authorization request against the IdP and returns the
// callback parameters, the nonce and the verifier.
func (f *oidcFixture) login(t *testing.T) (sso.CallbackParams, string, string, string) {
	t.Helper()
	state, nonce, verifier := ssotest.RandomString(), ssotest.RandomString(), oauth2.GenerateVerifier()
	authURL, err := f.rp.AuthURL(context.Background(), f.prov, state, nonce, verifier)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	if u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("nonce") != nonce || u.Query().Get("state") != state {
		t.Fatalf("authorization request lacks PKCE/nonce/state: %s", authURL)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, _ := url.Parse(resp.Header.Get("Location"))
	if back.Query().Get("state") != state {
		t.Fatal("state not echoed")
	}
	return sso.CallbackParams{Code: back.Query().Get("code"), Iss: back.Query().Get("iss")}, state, nonce, verifier
}

func TestOIDCHappyPath(t *testing.T) {
	f := newOIDCFixture(t)
	cb, _, nonce, verifier := f.login(t)
	id, err := f.rp.Exchange(context.Background(), f.prov, testSecret, cb, nonce, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "alice-subject" || id.Email != "alice@acme.test" || !id.EmailVerified || len(id.Groups) != 1 {
		t.Fatalf("identity %+v", id)
	}
	end, err := f.rp.EndSessionURL(context.Background(), f.prov)
	if err != nil || !strings.Contains(end, "client_id=nexus-client") || !strings.Contains(end, "post_logout_redirect_uri=") {
		t.Fatalf("end session url %q %v", end, err)
	}
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	cases := []struct {
		name   string
		mutate func(f *oidcFixture)
		want   string
	}{
		{"tampered signature", func(f *oidcFixture) {
			f.idp.Sign = func(c map[string]any) string {
				tok := f.idp.SignRS256(c)
				parts := strings.Split(tok, ".")
				c["email"] = "ceo@acme.test"
				forged := ssotest.SignJWT(otherKey, "k1", jose.RS256, c)
				return parts[0] + "." + strings.Split(forged, ".")[1] + "." + parts[2]
			}
		}, "signature"},
		{"signed by an unknown key", func(f *oidcFixture) {
			f.idp.Sign = func(c map[string]any) string { return ssotest.SignJWT(otherKey, "k1", jose.RS256, c) }
		}, "signature"},
		{"alg none", func(f *oidcFixture) { f.idp.Sign = ssotest.UnsignedJWT }, "id_token"},
		{"HS256 with the client secret", func(f *oidcFixture) {
			f.idp.Sign = func(c map[string]any) string { return ssotest.HS256JWT([]byte(testSecret), c) }
		}, "id_token"},
		{"wrong audience", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any { c := base(n); c["aud"] = "someone-else"; delete(c, "azp"); return c }
		}, "audience"},
		{"wrong issuer", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any { c := base(n); c["iss"] = "https://evil.test"; return c }
		}, "different provider"},
		{"expired beyond skew", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any {
				c := base(n)
				c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
				c["iat"] = time.Now().Add(-11 * time.Minute).Unix()
				return c
			}
		}, "expired"},
		{"nonce mismatch (replayed token)", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(string) map[string]any { return base("a-nonce-from-another-login") }
		}, "nonce"},
		{"azp names another client", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any {
				c := base(n)
				c["aud"] = []string{"nexus-client", "other"}
				c["azp"] = "other"
				return c
			}
		}, "azp"},
		{"multiple audiences without azp", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any {
				c := base(n)
				c["aud"] = []string{"nexus-client", "other"}
				delete(c, "azp")
				return c
			}
		}, "azp"},
		{"iat far in the past", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any { c := base(n); c["iat"] = time.Now().Add(-2 * time.Hour).Unix(); return c }
		}, "too old"},
		{"iat in the future", func(f *oidcFixture) {
			base := f.idp.Claims
			f.idp.Claims = func(n string) map[string]any { c := base(n); c["iat"] = time.Now().Add(time.Hour).Unix(); return c }
		}, "future"},
		{"iss parameter from another provider (mix-up)", func(f *oidcFixture) { f.idp.IssOverride = "https://evil.test" }, "iss"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOIDCFixture(t)
			tc.mutate(f)
			cb, _, nonce, verifier := f.login(t)
			_, err := f.rp.Exchange(context.Background(), f.prov, testSecret, cb, nonce, verifier)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("rejected for the wrong reason: %v (want %q)", err, tc.want)
			}
		})
	}
}

func TestOIDCClockSkewAndCodeReplay(t *testing.T) {
	f := newOIDCFixture(t)
	base := f.idp.Claims
	// Expired 60 s ago: inside the 2-minute skew, accepted.
	f.idp.Claims = func(n string) map[string]any {
		c := base(n)
		c["exp"] = time.Now().Add(-60 * time.Second).Unix()
		return c
	}
	cb, _, nonce, verifier := f.login(t)
	if _, err := f.rp.Exchange(context.Background(), f.prov, testSecret, cb, nonce, verifier); err != nil {
		t.Fatalf("token within clock skew refused: %v", err)
	}
	// The same code a second time: the IdP refuses it.
	if _, err := f.rp.Exchange(context.Background(), f.prov, testSecret, cb, nonce, verifier); err == nil {
		t.Fatal("authorization code replay accepted")
	}
	// A wrong PKCE verifier: refused by the IdP.
	cb2, _, nonce2, _ := f.login(t)
	if _, err := f.rp.Exchange(context.Background(), f.prov, testSecret, cb2, nonce2, oauth2.GenerateVerifier()); err == nil {
		t.Fatal("wrong PKCE verifier accepted")
	}
	// A missing iss parameter when the IdP advertises RFC 9207 support.
	cb3, _, nonce3, v3 := f.login(t)
	cb3.Iss = ""
	if _, err := f.rp.Exchange(context.Background(), f.prov, testSecret, cb3, nonce3, v3); !errors.Is(err, sso.ErrIssuerMismatch) {
		t.Fatalf("missing iss accepted: %v", err)
	}
}

// ── SAML ─────────────────────────────────────────────────────────────────────

type samlFixture struct {
	idp  *ssotest.SAMLIdP
	sp   *sso.SAML
	cfg  *sso.Config
	prov store.SSOProviderRow
}

func newSAMLFixture(t *testing.T) *samlFixture {
	t.Helper()
	idp := ssotest.NewSAMLIdP("https://idp.acme.test")
	cfg := &sso.Config{PublicURL: "http://localhost:3000"}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	prov := store.SSOProviderRow{ID: "22222222-2222-2222-2222-222222222222", Namespace: "acme", Kind: "saml",
		IDPMetadataXML: idp.Metadata(), IDPEntityID: idp.EntityID, GroupsClaim: "groups", Revision: 1}
	return &samlFixture{idp: idp, sp: sso.NewSAML(cfg), cfg: cfg, prov: prov}
}

func (f *samlFixture) opts(reqID string) ssotest.ResponseOpts {
	return ssotest.ResponseOpts{
		ACS: f.cfg.SAMLACSURL(f.prov.ID), SPEntityID: f.cfg.SAMLEntityID(f.prov.ID), RequestID: reqID,
		NameID: "bob-persistent-id", Email: "bob@acme.test", Groups: []string{"nexus-admins", "staff"},
	}
}

func TestSAMLAuthnRequestAndMetadata(t *testing.T) {
	f := newSAMLFixture(t)
	u, reqID, err := f.sp.AuthnRequestURL(f.prov, "relay")
	if err != nil || reqID == "" || !strings.HasPrefix(u, f.idp.SSOURL+"?SAMLRequest=") {
		t.Fatalf("authn request %q %q %v", u, reqID, err)
	}
	md, err := f.sp.Metadata(f.prov)
	if err != nil || !strings.Contains(string(md), f.cfg.SAMLACSURL(f.prov.ID)) || !strings.Contains(string(md), `WantAssertionsSigned="true"`) {
		t.Fatalf("metadata: %v\n%s", err, md)
	}
}

func TestSAMLHappyPath(t *testing.T) {
	f := newSAMLFixture(t)
	for _, signResponse := range []bool{false, true} {
		o := f.opts("req-1")
		o.SignResponse = signResponse
		res, err := f.sp.ParseResponse(f.prov, f.idp.Response(o), "req-1")
		if err != nil {
			t.Fatalf("sign response=%v: %v", signResponse, err)
		}
		if res.Identity.Subject != "bob-persistent-id" || res.Identity.Email != "bob@acme.test" || res.Identity.EmailVerified ||
			len(res.Identity.Groups) != 2 || res.AssertionID == "" || res.SessionIndex == "" {
			t.Fatalf("result %+v", res)
		}
	}
}

func TestSAMLRejectsBadResponses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *samlFixture, o *ssotest.ResponseOpts)
		reqID  string
		want   error
	}{
		{"unsigned assertion", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.UnsignedAssert = true }, "req", sso.ErrSAMLUnsigned},
		{"only the response is signed", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.UnsignedAssert = true; o.SignResponse = true }, "req", sso.ErrSAMLUnsigned},
		{"signed by an unknown key", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.SignWithOtherKey = true }, "req", nil},
		{"attribute tampered after signing", func(_ *samlFixture, o *ssotest.ResponseOpts) {
			o.AfterSign = func(doc *etree.Document) {
				for _, el := range doc.FindElements("//AttributeValue") {
					if el.Text() == "bob@acme.test" {
						el.SetText("ceo@acme.test")
					}
				}
			}
		}, "req", nil},
		{"NameID tampered after signing", func(_ *samlFixture, o *ssotest.ResponseOpts) {
			o.AfterSign = func(doc *etree.Document) { doc.FindElement("//NameID").SetText("admin") }
		}, "req", nil},
		{"wrong audience", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.Audience = "https://other-sp.test" }, "req", nil},
		{"no audience restriction", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.NoAudience = true }, "req", nil},
		{"wrong recipient", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.Recipient = "https://other-sp.test/acs" }, "req", nil},
		{"wrong destination", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.Destination = "https://other-sp.test/acs" }, "req", nil},
		{"InResponseTo of another request", func(_ *samlFixture, _ *ssotest.ResponseOpts) {}, "a-different-request", nil},
		{"expired", func(_ *samlFixture, o *ssotest.ResponseOpts) {
			o.Now = time.Now().Add(-30 * time.Minute)
		}, "req", nil},
		{"NotOnOrAfter passed (fresh IssueInstant)", func(_ *samlFixture, o *ssotest.ResponseOpts) {
			o.Lifetime = -10 * time.Minute
		}, "req", nil},
		{"wrong issuer", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.Issuer = "https://evil-idp.test" }, "req", nil},
		{"transient NameID", func(_ *samlFixture, o *ssotest.ResponseOpts) {
			o.NameIDFormat = "urn:oasis:names:tc:SAML:2.0:nameid-format:transient"
		}, "req", sso.ErrSAMLTransientName},
		{"no subject confirmation", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.NoSubjectConf = true }, "req", sso.ErrSAMLNoSubject},
		{"no conditions", func(_ *samlFixture, o *ssotest.ResponseOpts) { o.NoConditions = true }, "req", sso.ErrSAMLNoConditions},
		{"signature wrapping: a second, unsigned assertion", func(f *samlFixture, o *ssotest.ResponseOpts) {
			evil := *o
			evil.NameID, evil.Email = "admin", "ceo@acme.test"
			o.AfterSign = func(doc *etree.Document) {
				doc.Root().InsertChildAt(2, f.idp.Assertion(evil))
			}
		}, "req", sso.ErrSAMLAssertionCount},
		{"signature wrapping: signed assertion hidden in Extensions", func(f *samlFixture, o *ssotest.ResponseOpts) {
			evil := *o
			evil.NameID = "admin"
			o.AfterSign = func(doc *etree.Document) {
				root := doc.Root()
				signed := root.FindElement("./Assertion")
				root.RemoveChild(signed)
				ext := root.CreateElement("samlp:Extensions")
				ext.AddChild(signed)
				root.AddChild(f.idp.Assertion(evil))
			}
		}, "req", sso.ErrSAMLAssertionCount},
		{"encrypted assertion", func(_ *samlFixture, o *ssotest.ResponseOpts) {
			o.AfterSign = func(doc *etree.Document) {
				doc.Root().CreateElement("saml:EncryptedAssertion")
			}
		}, "req", sso.ErrSAMLAssertionCount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSAMLFixture(t)
			o := f.opts("req")
			tc.mutate(f, &o)
			_, err := f.sp.ParseResponse(f.prov, f.idp.Response(o), tc.reqID)
			if err == nil {
				t.Fatal("accepted")
			}
			t.Logf("rejected: %v", err)
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("rejected for the wrong reason: %v (want %v)", err, tc.want)
			}
		})
	}
}

func TestSAMLMalformedInput(t *testing.T) {
	f := newSAMLFixture(t)
	for _, in := range []string{"", "not base64!!", "PGZvbz4=" /* <foo> */, strings.Repeat("A", sso.MaxSAMLResponse+1)} {
		if _, err := f.sp.ParseResponse(f.prov, in, "req"); err == nil {
			t.Errorf("accepted malformed %q", in[:min(len(in), 20)])
		}
	}
}

func TestParseIdPMetadataRequiresSigningCert(t *testing.T) {
	idp := ssotest.NewSAMLIdP("https://idp.acme.test")
	if _, err := sso.ParseIdPMetadata(idp.Metadata()); err != nil {
		t.Fatal(err)
	}
	stripped := strings.ReplaceAll(idp.Metadata(), "signing", "encryption")
	if _, err := sso.ParseIdPMetadata(stripped); err == nil {
		t.Fatal("metadata without a signing certificate accepted")
	}
	if _, err := sso.ParseIdPMetadata("<not-metadata/>"); err == nil {
		t.Fatal("garbage metadata accepted")
	}
}

const testSecret = "client-secret-long-enough-for-an-hmac-key"
