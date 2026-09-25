//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// The two liveness sweepers (CONTRACT-FINDINGS F106) and partial-failure
// handling in the scheduler's recovery passes (F106, F108), against real
// Postgres.

// rmDeclaresDead does exactly what the resource-manager's Sweeper.sweepOnce
// does for one silent worker: the DEAD transition, then the capacity release.
// It deliberately leaves the attempts alone, as the real sweeper does.
func rmDeclaresDead(t *testing.T, f *fixture, workerID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	rm := store.NewRMStore(f.pool)
	applied, err := rm.SetWorkerState(ctx, workerID.String(), store.WorkerDead)
	if err != nil || !applied {
		t.Fatalf("resource-manager DEAD transition = %v, %v; want applied", applied, err)
	}
	if _, err := rm.ReleaseAllForWorker(ctx, workerID.String()); err != nil {
		t.Fatalf("resource-manager release: %v", err)
	}
}

// longLeaseStore is a scheduler whose leases outlive the test by far, so an
// attempt that becomes LOST can only have been recovered by the liveness
// sweep, never by lease expiry.
func longLeaseStore(f *fixture) *store.SchedulerStore {
	return f.newStore(func(c *store.ClaimConfig) {
		c.LeaseTTL = 10 * time.Minute
		c.LivenessWindow = 30 * time.Second
	})
}

