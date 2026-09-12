//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Integration tests for the DAG engine's I/O layer. Run with:
//
//	go test -tags integration ./services/dag-engine/...
//
// These SKIP cleanly when the database is unreachable rather than failing, so a
// developer without Postgres can still run the full unit suite. A test that fails
// for want of infrastructure trains people to ignore red builds.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

const defaultDSN = "postgres://postgres:test@localhost:55432/nexus"

func dsn() string {
	if v := os.Getenv("NEXUS_POSTGRES_DSN"); v != "" {
		return v
	}
	return defaultDSN
}

// testPool connects or SKIPS. See the package comment.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := store.NewPool(ctx, store.DefaultPoolConfig(dsn()))
	if err != nil {
		t.Skipf("postgres unreachable at %s, skipping integration test: %v", dsn(), err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedRun inserts a spec and a run, returning the run id. Every test gets fresh
// uuids so tests never collide with each other or with another agent's fixtures
// in the shared database.
func seedRun(t *testing.T, pool *pgxpool.Pool, spec *nexusv1.WorkflowSpec) string {
	t.Helper()
	ctx := context.Background()

	raw, err := protojson.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	specID := uuid.NewString()
	name := fmt.Sprintf("dagtest-%s", specID[:8])

	_, err = pool.Exec(ctx, `
		INSERT INTO workflow_specs (id, namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1::uuid, 'default', $2, 1, $3, $4, 'dag-engine-test')`,
		specID, name, specID, raw)
	if err != nil {
		t.Fatalf("insert spec: %v", err)
	}

	runID := uuid.NewString()
	_, err = pool.Exec(ctx, `
		INSERT INTO workflow_runs (id, spec_id, namespace, state, trigger_kind)
		VALUES ($1::uuid, $2::uuid, 'default', 'RUNNING', 'manual')`,
		runID, specID)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Cascades to nodes, edges and attempts, so each test leaves the shared
	// database as it found it.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_runs WHERE id = $1::uuid`, runID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_specs WHERE id = $1::uuid`, specID)
	})
	return runID
}

func task(id string, rule nexusv1.TriggerRule) *nexusv1.TaskSpec {
	return &nexusv1.TaskSpec{
		Id: id, TaskRef: id + ":v1", Version: 1, TriggerRule: rule, Queue: "default",
		Resources: &nexusv1.ResourceRequest{
			Request: &nexusv1.ResourceVector{CpuMillis: 1000, MemoryBytes: 1 << 30},
		},
		Retry: &nexusv1.RetryPolicy{MaxAttempts: 1},
	}
}

