// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	netmail "net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/mail"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Email ownership verification and password recovery.
//
// POLICY FOR UNVERIFIED ACCOUNTS (docs/MULTITENANCY.md): when verification is
// required -- the default -- an account whose address is unproven cannot sign in.
// Login checks the password FIRST and only then refuses with 403
// email_not_verified, so the refusal is visible only to someone who already knows
// the password and is not an oracle for which addresses have accounts. The
// alternative, a restricted session, was rejected: it would put a second
// authorization dimension into every guarded route, and a registration made with
// someone else's address would still get a working session inside a tenant it
// claims to own. Accounts that existed before migration 0017 are verified by the
// migration itself, so nobody who could sign in before is locked out by it.
//
// FOUR UNGUARDED ROUTES, like login and for the same reason: the caller has no
// credential. Each carries its own rate limits.
//
//   - POST /v1/auth/email/verification  request a (new) verification link
//   - POST /v1/auth/email/verify         consume a verification link
//   - POST /v1/auth/password/forgot      request a reset link
//   - POST /v1/auth/password/reset       consume a reset link, set a password
//
// NO ACCOUNT ORACLE. The two "request a link" routes answer with byte-identical
// responses whether or not the address has an account, and they answer BEFORE the
// lookup happens: the lookup, token issuance and delivery all run on a background
// worker. Answering first is what makes the timing identical too -- a synchronous
// "look up, mint, then respond" is measurably slower for a real account, and that
// difference enumerates accounts just as well as a different message would.
//
// SECRETS. A link is a bearer credential. The token travels in the URL FRAGMENT
// (never sent to a server, so never in an access log or a Referer header) and the
// console POSTs it in a body. Nothing here logs a token, a link, or a full
// recipient address.

// EmailFlowConfig configures verification and recovery.
type EmailFlowConfig struct {
	// Mailer delivers the links. mail.Disabled (or nil) makes the link-request
	// routes answer 503 email_unavailable rather than pretend to have sent mail.
	Mailer mail.Sender
	// PublicURL is the console's external origin, e.g. https://nexus.example.com.
	// Links are built from it. Required when the mailer is enabled.
	PublicURL string
	// VerifyTTL and ResetTTL bound how long a link works. A reset link is the
	// more dangerous credential, so it is short by default.
	VerifyTTL time.Duration
	ResetTTL  time.Duration
	// RequireVerified blocks sign-in for accounts whose address is unproven.
	RequireVerified bool
	// MaxLinksPerHour caps links per account and purpose, counted in the
	// database so it holds across gateway replicas.
	MaxLinksPerHour int
	// PerIP limits the four routes per requesting address; PerEmail limits link
	// requests per target address. Both are in-memory token buckets (the same
	// bounded limiter as the gateway's own buckets), so they are per replica.
	PerIP    RateLimitConfig
	PerEmail RateLimitConfig
	// Retry governs delivery retries after the response has been sent.
	Retry mail.RetryPolicy
	// Workers and QueueSize bound background delivery. A full queue answers 503
	// rather than growing without limit.
	Workers   int
	QueueSize int
	// JobTimeout bounds one request's background work, retries included.
	JobTimeout time.Duration
}

// DefaultEmailFlowConfig returns the production defaults.
func DefaultEmailFlowConfig() EmailFlowConfig {
	return EmailFlowConfig{
		Mailer:          mail.Disabled{},
		VerifyTTL:       24 * time.Hour,
		ResetTTL:        30 * time.Minute,
		RequireVerified: true,
		MaxLinksPerHour: 5,
		// Ten requests a minute from one address is ample for a person retrying a
		// form and useless for anyone enumerating or spraying.
		PerIP: RateLimitConfig{RequestsPerSecond: 10.0 / 60, Burst: 10, IdleEviction: time.Hour},
		// Three links per address per hour, so the gateway cannot be made to fill
		// someone's inbox; excess requests are silently dropped, with the SAME
		// response, because a different answer would itself be an oracle.
		PerEmail:   RateLimitConfig{RequestsPerSecond: 3.0 / 3600, Burst: 3, IdleEviction: 2 * time.Hour},
		Retry:      mail.DefaultRetry,
		Workers:    2,
		QueueSize:  256,
		JobTimeout: 2 * time.Minute,
	}
}

