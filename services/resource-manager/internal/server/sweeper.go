// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// DefaultLivenessWindow is how long a worker may go silent before it is declared
// DEAD (§5.5). 30s against a 5s heartbeat tolerates six consecutive missed beats,
// which absorbs a network blip or a control-plane restart without tolerating a
// genuinely dead worker for long.
const DefaultLivenessWindow = 30 * time.Second

// Sweeper transitions silent workers to DEAD and returns their capacity.
//
// This is the counterpart to heartbeat reconciliation, and BOTH are needed. A
// heartbeat can only reconcile a worker that is still heartbeating; a worker that
// has stopped entirely never reports again, so its reservations would stay
// committed forever with nothing to release them. The sweep is what closes that
// path. A ledger with only one of the two loops leaks, quietly and permanently.
type Sweeper struct {
	store *store.RMStore
	log   *slog.Logger

	// LivenessWindow is the silence after which a worker is DEAD.
	LivenessWindow time.Duration
	// Interval is the sweep period. Meaningfully shorter than the window so a
	// dead worker's capacity comes back promptly rather than a window later.
	Interval time.Duration

	// Metrics is optional; nil records nothing.
	Metrics *Metrics
}

// NewSweeper builds a sweeper with the §5.5 defaults.
func NewSweeper(s *store.RMStore, log *slog.Logger) *Sweeper {
	return &Sweeper{
		store: s, log: log,
		LivenessWindow: DefaultLivenessWindow,
		Interval:       10 * time.Second,
	}
}

// Run sweeps until ctx is cancelled.
//
// Multiple resource-manager replicas may sweep concurrently. That is safe by
// construction rather than by leader election: the state transition is a
// conditional UPDATE that only one replica can win, and release is idempotent, so
// a loser's release subtracts nothing. Correctness comes from the database, not
// from coordination — the same principle the scheduler relies on (§3.2).
func (sw *Sweeper) Run(ctx context.Context) error {
	t := time.NewTicker(sw.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			sw.log.Info("liveness sweeper stopping", "reason", ctx.Err())
			return nil
		case <-t.C:
			dead, released := sw.sweepOnce(ctx)
			sw.Metrics.workersDead(dead)
			sw.Metrics.leak("dead_worker", released)
		}
	}
}

// SweepOnce runs a single pass, exported for tests and for an operator-triggered
// repair.
func (sw *Sweeper) SweepOnce(ctx context.Context) (dead int, released int) {
	return sw.sweepOnce(ctx)
}

func (sw *Sweeper) sweepOnce(ctx context.Context) (dead int, released int) {
	stale, err := sw.store.ListStaleWorkers(ctx, sw.LivenessWindow)
	if err != nil {
		sw.log.Error("could not list stale workers", "error", err)
		return 0, 0
	}
	for _, w := range stale {
		log := sw.log.With("worker_id", w.ID, "hostname", w.Hostname,
			"last_heartbeat_at", w.LastHeartbeat, "previous_state", string(w.State))

		applied, err := sw.store.SetWorkerState(ctx, w.ID, store.WorkerDead)
		if err != nil {
			log.Error("could not mark a silent worker DEAD", "error", err)
			continue
		}
		if !applied {
			continue // another replica won the race; it owns the release
		}
		dead++
		log.Warn("worker declared DEAD: liveness window exceeded",
			"liveness_window", sw.LivenessWindow)

		// A dead worker is running nothing, so its entire committed vector is a
		// leak by definition. Its attempts are the scheduler's: its liveness
		// sweep expires the in-flight attempts of every DEAD worker, including
		// ones this sweeper marked, within one of its intervals (F106), with
		// lease expiry as the backstop. Our job is only the capacity half, and
		// the scheduler's release finds these reservations gone and releases
		// nothing twice.
		leaked, err := sw.store.ReleaseAllForWorker(ctx, w.ID)
		if err != nil {
			log.Error("worker is DEAD but its capacity could not be reclaimed", "error", err)
			continue
		}
		released += len(leaked)
		if len(leaked) > 0 {
			log.Warn("reclaimed a dead worker's capacity", "reservations", len(leaked))
		}
	}
	return dead, released
}
