// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Metrics are the scheduler's service instruments (docs/OPERATIONS.md, metrics
// catalogue). Every label is drawn from a closed set -- the replica's queue,
// an outcome enum, a loop name -- never from run, attempt or worker ids, so the
// series count is independent of load.
//
// A nil *Metrics is valid and records nothing, so tests and the headless
// construction paths need no metrics wiring.
type Metrics struct {
	queue string

	claims        *metrics.CounterVec
	claimDuration *metrics.HistogramVec
	dispatched    *metrics.CounterVec
	lost          *metrics.CounterVec
	leaseExpiries *metrics.Counter
	deadWorkers   *metrics.Counter
	timers        *metrics.CounterVec
	loopErrors    *metrics.CounterVec
	quotaBlocked  *metrics.CounterVec
	usageWrites   *metrics.CounterVec
	deferred      *metrics.Counter

	pending     *metrics.GaugeVec
	oldestAge   *metrics.GaugeVec
	backlogSeen map[string]bool
	backlogMu   sync.Mutex
}

// maxBacklogQueues caps the queues the backlog gauges report individually.
// Queue names come from workflow specs, so they are author-controlled; past
// the cap, further queues are summed into queue="_other" rather than minting a
// series each.
const maxBacklogQueues = 50

// OtherQueue is the backlog label for queues past maxBacklogQueues.
const OtherQueue = "_other"

// NewMetrics registers the scheduler families on reg for the replica draining
// queue.
func NewMetrics(reg *metrics.Registry, queue string) *Metrics {
	return &Metrics{
		queue: queue,
		claims: reg.NewCounter("nexus_scheduler_claims_total",
			"Claim transactions run, by queue, path (poll = ClaimForWorker on a worker's behalf, push = background dispatch loop) and outcome.",
			"queue", "path", "outcome"),
		claimDuration: reg.NewHistogram("nexus_scheduler_claim_duration_seconds",
			"Latency of one claim transaction (select, place, commit), by queue and path.",
			[]float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
			"queue", "path"),
		dispatched: reg.NewCounter("nexus_scheduler_attempts_dispatched_total",
			"Attempts leased to a worker, by queue and path. rate() of this is claims/sec that produced work.",
			"queue", "path"),
		lost: reg.NewCounter("nexus_scheduler_attempts_lost_total",
			"Attempts declared LOST, by queue, cause (lease_expired, worker_dead) and whether a retry was scheduled.",
			"queue", "cause", "retry"),
		leaseExpiries: reg.NewCounter("nexus_scheduler_lease_expiries_total",
			"Leases the reconciler found past their TTL and expired.", "queue").
			With(metrics.Labels{"queue": queue}),
		deadWorkers: reg.NewCounter("nexus_scheduler_workers_declared_dead_total",
			"Workers the liveness sweep declared DEAD because their heartbeat stopped.", "queue").
			With(metrics.Labels{"queue": queue}),
		timers: reg.NewCounter("nexus_scheduler_timers_fired_total",
			"Scheduler-owned timers handled, by kind and result (ok, error, released).",
			"queue", "kind", "result"),
		loopErrors: reg.NewCounter("nexus_scheduler_loop_errors_total",
			"Background loop iterations that failed, by loop. Any sustained rate also fails /readyz.",
			"queue", "loop"),
		quotaBlocked: reg.NewCounter("nexus_scheduler_quota_blocked_total",
			"Attempts a namespace quota held PENDING in a claim, by dimension (inflight_attempts, cpu_millis, memory_bytes, gpu_count, custom). Counted per examination, so one waiting attempt counts once per claim that looks at it (ADR-0021).",
			"queue", "dimension"),
		usageWrites: reg.NewCounter("nexus_scheduler_usage_ledger_writes_total",
			"Usage-ledger rows written, one per dispatched attempt that ended, by outcome (ADR-0021).",
			"queue", "outcome"),
		deferred: reg.NewCounter("nexus_scheduler_placement_deferrals_total",
			"Pull-path candidates left PENDING for a strictly better live worker (bounded delay scheduling, ADR-0029). Counted per examination.",
			"queue").With(metrics.Labels{"queue": queue}),
		pending: reg.NewGauge("nexus_scheduler_pending_attempts",
			"PENDING attempts per queue, fleet-wide (every replica reports every queue: aggregate with max by (queue)).",
			"queue"),
		oldestAge: reg.NewGauge("nexus_scheduler_oldest_pending_age_seconds",
			"Age of the oldest schedulable PENDING attempt per queue. The queue-age SLO signal.",
			"queue"),
		backlogSeen: map[string]bool{},
	}
}