// Validate refuses a configuration that would produce broken or unsafe links.
func (c EmailFlowConfig) Validate() error {
	if c.VerifyTTL < 5*time.Minute || c.VerifyTTL > 7*24*time.Hour {
		return fmt.Errorf("email verification link lifetime %s must be between 5m and 168h", c.VerifyTTL)
	}
	if c.ResetTTL < 5*time.Minute || c.ResetTTL > 24*time.Hour {
		return fmt.Errorf("password reset link lifetime %s must be between 5m and 24h", c.ResetTTL)
	}
	if mail.IsDisabled(c.Mailer) {
		return nil
	}
	if _, err := parsePublicURL(c.PublicURL); err != nil {
		return err
	}
	return nil
}

func parsePublicURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("NEXUS_PUBLIC_URL is required when email delivery is configured: links in emails are built from it")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("NEXUS_PUBLIC_URL %q must be an absolute http(s) URL with no query, fragment or credentials", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// emailFlows is the runtime half: limiters and the background delivery queue.
type emailFlows struct {
	cfg   EmailFlowConfig
	base  *url.URL
	db    store.GWDB
	log   *slog.Logger
	ipLim *rateLimiter
	adLim *rateLimiter

	mu      sync.RWMutex
	closed  bool
	jobs    chan func(context.Context)
	pending sync.WaitGroup
	workers sync.WaitGroup
	stop    chan struct{}
}

// WithEmailFlows enables verification and recovery and starts the delivery
// workers. Call CloseEmailFlows on shutdown so queued mail is drained.
//
// Opt-in like every other optional surface here: without it the four routes are
// not registered and login does not consult verification state, so a gateway
// assembled without mail behaves exactly as before.
func (a *API) WithEmailFlows(cfg EmailFlowConfig) *API {
	if cfg.Mailer == nil {
		cfg.Mailer = mail.Disabled{}
	}
	def := DefaultEmailFlowConfig()
	if cfg.Workers <= 0 {
		cfg.Workers = def.Workers
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = def.QueueSize
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = def.JobTimeout
	}
	if cfg.MaxLinksPerHour <= 0 {
		cfg.MaxLinksPerHour = def.MaxLinksPerHour
	}
	ef := &emailFlows{
		cfg:   cfg,
		db:    a.db,
		log:   a.log,
		ipLim: newRateLimiter(cfg.PerIP, a.clock),
		adLim: newRateLimiter(cfg.PerEmail, a.clock),
		jobs:  make(chan func(context.Context), cfg.QueueSize),
		stop:  make(chan struct{}),
	}
	// Validate() is the caller's startup check; here a bad URL only means links
	// cannot be built, which deliver() reports per message.
	ef.base, _ = parsePublicURL(cfg.PublicURL)
	for i := 0; i < cfg.Workers; i++ {
		ef.workers.Add(1)
		go ef.work()
	}
	ef.workers.Add(1)
	go ef.sweep()
	a.email = ef
	return a
}

// CloseEmailFlows stops accepting new mail work and waits for queued work to
// finish or ctx to end, whichever is first.
func (a *API) CloseEmailFlows(ctx context.Context) error {
	ef := a.email
	if ef == nil {
		return nil
	}
	ef.mu.Lock()
	if !ef.closed {
		ef.closed = true
		close(ef.jobs)
		close(ef.stop)
	}
	ef.mu.Unlock()
	done := make(chan struct{})
	go func() { ef.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("email delivery did not drain: %w", ctx.Err())
	}
}

func (ef *emailFlows) work() {
	defer ef.workers.Done()
	for job := range ef.jobs {
		ctx, cancel := context.WithTimeout(context.Background(), ef.cfg.JobTimeout)
		job(ctx)
		cancel()
		ef.pending.Done()
	}
}

// sweep deletes long-expired tokens hourly. Tokens are kept a day past expiry so
// the per-hour issuance cap, which counts recent tokens, is never undercounted.
func (ef *emailFlows) sweep() {
	defer ef.workers.Done()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ef.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			n, err := store.GatewayDeleteExpiredEmailTokens(ctx, ef.db, time.Now().Add(-24*time.Hour), 5000)
			cancel()
			if err != nil {
				ef.log.Error("email token sweep failed", "error", err)
			} else if n > 0 {
				ef.log.Info("expired email tokens removed", "count", n)
			}
		}
	}
}

