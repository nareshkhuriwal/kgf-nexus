//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Integration tests for dynamic expansion (§5.4) and memoization (§6.4), run
// against a real Postgres with migrations 0001-0021 applied:
//
//	NEXUS_POSTGRES_DSN=postgres://... go test -tags integration ./services/dag-engine/...
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// fanoutSpec: list -> process (map over list.files, item "file") -> summarize.
func fanoutSpec(maxFanout int32, fanIn nexusv1.TriggerRule) *nexusv1.WorkflowSpec {
	js := func(req bool) *nexusv1.ValueSpec { return &nexusv1.ValueSpec{Type: "json", Required: req} }
	list := task("list", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	list.Outputs = map[string]*nexusv1.ValueSpec{"files": js(true)}
	process := task("process", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	process.Inputs = map[string]*nexusv1.ValueSpec{"file": js(true)}
	process.Outputs = map[string]*nexusv1.ValueSpec{"result": js(true)}
	process.Map = &nexusv1.MapSpec{OverNodeId: "list", OverOutput: "files", ItemInput: "file", MaxFanout: maxFanout}
	sum := task("summarize", fanIn)
	sum.Inputs = map[string]*nexusv1.ValueSpec{"results": js(true)}
	return &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "fanout",
		Tasks: []*nexusv1.TaskSpec{list, process, sum},
		Edges: []*nexusv1.Edge{
			{FromNodeId: "list", ToNodeId: "process"},
			{FromNodeId: "process", ToNodeId: "summarize", OutputToInput: map[string]string{"results": "result"}},
		},
	}
}

// ref wraps a JSON value as an inline ResultRef, the shape the scheduler stores.
func ref(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	r, err := dag.InlineJSONRef(b)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func settleRef(t *testing.T, pool *pgxpool.Pool, runID, nodeID string, state domain.AttemptState, outputs map[string]json.RawMessage) {
	t.Helper()
	m := map[string]any{}
	for k, v := range outputs {
		m[k] = v
	}
	settleAttempt(t, pool, runID, nodeID, state, m)
}

func expansionRows(t *testing.T, pool *pgxpool.Pool, runID string) []store.DEExpansionRow {
	t.Helper()
	rows, err := store.NewDAGEngineStore(pool).LoadExpansions(context.Background(), runID)
	if err != nil {
		t.Fatalf("load expansions: %v", err)
	}
	return rows
}

func childIDs(states map[string]string) []string {
	var out []string
	for id := range states {
		if strings.HasPrefix(id, "process[") {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

func pendingAttempts(t *testing.T, pool *pgxpool.Pool, runID string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT node_id, count(*) FROM task_attempts WHERE run_id = $1::uuid GROUP BY node_id`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		out[id] = n
	}
	return out
}

// startFanout materializes a fanout run and completes `list` with `items`.
func startFanout(t *testing.T, pool *pgxpool.Pool, spec *nexusv1.WorkflowSpec, items []any) (string, *Engine) {
	t.Helper()
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	if _, err := e.Materialize(context.Background(), runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	settleRef(t, pool, runID, "list", domain.AttemptSucceeded, map[string]json.RawMessage{"files": ref(t, items)})
	return runID, e
}

func TestIntegration_ExpansionCardinalities(t *testing.T) {
	pool := testPool(t)
	for _, n := range []int{0, 1, 7} {
		t.Run(fmt.Sprintf("%d_items", n), func(t *testing.T) {
			items := make([]any, n)
			for i := range items {
				items[i] = fmt.Sprintf("s3://bucket/file-%d", i)
			}
			runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), items)
			ctx := context.Background()

			res, err := e.Reconcile(ctx, runID)
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			states := nodeStates(t, pool, runID)
			kids := childIDs(states)
			if len(kids) != n {
				t.Fatalf("materialized %d children, want %d (%v)", len(kids), n, kids)
			}
			exp := expansionRows(t, pool, runID)
			if len(exp) != 1 || exp[0].Outcome != "EXPANDED" || int(exp[0].ItemCount) != n || exp[0].SourceAttemptID == nil {
				t.Fatalf("expansion record = %+v", exp)
			}
			if n == 0 {
				// Empty collection: the template succeeds at once and the fan-in runs
				// over an empty list in the SAME reconcile call.
				if states["process"] != "SUCCEEDED" || states["summarize"] != "READY" {
					t.Fatalf("states = %v", states)
				}
				assertFanInInput(t, pool, runID, "[]")
				return
			}
			if states["process"] != "RUNNING" {
				t.Fatalf("template = %s, want RUNNING", states["process"])
			}
			// Every child got exactly one PENDING attempt in the same call, with its
			// own element and a per-child idempotency key built from map_index.
			att := pendingAttempts(t, pool, runID)
			for i, id := range kids {
				_ = i
				if att[id] != 1 {
					t.Fatalf("%s has %d attempts, want 1", id, att[id])
				}
			}
			for i := 0; i < n; i++ {
				id := dag.ChildID("process", i)
				var key string
				var inputs []byte
				var parent string
				var idx int
				if err := pool.QueryRow(ctx, `
					SELECT a.idempotency_key, a.input_refs, n.parent_node_id, n.map_index
					  FROM task_attempts a JOIN task_nodes n USING (run_id, node_id)
					 WHERE a.run_id = $1::uuid AND a.node_id = $2`, runID, id).
					Scan(&key, &inputs, &parent, &idx); err != nil {
					t.Fatalf("read child %s: %v", id, err)
				}
				if parent != "process" || idx != i {
					t.Errorf("%s parent/index = %s/%d", id, parent, idx)
				}
				i2 := i
				if want := domain.IdempotencyKey(runID, "process", &i2, 1); key != want {
					t.Errorf("%s idempotency key = %s, want the map_index-derived %s", id, key, want)
				}
				var refs map[string]json.RawMessage
				_ = json.Unmarshal(inputs, &refs)
				if got := decodeInline(t, refs["file"]); got != fmt.Sprintf("%q", items[i]) {
					t.Errorf("%s item = %s, want %q", id, got, items[i])
				}
			}
			if len(res.Expanded) != 1 {
				t.Errorf("Expanded = %v", res.Expanded)
			}

			// Fan-in waits for every child: finish all but one.
			for i := 0; i < n-1; i++ {
				settleRef(t, pool, runID, dag.ChildID("process", i), domain.AttemptSucceeded,
					map[string]json.RawMessage{"result": ref(t, map[string]int{"n": i})})
			}
			if _, err := e.Reconcile(ctx, runID); err != nil {
				t.Fatal(err)
			}
			if s := nodeStates(t, pool, runID); s["summarize"] != "PENDING" || s["process"] != "RUNNING" {
				t.Fatalf("fan-in started early: %v", s)
			}
			settleRef(t, pool, runID, dag.ChildID("process", n-1), domain.AttemptSucceeded,
				map[string]json.RawMessage{"result": ref(t, map[string]int{"n": n - 1})})
			if _, err := e.Reconcile(ctx, runID); err != nil {
				t.Fatal(err)
			}
			s := nodeStates(t, pool, runID)
			if s["process"] != "SUCCEEDED" || s["summarize"] != "READY" {
				t.Fatalf("after last child: %v", s)
			}
			var want []string
			for i := 0; i < n; i++ {
				want = append(want, fmt.Sprintf(`{"n":%d}`, i))
			}
			assertFanInInput(t, pool, runID, "["+strings.Join(want, ",")+"]")

			settleRef(t, pool, runID, "summarize", domain.AttemptSucceeded, nil)
			res, err = e.Reconcile(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if res.RunState != domain.RunSucceeded {
				t.Fatalf("run = %s, want SUCCEEDED", res.RunState)
			}
		})
	}
}

func decodeInline(t *testing.T, r json.RawMessage) string {
	t.Helper()
	var m struct {
		Inline []byte `json:"inline"`
	}
	if err := json.Unmarshal(r, &m); err != nil {
		t.Fatalf("not a ref: %s", r)
	}
	return string(m.Inline)
}

func assertFanInInput(t *testing.T, pool *pgxpool.Pool, runID, want string) {
	t.Helper()
	var inputs []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT input_refs FROM task_attempts WHERE run_id = $1::uuid AND node_id = 'summarize'`, runID).
		Scan(&inputs); err != nil {
		t.Fatalf("read fan-in attempt: %v", err)
	}
	var refs map[string]json.RawMessage
	_ = json.Unmarshal(inputs, &refs)
	if got := decodeInline(t, refs["results"]); got != want {
		t.Fatalf("fan-in input = %s, want %s", got, want)
	}
}

// Over the bound: the expansion FAILS explicitly, nothing is materialized, and
// the run settles (FAILED) rather than wedging.
func TestIntegration_ExpansionOverLimitFailsExplicitly(t *testing.T) {
	pool := testPool(t)
	runID, e := startFanout(t, pool, fanoutSpec(3, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		[]any{1, 2, 3, 4})
	res, err := e.Reconcile(context.Background(), runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	states := nodeStates(t, pool, runID)
	if n := len(childIDs(states)); n != 0 {
		t.Fatalf("over-limit expansion materialized %d children", n)
	}
	if states["process"] != "FAILED" || states["summarize"] != "UPSTREAM_FAILED" {
		t.Fatalf("states = %v", states)
	}
	exp := expansionRows(t, pool, runID)
	if len(exp) != 1 || exp[0].Outcome != "FAILED" || exp[0].Reason == nil || *exp[0].Reason != dag.ExpandMaxFanoutExceeded {
		t.Fatalf("expansion record = %+v", exp)
	}
	if res.RunState != domain.RunFailed || !res.RunTerminal {
		t.Fatalf("run = %s terminal=%v, want FAILED", res.RunState, res.RunTerminal)
	}
}

// A duplicate completion report for the upstream means the reconcile is triggered
// twice -- and, with several engine replicas, concurrently. Exactly one expansion
// must result.
func TestIntegration_ExpansionIsIdempotentUnderDuplicateReports(t *testing.T) {
	pool := testPool(t)
	runID, _ := startFanout(t, pool, fanoutSpec(50, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		[]any{"a", "b", "c", "d", "e", "f"})

	const replicas = 8
	var wg sync.WaitGroup
	errs := make(chan error, replicas)
	for i := 0; i < replicas; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A separate engine per goroutine: replicas share nothing but the DB.
			if _, err := newTestEngine(t, pool).Reconcile(context.Background(), runID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent reconcile: %v", err)
	}
	states := nodeStates(t, pool, runID)
	if n := len(childIDs(states)); n != 6 {
		t.Fatalf("children = %d, want exactly 6", n)
	}
	if n := len(expansionRows(t, pool, runID)); n != 1 {
		t.Fatalf("expansion records = %d, want 1", n)
	}
	for id, n := range pendingAttempts(t, pool, runID) {
		if n != 1 {
			t.Fatalf("%s has %d attempts, want 1 (double dispatch)", id, n)
		}
	}
	// And a later replay changes nothing.
	if _, err := newTestEngine(t, pool).Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if n := len(childIDs(nodeStates(t, pool, runID))); n != 6 {
		t.Fatalf("replay changed children to %d", n)
	}
}

// A crash part way through the expansion transaction must leave NO trace, and
// the retry must produce the complete, identical expansion.
//
// The crash is injected as a trigger that fails the insert of one specific
// child, which aborts the transaction after the template CAS, the record and
// every earlier child row were written inside it.
func TestIntegration_CrashMidExpansionLeavesNoPartialState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS),
		[]any{"a", "b", "c", "d"})

	fn := "crash_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE FUNCTION %[1]s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
		  IF NEW.run_id = '%[2]s'::uuid AND NEW.node_id = 'process[2]' THEN
		    RAISE EXCEPTION 'injected crash mid-expansion';
		  END IF;
		  RETURN NEW;
		END $$;
		CREATE TRIGGER %[1]s BEFORE INSERT ON task_nodes FOR EACH ROW EXECUTE FUNCTION %[1]s();`,
		fn, runID)); err != nil {
		t.Fatalf("install crash trigger: %v", err)
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP TRIGGER %[1]s ON task_nodes; DROP FUNCTION %[1]s();`, fn)); err != nil {
			t.Fatalf("drop crash trigger: %v", err)
		}
	}
	t.Cleanup(drop)

	if _, err := e.Reconcile(ctx, runID); err == nil || !strings.Contains(err.Error(), "injected crash") {
		t.Fatalf("Reconcile err = %v, want the injected crash", err)
	}
	states := nodeStates(t, pool, runID)
	if states["process"] != "PENDING" || len(childIDs(states)) != 0 || len(expansionRows(t, pool, runID)) != 0 {
		t.Fatalf("partial expansion survived the crash: %v", states)
	}
	var edges int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM task_edges WHERE run_id = $1::uuid AND to_node_id LIKE 'process[%'`, runID).Scan(&edges)
	if edges != 0 {
		t.Fatalf("%d child edges survived the crash", edges)
	}

	drop()
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatalf("Reconcile after recovery: %v", err)
	}
	states = nodeStates(t, pool, runID)
	if got := childIDs(states); len(got) != 4 || states["process"] != "RUNNING" {
		t.Fatalf("after recovery: %v", states)
	}
}

func TestIntegration_CancelBeforeAndAfterExpansion(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("before", func(t *testing.T) {
		runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), []any{1, 2})
		setRunState(t, pool, runID, "CANCELLING")
		res, err := e.Reconcile(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		states := nodeStates(t, pool, runID)
		if len(childIDs(states)) != 0 || len(expansionRows(t, pool, runID)) != 0 {
			t.Fatalf("a cancelling run expanded: %v", states)
		}
		if states["process"] != "CANCELLED" || res.RunState != domain.RunCancelled {
			t.Fatalf("states = %v run = %s", states, res.RunState)
		}
	})

	t.Run("after", func(t *testing.T) {
		runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), []any{1, 2, 3})
		if _, err := e.Reconcile(ctx, runID); err != nil {
			t.Fatal(err)
		}
		settleRef(t, pool, runID, "process[0]", domain.AttemptSucceeded, nil)
		// The scheduler's cancel fan-out moves the in-flight attempts; mirror it.
		if _, err := pool.Exec(ctx, `UPDATE task_attempts SET state='CANCELLED', ended_at=now()
			WHERE run_id=$1::uuid AND state IN ('PENDING','DISPATCHED','RUNNING')`, runID); err != nil {
			t.Fatal(err)
		}
		setRunState(t, pool, runID, "CANCELLING")
		res, err := e.Reconcile(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		states := nodeStates(t, pool, runID)
		// summarize is SKIPPED, not CANCELLED: its upstream settled CANCELLED before
		// the cancel sweep reached it, and a CANCELLED upstream makes all_success
		// unsatisfiable -- the same rule a static node follows (engine.Propagate).
		if states["process[1]"] != "CANCELLED" || states["process"] != "CANCELLED" || states["summarize"] != "SKIPPED" {
			t.Fatalf("states = %v", states)
		}
		if res.RunState != domain.RunCancelled {
			t.Fatalf("run = %s, want CANCELLED", res.RunState)
		}
	})
}

func TestIntegration_PausedRunDoesNotExpand(t *testing.T) {
	pool := testPool(t)
	runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), []any{1, 2})
	setRunState(t, pool, runID, "PAUSED")
	if _, err := e.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(childIDs(nodeStates(t, pool, runID))) != 0 {
		t.Fatal("a paused run expanded")
	}
	setRunState(t, pool, runID, "RUNNING")
	if _, err := e.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if n := len(childIDs(nodeStates(t, pool, runID))); n != 2 {
		t.Fatalf("after resume children = %d, want 2", n)
	}
}

// A failed child with retries exhausted fails the template; an all_done fan-in
// still runs and sees null in the failed child's slot.
func TestIntegration_ChildFailureWithTolerantFanIn(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_DONE), []any{"x", "y"})
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatal(err)
	}
	settleRef(t, pool, runID, "process[0]", domain.AttemptSucceeded, map[string]json.RawMessage{"result": ref(t, "ok")})
	settleRef(t, pool, runID, "process[1]", domain.AttemptFailed, nil) // max_attempts 1: exhausted
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatal(err)
	}
	states := nodeStates(t, pool, runID)
	if states["process"] != "FAILED" || states["summarize"] != "READY" {
		t.Fatalf("states = %v", states)
	}
	assertFanInInput(t, pool, runID, `["ok",null]`)
}

func setRunState(t *testing.T, pool *pgxpool.Pool, runID, state string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE workflow_runs SET state = $2::run_state WHERE id = $1::uuid`, runID, state); err != nil {
		t.Fatalf("set run state: %v", err)
	}
}

