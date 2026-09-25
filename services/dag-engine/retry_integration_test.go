//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Operator retry of selected nodes (CONTRACT-FINDINGS F100), against real
// Postgres: the reset, the reconcile that follows it, and the races around it.

// failedLinearRun materializes a -> b -> c, succeeds a, fails b terminally and
// leaves the run FAILED -- the state an operator retries from.
func failedLinearRun(t *testing.T, pool *pgxpool.Pool, bAttempts int32) (string, *Engine) {
	t.Helper()
	spec := linearSpec()
	spec.Tasks[1].Retry = &nexusv1.RetryPolicy{MaxAttempts: bAttempts}
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	ctx := context.Background()
	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptSucceeded, map[string]any{"out": "a-value"})
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile after a: %v", err)
	}
	// Spend b's whole budget. The scheduler would create each retry attempt;
	// the test inserts them the same way (attempt_number+1, same key).
	for i := int32(1); i <= bAttempts; i++ {
		if i > 1 {
			insertRetryAttempt(t, pool, runID, "b", i)
		}
		settleAttempt(t, pool, runID, "b", domain.AttemptFailed, nil)
	}
	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile after b: %v", err)
	}
	if res.RunState != domain.RunFailed {
		t.Fatalf("setup: run state %s, want FAILED", res.RunState)
	}
	setRunState(t, pool, runID, "FAILED")
	return runID, e
}

// insertRetryAttempt does what the scheduler's retry timer does: a new PENDING
// row with attempt_number n and the node's (unchanged) idempotency key.
func insertRetryAttempt(t *testing.T, pool *pgxpool.Pool, runID, nodeID string, n int32) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO task_attempts (run_id, node_id, attempt_number, state, queue, priority,
		                           resource_request, idempotency_key, input_refs)
		SELECT run_id, node_id, $3, 'PENDING', queue, priority, resource_request, idempotency_key, input_refs
		  FROM task_attempts WHERE run_id = $1::uuid AND node_id = $2 AND attempt_number = 1`,
		runID, nodeID, n); err != nil {
		t.Fatalf("insert retry attempt %d of %s: %v", n, nodeID, err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE task_nodes SET attempts_used = attempts_used + 1 WHERE run_id = $1::uuid AND node_id = $2`,
		runID, nodeID); err != nil {
		t.Fatalf("charge budget: %v", err)
	}
}

type attemptHist struct {
	Number int32
	State  string
	Key    string
}

func attemptHistory(t *testing.T, pool *pgxpool.Pool, runID, nodeID string) []attemptHist {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT attempt_number, state::text, idempotency_key FROM task_attempts
		 WHERE run_id = $1::uuid AND node_id = $2 ORDER BY attempt_number`, runID, nodeID)
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	defer rows.Close()
	var out []attemptHist
	for rows.Next() {
		var h attemptHist
		if err := rows.Scan(&h.Number, &h.State, &h.Key); err != nil {
			t.Fatalf("scan attempt: %v", err)
		}
		out = append(out, h)
	}
	return out
}

func nodeBudget(t *testing.T, pool *pgxpool.Pool, runID, nodeID string) (floor, used int32) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT retry_floor, attempts_used FROM task_nodes WHERE run_id = $1::uuid AND node_id = $2`,
		runID, nodeID).Scan(&floor, &used); err != nil {
		t.Fatalf("read budget of %s: %v", nodeID, err)
	}
	return floor, used
}

