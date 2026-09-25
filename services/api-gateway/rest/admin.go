// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// Identity and administration endpoints: session login/logout, user
// administration, scoped tokens, and the audit trail.
//
// Every route here is registered through guard() exactly like every other route,
// with ONE deliberate exception: login cannot be, because a caller logging in has
// no credential yet. That exception is handled explicitly in Routes() and is the
// only unguarded mutating endpoint in the gateway -- which is why it carries its
// own rate limit and its own audit record of every failure.

// IdentityAdmin is the identity surface the admin routes need.
//
// An interface, like WorkflowManager and ResourceManagerAPI above it, so the REST
// layer is testable without Postgres and so it is visible exactly which identity
// operations the HTTP surface can perform.
type IdentityAdmin interface {
	AuthenticatePassword(ctx context.Context, namespace, email, password string) (store.UserRow, error)
	CreateSession(ctx context.Context, userID string, ttl time.Duration, userAgent, clientIP string) (string, store.SessionRow, error)
	RevokeSession(ctx context.Context, sessionID string) error
	RevokeUserSessions(ctx context.Context, userID string) (int64, error)

	CreateUser(ctx context.Context, namespace, email, displayName, role, password string) (store.UserRow, error)
	ListUsers(ctx context.Context, namespace string, limit int) ([]store.UserRow, error)
	GetUser(ctx context.Context, namespace, userID string) (store.UserRow, error)
	SetUserDisabled(ctx context.Context, namespace, userID string, disabled bool) (store.UserRow, error)
	SetUserPassword(ctx context.Context, userID, password string) error
	ListUserSessions(ctx context.Context, userID string, limit int) ([]store.SessionRow, error)

	CreateScopedToken(ctx context.Context, namespace, name, role string, scopes []string, expiresAt *time.Time, createdBy *string) (string, store.ScopedTokenRow, error)
	ListScopedTokens(ctx context.Context, namespace string, limit int) ([]store.ScopedTokenRow, error)
	RevokeScopedToken(ctx context.Context, namespace, tokenID string) error

	AppendAudit(ctx context.Context, e store.AuditEntry) error
	ListAudit(ctx context.Context, namespace string, f store.AuditFilter) ([]store.AuditRow, error)
}

// WithIdentity enables the identity and admin routes.
//
// Opt-in and nil-safe: a gateway built without it serves exactly the route set it
// served before, so the feature can be wired per-deployment rather than in a flag
// day. Routes() checks this and simply does not register the routes when absent,
// which is better than registering handlers that 501 -- an endpoint that exists
// but never works is harder to diagnose than one that is not there.
func (a *API) WithIdentity(id IdentityAdmin) *API {
	a.identity = id
	return a
}

// maxLoginBytes bounds a login body. Small deliberately: a credential pair is a
// few hundred bytes, and the endpoint is unauthenticated, so the body limit is
// part of its abuse surface.
const maxLoginBytes = 4 << 10

// minPasswordLength is the shortest password accepted.
//
// Length is the only password rule enforced. Composition rules ("one symbol, one
// digit") measurably push people toward predictable substitutions and away from
// length, which is the property that actually resists an offline attack -- this
// follows NIST SP 800-63B rather than habit.
const minPasswordLength = 12

// ── login and logout ─────────────────────────────────────────────────────

