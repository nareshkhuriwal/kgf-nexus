// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package rest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/sso"
)

// Tenant single sign-on: the browser flow (ADR-0024 §3). The admin API is in
// sso_admin.go.
//
// Every route here is unguarded (the caller has no credential yet), rate
// limited per address, and ends in one of two places: the IdP, or a FIXED
// path under NEXUS_PUBLIC_URL. The gateway never redirects to a URL a request
// supplied.

const (
	ssoStateTTL   = 10 * time.Minute
	ssoHandoffTTL = 60 * time.Second
	ssoCookieAge  = 600
)

type ssoService struct {
	cfg  *sso.Config
	oidc *sso.OIDC
	saml *sso.SAML
}

// WithSSO enables tenant single sign-on. A nil or disabled config leaves the
// routes answering 503 sso_unavailable, so a console can say so honestly.
func (a *API) WithSSO(cfg *sso.Config) *API {
	if cfg == nil {
		return a
	}
	a.sso = &ssoService{cfg: cfg, oidc: sso.NewOIDC(cfg), saml: sso.NewSAML(cfg)}
	return a
}

func (a *API) registerSSORoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/auth/sso/tenants/{ns}/providers", a.ssoPublicProviders)
	mux.HandleFunc("GET /v1/auth/sso/{id}/start", a.ssoStart)
	mux.HandleFunc("GET /v1/auth/sso/oidc/{id}/callback", a.ssoOIDCCallback)
	mux.HandleFunc("POST /v1/auth/sso/saml/{id}/acs", a.ssoSAMLACS)
	mux.HandleFunc("GET /v1/auth/sso/saml/{id}/metadata", a.ssoSAMLMetadata)
	mux.HandleFunc("GET /v1/auth/sso/saml/{id}/slo", a.ssoSAMLSLO)
	mux.HandleFunc("POST /v1/auth/sso/saml/{id}/slo", a.ssoSAMLSLO)
	mux.HandleFunc("POST /v1/auth/sso/exchange", a.ssoExchange)

	mux.Handle("GET /v1/namespaces/{ns}/sso/providers", a.guard(authn.ActionRead, a.ssoListProviders))
	mux.Handle("POST /v1/namespaces/{ns}/sso/providers", a.guard(authn.ActionRead, a.ssoCreateProvider))
	mux.Handle("PUT /v1/namespaces/{ns}/sso/providers/{id}", a.guard(authn.ActionRead, a.ssoUpdateProvider))
	mux.Handle("DELETE /v1/namespaces/{ns}/sso/providers/{id}", a.guard(authn.ActionRead, a.ssoDeleteProvider))
	mux.Handle("GET /v1/namespaces/{ns}/sso/providers/{id}/mappings", a.guard(authn.ActionRead, a.ssoGetMappings))
	mux.Handle("PUT /v1/namespaces/{ns}/sso/providers/{id}/mappings", a.guard(authn.ActionRead, a.ssoPutMappings))
	mux.Handle("GET /v1/namespaces/{ns}/users/{id}/identities", a.guard(authn.ActionRead, a.listUserIdentities))
	mux.Handle("DELETE /v1/namespaces/{ns}/users/{id}/identities/{identity}", a.guard(authn.ActionRead, a.unlinkUserIdentity))
}

func (a *API) ssoReady(w http.ResponseWriter) bool {
	if a.sso == nil || !a.sso.cfg.Enabled() || a.db == nil {
		writeError(w, http.StatusServiceUnavailable, "sso_unavailable",
			"single sign-on is not configured on this gateway (NEXUS_PUBLIC_URL is unset)")
		return false
	}
	return true
}

// ── small crypto helpers ─────────────────────────────────────────────────────

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (a *API) ssoCookieName() string {
	if a.sso.cfg.SecureCookies() {
		// __Host-: Secure, no Domain, Path=/. A sibling or parent domain
		// cannot plant one (cookie tossing), which is what would otherwise let
		// an attacker pre-set a victim's binding to a value they know and bind
		// their own handoff to it.
		return "__Host-nexus_sso"
	}
	return "nexus_sso"
}