// linearSpec builds a -> b -> c.
func linearSpec() *nexusv1.WorkflowSpec {
	return &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "linear",
		Tasks: []*nexusv1.TaskSpec{
			task("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("b", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("c", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		},
		Edges: []*nexusv1.Edge{
			{FromNodeId: "a", ToNodeId: "b"},
			{FromNodeId: "b", ToNodeId: "c"},
		},
	}
}

func newTestEngine(t *testing.T, pool *pgxpool.Pool) *Engine {
	t.Helper()
	return NewEngine(store.NewDAGEngineStore(pool), nil, nil, testLogger())
}

// settleAttempt simulates what the SCHEDULER and a worker would do to an attempt.
// The engine never writes these transitions itself -- that is the scheduler's
// ownership -- so the test performs them directly.
func settleAttempt(t *testing.T, pool *pgxpool.Pool, runID, nodeID string, state domain.AttemptState, outputs map[string]any) {
	t.Helper()
	var raw []byte
	if outputs != nil {
		var err error
		raw, err = json.Marshal(outputs)
		if err != nil {
			t.Fatalf("marshal outputs: %v", err)
		}
	}
	tag, err := pool.Exec(context.Background(), `
		UPDATE task_attempts SET state = $3::attempt_state, output_ref = $4, ended_at = now()
		 WHERE run_id = $1::uuid AND node_id = $2
		   AND attempt_number = (SELECT MAX(attempt_number) FROM task_attempts
		                          WHERE run_id = $1::uuid AND node_id = $2)`,
		runID, nodeID, string(state), raw)
	if err != nil {
		t.Fatalf("settle attempt %s/%s: %v", runID, nodeID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("settle attempt %s/%s affected %d rows, want 1", runID, nodeID, tag.RowsAffected())
	}
}

func nodeStates(t *testing.T, pool *pgxpool.Pool, runID string) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT node_id, state::text FROM task_nodes WHERE run_id = $1::uuid`, runID)
	if err != nil {
		t.Fatalf("read node states: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, s string
		if err := rows.Scan(&id, &s); err != nil {
			t.Fatalf("scan node state: %v", err)
		}
		out[id] = s
	}
	return out
}

func TestIntegration_MaterializeCreatesNodesEdgesAndInitialFrontier(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)

	res, err := e.Materialize(context.Background(), runID)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if res.NodesInserted != 3 {
		t.Errorf("inserted %d nodes, want 3", res.NodesInserted)
	}
	if res.EdgesInserted != 2 {
		t.Errorf("inserted %d edges, want 2", res.EdgesInserted)
	}
	// Only the root is ready: b and c have unsatisfied dependencies.
	if len(res.Frontier) != 1 || res.Frontier[0] != "a" {
		t.Fatalf("initial frontier = %v, want [a]", res.Frontier)
	}
	if len(res.AttemptsCreated) != 1 {
		t.Fatalf("created %d attempts, want 1: %+v", len(res.AttemptsCreated), res.AttemptsCreated)
	}

	// The attempt must carry an idempotency key that excludes attempt_number
	// (CONTRACT.md invariant 1) and a resource_request from the spec.
	var key string
	var resReq []byte
	var state, queue string
	err = pool.QueryRow(context.Background(), `
		SELECT idempotency_key, resource_request, state::text, queue
		  FROM task_attempts WHERE run_id = $1::uuid AND node_id = 'a'`, runID).
		Scan(&key, &resReq, &state, &queue)
	if err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if want := domain.IdempotencyKey(runID, "a", nil, 1); key != want {
		t.Errorf("idempotency_key = %q, want %q", key, want)
	}
	if state != "PENDING" {
		t.Errorf("new attempt state = %s, want PENDING", state)
	}
	if queue != "default" {
		t.Errorf("queue = %s, want default", queue)
	}
	var rv map[string]any
	if err := json.Unmarshal(resReq, &rv); err != nil {
		t.Fatalf("resource_request is not valid JSON: %v", err)
	}
	if got := rv["cpu_millis"]; fmt.Sprint(got) != "1000" {
		t.Errorf("resource_request.cpu_millis = %v, want 1000", got)
	}
}

// At-least-once delivery means Materialize WILL be called twice. It must not
// duplicate anything.
func TestIntegration_MaterializeIsIdempotent(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("first Materialize: %v", err)
	}
	res2, err := e.Materialize(ctx, runID)
	if err != nil {
		t.Fatalf("second Materialize: %v", err)
	}
	if res2.NodesInserted != 0 || res2.EdgesInserted != 0 {
		t.Errorf("second materialize inserted %d nodes / %d edges, want 0/0",
			res2.NodesInserted, res2.EdgesInserted)
	}
	if !res2.AlreadyExisted {
		t.Error("second materialize should report the run already existed")
	}

	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_attempts WHERE run_id = $1::uuid`, runID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	// The crucial assertion: re-materializing must NOT create a second attempt
	// for node a, which would be a double dispatch.
	if attempts != 1 {
		t.Errorf("run has %d attempts after two materializes, want 1", attempts)
	}
}

// The full happy path, driven through the real reconcile loop against Postgres.
func TestIntegration_ReconcileAdvancesTheFrontierToSuccess(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	for _, node := range []string{"a", "b"} {
		settleAttempt(t, pool, runID, node, domain.AttemptSucceeded, map[string]any{"out": "v"})
		res, err := e.Reconcile(ctx, runID)
		if err != nil {
			t.Fatalf("Reconcile after %s: %v", node, err)
		}
		if res.RunTerminal {
			t.Fatalf("run went terminal early after %s: %s", node, res.RunState)
		}
		if len(res.Frontier) != 1 {
			t.Fatalf("frontier after %s = %v, want exactly the next node", node, res.Frontier)
		}
	}

	settleAttempt(t, pool, runID, "c", domain.AttemptSucceeded, nil)
	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("final Reconcile: %v", err)
	}
	if !res.RunTerminal || res.RunState != domain.RunSucceeded {
		t.Fatalf("run state = %s (terminal=%v), want SUCCEEDED", res.RunState, res.RunTerminal)
	}

	states := nodeStates(t, pool, runID)
	for _, id := range []string{"a", "b", "c"} {
		if states[id] != "SUCCEEDED" {
			t.Errorf("node %s = %s, want SUCCEEDED", id, states[id])
		}
	}

	var total int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_attempts WHERE run_id = $1::uuid`, runID).Scan(&total); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if total != 3 {
		t.Errorf("run created %d attempts, want exactly 3 (one per node)", total)
	}
}

// The propagation path end to end: a real FAILED attempt must mark its
// descendants UPSTREAM_FAILED in the database, not FAILED.
func TestIntegration_FailurePropagatesAsUpstreamFailed(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptFailed, nil)

	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res.RunTerminal || res.RunState != domain.RunFailed {
		t.Fatalf("run state = %s (terminal=%v), want FAILED", res.RunState, res.RunTerminal)
	}
	if len(res.Frontier) != 0 {
		t.Errorf("frontier = %v, want empty after a terminal failure", res.Frontier)
	}

	states := nodeStates(t, pool, runID)
	if states["a"] != "FAILED" {
		t.Errorf("node a = %s, want FAILED (it is what actually broke)", states["a"])
	}
	// The distinction that makes a large failure diagnosable at a glance.
	for _, id := range []string{"b", "c"} {
		if states[id] != "UPSTREAM_FAILED" {
			t.Errorf("node %s = %s, want UPSTREAM_FAILED", id, states[id])
		}
	}

	var attempts int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_attempts WHERE run_id = $1::uuid`, runID).Scan(&attempts); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	// No attempt may be created for a node that never ran: that would burn
	// capacity on work whose inputs can never exist.
	if attempts != 1 {
		t.Errorf("run created %d attempts, want 1 (b and c must never be dispatched)", attempts)
	}
}

