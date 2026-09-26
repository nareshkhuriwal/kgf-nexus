// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

package rest

// End-to-end SSO flows through the real handlers and a real PostgreSQL, against
// in-process identity providers that sign with real keys (sso/ssotest). The
// Keycloak suite (tests/sso) repeats the happy paths against a real IdP.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso/ssotest"
)

const ssoPublic = "http://localhost:3000"

type ssoEnv struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    http.Handler
	cfg  *sso.Config
	dir  string
}

func newSSOEnv(t *testing.T) *ssoEnv {
	t.Helper()
	pool := attentionPool(t)
	dir := t.TempDir()
	cfg := &sso.Config{PublicURL: ssoPublic, SecretDir: dir, AllowInsecureIssuers: true, AllowPrivateIssuers: true}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	auth := authn.New(pool).WithRBAC().WithIdentity(authn.StoreIdentity{DB: pool})
	h := New(pool, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(pool)).WithSSO(cfg).Routes()
	return &ssoEnv{t: t, pool: pool, h: h, cfg: cfg, dir: dir}
}

// tenant registers a namespace with an admin API key and returns both.
func (e *ssoEnv) tenant(prefix string) (string, string) {
	e.t.Helper()
	ns := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	testdb.RegisterTenant(e.t, e.pool, ns)
	key := "nx_sso_test_" + ns
	if _, err := store.GatewayCreateAPIKey(context.Background(), e.pool, ns, "sso-test-admin", "admin", key); err != nil {
		e.t.Fatal(err)
	}
	return ns, key
}

func (e *ssoEnv) secret(ns, name, value string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Join(e.dir, ns), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, ns, name), []byte(value), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// call performs an API request with a bearer credential.
func (e *ssoEnv) call(method, path, body, bearer string, want int) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if want != 0 && w.Code != want {
		e.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}

func decodeAs[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return v
}

// browser is one user agent: it keeps the gateway's cookies.
type browser struct {
	e       *ssoEnv
	cookies map[string]string
}

func (e *ssoEnv) browser() *browser { return &browser{e: e, cookies: map[string]string{}} }

func (b *browser) do(r *http.Request) *httptest.ResponseRecorder {
	for k, v := range b.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	b.e.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return w
}

func (b *browser) get(path string) *httptest.ResponseRecorder {
	return b.do(httptest.NewRequest(http.MethodGet, path, nil))
}

// local strips the public origin from a gateway redirect.
func local(t *testing.T, loc string) string {
	t.Helper()
	if !strings.HasPrefix(loc, ssoPublic) {
		t.Fatalf("redirect %q leaves the public origin", loc)
	}
	return strings.TrimPrefix(loc, ssoPublic)
}

// startOIDC starts a login and follows the IdP; it returns the gateway
// callback path the IdP redirected to.
func (b *browser) startOIDC(providerID, returnTo string) string {
	t := b.e.t
	t.Helper()
	w := b.get("/v1/auth/sso/" + providerID + "/start?return_to=" + url.QueryEscape(returnTo))
	if w.Code != http.StatusFound {
		t.Fatalf("start: %d %s (%s)", w.Code, w.Body.String(), w.Header().Get("Location"))
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return local(t, resp.Header.Get("Location"))
}

// finish follows a gateway result redirect: it returns the handoff code, or
// the sso_error code.
func finish(t *testing.T, w *httptest.ResponseRecorder) (handoff, ssoError string) {
	t.Helper()
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected a 303 to the console, got %d %s", w.Code, w.Body.String())
	}
	loc := local(t, w.Header().Get("Location"))
	if strings.HasPrefix(loc, "/sso/complete#handoff=") {
		return strings.TrimPrefix(loc, "/sso/complete#handoff="), ""
	}
	u, _ := url.Parse(loc)
	if u.Path != "/login" || u.Query().Get("sso_error") == "" {
		t.Fatalf("unexpected redirect %q", loc)
	}
	return "", u.Query().Get("sso_error")
}

