// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// ── Durable run advancement (CONTRACT-FINDINGS F160) ───────────────────────
//
// A run advances only when this engine reconciles it. Migration 0039 makes the
// DATABASE record that a reconcile is owed -- a row in run_reconcile_requests,
// written by trigger in the same transaction as the terminal attempt or the
// run state change that needs it -- so the obligation can no longer be lost
// between a commit and a best-effort RPC. The engine discharges it two ways:
//
//   - the synchronous ReconcileRun RPC (the gateway after a report, the
//     workflow-manager after resume, cancel and retry): the fast path. Each
//     pass snapshots the run's queued requests before it starts and deletes
//     exactly those once it has succeeded, so on the happy path the queue
//     empties as fast as it fills and nothing is reconciled twice.
//   - the ReconcileQueue consumer below: the durable path. It claims whatever
//     the fast path did not consume -- lost calls, timeouts, a restart, a
//     failed publish -- and reconciles those runs.
//
// Both go through the per-run coalescer (reconcileFlights), so a burst of
// reports and the consumer share passes instead of multiplying them.

// ackSnapshotLimit bounds how many queued requests one pass acknowledges.
// A run with more waiting simply has the rest consumed by the next pass.
const ackSnapshotLimit = 10000

// ReconcileDurably reconciles runID through the per-run coalescer and, on
// success, consumes the durable requests the pass covered. It is what the
// ReconcileRun RPC and the queue consumer call.
//
// It returns an error when the pass failed OR when the pass reached a run
// conclusion it could not publish: in both cases the run's requests stay
// queued, and the consumer will try again.
func (e *Engine) ReconcileDurably(ctx context.Context, runID string) (*ReconcileResult, error) {
	return e.flights.reconcile(ctx, runID)
}

// ReconcilePasses reports how many coalesced reconcile passes have run.
func (e *Engine) ReconcilePasses() int64 { return e.flights.passes.Load() }

// reconcileAndAcknowledge is one coalesced pass.
//
// The snapshot is taken BEFORE the reconcile reads anything: a request visible
// to it was committed before it, so the attempt (or run state) it announces is
// visible to the reconcile that follows. Deleting exactly those ids therefore
// never discards a fact the pass did not see; a request committed after the
// snapshot survives for the next pass.
func (e *Engine) reconcileAndAcknowledge(ctx context.Context, runID string) (*ReconcileResult, error) {
	ids, serr := e.store.PendingReconcileRequestIDs(ctx, runID, ackSnapshotLimit)
	if serr != nil {
		// Reconcile anyway: advancing the run matters more than tidying the
		// queue, and the requests stay for the consumer, which costs one
		// redundant pass.
		e.log.Warn("could not snapshot a run's reconcile requests; reconciling without consuming them",
			"run_id", runID, "error", serr)
	}
	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		return nil, err
	}
	if res.PublishErr != nil {
		return res, fmt.Errorf("reconcile run %s: its conclusion is not yet published: %w", runID, res.PublishErr)
	}
	if len(ids) > 0 {
		if _, err := e.store.AckReconcileRequests(ctx, ids); err != nil {
			// The run advanced; only the bookkeeping failed. The consumer will
			// reconcile it once more, which is harmless.
			e.log.Warn("reconciled a run but could not consume its reconcile requests",
				"run_id", runID, "requests", len(ids), "error", err)
		}
	}
	return res, nil
}

// ReconcileQueueConfig tunes the consumer. Zero fields take the defaults.
type ReconcileQueueConfig struct {
	// Owner names this replica in claimed_by, for debugging a stuck claim.
	Owner string
	// PollInterval is how often an idle consumer looks for work. It bounds
	// how late a run whose fast-path call was lost advances.
	PollInterval time.Duration
	// Lease is how long a claim hides requests from other replicas. A
	// replica that dies holding claims returns them when it expires. A pass
	// that outlives it is harmless: another replica reconciles the run too,
	// and reconciles are idempotent.
	Lease time.Duration
	// BatchSize bounds the requests one claim takes. A 500-way burst of one
	// run is one run to reconcile, whatever its row count.
	BatchSize int
	// BackoffBase and BackoffMax bound the retry of a run whose reconcile
	// failed: base * 2^failures, capped. Requests are never dropped for
	// failing -- a run that needs a reconcile still needs one.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// StatsInterval is how often the backlog gauges are sampled.
	StatsInterval time.Duration
}