// all_done is the cleanup rule: it must run even when its upstream failed.
func TestIntegration_AllDoneRunsAfterAFailure(t *testing.T) {
	pool := testPool(t)
	spec := &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "cleanup",
		Tasks: []*nexusv1.TaskSpec{
			task("work", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("cleanup", nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE),
		},
		Edges: []*nexusv1.Edge{{FromNodeId: "work", ToNodeId: "cleanup"}},
	}
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "work", domain.AttemptFailed, nil)

	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Frontier) != 1 || res.Frontier[0] != "cleanup" {
		t.Fatalf("frontier = %v, want [cleanup]: all_done must run despite the failure", res.Frontier)
	}
	if res.RunTerminal {
		t.Error("run must not be terminal while cleanup is still to run")
	}

	settleAttempt(t, pool, runID, "cleanup", domain.AttemptSucceeded, nil)
	res2, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	// The cleanup succeeding does NOT rescue the run: a node terminally failed.
	if res2.RunState != domain.RunFailed {
		t.Errorf("run state = %s, want FAILED (a node failed even though cleanup succeeded)", res2.RunState)
	}
}

// Input resolution against real rows: the downstream attempt's input_refs must
// carry the upstream attempt's output values, per the edge mapping.
func TestIntegration_ResolvesInputRefsFromUpstreamOutputs(t *testing.T) {
	pool := testPool(t)
	spec := &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "io",
		Tasks: []*nexusv1.TaskSpec{
			{
				Id: "extract", TaskRef: "extract:v1", Version: 1, Queue: "default",
				Outputs: map[string]*nexusv1.ValueSpec{"dataset": {Type: "file_ref"}},
				Retry:   &nexusv1.RetryPolicy{MaxAttempts: 1},
			},
			{
				Id: "train", TaskRef: "train:v1", Version: 1, Queue: "default",
				Inputs: map[string]*nexusv1.ValueSpec{"input_data": {Type: "file_ref", Required: true}},
				Retry:  &nexusv1.RetryPolicy{MaxAttempts: 1},
			},
		},
		Edges: []*nexusv1.Edge{{
			FromNodeId: "extract", ToNodeId: "train",
			// key = downstream input, value = upstream output.
			OutputToInput: map[string]string{"input_data": "dataset"},
		}},
	}
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "extract", domain.AttemptSucceeded, map[string]any{
		"dataset": map[string]any{"blob": map[string]any{"bucket": "nexus", "key": "default/abc123"}},
	})
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var raw []byte
	if err := pool.QueryRow(ctx, `
		SELECT input_refs FROM task_attempts
		 WHERE run_id = $1::uuid AND node_id = 'train'`, runID).Scan(&raw); err != nil {
		t.Fatalf("read train's input_refs: %v", err)
	}
	var refs map[string]any
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatalf("input_refs is not valid JSON: %v", err)
	}
	got, ok := refs["input_data"]
	if !ok {
		t.Fatalf("input_refs lacks input_data: %s", raw)
	}
	blob, ok := got.(map[string]any)["blob"].(map[string]any)
	if !ok {
		t.Fatalf("input_data is not a blob ref: %v", got)
	}
	if blob["key"] != "default/abc123" {
		t.Errorf("input_data.blob.key = %v, want default/abc123", blob["key"])
	}
}

