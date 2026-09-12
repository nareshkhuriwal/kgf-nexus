// Package timers implements the durable timer reconciler.
//
// EVERY deadline in the system is a row in the timers table rather than an
// in-memory timer. The difference is not stylistic: an in-memory timer wheel loses
// every pending deadline on a control-plane restart, SILENTLY. A workflow whose
// start_to_close should have fired during a rolling deploy would simply run
// forever, and nothing would report that anything was lost (architecture §3.2).
package timers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Timer kinds dispatched by this reconciler. The attempt-scoped kinds mirror the
// four-timeout model (§4.3): distinguishing "never got capacity" from "ran too
// long" from "went silent" tells an operator whether to fix the cluster, the code,
// or the network, and collapsing them into one loses that signal entirely.
const (
	KindScheduleToStart = "schedule_to_start"
	KindStartToClose    = "start_to_close"
	KindScheduleToClose = "schedule_to_close"
	KindHeartbeat       = "heartbeat"
	KindRetryBackoff    = "retry_backoff"
	KindLeaseExpiry     = "lease_expiry"
	KindRunDeadline     = "run_deadline"
)

// Handler acts on one fired timer.
//
// Handlers MUST be idempotent. A reconciler that crashes between claiming a timer
// and handling it leaves the claim behind, and the stale-claim sweeper returns it
// to the pool — so at-least-once delivery is the contract, not a bug. Every
// handler here achieves that by expressing its effect as a compare-and-set that is
// a no-op the second time.
type Handler func(ctx context.Context, t store.Timer) error

// Config tunes the reconciler loop.
type Config struct {
	// Interval is the polling period. It bounds how LATE a deadline can fire, so
	// it is a correctness-adjacent knob: a 10s interval means a 30s lease can
	// expire up to 10s late, which must stay well inside the retry budget.
	Interval time.Duration
	// BatchSize caps rows claimed per cycle, bounding transaction size and the
	// blast radius of a crash mid-batch.
	BatchSize int
	// ClaimTimeout is how long a claim may be held before the sweeper assumes the
	// claimant died and returns the timer to the pool. Too short and a slow
	// handler's timer fires twice; too long and a crashed replica's deadlines stall.
	ClaimTimeout time.Duration
	// Claimant identifies this replica in claimed_by, for debugging a stuck batch.
	Claimant string
}

// DefaultConfig is a sane starting point.
func DefaultConfig(claimant string) Config {
	return Config{
		Interval:     time.Second,
		BatchSize:    100,
		ClaimTimeout: 2 * time.Minute,
		Claimant:     claimant,
	}
}

// Reconciler polls the durable timer table and dispatches fired timers.
type Reconciler struct {
	db       store.GWDB
	cfg      Config
	log      *slog.Logger
	mu       sync.RWMutex
	handlers map[string]Handler
}

// New builds a reconciler.
func New(db store.GWDB, cfg Config, log *slog.Logger) *Reconciler {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.ClaimTimeout <= 0 {
		cfg.ClaimTimeout = 2 * time.Minute
	}
	return &Reconciler{db: db, cfg: cfg, log: log, handlers: make(map[string]Handler)}
}

// Handle registers the handler for a timer kind.
func (r *Reconciler) Handle(kind string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = h
}

// Run polls until the context is cancelled.
//
// Returns nil on cancellation rather than ctx.Err(): a graceful shutdown is a
// successful outcome, and reporting it as an error makes a clean deploy look like
// a failure in the logs.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()

	sweep := time.NewTicker(r.cfg.ClaimTimeout / 2)
	defer sweep.Stop()

	r.log.InfoContext(ctx, "timer reconciler started",
		"claimant", r.cfg.Claimant, "interval", r.cfg.Interval, "batch_size", r.cfg.BatchSize)

	for {
		select {
		case <-ctx.Done():
			r.log.InfoContext(ctx, "timer reconciler stopped")
			return nil
		case <-sweep.C:
			r.sweepStaleClaims(ctx)
		case <-ticker.C:
			// A cycle that errors is logged and retried on the next tick rather
			// than killing the loop. The reconciler is the only thing that makes
			// deadlines fire at all; stopping it on a transient database error
			// would turn a blip into an outage in which no timeout ever fires.
			if n, err := r.Cycle(ctx); err != nil {
				r.log.ErrorContext(ctx, "timer cycle failed; retrying next tick", "error", err)
			} else if n > 0 {
				r.log.DebugContext(ctx, "timer cycle dispatched", "count", n)
			}
		}
	}
}