// login exchanges an email and password for a session token.
//
// THE ONE UNGUARDED ROUTE. A caller logging in has no credential yet, so guard()
// cannot wrap it -- and because it is unguarded it is the endpoint most worth
// attacking. Three things compensate:
//   - the per-address limiter runs first, and every rejected password spends
//     the address's stricter auth-failure budget; once that is empty the address
//     gets 429 before its guess is even checked, so a credential-stuffing run is
//     held to a handful of attempts a minute (ratelimit.go);
//   - every failure is ONE opaque 401 with no hint about which half was wrong;
//   - every attempt, successful or not, lands in the audit log.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	if a.rateLimited(w, r) || a.authAttemptBlocked(w, r) {
		return
	}
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}

	var req struct {
		Namespace string `json:"namespace"`
		Email     string `json:"email"`
		Password  string `json:"password"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.Email = strings.TrimSpace(req.Email)

	user, err := a.identity.AuthenticatePassword(r.Context(), req.Namespace, req.Email, req.Password)
	if err != nil {
		if errors.Is(err, store.ErrIdentityUnknown) {
			a.recordAuthFailure(r, req.Namespace+"\x00"+req.Email+"\x00"+req.Password)
			// Recorded as DENIED so a burst of these is visible to an operator. The
			// email is recorded because an admin reviewing an attack needs to know
			// which accounts were targeted; the password never is.
			if req.Namespace != "" {
				a.auditAttempt(r, store.AuditEntry{
					Namespace: req.Namespace, ActorKind: "anonymous", ActorLabel: req.Email,
					Action: "session.login", Outcome: store.AuditDenied,
					Detail: map[string]any{"reason": "invalid credentials"},
				})
			} else {
				a.log.WarnContext(r.Context(), "email sign-in denied")
			}
			// One opaque 401. Never "no such user" or "wrong password": the pair would
			// let an attacker enumerate who has an account here.
			writeError(w, http.StatusUnauthorized, "unauthenticated", "invalid email or password")
			return
		}
		// A database fault is 503, not 401 -- the same rule authn.go states. A 401
		// here would tell every console user their password stopped working.
		a.log.ErrorContext(r.Context(), "login backend failed", "namespace", req.Namespace, "error", err)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "authentication is temporarily unavailable")
		return
	}

	// After the password check, never before: only someone who knows the
	// password learns the account is unverified.
	if a.blockUnverifiedLogin(w, r, user) {
		return
	}

	token, session, err := a.identity.CreateSession(r.Context(), user.ID,
		authn.SessionTTL, r.UserAgent(), clientIP(r))
	if err != nil {
		a.log.ErrorContext(r.Context(), "could not create session", "user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not start a session")
		return
	}

	a.auditAttempt(r, store.AuditEntry{
		Namespace: user.Namespace, ActorID: &user.ID, ActorKind: "user", ActorLabel: user.Email,
		Action: "session.login", TargetKind: "session", TargetID: session.ID,
		Outcome: store.AuditSuccess,
	})

	// The token is returned exactly ONCE, in this body, and is never recoverable.
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"expires_at": session.ExpiresAt,
		"user": map[string]any{
			"id": user.ID, "email": user.Email, "display_name": user.DisplayName,
			"namespace": user.Namespace, "role": user.Role,
		},
	})
}

// logout ends the CALLER'S OWN session.
//
// It takes no session id: a caller may only end the session it presented. Letting
// a request name an arbitrary session would make logout a way to forcibly evict
// another user -- mass revocation exists for that and is an admin operation.
func (a *API) logout(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	if p.Kind != authn.KindSession {
		// An API key has no session to end. Refusing plainly is more honest than
		// pretending to log out a credential that does not work that way.
		writeError(w, http.StatusBadRequest, "not_a_session",
			"this credential is not a session; only a logged-in session can be logged out")
		return
	}
	if err := a.identity.RevokeSession(r.Context(), p.KeyID); err != nil {
		a.fail(w, r, "revoke session", err)
		return
	}
	a.auditPrincipal(r, p, "session.logout", "session", p.KeyID, store.AuditSuccess, nil)
	writeJSON(w, http.StatusOK, map[string]any{"logged_out": true})
}

// whoami reports the calling principal.
//
// The console needs it to decide what to render: an operator who cannot drain a
// worker should not be shown a drain button that will 403. It deliberately
// returns the SAME shape for every credential kind so the UI has one code path.
func (a *API) whoami(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	kind := p.Kind
	if kind == "" {
		kind = authn.KindAPIKey
	}
	// Report the effective permissions rather than the role name alone: the UI must
	// not re-derive the authorization matrix, because a second copy of it is a
	// second opinion that will eventually disagree with the enforcement point.
	perms := map[string]bool{}
	for _, act := range []authn.Action{authn.ActionRead, authn.ActionWrite, authn.ActionAdminWrite} {
		perms[string(act)] = p.Authorize(p.Namespace, act) == nil
	}

	if p.Grants != nil {
		perms["read"] = false
		perms["write"] = false
		perms["admin_write"] = false
		for grant, allowed := range p.Grants {
			if allowed {
				if strings.HasSuffix(grant, ".view") {
					perms["read"] = true
				} else {
					perms["write"] = true
				}
			}
		}
	}
	out := map[string]any{
		"namespace":             p.Namespace,
		"name":                  p.Name,
		"role":                  string(p.Role),
		"kind":                  string(kind),
		"permissions":           perms,
		"effective_permissions": effectivePermissions(p),
		"roles":                 p.RoleNames,
	}
	if p.UserID != "" {
		out["user_id"] = p.UserID
	}
	if p.Scopes != nil {
		scopes := make([]string, 0, len(p.Scopes))
		for _, s := range p.Scopes {
			scopes = append(scopes, string(s))
		}
		out["scopes"] = scopes
	}
	writeJSON(w, http.StatusOK, out)
}

// ── users ────────────────────────────────────────────────────────────────

func (a *API) listUsers(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := a.identity.ListUsers(r.Context(), r.PathValue("ns"), intParam(r, "limit", 100))
	if err != nil {
		a.fail(w, r, "list users", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, u := range rows {
		out = append(out, userPayload(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// createUser adds a human account.
//
// The password is supplied by the admin creating the account and is never
// generated and mailed: this deployment has no mail path, and inventing one that
// silently drops a password would be worse than requiring the admin to convey it.
func (a *API) createUser(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns := r.PathValue("ns")
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Role        string `json:"role"`
		RoleName    string `json:"role_name"`
		Password    string `json:"password"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.RoleName != "" {
		req.Role = "reader"
		if req.RoleName == authn.AdministratorRole {
			req.Role = "admin"
		}
	}
	req.Role = orDefault(strings.TrimSpace(req.Role), string(authn.RoleReader))

	if problems := validateNewUser(req.Email, req.Role, req.Password); len(problems) > 0 {
		// 422, matching submitSpec: the HTTP and JSON were fine, the DOCUMENT is not.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "invalid_user", "message": "user could not be created", "problems": problems,
		})
		return
	}

	if req.RoleName != "" {
		a.createUserWithRole(w, r, p, req.Email, req.DisplayName, req.RoleName, req.Password)
		return
	}
	user, err := a.identity.CreateUser(r.Context(), ns, req.Email, req.DisplayName, req.Role, req.Password)
	if err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			a.auditPrincipal(r, p, "user.create", "user", req.Email, store.AuditDenied,
				map[string]any{"reason": "email already registered"})
			// 409 is returned to an ADMIN who can already list users, so it leaks
			// nothing they cannot read directly -- and hiding it would make the
			// failure undiagnosable.
			writeError(w, http.StatusConflict, "email_taken", "that email already has an account in this namespace")
			return
		}
		a.fail(w, r, "create user", err)
		return
	}

	a.auditPrincipal(r, p, "user.create", "user", user.ID, store.AuditSuccess,
		map[string]any{"email": user.Email, "role": user.Role})
	writeJSON(w, http.StatusCreated, userPayload(user))
}