func (m *Metrics) observeClaim(path, outcome string, d time.Duration, dispatched int) {
	if m == nil {
		return
	}
	m.claims.With(metrics.Labels{"queue": m.queue, "path": path, "outcome": outcome}).Inc()
	m.claimDuration.With(metrics.Labels{"queue": m.queue, "path": path}).Observe(d.Seconds())
	if dispatched > 0 {
		m.dispatched.With(metrics.Labels{"queue": m.queue, "path": path}).Add(float64(dispatched))
	}
}

// QuotaBlocked implements store.QuotaObserver. Custom dimensions are collapsed
// to "custom": their names are author-chosen, and a label per name would make
// the series count depend on tenants' configuration.
func (m *Metrics) QuotaBlocked(dimension string) {
	if m == nil {
		return
	}
	if strings.HasPrefix(dimension, platform.DimCustomPrefix) {
		dimension = "custom"
	}
	m.quotaBlocked.With(metrics.Labels{"queue": m.queue, "dimension": dimension}).Inc()
}

// PlacementDeferred implements store.PlacementObserver.
func (m *Metrics) PlacementDeferred() {
	if m == nil {
		return
	}
	m.deferred.Inc()
}

// UsageRecorded implements store.QuotaObserver.
func (m *Metrics) UsageRecorded(outcome string) {
	if m == nil {
		return
	}
	m.usageWrites.With(metrics.Labels{"queue": m.queue, "outcome": outcome}).Inc()
}

func (m *Metrics) observeLost(l store.LostAttempt, cause string) {
	if m == nil {
		return
	}
	retry := "false"
	if l.Decision.Retry {
		retry = "true"
	}
	m.lost.With(metrics.Labels{"queue": m.queue, "cause": cause, "retry": retry}).Inc()
	if cause == "lease_expired" {
		m.leaseExpiries.Inc()
	}
}

func (m *Metrics) observeDeadWorkers(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.deadWorkers.Add(float64(n))
}

func (m *Metrics) observeTimer(kind, result string) {
	if m == nil {
		return
	}
	m.timers.With(metrics.Labels{"queue": m.queue, "kind": kind, "result": result}).Inc()
}

func (m *Metrics) observeLoopError(loop string) {
	if m == nil {
		return
	}
	m.loopErrors.With(metrics.Labels{"queue": m.queue, "loop": loop}).Inc()
}

// setBacklog replaces the backlog gauges with a fresh sample.
//
// A queue that has drained is absent from the sample and is set to zero
// rather than left at its last value: a stale non-zero backlog is the one
// wrong answer an alert on it cannot tolerate.
func (m *Metrics) setBacklog(sample []store.QueueBacklog) {
	if m == nil {
		return
	}
	m.backlogMu.Lock()
	defer m.backlogMu.Unlock()

	pending := map[string]float64{}
	oldest := map[string]float64{}
	for _, b := range sample {
		q := b.Queue
		if !m.backlogSeen[q] && len(m.backlogSeen) >= maxBacklogQueues {
			q = OtherQueue
		}
		pending[q] += float64(b.Pending)
		if b.OldestAgeSeconds > oldest[q] {
			oldest[q] = b.OldestAgeSeconds
		}
		if q != OtherQueue {
			m.backlogSeen[q] = true
		}
	}
	for q := range m.backlogSeen {
		m.pending.With(metrics.Labels{"queue": q}).Set(pending[q])
		m.oldestAge.With(metrics.Labels{"queue": q}).Set(oldest[q])
	}
	if _, ok := pending[OtherQueue]; ok || m.backlogSeen[OtherQueue] {
		m.backlogSeen[OtherQueue] = true
		m.pending.With(metrics.Labels{"queue": OtherQueue}).Set(pending[OtherQueue])
		m.oldestAge.With(metrics.Labels{"queue": OtherQueue}).Set(oldest[OtherQueue])
	}
}

// backlogSampler periodically samples the PENDING backlog into the gauges.
//
// On its own goroutine and interval, and never on the scrape path: the query
// aggregates a shared table, and a /metrics that queried Postgres would fail
// exactly during the database incident an operator is trying to see.
func runBacklogSampler(ctx context.Context, st *store.SchedulerStore, m *Metrics,
	interval time.Duration, log *slog.Logger) {
	if m == nil || interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		sample, err := st.PendingBacklog(qctx, maxBacklogQueues+1)
		cancel()
		switch {
		case err == nil:
			m.setBacklog(sample)
		case !errors.Is(err, context.Canceled):
			m.observeLoopError("backlog-sampler")
			log.Warn("backlog sample failed; gauges keep their last value", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
