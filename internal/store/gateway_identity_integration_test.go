//go:build integration

// Integration coverage for the identity tables. These assert what a fake cannot:
// that the SQL is valid, that the unique index on lower(email) really folds case,
// that expiry and revocation are evaluated by the DATABASE clock inside the
// query, and that the append-only audit grant behaves as migration 0007 claims.
package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// cleanupUser removes a user and everything that references it.
func cleanupUser(t *testing.T, ctx context.Context, pool GWQuerier, userID string) {
	t.Helper()
	t.Cleanup(func() {
		// audit_log has no FK to users (actor_id is deliberately unconstrained so a
		// deleted user's history survives), so it is cleaned explicitly.
		_, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE actor_id = $1`, userID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})
}

func TestIntegrationUserCreateAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)
	const pw = "a-real-password-123"

	u, err := GatewayCreateUser(ctx, pool, ns, "Alice@Example.COM", "Alice", "admin", pw)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	// Email is normalised on the way in, so the stored value is canonical.
	if u.Email != "alice@example.com" {
		t.Errorf("stored email is %q, want it lowercased", u.Email)
	}

	// Login is case-insensitive in the address, which is what the unique index on
	// lower(email) exists to guarantee.
	for _, presented := range []string{"alice@example.com", "ALICE@EXAMPLE.COM", "Alice@Example.com"} {
		got, err := GatewayAuthenticatePassword(ctx, pool, ns, presented, pw)
		if err != nil {
			t.Errorf("authenticate with %q: %v", presented, err)
			continue
		}
		if got.ID != u.ID {
			t.Errorf("authenticate with %q resolved to user %s, want %s", presented, got.ID, u.ID)
		}
	}

	// A wrong password fails, and fails as ErrIdentityUnknown rather than as
	// anything that distinguishes it from an unknown account.
	if _, err := GatewayAuthenticatePassword(ctx, pool, ns, "alice@example.com", "wrong"); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("wrong password returned %v, want ErrIdentityUnknown", err)
	}
}

// TestIntegrationLoginIsNotAnAccountOracle is the security property authn.go
// insists on, asserted at the store layer where the decision is actually made.
func TestIntegrationLoginIsNotAnAccountOracle(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "real@example.com", "Real", "reader", "the-right-password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	_, errNoSuchUser := GatewayAuthenticatePassword(ctx, pool, ns, "ghost@example.com", "whatever")
	_, errWrongPassword := GatewayAuthenticatePassword(ctx, pool, ns, "real@example.com", "whatever")

	if !errors.Is(errNoSuchUser, ErrIdentityUnknown) || !errors.Is(errWrongPassword, ErrIdentityUnknown) {
		t.Fatalf("both failures must be ErrIdentityUnknown; got no-such-user=%v wrong-password=%v",
			errNoSuchUser, errWrongPassword)
	}
	// The STRINGS must match too: a caller that logs err.Error() would otherwise
	// leak the distinction straight into an operator-visible channel.
	if errNoSuchUser.Error() != errWrongPassword.Error() {
		t.Errorf("failure messages differ and so identify which accounts exist:\n  no such user:   %q\n  wrong password: %q",
			errNoSuchUser.Error(), errWrongPassword.Error())
	}
}

// TestIntegrationDisabledUserCannotAuthenticate covers offboarding at the point
// where it must hold regardless of what the gateway remembers.
func TestIntegrationDisabledUserCannotAuthenticate(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)
	const pw = "still-a-valid-password"

	u, err := GatewayCreateUser(ctx, pool, ns, "leaver@example.com", "Leaver", "admin", pw)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	// A live session exists before the account is disabled.
	plaintext, _, err := GatewayCreateSession(ctx, pool, u.ID, time.Hour, "test-agent", "127.0.0.1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := GatewayResolveSession(ctx, pool, plaintext); err != nil {
		t.Fatalf("session should resolve before the account is disabled: %v", err)
	}

	if _, err := GatewaySetUserDisabled(ctx, poolAsDB(t, pool), ns, u.ID, true); err != nil {
		t.Fatalf("disable user: %v", err)
	}

	// The password no longer works...
	if _, err := GatewayAuthenticatePassword(ctx, pool, ns, "leaver@example.com", pw); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("a disabled user authenticated by password: %v", err)
	}
	// ...and, critically, the session it already held is dead in the same instant.
	// Disabling an account while its sessions stay live leaves exactly the access
	// window an offboarding exists to close.
	if _, err := GatewayResolveSession(ctx, pool, plaintext); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("a disabled user's existing session still resolved: %v", err)
	}
}

func TestIntegrationDuplicateEmailIsRejected(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "dup@example.com", "First", "reader", "password-one")
	if err != nil {
		t.Fatalf("create first user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	// Different case, same address: the unique index is on lower(email), so this
	// must collide rather than create a second account nobody can tell apart.
	_, err = GatewayCreateUser(ctx, pool, ns, "DUP@example.com", "Second", "reader", "password-two")
	if !errors.Is(err, ErrEmailTaken) {
		t.Errorf("duplicate email returned %v, want ErrEmailTaken", err)
	}

	// The same address in a DIFFERENT namespace is legitimate: namespaces are the
	// tenancy boundary and two tenants may employ the same person.
	otherNS := ns + "-other"
	u2, err := GatewayCreateUser(ctx, pool, otherNS, "dup@example.com", "Other tenant", "reader", "password-three")
	if err != nil {
		t.Errorf("same email in another namespace was rejected: %v", err)
	} else {
		cleanupUser(t, ctx, pool, u2.ID)
	}
}

// ── sessions ─────────────────────────────────────────────────────────────

func TestIntegrationSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "session@example.com", "Session", "admin", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	plaintext, row, err := GatewayCreateSession(ctx, pool, u.ID, time.Hour, "curl/8", "10.0.0.1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The plaintext must not be recoverable from the stored row: that is the
	// property that makes a leak of this table non-catastrophic.
	var storedHash string
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM sessions WHERE id = $1`, row.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored hash: %v", err)
	}
	if storedHash == plaintext {
		t.Fatal("the session token is stored in plaintext")
	}
	if storedHash != HashAPIKey(plaintext) {
		t.Errorf("stored hash is not the sha256 of the token")
	}

	resolved, err := GatewayResolveSession(ctx, pool, plaintext)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if resolved.User.ID != u.ID || resolved.SessionID != row.ID {
		t.Errorf("resolved to user %s session %s, want %s / %s",
			resolved.User.ID, resolved.SessionID, u.ID, row.ID)
	}

	// Logout.
	if err := GatewayRevokeSession(ctx, pool, row.ID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}
	if _, err := GatewayResolveSession(ctx, pool, plaintext); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("a revoked session still resolved: %v", err)
	}
	// Revoking twice is not an error: a retried logout must not look like a failure.
	if err := GatewayRevokeSession(ctx, pool, row.ID); err != nil {
		t.Errorf("second revoke returned %v, want nil (logout is idempotent)", err)
	}
}

