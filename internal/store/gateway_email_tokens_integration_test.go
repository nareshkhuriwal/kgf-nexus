//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Integration coverage for email verification and password-reset tokens
// (migration 0017). These pin properties that only Postgres can demonstrate: that
// consumption is single-use under concurrency, that expiry is judged by the
// database clock, that a changed address un-verifies an account (a trigger), and
// that a reset revokes sessions in the same transaction as the password write.
package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// emailTestUser registers a tenant for a fresh namespace and creates one user in
// it. Tenant registration is required since 0014: users.tenant_id is assigned
// from the namespace's tenant by trigger.
func emailTestUser(t *testing.T, ctx context.Context, pool GWDB, email string) UserRow {
	t.Helper()
	ns := uniqueNS(t, pool)
	u, err := GatewayCreateUser(ctx, pool, ns, email, "Email Test", "admin", "original-password-123")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM email_tokens WHERE user_id = $1`, u.ID)
		_, _ = pool.Exec(c, `DELETE FROM sessions WHERE user_id = $1`, u.ID)
		_, _ = pool.Exec(c, `DELETE FROM audit_log WHERE namespace = $1`, ns)
		_, _ = pool.Exec(c, `DELETE FROM users WHERE id = $1`, u.ID)
		_, _ = pool.Exec(c, `DELETE FROM tenants WHERE namespace = $1`, ns)
	})
	return u
}

func uniqueEmail(t *testing.T) string {
	return strings.ToLower(uniqueName(t)) + "@example.test"
}

func isVerified(t *testing.T, ctx context.Context, pool GWQuerier, userID string) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(ctx, `SELECT email_verified_at IS NOT NULL FROM users WHERE id = $1`, userID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIntegrationNewAccountsStartUnverified(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))
	if isVerified(t, ctx, pool, u.ID) {
		t.Fatal("an account created after 0017 must start unverified")
	}
	got, err := GatewayAuthenticatePassword(ctx, pool, u.Namespace, u.Email, "original-password-123")
	if err != nil || got.EmailVerifiedAt != nil {
		t.Fatalf("authenticate must report the unverified state: %+v %v", got, err)
	}
}

func TestIntegrationVerifyEmailIsSingleUseAndHashedAtRest(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))

	tok, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeVerifyEmail, time.Hour, "203.0.113.9", 5)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.HasPrefix(tok.Plaintext, "nxverify_") {
		t.Errorf("plaintext %q lacks its purpose prefix", tok.Plaintext)
	}
	// Only the hash is stored; nothing in the row equals the plaintext.
	var stored string
	var plaintextStored bool
	if err := pool.QueryRow(ctx, `SELECT token_hash, EXISTS(SELECT 1 FROM email_tokens WHERE token_hash = $2 OR email = $2) FROM email_tokens WHERE id = $1`,
		tok.ID, tok.Plaintext).Scan(&stored, &plaintextStored); err != nil {
		t.Fatal(err)
	}
	if stored != HashAPIKey(tok.Plaintext) || plaintextStored {
		t.Fatal("token must be stored as its sha256 only")
	}

	got, err := GatewayVerifyEmail(ctx, pool, tok.Plaintext, "203.0.113.9")
	if err != nil || got.ID != u.ID || got.EmailVerifiedAt == nil {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if !isVerified(t, ctx, pool, u.ID) {
		t.Fatal("account not verified")
	}
	if _, err := GatewayVerifyEmail(ctx, pool, tok.Plaintext, ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("reuse returned %v, want ErrEmailTokenInvalid", err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE namespace=$1 AND action='user.email_verified' AND target_id=$2`, u.Namespace, u.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows = %d (%v), want 1", audits, err)
	}
}

func TestIntegrationEmailTokenPurposeIsEnforced(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))

	verify, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeVerifyEmail, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayResetPassword(ctx, pool, verify.Plaintext, "attacker-chosen-password", ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("verification link reset a password: %v", err)
	}
	if _, err := GatewayVerifyEmail(ctx, pool, reset.Plaintext, ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("reset link verified an address: %v", err)
	}
	// Neither failed attempt consumed anything: both links still work for their
	// own purpose.
	if _, err := GatewayVerifyEmail(ctx, pool, verify.Plaintext, ""); err != nil {
		t.Fatalf("verify after wrong-purpose attempt: %v", err)
	}
	if _, err := GatewayResetPassword(ctx, pool, reset.Plaintext, "a-new-password-456", ""); err != nil {
		t.Fatalf("reset after wrong-purpose attempt: %v", err)
	}
}

func TestIntegrationEmailTokenExpiresByDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))
	tok, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, 50*time.Millisecond, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if live, err := GatewayEmailTokenLive(ctx, pool, tok.Plaintext, PurposeResetPassword); err != nil || live {
		t.Fatalf("expired token reported live=%v err=%v", live, err)
	}
	if _, err := GatewayResetPassword(ctx, pool, tok.Plaintext, "a-new-password-456", ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("expired token reset a password: %v", err)
	}
	n, err := GatewayDeleteExpiredEmailTokens(ctx, pool, time.Now().Add(time.Minute), 100)
	if err != nil || n < 1 {
		t.Fatalf("sweep deleted %d (%v)", n, err)
	}
}

