// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Email ownership verification and password recovery tokens (migration 0017).
//
// A token here is a bearer proof delivered by email: whoever holds the link holds
// the mailbox, for one purpose, once, for a bounded time. Every property of that
// sentence is enforced in SQL rather than in Go, for the same reason session
// expiry is (GatewayResolveSession): a predicate in the WHERE clause cannot be
// forgotten by a caller.
//
//   - ONCE: consumption is a single conditional UPDATE ... WHERE consumed_at IS
//     NULL RETURNING. Two concurrent presentations of the same link race on one
//     row lock and exactly one of them gets a row back.
//   - ONE PURPOSE: the purpose is in every lookup, so a verification link cannot
//     reset a password.
//   - BOUNDED TIME: expires_at > now() by the database clock.
//   - THIS ADDRESS: the token records the address it was sent to and is only
//     honoured while the account still has that address.

// EmailTokenPurpose scopes a token to one flow.
type EmailTokenPurpose string

const (
	// PurposeVerifyEmail proves the account controls its address.
	PurposeVerifyEmail EmailTokenPurpose = "verify_email"
	// PurposeResetPassword authorizes setting a new password.
	PurposeResetPassword EmailTokenPurpose = "reset_password"
)

// tokenPrefix names the purpose in the plaintext so a leaked link is
// recognisable to a secret scanner and to the person reading their inbox. It
// grants nothing: the purpose that counts is the one stored with the hash.
func (p EmailTokenPurpose) tokenPrefix() string {
	if p == PurposeResetPassword {
		return "nxreset"
	}
	return "nxverify"
}

func (p EmailTokenPurpose) valid() bool {
	return p == PurposeVerifyEmail || p == PurposeResetPassword
}

// ErrEmailTokenInvalid covers every reason a presented link does not work:
// unknown, expired, already used, superseded, wrong purpose, address changed,
// account disabled. ONE error, because the person holding a dead link can act on
// only one thing -- request a new one -- and distinguishing the cases would tell
// a holder of a stolen link which of them applies.
var ErrEmailTokenInvalid = errors.New("email link is invalid or has expired")

// ErrEmailTokenThrottled means the account already received its allowance of
// links of this purpose in the last hour. It exists to stop the gateway being
// used to flood someone's inbox, and it holds across gateway replicas because it
// is counted in the database rather than in a replica's memory.
var ErrEmailTokenThrottled = errors.New("too many email links requested for this account")

// EmailRecipient is an account a link may be sent to.
type EmailRecipient struct {
	UserID      string
	Namespace   string
	Email       string
	DisplayName string
	Verified    bool
	HasPassword bool
}

// maxRecipientsPerAddress bounds the fan-out of one request. The same address
// can hold an account in several namespaces (MULTITENANCY.md: legacy tenants),
// and each needs its own link, but an unbounded list would let one request make
// the gateway send an unbounded number of mails.
const maxRecipientsPerAddress = 5

