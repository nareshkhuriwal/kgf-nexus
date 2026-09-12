//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Integration coverage for the sweeper against real Postgres. The unit tests
// prove the LOOP is correct against a fake; this proves the statement the loop
// issues actually removes expired rows and leaves live ones alone, which is the
// half a fake can never establish.
package sessions

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	nexuscfg "github.com/khuriwalgroup/kgf-nexus/internal/config"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// integrationPool connects, or SKIPS — matching internal/store's convention so a
// laptop with no Postgres does not turn the suite red.
func integrationPool(t *testing.T) store.GWDB {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	pool, err := store.GWOpen(ctx, nexuscfg.DSN("postgres://postgres:test@localhost:55432/nexus"), 5)
	if err != nil {
		t.Skipf("skipping integration test: test database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestIntegrationSweeperRemovesExpiredSessionsOnly(t *testing.T) {
	ctx := context.Background()
	pool := integrationPool(t)

	ns := "it-sweeper-" + time.Now().Format("20060102150405.000000000")
	u, err := store.GatewayCreateUser(ctx, pool, ns, "sweeper@example.com", "Sweeper", "reader", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE actor_id = $1`, u.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	for i := 0; i < 3; i++ {
		if _, _, err := store.GatewayCreateSession(ctx, pool, u.ID, -time.Hour, "expired", "127.0.0.1"); err != nil {
			t.Fatalf("create expired session %d: %v", i, err)
		}
	}
	// A live session for THIS user, and the assertion that matters most: the live
	// database holds real operator sessions, and a sweep that took one would log a
	// human out.
	livePlaintext, _, err := store.GatewayCreateSession(ctx, pool, u.ID, time.Hour, "live", "127.0.0.1")
	if err != nil {
		t.Fatalf("create live session: %v", err)
	}

	sw := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if n := sw.SweepOnce(ctx); n < 3 {
		t.Errorf("swept %d sessions, want at least the 3 expired ones", n)
	}
	if _, err := store.GatewayResolveSession(ctx, pool, livePlaintext); err != nil {
		t.Errorf("the sweep deleted a LIVE session: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND expires_at < now()`, u.ID).
		Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d expired sessions survived the sweep", remaining)
	}
}

func TestIntegrationSweepIsBoundedByTheBatchLimit(t *testing.T) {
	// The bound is the reason this is safe to run against a production table: one
	// tick must be a short transaction, not a statement that locks every expired
	// row in one go.
	ctx := context.Background()
	pool := integrationPool(t)

	ns := "it-sweepbound-" + time.Now().Format("20060102150405.000000000")
	u, err := store.GatewayCreateUser(ctx, pool, ns, "bound@example.com", "Bound", "reader", "password-here")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, u.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM audit_log WHERE actor_id = $1`, u.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	for i := 0; i < 5; i++ {
		if _, _, err := store.GatewayCreateSession(ctx, pool, u.ID, -time.Hour, "expired", "127.0.0.1"); err != nil {
			t.Fatalf("create expired session %d: %v", i, err)
		}
	}

	sw := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sw.BatchLimit = 2
	if n := sw.SweepOnce(ctx); n != 2 {
		t.Errorf("a limit of 2 deleted %d rows; the per-tick bound is not honoured", n)
	}
}