// The whole journey: reset the failed node and its dependent, leave succeeded
// upstream alone, re-run with the same idempotency key, and finish.
func TestIntegration_RetrySelectedNodeRerunsItAndItsDependents(t *testing.T) {
	pool := testPool(t)
	runID, e := failedLinearRun(t, pool, 1)
	ctx := context.Background()

	res, err := e.ResetNodes(ctx, runID, []string{"b"}, "user:alice")
	if err != nil {
		t.Fatalf("ResetNodes: %v", err)
	}
	if !reflect.DeepEqual(res.Selected, []string{"b"}) || !reflect.DeepEqual(res.Reset, []string{"b", "c"}) {
		t.Fatalf("result = %+v, want selected [b], reset [b c]", res)
	}
	states := nodeStates(t, pool, runID)
	if states["a"] != "SUCCEEDED" || states["b"] != "PENDING" || states["c"] != "PENDING" {
		t.Fatalf("states after reset = %v", states)
	}
	if floor, used := nodeBudget(t, pool, runID, "b"); floor != 1 || used != 0 {
		t.Errorf("b retry_floor/attempts_used = %d/%d, want 1/0", floor, used)
	}
	// History is immutable: the failed attempt is still there, still FAILED.
	if h := attemptHistory(t, pool, runID, "b"); len(h) != 1 || h[0].State != "FAILED" {
		t.Fatalf("b history after reset = %+v, want the one FAILED attempt untouched", h)
	}

	// ResetNodes does not reconcile; the workflow-manager moves the run to
	// RUNNING and then asks. Simulated here.
	setRunState(t, pool, runID, "RUNNING")
	rec, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile after reset: %v", err)
	}
	if !reflect.DeepEqual(rec.Frontier, []string{"b"}) {
		t.Fatalf("frontier after reset = %v, want [b]", rec.Frontier)
	}
	h := attemptHistory(t, pool, runID, "b")
	if len(h) != 2 || h[1].Number != 2 || h[1].State != "PENDING" {
		t.Fatalf("b history after reconcile = %+v, want attempt 2 PENDING", h)
	}
	// Invariant 1: the retry shares the key of every earlier try.
	if want := domain.IdempotencyKey(runID, "b", nil, 1); h[0].Key != want || h[1].Key != want {
		t.Errorf("idempotency keys = %q, %q; both must be %q", h[0].Key, h[1].Key, want)
	}
	// Succeeded upstream work is not re-run.
	if len(attemptHistory(t, pool, runID, "a")) != 1 {
		t.Error("succeeded upstream a was re-run")
	}

	settleAttempt(t, pool, runID, "b", domain.AttemptSucceeded, map[string]any{"out": "b-value"})
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile after b: %v", err)
	}
	settleAttempt(t, pool, runID, "c", domain.AttemptSucceeded, nil)
	final, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	if final.RunState != domain.RunSucceeded {
		t.Fatalf("retried run finished %s, want SUCCEEDED", final.RunState)
	}
}

// A retried node gets its declared budget again, counted from the retry.
func TestIntegration_RetryGrantsAFreshBudget(t *testing.T) {
	pool := testPool(t)
	runID, e := failedLinearRun(t, pool, 2) // b failed attempts 1 and 2
	ctx := context.Background()

	if _, err := e.ResetNodes(ctx, runID, []string{"b"}, "user:alice"); err != nil {
		t.Fatalf("ResetNodes: %v", err)
	}
	setRunState(t, pool, runID, "RUNNING")
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if floor, used := nodeBudget(t, pool, runID, "b"); floor != 2 || used != 1 {
		t.Fatalf("b retry_floor/attempts_used = %d/%d, want 2/1", floor, used)
	}

	// Attempt 3 fails. Counted from the floor it is the retry's FIRST try of
	// two, so the node is between tries (RUNNING), not FAILED.
	settleAttempt(t, pool, runID, "b", domain.AttemptFailed, nil)
	rec, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := nodeStates(t, pool, runID)["b"]; got != "RUNNING" || rec.RunTerminal {
		t.Fatalf("b = %s (run terminal=%v) after the retry's first failure; want RUNNING with budget left", got, rec.RunTerminal)
	}

	// Attempt 4 is the retry's second and last; its failure is terminal.
	insertRetryAttempt(t, pool, runID, "b", 4)
	settleAttempt(t, pool, runID, "b", domain.AttemptFailed, nil)
	rec, err = e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := nodeStates(t, pool, runID)["b"]; got != "FAILED" || rec.RunState != domain.RunFailed {
		t.Fatalf("b = %s, run = %s after the retry's budget ran out; want FAILED/FAILED", got, rec.RunState)
	}
}

