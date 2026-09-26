// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build sso_e2e

// Package sso_test runs the gateway's SSO flows against a REAL Keycloak, as an
// OIDC provider and as a SAML 2.0 IdP, with a real browser-like client that
// fills Keycloak's own login form. Run it with tests/sso/run-keycloak-e2e.sh,
// which starts pinned Keycloak and PostgreSQL containers.
package sso_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso"
)

const (
	gatewayAddr = "127.0.0.1:55813"
	publicURL   = "http://localhost:55813"
	realm       = "nexus-e2e"
)

type env struct {
	t        *testing.T
	kc       string // Keycloak base URL
	ns, key  string
	pool     store.GWDB
	adminTok string
}

func setup(t *testing.T) *env {
	t.Helper()
	kc, dsn := os.Getenv("NEXUS_TEST_KEYCLOAK_URL"), os.Getenv("NEXUS_TEST_DATABASE_URL")
	if kc == "" || dsn == "" {
		t.Skip("skipping sso e2e: NEXUS_TEST_KEYCLOAK_URL / NEXUS_TEST_DATABASE_URL unset (run tests/sso/run-keycloak-e2e.sh)")
	}
	ctx := context.Background()
	pool, err := store.GWOpen(ctx, dsn, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	dir := t.TempDir()
	cfg := &sso.Config{PublicURL: publicURL, SecretDir: dir, AllowInsecureIssuers: true, AllowPrivateIssuers: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	api := rest.New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(rest.NewStoreIdentityAdmin(pool)).WithSSO(cfg)
	ln, err := net.Listen("tcp", gatewayAddr)
	if err != nil {
		t.Fatalf("gateway listener on %s: %v", gatewayAddr, err)
	}
	srv := &http.Server{Handler: api.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ns := fmt.Sprintf("sso-e2e-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (namespace, name) VALUES ($1, 'sso e2e')`, ns); err != nil {
		t.Fatal(err)
	}
	key := "nx_sso_e2e_" + ns
	if _, err := store.GatewayCreateAPIKey(ctx, pool, ns, "e2e-admin", "admin", key); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ns), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ns, "keycloak"), []byte("nexus-oidc-e2e-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, kc: kc, ns: ns, key: key, pool: pool}
	e.adminTok = e.keycloakAdminToken()
	return e
}

// api calls the gateway with a bearer credential.
func (e *env) api(method, path, body, bearer string, want int) map[string]any {
	e.t.Helper()
	req, _ := http.NewRequest(method, publicURL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: got %d want %d: %s", method, path, resp.StatusCode, want, b)
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// ── Keycloak admin API ───────────────────────────────────────────────────────

func (e *env) keycloakAdminToken() string {
	e.t.Helper()
	form := url.Values{"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"},
		"password": {os.Getenv("NEXUS_TEST_KEYCLOAK_ADMIN_PASSWORD")}}
	resp, err := http.PostForm(e.kc+"/realms/master/protocol/openid-connect/token", form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		e.t.Fatalf("keycloak admin token: %v", err)
	}
	return tok.AccessToken
}

func (e *env) kcAdmin(method, path string, body any, want int) []byte {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, e.kc+"/admin/realms/"+realm+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.adminTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("keycloak %s %s: %d %s", method, path, resp.StatusCode, b)
	}
	return b
}

func (e *env) kcID(path, field, value string) string {
	e.t.Helper()
	var items []map[string]any
	_ = json.Unmarshal(e.kcAdmin("GET", path, nil, 200), &items)
	for _, it := range items {
		if it[field] == value {
			return it["id"].(string)
		}
	}
	e.t.Fatalf("keycloak: no %s=%s under %s", field, value, path)
	return ""
}

// ── a browser ────────────────────────────────────────────────────────────────

type browser struct {
	t *testing.T
	c *http.Client
}

func newBrowser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, c: &http.Client{Jar: jar, Timeout: 30 * time.Second,
		// Stop at the console: the gateway's final redirect carries the handoff
		// in a fragment, which a real browser keeps and hands to the SPA.
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Host == "localhost:55813" && (req.URL.Path == "/sso/complete" || req.URL.Path == "/login") {
				return http.ErrUseLastResponse
			}
			return nil
		}}}
}

var (
	formAction = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)
	anyForm    = regexp.MustCompile(`(?s)<form[^>]*action="([^"]+)"`)
	hiddenIn   = regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*value="([^"]*)"`)
)