type exchangeResult struct {
	Token    string `json:"token"`
	ReturnTo string `json:"return_to"`
	User     struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		Namespace string `json:"namespace"`
	} `json:"user"`
}

func (b *browser) exchange(handoff string, want int) exchangeResult {
	t := b.e.t
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/sso/exchange", strings.NewReader(`{"handoff":"`+handoff+`"}`))
	r.Header.Set("Content-Type", "application/json")
	w := b.do(r)
	if w.Code != want {
		t.Fatalf("exchange: got %d want %d: %s", w.Code, want, w.Body.String())
	}
	if want != http.StatusOK {
		return exchangeResult{}
	}
	return decodeAs[exchangeResult](t, w)
}

func (b *browser) loginOIDC(providerID string) exchangeResult {
	b.e.t.Helper()
	handoff, code := finish(b.e.t, b.get(b.startOIDC(providerID, "/runs")))
	if code != "" {
		b.e.t.Fatalf("sso_error=%s", code)
	}
	return b.exchange(handoff, http.StatusOK)
}

func (e *ssoEnv) oidcProvider(ns, key string, idp *ssotest.OIDCProvider, extra string) string {
	e.t.Helper()
	e.secret(ns, "client", idp.Secret)
	body := fmt.Sprintf(`{"kind":"oidc","slug":"idp","display_name":"Acme IdP","enabled":true,"issuer":%q,"client_id":%q,
		"client_secret_ref":"file:client","scopes":["email","profile"],"allowed_domains":["acme.test"],"groups_claim":"groups"%s}`,
		idp.Issuer(), idp.ClientID, extra)
	w := e.call("POST", "/v1/namespaces/"+ns+"/sso/providers", body, key, http.StatusCreated)
	return decodeAs[map[string]any](e.t, w)["id"].(string)
}

