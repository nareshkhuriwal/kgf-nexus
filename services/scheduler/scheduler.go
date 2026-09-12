// Package main implements the Nexus scheduler service: the process that turns
// PENDING attempts into leased work on live workers.
//
// It holds no scheduling state in memory. Correctness comes from the database
// (ADR-0002), which is why replicas need no leader election and are
// interchangeable and disposable — kill any one of them mid-cycle and its
// transaction rolls back, its row locks release, and a peer picks the work up.
package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// LoopConfig are the cadences of the background loops.
type LoopConfig struct {
	// DispatchInterval is the pause between claim cycles when the last cycle
	// found nothing. A busy scheduler does not wait at all: it keeps claiming
	// until the queue is empty, so throughput is not capped by a tick rate.
	DispatchInterval time.Duration
	// DispatchIdleBackoff is the longer pause after an empty queue, so an idle
	// cluster is not burning a Postgres connection on a spin loop.
	DispatchIdleBackoff time.Duration
	ReconcileInterval   time.Duration
	LivenessInterval    time.Duration
	TimerInterval       time.Duration
	// MaxClaimsPerBurst bounds one dispatch burst so the loop yields to
	// shutdown and so a single replica cannot monopolize the queue.
	MaxClaimsPerBurst int
}

// DefaultLoopConfig returns cadences matched to the ADR-0010 defaults: a 30s
// lease with 5s renewal means six missed renewals before expiry, so a 2s
// reconcile pass detects expiry well inside the window an operator expects.
func DefaultLoopConfig() LoopConfig {
	return LoopConfig{
		DispatchInterval:    100 * time.Millisecond,
		DispatchIdleBackoff: 1 * time.Second,
		ReconcileInterval:   2 * time.Second,
		LivenessInterval:    5 * time.Second,
		TimerInterval:       1 * time.Second,
		MaxClaimsPerBurst:   64,
	}
}

// Scheduler runs the dispatch, reconcile, liveness and timer loops.
type Scheduler struct {
	store *store.SchedulerStore
	log   *slog.Logger
	cfg   LoopConfig

	// notify is called AFTER a dispatch transaction commits (invariant 4). It is
	// a function rather than a NATS client so the correctness-critical path has
	// no bus dependency at all: the bus is a hint (§3.2), and if the publish
	// fails the worker's next poll or the reconciler picks the attempt up.
	// Latency degrades; correctness does not.
	notify func(context.Context, *store.DispatchResult)

	// healthy tracks whether the last loop iteration succeeded, for /readyz.
	mu      sync.RWMutex
	lastErr error
}

// NewScheduler wires the loops. notify may be nil when no bus is configured.
func NewScheduler(
	st *store.SchedulerStore, log *slog.Logger, cfg LoopConfig,
	notify func(context.Context, *store.DispatchResult),
) *Scheduler {
	if notify == nil {
		notify = func(context.Context, *store.DispatchResult) {}
	}
	return &Scheduler{store: st, log: log, cfg: cfg, notify: notify}
}

// Run starts every loop and blocks until ctx is cancelled and all loops have
// returned.
//
// Loops are independent goroutines with independent cadences because they have
// genuinely different urgency: dispatch latency is user-visible, whereas a
// liveness sweep every 5s is ample. Coupling them into one tick would force the
// slowest concern's cadence on the fastest.
func (s *Scheduler) Run(ctx context.Context) {
	var wg sync.WaitGroup
	loops := []struct {
		name string
		fn   func(context.Context)
	}{
		{"dispatch", s.dispatchLoop},
		{"lease-reconciler", s.reconcileLoop},
		{"liveness-sweeper", s.livenessLoop},
		{"timer-runner", s.timerLoop},
	}
	for _, l := range loops {
		wg.Add(1)
		go func(name string, fn func(context.Context)) {
			defer wg.Done()
			s.log.Info("loop started", "loop", name)
			fn(ctx)
			s.log.Info("loop stopped", "loop", name)
		}(l.name, l.fn)
	}
	wg.Wait()
}

// LastError reports the most recent loop error, for /readyz.
func (s *Scheduler) LastError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

func (s *Scheduler) setErr(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

// dispatchLoop drains the queue in bursts.
func (s *Scheduler) dispatchLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		dispatched, idle := s.dispatchBurst(ctx)
		wait := s.cfg.DispatchInterval
		if idle && dispatched == 0 {
			wait = s.cfg.DispatchIdleBackoff
		}
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

// dispatchBurst claims until the queue is drained, the burst cap is hit, or an
// error occurs. It reports whether the queue went empty, which is what earns the
// longer idle backoff.
func (s *Scheduler) dispatchBurst(ctx context.Context) (dispatched int, idle bool) {
	for i := 0; i < s.cfg.MaxClaimsPerBurst; i++ {
		if ctx.Err() != nil {
			return dispatched, false
		}
		rep, err := s.store.Claim(ctx, time.Now().UTC())
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return dispatched, false
			}
			s.setErr(err)
			s.log.Error("claim cycle failed", "err", err)
			return dispatched, true
		}
		s.setErr(nil)

		switch rep.Outcome {
		case store.ClaimDispatched:
			dispatched++
			d := rep.Dispatch
			s.log.Info("attempt dispatched",
				"run_id", d.RunID, "attempt_id", d.AttemptID, "node_id", d.NodeID,
				"worker_id", d.WorkerID, "attempt_number", rep.Attempt.AttemptNumber,
				"lease_expires_at", d.ExpiresAt)
			// Strictly after commit (invariant 4). Publishing inside the claim
			// transaction would require a distributed transaction across
			// Postgres and NATS, which is the wrong trade.
			s.notify(ctx, d)

		case store.ClaimBlocked:
			// Not an error and not a failure: the cluster is busy and the
			// attempt now carries a structured reason the UI can show (§5.3).
			s.log.Debug("attempt left pending",
				"run_id", rep.Attempt.RunID, "attempt_id", rep.Attempt.ID,
				"node_id", rep.Attempt.NodeID, "reason", rep.Reason,
				"effective_priority", rep.Attempt.EffectivePriority)
			return dispatched, true

		case store.ClaimRaceLost:
			// A peer replica won the CAS. This is the guard working, not a
			// fault, so it is logged at debug and the burst continues.
			s.log.Debug("claim lost to a peer replica",
				"run_id", rep.Attempt.RunID, "attempt_id", rep.Attempt.ID)

		case store.ClaimNothingPending:
			return dispatched, true
		}
	}
	return dispatched, false
}