// TestIntegrationSessionExpiryUsesDatabaseClock asserts that expiry is a query
// predicate, not a Go comparison. Gateway replicas are not clock-synchronised, and
// a session alive on one replica and dead on another is the bug this prevents.
func TestIntegrationSessionExpiryUsesDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "expiry@example.com", "Expiry", "reader", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	// A session that expired one hour ago, created via the same code path.
	plaintext, row, err := GatewayCreateSession(ctx, pool, u.ID, -time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if !row.ExpiresAt.Before(time.Now()) {
		t.Fatalf("setup is wrong: expires_at %s is not in the past", row.ExpiresAt)
	}
	if _, err := GatewayResolveSession(ctx, pool, plaintext); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("an expired session resolved: %v", err)
	}
}

func TestIntegrationRevokeAllUserSessions(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "many@example.com", "Many", "admin", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	var tokens []string
	for i := 0; i < 3; i++ {
		pt, _, err := GatewayCreateSession(ctx, pool, u.ID, time.Hour, fmt.Sprintf("agent-%d", i), "127.0.0.1")
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
		tokens = append(tokens, pt)
	}

	n, err := GatewayRevokeUserSessions(ctx, pool, u.ID)
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n != 3 {
		t.Errorf("revoked %d sessions, want 3; the count is what the admin screen reports", n)
	}
	for i, pt := range tokens {
		if _, err := GatewayResolveSession(ctx, pool, pt); !errors.Is(err, ErrIdentityUnknown) {
			t.Errorf("session %d survived a mass revocation: %v", i, err)
		}
	}
}