func (e *ssoEnv) auditActions(ns string) map[string]int {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT action || ':' || outcome FROM audit_log WHERE namespace=$1`, ns)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		out[a]++
	}
	return out
}

type whoami struct {
	Namespace     string   `json:"namespace"`
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
	Effective     []string `json:"effective_permissions"`
	HomeNamespace string   `json:"home_namespace"`
	SSOProvider   string   `json:"sso_provider_id"`
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestIntegrationSSOOIDCJITGroupSyncAndLogout(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("sso-oidc")
	idp := ssotest.NewOIDCProvider("client-"+ns, "secret-"+ns)
	defer idp.Close()
	pid := e.oidcProvider(ns, key, idp, `,"jit_provisioning":true,"revoke_sessions_on_role_loss":true`)
	e.call("PUT", "/v1/namespaces/"+ns+"/sso/providers/"+pid+"/mappings",
		`{"mappings":[{"idp_group":"nexus-operators","role_name":"Workflow Operator"}],"revision":0}`, key, http.StatusOK)

	// The public list shows only enabled providers, and an unknown tenant is
	// indistinguishable from one without SSO.
	list := decodeAs[map[string][]map[string]any](t, e.call("GET", "/v1/auth/sso/tenants/"+ns+"/providers", "", "", 200))
	if len(list["providers"]) != 1 || list["providers"][0]["display_name"] != "Acme IdP" {
		t.Fatalf("public providers %+v", list)
	}
	if l := decodeAs[map[string][]any](t, e.call("GET", "/v1/auth/sso/tenants/no-such-tenant/providers", "", "", 200)); len(l["providers"]) != 0 {
		t.Fatal("unknown tenant listed providers")
	}

	b := e.browser()
	first := b.loginOIDC(pid)
	if first.User.Email != "alice@acme.test" || first.User.Namespace != ns || first.ReturnTo != "/runs" {
		t.Fatalf("exchange %+v", first)
	}
	who := decodeAs[whoami](t, e.call("GET", "/v1/auth/whoami", "", first.Token, 200))
	if len(who.Roles) != 1 || who.Roles[0] != "Workflow Operator" || who.SSOProvider != pid {
		t.Fatalf("group mapping not applied: %+v", who)
	}
	e.call("GET", "/v1/namespaces/"+ns+"/runs", "", first.Token, 200)
	// JIT users never hold identity administration by default.
	e.call("GET", "/v1/namespaces/"+ns+"/users", "", first.Token, 403)

	// Remove the group at the IdP: the role is revoked at the next login and,
	// because the provider asks for it, the earlier session is ended.
	base := idp.Claims
	idp.Claims = func(n string) map[string]any { c := base(n); c["groups"] = []string{"unrelated"}; return c }
	second := b.loginOIDC(pid)
	e.call("GET", "/v1/auth/whoami", "", first.Token, 401)
	who = decodeAs[whoami](t, e.call("GET", "/v1/auth/whoami", "", second.Token, 200))
	if len(who.Roles) != 0 || len(who.Effective) != 0 {
		t.Fatalf("role not revoked after group removal: %+v", who)
	}
	e.call("GET", "/v1/namespaces/"+ns+"/runs", "", second.Token, 403)

	out := decodeAs[map[string]any](t, e.call("POST", "/v1/auth/logout", "", second.Token, 200))
	end, _ := out["end_session_url"].(string)
	if !strings.HasPrefix(end, idp.Issuer()+"/logout?") || !strings.Contains(end, "client_id=") {
		t.Fatalf("RP-initiated logout URL missing: %+v", out)
	}
	e.call("GET", "/v1/auth/whoami", "", second.Token, 401)

	audit := e.auditActions(ns)
	for _, a := range []string{"user.sso_provisioned:success", "user.roles.idp_sync:success", "session.login:success", "sso.provider.create:success", "sso.mappings.update:success"} {
		if audit[a] == 0 {
			t.Errorf("audit trail lacks %s: %v", a, audit)
		}
	}
	if audit["user.roles.idp_sync:success"] != 2 {
		t.Errorf("expected a sync row for the grant and for the revocation: %v", audit)
	}
}

func TestIntegrationSSOCallbackCSRFReplayAndRedirects(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("sso-csrf")
	idp := ssotest.NewOIDCProvider("client-"+ns, "secret-"+ns)
	defer idp.Close()
	pid := e.oidcProvider(ns, key, idp, `,"jit_provisioning":true`)

	// CSRF on the callback: an attacker's completed flow delivered to a victim
	// browser that never started it.
	attacker, victim := e.browser(), e.browser()
	cb := attacker.startOIDC(pid, "/runs")
	if _, code := finish(t, victim.get(cb)); code != "browser_mismatch" {
		t.Fatalf("callback in another browser: %q", code)
	}
	// The state was consumed by that attempt: even the right browser cannot
	// reuse it (replay).
	if _, code := finish(t, attacker.get(cb)); code != "state_invalid" {
		t.Fatalf("state replay: %q", code)
	}
	// Missing or forged state.
	if _, code := finish(t, attacker.get("/v1/auth/sso/oidc/"+pid+"/callback?code=x")); code != "state_invalid" {
		t.Fatalf("callback without state: %q", code)
	}
	if _, code := finish(t, attacker.get("/v1/auth/sso/oidc/"+pid+"/callback?code=x&state=forged")); code != "state_invalid" {
		t.Fatalf("forged state: %q", code)
	}

	// Login CSRF via the handoff: a handoff minted for the attacker's browser
	// cannot be exchanged by another browser, and the failed attempt does not
	// consume it.
	cb = attacker.startOIDC(pid, "/runs")
	handoff, code := finish(t, attacker.get(cb))
	if code != "" {
		t.Fatal(code)
	}
	victim.cookies = map[string]string{"nexus_sso": ssotest.RandomString()}
	victim.exchange(handoff, http.StatusUnauthorized)
	e.browser().exchange(handoff, http.StatusUnauthorized) // no cookie at all
	attacker.exchange(handoff, http.StatusOK)
	attacker.exchange(handoff, http.StatusUnauthorized) // single use

	// Open redirects: return_to never carries an absolute or scheme-relative URL.
	for _, rt := range []string{"//evil.test/x", "https://evil.test", "/\\evil.test", "/%2F%2Fevil.test"} {
		b := e.browser()
		h, c := finish(t, b.get(b.startOIDC(pid, rt)))
		if c != "" {
			t.Fatal(c)
		}
		if got := b.exchange(h, 200).ReturnTo; got != sso.DefaultReturnTo {
			t.Fatalf("return_to %q came back as %q", rt, got)
		}
	}
	b := e.browser()
	h, _ := finish(t, b.get(b.startOIDC(pid, "/runs/abc?tab=logs")))
	if got := b.exchange(h, 200).ReturnTo; got != "/runs/abc?tab=logs" {
		t.Fatalf("safe return_to lost: %q", got)
	}

	// Every gateway redirect stays on the public origin; the start endpoint for
	// an unknown or disabled provider never reaches an IdP.
	w := e.browser().get("/v1/auth/sso/00000000-0000-0000-0000-000000000000/start")
	if _, code := finish(t, w); code != "provider_unavailable" {
		t.Fatalf("unknown provider: %q", code)
	}
	if e.auditActions(ns)["session.sso_login:denied"] < 3 {
		t.Error("rejected callbacks were not audited")
	}
}

func TestIntegrationSSOTokenTamperingThroughTheCallback(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("sso-tamper")
	idp := ssotest.NewOIDCProvider("client-"+ns, "secret-"+ns)
	defer idp.Close()
	pid := e.oidcProvider(ns, key, idp, `,"jit_provisioning":true`)
	base := idp.Claims
	for name, claims := range map[string]func(string) map[string]any{
		"wrong audience": func(n string) map[string]any { c := base(n); c["aud"] = "other"; delete(c, "azp"); return c },
		"expired":        func(n string) map[string]any { c := base(n); c["exp"] = time.Now().Add(-time.Hour).Unix(); return c },
		"bad nonce":      func(string) map[string]any { return base("another-nonce") },
	} {
		idp.Claims = claims
		b := e.browser()
		if _, code := finish(t, b.get(b.startOIDC(pid, "/runs"))); code != "token_invalid" {
			t.Errorf("%s: got %q, want token_invalid", name, code)
		}
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE namespace=$1`, ns).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a rejected token provisioned an account (%d, %v)", n, err)
	}
}

