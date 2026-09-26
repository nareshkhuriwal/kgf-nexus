//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// dispatchOne claims exactly one attempt and fails the test if nothing was
// dispatched, so the reconciler tests start from a known real dispatch rather
// than a hand-written DISPATCHED row that might not match what Claim produces.
func dispatchOne(t *testing.T, st *store.SchedulerStore) store.ClaimReport {
	t.Helper()
	rep, err := st.Claim(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if rep.Outcome != store.ClaimDispatched {
		t.Fatalf("claim outcome = %s, want dispatched (reason %q)", rep.Outcome, rep.Reason)
	}
	return rep
}

// TestLeaseExpiryCreatesNewAttemptNeverMutatesTerminal covers invariants 2 and
// 9 together: an expired lease becomes LOST (not FAILED), and the retry is a
// NEW row whose idempotency key is IDENTICAL to the terminal attempt's.
//
// That last assertion is the one that matters. It is the §7.2 t+40 step — "its
// idempotency_key EQUALS A's, so any side effect A already committed is
// deduplicated rather than repeated. This is the whole design working."
func TestLeaseExpiryCreatesNewAttemptNeverMutatesTerminal(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	workerID := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	spec := nodeSpec{
		TaskRef: "test.task", Version: 1, MaxAttempts: 3,
		Idempotency:    "required",
		StartToCloseMs: 3600000,
		Retry:          map[string]any{"strategy": "fixed", "initial_ms": 1, "max_ms": 1, "jitter": "none"},
	}
	attemptID := f.addAttempt("work", 100, vec{CPUMillis: 1000, MemoryBytes: 1 << 30}, spec, time.Now().UTC())

	st := f.newStore(nil)
	rep := dispatchOne(t, st)
	if rep.Dispatch.AttemptID != attemptID {
		t.Fatalf("dispatched the wrong attempt")
	}
	if _, _, ok := f.leaseFor(attemptID); !ok {
		t.Fatal("no lease was created for a dispatched attempt")
	}
	if got := f.workerCommitted(workerID).CPUMillis; got != 1000 {
		t.Fatalf("committed cpu after dispatch = %d, want 1000", got)
	}

	f.expireLease(attemptID)

	lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(lost) != 1 {
		t.Fatalf("reconciled %d attempts, want 1", len(lost))
	}

	// LOST, not FAILED: we do not know whether the work ran (invariant 2).
	if got := f.attemptState(attemptID); got != "LOST" {
		t.Errorf("expired attempt state = %s, want LOST", got)
	}
	if !lost[0].Decision.Retry {
		t.Fatalf("idempotency=required + LOST must auto-retry per §6.5; got reason %q", lost[0].Decision.Reason)
	}
	if lost[0].Decision.Reason != domain.ReasonRetryLost {
		t.Errorf("reason = %q, want %q", lost[0].Decision.Reason, domain.ReasonRetryLost)
	}

	// Capacity returned, lease gone, slot released.
	if got := f.workerCommitted(workerID).CPUMillis; got != 0 {
		t.Errorf("committed cpu after expiry = %d, want 0; capacity leaked", got)
	}
	if _, _, ok := f.leaseFor(attemptID); ok {
		t.Error("lease outlived the attempt; a zombie worker's stale token could still match")
	}
	var reservations int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM reservations WHERE attempt_id = $1`, attemptID).Scan(&reservations); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if reservations != 0 {
		t.Errorf("%d reservations survived the expiry", reservations)
	}

	// The retry is scheduled as a timer, not an immediate attempt: backoff must
	// be respected, or a failing dependency gets hammered.
	kinds := f.timersFor(attemptID)
	if !contains(kinds, domain.TimerRetryBackoff) {
		t.Fatalf("timers for the lost attempt = %v, want a %s", kinds, domain.TimerRetryBackoff)
	}
	// Deadline timers for a now-terminal attempt must be gone so they cannot
	// fire against history.
	if contains(kinds, domain.TimerStartToClose) {
		t.Error("start_to_close timer survived a terminal attempt")
	}

	// Fire the backoff timer.
	fired, err := st.ClaimDueTimers(ctx, time.Now().UTC().Add(time.Minute),
		[]string{domain.TimerRetryBackoff}, 10)
	if err != nil {
		t.Fatalf("claim timers: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("claimed %d timers, want 1", len(fired))
	}
	created, err := st.CreatePendingRetry(ctx, time.Now().UTC(), fired[0])
	if err != nil {
		t.Fatalf("create retry: %v", err)
	}
	if created == nil {
		t.Fatal("no retry attempt was created")
	}

	// ── Invariant 9: a NEW row, and the old one untouched. ───────────────
	rows := f.attemptsForNode("work")
	if len(rows) != 2 {
		t.Fatalf("node has %d attempts, want 2 (the terminal one plus the retry)", len(rows))
	}
	if rows[0].ID != attemptID || rows[0].Number != 1 || rows[0].State != "LOST" {
		t.Errorf("the terminal attempt was mutated: %+v", rows[0])
	}
	if rows[1].Number != 2 || rows[1].State != "PENDING" {
		t.Errorf("retry row = %+v, want attempt_number 2 in PENDING", rows[1])
	}
	if rows[1].ID == attemptID {
		t.Error("the retry reused the terminal attempt's row")
	}

	// ── Invariant 1: the key identifies THE WORK, not THE TRY. ───────────
	if rows[0].IdemKey != rows[1].IdemKey {
		t.Errorf("idempotency key changed across the retry:\n  attempt 1: %s\n  attempt 2: %s\n"+
			"this breaks downstream deduplication and is how a customer gets charged twice",
			rows[0].IdemKey, rows[1].IdemKey)
	}

	// The audit trail must explain what happened.
	events := f.eventKinds(attemptID)
	for _, want := range []string{domain.EventAttemptDispatched, domain.EventAttemptLost, domain.EventRetryScheduled} {
		if !contains(events, want) {
			t.Errorf("run_events for the lost attempt = %v, missing %s", events, want)
		}
	}

	// The retry is dispatchable, which closes the loop.
	rep2 := dispatchOne(t, st)
	if rep2.Dispatch.AttemptID != rows[1].ID {
		t.Errorf("dispatched %s, want the retry attempt %s", rep2.Dispatch.AttemptID, rows[1].ID)
	}
	// A fresh lease token per generation: the old token must never still work.
	if rep2.Dispatch.LeaseToken == rep.Dispatch.LeaseToken {
		t.Error("the retry reused the previous lease token; fencing would not distinguish the generations")
	}
}

// TestLostWithIdempotencyNoneRefusesAutoRetry is the §6.5 refusal, end to end
// against the real database. A platform that quietly re-runs work the author
// declared unsafe to re-run has chosen the wrong failure mode.
func TestLostWithIdempotencyNoneRefusesAutoRetry(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	attemptID := f.addAttempt("charge", 100, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "billing.charge", Version: 1, MaxAttempts: 5, Idempotency: "none"},
		time.Now().UTC())

	st := f.newStore(nil)
	dispatchOne(t, st)
	f.expireLease(attemptID)

	lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(lost) != 1 {
		t.Fatalf("reconciled %d, want 1", len(lost))
	}
	if lost[0].Decision.Retry {
		t.Fatal("LOST + idempotency=none must NOT auto-retry (invariant 2, §6.5)")
	}
	if lost[0].Decision.Reason != domain.ReasonIndeterminateRequiresHuman {
		t.Errorf("reason = %q, want %q", lost[0].Decision.Reason, domain.ReasonIndeterminateRequiresHuman)
	}

	// No retry timer, and therefore no second attempt, may exist. The attempt
	// budget was nowhere near exhausted (1 of 5), so only the idempotency
	// declaration can be what stopped it.
	if kinds := f.timersFor(attemptID); contains(kinds, domain.TimerRetryBackoff) {
		t.Errorf("a retry_backoff timer was scheduled for non-idempotent lost work: %v", kinds)
	}
	if rows := f.attemptsForNode("charge"); len(rows) != 1 {
		t.Errorf("node has %d attempts, want 1; no retry may be created", len(rows))
	}

	// The event log must record this as a decision deferred to a human, with a
	// distinct kind, so the UI can offer `attempt retry --force` rather than
	// presenting the run as simply broken.
	events := f.eventKinds(attemptID)
	if !contains(events, domain.EventIndeterminateOutcome) {
		t.Errorf("events = %v, missing %s", events, domain.EventIndeterminateOutcome)
	}
	if contains(events, domain.EventRetryScheduled) {
		t.Errorf("events = %v, must not claim a retry was scheduled", events)
	}
}

// TestBestEffortLostWarnsAboutDuplicateExecution covers the middle §6.5 row: we
// retry, but we owe the operator an explicit warning that the work may run twice.
func TestBestEffortLostWarnsAboutDuplicateExecution(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	f.addWorker(vec{CPUMillis: 4000}, nil, nil)
	attemptID := f.addAttempt("besteffort", 100, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "test.task", Version: 1, MaxAttempts: 3, Idempotency: "best_effort"},
		time.Now().UTC())

	st := f.newStore(nil)
	dispatchOne(t, st)
	f.expireLease(attemptID)

	lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(lost) != 1 || !lost[0].Decision.Retry || !lost[0].Decision.Warn {
		t.Fatalf("best_effort + LOST must retry WITH a warning; got %+v", lost)
	}
	if !contains(f.eventKinds(attemptID), domain.EventPossibleDuplicateExecution) {
		t.Errorf("events = %v, missing %s", f.eventKinds(attemptID), domain.EventPossibleDuplicateExecution)
	}
}

// TestWorkerLivenessSweepReclaimsCapacity covers §5.5 and the §7.2 timeline: a
// worker that stops heartbeating becomes DEAD, its attempts become LOST, and its
// capacity returns to the pool immediately rather than after each lease expires.
func TestWorkerLivenessSweepReclaimsCapacity(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	doomed := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	spec := nodeSpec{TaskRef: "test.task", Version: 1, MaxAttempts: 3, Idempotency: "required"}

	st := f.newStore(func(c *store.ClaimConfig) {
		// A long TTL proves the sweep does not merely wait for lease expiry: if
		// it did, nothing would be reclaimed for five minutes.
		c.LeaseTTL = 5 * time.Minute
		c.LivenessWindow = 30 * time.Second
	})

	a1 := f.addAttempt("a1", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	a2 := f.addAttempt("a2", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	dispatchOne(t, st)
	dispatchOne(t, st)

	if got := f.workerCommitted(doomed).CPUMillis; got != 2000 {
		t.Fatalf("committed cpu = %d, want 2000", got)
	}

	f.staleHeartbeat(doomed, 90*time.Second)

	dead, err := st.SweepDeadWorkers(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("swept %d workers, want 1", len(dead))
	}
	if got := f.workerState(doomed); got != "DEAD" {
		t.Errorf("worker state = %s, want DEAD", got)
	}
	if len(dead[0].ExpiredAttempts) != 2 {
		t.Errorf("expired %d attempts, want 2", len(dead[0].ExpiredAttempts))
	}
	if got := f.attemptState(a1); got != "LOST" {
		t.Errorf("a1 state = %s, want LOST", got)
	}
	if got := f.attemptState(a2); got != "LOST" {
		t.Errorf("a2 state = %s, want LOST", got)
	}
	if got := f.workerCommitted(doomed).CPUMillis; got != 0 {
		t.Errorf("committed cpu after death = %d, want 0; a dead machine's capacity leaked", got)
	}
	if !contains(f.eventKinds(a1), domain.EventWorkerDead) {
		t.Errorf("events for a1 = %v, missing %s", f.eventKinds(a1), domain.EventWorkerDead)
	}

	// A DEAD worker must receive no further dispatch, even though its capacity
	// now looks entirely free.
	f.addAttempt("a3", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	rep, err := st.Claim(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim after worker death: %v", err)
	}
	if rep.Outcome == store.ClaimDispatched {
		t.Fatal("dispatched work to a DEAD worker")
	}
}

// TestBlockedAttemptRecordsStructuredReason covers §5.3 step 3: an attempt that
// cannot be placed stays PENDING with a machine-parseable explanation. Failing it
// would spend its retry budget on a capacity problem and report a task failure
// for what is an operations issue.
func TestBlockedAttemptRecordsStructuredReason(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// Exactly the §7.3 numbers: 4 GPUs, 3 free, a task needing 4.
	workerID := f.addWorker(vec{CPUMillis: 16000, GPUCount: 4}, nil, nil)
	f.occupy(workerID, "incumbent", vec{GPUCount: 1})

	attemptID := f.addAttempt("gpu-hog", 100, vec{GPUCount: 4},
		nodeSpec{TaskRef: "ml.train", Version: 1}, time.Now().UTC())

	st := f.newStore(nil)
	rep, err := st.Claim(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if rep.Outcome != store.ClaimBlocked {
		t.Fatalf("outcome = %s, want blocked", rep.Outcome)
	}
	if got := f.attemptState(attemptID); got != "PENDING" {
		t.Errorf("state = %s, want PENDING; capacity pressure must never fail an attempt", got)
	}
	// The §5.3 example string, exactly.
	if want := "waiting_for_resources: gpu (need 4, free 3)"; f.pendingReason(attemptID) != want {
		t.Errorf("pending_reason = %q, want %q", f.pendingReason(attemptID), want)
	}
}

// TestNodeSelectorAndExecutorArePlacementConstraints covers the hard placement
// constraints of §5.3 step 2. A mismatch must leave the attempt PENDING with an
// explanation, never place it on an incompatible worker.
func TestNodeSelectorAndExecutorArePlacementConstraints(t *testing.T) {
	t.Parallel()
	f := newFixture(t, newTestPool(t))
	ctx := context.Background()

	// A worker in the wrong zone with the wrong executor.
	f.addWorker(vec{CPUMillis: 8000}, map[string]string{"zone": "us-west-2a"}, []string{"subprocess"})

	labelMismatch := f.addAttempt("needs-east", 100, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1, NodeSelector: map[string]string{"zone": "us-east-1a"}},
		time.Now().UTC())

	st := f.newStore(nil)
	rep, err := st.Claim(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if rep.Outcome != store.ClaimBlocked {
		t.Fatalf("outcome = %s, want blocked on the label mismatch", rep.Outcome)
	}
	if r := f.pendingReason(labelMismatch); !strings.HasPrefix(r, "waiting_for_worker:") {
		t.Errorf("pending_reason = %q, want a waiting_for_worker explanation", r)
	}

	// A matching label but an unsupported executor must still be blocked.
	f.addWorker(vec{CPUMillis: 8000}, map[string]string{"zone": "us-east-1a"}, []string{"subprocess"})
	f.addAttempt("needs-docker", 200, vec{CPUMillis: 1000},
		nodeSpec{TaskRef: "t", Version: 1,
			NodeSelector: map[string]string{"zone": "us-east-1a"}, Executor: "docker"},
		time.Now().UTC())

	rep, err = st.Claim(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if rep.Outcome == store.ClaimDispatched && rep.Attempt.NodeID == "needs-docker" {
		t.Fatal("dispatched a docker task to a worker that only advertises subprocess")
	}

	// With a compatible worker present, the label-matched task must now place.
	f.addWorker(vec{CPUMillis: 8000}, map[string]string{"zone": "us-east-1a"}, []string{"docker"})
	placed := false
	for i := 0; i < 5; i++ {
		rep, err = st.Claim(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if rep.Outcome == store.ClaimDispatched {
			placed = true
			break
		}
		if rep.Outcome == store.ClaimNothingPending {
			break
		}
	}
	if !placed {
		t.Error("no attempt placed even though a fully compatible worker exists")
	}
}

// TestConcurrencySemaphoreLimitsGlobalDispatch covers §5.3 step 4. The limit is
// global across replicas, so it must be enforced in the database, not in memory.
func TestConcurrencySemaphoreLimitsGlobalDispatch(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	// Ample capacity: the only thing that can hold tasks back is the semaphore.
	f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 64 << 30}, nil, nil)

	key := "sem-" + f.queue
	spec := nodeSpec{
		TaskRef: "t", Version: 1,
		ConcurrencyKey: key, ConcurrencyLimit: 2,
	}
	for i := 0; i < 5; i++ {
		f.addAttempt(nodeName("sem", i), 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	}

	st := f.newStore(nil)
	dispatched := 0
	var lastReason string
	for i := 0; i < 10; i++ {
		rep, err := st.Claim(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if rep.Outcome == store.ClaimDispatched {
			dispatched++
			continue
		}
		lastReason = rep.Reason
		break
	}
	if dispatched != 2 {
		t.Errorf("dispatched %d attempts under a limit of 2", dispatched)
	}
	if !strings.HasPrefix(lastReason, "waiting_for_concurrency:") {
		t.Errorf("blocked reason = %q, want a waiting_for_concurrency explanation", lastReason)
	}

	var slots int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM concurrency_slots WHERE key = $1`, key).Scan(&slots); err != nil {
		t.Fatalf("count slots: %v", err)
	}
	if slots != 2 {
		t.Errorf("%d concurrency slots held, want 2", slots)
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// attemptsUsed reads the node's charged retry budget.
func (f *fixture) attemptsUsed(nodeID string) int32 {
	f.t.Helper()
	var n int32
	if err := f.pool.QueryRow(context.Background(),
		`SELECT attempts_used FROM task_nodes WHERE run_id = $1 AND node_id = $2`,
		f.runID, nodeID).Scan(&n); err != nil {
		f.t.Fatalf("read attempts_used for %s: %v", nodeID, err)
	}
	return n
}

// TestRetryChargesTheAttemptBudget covers the §5.1 RETRY predicate's counter.
//
// domain.ShouldRetry compares task_nodes.attempts_used against max_attempts, and
// retries are created by CreatePendingRetry — not by the dag-engine, which is the
// only other place that bumps the column. Without the increment inside the retry
// transaction the counter stayed at its initial value no matter how many times a
// node was retried, so max_attempts capped nothing and a task failing a retryable
// class would be retried indefinitely.
func TestRetryChargesTheAttemptBudget(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	spec := nodeSpec{
		TaskRef: "test.task", Version: 1, MaxAttempts: 3,
		Idempotency:    "required",
		StartToCloseMs: 3600000,
		Retry:          map[string]any{"strategy": "fixed", "initial_ms": 1, "max_ms": 1, "jitter": "none"},
	}
	attemptID := f.addAttempt("budgeted", 100, vec{CPUMillis: 1000, MemoryBytes: 1 << 30}, spec, time.Now().UTC())

	st := f.newStore(nil)
	dispatchOne(t, st)

	before := f.attemptsUsed("budgeted")

	// A lease expiry is the cheapest way to reach the retry path: it makes the
	// attempt LOST, which idempotency=required retries (§6.5).
	f.expireLease(attemptID)
	lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(lost) != 1 || !lost[0].Decision.Retry {
		t.Fatalf("expected one retryable LOST attempt, got %d", len(lost))
	}

	// Fire the backoff timer, which is what actually creates the retry attempt.
	fired, err := st.ClaimDueTimers(ctx, time.Now().UTC().Add(time.Minute),
		[]string{domain.TimerRetryBackoff}, 10)
	if err != nil {
		t.Fatalf("claim retry timer: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("claimed %d retry_backoff timers, want 1", len(fired))
	}
	created, err := st.CreatePendingRetry(ctx, time.Now().UTC(), fired[0])
	if err != nil {
		t.Fatalf("create retry: %v", err)
	}
	if created == nil {
		t.Fatal("no retry attempt was created")
	}

	if got := f.attemptsUsed("budgeted"); got != before+1 {
		t.Errorf("attempts_used = %d, want %d; the retry budget was not charged, "+
			"so max_attempts would never cap retries", got, before+1)
	}

	// The new attempt is a NEW row, never a mutation of the terminal one
	// (invariant 9), and it must share the idempotency key (invariant 1).
	rows := f.attemptsForNode("budgeted")
	if len(rows) != 2 {
		t.Fatalf("node has %d attempts, want 2", len(rows))
	}
	if rows[0].IdemKey != rows[1].IdemKey {
		t.Errorf("idempotency keys differ across retries (%q vs %q); invariant 1 broken",
			rows[0].IdemKey, rows[1].IdemKey)
	}
}

// setRunState forces the run into a lifecycle state, standing in for the
// workflow-manager transition the scheduler only ever observes.
func (f *fixture) setRunState(state string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE workflow_runs SET state = $2::run_state WHERE id = $1`, f.runID, state); err != nil {
		f.t.Fatalf("set run state %s: %v", state, err)
	}
}

// TestPauseDoesNotCancelInFlightWork is §5.2's central promise: pause stops new
// dispatch and leaves running work alone; cancel is the operation that stops work.
//
// PendingCancellations feeds the gateway, which turns EVERY id it returns into a
// CANCEL_ATTEMPT directive, and the agent implements that as SIGTERM/grace/SIGKILL.
// So including PAUSING in this query killed the in-flight attempts of a paused run:
// the attempt came back FAILED with "workload terminated by signal terminated" and
// the retry budget was charged for a failure the task never caused. An operator who
// paused a run to investigate destroyed the compute they were preserving.
//
// This test pins the distinction that no test covered before: a PAUSING run reports
// nothing to cancel, while CANCELLING still reports everything.
func TestPauseDoesNotCancelInFlightWork(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	workerID := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	attemptID := f.occupy(workerID, "long-running", vec{CPUMillis: 1000, MemoryBytes: 1 << 30})
	st := f.newStore(nil)

	// RUNNING: nothing to stop.
	if ids, err := st.PendingCancellations(ctx, workerID); err != nil {
		t.Fatalf("pending cancellations (RUNNING): %v", err)
	} else if len(ids) != 0 {
		t.Fatalf("a RUNNING run reported %d cancellations, want 0", len(ids))
	}

	// PAUSING: still nothing to stop. This is the regression.
	f.setRunState("PAUSING")
	ids, err := st.PendingCancellations(ctx, workerID)
	if err != nil {
		t.Fatalf("pending cancellations (PAUSING): %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("a PAUSING run reported %v as cancellable, want none; the gateway "+
			"turns these into CANCEL_ATTEMPT and the agent SIGKILLs them, so pause "+
			"would destroy the in-flight work §5.2 promises to preserve", ids)
	}

	// CANCELLING: the attempt MUST be reported, or a cancelled run leaves the
	// worker burning CPU on orphaned work with no channel left to stop it.
	f.setRunState("CANCELLING")
	ids, err = st.PendingCancellations(ctx, workerID)
	if err != nil {
		t.Fatalf("pending cancellations (CANCELLING): %v", err)
	}
	if len(ids) != 1 || ids[0] != attemptID {
		t.Errorf("a CANCELLING run reported %v, want [%s]", ids, attemptID)
	}
}

// TestTimedOutRunStopsInFlightWorkThroughTheHeartbeat (F160). A run deadline
// kills the run's work: the workflow-manager marks the run TIMED_OUT and then
// asks the scheduler to cancel the attempts. That second call was best-effort
// with no retry -- when it failed, the run was TIMED_OUT and its attempts ran
// on, holding their reservations, because the heartbeat only derived
// cancellations from CANCELLING and CANCELLED runs. The run state is the
// durable fact; deriving the kill from it makes a lost call harmless.
func TestTimedOutRunStopsInFlightWorkThroughTheHeartbeat(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	workerID := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	attemptID := f.occupy(workerID, "overran", vec{CPUMillis: 1000, MemoryBytes: 1 << 30})

	f.setRunState("TIMED_OUT")
	ids, err := f.newStore(nil).PendingCancellations(ctx, workerID)
	if err != nil {
		t.Fatalf("pending cancellations (TIMED_OUT): %v", err)
	}
	if len(ids) != 1 || ids[0] != attemptID {
		t.Errorf("a TIMED_OUT run reported %v, want [%s]; a lost cancel call would leave "+
			"its work running past the deadline with nothing left to stop it", ids, attemptID)
	}
}

// TestCancelledAttemptIsReportedRegardlessOfRunState covers the other source in
// the union: the attempt itself was moved terminal (an operator cancelled one
// node), so the work on the worker is orphaned even though the run continues.
func TestCancelledAttemptIsReportedRegardlessOfRunState(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	workerID := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	attemptID := f.occupy(workerID, "orphaned", vec{CPUMillis: 1000, MemoryBytes: 1 << 30})

	if _, err := pool.Exec(ctx,
		`UPDATE task_attempts SET state='CANCELLED', ended_at=now() WHERE id=$1`, attemptID); err != nil {
		t.Fatalf("cancel attempt: %v", err)
	}

	ids, err := f.newStore(nil).PendingCancellations(ctx, workerID)
	if err != nil {
		t.Fatalf("pending cancellations: %v", err)
	}
	if len(ids) != 1 || ids[0] != attemptID {
		t.Errorf("a CANCELLED attempt on a RUNNING run reported %v, want [%s]; "+
			"the worker would keep executing orphaned work", ids, attemptID)
	}
}