func TestIntegrationNewTokenSupersedesOldAndIssuanceIsThrottled(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))

	first, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeVerifyEmail, time.Hour, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeVerifyEmail, time.Hour, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeVerifyEmail, time.Hour, "", 2); !errors.Is(err, ErrEmailTokenThrottled) {
		t.Fatalf("third issue in an hour with cap 2 returned %v", err)
	}
	// The cap is per purpose: a reset link is still available.
	if _, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 2); err != nil {
		t.Fatalf("reset issuance blocked by verification cap: %v", err)
	}
	if _, err := GatewayVerifyEmail(ctx, pool, first.Plaintext, ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("superseded link still works: %v", err)
	}
	if _, err := GatewayVerifyEmail(ctx, pool, second.Plaintext, ""); err != nil {
		t.Fatalf("newest link: %v", err)
	}
}

func TestIntegrationAddressChangeInvalidatesLinksAndVerification(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))
	tok, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeVerifyEmail, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayVerifyEmail(ctx, pool, tok.Plaintext, ""); err != nil {
		t.Fatal(err)
	}
	reset, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, u.ID, uniqueEmail(t)+".moved"); err != nil {
		t.Fatal(err)
	}
	if isVerified(t, ctx, pool, u.ID) {
		t.Fatal("changing the address must un-verify the account (0017 trigger)")
	}
	if _, err := GatewayResetPassword(ctx, pool, reset.Plaintext, "a-new-password-456", ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("a link sent to the OLD address reset the password: %v", err)
	}
	// A case-only change is the same address and must not un-verify.
	var addr string
	_ = pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, u.ID).Scan(&addr)
	tok2, err := GatewayIssueEmailToken(ctx, pool, u.ID, addr, PurposeVerifyEmail, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GatewayVerifyEmail(ctx, pool, tok2.Plaintext, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET email = upper(email) WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if !isVerified(t, ctx, pool, u.ID) {
		t.Fatal("a case-only change un-verified the account")
	}
}

func TestIntegrationDisabledAccountsGetNoLinks(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))
	tok, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if rs, err := GatewayFindEmailRecipients(ctx, pool, "", u.Email); err != nil || len(rs) != 0 {
		t.Fatalf("disabled account offered as a recipient: %+v %v", rs, err)
	}
	if _, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issued a link to a disabled account: %v", err)
	}
	if _, err := GatewayResetPassword(ctx, pool, tok.Plaintext, "a-new-password-456", ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Fatalf("disabled account reset its password: %v", err)
	}
}

func TestIntegrationResetRevokesEverySessionAndAudits(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))

	var sessions []string
	for i := 0; i < 2; i++ {
		plain, _, err := GatewayCreateSession(ctx, pool, u.ID, time.Hour, "test", "")
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, plain)
	}
	tok, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	res, err := GatewayResetPassword(ctx, pool, tok.Plaintext, "brand-new-password-789", "198.51.100.7")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if res.SessionsRevoked != 2 {
		t.Errorf("sessions revoked = %d, want 2", res.SessionsRevoked)
	}
	for _, s := range sessions {
		if _, err := GatewayResolveSession(ctx, pool, s); !errors.Is(err, ErrIdentityUnknown) {
			t.Errorf("session survived a password reset: %v", err)
		}
	}
	if _, err := GatewayAuthenticatePassword(ctx, pool, u.Namespace, u.Email, "original-password-123"); !errors.Is(err, ErrIdentityUnknown) {
		t.Error("old password still works")
	}
	if _, err := GatewayAuthenticatePassword(ctx, pool, u.Namespace, u.Email, "brand-new-password-789"); err != nil {
		t.Errorf("new password: %v", err)
	}
	// Following a link sent to the address proves the address.
	if !isVerified(t, ctx, pool, u.ID) {
		t.Error("reset through the mailbox did not verify the address")
	}
	if _, err := GatewayResetPassword(ctx, pool, tok.Plaintext, "third-password-000", ""); !errors.Is(err, ErrEmailTokenInvalid) {
		t.Errorf("reset link reused: %v", err)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail::text FROM audit_log WHERE namespace=$1 AND action='user.password_reset' AND client_ip='198.51.100.7'`, u.Namespace).Scan(&detail); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if strings.Contains(detail, tok.Plaintext) || strings.Contains(detail, "brand-new") {
		t.Fatalf("audit detail leaked secret material: %s", detail)
	}
}

// TestIntegrationConcurrentConsumptionHasOneWinner: two tabs, one link. Exactly
// one presentation may succeed; the conditional UPDATE is what guarantees it.
func TestIntegrationConcurrentConsumptionHasOneWinner(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	u := emailTestUser(t, ctx, pool, uniqueEmail(t))
	tok, err := GatewayIssueEmailToken(ctx, pool, u.ID, u.Email, PurposeResetPassword, time.Hour, "", 5)
	if err != nil {
		t.Fatal(err)
	}
	const racers = 6
	var wg sync.WaitGroup
	results := make(chan error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := GatewayResetPassword(ctx, pool, tok.Plaintext, "raced-password-0000", "")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrEmailTokenInvalid):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent consumptions succeeded, want exactly 1", wins)
	}
}

func TestIntegrationFindRecipientsNarrowsByNamespace(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	email := uniqueEmail(t)
	a := emailTestUser(t, ctx, pool, email)
	b := emailTestUser(t, ctx, pool, email)
	all, err := GatewayFindEmailRecipients(ctx, pool, "", strings.ToUpper(email))
	if err != nil || len(all) != 2 {
		t.Fatalf("recipients = %+v (%v), want both namespaces", all, err)
	}
	one, err := GatewayFindEmailRecipients(ctx, pool, b.Namespace, email)
	if err != nil || len(one) != 1 || one[0].UserID != b.ID || one[0].Verified || !one[0].HasPassword {
		t.Fatalf("namespace filter: %+v (%v)", one, err)
	}
	_ = a
}
