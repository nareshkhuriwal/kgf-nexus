// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
//go:build integration

// Gateway-level coverage of verification and recovery against real Postgres.
// Run against an isolated database: NEXUS_TEST_DATABASE_URL must point at a
// schema with migrations through 0017 applied.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

var tokenInLink = regexp.MustCompile(`#token=([A-Za-z0-9_\-]+)`)

type emailHarness struct {
	t      *testing.T
	api    *API
	h      http.Handler
	mailer *recordingMailer
}

func newEmailHarness(t *testing.T, mutate func(*EmailFlowConfig)) *emailHarness {
	t.Helper()
	db := attentionPool(t)
	m := &recordingMailer{}
	cfg := DefaultEmailFlowConfig()
	cfg.Mailer = m
	cfg.PublicURL = "https://nexus.example"
	cfg.Retry = mail.RetryPolicy{Attempts: 1}
	if mutate != nil {
		mutate(&cfg)
	}
	auth := authn.New(db).WithRBAC().WithIdentity(authn.StoreIdentity{DB: db})
	api := New(db, auth, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithIdentity(NewStoreIdentityAdmin(db)).WithEmailFlows(cfg)
	t.Cleanup(func() { _ = api.CloseEmailFlows(context.Background()) })
	return &emailHarness{t: t, api: api, h: api.Routes(), mailer: m}
}

func (e *emailHarness) call(method, path, token, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *emailHarness) expect(method, path, token, body string, status int) *httptest.ResponseRecorder {
	e.t.Helper()
	w := e.call(method, path, token, body)
	if w.Code != status {
		e.t.Fatalf("%s %s: %d %s, want %d", method, path, w.Code, w.Body, status)
	}
	return w
}

// register creates a customer through the public route and returns its email.
func (e *emailHarness) register(password string) (email, namespace string) {
	e.t.Helper()
	email = fmt.Sprintf("owner-%d@example.test", time.Now().UnixNano())
	w := e.expect("POST", "/v1/auth/register", "", `{"name":"Email test","email":"`+email+`","password":"`+password+`"}`, 201)
	var out struct {
		Tenant struct {
			Namespace string `json:"namespace"`
		} `json:"tenant"`
		EmailVerification map[string]any `json:"email_verification"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	if out.EmailVerification["delivery"] != "queued" || out.EmailVerification["required"] != true {
		e.t.Fatalf("registration verification status: %v", out.EmailVerification)
	}
	ns := out.Tenant.Namespace
	// Remove the customer afterwards: other suites in this package count
	// registrations and assume they are alone in the database.
	e.t.Cleanup(func() {
		ctx := context.Background()
		for _, q := range []string{
			`UPDATE tenants SET owner_user_id = NULL WHERE namespace = $1`,
			`DELETE FROM email_tokens WHERE user_id IN (SELECT id FROM users WHERE namespace = $1)`,
			`DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE namespace = $1)`,
			`DELETE FROM audit_log WHERE namespace = $1`,
			`DELETE FROM users WHERE namespace = $1`,
			`DELETE FROM tenants WHERE namespace = $1`,
		} {
			if _, err := e.api.db.Exec(ctx, q, ns); err != nil {
				e.t.Logf("cleanup %q: %v", q, err)
			}
		}
	})
	return email, ns
}

// lastToken waits for queued mail and returns the token in the newest message
// to `to`, and how many messages that address has received.
func (e *emailHarness) lastToken(to string) (string, int) {
	e.t.Helper()
	e.api.email.flush()
	var tok string
	n := 0
	for _, m := range e.mailer.messages() {
		if m.To != to {
			continue
		}
		n++
		match := tokenInLink.FindStringSubmatch(m.Text)
		if match == nil {
			e.t.Fatalf("message to %s has no link: %s", to, m.Text)
		}
		tok = match[1]
	}
	return tok, n
}

func (e *emailHarness) login(email, password string) *httptest.ResponseRecorder {
	return e.call("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"`+password+`"}`)
}

func sessionToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("no session token: %s", w.Body)
	}
	return out.Token
}