// A diamond exercises fan-out and the join, which is where an off-by-one in the
// frontier shows up.
func TestIntegration_DiamondFanOutAndJoin(t *testing.T) {
	pool := testPool(t)
	spec := &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "diamond",
		Tasks: []*nexusv1.TaskSpec{
			task("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("b", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("c", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("d", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		},
		Edges: []*nexusv1.Edge{
			{FromNodeId: "a", ToNodeId: "b"}, {FromNodeId: "a", ToNodeId: "c"},
			{FromNodeId: "b", ToNodeId: "d"}, {FromNodeId: "c", ToNodeId: "d"},
		},
	}
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptSucceeded, nil)

	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Frontier) != 2 {
		t.Fatalf("frontier = %v, want both branches", res.Frontier)
	}

	// The join must NOT fire on the first branch alone.
	settleAttempt(t, pool, runID, "b", domain.AttemptSucceeded, nil)
	res2, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res2.Frontier) != 0 {
		t.Fatalf("frontier = %v, want empty: d must wait for c", res2.Frontier)
	}

	settleAttempt(t, pool, runID, "c", domain.AttemptSucceeded, nil)
	res3, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res3.Frontier) != 1 || res3.Frontier[0] != "d" {
		t.Fatalf("frontier = %v, want [d]", res3.Frontier)
	}
}

// Several reconcilers run concurrently with no leader election (ADR-0011), so
// repeated and concurrent reconciles must never create duplicate attempts.
func TestIntegration_ConcurrentReconcilesDoNotDoubleDispatch(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptSucceeded, nil)

	const racers = 8
	errs := make(chan error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			_, err := e.Reconcile(ctx, runID)
			errs <- err
		}()
	}
	close(start)
	for i := 0; i < racers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent reconcile: %v", err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM task_attempts WHERE run_id = $1::uuid AND node_id = 'b'`, runID).Scan(&n); err != nil {
		t.Fatalf("count b's attempts: %v", err)
	}
	// THE assertion: exactly one attempt for b, no matter how many reconcilers
	// raced. Two rows here would be a double dispatch the scheduler's CAS could
	// not catch, because they would be distinct rows.
	if n != 1 {
		t.Errorf("node b has %d attempts after %d concurrent reconciles, want exactly 1", n, racers)
	}
}

// fail_fast must stop new dispatch AND settle the work it withholds, or the run
// hangs. This is the DB-level counterpart of the regression test in
// internal/dag/resolve_test.go.
func TestIntegration_FailFastHaltsAndStillTerminates(t *testing.T) {
	pool := testPool(t)
	spec := &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "failfast",
		FailFast: true,
		// Two independent branches: a->b fails, root2->independent succeeds. Both
		// branches are connected, because a genuinely disconnected node is a
		// validation error in its own right (see the unreachable-node check).
		Tasks: []*nexusv1.TaskSpec{
			task("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("b", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("root2", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("independent", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		},
		Edges: []*nexusv1.Edge{
			{FromNodeId: "a", ToNodeId: "b"},
			{FromNodeId: "root2", ToNodeId: "independent"},
		},
	}
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	ctx := context.Background()

	res, err := e.Materialize(ctx, runID)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// Both roots start; no failure has happened yet.
	if len(res.Frontier) != 2 {
		t.Fatalf("initial frontier = %v, want both roots", res.Frontier)
	}

	// root2 succeeds BEFORE the failure, so its downstream is already dispatched
	// and in flight when fail_fast engages -- fail_fast withholds NEW work, it
	// does not kill what is already running (§5.2).
	settleAttempt(t, pool, runID, "root2", domain.AttemptSucceeded, nil)
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile after root2: %v", err)
	}
	settleAttempt(t, pool, runID, "independent", domain.AttemptSucceeded, nil)
	settleAttempt(t, pool, runID, "a", domain.AttemptFailed, nil)

	res2, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !res2.Halted {
		t.Error("fail_fast should report Halted after a terminal failure")
	}
	if len(res2.Frontier) != 0 {
		t.Errorf("frontier = %v, want empty under fail_fast", res2.Frontier)
	}
	// The run must actually TERMINATE rather than sitting RUNNING forever.
	if res2.RunState != domain.RunFailed {
		t.Errorf("run state = %s, want FAILED", res2.RunState)
	}
	states := nodeStates(t, pool, runID)
	for id, s := range states {
		switch id {
		case "a":
			if s != "FAILED" {
				t.Errorf("a = %s, want FAILED", s)
			}
		case "independent", "root2":
			if s != "SUCCEEDED" {
				t.Errorf("%s = %s, want SUCCEEDED", id, s)
			}
		default:
			if s == "PENDING" || s == "READY" {
				t.Errorf("node %s left %s: fail_fast must settle withheld work, not hang", id, s)
			}
		}
	}
}

