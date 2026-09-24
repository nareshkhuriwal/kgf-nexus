//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// These tests pin ADR-0017: weighted fair share ACROSS namespaces, priority and
// aging WITHIN one, all inside the unchanged §5.3 claim. Each was checked by
// mutation -- run with FairShare=false, or with the per-namespace exclusion in
// claimInTx removed -- and fails in that state (recorded in the ADR). A
// fair-share test that passes with fair share switched off would be proving
// nothing (F89).

// setWeight writes a namespace's fair-share weight exactly where the platform
// config API stores it, so the test exercises the scheduler_namespace_weights
// view (migration 0022) rather than a test-only shortcut.
func (f *fixture) setWeight(w int) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO platform_config (namespace, revision, config)
VALUES ($1, 1, jsonb_build_object('scheduler', jsonb_build_object('fair_share_weight', $2::int)))
ON CONFLICT (namespace) DO UPDATE SET config = EXCLUDED.config`, f.ns, w); err != nil {
		f.t.Fatalf("set weight: %v", err)
	}
}

// addAttempts bulk-creates n PENDING attempts, each on its own node.
func (f *fixture) addAttempts(prefix string, n int, priority int32, req vec, at time.Time) []uuid.UUID {
	f.t.Helper()
	ids := make([]uuid.UUID, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, f.addAttempt(nodeName(prefix, i), priority, req,
			nodeSpec{TaskRef: "test.task", Version: 1}, at))
	}
	return ids
}

// dispatchedByNamespace counts dispatched attempts per namespace on a queue,
// read from the durable attempt rows rather than from any claim report.
func dispatchedByNamespace(t *testing.T, pool *pgxpool.Pool, queue string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
SELECT r.namespace, count(*)
  FROM task_attempts a JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.queue = $1 AND a.state IN ('DISPATCHED','RUNNING')
 GROUP BY r.namespace`, queue)
	if err != nil {
		t.Fatalf("count dispatched: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var ns string
		var n int
		if err := rows.Scan(&ns, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[ns] = n
	}
	return out
}

// TestFairShareFloodDoesNotStarveAnotherNamespace: namespace A floods the
// shared queue with 300 attempts at a HIGHER priority than namespace B's five.
// Under the pre-ADR-0017 order (priority DESC, globally) every one of the first
// 300 claims goes to A. Under fair share, B's work is dispatched within the
// first handful of claims because B's share (0 in flight) is lower than A's.
func TestFairShareFloodDoesNotStarveAnotherNamespace(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	a := newFixture(t, pool)
	b := newFixtureOnQueue(t, pool, a.queue)
	ctx := context.Background()

	big := vec{CPUMillis: 1_000_000, MemoryBytes: 1 << 40}
	a.addWorker(big, nil, nil)
	b.addWorker(big, nil, nil)

	now := time.Now().UTC()
	a.addAttempts("flood", 300, 1000, vec{CPUMillis: 100, MemoryBytes: 1 << 20}, now.Add(-time.Minute))
	bIDs := b.addAttempts("victim", 5, 100, vec{CPUMillis: 100, MemoryBytes: 1 << 20}, now)

	st := a.newStore(nil)
	const claims = 12
	for i := 0; i < claims; i++ {
		rep, err := st.Claim(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if rep.Outcome != store.ClaimDispatched {
			t.Fatalf("claim %d: outcome %v, want dispatched", i, rep.Outcome)
		}
	}
	for _, id := range bIDs {
		if s := b.attemptState(id); s != "DISPATCHED" {
			t.Errorf("namespace B attempt %s is %s after %d claims: a higher-priority flood from "+
				"namespace A starved it", id, s, claims)
		}
	}
}

// TestFairShareBlockedFloodDoesNotStarveAnotherNamespace is the harder shape:
// A's flood cannot be placed at all (A has no worker), so nothing of A is ever
// in flight and A's share stays at the minimum forever. A flat look-ahead
// spends every slot of every cycle on A's blocked attempts; the per-namespace
// exclusion is what lets the cycle move on to B.
func TestFairShareBlockedFloodDoesNotStarveAnotherNamespace(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	a := newFixture(t, pool)
	b := newFixtureOnQueue(t, pool, a.queue)
	ctx := context.Background()

	b.addWorker(vec{CPUMillis: 1_000_000, MemoryBytes: 1 << 40}, nil, nil)
	now := time.Now().UTC()
	a.addAttempts("stuck", 200, 1000, vec{CPUMillis: 100, MemoryBytes: 1 << 20}, now.Add(-time.Minute))
	bIDs := b.addAttempts("victim", 3, 100, vec{CPUMillis: 100, MemoryBytes: 1 << 20}, now)

	st := a.newStore(nil)
	for i := 0; i < 3; i++ {
		rep, err := st.Claim(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if rep.Outcome != store.ClaimDispatched {
			t.Fatalf("claim %d: outcome %v (%s); namespace B was never reached behind A's blocked flood",
				i, rep.Outcome, rep.Reason)
		}
	}
	for _, id := range bIDs {
		if s := b.attemptState(id); s != "DISPATCHED" {
			t.Errorf("namespace B attempt %s is %s", id, s)
		}
	}
}

// TestFairShareWeightsRespected: two namespaces with unbounded backlogs and
// weights 3:1. Over 200 dispatches the split must be 3:1 within 3 percentage
// points. The workers are big enough that capacity never binds, so the only
// thing deciding who is served next is the fair-share order.
func TestFairShareWeightsRespected(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	a := newFixture(t, pool)
	b := newFixtureOnQueue(t, pool, a.queue)
	ctx := context.Background()
	a.setWeight(3)
	b.setWeight(1)

	big := vec{CPUMillis: 10_000_000, MemoryBytes: 1 << 50}
	a.addWorker(big, nil, nil)
	b.addWorker(big, nil, nil)
	now := time.Now().UTC()
	req := vec{CPUMillis: 10, MemoryBytes: 1 << 20}
	a.addAttempts("a", 250, 100, req, now)
	// B's work is OLDER and higher priority: if priority or aging leaked
	// across namespaces, B would win far more than a quarter of the claims.
	b.addAttempts("b", 250, 500, req, now.Add(-10*time.Minute))

	st := a.newStore(nil)
	const total = 200
	for i := 0; i < total; i++ {
		if rep, err := st.Claim(ctx, time.Now().UTC()); err != nil || rep.Outcome != store.ClaimDispatched {
			t.Fatalf("claim %d: %v %v", i, rep.Outcome, err)
		}
	}
	got := dispatchedByNamespace(t, pool, a.queue)
	fracA := float64(got[a.ns]) / float64(total)
	t.Logf("weights 3:1 -> dispatched A=%d B=%d (A share %.3f, want 0.750)", got[a.ns], got[b.ns], fracA)
	if math.Abs(fracA-0.75) > 0.03 {
		t.Fatalf("namespace A received %.1f%% of dispatches, want 75%% +/- 3", fracA*100)
	}
}

// TestAgingLiftsOldLowPriorityWorkWithinNamespace: inside one namespace the
// order is priority plus aging (1 point/s by default). An attempt at priority
// 10 that has waited 300s (effective 310) must beat a fresh priority-200 one;
// one that has waited only 60s (effective 70) must not.
func TestAgingLiftsOldLowPriorityWorkWithinNamespace(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	f.addWorker(vec{CPUMillis: 1000, MemoryBytes: 1 << 30}, nil, nil) // room for exactly one

	now := time.Now().UTC()
	req := vec{CPUMillis: 1000, MemoryBytes: 1 << 30}
	spec := nodeSpec{TaskRef: "test.task", Version: 1}
	fresh := f.addAttempt("fresh-high", 200, req, spec, now)
	recent := f.addAttempt("recent-low", 10, req, spec, now.Add(-60*time.Second))
	old := f.addAttempt("old-low", 10, req, spec, now.Add(-300*time.Second))

	rep, err := f.newStore(nil).Claim(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Outcome != store.ClaimDispatched || rep.Dispatch.AttemptID != old {
		t.Fatalf("dispatched %v, want the aged low-priority attempt %s (fresh=%s recent=%s)",
			rep.Dispatch, old, fresh, recent)
	}
	if s := f.attemptState(recent); s != "PENDING" {
		t.Fatalf("recent low-priority attempt is %s: aging over-lifted it past priority 200", s)
	}
}

// TestFairShareConcurrentSchedulersNeverDoubleDispatch re-proves §5.3's central
// claim with the fair-share ORDER BY in place: 8 replicas, each with its OWN
// connection pool, race over three namespaces on one queue with capacity well
// below demand. Assertions read durable rows: every attempt has at most one
// AttemptDispatched event and one lease, and no worker's committed ledger
// differs from the sum of its reservations or exceeds its capacity.
func TestFairShareConcurrentSchedulersNeverDoubleDispatch(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	a := newFixture(t, pool)
	b := newFixtureOnQueue(t, pool, a.queue)
	c := newFixtureOnQueue(t, pool, a.queue)
	a.setWeight(2)
	ctx := context.Background()

	var workers []uuid.UUID
	for _, f := range []*fixture{a, b, c} {
		for i := 0; i < 2; i++ {
			workers = append(workers, f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 4 << 30}, nil, nil))
		}
	}
	now := time.Now().UTC()
	req := vec{CPUMillis: 1000, MemoryBytes: 1 << 30}
	a.addAttempts("a", 40, 100, req, now)
	b.addAttempts("b", 40, 300, req, now)
	c.addAttempts("c", 40, 100, req, now.Add(-time.Minute))

	const replicas = 8
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	deadline := time.Now().Add(15 * time.Second)
	for r := 0; r < replicas; r++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			own := newTestPool(t)
			cfg := store.DefaultClaimConfig(a.queue)
			cfg.LeaseTTL = 5 * time.Minute
			st := store.NewSchedulerStore(own, cfg, fmt.Sprintf("fs-%d", idx))
			idle := 0
			for time.Now().Before(deadline) && idle < 20 {
				rep, err := st.Claim(ctx, time.Now().UTC())
				if err != nil {
					errs[idx] = err
					return
				}
				if rep.Outcome == store.ClaimDispatched {
					idle = 0
				} else {
					idle++
				}
			}
		}(r)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d: %v", i, err)
		}
	}

	var dupEvents, dupLeases int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM (
  SELECT e.attempt_id FROM run_events e JOIN task_attempts t ON t.id = e.attempt_id
   WHERE t.queue = $1 AND e.kind = 'AttemptDispatched'
   GROUP BY e.attempt_id HAVING count(*) > 1) d`, a.queue).Scan(&dupEvents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM (
  SELECT l.attempt_id FROM leases l JOIN task_attempts t ON t.id = l.attempt_id
   WHERE t.queue = $1 GROUP BY l.attempt_id HAVING count(*) > 1) d`, a.queue).Scan(&dupLeases); err != nil {
		t.Fatal(err)
	}
	if dupEvents != 0 || dupLeases != 0 {
		t.Fatalf("DOUBLE DISPATCH: %d attempts with >1 dispatch event, %d with >1 lease", dupEvents, dupLeases)
	}

	for _, w := range workers {
		var committed, reserved, capacity int64
		if err := pool.QueryRow(ctx, `
SELECT COALESCE((w.committed->>'cpu_millis')::bigint,0),
       COALESCE((SELECT sum((r.resources->>'cpu_millis')::bigint) FROM reservations r WHERE r.worker_id = w.id),0),
       (w.capacity->>'cpu_millis')::bigint
  FROM workers w WHERE w.id = $1`, w).Scan(&committed, &reserved, &capacity); err != nil {
			t.Fatal(err)
		}
		if committed != reserved {
			t.Errorf("worker %s: committed %d != reservations %d (ledger drift)", w, committed, reserved)
		}
		if committed > capacity {
			t.Errorf("worker %s: committed %d > capacity %d (over-commit)", w, committed, capacity)
		}
	}

	got := dispatchedByNamespace(t, pool, a.queue)
	t.Logf("dispatched per namespace: %v", got)
	// Every worker is saturated (2 workers x 4 slots per namespace), so each
	// namespace must have filled its own 8 slots: none was starved by the race.
	for _, f := range []*fixture{a, b, c} {
		if got[f.ns] != 8 {
			t.Errorf("namespace %s dispatched %d, want its full 8 slots", f.ns, got[f.ns])
		}
	}
}
