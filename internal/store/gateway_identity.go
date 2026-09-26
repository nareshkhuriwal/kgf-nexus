// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrIdentityUnknown means no live principal matched a presented credential.
//
// ONE error for every distinct failure -- no such user, wrong password, disabled
// account, expired session, revoked token -- because the caller must not be able
// to tell them apart. gateway_apikeys.go makes the same argument for
// ErrAPIKeyUnknown: a login that distinguishes "no such account" from "wrong
// password" is an oracle for enumerating who has an account here.
var ErrIdentityUnknown = errors.New("identity not recognized")

// ErrEmailTaken means the namespace already has an account with this email. This
// one IS distinguishable, and deliberately so: it is returned to an authenticated
// ADMIN creating a user, not to an anonymous caller, so it leaks nothing an admin
// could not read directly, and hiding it would make the admin UI unusable.
var ErrEmailTaken = errors.New("email already registered in this namespace")

// UserRow is a human account. The password verifier is deliberately NOT a field:
// nothing outside the password-checking query needs it, so it cannot be logged or
// serialised into an API response by accident.
type UserRow struct {
	ID          string
	Namespace   string
	Email       string
	DisplayName string
	Role        string
	CreatedAt   time.Time
	DisabledAt  *time.Time
	// EmailVerifiedAt is when the account proved it controls Email (migration
	// 0017). Nil means unproven. Populated by the reads that feed a decision or a
	// display -- password authentication, GetUser and ListUsers -- and left nil by
	// writes that return the row they just inserted, where it is nil anyway.
	EmailVerifiedAt *time.Time
}

// SessionRow is a live browser session.
type SessionRow struct {
	ID         string
	UserID     string
	ExpiresAt  time.Time
	CreatedAt  time.Time
	RevokedAt  *time.Time
	UserAgent  string
	ClientIP   string
	LastUsedAt *time.Time
}

// ScopedTokenRow is a narrow machine credential. Like UserRow, it carries no
// secret: the plaintext exists only in the moment of creation.
type ScopedTokenRow struct {
	ID         string
	Namespace  string
	Name       string
	Scopes     []string
	Role       string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedBy  *string
	CreatedAt  time.Time
	RevokedAt  *time.Time
	// ServiceAccountID and Permissions are migration 0032's additions; nil for
	// a pre-0032 token.
	ServiceAccountID *string
	Permissions      []string
}

// ── credential generation ────────────────────────────────────────────────

// tokenBytes is the entropy in a generated session token or scoped token.
//
// 32 bytes is what makes plain SHA-256 the right at-rest hash for these
// credentials rather than a KDF (the argument in gateway_apikeys.go): at this
// size there is no dictionary and no feasible brute force, so the KDF would buy
// nothing and cost a round of argon2 on every authenticated request.
const tokenBytes = 32

// GenerateSecret mints a new high-entropy credential with a readable prefix.
//
// The prefix is not decoration: it lets a secret scanner recognise a leaked
// credential in a commit or a log and lets an operator tell a session token from
// a scoped token at a glance without being able to derive either.
//
// Returned to the caller ONCE. Nothing can re-derive it from the stored hash,
// which is the property that makes a leak of these tables non-catastrophic.
func GenerateSecret(prefix string) (string, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate %s secret: %w", prefix, err)
	}
	// URL-safe and unpadded so the value survives a header, a query string and a
	// copy-paste without being re-encoded into something that no longer matches.
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// isUniqueViolation reports whether an error is Postgres' unique-violation.
//
// Named rather than inlined (wfm_runs.go and wfm_specs.go each compare the code
// in place) because this file needs the check to distinguish a duplicate EMAIL
// from a genuine database fault: one is a 409 the admin UI must explain, the
// other is a 500. Collapsing them would report an outage as "email taken".
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ── users ────────────────────────────────────────────────────────────────

// GatewayCreateUser creates a human account with a password.
//
// The password is hashed with argon2id here rather than by the caller, so there
// is exactly one place a plaintext password can enter the database and it is a
// place that cannot forget to hash it.
func GatewayCreateUser(ctx context.Context, q GWQuerier, namespace, email, displayName, role, password string) (UserRow, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return UserRow{}, fmt.Errorf("create user: %w", err)
	}
	return gatewayInsertUser(ctx, q, namespace, email, displayName, role, &hash)
}