// validateNewUser returns every problem at once rather than the first.
//
// A form that reveals one error per round trip is a form people fill in four
// times; the spec endpoint already reports problems as a list for the same reason.
func validateNewUser(email, role, password string) []string {
	var problems []string
	if email == "" {
		problems = append(problems, "email is required")
	} else if !strings.Contains(email, "@") || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") {
		problems = append(problems, "email must contain a local part and a domain")
	}
	switch authn.Role(role) {
	case authn.RoleAdmin, authn.RoleReader:
	case authn.RoleWorker:
		// The worker role is for execution hosts, not people (authn.go). A human
		// holding it would be a console that can poll for tasks.
		problems = append(problems, "role 'worker' is for machine credentials and cannot be given to a user")
	default:
		problems = append(problems, fmt.Sprintf("role %q is not one of admin, reader", role))
	}
	if len(password) < minPasswordLength {
		problems = append(problems, fmt.Sprintf("password must be at least %d characters", minPasswordLength))
	}
	return problems
}

// setUserDisabled disables or re-enables an account.
//
// Disabling also revokes every session the user holds, atomically, in the store.
// The response reports that so the operator is not left wondering whether the
// person is actually out.
func (a *API) setUserDisabled(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	disabled := !strings.HasSuffix(r.URL.Path, "/enable")

	if disabled && p.UserID != "" && p.UserID == id {
		// Refusing self-disable is not paternalism: an admin who locks themselves out
		// of the only admin account leaves a namespace with no way back in, and this
		// deployment has no out-of-band recovery path.
		writeError(w, http.StatusConflict, "cannot_disable_self",
			"you cannot disable your own account; ask another admin")
		return
	}

	user, err := a.identity.SetUserDisabled(r.Context(), ns, id, disabled)
	if errors.Is(err, store.ErrLastAdministrator) {
		writeError(w, 409, "last_administrator", err.Error())
		return
	}
	if err != nil {
		a.fail(w, r, "set user disabled", err)
		return
	}
	action := "user.enable"
	if disabled {
		action = "user.disable"
	}
	a.auditPrincipal(r, p, action, "user", user.ID, store.AuditSuccess,
		map[string]any{"email": user.Email})
	writeJSON(w, http.StatusOK, userPayload(user))
}

