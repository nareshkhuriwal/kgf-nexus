//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"
)

// The durable run-reconcile queue (migration 0039, CONTRACT-FINDINGS F160).
//
// A run advances only when the DAG engine reconciles it, and before 0039 the
// only thing that asked was a synchronous RPC made AFTER the terminal attempt
// had committed. Losing that call -- a timeout, an engine restart, a flooded
// pool -- stranded the run forever. These tests pin the durable half: the
// request is written by the database in the SAME transaction as the fact that
// needs it, and consumed only after a reconcile has succeeded.

func reconcileRequests(t *testing.T, ctx context.Context, q GWQuerier, runID string) []string {
	t.Helper()
	rows, err := q.Query(ctx, `
		SELECT reason || ':' || coalesce(node_id, '')
		  FROM run_reconcile_requests WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		t.Fatalf("read reconcile requests: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func clearReconcileRequests(t *testing.T, ctx context.Context, q GWQuerier, runID string) {
	t.Helper()
	mustExecQ(t, ctx, q, `DELETE FROM run_reconcile_requests WHERE run_id = $1`, runID)
}

func mustExecQ(t *testing.T, ctx context.Context, q GWQuerier, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestIntegrationTerminalAttemptEnqueuesARunReconcile(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	mustExec(t, ctx, pool, `UPDATE workflow_runs SET state = 'RUNNING' WHERE id = $1`, f.runID)
	a1 := f.attempt("a", 1)
	f.attempt("b", 1)
	clearReconcileRequests(t, ctx, pool, f.runID)

	// Non-terminal moves need no reconcile: the scheduler owns them and the
	// graph does not change.
	mustExec(t, ctx, pool, `UPDATE task_attempts SET state = 'DISPATCHED' WHERE id = $1`, a1)
	mustExec(t, ctx, pool, `UPDATE task_attempts SET state = 'RUNNING' WHERE id = $1`, a1)
	if got := reconcileRequests(t, ctx, pool, f.runID); len(got) != 0 {
		t.Fatalf("a non-terminal attempt transition enqueued %v, want nothing", got)
	}

	// A terminal write that ROLLS BACK must leave no request: the request
	// exists if and only if the fact does.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustExecQ(t, ctx, tx, `UPDATE task_attempts SET state = 'SUCCEEDED', ended_at = now() WHERE id = $1`, a1)
	if got := reconcileRequests(t, ctx, tx, f.runID); len(got) != 1 {
		t.Fatalf("inside the terminal write's transaction: %v, want exactly one request", got)
	}
	_ = tx.Rollback(ctx)
	if got := reconcileRequests(t, ctx, pool, f.runID); len(got) != 0 {
		t.Fatalf("a rolled-back terminal write left %v", got)
	}

	// Committed: exactly one request, naming the node.
	mustExec(t, ctx, pool, `UPDATE task_attempts SET state = 'SUCCEEDED', ended_at = now() WHERE id = $1`, a1)
	if got := reconcileRequests(t, ctx, pool, f.runID); len(got) != 1 || got[0] != "attempt_terminal:a" {
		t.Fatalf("after a committed SUCCEEDED: %v, want [attempt_terminal:a]", got)
	}
	// Rewriting the same terminal state (a duplicate report) is not a new fact.
	mustExec(t, ctx, pool, `UPDATE task_attempts SET state = 'SUCCEEDED' WHERE id = $1`, a1)
	if got := reconcileRequests(t, ctx, pool, f.runID); len(got) != 1 {
		t.Fatalf("a duplicate terminal write enqueued again: %v", got)
	}

	// Every terminal kind counts, including the ones no worker reports: LOST
	// is written by lease expiry (F152), TIMED_OUT by the deadline timers.
	for _, st := range []string{"FAILED", "TIMED_OUT", "LOST", "CANCELLED"} {
		id := f.attempt("k-"+st, 1)
		clearReconcileRequests(t, ctx, pool, f.runID)
		mustExec(t, ctx, pool, `UPDATE task_attempts SET state = $2::attempt_state, ended_at = now() WHERE id = $1`, id, st)
		if got := reconcileRequests(t, ctx, pool, f.runID); len(got) != 1 {
			t.Errorf("a %s attempt enqueued %v, want one request", st, got)
		}
	}

	// An attempt BORN terminal (a memo hit the engine inserts as SUCCEEDED)
	// also needs its run looked at again.
	clearReconcileRequests(t, ctx, pool, f.runID)
	mustExec(t, ctx, pool, `INSERT INTO task_nodes (run_id, node_id, task_ref, spec_json) VALUES ($1, 'memo', 't:v1', '{}')`, f.runID)
	mustExec(t, ctx, pool, `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, resource_request, idempotency_key, ended_at)
		VALUES ($1::uuid, 'memo', 1, 'SUCCEEDED', '{}'::jsonb, 'memo-' || $1::text, now())`, f.runID)
	if got := reconcileRequests(t, ctx, pool, f.runID); len(got) != 1 {
		t.Errorf("an attempt inserted SUCCEEDED enqueued %v, want one request", got)
	}
}

// The scheduler writes the terminal attempt with its own role, which has no
// grant on the queue: the trigger must still enqueue (SECURITY DEFINER), or
// the scheduler's report transaction would fail outright.
func TestIntegrationSchedulerRoleEnqueuesThroughTheTrigger(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	var roles int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname IN ('nexus_scheduler','nexus_workflow_manager','nexus_dag_engine')`).Scan(&roles)
	if roles != 3 {
		t.Skip("per-service roles (0005) not applied")
	}
	f := newFixture(t, ctx, pool)
	mustExec(t, ctx, pool, `UPDATE workflow_runs SET state = 'RUNNING' WHERE id = $1`, f.runID)
	a := f.attempt("a", 1)
	clearReconcileRequests(t, ctx, pool, f.runID)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	mustExecQ(t, ctx, tx, `SET LOCAL ROLE nexus_scheduler`)
	mustExecQ(t, ctx, tx, `UPDATE task_attempts SET state = 'FAILED', ended_at = now() WHERE id = $1`, a)
	mustExecQ(t, ctx, tx, `RESET ROLE`)
	if got := reconcileRequests(t, ctx, tx, f.runID); len(got) != 1 {
		t.Fatalf("a terminal write as nexus_scheduler enqueued %v, want one request", got)
	}

	// Only the owner consumes. Nobody else may read-modify the backlog.
	check := func(role, priv string, want bool) {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1, 'run_reconcile_requests', $2)`, role, priv).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("%s %s on run_reconcile_requests = %v, want %v", role, priv, ok, want)
		}
	}
	check("nexus_dag_engine", "SELECT", true)
	check("nexus_dag_engine", "UPDATE", true)
	check("nexus_dag_engine", "DELETE", true)
	check("nexus_scheduler", "SELECT", true)
	check("nexus_scheduler", "INSERT", false)
	check("nexus_scheduler", "DELETE", false)
	check("nexus_workflow_manager", "DELETE", false)
}

// Operator actions that reopen a run's graph need a reconcile too, and their
// follow-up call was just as best-effort as the gateway's (Resume, Cancel,
// Retry in the workflow-manager).
func TestIntegrationRunStateChangesThatNeedTheEngineEnqueue(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	f := newFixture(t, ctx, pool)
	f.attempt("a", 1)

	cases := []struct {
		from, to string
		want     bool
	}{
		// The engine's own first publish: materialize already reconciled.
		{"PENDING", "RUNNING", false},
		{"RUNNING", "PAUSING", true},
		{"PAUSING", "PAUSED", false},
		{"PAUSED", "RUNNING", true},     // resume
		{"RUNNING", "CANCELLING", true}, // cancel settles the withheld nodes
		{"CANCELLING", "CANCELLED", false},
		{"FAILED", "RUNNING", true}, // operator retry
		{"RUNNING", "SUCCEEDED", false},
	}
	for _, c := range cases {
		mustExec(t, ctx, pool, `UPDATE workflow_runs SET state = $2::run_state WHERE id = $1`, f.runID, c.from)
		clearReconcileRequests(t, ctx, pool, f.runID)
		mustExec(t, ctx, pool, `UPDATE workflow_runs SET state = $2::run_state WHERE id = $1`, f.runID, c.to)
		got := reconcileRequests(t, ctx, pool, f.runID)
		if c.want && (len(got) != 1 || got[0] != "run_state:") {
			t.Errorf("%s -> %s enqueued %v, want [run_state:]", c.from, c.to, got)
		}
		if !c.want && len(got) != 0 {
			t.Errorf("%s -> %s enqueued %v, want nothing", c.from, c.to, got)
		}
	}
}

func TestIntegrationReconcileQueueClaimLeaseAndAck(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	st := NewDAGEngineStore(pool)

	// Two runs with requests; run a has three (a burst), run b one.
	fa := newFixture(t, ctx, pool)
	fb := newFixture(t, ctx, pool)
	for _, f := range []*fixture{fa, fb} {
		mustExec(t, ctx, pool, `UPDATE workflow_runs SET state = 'RUNNING' WHERE id = $1`, f.runID)
		f.attempt("x", 1)
		clearReconcileRequests(t, ctx, pool, f.runID)
	}
	for i := 0; i < 3; i++ {
		mustExec(t, ctx, pool, `INSERT INTO run_reconcile_requests (run_id, reason) VALUES ($1, 'test')`, fa.runID)
	}
	mustExec(t, ctx, pool, `INSERT INTO run_reconcile_requests (run_id, reason) VALUES ($1, 'test')`, fb.runID)

	// Other tests share the database: only look at our two runs.
	ours := func(cs []ReconcileClaim) map[string]ReconcileClaim {
		out := map[string]ReconcileClaim{}
		for _, c := range cs {
			if c.RunID == fa.runID || c.RunID == fb.runID {
				out[c.RunID] = c
			}
		}
		return out
	}

	// Two replicas claim at once: every request goes to exactly one of them.
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		claims [][]ReconcileClaim
	)
	for _, owner := range []string{"replica-1", "replica-2"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			c, err := st.ClaimReconcileRequests(ctx, owner, time.Minute, 1000)
			if err != nil {
				t.Errorf("claim as %s: %v", owner, err)
				return
			}
			mu.Lock()
			claims = append(claims, c)
			mu.Unlock()
		}(owner)
	}
	wg.Wait()
	var ids []int64
	perRun := map[string]int{}
	for _, c := range claims {
		for run, rc := range ours(c) {
			ids = append(ids, rc.IDs...)
			perRun[run] += len(rc.IDs)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("request %d was claimed by both replicas", ids[i])
		}
	}
	if perRun[fa.runID] != 3 || perRun[fb.runID] != 1 {
		t.Fatalf("claimed %v, want all 3 of run a and 1 of run b", perRun)
	}

	// Leased: nobody else gets them until the lease runs out.
	again, err := st.ClaimReconcileRequests(ctx, "replica-3", time.Minute, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := ours(again); len(got) != 0 {
		t.Fatalf("leased requests were claimed again: %v", got)
	}

	// A failed reconcile releases its requests with a backoff: not claimable
	// yet, still present, and the failure recorded.
	var aIDs []int64
	for _, c := range claims {
		if rc, ok := ours(c)[fa.runID]; ok {
			aIDs = append(aIDs, rc.IDs...)
		}
	}
	if err := st.ReleaseReconcileRequests(ctx, aIDs, time.Hour, "engine unavailable"); err != nil {
		t.Fatal(err)
	}
	again, _ = st.ClaimReconcileRequests(ctx, "replica-3", time.Minute, 1000)
	if got := ours(again); len(got) != 0 {
		t.Fatalf("a request backed off for an hour was claimed at once: %v", got)
	}
	var attempts int
	var lastErr string
	if err := pool.QueryRow(ctx, `SELECT max(attempts), max(last_error) FROM run_reconcile_requests WHERE run_id = $1`,
		fa.runID).Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr != "engine unavailable" {
		t.Errorf("released requests record attempts=%d last_error=%q, want 1 and the cause", attempts, lastErr)
	}

	// An expired lease is reclaimable: a replica that died holding requests
	// does not strand them.
	mustExec(t, ctx, pool, `UPDATE run_reconcile_requests SET claimed_until = now() - interval '1 second' WHERE run_id = $1`, fb.runID)
	again, _ = st.ClaimReconcileRequests(ctx, "replica-3", time.Minute, 1000)
	if got := ours(again); len(got[fb.runID].IDs) != 1 {
		t.Fatalf("a request whose lease expired was not reclaimed: %v", got)
	}

	// Ack by id deletes exactly what was consumed; a request written AFTER the
	// snapshot survives it.
	snap, err := st.PendingReconcileRequestIDs(ctx, fb.runID, 100)
	if err != nil || len(snap) != 1 {
		t.Fatalf("snapshot of run b: %v %v", snap, err)
	}
	mustExec(t, ctx, pool, `INSERT INTO run_reconcile_requests (run_id, reason) VALUES ($1, 'late')`, fb.runID)
	if n, err := st.AckReconcileRequests(ctx, snap); err != nil || n != 1 {
		t.Fatalf("ack: %d %v", n, err)
	}
	if got := reconcileRequests(t, ctx, pool, fb.runID); len(got) != 1 || got[0] != "late:" {
		t.Fatalf("after ack: %v, want only the late request", got)
	}
}