// enqueue schedules background work, reporting false when the queue is full or
// closed. Never blocks the request.
func (ef *emailFlows) enqueue(job func(context.Context)) bool {
	ef.mu.RLock()
	defer ef.mu.RUnlock()
	if ef.closed {
		return false
	}
	ef.pending.Add(1)
	select {
	case ef.jobs <- job:
		return true
	default:
		ef.pending.Done()
		return false
	}
}

// flush waits for every queued job. For tests: production never waits on mail.
func (ef *emailFlows) flush() { ef.pending.Wait() }

func (ef *emailFlows) mailEnabled() bool { return !mail.IsDisabled(ef.cfg.Mailer) }

// allowIP applies the per-address limit, writing the 429 when refused.
//
// Keyed like the gateway's own per-address bucket (clientAddressKey), so an
// IPv6 client cannot rotate through its /64 for a fresh allowance here either.
func (ef *emailFlows) allowIP(w http.ResponseWriter, r *http.Request) bool {
	v := ef.ipLim.take(clientAddressKey(r))
	if v.ok {
		return true
	}
	writeRateLimited(w, v, "Too many requests from this address. Try again in a few minutes.")
	return false
}

// requesterIP is the address the per-IP limit is keyed on.
//
// NOT clientIP(): that trusts X-Forwarded-For from anyone, which is fine for an
// audit note and useless for a limit, since a client can put any value in it
// and rotate it per request. Here the peer address is used, EXCEPT when the peer
// is a loopback or private address -- i.e. the console's own nginx, which
// overwrites X-Real-IP with the address it saw. A caller that reaches the
// gateway directly from a private network could forge X-Real-IP; the per-address
// limit and the database cap still bound what that buys them.
func requesterIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if peer := net.ParseIP(host); peer != nil && (peer.IsLoopback() || peer.IsPrivate()) {
		if real := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); real != nil {
			return real.String()
		}
	}
	return host
}

// addressKey keys the per-address limiter by a hash, so the limiter's map does
// not become an in-memory list of every address anyone asked about.
func addressKey(purpose store.EmailTokenPurpose, email string) string {
	sum := sha256.Sum256([]byte(email))
	return string(purpose) + ":" + hex.EncodeToString(sum[:])
}

// normalizeEmail lowercases and validates a bare address.
func normalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" || len(e) > 254 {
		return "", false
	}
	a, err := netmail.ParseAddress(e)
	if err != nil || a.Address != e {
		return "", false
	}
	return e, true
}

// ── request a link ───────────────────────────────────────────────────────

func (a *API) requestVerificationEmail(w http.ResponseWriter, r *http.Request) {
	a.requestEmailLink(w, r, store.PurposeVerifyEmail)
}

func (a *API) forgotPassword(w http.ResponseWriter, r *http.Request) {
	a.requestEmailLink(w, r, store.PurposeResetPassword)
}

// linkAccepted is THE response to a link request. One function, so the "account
// exists" and "no such account" paths cannot drift apart.
func linkAccepted(w http.ResponseWriter, purpose store.EmailTokenPurpose) {
	msg := "If an account with that email address needs verifying, a new verification link is on its way."
	if purpose == store.PurposeResetPassword {
		msg = "If an account with that email address exists, a password reset link is on its way."
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "accepted", "message": msg})
}

