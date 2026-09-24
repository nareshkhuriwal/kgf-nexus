// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// recordingMailer captures what would have been sent.
type recordingMailer struct {
	mu   sync.Mutex
	sent []mail.Message
	err  error
}

func (m *recordingMailer) Send(_ context.Context, msg mail.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

func (m *recordingMailer) messages() []mail.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mail.Message(nil), m.sent...)
}

// newEmailUnitAPI wires email flows over the in-memory harness. Only paths that
// answer before touching the database are exercised here; everything that
// issues or consumes a token is covered against Postgres in the integration file.
func newEmailUnitAPI(t *testing.T, cfg EmailFlowConfig) (*API, http.Handler, *fakeIdentityAdmin) {
	t.Helper()
	db := newRestDB()
	ids := newFakeIdentityAdmin()
	auth := authn.New(db).WithIdentity(sessionBridge{ids})
	api := New(db, auth, &fakeWFM{}, fakeRM{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(ids).WithEmailFlows(cfg)
	t.Cleanup(func() { _ = api.CloseEmailFlows(context.Background()) })
	return api, api.Routes(), ids
}

func TestEmailRoutesAbsentWhenNotWired(t *testing.T) {
	h, _, _ := newAdminAPI(t)
	for _, p := range []string{"/v1/auth/email/verification", "/v1/auth/email/verify", "/v1/auth/password/forgot", "/v1/auth/password/reset"} {
		if rec := doJSON(t, h, "POST", p, "", `{}`); rec.Code != http.StatusNotFound {
			t.Errorf("%s without email flows: %d, want 404", p, rec.Code)
		}
	}
}

// TestDisabledMailerIsExplicit: with no mail server, asking for a link must say
// so -- not answer "check your inbox" for a message that will never exist.
func TestDisabledMailerIsExplicit(t *testing.T) {
	_, h, _ := newEmailUnitAPI(t, DefaultEmailFlowConfig())
	for _, p := range []string{"/v1/auth/password/forgot", "/v1/auth/email/verification"} {
		known := doJSON(t, h, "POST", p, "", `{"email":"admin@example.com"}`)
		unknown := doJSON(t, h, "POST", p, "", `{"email":"ghost@example.com"}`)
		if known.Code != http.StatusServiceUnavailable || !strings.Contains(known.Body.String(), "email_unavailable") {
			t.Fatalf("%s with mail disabled: %d %s", p, known.Code, known.Body)
		}
		// Still not an oracle: the answer does not depend on the account.
		if known.Body.String() != unknown.Body.String() {
			t.Errorf("%s: disabled-mail responses differ by account", p)
		}
	}
}

func TestLinkRequestValidatesEmailSyntax(t *testing.T) {
	cfg := DefaultEmailFlowConfig()
	cfg.Mailer = &recordingMailer{}
	cfg.PublicURL = "https://nexus.example"
	_, h, _ := newEmailUnitAPI(t, cfg)
	for _, body := range []string{`{"email":"not-an-address"}`, `{"email":""}`, `{"email":"a@b.c\r\nBcc: x@y.z"}`} {
		if rec := doJSON(t, h, "POST", "/v1/auth/password/forgot", "", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d, want 422", body, rec.Code)
		}
	}
	if rec := doJSON(t, h, "POST", "/v1/auth/password/forgot", "", `{not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d", rec.Code)
	}
}

func TestPerIPRateLimitOnEveryEmailRoute(t *testing.T) {
	cfg := DefaultEmailFlowConfig()
	cfg.PerIP = RateLimitConfig{RequestsPerSecond: 0.001, Burst: 2}
	api, h, _ := newEmailUnitAPI(t, cfg)
	frozen := time.Unix(1_700_000_000, 0)
	api.email.ipLim.clock = func() time.Time { return frozen }

	req := func(path, ip string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(`{"email":"x@example.com","token":"t"}`))
		r.RemoteAddr = ip + ":40000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	// The limit is shared across the four routes: they are one abuse surface.
	req("/v1/auth/password/forgot", "203.0.113.5")
	req("/v1/auth/email/verify", "203.0.113.5")
	rec := req("/v1/auth/password/reset", "203.0.113.5")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third request from one address: %d (Retry-After %q), want 429 with a hint", rec.Code, rec.Header().Get("Retry-After"))
	}
	// A different address has its own allowance.
	if rec := req("/v1/auth/password/forgot", "203.0.113.6"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("one address's limit throttled another")
	}
}

func TestRequesterIPTrustsOnlyTheLocalProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	r.Header.Set("X-Real-IP", "198.51.100.1")
	r.Header.Set("X-Forwarded-For", "198.51.100.2")
	if got := requesterIP(r); got != "203.0.113.9" {
		t.Errorf("public peer: keyed on %q; a client-supplied header must not choose its own bucket", got)
	}
	r.RemoteAddr = "172.18.0.4:5555" // the console's nginx on the compose network
	if got := requesterIP(r); got != "198.51.100.1" {
		t.Errorf("private proxy peer: keyed on %q, want the X-Real-IP it set", got)
	}
	r.Header.Del("X-Real-IP")
	if got := requesterIP(r); got != "172.18.0.4" {
		t.Errorf("proxy without X-Real-IP: %q", got)
	}
}

// TestUnverifiedLoginBlockedOnlyAfterPassword: the 403 is for someone who knows
// the password; a wrong password stays the same opaque 401 as before.
func TestUnverifiedLoginBlockedOnlyAfterPassword(t *testing.T) {
	_, h, ids := newEmailUnitAPI(t, DefaultEmailFlowConfig())

	wrong := doJSON(t, h, "POST", "/v1/auth/login", "", `{"namespace":"team-ml","email":"admin@example.com","password":"not-the-password"}`)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password on an unverified account: %d, want 401", wrong.Code)
	}
	right := doJSON(t, h, "POST", "/v1/auth/login", "", `{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`)
	if right.Code != http.StatusForbidden || !strings.Contains(right.Body.String(), "email_not_verified") {
		t.Fatalf("unverified login: %d %s, want 403 email_not_verified", right.Code, right.Body)
	}
	if strings.Contains(right.Body.String(), "nxsess_") {
		t.Fatal("a session token was issued to an unverified account")
	}
	if got := ids.auditedActions(); got[len(got)-1] != "session.login/denied" {
		t.Errorf("blocked login audited as %v", got)
	}

	// Verified accounts sign in normally.
	now := time.Now()
	for id, u := range ids.users {
		u.EmailVerifiedAt = &now
		ids.users[id] = u
	}
	if rec := doJSON(t, h, "POST", "/v1/auth/login", "", `{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`); rec.Code != http.StatusOK {
		t.Fatalf("verified login: %d %s", rec.Code, rec.Body)
	}
}

func TestVerificationNotRequiredWhenPolicyOff(t *testing.T) {
	cfg := DefaultEmailFlowConfig()
	cfg.RequireVerified = false
	_, h, _ := newEmailUnitAPI(t, cfg)
	if rec := doJSON(t, h, "POST", "/v1/auth/login", "", `{"namespace":"team-ml","email":"admin@example.com","password":"correct-password-1234"}`); rec.Code != http.StatusOK {
		t.Fatalf("policy off: %d", rec.Code)
	}
}

func TestEmailFlowConfigValidate(t *testing.T) {
	ok := DefaultEmailFlowConfig()
	if err := ok.Validate(); err != nil {
		t.Fatalf("defaults with mail disabled: %v", err)
	}
	enabled := ok
	enabled.Mailer = &recordingMailer{}
	if err := enabled.Validate(); err == nil || !strings.Contains(err.Error(), "NEXUS_PUBLIC_URL") {
		t.Fatalf("mail enabled without a public URL: %v", err)
	}
	for _, u := range []string{"nexus.example", "ftp://nexus.example", "https://nexus.example/?a=1", "https://u:p@nexus.example", "https://nexus.example/#x"} {
		enabled.PublicURL = u
		if err := enabled.Validate(); err == nil {
			t.Errorf("public URL %q accepted", u)
		}
	}
	enabled.PublicURL = "https://nexus.example/console/"
	if err := enabled.Validate(); err != nil {
		t.Fatalf("valid URL with a path: %v", err)
	}
	short := ok
	short.ResetTTL = time.Minute
	if short.Validate() == nil {
		t.Error("a one-minute reset link was accepted")
	}
	long := ok
	long.VerifyTTL = 30 * 24 * time.Hour
	if long.Validate() == nil {
		t.Error("a thirty-day verification link was accepted")
	}
}

// TestLinksCarryTheTokenInTheFragment: never the query string, which would put
// the credential in access logs and Referer headers.
func TestLinksCarryTheTokenInTheFragment(t *testing.T) {
	cfg := DefaultEmailFlowConfig()
	cfg.Mailer = &recordingMailer{}
	cfg.PublicURL = "https://nexus.example/console/"
	api, _, _ := newEmailUnitAPI(t, cfg)
	link := api.email.link(store.PurposeResetPassword, "nxreset_abc-DEF_123")
	if link != "https://nexus.example/console/reset-password#token=nxreset_abc-DEF_123" {
		t.Fatalf("link = %q", link)
	}
	msg := api.email.compose(store.EmailRecipient{Email: "p@example.com", Namespace: "ns", DisplayName: "Pat\r\nBcc: x"}, store.PurposeVerifyEmail,
		store.IssuedEmailToken{Plaintext: "nxverify_zzz"})
	if !strings.Contains(msg.Text, "https://nexus.example/console/verify-email#token=nxverify_zzz") || !strings.Contains(msg.Text, "24 hours") {
		t.Fatalf("verification body: %s", msg.Text)
	}
	if strings.Contains(msg.Text, "Bcc") {
		t.Fatal("a display name with a line break was copied into the body")
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute: "30 minutes", time.Hour: "1 hour", 2 * time.Hour: "2 hours",
		24 * time.Hour: "24 hours", 72 * time.Hour: "3 days", time.Minute: "1 minute",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestCloseRefusesNewWork(t *testing.T) {
	cfg := DefaultEmailFlowConfig()
	cfg.Mailer = &recordingMailer{}
	cfg.PublicURL = "https://nexus.example"
	api, h, _ := newEmailUnitAPI(t, cfg)
	if err := api.CloseEmailFlows(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, h, "POST", "/v1/auth/password/forgot", "", `{"email":"admin@example.com"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("after close: %d, want 503 rather than a promise nobody will keep", rec.Code)
	}
}