// resetUserPassword sets a new password and ends every session that account holds.
//
// The revocation is the point. A password reset that leaves existing sessions
// alive does not lock out whoever prompted the reset, which is usually the entire
// reason it is being done.
func (a *API) resetUserPassword(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	if len(req.Password) < minPasswordLength {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "invalid_password", "message": "password could not be set",
			"problems": []string{fmt.Sprintf("password must be at least %d characters", minPasswordLength)},
		})
		return
	}

	// Confirm the user is in this namespace BEFORE touching the password: the store
	// sets by user id alone, so the tenancy check has to happen here or an admin in
	// tenant A could reset a password in tenant B by guessing a UUID.
	user, err := a.identity.GetUser(r.Context(), ns, id)
	if err != nil {
		a.fail(w, r, "get user", err)
		return
	}
	if err := a.identity.SetUserPassword(r.Context(), user.ID, req.Password); err != nil {
		a.fail(w, r, "set password", err)
		return
	}
	revoked, err := a.identity.RevokeUserSessions(r.Context(), user.ID)
	if err != nil {
		// The password IS changed at this point. Reporting success would hide live
		// sessions that still hold the old credential, so this is an error the
		// operator must see and act on.
		a.log.ErrorContext(r.Context(), "password reset but sessions not revoked",
			"user_id", user.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "partial_reset",
			"the password was changed but existing sessions could not be revoked; revoke them explicitly")
		return
	}

	a.auditPrincipal(r, p, "user.password_reset", "user", user.ID, store.AuditSuccess,
		map[string]any{"email": user.Email, "sessions_revoked": revoked})
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": user.ID, "sessions_revoked": revoked,
	})
}

