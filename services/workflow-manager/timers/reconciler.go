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

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
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
	// BacklogInterval is how often the backlog is sampled into the metrics gauges
	// and, past BacklogAlert, logged. It is deliberately much coarser than Interval:
	// the sample is an aggregate over the whole timers table, so one query per
	// replica per second would add real load to the database precisely when the
	// backlog is large and the database is already the bottleneck.
	BacklogInterval time.Duration
	// BacklogAlert is the overdue count past which the sampler logs a warning. It is
	// an ALERTING threshold only — crossing it must never change whether this
	// replica serves traffic, for the reason documented on Reconciler.Backlog.
	BacklogAlert int64
	// Metrics receives backlog, lateness and dispatch-outcome observations. Nil
	// disables instrumentation, which keeps the reconciler usable in tests without
	// a registry.
	Metrics *Metrics
}

// DefaultConfig is a sane starting point.
func DefaultConfig(claimant string) Config {
	return Config{
		Interval:        time.Second,
		BatchSize:       100,
		ClaimTimeout:    2 * time.Minute,
		Claimant:        claimant,
		BacklogInterval: 15 * time.Second,
		BacklogAlert:    1000,
	}
}

// Metrics holds the reconciler's instruments.
//
// The backlog lives here, as a GAUGE an operator alerts on, rather than in the
// readiness probe. Timer backlog is a GLOBAL property of the timers table: every
// replica queries the same rows and computes the same number, so failing readiness
// on it fails EVERY replica at once and empties the tier from the load balancer —
// and since this reconciler is what DRAINS the backlog, shedding replicas makes
// recovery strictly slower. Readiness answers "can I serve", and a replica with a
// healthy database connection can serve regardless of how far behind the fleet is.
type Metrics struct {
	// BacklogDue is the overdue-and-unclaimed count: the number that says deadlines
	// are firing late right now.
	BacklogDue *metrics.Gauge
	// BacklogTotal is every pending timer, due or not. Due alone cannot distinguish
	// "a burst is about to land" from "nothing is armed".
	BacklogTotal *metrics.Gauge
	// Lateness is how far past fire_at a timer actually fired, which is the signal
	// that the reconciler is falling behind. A deadline that fires late still fires,
	// so this is the only place the degradation is visible.
	Lateness *metrics.Histogram
	// Fired counts dispatch outcomes by kind and result, so an operator can tell a
	// failing handler apart from an idle one.
	Fired *metrics.CounterVec
	// StaleClaims counts timers returned to the pool by the sweeper: nonzero means a
	// replica died holding claims, or a handler is persistently failing.
	StaleClaims *metrics.Counter
}

// NewMetrics registers the reconciler's instruments on a registry.
func NewMetrics(reg *metrics.Registry) *Metrics {
	return &Metrics{
		BacklogDue:   reg.NewGauge("nexus_timer_backlog_due", "Timers past fire_at and unclaimed.").Gauge(),
		BacklogTotal: reg.NewGauge("nexus_timer_backlog_total", "Pending timers, due or not.").Gauge(),
		// The buckets run out to five minutes because the interesting question is
		// not "is it a few milliseconds late" but "is a 30s lease expiring minutes
		// after it should have" — the default latency buckets top out too early to
		// show that at all.
		Lateness: reg.NewHistogram("nexus_timer_lateness_seconds",
			"Seconds between a timer's fire_at and its dispatch.",
			[]float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 300}).Histogram(),
		Fired: reg.NewCounter("nexus_timers_fired_total",
			"Timer dispatch outcomes by kind and result.", "kind", "result"),
		StaleClaims: reg.NewCounter("nexus_timer_stale_claims_total",
			"Timers returned to the pool after a claimant stopped making progress.").Counter(),
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
	if cfg.BacklogInterval <= 0 {
		cfg.BacklogInterval = 15 * time.Second
	}
	if cfg.BacklogAlert <= 0 {
		cfg.BacklogAlert = 1000
	}
	return &Reconciler{db: db, cfg: cfg, log: log, handlers: make(map[string]Handler)}
}

// Handle registers the handler for a timer kind.
func (r *Reconciler) Handle(kind string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = h
}

