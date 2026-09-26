//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/timers"
)

// CONTRACT-FINDINGS F160: run advancement must not depend on a best-effort
// synchronous RPC.
//
// On the dev cluster a 500-way fan-out lost 446 of its post-report reconcile
// calls to DeadlineExceeded (the dag-engine's pool was 3 connections, and ~500
// reconciles of the same run arrived at once). Every attempt SUCCEEDED; 26
// children stayed RUNNING and the fan-in stayed PENDING, forever, because
// nothing ever asked the engine again.
//
// These tests drive the real engine against a real, migrated Postgres. The
// "fleet" plays the scheduler and the workers: it settles each PENDING attempt
// (the terminal write fires migration 0039's trigger, exactly as the
// scheduler's report transaction does) and then plays the gateway, calling
// ReconcileRun -- except when the fault injector says the call is lost.

// dbRunStates plays the workflow-manager's SetRunState: it writes the run's
// state directly. failNext fails that many publishes first, the way a
// workflow-manager database fault does.
type dbRunStates struct {
	pool     *pgxpool.Pool
	failNext atomic.Int32
}

func (w *dbRunStates) SetRunState(ctx context.Context, runID string, s domain.RunState, _ string) error {
	if w.failNext.Add(-1) >= 0 {
		return errors.New("injected: workflow-manager unavailable")
	}
	_, err := w.pool.Exec(ctx, `
		UPDATE workflow_runs SET state = $2::run_state,
		       ended_at = CASE WHEN $2 IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT') THEN now() END
		 WHERE id = $1::uuid AND state NOT IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')`,
		runID, string(s))
	return err
}

// durableEngine is one dag-engine replica: an engine, its gRPC surface and its
// reconcile-queue consumer, on its own pool.
type durableEngine struct {
	pool   *pgxpool.Pool
	engine *Engine
	srv    *GRPCServer
	queue  *ReconcileQueue
	stop   context.CancelFunc
	done   chan struct{}
}

func startDurableEngine(t *testing.T, maxConns int32, runs RunStateWriter, consume bool) *durableEngine {
	t.Helper()
	// NEXUS_DB_MAX_CONNS overrides a pool's compiled-in size (ApplyPoolEnv),
	// so the replica's cap is set through it: the dev cluster's engine was
	// capped exactly this way.
	t.Setenv(store.EnvDBMaxConns, strconv.Itoa(int(maxConns)))
	cfg := store.DefaultPoolConfig(testdb.DSN(t))
	cfg.MaxConns = maxConns
	cfg.MinConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := store.NewPool(ctx, cfg)
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	st := store.NewDAGEngineStore(pool)
	e := NewEngine(st, runs, nil, testLogger())
	d := &durableEngine{pool: pool, engine: e, srv: NewGRPCServer(e, testLogger()), done: make(chan struct{})}
	if consume {
		d.queue = NewReconcileQueue(e, st, ReconcileQueueConfig{
			Owner:        fmt.Sprintf("test-%d", time.Now().UnixNano()),
			PollInterval: 50 * time.Millisecond,
			Lease:        2 * time.Second,
			BatchSize:    1000,
			BackoffBase:  100 * time.Millisecond,
			BackoffMax:   time.Second,
		}, testLogger())
		qctx, qcancel := context.WithCancel(context.Background())
		d.stop = qcancel
		go func() { defer close(d.done); d.queue.Run(qctx) }()
	} else {
		close(d.done)
		d.stop = func() {}
	}
	t.Cleanup(d.shutdown)
	return d
}

// shutdown stops the consumer and closes the pool: a replica going away.
func (d *durableEngine) shutdown() {
	d.stop()
	<-d.done
	d.pool.Close()
}

// fleet settles every PENDING attempt of a run as SUCCEEDED, with the outputs
// the fan-out spec needs, and then "reports" it.
type fleet struct {
	t       *testing.T
	pool    *pgxpool.Pool
	runID   string
	outputs map[string][]byte // node-id prefix -> output_ref
	report  func(n int64, nodeID string)
	settled atomic.Int64
	// limit stops the fleet settling after this many attempts; 0 = no limit.
	limit int64
	wg    sync.WaitGroup
}