// A cyclic or otherwise invalid spec must be refused at materialization, with a
// precise reason, rather than producing a run that stalls later.
func TestIntegration_MaterializeRefusesAnInvalidSpec(t *testing.T) {
	pool := testPool(t)
	spec := &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "cyclic",
		Tasks: []*nexusv1.TaskSpec{
			task("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
			task("b", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		},
		Edges: []*nexusv1.Edge{
			{FromNodeId: "a", ToNodeId: "b"}, {FromNodeId: "b", ToNodeId: "a"},
		},
	}
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)

	if _, err := e.Materialize(context.Background(), runID); err == nil {
		t.Fatal("expected materialization of a cyclic spec to fail")
	}

	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM task_nodes WHERE run_id = $1::uuid`, runID).Scan(&n); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if n != 0 {
		t.Errorf("an invalid spec materialized %d nodes, want 0", n)
	}
}

func TestIntegration_LoadRunNotFound(t *testing.T) {
	pool := testPool(t)
	e := newTestEngine(t, pool)
	_, err := e.Materialize(context.Background(), uuid.NewString())
	if err == nil {
		t.Fatal("expected an error for an unknown run")
	}
	if !errorsIs(err, store.ErrDAGEngineRunNotFound) {
		t.Errorf("error = %v, want ErrDAGEngineRunNotFound so it maps to NOT_FOUND", err)
	}
}

// The engine must respect operator interventions it does not own: a PAUSING run
// dispatches nothing new.
func TestIntegration_PausedRunDispatchesNothing(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptSucceeded, nil)

	if _, err := pool.Exec(ctx,
		`UPDATE workflow_runs SET state = 'PAUSING' WHERE id = $1::uuid`, runID); err != nil {
		t.Fatalf("pause run: %v", err)
	}

	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Frontier) != 0 {
		t.Errorf("frontier = %v, want empty while PAUSING", res.Frontier)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM task_attempts WHERE run_id = $1::uuid AND node_id = 'b'`, runID).Scan(&n); err != nil {
		t.Fatalf("count b's attempts: %v", err)
	}
	if n != 0 {
		t.Errorf("node b has %d attempts, want 0: pause must not dispatch new work", n)
	}
}

// Reconcile rebuilds node state from ATTEMPTS, so a task_nodes row that has
// drifted (a crashed reconcile, a manual edit) must self-heal rather than need
// repair.
func TestIntegration_ReconcileHealsDriftedNodeState(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptSucceeded, nil)

	// Simulate drift: the attempt succeeded but the node row still says PENDING.
	if _, err := pool.Exec(ctx,
		`UPDATE task_nodes SET state = 'PENDING' WHERE run_id = $1::uuid AND node_id = 'a'`, runID); err != nil {
		t.Fatalf("introduce drift: %v", err)
	}

	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := nodeStates(t, pool, runID)["a"]; got != "SUCCEEDED" {
		t.Errorf("node a = %s, want SUCCEEDED: attempts are the ground truth", got)
	}
}

func TestIntegration_StoreRejectsUnknownRun(t *testing.T) {
	pool := testPool(t)
	st := store.NewDAGEngineStore(pool)
	ctx := context.Background()

	if _, err := st.LoadRun(ctx, uuid.NewString()); err == nil {
		t.Error("LoadRun should fail for an unknown run")
	}
	// Reads of an empty run must be empty, not an error: "no nodes yet" is a
	// normal state during materialization.
	nodes, err := st.LoadNodes(ctx, uuid.NewString())
	if err != nil {
		t.Errorf("LoadNodes on an unknown run should be empty, not an error: %v", err)
	}
	if len(nodes) != 0 {
		t.Errorf("LoadNodes returned %d rows for an unknown run", len(nodes))
	}
}