// ── Memoization ──────────────────────────────────────────────────────────────

func memoSpec(name string, ttlMs int64, mode nexusv1.IdempotencyMode) *nexusv1.WorkflowSpec {
	a := task("a", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	a.TaskRef = "memo.pure:" + name
	a.Idempotency = mode
	a.Cache = &nexusv1.CachePolicy{Enabled: true, TtlMs: ttlMs}
	a.Inputs = map[string]*nexusv1.ValueSpec{"x": {Type: "json", Required: true}}
	a.InputLiterals = map[string]string{"x": `{"k":1}`}
	a.Outputs = map[string]*nexusv1.ValueSpec{"y": {Type: "json"}}
	b := task("b", nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS)
	return &nexusv1.WorkflowSpec{
		ApiVersion: "v1", Namespace: "default", Name: "memo-" + name,
		Tasks: []*nexusv1.TaskSpec{a, b},
		Edges: []*nexusv1.Edge{{FromNodeId: "a", ToNodeId: "b"}},
	}
}

type attemptRow struct {
	State      string
	CacheKey   *string
	CacheHitOf *string
	OutputRef  []byte
}

func attemptOf(t *testing.T, pool *pgxpool.Pool, runID, nodeID string) attemptRow {
	t.Helper()
	var a attemptRow
	if err := pool.QueryRow(context.Background(), `
		SELECT state::text, cache_key, cache_hit_of::text, output_ref FROM task_attempts
		 WHERE run_id = $1::uuid AND node_id = $2 ORDER BY attempt_number DESC LIMIT 1`, runID, nodeID).
		Scan(&a.State, &a.CacheKey, &a.CacheHitOf, &a.OutputRef); err != nil {
		t.Fatalf("read attempt %s/%s: %v", runID, nodeID, err)
	}
	return a
}

func materialize(t *testing.T, pool *pgxpool.Pool, spec *nexusv1.WorkflowSpec) (string, *Engine) {
	t.Helper()
	runID := seedRun(t, pool, spec)
	e := newTestEngine(t, pool)
	if _, err := e.Materialize(context.Background(), runID); err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return runID, e
}

func TestIntegration_MemoMissThenHit(t *testing.T) {
	pool := testPool(t)
	name := uuid.NewString()[:8]
	spec := memoSpec(name, time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)

	run1, _ := materialize(t, pool, spec)
	first := attemptOf(t, pool, run1, "a")
	if first.State != "PENDING" || first.CacheKey == nil || first.CacheHitOf != nil {
		t.Fatalf("first run must MISS and execute: %+v", first)
	}
	settleRef(t, pool, run1, "a", domain.AttemptSucceeded, map[string]json.RawMessage{"y": ref(t, 42)})

	run2, e2 := materialize(t, pool, spec)
	hit := attemptOf(t, pool, run2, "a")
	if hit.State != "SUCCEEDED" || hit.CacheHitOf == nil || *hit.CacheKey != *first.CacheKey {
		t.Fatalf("second run must HIT: %+v", hit)
	}
	if string(hit.OutputRef) == "" || !strings.Contains(string(hit.OutputRef), `"y"`) {
		t.Fatalf("hit did not reuse the output_ref: %s", hit.OutputRef)
	}
	// The hit released the downstream in the SAME Materialize call: nothing will
	// ever report on a memoized attempt, so nothing else would.
	if s := nodeStates(t, pool, run2); s["a"] != "SUCCEEDED" || s["b"] != "READY" {
		t.Fatalf("after hit: %v", s)
	}
	_ = e2

	// A hit is never itself a source: a third run reuses the ORIGINAL attempt.
	run3, _ := materialize(t, pool, spec)
	third := attemptOf(t, pool, run3, "a")
	var origID string
	_ = pool.QueryRow(context.Background(),
		`SELECT id::text FROM task_attempts WHERE run_id=$1::uuid AND node_id='a'`, run1).Scan(&origID)
	if third.CacheHitOf == nil || *third.CacheHitOf != origID {
		t.Fatalf("third run hit %v, want the original executed attempt %s", third.CacheHitOf, origID)
	}
}

func TestIntegration_MemoNeverReusesFailuresOrExpiredOrNonIdempotent(t *testing.T) {
	pool := testPool(t)

	t.Run("failed result", func(t *testing.T) {
		spec := memoSpec(uuid.NewString()[:8], time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)
		run1, _ := materialize(t, pool, spec)
		settleRef(t, pool, run1, "a", domain.AttemptFailed, nil)
		run2, _ := materialize(t, pool, spec)
		if a := attemptOf(t, pool, run2, "a"); a.State != "PENDING" || a.CacheHitOf != nil {
			t.Fatalf("a FAILED result was reused: %+v", a)
		}
	})

	t.Run("ttl expiry", func(t *testing.T) {
		spec := memoSpec(uuid.NewString()[:8], 1000, nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)
		run1, _ := materialize(t, pool, spec)
		settleRef(t, pool, run1, "a", domain.AttemptSucceeded, nil)
		// Age the source past the 1s TTL rather than sleeping.
		if _, err := pool.Exec(context.Background(),
			`UPDATE task_attempts SET ended_at = now() - interval '5 seconds' WHERE run_id = $1::uuid`, run1); err != nil {
			t.Fatal(err)
		}
		run2, _ := materialize(t, pool, spec)
		if a := attemptOf(t, pool, run2, "a"); a.State != "PENDING" || a.CacheHitOf != nil {
			t.Fatalf("an expired entry was reused: %+v", a)
		}
	})

	t.Run("idempotency none", func(t *testing.T) {
		spec := memoSpec(uuid.NewString()[:8], time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_NONE)
		run1, _ := materialize(t, pool, spec)
		if a := attemptOf(t, pool, run1, "a"); a.CacheKey != nil {
			t.Fatalf("idempotency:none got a cache key: %+v", a)
		}
		settleRef(t, pool, run1, "a", domain.AttemptSucceeded, nil)
		run2, _ := materialize(t, pool, spec)
		if a := attemptOf(t, pool, run2, "a"); a.State != "PENDING" || a.CacheHitOf != nil {
			t.Fatalf("idempotency:none was memoized: %+v", a)
		}
	})

	t.Run("different inputs", func(t *testing.T) {
		spec := memoSpec(uuid.NewString()[:8], time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)
		run1, _ := materialize(t, pool, spec)
		settleRef(t, pool, run1, "a", domain.AttemptSucceeded, nil)
		spec.Tasks[0].InputLiterals = map[string]string{"x": `{"k":2}`}
		run2, _ := materialize(t, pool, spec)
		if a := attemptOf(t, pool, run2, "a"); a.CacheHitOf != nil {
			t.Fatalf("different inputs hit the cache: %+v", a)
		}
	})
}

func TestIntegration_MemoInvalidation(t *testing.T) {
	pool := testPool(t)
	name := uuid.NewString()[:8]
	spec := memoSpec(name, time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)
	run1, e := materialize(t, pool, spec)
	settleRef(t, pool, run1, "a", domain.AttemptSucceeded, nil)

	if _, _, err := e.InvalidateCache(context.Background(), store.MemoInvalidation{
		Namespace: "default", TaskRef: spec.Tasks[0].TaskRef, Reason: "bad upstream data", Actor: "user:test",
	}); err != nil {
		t.Fatalf("InvalidateCache: %v", err)
	}
	run2, _ := materialize(t, pool, spec)
	if a := attemptOf(t, pool, run2, "a"); a.CacheHitOf != nil {
		t.Fatalf("invalidated entry was reused: %+v", a)
	}
	// Results produced AFTER the invalidation are trusted again.
	settleRef(t, pool, run2, "a", domain.AttemptSucceeded, nil)
	run3, _ := materialize(t, pool, spec)
	if a := attemptOf(t, pool, run3, "a"); a.CacheHitOf == nil {
		t.Fatalf("a post-invalidation result was not reused: %+v", a)
	}
}

// Concurrent identical runs: both may miss (both execute), or one may hit the
// other -- but no run may ever hit a result that is not SUCCEEDED, and no hit
// may reference another hit.
func TestIntegration_MemoConcurrentIdenticalRuns(t *testing.T) {
	pool := testPool(t)
	spec := memoSpec(uuid.NewString()[:8], time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)
	const runs = 6
	ids := make([]string, runs)
	for i := range ids {
		ids[i] = seedRun(t, pool, spec)
	}
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(runID string) {
			defer wg.Done()
			if _, err := newTestEngine(t, pool).Materialize(context.Background(), runID); err != nil {
				t.Errorf("Materialize: %v", err)
			}
		}(ids[i])
	}
	wg.Wait()
	for _, id := range ids {
		if a := attemptOf(t, pool, id, "a"); a.CacheHitOf != nil {
			t.Fatalf("run %s hit a cache no attempt had populated: %+v", id, a)
		}
	}
	// One finishes; the rest re-reconcile concurrently -- they are already
	// PENDING, so they stay misses (the lookup is only at creation). A NEW run
	// now hits, and hits the executed attempt.
	settleRef(t, pool, ids[0], "a", domain.AttemptSucceeded, nil)
	late := make([]string, runs)
	for i := range late {
		late[i] = seedRun(t, pool, spec)
	}
	for i := range late {
		wg.Add(1)
		go func(runID string) {
			defer wg.Done()
			if _, err := newTestEngine(t, pool).Materialize(context.Background(), runID); err != nil {
				t.Errorf("Materialize: %v", err)
			}
		}(late[i])
	}
	wg.Wait()
	var src string
	_ = pool.QueryRow(context.Background(),
		`SELECT id::text FROM task_attempts WHERE run_id=$1::uuid AND node_id='a'`, ids[0]).Scan(&src)
	for _, id := range late {
		a := attemptOf(t, pool, id, "a")
		if a.CacheHitOf == nil || *a.CacheHitOf != src {
			t.Fatalf("late run %s: %+v, want a hit on %s", id, a, src)
		}
	}
}