// GatewayCreateUserWithoutPassword creates an account that cannot log in by
// password, which is the correct initial state for a user who will authenticate
// through an external identity provider. Separate from GatewayCreateUser so that
// creating a passwordless account is always deliberate and never the result of an
// empty string reaching the hasher.
func GatewayCreateUserWithoutPassword(ctx context.Context, q GWQuerier, namespace, email, displayName, role string) (UserRow, error) {
	return gatewayInsertUser(ctx, q, namespace, email, displayName, role, nil)
}

func gatewayInsertUser(ctx context.Context, q GWQuerier, namespace, email, displayName, role string, hash *string) (UserRow, error) {
	// Normalised on the way in so the unique index on lower(email) and every
	// subsequent lookup agree about what this address is.
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return UserRow{}, fmt.Errorf("create user: %w", errors.New("email must not be empty"))
	}

	var u UserRow
	err := q.QueryRow(ctx, `
		INSERT INTO users (namespace, email, display_name, role, password_hash)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, namespace, email, display_name, role, created_at, disabled_at`,
		namespace, email, displayName, role, hash,
	).Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt)
	if err != nil {
		if isUniqueViolation(err) {
			return UserRow{}, fmt.Errorf("create user %s: %w", email, ErrEmailTaken)
		}
		return UserRow{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

// GatewayAuthenticatePassword verifies an email and password and returns the user.
//
// It returns ErrIdentityUnknown for every failure mode, and it performs the
// argon2 verification even when no user was found, against a dummy verifier. That
// second part is not paranoia: without it, a missing account returns in
// microseconds while a real one takes ~50ms of KDF, and that difference is
// directly measurable over a network. The timing would enumerate every account in
// the namespace regardless of how carefully the error strings are matched.
func GatewayAuthenticatePassword(ctx context.Context, q GWQuerier, namespace, email, password string) (UserRow, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	// Email-only sign-in resolves a unique enabled account; never pick an
	// arbitrary tenant when legacy accounts share an address.
	if namespace == "" {
		rows, err := q.Query(ctx, `SELECT namespace FROM users WHERE lower(email)=$1 AND disabled_at IS NULL LIMIT 2`, email)
		if err != nil {
			return UserRow{}, fmt.Errorf("resolve sign-in workspace: %w", err)
		}
		namespaces := []string{}
		for rows.Next() {
			var ns string
			if err = rows.Scan(&ns); err != nil {
				rows.Close()
				return UserRow{}, err
			}
			namespaces = append(namespaces, ns)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return UserRow{}, err
		}
		if len(namespaces) != 1 {
			_ = VerifyPassword(dummyVerifier, password)
			return UserRow{}, ErrIdentityUnknown
		}
		namespace = namespaces[0]
	}

	var u UserRow
	var hash *string
	err := q.QueryRow(ctx, `
		SELECT id::text, namespace, email, display_name, role, created_at, disabled_at, password_hash, email_verified_at
		  FROM users
		 WHERE namespace = $1 AND lower(email) = $2`,
		namespace, email,
	).Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt, &hash, &u.EmailVerifiedAt)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Spend the same work a real verification would, then fail. The dummy hash
		// is a constant with the current parameters, so the cost matches.
		_ = VerifyPassword(dummyVerifier, password)
		return UserRow{}, ErrIdentityUnknown
	case err != nil:
		// A database fault is NOT an authentication failure: it must surface as 503,
		// not 401 (authn.go). Wrapping rather than collapsing to ErrIdentityUnknown
		// is what preserves that distinction for the caller.
		return UserRow{}, fmt.Errorf("lookup user: %w", err)
	}

	if u.DisabledAt != nil || hash == nil {
		// A disabled account and an account with no password both burn the same work
		// before failing, so neither is distinguishable from a wrong password.
		_ = VerifyPassword(dummyVerifier, password)
		return UserRow{}, ErrIdentityUnknown
	}
	if verr := VerifyPassword(*hash, password); verr != nil {
		if errors.Is(verr, ErrPasswordHashMalformed) {
			// A corrupt verifier is a data fault, not a credential decision. Reporting
			// it as "wrong password" would hide a real integrity problem behind a
			// message that sends an operator to reset a password that was fine.
			return UserRow{}, fmt.Errorf("verify password for user %s: %w", u.ID, verr)
		}
		return UserRow{}, ErrIdentityUnknown
	}
	return u, nil
}