// The pure engine's decision must be identical whether it comes from memory or
// from a round trip through Postgres. If the two ever disagree, the persistence
// layer is lying about the graph.
func TestIntegration_StoredGraphMatchesTheInMemoryGraph(t *testing.T) {
	pool := testPool(t)
	spec := linearSpec()
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	nodes, err := e.store.LoadNodes(ctx, runID)
	if err != nil {
		t.Fatalf("LoadNodes: %v", err)
	}
	edges, err := e.store.LoadEdges(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEdges: %v", err)
	}
	fromDB := graphFromRows(nodes, edges, spec)
	inMem := dag.Build(spec)

	if fmt.Sprint(fromDB.Nodes()) != fmt.Sprint(inMem.Nodes()) {
		t.Errorf("node order differs: db=%v memory=%v", fromDB.Nodes(), inMem.Nodes())
	}
	if len(fromDB.Edges()) != len(inMem.Edges()) {
		t.Errorf("edge count differs: db=%d memory=%d", len(fromDB.Edges()), len(inMem.Edges()))
	}
	st := dag.States{"a": dag.NodeSucceeded}
	if fmt.Sprint(dag.ReadyFrontier(fromDB, st)) != fmt.Sprint(dag.ReadyFrontier(inMem, st)) {
		t.Error("the stored graph and the in-memory graph disagree on the frontier")
	}
}

// A reconciler that LOST the node's PENDING->READY CAS must not create the
// attempt anyway. This is the store-level shape of the bug the test above aims
// at but cannot reliably produce: eight in-process goroutines sharing one pool
// almost always serialize, so the race window between the CAS and the insert
// stays closed and the test passes over a broken implementation. Driving the
// store directly opens that window deterministically.
//
// Observed in production on a live chaos run: node `notify` held two attempts
// created 0.7ms apart, sharing one idempotency key but holding two distinct
// lease ids, while the run had already been published SUCCEEDED.
func TestIntegration_LosingReconcilerCreatesNoAttempt(t *testing.T) {
	pool := testPool(t)
	runID := seedRun(t, pool, linearSpec())
	e := newTestEngine(t, pool)
	ctx := context.Background()

	if _, err := e.Materialize(ctx, runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleAttempt(t, pool, runID, "a", domain.AttemptSucceeded, nil)

	s := store.NewDAGEngineStore(pool)
	// Both reconcilers computed the same decision: b moves PENDING->READY and
	// gets one attempt. This is precisely what two replicas hold after reading
	// the same committed state.
	transitions := []store.NodeTransition{
		{NodeID: "b", From: domain.NodePending, To: domain.NodeReady},
	}
	newAttempts := []store.NewAttempt{{
		RunID: runID, NodeID: "b", Queue: "default", Priority: 100,
		IdempotencyKey: domain.IdempotencyKey(runID, "b", nil, 1),
		InputRefs:      []byte(`{}`),
	}}

	first, err := s.ApplyNodeStateAndCreateAttempts(ctx, runID, transitions, newAttempts)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if len(first.AttemptsCreated) != 1 {
		t.Fatalf("first apply created %d attempts, want 1", len(first.AttemptsCreated))
	}

	// The loser replays the SAME decision against state the winner has already
	// moved on. Its CAS must miss, and that miss must suppress the insert.
	second, err := s.ApplyNodeStateAndCreateAttempts(ctx, runID, transitions, newAttempts)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(second.AttemptsCreated) != 0 {
		t.Errorf("losing reconciler created %d attempts, want 0", len(second.AttemptsCreated))
	}
	if len(second.NodeCASMisses) != 1 {
		t.Errorf("losing reconciler recorded %d CAS misses, want 1", len(second.NodeCASMisses))
	}

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM task_attempts WHERE run_id = $1::uuid AND node_id = 'b'`, runID).Scan(&n); err != nil {
		t.Fatalf("count b's attempts: %v", err)
	}
	if n != 1 {
		t.Errorf("node b has %d attempts, want exactly 1", n)
	}

	// attempts_used must not have been bumped by the loser either: a drifted
	// retry budget silently shortens how many times the node may legitimately
	// retry later.
	var used int
	if err := pool.QueryRow(ctx, `
		SELECT attempts_used FROM task_nodes WHERE run_id = $1::uuid AND node_id = 'b'`, runID).Scan(&used); err != nil {
		t.Fatalf("read attempts_used: %v", err)
	}
	if used != 1 {
		t.Errorf("node b attempts_used = %d, want 1", used)
	}
}