// reconcileLoop expires leases whose TTL has passed.
func (s *Scheduler) reconcileLoop(ctx context.Context) {
	for {
		if !sleepCtx(ctx, s.cfg.ReconcileInterval) {
			return
		}
		lost, err := s.store.ReconcileExpiredLeases(ctx, time.Now().UTC(), 100)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.setErr(err)
			s.log.Error("lease reconciliation failed", "err", err)
		}
		for _, l := range lost {
			s.logLost(l, "lease_expired")
		}
	}
}

// livenessLoop declares workers dead when their heartbeat stops.
func (s *Scheduler) livenessLoop(ctx context.Context) {
	for {
		if !sleepCtx(ctx, s.cfg.LivenessInterval) {
			return
		}
		dead, err := s.store.SweepDeadWorkers(ctx, time.Now().UTC(), 50)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.setErr(err)
			s.log.Error("liveness sweep failed", "err", err)
		}
		for _, w := range dead {
			s.log.Warn("worker declared dead",
				"worker_id", w.ID, "hostname", w.Hostname,
				"last_heartbeat_at", w.LastHeartbeat,
				"expired_attempts", len(w.ExpiredAttempts))
			for _, l := range w.ExpiredAttempts {
				s.logLost(l, "worker_dead")
			}
		}
	}
}

// timerLoop fires due timers: retry backoffs and schedule_to_start deadlines.
func (s *Scheduler) timerLoop(ctx context.Context) {
	kinds := []string{domain.TimerRetryBackoff, domain.TimerScheduleToStart}
	for {
		if !sleepCtx(ctx, s.cfg.TimerInterval) {
			return
		}
		now := time.Now().UTC()
		fired, err := s.store.ClaimDueTimers(ctx, now, kinds, 50)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				s.setErr(err)
				s.log.Error("timer claim failed", "err", err)
			}
			continue
		}
		for _, t := range fired {
			s.handleTimer(ctx, now, t)
		}
	}
}

func (s *Scheduler) handleTimer(ctx context.Context, now time.Time, t store.FiredTimer) {
	var err error
	switch t.Kind {
	case domain.TimerRetryBackoff:
		var created *store.RetryCreated
		created, err = s.store.CreatePendingRetry(ctx, now, t)
		if err == nil && created != nil {
			s.log.Info("retry attempt created",
				"run_id", created.RunID, "node_id", created.NodeID,
				"attempt_id", created.NewAttemptID,
				"previous_attempt_id", created.PrevAttemptID,
				"attempt_number", created.AttemptNumber)
		}
	case domain.TimerScheduleToStart:
		err = s.store.ExpireScheduleToStart(ctx, now, t)
	default:
		// A kind we did not ask for: release it rather than deleting it, so the
		// service that owns it can still act on it.
		if rerr := s.store.ReleaseTimer(ctx, t.ID); rerr != nil {
			s.log.Error("failed to release foreign timer", "timer_id", t.ID, "err", rerr)
		}
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		s.setErr(err)
		s.log.Error("timer handling failed", "timer_id", t.ID, "kind", t.Kind, "err", err)
		// Return it to the pool so a transient DB error does not permanently
		// drop a deadline. A dropped retry_backoff timer means a workflow that
		// silently never resumes.
		if rerr := s.store.ReleaseTimer(ctx, t.ID); rerr != nil {
			s.log.Error("failed to release timer after failure", "timer_id", t.ID, "err", rerr)
		}
	}
}

// logLost records a LOST attempt and its retry decision. The decision reason is
// logged even when a retry happened, because "why did this retry?" is as
// operational a question as "why did it not?".
func (s *Scheduler) logLost(l store.LostAttempt, cause string) {
	attrs := []any{
		"run_id", l.RunID, "attempt_id", l.AttemptID, "node_id", l.NodeID,
		"worker_id", l.WorkerID, "attempt_number", l.AttemptNumber,
		"cause", cause, "retry", l.Decision.Retry, "reason", l.Decision.Reason,
	}
	if l.Decision.Retry {
		attrs = append(attrs, "retry_at", l.RetryAt)
	}
	switch {
	case l.Decision.Reason == domain.ReasonIndeterminateRequiresHuman:
		// The §6.5 refusal. WARN, not INFO: a human must decide, and a run is
		// stalled until they do.
		s.log.Warn("attempt LOST; refusing to auto-retry non-idempotent work", attrs...)
	case l.Decision.Warn:
		s.log.Warn("attempt LOST; retrying with possible duplicate execution", attrs...)
	default:
		s.log.Info("attempt LOST", attrs...)
	}
}

// sleepCtx waits for d or until ctx is done, reporting false when it should stop.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