// dummyVerifier is a real argon2id hash of a value nothing can present, used to
// equalise the timing of a failed lookup. Generated once at init with the current
// parameters so it costs exactly what a genuine verification costs.
var dummyVerifier = mustDummyVerifier()

func mustDummyVerifier() string {
	h, err := HashPassword("dummy-verifier-never-matches-any-real-password")
	if err != nil {
		// Unreachable unless crypto/rand is broken, in which case nothing in this
		// package can be trusted and failing loudly at startup is correct.
		panic(fmt.Sprintf("cannot build password timing verifier: %v", err))
	}
	return h
}

// GatewaySetUserPassword replaces a user's password verifier.
//
// Used both by an admin reset and by the automatic re-hash when parameters are
// raised (PasswordNeedsRehash), which is why it takes a plaintext rather than a
// hash: the hashing decision stays in one place.
func GatewaySetUserPassword(ctx context.Context, q GWQuerier, userID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	tag, err := q.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash)
	if err != nil {
		return fmt.Errorf("set password for user %s: %w", userID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set password for user %s: %w", userID, ErrNotFound)
	}
	return nil
}

// GatewayListUsers lists accounts in a namespace, newest first.
func GatewayListUsers(ctx context.Context, q GWQuerier, namespace string, limit int) ([]UserRow, error) {
	limit = ClampLimit(limit, DefaultUserList, MaxUserList)
	rows, err := q.Query(ctx, `
		SELECT id::text, namespace, email, display_name, role, created_at, disabled_at, email_verified_at
		  FROM users
		 WHERE namespace = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, namespace, limit)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	out := make([]UserRow, 0, 16)
	for rows.Next() {
		var u UserRow
		if err := rows.Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt, &u.EmailVerifiedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return out, nil
}

// GatewayGetUser reads one account, scoped to a namespace so a user in tenant A
// is not readable by id from tenant B.
func GatewayGetUser(ctx context.Context, q GWQuerier, namespace, userID string) (UserRow, error) {
	var u UserRow
	err := q.QueryRow(ctx, `
		SELECT id::text, namespace, email, display_name, role, created_at, disabled_at, email_verified_at
		  FROM users
		 WHERE namespace = $1 AND id = $2`, namespace, userID,
	).Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt, &u.EmailVerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("get user %s: %w", userID, err)
	}
	return u, nil
}

// GatewaySetUserDisabled disables or re-enables an account and, when disabling,
// revokes every session it holds IN THE SAME TRANSACTION.
//
// The atomicity is the point. Disabling an account while leaving its sessions
// alive would mean a person who was just deactivated keeps full access until
// their token happens to expire -- which is exactly the window an offboarding
// exists to close. The two facts must commit together or the guarantee is empty.
var ErrLastAdministrator = errors.New("cannot disable the last active administrator")

func GatewaySetUserDisabled(ctx context.Context, db GWDB, namespace, userID string, disabled bool) (UserRow, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return UserRow{}, fmt.Errorf("begin disable user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, namespace); err != nil {
		return UserRow{}, err
	}
	if disabled {
		var last bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE namespace=$1 AND id=$2 AND role='admin' AND disabled_at IS NULL) AND (SELECT count(*) FROM users WHERE namespace=$1 AND role='admin' AND disabled_at IS NULL)<=1`, namespace, userID).Scan(&last)
		if err != nil {
			return UserRow{}, err
		}
		if last {
			return UserRow{}, ErrLastAdministrator
		}
	}
	var at *time.Time
	if disabled {
		now := time.Now().UTC()
		at = &now
	}

	var u UserRow
	err = tx.QueryRow(ctx, `
		UPDATE users SET disabled_at = $3
		 WHERE namespace = $1 AND id = $2
		 RETURNING id::text, namespace, email, display_name, role, created_at, disabled_at`,
		namespace, userID, at,
	).Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRow{}, ErrNotFound
	}
	if err != nil {
		return UserRow{}, fmt.Errorf("disable user %s: %w", userID, err)
	}

	if disabled {
		if _, err := tx.Exec(ctx, `
			UPDATE sessions SET revoked_at = now()
			 WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
			return UserRow{}, fmt.Errorf("revoke sessions for user %s: %w", userID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return UserRow{}, fmt.Errorf("commit disable user %s: %w", userID, err)
	}
	return u, nil
}

// ── sessions ─────────────────────────────────────────────────────────────

// ResolvedSession is what a presented session token resolves to: the session and
// the human behind it, in one round trip. Combined because authentication needs
// both on every request and two queries would double the cost of the hot path.
type ResolvedSession struct {
	SessionID string
	User      UserRow
	ExpiresAt time.Time
}

// GatewayCreateSession mints a session token for a user and stores its hash.
//
// Returns the PLAINTEXT token exactly once. It is never stored, never logged and
// cannot be recovered -- the caller must hand it to the browser in this response
// or it is lost, which is the same contract as an API key.
func GatewayCreateSession(ctx context.Context, q GWQuerier, userID string, ttl time.Duration, userAgent, clientIP string) (plaintext string, row SessionRow, err error) {
	plaintext, err = GenerateSecret("nxsess")
	if err != nil {
		return "", SessionRow{}, err
	}
	// User-agent strings are attacker-controlled and unbounded; truncating here
	// keeps a hostile client from writing a megabyte per login into a table an
	// admin screen renders.
	if len(userAgent) > 400 {
		userAgent = userAgent[:400]
	}

	err = q.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, expires_at, user_agent, client_ip)
		VALUES ($1, $2, now() + $3::interval, $4, $5)
		RETURNING id::text, user_id::text, expires_at, created_at, revoked_at, user_agent, client_ip, last_used_at`,
		userID, HashAPIKey(plaintext), ttl.String(), userAgent, clientIP,
	).Scan(&row.ID, &row.UserID, &row.ExpiresAt, &row.CreatedAt, &row.RevokedAt, &row.UserAgent, &row.ClientIP, &row.LastUsedAt)
	if err != nil {
		return "", SessionRow{}, fmt.Errorf("create session: %w", err)
	}
	return plaintext, row, nil
}