func TestIntegrationSSOAccountLinking(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	ns, key := e.tenant("sso-link")
	idp := ssotest.NewOIDCProvider("client-"+ns, "secret-"+ns)
	defer idp.Close()
	pid := e.oidcProvider(ns, key, idp, `,"jit_provisioning":false,"link_by_verified_email":false`)
	prov := decodeAs[map[string]any](t, e.call("GET", "/v1/namespaces/"+ns+"/sso/providers", "", key, 200))["providers"].([]any)[0].(map[string]any)
	rev := int64(prov["revision"].(float64))
	update := func(jit, link bool) {
		body := fmt.Sprintf(`{"slug":"idp","display_name":"Acme IdP","enabled":true,"issuer":%q,"client_id":%q,"client_secret_ref":"file:client",
			"scopes":["email"],"allowed_domains":["acme.test"],"jit_provisioning":%t,"link_by_verified_email":%t,"revision":%d}`,
			idp.Issuer(), idp.ClientID, jit, link, rev)
		rev = int64(decodeAs[map[string]any](t, e.call("PUT", "/v1/namespaces/"+ns+"/sso/providers/"+pid, body, key, 200))["revision"].(float64))
	}
	// An existing password account, which happens to be an administrator.
	existing, err := store.GatewayCreateUser(ctx, e.pool, ns, "bob@acme.test", "Bob", "admin", "a-long-password-1234")
	if err != nil {
		t.Fatal(err)
	}
	base := idp.Claims
	claims := func(email string, verified bool, sub string) {
		idp.Claims = func(n string) map[string]any {
			c := base(n)
			c["email"], c["email_verified"], c["sub"] = email, verified, sub
			return c
		}
	}
	attempt := func() (string, string) {
		b := e.browser()
		h, code := finish(t, b.get(b.startOIDC(pid, "/runs")))
		if code != "" {
			return "", code
		}
		return b.exchange(h, 200).User.ID, ""
	}

	// JIT off and no account: refused.
	claims("nobody@acme.test", true, "sub-nobody")
	if _, code := attempt(); code != "no_account" {
		t.Fatalf("no account: %q", code)
	}
	// Linking disabled: an IdP asserting an existing address (even verified)
	// does NOT get that account.
	claims("bob@acme.test", true, "attacker-sub")
	if _, code := attempt(); code != "account_not_linked" {
		t.Fatalf("link disabled: %q", code)
	}
	// Linking enabled but the IdP does not assert email_verified: refused.
	update(false, true)
	claims("bob@acme.test", false, "attacker-sub")
	if _, code := attempt(); code != "account_not_linked" {
		t.Fatalf("unverified email linked: %q", code)
	}
	// A different domain is refused before any lookup.
	claims("bob@evil.test", true, "evil-sub")
	if _, code := attempt(); code != "domain_not_allowed" {
		t.Fatalf("foreign domain: %q", code)
	}
	// Verified AND allowed: linked, audited, and stable on the next login.
	claims("bob@acme.test", true, "bob-sub")
	id, code := attempt()
	if code != "" || id != existing.ID {
		t.Fatalf("verified link: id=%q code=%q", id, code)
	}
	if id2, _ := attempt(); id2 != existing.ID {
		t.Fatal("second login did not resolve the linked identity")
	}
	if e.auditActions(ns)["user.sso_linked:success"] != 1 {
		t.Fatal("link not audited exactly once")
	}
	// The identity is keyed on the subject: a different subject with the same
	// verified email cannot take over the now-linked account.
	claims("bob@acme.test", true, "second-sub")
	if _, code := attempt(); code != "account_not_linked" {
		t.Fatalf("a second subject with the same email took over the linked account: %q", code)
	}
	// A disabled account cannot sign in by SSO.
	if _, err := store.GatewaySetUserDisabled(ctx, e.pool, ns, existing.ID, true); err != nil && err != store.ErrLastAdministrator {
		t.Fatal(err)
	}
	other, _ := store.GatewayCreateUser(ctx, e.pool, ns, "carl@acme.test", "Carl", "reader", "a-long-password-1234")
	update(true, true)
	claims("carl@acme.test", true, "carl-sub")
	if id, code := attempt(); code != "" || id != other.ID {
		t.Fatalf("carl: %q %q", id, code)
	}
	if _, err := store.GatewaySetUserDisabled(ctx, e.pool, ns, other.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, code := attempt(); code != "account_disabled" {
		t.Fatalf("disabled account signed in: %q", code)
	}
	// Unlinking through the admin API removes the identity.
	ids := decodeAs[map[string][]map[string]any](t, e.call("GET", "/v1/namespaces/"+ns+"/users/"+other.ID+"/identities", "", key, 200))["identities"]
	if len(ids) != 1 {
		t.Fatalf("identities %+v", ids)
	}
	e.call("DELETE", "/v1/namespaces/"+ns+"/users/"+other.ID+"/identities/"+ids[0]["id"].(string), "", key, 204)
}

func TestIntegrationSSOCrossTenantMisuse(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	nsA, keyA := e.tenant("sso-a")
	nsB, keyB := e.tenant("sso-b")
	idpA := ssotest.NewOIDCProvider("client-"+nsA, "secret-a")
	defer idpA.Close()
	idpB := ssotest.NewOIDCProvider("client-"+nsB, "secret-b")
	defer idpB.Close()
	// Tenant A's IdP is (mis)configured to allow B's domain and JIT.
	e.secret(nsA, "client", idpA.Secret)
	body := fmt.Sprintf(`{"kind":"oidc","slug":"a","display_name":"A","enabled":true,"issuer":%q,"client_id":%q,"client_secret_ref":"file:client",
		"allowed_domains":["b.test"],"jit_provisioning":true,"link_by_verified_email":true}`, idpA.Issuer(), idpA.ClientID)
	pidA := decodeAs[map[string]any](t, e.call("POST", "/v1/namespaces/"+nsA+"/sso/providers", body, keyA, 201))["id"].(string)
	pidB := e.oidcProvider(nsB, keyB, idpB, `,"jit_provisioning":true`)

	// B's administrator.
	carol, err := store.GatewayCreateUser(ctx, e.pool, nsB, "carol@b.test", "Carol", "admin", "a-long-password-1234")
	if err != nil {
		t.Fatal(err)
	}
	// IdP A asserts B's administrator, verified.
	base := idpA.Claims
	idpA.Claims = func(n string) map[string]any {
		c := base(n)
		c["email"], c["sub"] = "carol@b.test", "carol-at-a"
		return c
	}
	b := e.browser()
	res := b.loginOIDC(pidA)
	if res.User.Namespace != nsA || res.User.ID == carol.ID {
		t.Fatalf("tenant A's IdP produced a tenant B identity: %+v", res)
	}
	e.call("GET", "/v1/namespaces/"+nsB+"/runs", "", res.Token, 403)
	e.call("GET", "/v1/namespaces/"+nsB+"/users", "", res.Token, 403)
	var bIdentities int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM user_identities WHERE user_id=$1`, carol.ID).Scan(&bIdentities)
	if bIdentities != 0 {
		t.Fatal("B's administrator was linked to A's IdP")
	}

	// A state started for A's provider cannot be completed at B's callback.
	cb := b.startOIDC(pidA, "/runs")
	u, _ := url.Parse(cb)
	if _, code := finish(t, b.get("/v1/auth/sso/oidc/"+pidB+"/callback?"+u.RawQuery)); code != "state_invalid" {
		t.Fatalf("A's response accepted at B's callback: %q", code)
	}
	// A handoff is bound to its provider's user; B's admin API cannot see A's
	// providers, and the same IdP application cannot be registered twice.
	e.call("PUT", "/v1/namespaces/"+nsA+"/sso/providers/"+pidB, `{"slug":"x","display_name":"x","revision":1}`, keyA, 404)
	dup := fmt.Sprintf(`{"kind":"oidc","slug":"dup","display_name":"dup","issuer":%q,"client_id":%q,"client_secret_ref":"file:client"}`, idpA.Issuer(), idpA.ClientID)
	e.call("POST", "/v1/namespaces/"+nsB+"/sso/providers", dup, keyB, 409)
	// Tenant B cannot reference tenant A's secret file.
	e.secret(nsA, "only-a", "x")
	sneak := fmt.Sprintf(`{"kind":"oidc","slug":"sneak","display_name":"s","enabled":true,"issuer":%q,"client_id":"sneaky","client_secret_ref":"file:only-a"}`, idpB.Issuer())
	w := e.call("POST", "/v1/namespaces/"+nsB+"/sso/providers", sneak, keyB, 422)
	if !strings.Contains(w.Body.String(), "does not resolve") {
		t.Fatalf("cross-tenant secret reference: %s", w.Body.String())
	}
	sneak = strings.Replace(sneak, "file:only-a", "file:../"+nsA+"/only-a", 1)
	e.call("POST", "/v1/namespaces/"+nsB+"/sso/providers", sneak, keyB, 422)
}

func TestIntegrationSSOSAMLFlowAndReplay(t *testing.T) {
	e := newSSOEnv(t)
	ctx := context.Background()
	ns, key := e.tenant("sso-saml")
	idp := ssotest.NewSAMLIdP("https://idp-" + ns + ".test")
	md, _ := json.Marshal(idp.Metadata())
	body := `{"kind":"saml","slug":"saml","display_name":"Acme SAML","enabled":true,"idp_metadata_xml":` + string(md) +
		`,"allowed_domains":["acme.test"],"jit_provisioning":true}`
	prov := decodeAs[map[string]any](t, e.call("POST", "/v1/namespaces/"+ns+"/sso/providers", body, key, 201))
	pid := prov["id"].(string)
	if prov["acs_url"] != e.cfg.SAMLACSURL(pid) {
		t.Fatalf("acs url %v", prov["acs_url"])
	}
	e.call("PUT", "/v1/namespaces/"+ns+"/sso/providers/"+pid+"/mappings",
		`{"mappings":[{"idp_group":"staff","role_name":"Viewer"}],"revision":0}`, key, 200)
	mdResp := e.call("GET", "/v1/auth/sso/saml/"+pid+"/metadata", "", "", 200)
	if !strings.Contains(mdResp.Body.String(), e.cfg.SAMLACSURL(pid)) {
		t.Fatal("SP metadata lacks the ACS URL")
	}

	start := func(b *browser) (relay, reqID string) {
		w := b.get("/v1/auth/sso/" + pid + "/start?return_to=/workflows")
		loc, _ := url.Parse(w.Header().Get("Location"))
		if !strings.HasPrefix(w.Header().Get("Location"), idp.SSOURL) || loc.Query().Get("SAMLRequest") == "" {
			t.Fatalf("start: %d %s", w.Code, w.Header().Get("Location"))
		}
		relay = loc.Query().Get("RelayState")
		if err := e.pool.QueryRow(ctx, `SELECT saml_request_id FROM sso_login_states WHERE state_hash=$1`, sha256Hex(relay)).Scan(&reqID); err != nil {
			t.Fatal(err)
		}
		return relay, reqID
	}
	post := func(b *browser, providerID, relay, resp string) *httptest.ResponseRecorder {
		form := url.Values{"SAMLResponse": {resp}, "RelayState": {relay}}
		r := httptest.NewRequest(http.MethodPost, "/v1/auth/sso/saml/"+providerID+"/acs", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return b.do(r)
	}
	opts := func(reqID, assertionID string) ssotest.ResponseOpts {
		return ssotest.ResponseOpts{ACS: e.cfg.SAMLACSURL(pid), SPEntityID: e.cfg.SAMLEntityID(pid), RequestID: reqID,
			NameID: "dana-persistent", Email: "dana@acme.test", Groups: []string{"staff"}, AssertionID: assertionID}
	}

	b := e.browser()
	relay, reqID := start(b)
	samlResp := idp.Response(opts(reqID, "_assertion-one"))
	h, code := finish(t, post(b, pid, relay, samlResp))
	if code != "" {
		t.Fatal(code)
	}
	res := b.exchange(h, 200)
	who := decodeAs[whoami](t, e.call("GET", "/v1/auth/whoami", "", res.Token, 200))
	if who.Namespace != ns || len(who.Roles) != 1 || who.Roles[0] != "Viewer" || res.ReturnTo != "/workflows" {
		t.Fatalf("saml login %+v %+v", who, res)
	}

	// Replay: the same POST again (state already consumed).
	if _, code := finish(t, post(b, pid, relay, samlResp)); code != "state_invalid" {
		t.Fatalf("replayed POST: %q", code)
	}
	// Replay of the same assertion ID under a fresh login.
	relay2, reqID2 := start(b)
	if _, code := finish(t, post(b, pid, relay2, idp.Response(opts(reqID2, "_assertion-one")))); code != "assertion_invalid" {
		t.Fatalf("assertion ID replay: %q", code)
	}
	// An assertion answering a DIFFERENT request (InResponseTo) is refused.
	relay3, _ := start(b)
	if _, code := finish(t, post(b, pid, relay3, idp.Response(opts(reqID, "_assertion-three")))); code != "assertion_invalid" {
		t.Fatalf("InResponseTo mismatch: %q", code)
	}
	// IdP-initiated (no RelayState): refused.
	if _, code := finish(t, post(b, pid, "", idp.Response(opts("", "_idp-init")))); code != "state_invalid" {
		t.Fatalf("IdP-initiated: %q", code)
	}
	// Tampered assertion.
	relay4, reqID4 := start(b)
	tampered := opts(reqID4, "_assertion-four")
	tampered.SignWithOtherKey = true
	if _, code := finish(t, post(b, pid, relay4, idp.Response(tampered))); code != "assertion_invalid" {
		t.Fatalf("tampered assertion: %q", code)
	}
	// A valid assertion for THIS provider posted to another tenant's ACS.
	ns2, key2 := e.tenant("sso-saml-b")
	idp2 := ssotest.NewSAMLIdP("https://idp-" + ns2 + ".test")
	md2, _ := json.Marshal(idp2.Metadata())
	pid2 := decodeAs[map[string]any](t, e.call("POST", "/v1/namespaces/"+ns2+"/sso/providers",
		`{"kind":"saml","slug":"s","display_name":"S","enabled":true,"idp_metadata_xml":`+string(md2)+`}`, key2, 201))["id"].(string)
	relay5, reqID5 := start(b)
	if _, code := finish(t, post(b, pid2, relay5, idp.Response(opts(reqID5, "_assertion-five")))); code != "state_invalid" {
		t.Fatalf("cross-tenant ACS: %q", code)
	}
	// The same IdP entity cannot be registered by a second tenant.
	e.call("POST", "/v1/namespaces/"+ns2+"/sso/providers", `{"kind":"saml","slug":"dup","display_name":"D","idp_metadata_xml":`+string(md)+`}`, key2, 409)
}

func TestIntegrationSSOAdminValidation(t *testing.T) {
	e := newSSOEnv(t)
	ns, key := e.tenant("sso-admin")
	w := e.call("POST", "/v1/namespaces/"+ns+"/sso/providers",
		`{"kind":"oidc","slug":"BAD SLUG","display_name":"","issuer":"http://169.254.169.254","client_id":"","client_secret_ref":"env:NEXUS_DATABASE_URL","jit_provisioning":true}`, key, 422)
	var body struct {
		Problems []string `json:"problems"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Problems) < 4 {
		t.Fatalf("expected every problem reported at once: %v", body.Problems)
	}
	// Enabling checks the secret resolves and discovery works.
	idp := ssotest.NewOIDCProvider("c-"+ns, "distinct-secret-value-"+ns)
	defer idp.Close()
	e.call("POST", "/v1/namespaces/"+ns+"/sso/providers", fmt.Sprintf(`{"kind":"oidc","slug":"x","display_name":"X","enabled":true,"issuer":%q,"client_id":"c-%s","client_secret_ref":"file:missing"}`, idp.Issuer(), ns), key, 422)
	// Administrator can never be mapped from an IdP group.
	pid := e.oidcProvider(ns, key, idp, "")
	e.call("PUT", "/v1/namespaces/"+ns+"/sso/providers/"+pid+"/mappings", `{"mappings":[{"idp_group":"g","role_name":"Administrator"}],"revision":0}`, key, 422)
	e.call("PUT", "/v1/namespaces/"+ns+"/sso/providers/"+pid+"/mappings", `{"mappings":[{"idp_group":"g","role_name":"No such role"}],"revision":0}`, key, 400)
	e.call("PUT", "/v1/namespaces/"+ns+"/sso/providers/"+pid+"/mappings", `{"mappings":[],"revision":7}`, key, 409)
	// The listing never contains a secret, only its reference.
	lst := e.call("GET", "/v1/namespaces/"+ns+"/sso/providers", "", key, 200).Body.String()
	if strings.Contains(lst, idp.Secret) || !strings.Contains(lst, `"client_secret_resolves":true`) {
		t.Fatalf("provider listing: %s", lst)
	}
	e.call("DELETE", "/v1/namespaces/"+ns+"/sso/providers/"+pid+"?revision=1", "", key, 204)
}
