//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// TestRetryAccountingMixesFreePreemptedAndChargedRetries drives ONE node
// through the three retry budgets together, against real Postgres:
//
//	attempt 1  RATE_LIMITED -> a free retry (task_nodes.free_retries, ADR-0032)
//	attempt 2  PREEMPTED    -> a preemption retry (preemptions_used, ADR-0030)
//	attempt 3  TRANSIENT    -> an ordinary retry, charged to attempts_used
//	attempt 4  TRANSIENT    -> refused: the charged budget (max_attempts 2) is spent
//
// Each retry must charge exactly its own counter, the charged count the
// scheduler compares against max_attempts must exclude the other two, and the
// DAG engine's view must follow (a pending retry keeps the node alive; the
// refusal ends it) -- the combination of F145, F147/F149 and both ADRs.
func TestRetryAccountingMixesFreePreemptedAndChargedRetries(t *testing.T) {
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()
	w := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	id := f.addAttempt("mixed", 100, vec{CPUMillis: 100}, nodeSpec{TaskRef: "t", Version: 1, MaxAttempts: 2}, time.Now().UTC())
	run := func(attemptID uuid.UUID) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE task_attempts SET state='RUNNING', worker_id=$2, dispatched_at=now(), started_at=now() WHERE id=$1`, attemptID, w); err != nil {
			t.Fatal(err)
		}
	}
	run(id)
	// The DAG engine charges the first attempt when it creates it
	// (dagengine_writes.go); the fixture does not, so do it here.
	if _, err := pool.Exec(ctx, `UPDATE task_nodes SET attempts_used = 1 WHERE run_id=$1 AND node_id='mixed'`, f.runID); err != nil {
		t.Fatal(err)
	}
	st := f.newStore(func(c *store.ClaimConfig) {
		c.RateLimitedFreeRetries = 1
		c.MaxPreemptions = 1
		c.PreemptionRetryDelay = time.Millisecond
	})
	de := store.NewDAGEngineStore(pool)

	fail := func(attemptID uuid.UUID, class nexusv1.ErrorClass) store.AppliedResult {
		t.Helper()
		res, err := st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
			AttemptId: attemptID.String(), Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
			Error: &nexusv1.TaskError{Class: class, Retryable: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Applied {
			t.Fatalf("result for %s not applied", attemptID)
		}
		return res
	}
	budgets := func() (used, free, preempted int32) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT attempts_used, free_retries, preemptions_used FROM task_nodes
			WHERE run_id=$1 AND node_id='mixed'`, f.runID).Scan(&used, &free, &preempted); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := func(step string, used, free, preempted int32) {
		t.Helper()
		u, fr, p := budgets()
		if u != used || fr != free || p != preempted {
			t.Fatalf("%s: attempts_used/free_retries/preemptions_used = %d/%d/%d, want %d/%d/%d",
				step, u, fr, p, used, free, preempted)
		}
	}
	lastAttempt := func() store.DEAttemptRow {
		t.Helper()
		rows, err := de.LoadAttempts(ctx, f.runID.String())
		if err != nil {
			t.Fatal(err)
		}
		var last store.DEAttemptRow
		for _, a := range rows {
			if a.NodeID == "mixed" && a.AttemptNumber >= last.AttemptNumber {
				last = a
			}
		}
		return last
	}
	fireRetry := func(prev uuid.UUID, number int32) uuid.UUID {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE timers SET fire_at = now() - interval '1 second' WHERE subject_id = $1`, prev); err != nil {
			t.Fatal(err)
		}
		timers, err := st.ClaimDueTimers(ctx, time.Now().UTC(), []string{string(domain.TimerRetryBackoff)}, 10)
		if err != nil || len(timers) != 1 {
			t.Fatalf("claim retry timer: %v (%d)", err, len(timers))
		}
		created, err := st.CreatePendingRetry(ctx, time.Now().UTC(), timers[0])
		if err != nil || created == nil || created.AttemptNumber != number {
			t.Fatalf("create retry %d: %+v %v", number, created, err)
		}
		run(created.NewAttemptID)
		return created.NewAttemptID
	}

	// 1: a provider throttle -- free, recorded in the terminal transaction.
	if r := fail(id, nexusv1.ErrorClass_ERROR_CLASS_RATE_LIMITED); !r.Retry {
		t.Fatalf("RATE_LIMITED not retried: %+v", r)
	}
	want("after the throttle", 1, 1, 0)
	if a := lastAttempt(); !a.RetryPending || a.RetryRefused {
		t.Fatalf("after the throttle the engine sees pending=%v refused=%v, want a pending retry", a.RetryPending, a.RetryRefused)
	}
	a2 := fireRetry(id, 2)
	want("retry 2 created (charged to attempts_used, offset by the free retry)", 2, 1, 0)

	// 2: an announced eviction -- charged to preemptions_used when the retry
	// attempt is created, never to attempts_used.
	if r := fail(a2, nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED); !r.Retry {
		t.Fatalf("PREEMPTED not retried: %+v", r)
	}
	a3 := fireRetry(a2, 3)
	want("retry 3 created (a preemption)", 2, 1, 1)

	// 3: an ordinary failure -- charged: attempts_used 2 - free 1 = 1 of 2.
	if r := fail(a3, nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT); !r.Retry {
		t.Fatalf("TRANSIENT with one charged attempt of two not retried: %+v", r)
	}
	a4 := fireRetry(a3, 4)
	want("retry 4 created (charged)", 3, 1, 1)

	// 4: the charged budget is spent (3 - 1 = 2 of 2): refused, recorded as
	// RetryExhausted in the same transaction, and the engine sees the refusal.
	if r := fail(a4, nexusv1.ErrorClass_ERROR_CLASS_TRANSIENT); r.Retry {
		t.Fatalf("a fourth attempt with the charged budget spent was retried: %+v", r)
	}
	want("after the refusal", 3, 1, 1)
	if a := lastAttempt(); !a.RetryRefused || a.RetryPending {
		t.Fatalf("after the refusal the engine sees pending=%v refused=%v, want refused", a.RetryPending, a.RetryRefused)
	}
	var reason string
	if err := pool.QueryRow(ctx, `SELECT payload->>'reason' FROM run_events WHERE attempt_id = $1 AND kind = $2`,
		a4, domain.EventRetryExhausted).Scan(&reason); err != nil || reason == "" {
		t.Fatalf("RetryExhausted for attempt 4: %q %v", reason, err)
	}
}