// GatewayResolveSession authenticates a presented session token.
//
// Expiry, revocation and the user's disabled flag are all evaluated IN THE QUERY
// rather than in Go. A row that fails any of them simply does not match, so there
// is no path on which a caller forgets one of the three checks -- the same
// reasoning as api_keys' `revoked_at IS NULL` predicate.
//
// Expiry is judged by the DATABASE clock (now()), never the gateway's, because
// gateway replicas are not clock-synchronised with each other and a session must
// not be alive on one replica and dead on another (invariant 7's principle).
func GatewayResolveSession(ctx context.Context, q GWQuerier, presented string) (ResolvedSession, error) {
	var rs ResolvedSession
	err := q.QueryRow(ctx, `
		SELECT s.id::text, s.expires_at,
		       u.id::text, u.namespace, u.email, u.display_name, u.role, u.created_at, u.disabled_at
		  FROM sessions s
		  JOIN users u ON u.id = s.user_id
		 WHERE s.token_hash = $1
		   AND s.revoked_at IS NULL
		   AND s.expires_at > now()
		   AND u.disabled_at IS NULL`,
		HashAPIKey(presented),
	).Scan(&rs.SessionID, &rs.ExpiresAt,
		&rs.User.ID, &rs.User.Namespace, &rs.User.Email, &rs.User.DisplayName,
		&rs.User.Role, &rs.User.CreatedAt, &rs.User.DisabledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedSession{}, ErrIdentityUnknown
	}
	if err != nil {
		return ResolvedSession{}, fmt.Errorf("resolve session: %w", err)
	}
	return rs, nil
}