// listUserSessions shows where an account is currently logged in.
func (a *API) listUserSessions(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	// Namespace-scoped existence check first, so sessions are not enumerable across
	// tenants by user UUID.
	if _, err := a.identity.GetUser(r.Context(), ns, id); err != nil {
		a.fail(w, r, "get user", err)
		return
	}
	rows, err := a.identity.ListUserSessions(r.Context(), id, intParam(r, "limit", 50))
	if err != nil {
		a.fail(w, r, "list sessions", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, map[string]any{
			"id": s.ID, "created_at": s.CreatedAt, "expires_at": s.ExpiresAt,
			"revoked_at": s.RevokedAt, "last_used_at": s.LastUsedAt,
			"user_agent": s.UserAgent, "client_ip": s.ClientIP,
			// Derived server-side so every client agrees what "live" means rather
			// than each re-implementing the expiry comparison.
			"live": s.RevokedAt == nil && s.ExpiresAt.After(time.Now()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

// revokeUserSessions ends every session an account holds.
func (a *API) revokeUserSessions(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	user, err := a.identity.GetUser(r.Context(), ns, id)
	if err != nil {
		a.fail(w, r, "get user", err)
		return
	}
	n, err := a.identity.RevokeUserSessions(r.Context(), user.ID)
	if err != nil {
		a.fail(w, r, "revoke sessions", err)
		return
	}
	a.auditPrincipal(r, p, "user.sessions_revoked", "user", user.ID, store.AuditSuccess,
		map[string]any{"email": user.Email, "sessions_revoked": n})
	writeJSON(w, http.StatusOK, map[string]any{"user_id": user.ID, "sessions_revoked": n})
}

func userPayload(u store.UserRow) map[string]any {
	return map[string]any{
		"id": u.ID, "namespace": u.Namespace, "email": u.Email,
		"display_name": u.DisplayName, "role": u.Role,
		"created_at": u.CreatedAt, "disabled_at": u.DisabledAt,
		// Derived rather than left to the client, so "is this person active" has one
		// definition. A null disabled_at is easy to misread as "unknown".
		"disabled": u.DisabledAt != nil,
		// Whether the address is proven (migration 0017). Derived for the same
		// reason as "disabled"; an admin needs to see why a person cannot sign in.
		"email_verified": u.EmailVerifiedAt != nil,
	}
}

// ── scoped tokens ────────────────────────────────────────────────────────

func (a *API) listTokens(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := a.identity.ListScopedTokens(r.Context(), r.PathValue("ns"), intParam(r, "limit", 100))
	if err != nil {
		a.fail(w, r, "list tokens", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, tk := range rows {
		out = append(out, tokenPayload(tk))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

// createToken mints a scoped token and returns the secret EXACTLY ONCE.
func (a *API) createToken(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns := r.PathValue("ns")
	body, err := readBody(r, maxLoginBytes)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", err.Error())
		return
	}
	var req struct {
		Name          string   `json:"name"`
		Role          string   `json:"role"`
		Scopes        []string `json:"scopes"`
		ExpiresInDays int      `json:"expires_in_days"`
	}
	if err := json.Unmarshal(nonEmptyJSON(body), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Role = orDefault(strings.TrimSpace(req.Role), string(authn.RoleReader))

	problems := validateNewToken(req.Name, req.Role, req.Scopes)
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "invalid_token_request", "message": "token could not be created", "problems": problems,
		})
		return
	}

	// An expiry is the default, not the exception: an unbounded credential is the
	// thing scoped tokens exist to replace. Zero means "use the default" rather
	// than "never expires", so forgetting the field yields a bounded token.
	days := req.ExpiresInDays
	if days == 0 {
		days = defaultTokenDays
	}
	var expiresAt *time.Time
	if days > 0 {
		t := time.Now().UTC().AddDate(0, 0, days)
		expiresAt = &t
	}
	// A negative value is the explicit "no expiry" escape hatch, kept because some
	// long-lived integrations genuinely need it; it is deliberately awkward to ask
	// for so it cannot happen by accident.

	var createdBy *string
	if p.UserID != "" {
		createdBy = &p.UserID
	}

	secret, row, err := a.identity.CreateScopedToken(r.Context(), ns, req.Name, req.Role, req.Scopes, expiresAt, createdBy)
	if err != nil {
		a.fail(w, r, "create token", err)
		return
	}

	// The audit row records WHAT was granted but never the secret itself.
	a.auditPrincipal(r, p, "token.create", "token", row.ID, store.AuditSuccess,
		map[string]any{"name": row.Name, "role": row.Role, "scopes": row.Scopes, "expires_at": row.ExpiresAt})

	payload := tokenPayload(row)
	// Shown once and never recoverable. The field name says so, because a client
	// that stores the response wholesale should at least have been told.
	payload["token"] = secret
	payload["token_shown_once"] = true
	writeJSON(w, http.StatusCreated, payload)
}

// defaultTokenDays bounds a token created without an explicit lifetime. Ninety
// days is a quarter: long enough not to be a constant chore, short enough that a
// forgotten credential expires within one planning cycle.
const defaultTokenDays = 90

func validateNewToken(name, role string, scopes []string) []string {
	var problems []string
	if name == "" {
		problems = append(problems, "name is required, so the token can be recognised later")
	}
	switch authn.Role(role) {
	case authn.RoleAdmin, authn.RoleReader, authn.RoleWorker:
	default:
		problems = append(problems, fmt.Sprintf("role %q is not one of admin, reader, worker", role))
	}
	if len(scopes) == 0 {
		// An empty scope list authorizes nothing, so creating one is almost always a
		// mistake. Rejecting it at the edge is kinder than minting a token that
		// mysteriously 403s on every call.
		problems = append(problems, "at least one scope is required; a token with no scopes can do nothing")
	}
	known := map[string]bool{
		string(authn.ActionRead): true, string(authn.ActionWrite): true,
		string(authn.ActionAdminWrite): true, string(authn.ActionWorkerRPC): true,
	}
	for _, s := range scopes {
		if !known[strings.TrimSpace(s)] {
			problems = append(problems, fmt.Sprintf("scope %q is not one of read, write, admin_write, worker_rpc", s))
		}
	}
	return problems
}

func (a *API) revokeToken(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	ns, id := r.PathValue("ns"), r.PathValue("id")
	if err := a.identity.RevokeScopedToken(r.Context(), ns, id); err != nil {
		a.fail(w, r, "revoke token", err)
		return
	}
	a.auditPrincipal(r, p, "token.revoke", "token", id, store.AuditSuccess, nil)
	writeJSON(w, http.StatusOK, map[string]any{"token_id": id, "revoked": true})
}

func tokenPayload(t store.ScopedTokenRow) map[string]any {
	return map[string]any{
		"id": t.ID, "namespace": t.Namespace, "name": t.Name,
		"scopes": t.Scopes, "role": t.Role,
		"expires_at": t.ExpiresAt, "last_used_at": t.LastUsedAt,
		"created_by": t.CreatedBy, "created_at": t.CreatedAt, "revoked_at": t.RevokedAt,
		// Derived server-side: "can this credential be used right now" has one
		// answer, and it is the one the authenticator's query computes.
		"live": t.RevokedAt == nil && (t.ExpiresAt == nil || t.ExpiresAt.After(time.Now())),
	}
}

// ── audit ────────────────────────────────────────────────────────────────

func (a *API) listAudit(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	rows, err := a.identity.ListAudit(r.Context(), r.PathValue("ns"), store.AuditFilter{
		Action:  r.URL.Query().Get("action"),
		ActorID: r.URL.Query().Get("actor_id"),
		Outcome: r.URL.Query().Get("outcome"),
		Limit:   intParam(r, "limit", 100),
	})
	if err != nil {
		a.fail(w, r, "list audit", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, e := range rows {
		out = append(out, map[string]any{
			"id": e.ID, "namespace": e.Namespace,
			"actor_id": e.ActorID, "actor_kind": e.ActorKind, "actor_label": e.ActorLabel,
			"action": e.Action, "target_kind": e.TargetKind, "target_id": e.TargetID,
			"outcome": e.Outcome, "detail": rawJSON(e.Detail),
			"client_ip": e.ClientIP, "occurred_at": e.OccurredAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out})
}

// ── audit helpers ────────────────────────────────────────────────────────

// auditPrincipal records an action taken by an authenticated caller.
//
// A failure to write the audit row is LOGGED, not returned. That is a deliberate
// trade and worth stating: refusing an operation because its audit record could
// not be written would mean a database hiccup blocks administration during
// exactly the incident when an operator needs it. The log line is the fallback
// record. The alternative -- failing closed -- is defensible for a compliance
// system, and if this platform ever needs that, this is the single place to change.
func (a *API) auditPrincipal(r *http.Request, p authn.Principal, action, targetKind, targetID, outcome string, detail map[string]any) {
	kind := string(p.Kind)
	if kind == "" {
		kind = string(authn.KindAPIKey)
	}
	e := store.AuditEntry{
		Namespace: p.Namespace, ActorKind: kind, ActorLabel: p.Name,
		Action: action, TargetKind: targetKind, TargetID: targetID,
		Outcome: outcome, Detail: detail, ClientIP: clientIP(r),
	}
	if p.UserID != "" {
		e.ActorID = &p.UserID
	}
	a.auditAttempt(r, e)
}

func (a *API) auditAttempt(r *http.Request, e store.AuditEntry) {
	if a.identity == nil {
		return
	}
	if e.ClientIP == "" {
		e.ClientIP = clientIP(r)
	}
	if err := a.identity.AppendAudit(r.Context(), e); err != nil {
		a.log.ErrorContext(r.Context(), "could not write audit record",
			"action", e.Action, "outcome", e.Outcome, "error", err)
	}
}

// clientIP reports the caller's address for the audit trail.
//
// X-Forwarded-For is honoured because the gateway runs behind a load balancer in
// every real deployment, and RemoteAddr there is the proxy. It is recorded as
// EVIDENCE ONLY and never used for an authorization decision: the header is
// client-supplied and trivially forged, so trusting it for access control would
// be a vulnerability, while recording a possibly-forged value in an audit row an
// operator interprets is merely a caveat.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