// TestIntegrationTouchSessionIsCoarse covers the deliberate decision NOT to write
// on every request. A per-request UPDATE would put a row write and its WAL on the
// hot path of a console that polls every four seconds.
func TestIntegrationTouchSessionIsCoarse(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "touch@example.com", "Touch", "reader", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	_, row, err := GatewayCreateSession(ctx, pool, u.ID, time.Hour, "test", "127.0.0.1")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// First touch writes, because last_used_at is NULL.
	if err := GatewayTouchSession(ctx, pool, row.ID); err != nil {
		t.Fatalf("first touch: %v", err)
	}
	var first *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM sessions WHERE id = $1`, row.ID).Scan(&first); err != nil {
		t.Fatalf("read last_used_at: %v", err)
	}
	if first == nil {
		t.Fatal("the first touch did not record last_used_at")
	}

	// An immediate second touch must NOT write again.
	if err := GatewayTouchSession(ctx, pool, row.ID); err != nil {
		t.Fatalf("second touch: %v", err)
	}
	var second *time.Time
	if err := pool.QueryRow(ctx, `SELECT last_used_at FROM sessions WHERE id = $1`, row.ID).Scan(&second); err != nil {
		t.Fatalf("re-read last_used_at: %v", err)
	}
	if second == nil || !second.Equal(*first) {
		t.Errorf("a second touch within the interval rewrote last_used_at (%v -> %v); "+
			"every authenticated request would become a write", first, second)
	}
}

// ── scoped tokens ────────────────────────────────────────────────────────

func TestIntegrationScopedTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	plaintext, row, err := GatewayCreateScopedToken(ctx, pool, ns, "ci-pipeline", "admin",
		[]string{"read", "write"}, nil, nil)
	if err != nil {
		t.Fatalf("create scoped token: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM scoped_tokens WHERE id = $1`, row.ID) })

	if len(row.Scopes) != 2 || row.Scopes[0] != "read" || row.Scopes[1] != "write" {
		t.Errorf("scopes round-tripped as %v, want [read write]", row.Scopes)
	}

	resolved, err := GatewayResolveScopedToken(ctx, pool, plaintext)
	if err != nil {
		t.Fatalf("resolve scoped token: %v", err)
	}
	if resolved.Namespace != ns || resolved.Role != "admin" {
		t.Errorf("resolved %+v, want namespace %s role admin", resolved, ns)
	}
	if len(resolved.Scopes) != 2 {
		t.Errorf("resolved scopes %v, want two entries", resolved.Scopes)
	}

	if err := GatewayRevokeScopedToken(ctx, pool, ns, row.ID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	if _, err := GatewayResolveScopedToken(ctx, pool, plaintext); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("a revoked token still resolved: %v", err)
	}
}