// GatewayTouchSession records that a session was used.
//
// Best-effort and COARSE on purpose: it only writes when the recorded time is
// more than touchInterval old. Writing on every request would turn every
// authenticated read into a write, putting a row update and its WAL on the
// hot path of a console that polls every 4 seconds -- which is a real cost for a
// field whose only consumer is a human asking "is this session still in use".
func GatewayTouchSession(ctx context.Context, q GWQuerier, sessionID string) error {
	_, err := q.Exec(ctx, `
		UPDATE sessions SET last_used_at = now()
		 WHERE id = $1
		   AND (last_used_at IS NULL OR last_used_at < now() - $2::interval)`,
		sessionID, touchInterval.String())
	if err != nil {
		return fmt.Errorf("touch session %s: %w", sessionID, err)
	}
	return nil
}

// touchInterval bounds how stale last_used_at may be. Five minutes is far finer
// than any offboarding decision needs and coarse enough to make the write rare.
const touchInterval = 5 * time.Minute

// GatewayRevokeSession ends one session (logout).
//
// Revoking rather than deleting keeps the row for the audit trail: "this session
// was ended at 14:02" is evidence, and a deleted row is silence.
func GatewayRevokeSession(ctx context.Context, q GWQuerier, sessionID string) error {
	tag, err := q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		 WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	if err != nil {
		return fmt.Errorf("revoke session %s: %w", sessionID, err)
	}
	if tag.RowsAffected() == 0 {
		// Already revoked or never existed. Idempotent: logging out twice is not an
		// error, and reporting one would make a retried logout look like a failure.
		return nil
	}
	return nil
}

// GatewayRevokeUserSessions ends every session a user holds, returning how many.
//
// The offboarding and "lost my laptop" control. Returning the count is what lets
// an admin screen say "3 sessions ended" rather than leaving the operator
// wondering whether anything happened.
func GatewayRevokeUserSessions(ctx context.Context, q GWQuerier, userID string) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, fmt.Errorf("revoke sessions for user %s: %w", userID, err)
	}
	return tag.RowsAffected(), nil
}