func newFleet(t *testing.T, pool *pgxpool.Pool, runID string, items int, report func(n int64, nodeID string)) *fleet {
	files := make([]any, items)
	for i := range files {
		files[i] = fmt.Sprintf("f%d", i)
	}
	enc := func(m map[string]json.RawMessage) []byte {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return &fleet{
		t: t, pool: pool, runID: runID, report: report,
		outputs: map[string][]byte{
			"list":      enc(map[string]json.RawMessage{"files": ref(t, files)}),
			"process":   enc(map[string]json.RawMessage{"result": ref(t, "ok")}),
			"summarize": enc(map[string]json.RawMessage{}),
		},
	}
}

func (f *fleet) run(ctx context.Context) {
	for ctx.Err() == nil {
		rows, err := f.pool.Query(ctx, `
			SELECT id::text, node_id FROM task_attempts
			 WHERE run_id = $1::uuid AND state = 'PENDING'`, f.runID)
		if err != nil {
			return
		}
		type att struct{ id, node string }
		var batch []att
		for rows.Next() {
			var a att
			if rows.Scan(&a.id, &a.node) == nil {
				batch = append(batch, a)
			}
		}
		rows.Close()
		for _, a := range batch {
			if f.limit > 0 && f.settled.Load() >= f.limit {
				break
			}
			prefix := a.node
			if i := strings.IndexByte(prefix, '['); i >= 0 {
				prefix = prefix[:i]
			}
			// The scheduler's terminal write. The 0039 trigger enqueues the
			// durable reconcile request in this same statement's transaction.
			tag, err := f.pool.Exec(ctx, `
				UPDATE task_attempts SET state = 'SUCCEEDED', output_ref = $2,
				       started_at = now(), ended_at = now()
				 WHERE id = $1::uuid AND state = 'PENDING'`, a.id, f.outputs[prefix])
			if err != nil || tag.RowsAffected() != 1 {
				continue
			}
			n := f.settled.Add(1)
			f.wg.Add(1)
			go func(node string) {
				defer f.wg.Done()
				f.report(n, node)
			}(a.node)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// gatewayReport is the gateway's post-commit call, with its per-call timeout.
func gatewayReport(srv *GRPCServer, runID string) func(int64, string) {
	return func(_ int64, node string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = srv.ReconcileRun(ctx, &ReconcileRunRequest{RunID: runID, NodeID: node})
	}
}

// lossy drops the call for every completion lost() selects -- the call
// "never reached the engine": a timeout, a network error, a restart.
func lossy(next func(int64, string), lost func(n int64) bool) func(int64, string) {
	return func(n int64, node string) {
		if lost(n) {
			return
		}
		next(n, node)
	}
}

func runState(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT state::text FROM workflow_runs WHERE id = $1::uuid`, runID).Scan(&s); err != nil {
		t.Fatalf("read run state: %v", err)
	}
	return s
}

// strandedNodes lists nodes that are not SUCCEEDED, with their state.
func strandedNodes(t *testing.T, pool *pgxpool.Pool, runID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT node_id || '=' || state::text FROM task_nodes
		 WHERE run_id = $1::uuid AND state <> 'SUCCEEDED' ORDER BY node_id LIMIT 20`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// awaitSucceeded waits for the run to be SUCCEEDED with every node SUCCEEDED
// and nothing left in the queue for it.
func awaitSucceeded(t *testing.T, pool *pgxpool.Pool, runID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if runState(t, pool, runID) == "SUCCEEDED" && len(strandedNodes(t, pool, runID)) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s is %s after %s, not SUCCEEDED; nodes not SUCCEEDED: %v",
		runID, runState(t, pool, runID), within, strandedNodes(t, pool, runID))
}

func queued(t *testing.T, pool *pgxpool.Pool, runID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM run_reconcile_requests WHERE run_id = $1::uuid`, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedFanout creates and materializes a list -> process[map] -> summarize run.
func seedFanout(t *testing.T, e *Engine, pool *pgxpool.Pool) string {
	t.Helper()
	runID := seedRun(t, pool, fanoutSpec(1000, nexusv1.TriggerRule_TRIGGER_RULE_ALL_SUCCESS))
	if _, err := e.Materialize(context.Background(), runID); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return runID
}

// The control: with the post-report RPC lost and nothing durable behind it,
// the fan-out strands exactly as it did on the dev cluster. This is what the
// tests below would see without the fix, and it proves the fault injector
// really takes the engine's only trigger away.
func TestIntegration_F160_LostReconcilesStrandARunWithoutTheQueue(t *testing.T) {
	pool := testPool(t)
	d := startDurableEngine(t, 4, &dbRunStates{pool: pool}, false)
	runID := seedFanout(t, d.engine, pool)

	// Only the first report (list's) reaches the engine: it expands the map.
	f := newFleet(t, pool, runID, 20, lossy(gatewayReport(d.srv, runID), func(n int64) bool { return n > 1 }))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.run(ctx)
	f.wg.Wait()

	if s := runState(t, pool, runID); s != "RUNNING" {
		t.Fatalf("control: run is %s; the injector did not strand it", s)
	}
	if got := strandedNodes(t, pool, runID); len(got) == 0 {
		t.Fatal("control: no stranded nodes")
	}
	if queued(t, pool, runID) == 0 {
		t.Fatal("control: the terminal writes left no durable reconcile request")
	}
}

// Two of every three post-report reconciles are lost, and the first terminal
// publish fails too. The run must still reach SUCCEEDED, promptly.
func TestIntegration_F160_LostReconcilesDoNotStrandAFanOut(t *testing.T) {
	pool := testPool(t)
	runs := &dbRunStates{pool: pool}
	runs.failNext.Store(1)
	d := startDurableEngine(t, 4, runs, true)
	runID := seedFanout(t, d.engine, pool)

	f := newFleet(t, pool, runID, 60, lossy(gatewayReport(d.srv, runID), func(n int64) bool { return n%3 != 0 }))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.run(ctx)

	awaitSucceeded(t, pool, runID, 30*time.Second)
	cancel()
	f.wg.Wait()
	if n := queued(t, pool, runID); n != 0 {
		// Every request is consumed once a reconcile covering it succeeded;
		// a leftover would be a leak (or a reconcile that never succeeded).
		t.Errorf("%d reconcile requests left for a finished run", n)
	}
}

// A dag-engine replica dies mid-run -- its consumer stops, its pool closes,
// and it dies HOLDING leases on queued requests. Every post-report RPC is
// lost. A fresh replica must pick the leases up once they expire and finish
// the run.
func TestIntegration_F160_EngineRestartMidRunDoesNotStrandIt(t *testing.T) {
	pool := testPool(t)
	runs := &dbRunStates{pool: pool}
	a := startDurableEngine(t, 4, runs, true)
	runID := seedFanout(t, a.engine, pool)

	f := newFleet(t, pool, runID, 40, func(int64, string) {}) // every RPC lost
	f.limit = 15                                              // leave work PENDING for after the crash
	ctx, cancel := context.WithCancel(context.Background())
	go f.run(ctx)

	// Let replica A make some progress, then stop the fleet and kill A.
	deadline := time.Now().Add(20 * time.Second)
	for f.settled.Load() < 15 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	f.wg.Wait()
	if f.settled.Load() < 15 {
		t.Fatalf("replica A made no progress: %d attempts settled", f.settled.Load())
	}
	a.shutdown()

	// One more attempt finishes while no engine is up: its node now reads
	// RUNNING over a SUCCEEDED attempt, and only its queued request says so.
	var nodeID string
	if err := pool.QueryRow(context.Background(), `
		UPDATE task_attempts SET state = 'SUCCEEDED', output_ref = $2, started_at = now(), ended_at = now()
		 WHERE id = (SELECT id FROM task_attempts WHERE run_id = $1::uuid AND state = 'PENDING' LIMIT 1)
		RETURNING node_id`, runID, f.outputs["process"]).Scan(&nodeID); err != nil {
		t.Fatalf("settle an attempt while the engine is down: %v", err)
	}
	// The dying replica had claimed everything queued, and will never
	// acknowledge or release it.
	crashed := store.NewDAGEngineStore(pool)
	claims, err := crashed.ClaimReconcileRequests(context.Background(), "crashed-replica", 2*time.Second, 10000)
	if err != nil {
		t.Fatalf("claim as the crashed replica: %v", err)
	}
	held := 0
	for _, c := range claims {
		if c.RunID == runID {
			held += len(c.IDs)
		}
	}
	if held == 0 {
		t.Fatal("the crashed replica holds no request for the run; the test proves nothing")
	}

	// Replica B: it must wait out the dead lease, then reconcile the run.
	b := startDurableEngine(t, 4, runs, true)
	waitFor(t, 20*time.Second, "the dead replica's leased requests to be consumed", func() bool {
		return queued(t, pool, runID) == 0 && nodeState(t, pool, runID, nodeID) == "SUCCEEDED"
	})

	// And the rest of the run completes on B.
	f2 := newFleet(t, pool, runID, 40, gatewayReport(b.srv, runID))
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go f2.run(ctx2)
	awaitSucceeded(t, pool, runID, 30*time.Second)
	cancel2()
	f2.wg.Wait()
}

func nodeState(t *testing.T, pool *pgxpool.Pool, runID, nodeID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT state::text FROM task_nodes WHERE run_id = $1::uuid AND node_id = $2`, runID, nodeID).Scan(&s); err != nil {
		t.Fatalf("read node %s: %v", nodeID, err)
	}
	return s
}

func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// 500 completions of one run, all reported at once through the RPC with the
// gateway's timeout, against an engine with a 3-connection pool: the dev
// cluster's exact shape. Nothing may be stranded, and the reconciles must be
// coalesced -- a pass per completion is what flooded the pool.
func TestIntegration_F160_BurstOfCompletionsOnASmallPool(t *testing.T) {
	pool := testPool(t)
	d := startDurableEngine(t, 3, &dbRunStates{pool: pool}, true)
	runID := seedFanout(t, d.engine, pool)

	f := newFleet(t, pool, runID, 500, gatewayReport(d.srv, runID))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := d.engine.ReconcilePasses()
	go f.run(ctx)

	awaitSucceeded(t, pool, runID, 90*time.Second)
	cancel()
	f.wg.Wait()
	if n := f.settled.Load(); n != 502 {
		t.Errorf("settled %d attempts, want 502 (list, 500 children, summarize)", n)
	}
	passes := d.engine.ReconcilePasses() - before
	t.Logf("502 completions reconciled in %d coalesced passes", passes)
	if passes > 100 {
		t.Errorf("%d reconcile passes for 502 completions of one run; they are not being coalesced", passes)
	}
	if n := queued(t, pool, runID); n != 0 {
		t.Errorf("%d reconcile requests left for a finished run", n)
	}
}

// A run stranded BEFORE the upgrade has no queued request -- the facts that
// should have produced one are in the past. Build exactly the dev cluster's
// stranded shape (children SUCCEEDED but reading RUNNING, the template
// RUNNING, the fan-in PENDING with no attempt), with no request queued, then
// start the fixed services: the workflow-manager's stalled-run sweep must find
// it and the engine must finish it.
func TestIntegration_F160_RunStrandedBeforeTheUpgradeIsRepaired(t *testing.T) {
	pool := testPool(t)
	runs := &dbRunStates{pool: pool}

	// Strand it: every report lost, no consumer.
	old := startDurableEngine(t, 4, runs, false)
	runID := seedFanout(t, old.engine, pool)
	f := newFleet(t, pool, runID, 30, lossy(gatewayReport(old.srv, runID), func(n int64) bool { return n > 1 }))
	fctx, fcancel := context.WithTimeout(context.Background(), 2*time.Second)
	f.run(fctx)
	fcancel()
	f.wg.Wait()
	old.shutdown()
	// Pre-upgrade: no queue existed, so nothing is queued; and it has been
	// stuck a while.
	mustExecT(t, pool, `DELETE FROM run_reconcile_requests WHERE run_id = $1::uuid`, runID)
	mustExecT(t, pool, `UPDATE task_attempts SET ended_at = now() - interval '1 hour' WHERE run_id = $1::uuid`, runID)
	if a, b := nodeState(t, pool, runID, "summarize"), nodeState(t, pool, runID, "process"); a != "PENDING" || b != "RUNNING" {
		t.Fatalf("not the stranded shape: fan-in %s, template %s", a, b)
	}
	var unsettled int
	_ = pool.QueryRow(context.Background(), `
		SELECT count(*) FROM task_nodes n
		 WHERE n.run_id = $1::uuid AND n.node_id LIKE 'process[%' AND n.state <> 'SUCCEEDED'
		   AND EXISTS (SELECT 1 FROM task_attempts a WHERE a.run_id = n.run_id AND a.node_id = n.node_id AND a.state = 'SUCCEEDED')`,
		runID).Scan(&unsettled)
	if unsettled == 0 {
		t.Fatal("not the stranded shape: no child reads unfinished over a SUCCEEDED attempt")
	}

	// The fixed services.
	d := startDurableEngine(t, 4, runs, true)
	cfg := timers.DefaultConfig("f160-test")
	cfg.StalledRunAge = time.Second
	cfg.StalledRunInterval = 100 * time.Millisecond
	wfm := timers.New(pool, cfg, testLogger())
	var swept atomic.Int32
	wfm.OnStalledRun(func(ctx context.Context, id string) error {
		if id == runID {
			swept.Add(1)
		}
		_, err := d.srv.ReconcileRun(ctx, &ReconcileRunRequest{RunID: id})
		return err
	})
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	go func() { _ = wfm.Run(wctx) }()

	// Only the fan-in's own attempt is left to execute once it is repaired.
	fleet2 := newFleet(t, pool, runID, 30, gatewayReport(d.srv, runID))
	go fleet2.run(wctx)
	awaitSucceeded(t, pool, runID, 30*time.Second)
	wcancel()
	fleet2.wg.Wait()
	if swept.Load() == 0 {
		t.Error("the run finished without the stalled-run sweep finding it; the test did not exercise the repair")
	}
}

func mustExecT(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