// TestIntegrationScopedTokenExpiryUsesDatabaseClock mirrors the session case: an
// expired credential must be rejected by the query, not by the caller.
func TestIntegrationScopedTokenExpiryUsesDatabaseClock(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	past := time.Now().Add(-time.Hour)
	expired, row, err := GatewayCreateScopedToken(ctx, pool, ns, "expired", "reader", []string{"read"}, &past, nil)
	if err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM scoped_tokens WHERE id = $1`, row.ID) })

	if _, err := GatewayResolveScopedToken(ctx, pool, expired); !errors.Is(err, ErrIdentityUnknown) {
		t.Errorf("an expired scoped token resolved: %v", err)
	}

	// A NULL expiry means no expiry, preserving the current api_keys behaviour for
	// a token deliberately created as long-lived.
	forever, row2, err := GatewayCreateScopedToken(ctx, pool, ns, "forever", "reader", []string{"read"}, nil, nil)
	if err != nil {
		t.Fatalf("create non-expiring token: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM scoped_tokens WHERE id = $1`, row2.ID) })
	if _, err := GatewayResolveScopedToken(ctx, pool, forever); err != nil {
		t.Errorf("a token with no expiry was rejected: %v", err)
	}
}

// TestIntegrationScopedTokenCannotBeRevokedAcrossNamespaces covers the tenancy
// boundary on a destructive operation.
func TestIntegrationScopedTokenCannotBeRevokedAcrossNamespaces(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	plaintext, row, err := GatewayCreateScopedToken(ctx, pool, ns, "tenant-a", "reader", []string{"read"}, nil, nil)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM scoped_tokens WHERE id = $1`, row.ID) })

	if err := GatewayRevokeScopedToken(ctx, pool, ns+"-other", row.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking from another namespace returned %v, want ErrNotFound", err)
	}
	// It must still work for its owner.
	if _, err := GatewayResolveScopedToken(ctx, pool, plaintext); err != nil {
		t.Errorf("the token was revoked by a foreign namespace: %v", err)
	}
}

// ── audit log ────────────────────────────────────────────────────────────

func TestIntegrationAuditAppendAndList(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	entries := []AuditEntry{
		{Namespace: ns, ActorKind: "user", ActorLabel: "alice@example.com", Action: "user.create",
			TargetKind: "user", TargetID: "u1", Outcome: AuditSuccess, Detail: map[string]any{"role": "reader"}},
		{Namespace: ns, ActorKind: "user", ActorLabel: "alice@example.com", Action: "token.create",
			TargetKind: "token", TargetID: "t1", Outcome: AuditSuccess},
		{Namespace: ns, ActorKind: "anonymous", ActorLabel: "", Action: "session.login",
			Outcome: AuditDenied, Detail: map[string]any{"reason": "bad password"}},
	}
	for i, e := range entries {
		if err := GatewayAppendAudit(ctx, pool, e); err != nil {
			t.Fatalf("append audit %d: %v", i, err)
		}
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE namespace = $1`, ns) })

	rows, err := GatewayListAudit(ctx, pool, ns, AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("listed %d audit rows, want 3", len(rows))
	}
	// Most recent first is what an incident review reads.
	if rows[0].Action != "session.login" {
		t.Errorf("first row is %q, want the most recent (session.login)", rows[0].Action)
	}

	// A DENIED outcome must be recorded as loudly as a success: an audit log of
	// only successes cannot show an attack in progress.
	denied, err := GatewayListAudit(ctx, pool, ns, AuditFilter{Outcome: AuditDenied})
	if err != nil {
		t.Fatalf("filter by outcome: %v", err)
	}
	if len(denied) != 1 || denied[0].Action != "session.login" {
		t.Errorf("outcome filter returned %d rows, want the one denied login", len(denied))
	}

	byAction, err := GatewayListAudit(ctx, pool, ns, AuditFilter{Action: "user.create"})
	if err != nil {
		t.Fatalf("filter by action: %v", err)
	}
	if len(byAction) != 1 {
		t.Errorf("action filter returned %d rows, want 1", len(byAction))
	}
}

