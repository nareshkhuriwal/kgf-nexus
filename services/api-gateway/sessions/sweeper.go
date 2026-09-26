// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package sessions holds the gateway's background maintenance of the `sessions`
// table.
//
// The table is written on every console login and never read again once a row
// expires, so without a sweeper it grows at one row per login forever. The store
// already had the bounded DELETE (store.GatewayDeleteExpiredSessions) and nothing
// called it, which is the quiet failure mode: correct code that never runs.
package sessions

import (
	"context"
	"log/slog"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// DefaultInterval is how often expired rows are swept.
//
// Fifteen minutes is chosen against the 12h session TTL (authn.SessionTTL): the
// rows being removed are already dead to authentication, so nothing is gained by
// sweeping faster, and a quarter-hour keeps the steady-state backlog small enough
// that one bounded batch clears it on a normal deployment. Sweeping aggressively
// would only buy a cleaner table at the cost of constant background load on the
// one Postgres every service shares.
const DefaultInterval = 15 * time.Minute

// DefaultBatchLimit is the most rows one tick will delete.
//
// A thousand is what the store already defaults to, and it keeps each DELETE a
// short transaction — the point of the bound is that the sweep never holds locks
// on `sessions` long enough to block a login, which is a foreground, latency-
// sensitive write. A backlog larger than one batch is simply cleared over
// successive ticks; there is no deadline on removing a row that already cannot
// authenticate.
const DefaultBatchLimit = 1000

// Sweeper deletes expired session rows on a timer.
//
// Multiple gateway replicas may sweep concurrently, and that needs no
// coordination: the DELETE selects its own bounded set of already-expired rows,
// so a loser deletes nothing and both outcomes are the intended one. This is the
// same "correctness comes from the database, not from leader election" property
// the resource-manager's liveness sweeper relies on.
type Sweeper struct {
	db  store.GWQuerier
	log *slog.Logger

	// Interval is the sweep period.
	Interval time.Duration
	// BatchLimit bounds the rows removed per tick.
	BatchLimit int
}

// New builds a sweeper with the defaults above.
func New(db store.GWQuerier, log *slog.Logger) *Sweeper {
	return &Sweeper{
		db: db, log: log,
		Interval:   DefaultInterval,
		BatchLimit: DefaultBatchLimit,
	}
}

// Run sweeps until ctx is cancelled.
//
// Returns nil on cancellation rather than ctx.Err(): a clean shutdown is not a
// service failure, and the caller treats a non-nil return as a reason to exit
// non-zero.
func (sw *Sweeper) Run(ctx context.Context) error {
	t := time.NewTicker(sw.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			sw.log.Info("expired-session sweeper stopping", "reason", ctx.Err())
			return nil
		case <-t.C:
			sw.sweepOnce(ctx)
			sw.sweepSSO(ctx)
		}
	}
}

// SweepOnce runs a single pass, exported for tests and for an operator-triggered
// cleanup after a backlog has built up.
func (sw *Sweeper) SweepOnce(ctx context.Context) int64 {
	return sw.sweepOnce(ctx)
}

func (sw *Sweeper) sweepOnce(ctx context.Context) int64 {
	// `before` is now: a row is expired exactly when the authenticator would have
	// rejected it, so the sweep can never remove a session that could still log
	// someone in. No grace period is subtracted, because there is no downstream
	// reader that needs the row to linger — the audit log holds the evidence.
	deleted, err := store.GatewayDeleteExpiredSessions(ctx, sw.db, time.Now(), sw.BatchLimit)
	if err != nil {
		// A failed sweep is not a reason to take the gateway down: the table merely
		// stays larger until the next tick, while the gateway can still authenticate
		// and serve. Logged at error so it is visible if it stops being transient.
		sw.log.Error("could not sweep expired sessions", "error", err)
		return 0
	}
	if deleted > 0 {
		sw.log.Info("swept expired sessions", "deleted", deleted, "limit", sw.BatchLimit)
	}
	return deleted
}

// sweepSSO bounds the SSO tables (ADR-0024).
func (sw *Sweeper) sweepSSO(ctx context.Context) {
	// SSO login states, handoffs and SAML replay records (ADR-0024). They are
	// already unusable once expired (every consume checks expires_at); this only
	// bounds the tables. Replay records are kept until the assertion itself
	// could no longer be valid, so sweeping never reopens a replay window.
	if n, err := store.GatewayDeleteExpiredSSO(ctx, sw.db, sw.BatchLimit); err != nil {
		sw.log.Error("could not sweep expired sso records", "error", err)
	} else if n > 0 {
		sw.log.Info("swept expired sso records", "deleted", n)
	}
}