// Cycle claims and dispatches one batch. Exported so tests can drive the
// reconciler deterministically instead of waiting on wall-clock ticks.
func (r *Reconciler) Cycle(ctx context.Context) (int, error) {
	claimed, err := store.WFMClaimDueTimers(ctx, r.db, r.cfg.Claimant, r.cfg.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("claim due timers: %w", err)
	}

	dispatched := 0
	for _, t := range claimed {
		if err := r.dispatch(ctx, t); err != nil {
			// The claim is LEFT IN PLACE on failure. The stale-claim sweeper
			// returns it to the pool after ClaimTimeout, giving a bounded retry
			// without a hot loop against a handler that is consistently failing.
			r.log.ErrorContext(ctx, "timer handler failed; will be retried after claim timeout",
				"timer_id", t.ID, "kind", t.Kind, "subject_id", t.SubjectID, "error", err)
			continue
		}
		if err := store.WFMDeleteTimer(ctx, r.db, t.ID); err != nil {
			// The effect already happened; failing to delete means the timer may
			// fire again, which the idempotent-handler contract covers.
			r.log.ErrorContext(ctx, "timer handled but not deleted; handler idempotency will absorb a repeat",
				"timer_id", t.ID, "kind", t.Kind, "error", err)
			continue
		}
		dispatched++
	}
	return dispatched, nil
}

func (r *Reconciler) dispatch(ctx context.Context, t store.Timer) error {
	r.mu.RLock()
	h, ok := r.handlers[t.Kind]
	r.mu.RUnlock()
	if !ok {
		// An unregistered kind is a deployment bug (a timer armed by a newer
		// version than this replica understands). Returning an error leaves it
		// claimed and then re-pooled, so a rolling deploy's newer replica picks it
		// up rather than this one silently discarding a real deadline.
		return fmt.Errorf("no handler registered for timer kind %q", t.Kind)
	}

	log := r.log.With("timer_id", t.ID, "kind", t.Kind, "subject_id", t.SubjectID)
	if lateness := time.Since(t.FireAt); lateness > 5*r.cfg.Interval {
		// Lateness is the metric that reveals a reconciler falling behind, which
		// is a silent correctness problem: deadlines still fire, just too late to
		// mean what they promised.
		log.WarnContext(ctx, "timer fired late", "lateness", lateness)
	}

	if err := h(ctx, t); err != nil {
		return fmt.Errorf("handle %s timer for %s: %w", t.Kind, t.SubjectID, err)
	}
	return nil
}

func (r *Reconciler) sweepStaleClaims(ctx context.Context) {
	n, err := store.WFMReleaseStaleTimerClaims(ctx, r.db, r.cfg.ClaimTimeout)
	if err != nil {
		r.log.ErrorContext(ctx, "failed to sweep stale timer claims", "error", err)
		return
	}
	if n > 0 {
		// A nonzero sweep means a replica died holding claims, or a handler is
		// persistently failing. Either is worth an operator's attention.
		r.log.WarnContext(ctx, "returned stale timer claims to the pool", "count", n)
	}
}

// Backlog reports the due-but-unclaimed count and the total, for readiness and
// metrics. A growing due count means deadlines are firing late.
func (r *Reconciler) Backlog(ctx context.Context) (due int64, total int64, err error) {
	return store.WFMCountPendingTimers(ctx, r.db)
}

// DecodePayload reads a timer's payload into a typed value.
func DecodePayload(t store.Timer, into any) error {
	if len(t.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(t.Payload, into); err != nil {
		return fmt.Errorf("decode payload of timer %s: %w", t.ID, err)
	}
	return nil
}

// ErrNoHandler is returned when a timer kind has no registered handler.
var ErrNoHandler = errors.New("no handler for timer kind")