func body(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

// login starts SSO at the gateway, fills Keycloak's login form, follows the
// protocol back to the gateway, and returns the gateway's final redirect.
func (b *browser) login(providerID, username, password string) *url.URL {
	b.t.Helper()
	resp, err := b.c.Get(publicURL + "/v1/auth/sso/" + providerID + "/start?return_to=/runs")
	if err != nil {
		b.t.Fatal(err)
	}
	page := body(resp)
	m := formAction.FindStringSubmatch(page)
	if m == nil {
		b.t.Fatalf("no Keycloak login form (status %d): %.500s", resp.StatusCode, page)
	}
	resp, err = b.c.PostForm(html.UnescapeString(m[1]), url.Values{"username": {username}, "password": {password}, "credentialId": {""}})
	if err != nil {
		b.t.Fatal(err)
	}
	// SAML POST binding: Keycloak answers with an auto-submitting form.
	if resp.StatusCode == http.StatusOK {
		page = body(resp)
		fm := anyForm.FindStringSubmatch(page)
		if fm == nil || !strings.Contains(page, "SAMLResponse") {
			b.t.Fatalf("unexpected Keycloak page: %.800s", page)
		}
		vals := url.Values{}
		for _, in := range hiddenIn.FindAllStringSubmatch(page, -1) {
			vals.Set(html.UnescapeString(in[1]), html.UnescapeString(in[2]))
		}
		resp, err = b.c.PostForm(html.UnescapeString(fm[1]), vals)
		if err != nil {
			b.t.Fatal(err)
		}
	}
	defer resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("expected the gateway's 303, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc
}

// samlPost captures the SAMLResponse Keycloak would post, without posting it.
func (b *browser) samlResponse(providerID, username, password string) (acs string, form url.Values) {
	b.t.Helper()
	resp, err := b.c.Get(publicURL + "/v1/auth/sso/" + providerID + "/start?return_to=/runs")
	if err != nil {
		b.t.Fatal(err)
	}
	m := formAction.FindStringSubmatch(body(resp))
	if m == nil {
		b.t.Fatal("no login form")
	}
	resp, err = b.c.PostForm(html.UnescapeString(m[1]), url.Values{"username": {username}, "password": {password}, "credentialId": {""}})
	if err != nil {
		b.t.Fatal(err)
	}
	page := body(resp)
	fm := anyForm.FindStringSubmatch(page)
	form = url.Values{}
	for _, in := range hiddenIn.FindAllStringSubmatch(page, -1) {
		form.Set(html.UnescapeString(in[1]), html.UnescapeString(in[2]))
	}
	return html.UnescapeString(fm[1]), form
}

func (b *browser) exchange(loc *url.URL, want int) map[string]any {
	b.t.Helper()
	if loc.Path != "/sso/complete" || !strings.HasPrefix(loc.Fragment, "handoff=") {
		b.t.Fatalf("expected /sso/complete#handoff=..., got %s", loc)
	}
	resp, err := b.c.Post(publicURL+"/v1/auth/sso/exchange", "application/json",
		strings.NewReader(`{"handoff":"`+strings.TrimPrefix(loc.Fragment, "handoff=")+`"}`))
	if err != nil {
		b.t.Fatal(err)
	}
	s := body(resp)
	if resp.StatusCode != want {
		b.t.Fatalf("exchange: %d %s", resp.StatusCode, s)
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

func ssoError(loc *url.URL) string { return loc.Query().Get("sso_error") }

// ── OIDC ─────────────────────────────────────────────────────────────────────

func TestKeycloakOIDC(t *testing.T) {
	e := setup(t)
	issuer := e.kc + "/realms/" + realm
	prov := e.api("POST", "/v1/namespaces/"+e.ns+"/sso/providers", fmt.Sprintf(`{"kind":"oidc","slug":"keycloak","display_name":"Keycloak",
		"enabled":true,"issuer":%q,"client_id":"nexus-oidc","client_secret_ref":"file:keycloak","scopes":["email","profile"],
		"allowed_domains":["acme.test"],"jit_provisioning":true,"link_by_verified_email":true,"revoke_sessions_on_role_loss":true}`, issuer), e.key, 201)
	pid := prov["id"].(string)
	e.api("PUT", "/v1/namespaces/"+e.ns+"/sso/providers/"+pid+"/mappings",
		`{"mappings":[{"idp_group":"nexus-operators","role_name":"Workflow Operator"}],"revision":0}`, e.key, 200)

	// Alice: JIT-provisioned, role from her Keycloak group.
	b := newBrowser(t)
	res := b.exchange(b.login(pid, "alice", "alice-e2e-password"), 200)
	tok := res["token"].(string)
	who := e.api("GET", "/v1/auth/whoami", "", tok, 200)
	if who["name"] != "alice@acme.test" || fmt.Sprint(who["roles"]) != "[Workflow Operator]" {
		t.Fatalf("whoami %+v", who)
	}
	e.api("GET", "/v1/namespaces/"+e.ns+"/runs", "", tok, 200)
	e.api("GET", "/v1/namespaces/"+e.ns+"/users", "", tok, 403)

	// Second login in the same browser reuses Keycloak's SSO session (no form).
	resp, err := b.c.Get(publicURL + "/v1/auth/sso/" + pid + "/start")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	second := b.exchange(loc, 200)["token"].(string)

	// Remove Alice from the group in Keycloak: the role goes at her next login,
	// and her older sessions are revoked because the provider asks for it.
	uid := e.kcID("/users?username=alice&exact=true", "username", "alice")
	gid := e.kcID("/groups", "name", "nexus-operators")
	e.kcAdmin("DELETE", "/users/"+uid+"/groups/"+gid, nil, 204)
	t.Cleanup(func() { e.kcAdmin("PUT", "/users/"+uid+"/groups/"+gid, nil, 204) })
	nb := newBrowser(t)
	third := nb.exchange(nb.login(pid, "alice", "alice-e2e-password"), 200)["token"].(string)
	e.api("GET", "/v1/auth/whoami", "", tok, 401)
	e.api("GET", "/v1/auth/whoami", "", second, 401)
	if who := e.api("GET", "/v1/auth/whoami", "", third, 200); fmt.Sprint(who["roles"]) != "[]" {
		t.Fatalf("role not revoked after group removal: %+v", who)
	}
	e.api("GET", "/v1/namespaces/"+e.ns+"/runs", "", third, 403)

	// RP-initiated logout: the gateway ends the session and hands back
	// Keycloak's end_session_endpoint, which Keycloak accepts.
	out := e.api("POST", "/v1/auth/logout", "", third, 200)
	end, _ := out["end_session_url"].(string)
	if !strings.HasPrefix(end, issuer+"/protocol/openid-connect/logout?") {
		t.Fatalf("end_session_url %q", end)
	}
	resp, err = nb.c.Get(end)
	if err != nil {
		t.Fatal(err)
	}
	if page := body(resp); resp.StatusCode != 200 && resp.StatusCode != 302 {
		t.Fatalf("Keycloak refused the logout URL: %d %.300s", resp.StatusCode, page)
	}
	e.api("GET", "/v1/auth/whoami", "", third, 401)

	// Account linking with a real IdP: Mallory's address is unverified in
	// Keycloak and an account with it already exists, so it is NOT linked.
	if _, err := store.GatewayCreateUser(context.Background(), e.pool, e.ns, "mallory@acme.test", "Existing", "reader", "a-long-password-1234"); err != nil {
		t.Fatal(err)
	}
	mb := newBrowser(t)
	if code := ssoError(mb.login(pid, "mallory", "mallory-e2e-password")); code != "account_not_linked" {
		t.Fatalf("unverified email linked: %q", code)
	}

	// CSRF: a callback delivered to a browser that did not start the login.
	attacker, victim := newBrowser(t), newBrowser(t)
	resp, err = attacker.c.Get(publicURL + "/v1/auth/sso/" + pid + "/start")
	if err != nil {
		t.Fatal(err)
	}
	m := formAction.FindStringSubmatch(body(resp))
	attacker.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = attacker.c.PostForm(html.UnescapeString(m[1]), url.Values{"username": {"bob"}, "password": {"bob-e2e-password"}, "credentialId": {""}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	callback := resp.Header.Get("Location")
	if !strings.HasPrefix(callback, publicURL+"/v1/auth/sso/oidc/") {
		t.Fatalf("expected Keycloak to redirect to the callback, got %q", callback)
	}
	resp, err = victim.c.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	vloc, _ := url.Parse(resp.Header.Get("Location"))
	if ssoError(vloc) != "browser_mismatch" {
		t.Fatalf("victim browser completed the attacker's login: %s", vloc)
	}
	// ...and the state is spent: replaying it in the attacker's browser fails.
	attacker.c.CheckRedirect = newBrowser(t).c.CheckRedirect
	resp, err = attacker.c.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if aloc, _ := url.Parse(resp.Header.Get("Location")); ssoError(aloc) != "state_invalid" {
		t.Fatalf("state replay: %s", aloc)
	}
}

// ── SAML ─────────────────────────────────────────────────────────────────────

func TestKeycloakSAML(t *testing.T) {
	e := setup(t)
	resp, err := http.Get(e.kc + "/realms/" + realm + "/protocol/saml/descriptor")
	if err != nil {
		t.Fatal(err)
	}
	md := body(resp)
	mdJSON, _ := json.Marshal(md)
	prov := e.api("POST", "/v1/namespaces/"+e.ns+"/sso/providers", `{"kind":"saml","slug":"keycloak-saml","display_name":"Keycloak SAML",
		"enabled":true,"idp_metadata_xml":`+string(mdJSON)+`,"allowed_domains":["acme.test"],"jit_provisioning":true}`, e.key, 201)
	pid := prov["id"].(string)
	spEntity, acs, slo := prov["sp_entity_id"].(string), prov["acs_url"].(string), prov["slo_url"].(string)
	e.api("PUT", "/v1/namespaces/"+e.ns+"/sso/providers/"+pid+"/mappings",
		`{"mappings":[{"idp_group":"staff","role_name":"Viewer"}],"revision":0}`, e.key, 200)

	// Register this SP in Keycloak, the way an administrator would from the
	// values the provider page shows: signed assertions, persistent NameID.
	e.kcAdmin("POST", "/clients", map[string]any{
		"clientId": spEntity, "protocol": "saml", "enabled": true, "redirectUris": []string{acs},
		"frontchannelLogout": true,
		"attributes": map[string]string{
			"saml.assertion.signature": "true", "saml.server.signature": "true", "saml.client.signature": "false",
			"saml.authnstatement": "true", "saml.force.post.binding": "true", "saml.signature.algorithm": "RSA_SHA256",
			"saml_force_name_id_format": "true", "saml_name_id_format": "persistent",
			"saml_assertion_consumer_url_post": acs, "saml_single_logout_service_url_redirect": slo,
		},
		"protocolMappers": []map[string]any{
			{"name": "email", "protocol": "saml", "protocolMapper": "saml-user-property-mapper",
				"config": map[string]string{"user.attribute": "email", "attribute.name": "email", "attribute.nameformat": "Basic"}},
			{"name": "groups", "protocol": "saml", "protocolMapper": "saml-group-membership-mapper",
				"config": map[string]string{"attribute.name": "groups", "full.path": "false", "single": "false", "attribute.nameformat": "Basic"}},
		},
	}, 201)
	t.Cleanup(func() {
		id := e.kcID("/clients?clientId="+url.QueryEscape(spEntity), "clientId", spEntity)
		e.kcAdmin("DELETE", "/clients/"+id, nil, 204)
	})

	b := newBrowser(t)
	res := b.exchange(b.login(pid, "bob", "bob-e2e-password"), 200)
	tok := res["token"].(string)
	who := e.api("GET", "/v1/auth/whoami", "", tok, 200)
	if who["name"] != "bob@acme.test" || fmt.Sprint(who["roles"]) != "[Viewer]" || who["namespace"] != e.ns {
		t.Fatalf("whoami %+v", who)
	}
	e.api("GET", "/v1/namespaces/"+e.ns+"/runs", "", tok, 200)

	// Replay: capture a real signed SAMLResponse, use it once, then again.
	rb := newBrowser(t)
	acsURL, form := rb.samlResponse(pid, "bob", "bob-e2e-password")
	post := func(br *browser) *url.URL {
		resp, err := br.c.PostForm(acsURL, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		return loc
	}
	rb.exchange(post(rb), 200)
	if code := ssoError(post(rb)); code != "state_invalid" {
		t.Fatalf("replayed SAMLResponse: %q", code)
	}
	// Tampered: flip one byte inside the signed assertion.
	tb := newBrowser(t)
	acsURL, form = tb.samlResponse(pid, "bob", "bob-e2e-password")
	raw := form.Get("SAMLResponse")
	decoded, _ := decodeB64(raw)
	tampered := strings.Replace(decoded, "bob@acme.test", "eve@acme.test", 1)
	if tampered == decoded {
		t.Fatal("could not find the email in the assertion to tamper with")
	}
	form.Set("SAMLResponse", encodeB64(tampered))
	if code := ssoError(post(tb)); code != "assertion_invalid" {
		t.Fatalf("tampered assertion: %q", code)
	}

	// SP-initiated logout goes to Keycloak's SLO endpoint.
	out := e.api("POST", "/v1/auth/logout", "", tok, 200)
	if end, _ := out["end_session_url"].(string); !strings.Contains(end, "SAMLRequest=") {
		t.Fatalf("SAML logout URL %+v", out)
	}
	e.api("GET", "/v1/auth/whoami", "", tok, 401)
}

var errNotB64 = errors.New("not base64")