func (c ReconcileQueueConfig) withDefaults() ReconcileQueueConfig {
	if c.Owner == "" {
		c.Owner = "dag-engine"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.Lease <= 0 {
		c.Lease = 30 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = time.Minute
	}
	if c.StatsInterval <= 0 {
		c.StatsInterval = 15 * time.Second
	}
	return c
}

// ReconcileQueue consumes run_reconcile_requests. Any number of replicas may
// run one: claims are SKIP LOCKED and leased, so they share the backlog with
// no leader.
type ReconcileQueue struct {
	engine *Engine
	store  *store.DAGEngineStore
	cfg    ReconcileQueueConfig
	log    *slog.Logger
}

// NewReconcileQueue wires a consumer.
func NewReconcileQueue(e *Engine, st *store.DAGEngineStore, cfg ReconcileQueueConfig, log *slog.Logger) *ReconcileQueue {
	if log == nil {
		log = slog.Default()
	}
	return &ReconcileQueue{engine: e, store: st, cfg: cfg.withDefaults(), log: log}
}

// Run consumes until ctx ends. A full batch is followed at once by another,
// so a backlog drains at reconcile speed rather than one batch per poll.
func (q *ReconcileQueue) Run(ctx context.Context) {
	q.log.Info("run reconcile queue consumer started",
		"owner", q.cfg.Owner, "poll_interval", q.cfg.PollInterval, "lease", q.cfg.Lease,
		"batch_size", q.cfg.BatchSize)
	q.sample(ctx)
	stats := time.NewTicker(q.cfg.StatsInterval)
	defer stats.Stop()
	for {
		claimed, err := q.Drain(ctx)
		if err != nil && ctx.Err() == nil {
			q.log.Error("run reconcile queue: claim failed; retrying", "error", err)
		}
		wait := q.cfg.PollInterval
		if claimed >= q.cfg.BatchSize {
			wait = 0
		}
		select {
		case <-ctx.Done():
			q.log.Info("run reconcile queue consumer stopped")
			return
		case <-stats.C:
			q.sample(ctx)
		case <-time.After(wait):
		}
	}
}

func (q *ReconcileQueue) sample(ctx context.Context) {
	depth, oldest, err := q.store.ReconcileQueueStats(ctx)
	if err != nil {
		if ctx.Err() == nil {
			q.log.Warn("could not sample the run reconcile queue", "error", err)
		}
		return
	}
	q.engine.metrics.observeQueue(depth, oldest)
	if oldest > 5*time.Minute {
		q.log.Warn("run reconcile requests are waiting; runs are not advancing",
			"queued", depth, "oldest", oldest.Round(time.Second))
	}
}

// Drain claims one batch and reconciles every run in it, returning how many
// requests it claimed.
func (q *ReconcileQueue) Drain(ctx context.Context) (int, error) {
	claims, err := q.store.ClaimReconcileRequests(ctx, q.cfg.Owner, q.cfg.Lease, q.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	n := 0
	var wg sync.WaitGroup
	for _, c := range claims {
		n += len(c.IDs)
		wg.Add(1)
		// Concurrency is bounded by the coalescer's semaphore, not here: a
		// goroutine per run in the batch only waits for its turn.
		go func(c store.ReconcileClaim) {
			defer wg.Done()
			q.process(ctx, c)
		}(c)
	}
	wg.Wait()
	return n, nil
}

func (q *ReconcileQueue) process(ctx context.Context, c store.ReconcileClaim) {
	log := q.log.With("run_id", c.RunID)
	_, err := q.engine.ReconcileDurably(ctx, c.RunID)
	// Bookkeeping after the pass must happen even while shutting down, so a
	// stopping replica hands its claims back instead of leaving them leased.
	bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	switch {
	case err == nil:
		// The pass consumed its own snapshot, which includes these rows; this
		// catches a snapshot truncated by ackSnapshotLimit.
		if _, aerr := q.store.AckReconcileRequests(bctx, c.IDs); aerr != nil {
			log.Warn("could not consume reconcile requests after reconciling", "error", aerr)
		}
		q.engine.metrics.observeQueueRun("ok")
	case errors.Is(err, store.ErrDAGEngineRunNotFound), errors.Is(err, ErrNotMaterialized):
		// Nothing to reconcile: the run was deleted, or its graph was never
		// materialized (a PENDING run cancelled before it started). Keeping
		// the request would retry forever for nothing.
		log.Debug("dropping reconcile requests for a run with nothing to reconcile", "reason", err)
		if _, aerr := q.store.AckReconcileRequests(bctx, c.IDs); aerr != nil {
			log.Warn("could not drop reconcile requests", "error", aerr)
		}
		q.engine.metrics.observeQueueRun("dropped")
	default:
		backoff := q.backoff(c.Attempts)
		if ctx.Err() != nil {
			// Shutting down: hand the claim back for another replica now.
			backoff = 0
		} else {
			log.Warn("reconcile from the durable queue failed; will retry",
				"error", err, "failures", c.Attempts+1, "retry_in", backoff)
		}
		if rerr := q.store.ReleaseReconcileRequests(bctx, c.IDs, backoff, err.Error()); rerr != nil {
			// The lease still expires, which returns the rows anyway.
			log.Warn("could not release reconcile requests; their lease will return them", "error", rerr)
		}
		q.engine.metrics.observeQueueRun("retry")
	}
}

func (q *ReconcileQueue) backoff(failures int) time.Duration {
	d := q.cfg.BackoffBase
	for i := 0; i < failures && d < q.cfg.BackoffMax; i++ {
		d *= 2
	}
	if d > q.cfg.BackoffMax {
		d = q.cfg.BackoffMax
	}
	return d
}