// The new tables are owned by exactly the services the migrations say, and
// append-only where they claim to be.
func TestIntegration_NewTableGrants(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var roles int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_roles WHERE rolname LIKE 'nexus\_%'`).Scan(&roles)
	if roles != 6 {
		t.Skip("per-service roles (0005) not applied")
	}
	check := func(role, table, priv string, want bool) {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT has_table_privilege($1,$2,$3)`, role, table, priv).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("%s %s on %s = %v, want %v", role, priv, table, ok, want)
		}
	}
	check("nexus_dag_engine", "task_expansions", "INSERT", true)
	check("nexus_dag_engine", "task_expansions", "UPDATE", false)
	check("nexus_dag_engine", "task_expansions", "DELETE", false)
	check("nexus_dag_engine", "memo_invalidations", "INSERT", true)
	check("nexus_dag_engine", "memo_invalidations", "DELETE", false)
	check("nexus_resource_manager", "worker_quarantines", "INSERT", true)
	check("nexus_resource_manager", "worker_quarantines", "UPDATE", true)
	check("nexus_resource_manager", "worker_quarantines", "DELETE", false)
	for _, other := range []string{"nexus_api_gateway", "nexus_scheduler", "nexus_workflow_manager", "nexus_event_service"} {
		for _, table := range []string{"task_expansions", "memo_invalidations", "worker_quarantines"} {
			check(other, table, "INSERT", false)
			check(other, table, "SELECT", true)
		}
	}
	check("nexus_scheduler", "task_expansions", "INSERT", false)
	check("nexus_dag_engine", "worker_quarantines", "INSERT", false)
}