// bindingCookie returns the browser-binding value from the request, if any
// well-formed one is present.
func (a *API) bindingCookie(r *http.Request) string {
	c, err := r.Cookie(a.ssoCookieName())
	if err != nil || len(c.Value) != 43 {
		return ""
	}
	if _, err := base64.RawURLEncoding.DecodeString(c.Value); err != nil {
		return ""
	}
	return c.Value
}

func (a *API) setBindingCookie(w http.ResponseWriter, value string, maxAge int) {
	path := "/v1/auth/sso"
	if a.sso.cfg.SecureCookies() {
		path = "/" // required by the __Host- prefix
	}
	http.SetCookie(w, &http.Cookie{
		Name: a.ssoCookieName(), Value: value, Path: path, MaxAge: maxAge,
		HttpOnly: true, Secure: a.sso.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// ssoFail sends the browser back to the login page with an opaque code. The
// reason is in the audit log, never in the URL.
func (a *API) ssoFail(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, a.sso.cfg.LoginURL()+"?sso_error="+url.QueryEscape(code), http.StatusSeeOther)
}

func (a *API) ssoAuditFailure(r *http.Request, p store.SSOProviderRow, reason string, err error, detail map[string]any) {
	d := map[string]any{"provider_id": p.ID, "provider": p.Slug, "reason": reason}
	if err != nil {
		d["error"] = truncate(err.Error(), 500)
	}
	for k, v := range detail {
		d[k] = v
	}
	label := ""
	if e, ok := detail["email"].(string); ok {
		label = e
	}
	a.auditAttempt(r, store.AuditEntry{Namespace: p.Namespace, ActorKind: "anonymous", ActorLabel: label,
		Action: "session.sso_login", TargetKind: "sso_provider", TargetID: p.ID, Outcome: store.AuditDenied, Detail: d})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ── public provider list ─────────────────────────────────────────────────────

// ssoPublicProviders lists a tenant's ENABLED providers for the login page.
// An unknown tenant gets the same empty list as a tenant without SSO, so the
// endpoint does not confirm which tenants exist.
func (a *API) ssoPublicProviders(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.ssoReady(w) {
		return
	}
	rows, err := store.GatewayListSSOProviders(r.Context(), a.db, r.PathValue("ns"))
	if err != nil {
		a.fail(w, r, "list sso providers", err)
		return
	}
	out := []map[string]any{}
	for _, p := range rows {
		if !p.Enabled {
			continue
		}
		out = append(out, map[string]any{
			"id": p.ID, "kind": p.Kind, "display_name": p.DisplayName,
			"start_url": "/v1/auth/sso/" + p.ID + "/start",
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

// ── start ────────────────────────────────────────────────────────────────────

func (a *API) ssoStart(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.ssoReady(w) {
		return
	}
	ctx := r.Context()
	p, err := store.GatewayGetSSOProviderByID(ctx, a.db, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && !p.Enabled) {
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	if err != nil {
		a.log.ErrorContext(ctx, "sso start: provider lookup failed", "error", err)
		a.ssoFail(w, r, "temporarily_unavailable")
		return
	}
	returnTo := sso.SafeReturnTo(r.URL.Query().Get("return_to"))
	state, err1 := randomToken()
	nonce, err2 := randomToken()
	binding := a.bindingCookie(r)
	var err3 error
	if binding == "" {
		binding, err3 = randomToken()
	}
	if err1 != nil || err2 != nil || err3 != nil {
		a.ssoFail(w, r, "temporarily_unavailable")
		return
	}
	st := store.SSOLoginState{StateHash: sha256Hex(state), ProviderID: p.ID, Namespace: p.Namespace,
		ReturnTo: returnTo, BindingHash: sha256Hex(binding)}
	var target string
	switch p.Kind {
	case "oidc":
		st.Nonce = nonce
		st.CodeVerifier = oauth2.GenerateVerifier()
		target, err = a.sso.oidc.AuthURL(ctx, p, state, nonce, st.CodeVerifier)
	case "saml":
		target, st.SAMLRequestID, err = a.sso.saml.AuthnRequestURL(p, state)
	default:
		err = errors.New("unknown provider kind")
	}
	if err != nil {
		a.ssoAuditFailure(r, p, "provider could not start a login", err, nil)
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	if err := store.GatewayCreateLoginState(ctx, a.db, st, ssoStateTTL); err != nil {
		a.log.ErrorContext(ctx, "sso start: could not store state", "error", err)
		a.ssoFail(w, r, "temporarily_unavailable")
		return
	}
	a.setBindingCookie(w, binding, ssoCookieAge)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target, http.StatusFound)
}

// ── OIDC callback ────────────────────────────────────────────────────────────

func (a *API) ssoOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.ssoReady(w) {
		return
	}
	ctx := r.Context()
	p, err := store.GatewayGetSSOProviderByID(ctx, a.db, r.PathValue("id"))
	if err != nil || p.Kind != "oidc" {
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	q := r.URL.Query()
	stateParam := q.Get("state")
	if stateParam == "" {
		a.ssoAuditFailure(r, p, "callback without state", nil, nil)
		a.ssoFail(w, r, "state_invalid")
		return
	}
	// Consumed FIRST and exactly once, whatever happens next: a state that
	// reached a callback is spent, success or not.
	st, err := store.GatewayConsumeLoginState(ctx, a.db, sha256Hex(stateParam), p.ID)
	if err != nil {
		a.ssoAuditFailure(r, p, "unknown, expired or reused state", err, nil)
		a.ssoFail(w, r, "state_invalid")
		return
	}
	// CSRF / login-CSRF: the browser completing the flow must be the one that
	// started it. A top-level GET from the IdP carries the Lax cookie.
	if b := a.bindingCookie(r); b == "" || subtle.ConstantTimeCompare([]byte(sha256Hex(b)), []byte(st.BindingHash)) != 1 {
		a.ssoAuditFailure(r, p, "callback from a browser that did not start the login", nil, nil)
		a.ssoFail(w, r, "browser_mismatch")
		return
	}
	if e := q.Get("error"); e != "" {
		a.ssoAuditFailure(r, p, "identity provider returned an error", nil, map[string]any{"idp_error": truncate(e, 100)})
		a.ssoFail(w, r, "idp_error")
		return
	}
	if !p.Enabled {
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	secret, err := a.sso.cfg.ResolveSecret(p.Namespace, p.ClientSecretRef)
	if err != nil {
		a.ssoAuditFailure(r, p, "client secret reference does not resolve", err, nil)
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	id, err := a.sso.oidc.Exchange(ctx, p, secret, sso.CallbackParams{Code: q.Get("code"), Iss: q.Get("iss")}, st.Nonce, st.CodeVerifier)
	if err != nil {
		a.ssoAuditFailure(r, p, "id token or code exchange rejected", err, nil)
		a.ssoFail(w, r, "token_invalid")
		return
	}
	a.ssoComplete(w, r, p, id, st, "", "")
}

// ── SAML ACS ─────────────────────────────────────────────────────────────────

func (a *API) ssoSAMLACS(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.ssoReady(w) {
		return
	}
	ctx := r.Context()
	p, err := store.GatewayGetSSOProviderByID(ctx, a.db, r.PathValue("id"))
	if err != nil || p.Kind != "saml" {
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sso.MaxSAMLResponse+16<<10)
	if err := r.ParseForm(); err != nil {
		a.ssoAuditFailure(r, p, "unreadable SAML POST", err, nil)
		a.ssoFail(w, r, "assertion_invalid")
		return
	}
	relay := r.PostForm.Get("RelayState")
	if relay == "" {
		// IdP-initiated SSO is refused: nothing to bind InResponseTo to.
		a.ssoAuditFailure(r, p, "unsolicited (IdP-initiated) response refused", nil, nil)
		a.ssoFail(w, r, "state_invalid")
		return
	}
	st, err := store.GatewayConsumeLoginState(ctx, a.db, sha256Hex(relay), p.ID)
	if err != nil {
		a.ssoAuditFailure(r, p, "unknown, expired or reused state", err, nil)
		a.ssoFail(w, r, "state_invalid")
		return
	}
	if !p.Enabled {
		a.ssoFail(w, r, "provider_unavailable")
		return
	}
	res, err := a.sso.saml.ParseResponse(p, r.PostForm.Get("SAMLResponse"), st.SAMLRequestID)
	if err != nil {
		a.ssoAuditFailure(r, p, "SAML response rejected", err, nil)
		a.ssoFail(w, r, "assertion_invalid")
		return
	}
	if err := store.GatewayRecordAssertion(ctx, a.db, p.ID, res.AssertionID, res.NotOnOrAfter); err != nil {
		a.ssoAuditFailure(r, p, "SAML assertion replayed", err, map[string]any{"assertion_id": truncate(res.AssertionID, 100)})
		a.ssoFail(w, r, "assertion_invalid")
		return
	}
	// No binding-cookie check HERE: the IdP's POST is cross-site and carries no
	// Lax cookie. The handoff exchange enforces it instead (ADR-0024 §3).
	a.ssoComplete(w, r, p, res.Identity, st, res.NameID, res.SessionIndex)
}

// ssoComplete provisions/links the user, syncs groups and issues the handoff.
func (a *API) ssoComplete(w http.ResponseWriter, r *http.Request, p store.SSOProviderRow, id store.SSOAssertedIdentity, st store.SSOLoginState, nameID, sessionIndex string) {
	ctx := r.Context()
	res, err := store.GatewaySSOLogin(ctx, a.db, p, id, clientIP(r))
	if err != nil {
		code := "login_failed"
		switch {
		case errors.Is(err, store.ErrSSODomainNotAllowed):
			code = "domain_not_allowed"
		case errors.Is(err, store.ErrSSOAccountUnlinked):
			code = "account_not_linked"
		case errors.Is(err, store.ErrSSONoAccount):
			code = "no_account"
		case errors.Is(err, store.ErrSSOUserDisabled):
			code = "account_disabled"
		case errors.Is(err, store.ErrSSOEmailMissing):
			code = "email_missing"
		default:
			a.log.ErrorContext(ctx, "sso login failed", "provider_id", p.ID, "error", err)
		}
		a.ssoAuditFailure(r, p, code, err, map[string]any{"email": id.Email, "subject": truncate(id.Subject, 200),
			"email_verified": id.EmailVerified})
		a.ssoFail(w, r, code)
		return
	}
	code, err := randomToken()
	if err == nil {
		err = store.GatewayCreateHandoff(ctx, a.db, sha256Hex(code), st.BindingHash, store.SSOHandoff{
			UserID: res.User.ID, ProviderID: p.ID, Namespace: p.Namespace, ReturnTo: st.ReturnTo,
			SSONameID: nameID, SSOSessionIndex: sessionIndex,
		}, ssoHandoffTTL)
	}
	if err != nil {
		a.log.ErrorContext(ctx, "sso handoff failed", "error", err)
		a.ssoFail(w, r, "temporarily_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The fragment is never sent to a server, so the code stays out of access
	// logs and Referer headers.
	http.Redirect(w, r, a.sso.cfg.CompleteURL()+"#handoff="+code, http.StatusSeeOther)
}

// ── exchange ─────────────────────────────────────────────────────────────────

func (a *API) ssoExchange(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || a.authAttemptBlocked(w, r) || !a.ssoReady(w) {
		return
	}
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Handoff string `json:"handoff"`
	}
	if err := jsonUnmarshalStrict(body, &req); err != nil || len(req.Handoff) != 43 {
		writeError(w, http.StatusBadRequest, "invalid", "a handoff code is required")
		return
	}
	binding := a.bindingCookie(r)
	h, err := store.GatewayConsumeHandoff(r.Context(), a.db, sha256Hex(req.Handoff), sha256Hex(binding))
	if errors.Is(err, store.ErrSSOHandoffInvalid) || (err == nil && binding == "") {
		a.recordAuthFailure(r, "sso-handoff\x00"+req.Handoff)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "this sign-in link is invalid, expired, already used, or was started in another browser")
		return
	}
	if err != nil {
		a.log.ErrorContext(r.Context(), "sso exchange failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is temporarily unavailable")
		return
	}
	user, err := store.GatewayGetUser(r.Context(), a.db, h.Namespace, h.UserID)
	if err != nil {
		a.fail(w, r, "sso exchange", err)
		return
	}
	token, session, err := store.GatewayCreateSSOSession(r.Context(), a.db, user.ID, h.ProviderID, h.SSONameID, h.SSOSessionIndex,
		authn.SessionTTL, r.UserAgent(), clientIP(r))
	if err != nil {
		a.log.ErrorContext(r.Context(), "could not create sso session", "user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not start a session")
		return
	}
	a.setBindingCookie(w, "", -1)
	a.auditAttempt(r, store.AuditEntry{
		Namespace: user.Namespace, ActorID: &user.ID, ActorKind: "user", ActorLabel: user.Email,
		Action: "session.login", TargetKind: "session", TargetID: session.ID, Outcome: store.AuditSuccess,
		Detail: map[string]any{"method": "sso", "provider_id": h.ProviderID},
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"token": token, "expires_at": session.ExpiresAt, "return_to": sso.SafeReturnTo(h.ReturnTo),
		"user": map[string]any{"id": user.ID, "email": user.Email, "display_name": user.DisplayName,
			"namespace": user.Namespace, "role": user.Role},
	})
}

// ── SAML metadata and logout landing ─────────────────────────────────────────

func (a *API) ssoSAMLMetadata(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.ssoReady(w) {
		return
	}
	p, err := store.GatewayGetSSOProviderByID(r.Context(), a.db, r.PathValue("id"))
	if err != nil || p.Kind != "saml" {
		writeError(w, http.StatusNotFound, "not_found", "no such SAML provider")
		return
	}
	md, err := a.sso.saml.Metadata(p)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_metadata", "the stored IdP metadata is not usable")
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(md)
}

// ssoSAMLSLO is where the IdP returns after SP-initiated logout. The local
// session was revoked before the browser left; nothing here grants anything.
func (a *API) ssoSAMLSLO(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.ssoReady(w) {
		return
	}
	http.Redirect(w, r, a.sso.cfg.PostLogoutRedirect(), http.StatusSeeOther)
}

// ssoLogoutURL is the IdP logout URL for an SSO session, or "".
func (a *API) ssoLogoutURL(ctx context.Context, p authn.Principal) string {
	if a.sso == nil || !a.sso.cfg.Enabled() || a.db == nil || p.Kind != authn.KindSession || p.SSOProviderID == "" {
		return ""
	}
	prov, err := store.GatewayGetSSOProviderByID(ctx, a.db, p.SSOProviderID)
	if err != nil {
		return ""
	}
	switch prov.Kind {
	case "oidc":
		u, err := a.sso.oidc.EndSessionURL(ctx, prov)
		if err != nil {
			a.log.WarnContext(ctx, "oidc end_session_endpoint unavailable", "error", err)
			return ""
		}
		return u
	case "saml":
		_, nameID, _, err := store.GatewaySessionSSO(ctx, a.db, p.KeyID)
		if err != nil {
			return ""
		}
		u, err := a.sso.saml.LogoutURL(prov, nameID, "")
		if err != nil {
			return ""
		}
		return u
	}
	return ""
}

func jsonUnmarshalStrict(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(nonEmptyJSON(body)))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