// GatewayFindEmailRecipients lists the ENABLED accounts with this address,
// optionally narrowed to one namespace.
//
// Disabled accounts are excluded here rather than at consumption only, so a
// disabled person cannot even trigger a mail -- recovering a disabled account is
// an administrator's decision, not the mailbox holder's.
func GatewayFindEmailRecipients(ctx context.Context, q GWQuerier, namespace, email string) ([]EmailRecipient, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	rows, err := q.Query(ctx, `
		SELECT id::text, namespace, email, display_name,
		       email_verified_at IS NOT NULL, password_hash IS NOT NULL
		  FROM users
		 WHERE lower(email) = $1
		   AND ($2 = '' OR namespace = $2)
		   AND disabled_at IS NULL
		 ORDER BY created_at, id
		 LIMIT $3`, email, namespace, maxRecipientsPerAddress)
	if err != nil {
		return nil, fmt.Errorf("find email recipients: %w", err)
	}
	defer rows.Close()
	var out []EmailRecipient
	for rows.Next() {
		var r EmailRecipient
		if err := rows.Scan(&r.UserID, &r.Namespace, &r.Email, &r.DisplayName, &r.Verified, &r.HasPassword); err != nil {
			return nil, fmt.Errorf("scan email recipient: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate email recipients: %w", err)
	}
	return out, nil
}

// IssuedEmailToken is a freshly minted link token. Plaintext exists only here.
type IssuedEmailToken struct {
	Plaintext string
	ID        string
	ExpiresAt time.Time
}

// GatewayIssueEmailToken mints a token for one account and purpose.
//
// It runs in its own transaction (a savepoint when db is already a transaction,
// as it is during registration), and within it:
//   - locks the account row, so two concurrent requests for the same account
//     serialize and the hourly cap below cannot be raced past;
//   - re-checks the account is enabled and still has this address -- the caller
//     may have looked it up a moment ago, and a stale lookup must not mint a link
//     for an address the account no longer has;
//   - enforces maxPerHour per account and purpose (ErrEmailTokenThrottled);
//   - revokes every outstanding token of the same purpose, so only the newest
//     link works. Otherwise every "resend" would leave another live credential in
//     the person's inbox.
//
// The PLAINTEXT is returned once and never stored.
func GatewayIssueEmailToken(ctx context.Context, db GWDB, userID, email string, purpose EmailTokenPurpose, ttl time.Duration, requestedIP string, maxPerHour int) (IssuedEmailToken, error) {
	if !purpose.valid() {
		return IssuedEmailToken{}, fmt.Errorf("issue email token: unknown purpose %q", purpose)
	}
	if ttl <= 0 {
		return IssuedEmailToken{}, fmt.Errorf("issue email token: ttl must be positive")
	}
	plaintext, err := GenerateSecret(purpose.tokenPrefix())
	if err != nil {
		return IssuedEmailToken{}, err
	}
	if len(requestedIP) > 64 {
		requestedIP = requestedIP[:64]
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return IssuedEmailToken{}, fmt.Errorf("begin issue email token: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var locked string
	err = tx.QueryRow(ctx, `
		SELECT id::text FROM users
		 WHERE id = $1 AND lower(email) = lower($2) AND disabled_at IS NULL
		 FOR UPDATE`, userID, email).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssuedEmailToken{}, ErrNotFound
	}
	if err != nil {
		return IssuedEmailToken{}, fmt.Errorf("lock user %s for email token: %w", userID, err)
	}

	if maxPerHour > 0 {
		var recent int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM email_tokens
			 WHERE user_id = $1 AND purpose = $2 AND created_at > now() - interval '1 hour'`,
			userID, string(purpose)).Scan(&recent); err != nil {
			return IssuedEmailToken{}, fmt.Errorf("count recent email tokens: %w", err)
		}
		if recent >= maxPerHour {
			return IssuedEmailToken{}, ErrEmailTokenThrottled
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE email_tokens SET revoked_at = now()
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND revoked_at IS NULL`,
		userID, string(purpose)); err != nil {
		return IssuedEmailToken{}, fmt.Errorf("supersede email tokens: %w", err)
	}

	out := IssuedEmailToken{Plaintext: plaintext}
	err = tx.QueryRow(ctx, `
		INSERT INTO email_tokens (user_id, purpose, token_hash, email, expires_at, requested_ip)
		VALUES ($1, $2, $3, lower($4), now() + $5::interval, $6)
		RETURNING id::text, expires_at`,
		userID, string(purpose), HashAPIKey(plaintext), email, ttl.String(), requestedIP,
	).Scan(&out.ID, &out.ExpiresAt)
	if err != nil {
		return IssuedEmailToken{}, fmt.Errorf("insert email token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return IssuedEmailToken{}, fmt.Errorf("commit email token: %w", err)
	}
	return out, nil
}

// consumeEmailToken atomically marks a live token of this purpose used and
// returns the account it belongs to, provided that account is enabled and still
// has the address the token was sent to. Must run inside the caller's
// transaction so that a failure after consumption (a password write, an audit
// row) rolls the consumption back too.
func consumeEmailToken(ctx context.Context, tx pgx.Tx, presented string, purpose EmailTokenPurpose) (UserRow, string, error) {
	var tokenID, userID, sentTo string
	err := tx.QueryRow(ctx, `
		UPDATE email_tokens SET consumed_at = now()
		 WHERE token_hash = $1
		   AND purpose = $2
		   AND consumed_at IS NULL
		   AND revoked_at IS NULL
		   AND expires_at > now()
		 RETURNING id::text, user_id::text, email`,
		HashAPIKey(presented), string(purpose),
	).Scan(&tokenID, &userID, &sentTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserRow{}, "", ErrEmailTokenInvalid
	}
	if err != nil {
		return UserRow{}, "", fmt.Errorf("consume email token: %w", err)
	}

	var u UserRow
	err = tx.QueryRow(ctx, `
		SELECT id::text, namespace, email, display_name, role, created_at, disabled_at, email_verified_at
		  FROM users
		 WHERE id = $1 AND lower(email) = $2 AND disabled_at IS NULL
		 FOR UPDATE`, userID, sentTo,
	).Scan(&u.ID, &u.Namespace, &u.Email, &u.DisplayName, &u.Role, &u.CreatedAt, &u.DisabledAt, &u.EmailVerifiedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// The address changed or the account was disabled after the link was sent.
		// The caller rolls back, so the token is not marked used by this attempt;
		// it can never succeed anyway, because the same check will fail again.
		return UserRow{}, "", ErrEmailTokenInvalid
	}
	if err != nil {
		return UserRow{}, "", fmt.Errorf("load user for email token: %w", err)
	}
	return u, tokenID, nil
}

// GatewayEmailTokenLive reports whether a presented token is currently usable for
// purpose, without consuming it.
//
// Exists so the reset path can refuse a garbage or dead link BEFORE spending an
// argon2 hash on the new password: without it, an anonymous caller could make the
// gateway run the KDF once per request for free. It is only a pre-check -- the
// consuming UPDATE is still the authority, so a race between the two is harmless.
func GatewayEmailTokenLive(ctx context.Context, q GWQuerier, presented string, purpose EmailTokenPurpose) (bool, error) {
	var live bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM email_tokens
			 WHERE token_hash = $1 AND purpose = $2
			   AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > now())`,
		HashAPIKey(presented), string(purpose)).Scan(&live)
	if err != nil {
		return false, fmt.Errorf("check email token: %w", err)
	}
	return live, nil
}

// GatewayVerifyEmail consumes a verification token and marks the address proven.
//
// Idempotent in effect: an already-verified account keeps its original
// verification time (COALESCE), so a second, older link cannot rewrite history --
// though issuing a new link revokes the old one, so this is belt and braces.
func GatewayVerifyEmail(ctx context.Context, db GWDB, presented, clientIP string) (UserRow, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return UserRow{}, fmt.Errorf("begin verify email: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	u, tokenID, err := consumeEmailToken(ctx, tx, presented, PurposeVerifyEmail)
	if err != nil {
		return UserRow{}, err
	}
	if err := tx.QueryRow(ctx, `
		UPDATE users SET email_verified_at = COALESCE(email_verified_at, now())
		 WHERE id = $1
		 RETURNING email_verified_at`, u.ID).Scan(&u.EmailVerifiedAt); err != nil {
		return UserRow{}, fmt.Errorf("mark email verified for user %s: %w", u.ID, err)
	}
	// Audited in the SAME transaction: a verification with no audit row, or an
	// audit row for a verification that rolled back, would both be lies.
	if err := GatewayAppendAudit(ctx, tx, AuditEntry{
		Namespace: u.Namespace, ActorID: &u.ID, ActorKind: "user", ActorLabel: u.Email,
		Action: "user.email_verified", TargetKind: "user", TargetID: u.ID,
		Outcome: AuditSuccess, ClientIP: clientIP,
		Detail: map[string]any{"email_token_id": tokenID},
	}); err != nil {
		return UserRow{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return UserRow{}, fmt.Errorf("commit verify email: %w", err)
	}
	return u, nil
}

// PasswordResetResult reports what a reset changed.
type PasswordResetResult struct {
	User            UserRow
	SessionsRevoked int64
}

// GatewayResetPassword consumes a reset token and sets a new password.
//
// Everything a reset implies commits together or not at all:
//   - the new verifier (argon2id, via HashPassword -- the one hashing path);
//   - EVERY session the account holds is revoked. A reset is what someone does
//     when they believe their password is known to someone else, and a reset that
//     leaves that someone's session alive has not locked them out;
//   - every other outstanding reset link for the account is revoked;
//   - the address is marked verified if it was not: following a link mailed to
//     it is exactly the proof verification asks for;
//   - an audit row.
//
// The password is hashed BEFORE the transaction opens, so the argon2 cost is not
// paid while holding the token and account row locks.
func GatewayResetPassword(ctx context.Context, db GWDB, presented, newPassword, clientIP string) (PasswordResetResult, error) {
	hash, err := HashPassword(newPassword)
	if err != nil {
		return PasswordResetResult{}, fmt.Errorf("reset password: %w", err)
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return PasswordResetResult{}, fmt.Errorf("begin reset password: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	u, tokenID, err := consumeEmailToken(ctx, tx, presented, PurposeResetPassword)
	if err != nil {
		return PasswordResetResult{}, err
	}
	if err := tx.QueryRow(ctx, `
		UPDATE users
		   SET password_hash = $2,
		       email_verified_at = COALESCE(email_verified_at, now())
		 WHERE id = $1
		 RETURNING email_verified_at`, u.ID, hash).Scan(&u.EmailVerifiedAt); err != nil {
		return PasswordResetResult{}, fmt.Errorf("set password for user %s: %w", u.ID, err)
	}
	revoked, err := GatewayRevokeUserSessions(ctx, tx, u.ID)
	if err != nil {
		return PasswordResetResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE email_tokens SET revoked_at = now()
		 WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL AND revoked_at IS NULL`,
		u.ID, string(PurposeResetPassword)); err != nil {
		return PasswordResetResult{}, fmt.Errorf("revoke other reset links for user %s: %w", u.ID, err)
	}
	if err := GatewayAppendAudit(ctx, tx, AuditEntry{
		Namespace: u.Namespace, ActorID: &u.ID, ActorKind: "user", ActorLabel: u.Email,
		Action: "user.password_reset", TargetKind: "user", TargetID: u.ID,
		Outcome: AuditSuccess, ClientIP: clientIP,
		Detail: map[string]any{"email_token_id": tokenID, "sessions_revoked": revoked, "via": "email_link"},
	}); err != nil {
		return PasswordResetResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PasswordResetResult{}, fmt.Errorf("commit reset password: %w", err)
	}
	return PasswordResetResult{User: u, SessionsRevoked: revoked}, nil
}

// GatewayDeleteExpiredEmailTokens removes tokens that expired before `before`.
//
// A dead token has no evidentiary value the audit log does not hold (issuance,
// verification and resets are all audited), so it is deleted rather than kept.
// Bounded per call, like the session sweep, so the sweep is a series of short
// statements rather than one long lock.
func GatewayDeleteExpiredEmailTokens(ctx context.Context, q GWQuerier, before time.Time, limit int) (int64, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	tag, err := q.Exec(ctx, `
		DELETE FROM email_tokens
		 WHERE id IN (SELECT id FROM email_tokens WHERE expires_at < $1 LIMIT $2)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired email tokens: %w", err)
	}
	return tag.RowsAffected(), nil
}