func TestIntegration_RetryRefusalsChangeNothing(t *testing.T) {
	pool := testPool(t)
	runID, e := failedLinearRun(t, pool, 1)
	srv := NewGRPCServer(e, testLogger())
	ctx := context.Background()
	before := nodeStates(t, pool, runID)

	cases := []struct {
		name     string
		nodes    []string
		wantCode codes.Code
	}{
		{"unknown node", []string{"b", "nope"}, codes.InvalidArgument},
		{"succeeded node", []string{"a"}, codes.FailedPrecondition},
		{"dependent without its failed cause", []string{"c"}, codes.FailedPrecondition},
	}
	for _, c := range cases {
		_, err := srv.ResetNodes(ctx, &ResetNodesRequest{RunID: runID, NodeIDs: c.nodes})
		if status.Code(err) != c.wantCode {
			t.Errorf("%s: code %v (%v), want %v", c.name, status.Code(err), err, c.wantCode)
		}
	}
	if after := nodeStates(t, pool, runID); !reflect.DeepEqual(before, after) {
		t.Errorf("a refused retry changed node states: %v -> %v", before, after)
	}

	// A run that is not terminal-failed is refused outright.
	setRunState(t, pool, runID, "RUNNING")
	_, err := srv.ResetNodes(ctx, &ResetNodesRequest{RunID: runID, NodeIDs: []string{"b"}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("reset under a RUNNING run: code %v, want FailedPrecondition", status.Code(err))
	}
	if _, err := e.ResetNodes(ctx, runID, []string{"b"}, ""); !errors.Is(err, ErrRunNotRetryable) {
		t.Errorf("reset under a RUNNING run: %v, want ErrRunNotRetryable", err)
	}
}

func TestIntegration_WholeRunRetryResetsEverythingThatDidNotSucceed(t *testing.T) {
	pool := testPool(t)
	runID, e := failedLinearRun(t, pool, 1)
	res, err := e.ResetNodes(context.Background(), runID, nil, "user:alice")
	if err != nil {
		t.Fatalf("ResetNodes: %v", err)
	}
	if !reflect.DeepEqual(res.Reset, []string{"b", "c"}) {
		t.Fatalf("reset = %v, want [b c]", res.Reset)
	}
}

// Two operators retry the same run at the same instant. Exactly one reset
// applies; the other is refused as a whole, and the graph is never half-reset.
func TestIntegration_ConcurrentRetriesApplyOnce(t *testing.T) {
	pool := testPool(t)
	for round := 0; round < 10; round++ {
		runID, e := failedLinearRun(t, pool, 1)
		ctx := context.Background()

		const racers = 4
		var wg sync.WaitGroup
		errs := make([]error, racers)
		start := make(chan struct{})
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = e.ResetNodes(ctx, runID, []string{"b"}, "user:racer")
			}(i)
		}
		close(start)
		wg.Wait()

		wins := 0
		for _, err := range errs {
			if err == nil {
				wins++
				continue
			}
			// The loser either lost the CAS (Aborted) or planned after the
			// winner committed and found b already PENDING (refused).
			var re *dag.RetryError
			if !errors.Is(err, store.ErrNodeRetryConflict) && !errors.As(err, &re) {
				t.Fatalf("round %d: losing retry failed with %v, want a conflict or a refusal", round, err)
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d retries applied, want exactly 1 (errs %v)", round, wins, errs)
		}
		if floor, used := nodeBudget(t, pool, runID, "b"); floor != 1 || used != 0 {
			t.Fatalf("round %d: b floor/used = %d/%d after the race", round, floor, used)
		}
		st := nodeStates(t, pool, runID)
		if st["b"] != "PENDING" || st["c"] != "PENDING" {
			t.Fatalf("round %d: states after the race = %v", round, st)
		}
	}
}