func (a *API) requestEmailLink(w http.ResponseWriter, r *http.Request, purpose store.EmailTokenPurpose) {
	if a.rateLimited(w, r) {
		return
	}
	ef := a.email
	if !ef.allowIP(w, r) {
		return
	}
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Email     string `json:"email"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	email, ok := normalizeEmail(req.Email)
	if !ok {
		// Judged on syntax alone, so it reveals nothing about accounts.
		writeError(w, http.StatusUnprocessableEntity, "invalid_email", "Enter a valid email address.")
		return
	}
	namespace := strings.TrimSpace(req.Namespace)
	if len(namespace) > 200 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_namespace", "Workspace identifier is too long.")
		return
	}
	if !ef.mailEnabled() {
		// Explicit, and independent of whether the address has an account, so it
		// is not an oracle. Promising an email that will never be sent would leave
		// a locked-out person waiting on an inbox.
		a.log.WarnContext(r.Context(), "email link requested but email delivery is not configured",
			"purpose", string(purpose))
		writeError(w, http.StatusServiceUnavailable, "email_unavailable",
			"Email delivery is not configured for this deployment. Contact your administrator.")
		return
	}
	if ok, _ := ef.adLim.allow(addressKey(purpose, email)); !ok {
		// Dropped silently, with the identical response: a distinct "slow down"
		// here would be answered for real accounts and fake ones alike, but a
		// silent drop also denies a flooder any feedback at all.
		a.log.InfoContext(r.Context(), "email link request dropped by per-address limit",
			"purpose", string(purpose), "recipient_domain", mail.RecipientDomain(email))
		linkAccepted(w, purpose)
		return
	}
	ip := requesterIP(r)
	if !ef.enqueue(func(ctx context.Context) { ef.processLinkRequest(ctx, purpose, namespace, email, ip) }) {
		// Queue full or shutting down. Independent of the address, so not an
		// oracle; 503 tells the person to try again rather than to wait.
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Email delivery is busy. Try again shortly.")
		return
	}
	linkAccepted(w, purpose)
}

// processLinkRequest runs on a worker, after the response has been sent.
func (ef *emailFlows) processLinkRequest(ctx context.Context, purpose store.EmailTokenPurpose, namespace, email, ip string) {
	recipients, err := store.GatewayFindEmailRecipients(ctx, ef.db, namespace, email)
	if err != nil {
		ef.log.ErrorContext(ctx, "email link lookup failed", "purpose", string(purpose), "error", err)
		return
	}
	for _, rc := range recipients {
		switch {
		case purpose == store.PurposeVerifyEmail && rc.Verified:
			continue // nothing to verify
		case purpose == store.PurposeResetPassword && !rc.HasPassword:
			// A password-less account authenticates elsewhere (a future IdP); a
			// reset link would CREATE a password it was never meant to have.
			continue
		}
		ef.issueAndDeliver(ctx, rc, purpose, ip)
	}
}

// issueAndDeliver mints a token (its own short transaction) and then sends it,
// outside any transaction, with retries.
func (ef *emailFlows) issueAndDeliver(ctx context.Context, rc store.EmailRecipient, purpose store.EmailTokenPurpose, ip string) {
	tok, err := store.GatewayIssueEmailToken(ctx, ef.db, rc.UserID, rc.Email, purpose, ef.ttl(purpose), ip, ef.cfg.MaxLinksPerHour)
	switch {
	case errors.Is(err, store.ErrEmailTokenThrottled):
		ef.log.InfoContext(ctx, "email link not issued: hourly cap reached", "purpose", string(purpose), "user_id", rc.UserID)
		return
	case errors.Is(err, store.ErrNotFound):
		return // disabled or re-addressed between lookup and issue
	case err != nil:
		ef.log.ErrorContext(ctx, "email link could not be issued", "purpose", string(purpose), "user_id", rc.UserID, "error", err)
		return
	}
	ef.deliver(ctx, rc, purpose, tok)
}

func (ef *emailFlows) ttl(p store.EmailTokenPurpose) time.Duration {
	if p == store.PurposeResetPassword {
		return ef.cfg.ResetTTL
	}
	return ef.cfg.VerifyTTL
}

// deliver sends one link and records the outcome in the audit log.
func (ef *emailFlows) deliver(ctx context.Context, rc store.EmailRecipient, purpose store.EmailTokenPurpose, tok store.IssuedEmailToken) {
	action := "user.email_verification_sent"
	if purpose == store.PurposeResetPassword {
		action = "user.password_reset_requested"
	}
	var sendErr error
	if ef.base == nil {
		sendErr = errors.New("NEXUS_PUBLIC_URL is not configured; cannot build the link")
	} else {
		sendErr = mail.SendWithRetry(ctx, ef.cfg.Mailer, ef.compose(rc, purpose, tok), ef.cfg.Retry)
	}
	outcome, delivery := store.AuditSuccess, "sent"
	if sendErr != nil {
		outcome, delivery = store.AuditError, "failed"
		ef.log.ErrorContext(ctx, "email link delivery failed", "purpose", string(purpose),
			"user_id", rc.UserID, "recipient_domain", mail.RecipientDomain(rc.Email), "error", sendErr)
	} else {
		ef.log.InfoContext(ctx, "email link sent", "purpose", string(purpose),
			"user_id", rc.UserID, "recipient_domain", mail.RecipientDomain(rc.Email))
	}
	actorID := rc.UserID
	if err := store.GatewayAppendAudit(ctx, ef.db, store.AuditEntry{
		Namespace: rc.Namespace, ActorID: &actorID, ActorKind: "anonymous", ActorLabel: rc.Email,
		Action: action, TargetKind: "user", TargetID: rc.UserID, Outcome: outcome,
		// The token id, never the token: enough to correlate with a later
		// verification or reset, useless to anyone reading the log.
		Detail: map[string]any{"email_token_id": tok.ID, "expires_at": tok.ExpiresAt, "delivery": delivery},
	}); err != nil {
		ef.log.ErrorContext(ctx, "could not audit email link", "action", action, "user_id", rc.UserID, "error", err)
	}
}

// link builds the console URL carrying the token in its FRAGMENT.
//
// A fragment is never sent to a server: not to nginx (so never in an access
// log), not to the gateway, and not in a Referer header when the page later
// loads anything. A query parameter would be all three.
func (ef *emailFlows) link(purpose store.EmailTokenPurpose, token string) string {
	u := *ef.base
	page := "/verify-email"
	if purpose == store.PurposeResetPassword {
		page = "/reset-password"
	}
	u.Path = ef.base.Path + page
	u.Fragment = "token=" + token
	return u.String()
}

func (ef *emailFlows) compose(rc store.EmailRecipient, purpose store.EmailTokenPurpose, tok store.IssuedEmailToken) mail.Message {
	link := ef.link(purpose, tok.Plaintext)
	name := strings.TrimSpace(rc.DisplayName)
	if name == "" || strings.ContainsAny(name, "\r\n") {
		name = "there"
	}
	lifetime := humanDuration(ef.ttl(purpose))
	if purpose == store.PurposeResetPassword {
		return mail.Message{
			To:      rc.Email,
			Subject: "Reset your KGF Nexus password",
			Text: "Hello " + name + ",\n\n" +
				"Someone asked to reset the password for " + rc.Email + " in KGF Nexus workspace " + rc.Namespace + ".\n\n" +
				"To choose a new password, open this link:\n\n" + link + "\n\n" +
				"The link works once and expires in " + lifetime + ". Resetting your password signs out every existing session.\n\n" +
				"If you did not ask for this, ignore this email. Your password has not been changed.\n",
		}
	}
	return mail.Message{
		To:      rc.Email,
		Subject: "Confirm your email address for KGF Nexus",
		Text: "Hello " + name + ",\n\n" +
			"Confirm that " + rc.Email + " is your address for KGF Nexus workspace " + rc.Namespace + " by opening this link:\n\n" +
			link + "\n\n" +
			"The link works once and expires in " + lifetime + ". You cannot sign in until your address is confirmed.\n\n" +
			"If you did not create this account, ignore this email.\n",
	}
}

func humanDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0 && d >= 24*time.Hour:
		n := int(d / (24 * time.Hour))
		if n == 1 {
			return "24 hours"
		}
		return strconv.Itoa(n) + " days"
	case d%time.Hour == 0:
		n := int(d / time.Hour)
		if n == 1 {
			return "1 hour"
		}
		return strconv.Itoa(n) + " hours"
	default:
		n := int(d.Round(time.Minute) / time.Minute)
		if n == 1 {
			return "1 minute"
		}
		return strconv.Itoa(n) + " minutes"
	}
}

// ── registration hook ────────────────────────────────────────────────────

// pendingVerification is a link minted inside a registration transaction,
// delivered only after that transaction commits.
type pendingVerification struct {
	recipient store.EmailRecipient
	token     store.IssuedEmailToken
}

// issueRegistrationVerification mints the owner's verification link inside the
// registration transaction, so a registration never commits without its token
// and a token never survives a rolled-back registration. Returns nil when mail
// is disabled: there is no point minting a link nobody can receive.
func (a *API) issueRegistrationVerification(ctx context.Context, tx store.GWDB, u store.UserRow, r *http.Request) (*pendingVerification, error) {
	if a.email == nil || !a.email.mailEnabled() {
		return nil, nil
	}
	tok, err := store.GatewayIssueEmailToken(ctx, tx, u.ID, u.Email, store.PurposeVerifyEmail,
		a.email.cfg.VerifyTTL, requesterIP(r), a.email.cfg.MaxLinksPerHour)
	if err != nil {
		return nil, fmt.Errorf("issue verification link: %w", err)
	}
	return &pendingVerification{
		recipient: store.EmailRecipient{UserID: u.ID, Namespace: u.Namespace, Email: u.Email, DisplayName: u.DisplayName, HasPassword: true},
		token:     tok,
	}, nil
}

// dispatchRegistrationVerification queues delivery AFTER commit and reports what
// happened, for the registration response. Honest by construction: "queued" is
// only said when a worker has the message, never when mail is disabled.
func (a *API) dispatchRegistrationVerification(ctx context.Context, pv *pendingVerification) map[string]any {
	if a.email == nil {
		return nil
	}
	out := map[string]any{"required": a.email.cfg.RequireVerified}
	switch {
	case !a.email.mailEnabled():
		out["delivery"] = "mail_disabled"
		a.log.WarnContext(ctx, "registration completed without a verification email: email delivery is not configured")
	case pv == nil:
		out["delivery"] = "not_sent"
	default:
		ef := a.email
		if ef.enqueue(func(ctx context.Context) { ef.deliver(ctx, pv.recipient, store.PurposeVerifyEmail, pv.token) }) {
			out["delivery"] = "queued"
		} else {
			out["delivery"] = "not_queued"
			a.log.WarnContext(ctx, "verification email could not be queued; the owner can request another", "user_id", pv.recipient.UserID)
		}
	}
	return out
}

// ── consume a link ───────────────────────────────────────────────────────

const invalidLinkMessage = "This link is invalid, has expired, or has already been used. Request a new one."

// maxTokenLength bounds a presented token before it is hashed. Generated tokens
// are ~52 characters; anything far longer is not one of ours.
const maxTokenLength = 256

func (a *API) verifyEmail(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.email.allowIP(w, r) {
		return
	}
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || len(token) > maxTokenLength {
		writeError(w, http.StatusBadRequest, "invalid_token", invalidLinkMessage)
		return
	}
	u, err := store.GatewayVerifyEmail(r.Context(), a.db, token, clientIP(r))
	if errors.Is(err, store.ErrEmailTokenInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_token", invalidLinkMessage)
		return
	}
	if err != nil {
		a.log.ErrorContext(r.Context(), "email verification backend failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Verification is temporarily unavailable. Try the link again shortly.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "email": u.Email})
}

func (a *API) resetPasswordWithLink(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || !a.email.allowIP(w, r) {
		return
	}
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" || len(token) > maxTokenLength {
		writeError(w, http.StatusBadRequest, "invalid_token", invalidLinkMessage)
		return
	}
	if len(req.Password) < minPasswordLength || len(req.Password) > 1024 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_password",
			fmt.Sprintf("Use a password of %d to 1024 characters.", minPasswordLength))
		return
	}
	// Cheap existence check first, so a dead link never costs an argon2 hash.
	live, err := store.GatewayEmailTokenLive(r.Context(), a.db, token, store.PurposeResetPassword)
	if err != nil {
		a.log.ErrorContext(r.Context(), "password reset backend failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Password reset is temporarily unavailable. Try again shortly.")
		return
	}
	if !live {
		writeError(w, http.StatusBadRequest, "invalid_token", invalidLinkMessage)
		return
	}
	res, err := store.GatewayResetPassword(r.Context(), a.db, token, req.Password, clientIP(r))
	if errors.Is(err, store.ErrEmailTokenInvalid) {
		writeError(w, http.StatusBadRequest, "invalid_token", invalidLinkMessage)
		return
	}
	if err != nil {
		a.log.ErrorContext(r.Context(), "password reset backend failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Password reset is temporarily unavailable. Try again shortly.")
		return
	}
	a.log.InfoContext(r.Context(), "password reset through email link",
		"user_id", res.User.ID, "sessions_revoked", res.SessionsRevoked)
	writeJSON(w, http.StatusOK, map[string]any{"password_reset": true, "sessions_revoked": res.SessionsRevoked})
}

// ── login hook ───────────────────────────────────────────────────────────

// blockUnverifiedLogin refuses a session to an account whose address is
// unproven, when policy requires it. Called only AFTER the password verified.
func (a *API) blockUnverifiedLogin(w http.ResponseWriter, r *http.Request, user store.UserRow) bool {
	if a.email == nil || !a.email.cfg.RequireVerified || user.EmailVerifiedAt != nil {
		return false
	}
	a.auditAttempt(r, store.AuditEntry{
		Namespace: user.Namespace, ActorID: &user.ID, ActorKind: "user", ActorLabel: user.Email,
		Action: "session.login", Outcome: store.AuditDenied,
		Detail: map[string]any{"reason": "email not verified"},
	})
	writeError(w, http.StatusForbidden, "email_not_verified",
		"Confirm your email address to sign in. Use the link we emailed you, or request a new one.")
	return true
}