func TestIntegrationRegistrationVerificationGatesLogin(t *testing.T) {
	e := newEmailHarness(t, nil)
	email, _ := e.register("registration-password-1")

	tok, n := e.lastToken(email)
	if n != 1 || !strings.HasPrefix(tok, "nxverify_") {
		t.Fatalf("registration sent %d messages, token %q", n, tok)
	}
	if w := e.login(email, "registration-password-1"); w.Code != 403 || !strings.Contains(w.Body.String(), "email_not_verified") {
		t.Fatalf("unverified owner login: %d %s", w.Code, w.Body)
	}
	w := e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+tok+`"}`, 200)
	if !strings.Contains(w.Body.String(), `"verified":true`) {
		t.Fatalf("verify response: %s", w.Body)
	}
	// Reuse: the same link a second time is dead.
	w = e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+tok+`"}`, 400)
	if !strings.Contains(w.Body.String(), "invalid_token") {
		t.Fatalf("reuse: %s", w.Body)
	}
	e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"registration-password-1"}`, 200)

	// A verified account asking for another verification link gets the same
	// answer and no mail.
	e.expect("POST", "/v1/auth/email/verification", "", `{"email":"`+email+`"}`, 202)
	if _, n := e.lastToken(email); n != 1 {
		t.Fatalf("verified account was sent another verification link (%d total)", n)
	}
}

func TestIntegrationResendSupersedesEarlierLink(t *testing.T) {
	e := newEmailHarness(t, nil)
	email, _ := e.register("registration-password-2")
	first, _ := e.lastToken(email)
	e.expect("POST", "/v1/auth/email/verification", "", `{"email":"`+strings.ToUpper(email)+`"}`, 202)
	second, n := e.lastToken(email)
	if n != 2 || second == first {
		t.Fatalf("resend: %d messages", n)
	}
	e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+first+`"}`, 400)
	e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+second+`"}`, 200)
}

// TestIntegrationForgotPasswordIsNotAnAccountOracle: an existing and a
// nonexistent address get byte-identical responses with identical headers.
func TestIntegrationForgotPasswordIsNotAnAccountOracle(t *testing.T) {
	e := newEmailHarness(t, nil)
	email, _ := e.register("registration-password-3")
	ghost := fmt.Sprintf("ghost-%d@example.test", time.Now().UnixNano())

	for _, path := range []string{"/v1/auth/password/forgot", "/v1/auth/email/verification"} {
		real := e.call("POST", path, "", `{"email":"`+email+`"}`)
		fake := e.call("POST", path, "", `{"email":"`+ghost+`"}`)
		if real.Code != 202 || fake.Code != 202 {
			t.Fatalf("%s: %d / %d, want 202 / 202", path, real.Code, fake.Code)
		}
		if real.Body.String() != fake.Body.String() {
			t.Errorf("%s bodies differ:\n real: %s\n fake: %s", path, real.Body, fake.Body)
		}
		for k := range real.Header() {
			if real.Header().Get(k) != fake.Header().Get(k) {
				t.Errorf("%s header %s differs: %q vs %q", path, k, real.Header().Get(k), fake.Header().Get(k))
			}
		}
		if len(real.Header()) != len(fake.Header()) {
			t.Errorf("%s header sets differ", path)
		}
	}
	e.api.email.flush()
	for _, m := range e.mailer.messages() {
		if m.To == ghost {
			t.Fatal("mail was sent to an address with no account")
		}
	}
}

func TestIntegrationResetFlowRevokesSessionsAndConsumesLink(t *testing.T) {
	e := newEmailHarness(t, nil)
	email, ns := e.register("original-password-44")
	vtok, _ := e.lastToken(email)

	// Wrong purpose: a verification link does not reset a password.
	e.expect("POST", "/v1/auth/password/reset", "", `{"token":"`+vtok+`","password":"attacker-password-1"}`, 400)
	e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+vtok+`"}`, 200)

	sess := sessionToken(t, e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"original-password-44"}`, 200))
	e.expect("GET", "/v1/auth/whoami", sess, "", 200)

	e.expect("POST", "/v1/auth/password/forgot", "", `{"email":"`+email+`","namespace":"`+ns+`"}`, 202)
	rtok, _ := e.lastToken(email)
	if !strings.HasPrefix(rtok, "nxreset_") {
		t.Fatalf("reset token %q", rtok)
	}
	// Wrong purpose the other way round.
	e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+rtok+`"}`, 400)
	// Too short a password is refused before the link is spent.
	e.expect("POST", "/v1/auth/password/reset", "", `{"token":"`+rtok+`","password":"short"}`, 422)

	w := e.expect("POST", "/v1/auth/password/reset", "", `{"token":"`+rtok+`","password":"replacement-password-55"}`, 200)
	if !strings.Contains(w.Body.String(), `"sessions_revoked":1`) {
		t.Fatalf("reset response: %s", w.Body)
	}
	// The session that existed before the reset is dead.
	e.expect("GET", "/v1/auth/whoami", sess, "", 401)
	// Reuse is refused.
	e.expect("POST", "/v1/auth/password/reset", "", `{"token":"`+rtok+`","password":"third-password-66666"}`, 400)
	// Old password out, new one in.
	e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"original-password-44"}`, 401)
	e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"replacement-password-55"}`, 200)

	var audits int
	if err := e.api.db.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_log WHERE namespace=$1 AND action IN ('user.password_reset','user.password_reset_requested','user.email_verified','user.email_verification_sent')`, ns).Scan(&audits); err != nil || audits != 4 {
		t.Fatalf("audit rows = %d (%v), want 4 (sent, verified, requested, reset)", audits, err)
	}
}

func TestIntegrationExpiredResetLinkIsRefused(t *testing.T) {
	e := newEmailHarness(t, nil)
	email, _ := e.register("original-password-77")
	vtok, _ := e.lastToken(email)
	e.expect("POST", "/v1/auth/email/verify", "", `{"token":"`+vtok+`"}`, 200)
	e.expect("POST", "/v1/auth/password/forgot", "", `{"email":"`+email+`"}`, 202)
	rtok, _ := e.lastToken(email)
	// Age the token past its deadline by the database clock.
	if _, err := e.api.db.Exec(context.Background(),
		`UPDATE email_tokens SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' WHERE purpose='reset_password' AND email=$1`, email); err != nil {
		t.Fatal(err)
	}
	w := e.expect("POST", "/v1/auth/password/reset", "", `{"token":"`+rtok+`","password":"replacement-password-88"}`, 400)
	if !strings.Contains(w.Body.String(), "invalid_token") {
		t.Fatalf("expired link: %s", w.Body)
	}
	e.expect("POST", "/v1/auth/login", "", `{"email":"`+email+`","password":"original-password-77"}`, 200)
}