// GatewayListUserSessions lists a user's sessions, live ones first.
func GatewayListUserSessions(ctx context.Context, q GWQuerier, userID string, limit int) ([]SessionRow, error) {
	limit = ClampLimit(limit, DefaultSessionPage, MaxSessionPage)
	rows, err := q.Query(ctx, `
		SELECT id::text, user_id::text, expires_at, created_at, revoked_at, user_agent, client_ip, last_used_at
		  FROM sessions
		 WHERE user_id = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := make([]SessionRow, 0, 8)
	for rows.Next() {
		var s SessionRow
		if err := rows.Scan(&s.ID, &s.UserID, &s.ExpiresAt, &s.CreatedAt, &s.RevokedAt,
			&s.UserAgent, &s.ClientIP, &s.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sessions: %w", err)
	}
	return out, nil
}

// GatewayDeleteExpiredSessions removes sessions that expired before `before`.
//
// A DELETE rather than a soft flag: an expired session has no evidentiary value
// that the audit log does not already hold, and this table would otherwise grow
// without bound at one row per login forever. Bounded by `limit` so the sweep is
// a series of short transactions rather than one lock-heavy statement.
func GatewayDeleteExpiredSessions(ctx context.Context, q GWQuerier, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	tag, err := q.Exec(ctx, `
		DELETE FROM sessions
		 WHERE id IN (SELECT id FROM sessions WHERE expires_at < $1 LIMIT $2)`,
		before, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ── scoped tokens ────────────────────────────────────────────────────────

// ResolvedToken is what a presented scoped token resolves to.
type ResolvedToken struct {
	TokenID   string
	Namespace string
	Name      string
	Role      string
	Scopes    []string
	// ServiceAccountID is set when the token belongs to a service account
	// (migration 0032). Its grants then come from the account's role bindings.
	ServiceAccountID string
	// Permissions is the token's explicit ceiling, nil when it has none. A
	// non-nil empty slice is a ceiling of nothing.
	Permissions []string
}

// GatewayCreateScopedToken mints a scoped token, returning the plaintext once.
//
// An empty scope list is stored as an empty array, which grants NOTHING once the
// authorizer intersects it with the role. Failing closed matters here: a token
// created with a typo'd scope must be useless rather than accidentally broad.
func GatewayCreateScopedToken(ctx context.Context, q GWQuerier, namespace, name, role string, scopes []string, expiresAt *time.Time, createdBy *string) (plaintext string, row ScopedTokenRow, err error) {
	return GatewayCreateScopedTokenWith(ctx, q, TokenSpec{Namespace: namespace, Name: name, Role: role, Scopes: scopes, ExpiresAt: expiresAt, CreatedBy: createdBy})
}

// TokenSpec describes a scoped token to mint.
type TokenSpec struct {
	Namespace        string
	Name             string
	Role             string
	Scopes           []string
	ExpiresAt        *time.Time
	CreatedBy        *string
	ServiceAccountID *string
	// Permissions is the explicit ceiling; nil stores SQL NULL (no ceiling).
	Permissions []string
}

// GatewayCreateScopedTokenWith mints a token, optionally owned by a service
// account and carrying a permission ceiling (migration 0032).
func GatewayCreateScopedTokenWith(ctx context.Context, q GWQuerier, spec TokenSpec) (plaintext string, row ScopedTokenRow, err error) {
	namespace, name, role, scopes, expiresAt, createdBy := spec.Namespace, spec.Name, spec.Role, spec.Scopes, spec.ExpiresAt, spec.CreatedBy
	var perms []byte
	if spec.Permissions != nil {
		if perms, err = json.Marshal(spec.Permissions); err != nil {
			return "", ScopedTokenRow{}, fmt.Errorf("encode permissions: %w", err)
		}
	}
	plaintext, err = GenerateSecret("nxtok")
	if err != nil {
		return "", ScopedTokenRow{}, err
	}
	if scopes == nil {
		scopes = []string{}
	}
	encoded, err := json.Marshal(scopes)
	if err != nil {
		return "", ScopedTokenRow{}, fmt.Errorf("encode scopes: %w", err)
	}

	var rawScopes, rawPerms []byte
	err = q.QueryRow(ctx, `
		INSERT INTO scoped_tokens (namespace, name, token_hash, scopes, role, expires_at, created_by, service_account_id, permissions)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8, $9::jsonb)
		RETURNING id::text, namespace, name, scopes, role, expires_at, last_used_at,
		          created_by::text, created_at, revoked_at, service_account_id::text, permissions`,
		namespace, name, HashAPIKey(plaintext), encoded, role, expiresAt, createdBy, spec.ServiceAccountID, perms,
	).Scan(&row.ID, &row.Namespace, &row.Name, &rawScopes, &row.Role, &row.ExpiresAt,
		&row.LastUsedAt, &row.CreatedBy, &row.CreatedAt, &row.RevokedAt, &row.ServiceAccountID, &rawPerms)
	if err != nil {
		return "", ScopedTokenRow{}, fmt.Errorf("create scoped token: %w", err)
	}
	if err := json.Unmarshal(rawScopes, &row.Scopes); err != nil {
		return "", ScopedTokenRow{}, fmt.Errorf("decode scopes: %w", err)
	}
	if rawPerms != nil {
		if err := json.Unmarshal(rawPerms, &row.Permissions); err != nil {
			return "", ScopedTokenRow{}, fmt.Errorf("decode permissions: %w", err)
		}
	}
	return plaintext, row, nil
}

// GatewayResolveScopedToken authenticates a presented scoped token.
//
// Revocation and expiry are predicates in the query, and expiry uses the DATABASE
// clock, for the same reasons as GatewayResolveSession. A NULL expires_at means
// no expiry, which is what `OR expires_at > now()` expresses.
func GatewayResolveScopedToken(ctx context.Context, q GWQuerier, presented string) (ResolvedToken, error) {
	var rt ResolvedToken
	var rawScopes, rawPerms []byte
	var saID *string
	// A service-account token is live only while its account is enabled: the
	// join is the revocation, so disabling the account needs no token sweep.
	err := q.QueryRow(ctx, `
		SELECT t.id::text, t.namespace, t.name, t.role, t.scopes, t.service_account_id::text, t.permissions
		  FROM scoped_tokens t
		  LEFT JOIN service_accounts sa ON sa.id = t.service_account_id
		 WHERE t.token_hash = $1
		   AND t.revoked_at IS NULL
		   AND (t.expires_at IS NULL OR t.expires_at > now())
		   AND (t.service_account_id IS NULL OR (sa.id IS NOT NULL AND sa.disabled_at IS NULL))`,
		HashAPIKey(presented),
	).Scan(&rt.TokenID, &rt.Namespace, &rt.Name, &rt.Role, &rawScopes, &saID, &rawPerms)
	if saID != nil {
		rt.ServiceAccountID = *saID
	}
	if err == nil && rawPerms != nil {
		rt.Permissions = []string{}
		if err = json.Unmarshal(rawPerms, &rt.Permissions); err != nil {
			return ResolvedToken{}, fmt.Errorf("decode permissions for token %s: %w", rt.TokenID, err)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedToken{}, ErrIdentityUnknown
	}
	if err != nil {
		return ResolvedToken{}, fmt.Errorf("resolve scoped token: %w", err)
	}
	if err := json.Unmarshal(rawScopes, &rt.Scopes); err != nil {
		// A token whose scopes cannot be decoded must not authenticate: an
		// undecodable scope list is indistinguishable from an empty one, and
		// guessing would be guessing about authorization.
		return ResolvedToken{}, fmt.Errorf("decode scopes for token %s: %w", rt.TokenID, err)
	}
	return rt, nil
}

// GatewayTouchScopedToken records use, on the same coarse cadence and for the
// same reason as GatewayTouchSession.
func GatewayTouchScopedToken(ctx context.Context, q GWQuerier, tokenID string) error {
	_, err := q.Exec(ctx, `
		UPDATE scoped_tokens SET last_used_at = now()
		 WHERE id = $1
		   AND (last_used_at IS NULL OR last_used_at < now() - $2::interval)`,
		tokenID, touchInterval.String())
	if err != nil {
		return fmt.Errorf("touch scoped token %s: %w", tokenID, err)
	}
	return nil
}

// GatewayListScopedTokens lists a namespace's tokens, newest first.
func GatewayListScopedTokens(ctx context.Context, q GWQuerier, namespace string, limit int) ([]ScopedTokenRow, error) {
	limit = ClampLimit(limit, DefaultTokenPage, MaxTokenPage)
	rows, err := q.Query(ctx, `
		SELECT id::text, namespace, name, scopes, role, expires_at, last_used_at,
		       created_by::text, created_at, revoked_at, service_account_id::text, permissions
		  FROM scoped_tokens
		 WHERE namespace = $1
		 ORDER BY created_at DESC
		 LIMIT $2`, namespace, limit)
	if err != nil {
		return nil, fmt.Errorf("list scoped tokens: %w", err)
	}
	defer rows.Close()

	out := make([]ScopedTokenRow, 0, 16)
	for rows.Next() {
		var t ScopedTokenRow
		var rawScopes, rawPerms []byte
		if err := rows.Scan(&t.ID, &t.Namespace, &t.Name, &rawScopes, &t.Role, &t.ExpiresAt,
			&t.LastUsedAt, &t.CreatedBy, &t.CreatedAt, &t.RevokedAt, &t.ServiceAccountID, &rawPerms); err != nil {
			return nil, fmt.Errorf("scan scoped token: %w", err)
		}
		if rawPerms != nil {
			if err := json.Unmarshal(rawPerms, &t.Permissions); err != nil {
				return nil, fmt.Errorf("decode permissions for token %s: %w", t.ID, err)
			}
		}
		if err := json.Unmarshal(rawScopes, &t.Scopes); err != nil {
			return nil, fmt.Errorf("decode scopes for token %s: %w", t.ID, err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scoped tokens: %w", err)
	}
	return out, nil
}

// GatewayRevokeScopedToken revokes a token, scoped to its namespace so a token in
// tenant A cannot be revoked by id from tenant B.
func GatewayRevokeScopedToken(ctx context.Context, q GWQuerier, namespace, tokenID string) error {
	tag, err := q.Exec(ctx, `
		UPDATE scoped_tokens SET revoked_at = now()
		 WHERE namespace = $1 AND id = $2 AND revoked_at IS NULL`, namespace, tokenID)
	if err != nil {
		return fmt.Errorf("revoke scoped token %s: %w", tokenID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// The audit log lives in gateway_audit.go.