// TestIntegrationAuditIsNamespaceScoped: one tenant must not read another's
// administrative history.
func TestIntegrationAuditIsNamespaceScoped(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	nsA, nsB := uniqueNS(t)+"-a", uniqueNS(t)+"-b"

	if err := GatewayAppendAudit(ctx, pool, AuditEntry{Namespace: nsA, Action: "user.create"}); err != nil {
		t.Fatalf("append to A: %v", err)
	}
	if err := GatewayAppendAudit(ctx, pool, AuditEntry{Namespace: nsB, Action: "token.revoke"}); err != nil {
		t.Fatalf("append to B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE namespace IN ($1, $2)`, nsA, nsB)
	})

	rows, err := GatewayListAudit(ctx, pool, nsA, AuditFilter{})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	for _, r := range rows {
		if r.Namespace != nsA {
			t.Errorf("namespace %s leaked a row from %s", nsA, r.Namespace)
		}
	}
	if len(rows) != 1 {
		t.Errorf("namespace A has %d rows, want 1", len(rows))
	}
}

// TestIntegrationAuditDetailCarriesNoSecret is a regression guard on the one rule
// that makes this table safe to show an operator. It asserts the shape the store
// enforces: Detail is a caller-built map, so a token can only appear if a caller
// puts it there deliberately.
func TestIntegrationAuditDetailIsStructured(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	err := GatewayAppendAudit(ctx, pool, AuditEntry{
		Namespace: ns, Action: "token.create", TargetKind: "token", TargetID: "abc",
		Detail: map[string]any{"scopes": []string{"read"}, "expires_in_days": 30},
	})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE namespace = $1`, ns) })

	rows, err := GatewayListAudit(ctx, pool, ns, AuditFilter{})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d rows, want 1", len(rows))
	}
	if len(rows[0].Detail) == 0 || string(rows[0].Detail) == "{}" {
		t.Errorf("detail did not round-trip: %s", rows[0].Detail)
	}
	// Defaults are applied so a row is never missing the fields a reviewer reads.
	if rows[0].Outcome != AuditSuccess {
		t.Errorf("outcome defaulted to %q, want %q", rows[0].Outcome, AuditSuccess)
	}
	if rows[0].ActorKind != "anonymous" {
		t.Errorf("actor_kind defaulted to %q, want anonymous", rows[0].ActorKind)
	}
}

// TestIntegrationExpiredSessionSweep covers the bounded cleanup: this table grows
// at one row per login forever without it.
func TestIntegrationExpiredSessionSweep(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	ns := uniqueNS(t)

	u, err := GatewayCreateUser(ctx, pool, ns, "sweep@example.com", "Sweep", "reader", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	cleanupUser(t, ctx, pool, u.ID)

	for i := 0; i < 3; i++ {
		if _, _, err := GatewayCreateSession(ctx, pool, u.ID, -time.Hour, "old", "127.0.0.1"); err != nil {
			t.Fatalf("create expired session %d: %v", i, err)
		}
	}
	// One live session that must SURVIVE the sweep.
	livePlaintext, _, err := GatewayCreateSession(ctx, pool, u.ID, time.Hour, "live", "127.0.0.1")
	if err != nil {
		t.Fatalf("create live session: %v", err)
	}

	n, err := GatewayDeleteExpiredSessions(ctx, pool, time.Now(), 100)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n < 3 {
		t.Errorf("swept %d sessions, want at least the 3 expired ones", n)
	}
	if _, err := GatewayResolveSession(ctx, pool, livePlaintext); err != nil {
		t.Errorf("the sweep deleted a LIVE session: %v", err)
	}
}

// poolAsDB adapts the test pool to GWDB. *pgxpool.Pool already satisfies it; the
// helper exists so the assertion failure is readable if that ever changes.
func poolAsDB(t *testing.T, q GWQuerier) GWDB {
	t.Helper()
	db, ok := q.(GWDB)
	if !ok {
		t.Fatalf("test pool does not satisfy GWDB")
	}
	return db
}