// The store's reset is all-or-nothing: one stale expectation, or one attempt
// in flight, and nothing is written.
func TestIntegration_ApplyRetryResetsIsAllOrNothing(t *testing.T) {
	pool := testPool(t)
	runID, _ := failedLinearRun(t, pool, 1)
	st := store.NewDAGEngineStore(pool)
	ctx := context.Background()

	// c is UPSTREAM_FAILED, not FAILED: the second CAS misses.
	err := st.ApplyRetryResets(ctx, runID, []store.NodeRetryReset{
		{NodeID: "b", From: domain.NodeFailed, To: domain.NodePending},
		{NodeID: "c", From: domain.NodeFailed, To: domain.NodePending},
	})
	if !errors.Is(err, store.ErrNodeRetryConflict) {
		t.Fatalf("stale expectation: %v, want ErrNodeRetryConflict", err)
	}
	if got := nodeStates(t, pool, runID)["b"]; got != "FAILED" {
		t.Fatalf("b = %s after a rolled-back reset; the first CAS leaked", got)
	}

	// A cancel that has not landed yet: b has a live attempt. Resetting now
	// would let a second attempt start beside it.
	insertRetryAttempt(t, pool, runID, "b", 2)
	err = st.ApplyRetryResets(ctx, runID, []store.NodeRetryReset{
		{NodeID: "b", From: domain.NodeFailed, To: domain.NodePending},
	})
	if !errors.Is(err, store.ErrNodeRetryConflict) {
		t.Fatalf("in-flight attempt: %v, want ErrNodeRetryConflict", err)
	}
	if got := nodeStates(t, pool, runID)["b"]; got != "FAILED" {
		t.Fatalf("b = %s; a reset with an attempt in flight must not apply", got)
	}
}

// A selected expansion child resets its template and the fan-in, and only the
// failed child re-runs.
func TestIntegration_RetryAFailedExpansionChild(t *testing.T) {
	pool := testPool(t)
	runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		[]any{"x", "y", "z"})
	ctx := context.Background()
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile to expand: %v", err)
	}

	children := childIDs(nodeStates(t, pool, runID))
	sort.Strings(children)
	if len(children) != 3 {
		t.Fatalf("children = %v, want 3", children)
	}
	failed := children[1]
	for _, c := range children {
		state := domain.AttemptSucceeded
		if c == failed {
			state = domain.AttemptFailed
		}
		settleRef(t, pool, runID, c, state, nil)
	}
	rec, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if rec.RunState != domain.RunFailed {
		t.Fatalf("setup: run %s, want FAILED", rec.RunState)
	}
	setRunState(t, pool, runID, "FAILED")

	res, err := e.ResetNodes(ctx, runID, []string{failed}, "user:alice")
	if err != nil {
		t.Fatalf("ResetNodes(%s): %v", failed, err)
	}
	var tmpl string
	for _, id := range res.Reset {
		if id != failed && nodeStates(t, pool, runID)[id] == "RUNNING" {
			tmpl = id
		}
	}
	if tmpl == "" {
		t.Fatalf("reset = %v; the template must go back to RUNNING", res.Reset)
	}
	setRunState(t, pool, runID, "RUNNING")
	rec, err = e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile after reset: %v", err)
	}
	if !reflect.DeepEqual(rec.Frontier, []string{failed}) {
		t.Fatalf("frontier = %v, want only the failed child %s", rec.Frontier, failed)
	}
	settleRef(t, pool, runID, failed, domain.AttemptSucceeded, nil)
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile after child: %v", err)
	}
	if got := nodeStates(t, pool, runID)[tmpl]; got != "SUCCEEDED" {
		t.Errorf("template = %s once every child succeeded, want SUCCEEDED", got)
	}
}