// registeredKinds lists the kinds with a handler, for the claim filter.
//
// Derived from the handler map rather than from a separate constant so the two can
// never disagree: a kind registered without being claimable would never fire, and a
// kind claimed without a handler would be taken from its real owner.
func (r *Reconciler) registeredKinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
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

	backlog := time.NewTicker(r.cfg.BacklogInterval)
	defer backlog.Stop()

	r.log.InfoContext(ctx, "timer reconciler started",
		"claimant", r.cfg.Claimant, "interval", r.cfg.Interval, "batch_size", r.cfg.BatchSize)

	// Sampled once at startup so a replica that comes up into an existing backlog
	// reports it immediately, instead of exposing a zero gauge for the first
	// BacklogInterval — which reads as "healthy" on a dashboard at the exact moment
	// an operator is looking for the truth.
	r.SampleBacklog(ctx)

	for {
		select {
		case <-ctx.Done():
			r.log.InfoContext(ctx, "timer reconciler stopped")
			return nil
		case <-backlog.C:
			r.SampleBacklog(ctx)
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
	// Claim only the kinds this reconciler has a handler for. The scheduler's timer
	// loop claims the same table for the kinds IT owns (retry_backoff,
	// schedule_to_start), and a row claimed by whoever cannot apply it is a row that
	// stalls until the stale-claim sweeper releases it.
	claimed, err := store.WFMClaimDueTimers(ctx, r.db, r.cfg.Claimant, r.cfg.BatchSize, r.registeredKinds()...)
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
		r.countFired(t.Kind, "no_handler")
		return fmt.Errorf("no handler registered for timer kind %q", t.Kind)
	}

	log := r.log.With("timer_id", t.ID, "kind", t.Kind, "subject_id", t.SubjectID)
	lateness := time.Since(t.FireAt)
	if m := r.cfg.Metrics; m != nil {
		// Recorded for EVERY timer, not only the late ones: a distribution that
		// contains only the samples past a warning threshold cannot show a
		// reconciler drifting from "always prompt" towards "usually prompt", which
		// is the shape of the problem an operator wants to catch early.
		m.Lateness.Observe(lateness.Seconds())
	}
	if lateness > 5*r.cfg.Interval {
		// Lateness is the metric that reveals a reconciler falling behind, which
		// is a silent correctness problem: deadlines still fire, just too late to
		// mean what they promised.
		log.WarnContext(ctx, "timer fired late", "lateness", lateness)
	}

	if err := h(ctx, t); err != nil {
		r.countFired(t.Kind, "error")
		return fmt.Errorf("handle %s timer for %s: %w", t.Kind, t.SubjectID, err)
	}
	r.countFired(t.Kind, "ok")
	return nil
}

func (r *Reconciler) countFired(kind, result string) {
	if m := r.cfg.Metrics; m != nil {
		// kind and result are both from closed sets (the Kind constants, and ok /
		// error / no_handler), so the label cardinality is bounded by construction.
		// Putting subject_id here instead would be one series per run, forever.
		m.Fired.With(metrics.Labels{"kind": kind, "result": result}).Inc()
	}
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
		if m := r.cfg.Metrics; m != nil {
			m.StaleClaims.Add(float64(n))
		}
	}
}

// Backlog reports the due-but-unclaimed count and the total. A growing due count
// means deadlines are firing late.
//
// This must NOT be used as a readiness check. The backlog is a property of the
// shared timers table, so every replica computes the same number and would fail
// readiness together — draining the whole workflow-manager tier from the load
// balancer at the exact moment deadlines are already firing late. Worse, this
// reconciler is what drains the backlog: removing its replicas from service cannot
// help and makes recovery slower. Readiness must answer "can I serve", which for
// this service means "is my database connection healthy" (see health.Handler).
//
// The backlog is exposed as a gauge by SampleBacklog instead, where an operator
// alerts on it without it being able to take the tier down.
func (r *Reconciler) Backlog(ctx context.Context) (due int64, total int64, err error) {
	return store.WFMCountPendingTimers(ctx, r.db)
}

// SampleBacklog reads the backlog once and publishes it to the gauges and, past
// BacklogAlert, the log.
//
// A failed sample is logged and the gauges are LEFT ALONE rather than zeroed. A zero
// reads identically to "the backlog just drained", so zeroing on a database error
// would make an unreachable database look like a recovery on every dashboard.
func (r *Reconciler) SampleBacklog(ctx context.Context) {
	due, total, err := r.Backlog(ctx)
	if err != nil {
		r.log.ErrorContext(ctx, "failed to sample timer backlog; gauges keep their last value", "error", err)
		return
	}
	if m := r.cfg.Metrics; m != nil {
		m.BacklogDue.Set(float64(due))
		m.BacklogTotal.Set(float64(total))
	}
	if due > r.cfg.BacklogAlert {
		// Logged as well as gauged so the condition is visible without a scraper
		// configured. This is the whole replacement for the old readiness check: the
		// operator learns the same fact, and the tier stays in service to drain it.
		r.log.WarnContext(ctx, "timer backlog exceeds the alert threshold; deadlines are firing late",
			"due", due, "total", total, "threshold", r.cfg.BacklogAlert)
	}
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