// TestIntegrationPerAddressLimitDropsSilently: the fourth request in an hour
// gets the same answer as the first three but sends nothing.
func TestIntegrationPerAddressLimitDropsSilently(t *testing.T) {
	e := newEmailHarness(t, func(c *EmailFlowConfig) { c.MaxLinksPerHour = 100 })
	email, _ := e.register("original-password-99")
	_, before := e.lastToken(email)

	var bodies []string
	for i := 0; i < 4; i++ {
		w := e.expect("POST", "/v1/auth/password/forgot", "", `{"email":"`+email+`"}`, 202)
		bodies = append(bodies, w.Body.String())
	}
	for _, b := range bodies[1:] {
		if b != bodies[0] {
			t.Fatal("a throttled request answered differently")
		}
	}
	if _, after := e.lastToken(email); after-before != 3 {
		t.Fatalf("sent %d reset mails for 4 requests, want 3 (per-address burst)", after-before)
	}
}

// TestIntegrationDatabaseCapHoldsAcrossReplicas: two gateways, generous in-memory
// limits, one shared database. The hourly cap is in Postgres, so it still holds.
func TestIntegrationDatabaseCapHoldsAcrossReplicas(t *testing.T) {
	generous := func(c *EmailFlowConfig) {
		c.MaxLinksPerHour = 2
		c.PerEmail = RateLimitConfig{RequestsPerSecond: 100, Burst: 100}
	}
	a := newEmailHarness(t, generous)
	b := newEmailHarness(t, generous)
	email, _ := a.register("original-password-10")
	a.lastToken(email) // registration used one of the verification allowance
	for _, h := range []*emailHarness{a, b, a, b} {
		h.expect("POST", "/v1/auth/password/forgot", "", `{"email":"`+email+`"}`, 202)
		h.api.email.flush()
	}
	count := 0
	for _, h := range []*emailHarness{a, b} {
		for _, m := range h.mailer.messages() {
			if m.To == email && strings.Contains(m.Text, "#token=nxreset_") {
				count++
			}
		}
	}
	if count != 2 {
		t.Fatalf("%d reset mails across two replicas, want the database cap of 2", count)
	}
}

func TestIntegrationDeliveryFailureIsAuditedNotHidden(t *testing.T) {
	e := newEmailHarness(t, nil)
	email, ns := e.register("original-password-11")
	e.lastToken(email)
	e.mailer.mu.Lock()
	e.mailer.err = mail.Permanent(fmt.Errorf("550 mailbox unavailable"))
	e.mailer.mu.Unlock()
	e.expect("POST", "/v1/auth/password/forgot", "", `{"email":"`+email+`"}`, 202)
	e.api.email.flush()
	var outcome string
	if err := e.api.db.QueryRow(context.Background(),
		`SELECT outcome FROM audit_log WHERE namespace=$1 AND action='user.password_reset_requested' ORDER BY id DESC LIMIT 1`, ns).Scan(&outcome); err != nil || outcome != "error" {
		t.Fatalf("failed delivery audited as %q (%v), want error", outcome, err)
	}
}