// The gateway's run-detail reads must see what expansion and memoization wrote
// (parent_node_id, the expansion record, cache_hit_of) -- exercised against the
// real schema, since the REST unit tests use a fake database.
func TestIntegration_GatewayReadsExpansionAndMemo(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	runID, e := startFanout(t, pool, fanoutSpec(10, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), []any{"a", "b"})
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatal(err)
	}
	nodes, err := store.GatewayListRunNodes(ctx, pool, runID)
	if err != nil {
		t.Fatalf("GatewayListRunNodes: %v", err)
	}
	var kids int
	for _, n := range nodes {
		if n.ParentNodeID != nil && *n.ParentNodeID == "process" && n.MapIndex != nil {
			kids++
		}
	}
	if kids != 2 {
		t.Fatalf("gateway saw %d children, want 2", kids)
	}
	exps, err := store.GatewayListRunExpansions(ctx, pool, runID)
	if err != nil || len(exps) != 1 || exps[0].ItemCount != 2 {
		t.Fatalf("expansions = %+v err=%v", exps, err)
	}

	spec := memoSpec(uuid.NewString()[:8], time.Hour.Milliseconds(), nexusv1.IdempotencyMode_IDEMPOTENCY_MODE_REQUIRED)
	run1, _ := materialize(t, pool, spec)
	settleRef(t, pool, run1, "a", domain.AttemptSucceeded, nil)
	run2, _ := materialize(t, pool, spec)
	atts, err := store.GatewayListRunAttempts(ctx, pool, run2)
	if err != nil {
		t.Fatalf("GatewayListRunAttempts: %v", err)
	}
	var hit bool
	for _, a := range atts {
		if a.NodeID == "a" && a.CacheHitOf != nil {
			hit = true
		}
	}
	if !hit {
		t.Fatal("gateway did not report the memo hit")
	}
}