// TestResourceManagerWinsTheDeadRaceAttemptsStillRecover is F106's main case:
// the resource-manager marks the worker DEAD first, so the scheduler's sweep
// no longer sees a live stale worker. Its in-flight attempts must still become
// LOST on the scheduler's next sweep, not a lease TTL later.
func TestResourceManagerWinsTheDeadRaceAttemptsStillRecover(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	doomed := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := longLeaseStore(f)
	spec := nodeSpec{TaskRef: "test.task", Version: 1, MaxAttempts: 3, Idempotency: "required",
		Retry: map[string]any{"strategy": "fixed", "initial_ms": 1, "max_ms": 1, "jitter": "none"}}
	a1 := f.addAttempt("a1", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	a2 := f.addAttempt("a2", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	dispatchOne(t, st)
	dispatchOne(t, st)
	if err := st.MarkStarted(ctx, a2, time.Now().UTC()); err != nil {
		t.Fatalf("mark a2 started: %v", err)
	}

	f.staleHeartbeat(doomed, 90*time.Second)
	rmDeclaresDead(t, f, doomed)
	if got := f.workerCommitted(doomed).CPUMillis; got != 0 {
		t.Fatalf("committed after the resource-manager release = %d, want 0", got)
	}
	for _, id := range []uuid.UUID{a1, a2} {
		if s := f.attemptState(id); s != "DISPATCHED" && s != "RUNNING" {
			t.Fatalf("attempt %s state = %s before the scheduler sweep; the test premise is broken", id, s)
		}
	}

	// ONE scheduler sweep, i.e. within one sweep interval.
	start := time.Now()
	dead, err := st.SweepDeadWorkers(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != doomed {
		t.Fatalf("sweep reported %+v, want the doomed worker", dead)
	}
	if !dead[0].AlreadyDead {
		t.Error("the worker was reported as newly declared DEAD; the resource-manager declared it")
	}
	if len(dead[0].ExpiredAttempts) != 2 {
		t.Errorf("expired %d attempts, want 2", len(dead[0].ExpiredAttempts))
	}
	for _, id := range []uuid.UUID{a1, a2} {
		if s := f.attemptState(id); s != "LOST" {
			t.Errorf("attempt %s state = %s, want LOST", id, s)
		}
		if _, _, ok := f.leaseFor(id); ok {
			t.Errorf("attempt %s kept its lease; a late report from the dead session would still pass the fence", id)
		}
		if !contains(f.eventKinds(id), domain.EventWorkerDead) {
			t.Errorf("events for %s = %v, missing %s", id, f.eventKinds(id), domain.EventWorkerDead)
		}
		if !contains(f.eventKinds(id), domain.EventRetryScheduled) {
			t.Errorf("events for %s = %v, missing the retry", id, f.eventKinds(id))
		}
		// The already-DEAD path meters like every other end of a dispatch.
		assertMeteredOnce(t, f, id, "LOST")
	}
	// Both sweepers released; the ledger must have been released exactly once.
	if got := f.workerCommitted(doomed).CPUMillis; got != 0 {
		t.Errorf("committed cpu = %d, want 0", got)
	}
	if waited := time.Since(start); waited > 10*time.Second {
		t.Errorf("recovery took %s", waited)
	}

	// Idempotent: a second pass finds nothing left to do.
	again, err := st.SweepDeadWorkers(ctx, time.Now().UTC(), 10)
	if err != nil || len(again) != 0 {
		t.Fatalf("second sweep = %+v, %v; want nothing", again, err)
	}
	for _, id := range []uuid.UUID{a1, a2} {
		assertMeteredOnce(t, f, id, "LOST")
	}
}

// TestDeadSweepContinuesPastAFailedAttempt: one attempt that cannot be
// expired must not stop the rest of that worker's attempts, nor other stale
// workers. The failed one is recovered on a later pass even though its worker
// is already DEAD by then.
func TestDeadSweepContinuesPastAFailedAttempt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	w1 := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := longLeaseStore(f)
	spec := nodeSpec{TaskRef: "test.task", Version: 1, MaxAttempts: 3, Idempotency: "required"}
	poisoned := f.addAttempt("poisoned", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	dispatchOne(t, st)
	healthy := f.addAttempt("healthy", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	dispatchOne(t, st)
	w2 := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	f.staleHeartbeat(w1, 5*time.Minute) // oldest first: w1 is swept before w2
	other := f.addAttempt("other", 100, vec{CPUMillis: 1000}, spec, time.Now().UTC())
	dispatchOne(t, st)
	f.staleHeartbeat(w2, 90*time.Second)

	poison(t, f, poisoned)

	dead, err := st.SweepDeadWorkers(ctx, time.Now().UTC(), 10)
	if err == nil || !strings.Contains(err.Error(), poisoned.String()) {
		t.Fatalf("sweep error = %v, want one naming the poisoned attempt", err)
	}
	if len(dead) != 2 {
		t.Fatalf("swept %d workers, want both despite the failure", len(dead))
	}
	for _, id := range []uuid.UUID{healthy, other} {
		if s := f.attemptState(id); s != "LOST" {
			t.Errorf("attempt %s state = %s, want LOST; the failure stopped the pass", id, s)
		}
	}
	if s := f.attemptState(poisoned); s == "LOST" {
		t.Fatal("the poisoned attempt was expired; the test premise is broken")
	}
	// The per-attempt continue path meters what it expired, and the failed
	// attempt's rolled-back transaction left no row behind.
	for _, id := range []uuid.UUID{healthy, other} {
		assertMeteredOnce(t, f, id, "LOST")
	}
	if n := f.ledgerRows(poisoned); n != 0 {
		t.Fatalf("the failed expiry left %d ledger rows; its transaction must roll back whole", n)
	}
	if s := f.workerState(w1); s != "DEAD" {
		t.Fatalf("w1 state = %s, want DEAD", s)
	}

	// Repaired, it is recovered on the next pass through the already-DEAD path.
	unpoison(t, f, poisoned)
	dead, err = st.SweepDeadWorkers(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != w1 || !dead[0].AlreadyDead || len(dead[0].ExpiredAttempts) != 1 {
		t.Fatalf("second sweep = %+v, want w1's remaining attempt recovered", dead)
	}
	if s := f.attemptState(poisoned); s != "LOST" {
		t.Errorf("poisoned attempt state = %s after repair, want LOST", s)
	}
	for _, id := range []uuid.UUID{poisoned, healthy, other} {
		assertMeteredOnce(t, f, id, "LOST")
	}
}

// TestLeaseReconcilerContinuesPastAFailedAttempt is F108: the lease
// reconciler's comment promised that one bad attempt would not stop recovery
// of the rest, and the code returned on the first error.
func TestLeaseReconcilerContinuesPastAFailedAttempt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(nil)
	spec := nodeSpec{TaskRef: "test.task", Version: 1, MaxAttempts: 3, Idempotency: "required"}
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		ids = append(ids, f.addAttempt(nodeName("l", i), 100, vec{CPUMillis: 100}, spec, time.Now().UTC()))
		dispatchOne(t, st)
	}
	// Expired in order, poisoned first, so the old code stopped immediately.
	for i, id := range ids {
		if _, err := pool.Exec(ctx,
			`UPDATE leases SET expires_at = now() - make_interval(secs => $2) WHERE attempt_id = $1`,
			id, 60-i); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
	}
	poison(t, f, ids[0])

	lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10)
	if err == nil || !strings.Contains(err.Error(), ids[0].String()) {
		t.Fatalf("reconcile error = %v, want one naming the poisoned attempt", err)
	}
	if len(lost) != 2 {
		t.Fatalf("recovered %d attempts, want the 2 healthy ones", len(lost))
	}
	for _, id := range ids[1:] {
		if s := f.attemptState(id); s != "LOST" {
			t.Errorf("attempt %s state = %s, want LOST", id, s)
		}
	}
	if n := f.ledgerRows(ids[0]); n != 0 {
		t.Fatalf("the failed expiry left %d ledger rows", n)
	}
	unpoison(t, f, ids[0])
	if lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10); err != nil || len(lost) != 1 {
		t.Fatalf("second pass = %d, %v; want the repaired attempt", len(lost), err)
	}
	for _, id := range ids {
		assertMeteredOnce(t, f, id, "LOST")
	}
}

// poison makes expireOne fail for one attempt after it has taken the row: its
// resource_request no longer decodes. A stand-in for any per-attempt fault.
func poison(t *testing.T, f *fixture, id uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE task_attempts SET resource_request = '{"gpu_kind": 7}' WHERE id = $1`, id); err != nil {
		t.Fatalf("poison attempt: %v", err)
	}
}

func unpoison(t *testing.T, f *fixture, id uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE task_attempts SET resource_request = '{"cpu_millis": 1000}' WHERE id = $1`, id); err != nil {
		t.Fatalf("unpoison attempt: %v", err)
	}
}