// fanInRun expands n children, completes each with a `size`-char string, and
// reconciles once with the engine's fan-in limit set to limit.
func fanInRun(t *testing.T, pool *pgxpool.Pool, n, size int, limit int64) (string, *ReconcileResult) {
	t.Helper()
	ctx := context.Background()
	items := make([]any, n)
	for i := range items {
		items[i] = i
	}
	runID, e := startFanout(t, pool, fanoutSpec(int32(n), nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS), items)
	e.SetFanInLimit(limit)
	if _, err := e.Reconcile(ctx, runID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		settleRef(t, pool, runID, dag.ChildID("process", i), domain.AttemptSucceeded,
			map[string]json.RawMessage{"result": ref(t, strings.Repeat("x", size))})
	}
	res, err := e.Reconcile(ctx, runID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return runID, res
}

type refusalRow struct {
	Reason, Detail  string
	Limit           int64
	Total, Accepted int
}

func readRefusal(t *testing.T, pool *pgxpool.Pool, runID string) *refusalRow {
	t.Helper()
	var r refusalRow
	err := pool.QueryRow(context.Background(), `
		SELECT reason, detail, limit_bytes, children_total, children_before_limit
		  FROM fan_in_refusals WHERE run_id = $1::uuid AND node_id = 'summarize'`, runID).
		Scan(&r.Reason, &r.Detail, &r.Limit, &r.Total, &r.Accepted)
	if err != nil {
		return nil
	}
	return &r
}

// The fan-in bound, end to end: under and exactly at the cap the consumer runs
// with the full array; one byte over, it FAILS with the recorded reason, gets
// no attempt, the run settles FAILED, and assembly stopped early.
func TestIntegration_FanInSizeLimit(t *testing.T) {
	pool := testPool(t)
	const n, size = 40, 1000
	exact := int64(n*(size+2) + (n - 1) + 2) // ["xx..","xx..",...]

	for _, tc := range []struct {
		name  string
		limit int64
		ok    bool
	}{
		{"under the cap", exact + 100, true},
		{"exactly at the cap", exact, true},
		{"one byte over the cap", exact - 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID, res := fanInRun(t, pool, n, size, tc.limit)
			states := nodeStates(t, pool, runID)
			att := pendingAttempts(t, pool, runID)
			if tc.ok {
				if states["summarize"] != "READY" || att["summarize"] != 1 {
					t.Fatalf("under/at cap: summarize=%s attempts=%d", states["summarize"], att["summarize"])
				}
				var inputs []byte
				_ = pool.QueryRow(context.Background(),
					`SELECT input_refs FROM task_attempts WHERE run_id=$1::uuid AND node_id='summarize'`, runID).Scan(&inputs)
				var refs map[string]json.RawMessage
				_ = json.Unmarshal(inputs, &refs)
				if got := int64(len(decodeInline(t, refs["results"]))); got != exact {
					t.Fatalf("fan-in payload = %d bytes, want %d", got, exact)
				}
				if readRefusal(t, pool, runID) != nil {
					t.Fatal("a refusal was recorded for a fan-in that fit")
				}
				return
			}
			if states["summarize"] != "FAILED" {
				t.Fatalf("over cap: summarize = %s, want FAILED", states["summarize"])
			}
			if att["summarize"] != 0 {
				t.Fatalf("over cap: summarize got %d attempt(s); nothing may be materialized", att["summarize"])
			}
			r := readRefusal(t, pool, runID)
			if r == nil {
				t.Fatal("over cap: no fan_in_refusals row -- the reason is not operator-visible")
			}
			if r.Reason != dag.FanInTooLargeReason || r.Limit != exact-1 || r.Total != n ||
				!strings.Contains(r.Detail, "NEXUS_DAG_MAX_FANIN_BYTES") {
				t.Fatalf("refusal = %+v", r)
			}
			if seen, err := store.GatewayListRunFanInRefusals(context.Background(), pool, runID); err != nil ||
				!strings.HasPrefix(seen["summarize"], dag.FanInTooLargeReason) {
				t.Fatalf("the gateway does not surface the reason: %v %v", seen, err)
			}
			if r.Accepted >= n {
				t.Fatalf("assembly did not stop early: accepted %d of %d", r.Accepted, n)
			}
			if res.RunState != domain.RunFailed {
				t.Fatalf("run = %s, want FAILED (the refusal settles downstream in the same call)", res.RunState)
			}
		})
	}
}

func TestParseFanInLimit(t *testing.T) {
	for in, ok := range map[string]bool{
		"": true, "16777216": true, "65536": true, "268435456": true,
		"65535": false, "268435457": false, "lots": false, "-1": false,
	} {
		if _, err := parseFanInLimit(in); (err == nil) != ok {
			t.Errorf("parseFanInLimit(%q) err = %v, want ok=%v", in, err, ok)
		}
	}
}
