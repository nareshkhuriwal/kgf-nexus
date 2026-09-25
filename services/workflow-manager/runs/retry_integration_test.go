//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package runs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// The workflow-manager's half of an operator retry (CONTRACT-FINDINGS F100),
// against real Postgres: the run-row transaction, the deadline it renews, and
// the races it must survive -- a second retry, a stale terminal report and a
// cancel, each arriving while the first retry is mid-flight.
//
// The dag-engine is faked. Its half is tested against the same database in
// services/dag-engine; what matters here is WHEN it is called relative to this
// service's lock and commit, which a fake can control precisely.

func retryPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.DSN(t))
	if err != nil {
		testdb.SkipUnreachable(t, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		testdb.SkipUnreachable(t, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// failedRunFixture seeds a FAILED run (a SUCCEEDED, b FAILED, c UPSTREAM_FAILED)
// whose deadline has passed, as a TIMED_OUT-or-FAILED run's would have.
func failedRunFixture(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	ns := fmt.Sprintf("retry-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)

	var specID, runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'retry-fixture', 1, 'h-'||gen_random_uuid()::text, '{"run_timeout_ms": 3600000}'::jsonb, 'test')
		RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at, ended_at, deadline_at)
		VALUES ($1, $2, 'FAILED', 'manual', now() - interval '2 hours', now() - interval '1 hour',
		        now() - interval '1 hour')
		RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for n, st := range map[string]string{"a": "SUCCEEDED", "b": "FAILED", "c": "UPSTREAM_FAILED"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json)
			VALUES ($1, $2, $2, $3::node_state, '{}'::jsonb)`, runID, n, st); err != nil {
			t.Fatalf("seed node %s: %v", n, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM timers WHERE subject_id = $1::uuid`, runID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_runs WHERE id = $1::uuid`, runID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM workflow_specs WHERE id = $1::uuid`, specID)
	})
	return runID
}

// fakeEngine plays the dag-engine. reset decides what ResetNodes does; by
// default it performs the reset the real engine would for selecting b.
type fakeEngine struct {
	pool       *pgxpool.Pool
	reset      func(ctx context.Context, runID string, nodes []string) ([]string, []string, error)
	resets     atomic.Int32
	reconciles atomic.Int32
	// reconcileSaw records the run state visible to the reconcile, which must
	// be the COMMITTED retry: a reconcile that saw FAILED would create
	// attempts no scheduler will dispatch.
	mu           sync.Mutex
	reconcileSaw []string
}

func (f *fakeEngine) MaterializeRun(context.Context, string, string, []byte) error { return nil }

func (f *fakeEngine) ResetNodes(ctx context.Context, runID string, nodes []string, actor string) ([]string, []string, error) {
	f.resets.Add(1)
	if f.reset != nil {
		return f.reset(ctx, runID, nodes)
	}
	return f.resetB(ctx, runID)
}

func (f *fakeEngine) resetB(ctx context.Context, runID string) ([]string, []string, error) {
	if _, err := f.pool.Exec(ctx, `
		UPDATE task_nodes SET state = 'PENDING', retry_floor = 1, attempts_used = 0
		 WHERE run_id = $1::uuid AND node_id IN ('b','c')`, runID); err != nil {
		return nil, nil, err
	}
	return []string{"b"}, []string{"b", "c"}, nil
}

func (f *fakeEngine) ReconcileRun(ctx context.Context, runID, _ string) ([]string, string, bool, error) {
	f.reconciles.Add(1)
	var st string
	if err := f.pool.QueryRow(ctx, `SELECT state::text FROM workflow_runs WHERE id = $1::uuid`, runID).Scan(&st); err != nil {
		return nil, "", false, err
	}
	f.mu.Lock()
	f.reconcileSaw = append(f.reconcileSaw, st)
	f.mu.Unlock()
	return nil, st, false, nil
}

type recordedEvent struct {
	kind    string
	payload map[string]any
	actor   string
}

type fakeEvents struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (f *fakeEvents) Publish(_ context.Context, _ string, _, _ *string, kind string, payload any, actor string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, _ := payload.(map[string]any)
	f.events = append(f.events, recordedEvent{kind: kind, payload: p, actor: actor})
	return nil
}

func (f *fakeEvents) find(kind string) *recordedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.events {
		if f.events[i].kind == kind {
			return &f.events[i]
		}
	}
	return nil
}

type noopScheduler struct{}

func (noopScheduler) CancelRunAttempts(context.Context, string) error { return nil }

func newRetryService(pool *pgxpool.Pool, eng *fakeEngine, ev *fakeEvents) *Service {
	return New(pool, ev, eng, noopScheduler{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func runState(t *testing.T, pool *pgxpool.Pool, runID string) (state string, ended *time.Time, deadline *time.Time) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT state::text, ended_at, deadline_at FROM workflow_runs WHERE id = $1::uuid`, runID).
		Scan(&state, &ended, &deadline); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return state, ended, deadline
}

func TestIntegrationRetryCommitsRunningWithAFreshDeadline(t *testing.T) {
	pool := retryPool(t)
	runID := failedRunFixture(t, pool)
	eng := &fakeEngine{pool: pool}
	ev := &fakeEvents{}
	svc := newRetryService(pool, eng, ev)

	res, err := svc.Retry(context.Background(), RetryRequest{RunID: runID, NodeIDs: []string{"b"}, Actor: "user:alice"})
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if res.Run.State != "RUNNING" || fmt.Sprint(res.Reset) != "[b c]" {
		t.Fatalf("result = %+v", res)
	}

	state, ended, deadline := runState(t, pool, runID)
	if state != "RUNNING" {
		t.Fatalf("run state = %s, want RUNNING", state)
	}
	if ended != nil {
		t.Errorf("ended_at = %v on a running run; a retry must clear it", ended)
	}
	// The old deadline had passed; keeping it would time the retry out at once.
	if deadline == nil || time.Until(*deadline) < 50*time.Minute {
		t.Errorf("deadline_at = %v, want renewed about an hour ahead", deadline)
	}
	var timers int
	var fireAt time.Time
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*), max(fire_at) FROM timers WHERE subject_id = $1::uuid AND kind = 'run_deadline'`,
		runID).Scan(&timers, &fireAt); err != nil {
		t.Fatalf("read timers: %v", err)
	}
	if timers != 1 || !fireAt.Equal(*deadline) {
		t.Errorf("run_deadline timers = %d firing at %v, want exactly one at the new deadline %v", timers, fireAt, deadline)
	}

	// The reconcile ran once, AFTER commit: it saw the run RUNNING.
	if eng.reconciles.Load() != 1 || fmt.Sprint(eng.reconcileSaw) != "[RUNNING]" {
		t.Errorf("reconciles = %d seeing %v, want one seeing [RUNNING]", eng.reconciles.Load(), eng.reconcileSaw)
	}

	// The audit trail: who asked, what they asked for, what it reset.
	e := ev.find("RunRetriedFromFailed")
	if e == nil {
		t.Fatal("no RunRetriedFromFailed event")
	}
	if e.actor != "user:alice" {
		t.Errorf("retry event actor = %q, want user:alice", e.actor)
	}
	if fmt.Sprint(e.payload["requested_nodes"]) != "[b]" || fmt.Sprint(e.payload["reset_nodes"]) != "[b c]" ||
		e.payload["mode"] != "nodes" {
		t.Errorf("retry event payload = %v", e.payload)
	}
}

func TestIntegrationRetryRefusedByTheEngineChangesNothing(t *testing.T) {
	for _, refusal := range []error{ErrRetryRefused, ErrRetryInvalidNodes, ErrRetryConflict, ErrRetryUnavailable} {
		t.Run(refusal.Error(), func(t *testing.T) {
			pool := retryPool(t)
			runID := failedRunFixture(t, pool)
			eng := &fakeEngine{pool: pool, reset: func(context.Context, string, []string) ([]string, []string, error) {
				return nil, nil, fmt.Errorf("%w: node x", refusal)
			}}
			ev := &fakeEvents{}
			svc := newRetryService(pool, eng, ev)

			_, err := svc.Retry(context.Background(), RetryRequest{RunID: runID, NodeIDs: []string{"x"}})
			if !errors.Is(err, refusal) {
				t.Fatalf("Retry err = %v, want %v", err, refusal)
			}
			state, _, deadline := runState(t, pool, runID)
			if state != "FAILED" || deadline == nil || time.Until(*deadline) > 0 {
				t.Errorf("a refused retry left the run %s with deadline %v; want it untouched", state, deadline)
			}
			if eng.reconciles.Load() != 0 || len(ev.events) != 0 {
				t.Errorf("a refused retry reconciled (%d) or emitted %v", eng.reconciles.Load(), ev.events)
			}
		})
	}
}

// The engine call fails without saying whether its reset committed. The run is
// committed RUNNING and reconciled so it converges either way, and the caller
// is told the outcome is unknown rather than that it failed.
func TestIntegrationRetryWithAnIndeterminateEngineCallConverges(t *testing.T) {
	pool := retryPool(t)
	runID := failedRunFixture(t, pool)
	eng := &fakeEngine{pool: pool}
	eng.reset = func(ctx context.Context, runID string, _ []string) ([]string, []string, error) {
		// The reset commits, then the answer is lost.
		if _, _, err := eng.resetB(ctx, runID); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("rpc error: code = DeadlineExceeded")
	}
	svc := newRetryService(pool, eng, &fakeEvents{})

	_, err := svc.Retry(context.Background(), RetryRequest{RunID: runID, NodeIDs: []string{"b"}})
	if !errors.Is(err, ErrRetryIndeterminate) {
		t.Fatalf("Retry err = %v, want ErrRetryIndeterminate", err)
	}
	if state, _, _ := runState(t, pool, runID); state != "RUNNING" {
		t.Fatalf("run = %s; the reset committed, so the run must be RUNNING or its nodes never dispatch", state)
	}
	if eng.reconciles.Load() != 1 {
		t.Errorf("reconciles = %d, want 1: only the reconcile makes an unknown outcome converge", eng.reconciles.Load())
	}
}

func TestIntegrationRetryOfARunThatDidNotFailIsAConflict(t *testing.T) {
	pool := retryPool(t)
	runID := failedRunFixture(t, pool)
	if _, err := pool.Exec(context.Background(),
		`UPDATE workflow_runs SET state = 'SUCCEEDED' WHERE id = $1::uuid`, runID); err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{pool: pool}
	_, err := newRetryService(pool, eng, &fakeEvents{}).Retry(context.Background(), RetryRequest{RunID: runID})
	if !errors.Is(err, store.ErrRunStateConflict) {
		t.Fatalf("Retry err = %v, want ErrRunStateConflict", err)
	}
	if eng.resets.Load() != 0 {
		t.Error("the engine was asked to reset nodes of a SUCCEEDED run")
	}
}

// blockingEngine holds the first reset open until released, so a test can
// fire a competing operation while the retry holds the run row.
func blockingEngine(pool *pgxpool.Pool) (*fakeEngine, chan struct{}, chan struct{}) {
	entered, release := make(chan struct{}), make(chan struct{})
	eng := &fakeEngine{pool: pool}
	var once sync.Once
	eng.reset = func(ctx context.Context, runID string, _ []string) ([]string, []string, error) {
		first := false
		once.Do(func() { first = true })
		if first {
			close(entered)
			<-release
		}
		return eng.resetB(ctx, runID)
	}
	return eng, entered, release
}

// assertBlocked checks that an operation started while the retry holds the run
// row has NOT completed, i.e. that it is waiting on the lock.
func assertBlocked(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s completed (%v) while the retry held the run; it must wait for the retry to commit", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestIntegrationConcurrentRetriesApplyOnce(t *testing.T) {
	pool := retryPool(t)
	runID := failedRunFixture(t, pool)
	eng, entered, release := blockingEngine(pool)
	svc := newRetryService(pool, eng, &fakeEvents{})
	ctx := context.Background()

	first := make(chan error, 1)
	go func() { _, err := svc.Retry(ctx, RetryRequest{RunID: runID, NodeIDs: []string{"b"}}); first <- err }()
	<-entered

	second := make(chan error, 1)
	go func() { _, err := svc.Retry(ctx, RetryRequest{RunID: runID, NodeIDs: []string{"b"}}); second <- err }()
	assertBlocked(t, second, "a second retry")

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first retry: %v", err)
	}
	if err := <-second; !errors.Is(err, store.ErrRunStateConflict) {
		t.Fatalf("second retry: %v, want ErrRunStateConflict once the first has made the run RUNNING", err)
	}
	if n := eng.resets.Load(); n != 1 {
		t.Errorf("engine asked to reset %d times, want 1", n)
	}
}

// A reconcile that concluded FAILED just before the reset reports it while the
// retry is mid-flight. Applied, it would put the run back to FAILED over a graph
// that is running again, and nothing would ever dispatch the retried nodes.
func TestIntegrationStaleTerminalReportCannotUndoARetry(t *testing.T) {
	pool := retryPool(t)
	runID := failedRunFixture(t, pool)
	eng, entered, release := blockingEngine(pool)
	svc := newRetryService(pool, eng, &fakeEvents{})
	ctx := context.Background()

	retried := make(chan error, 1)
	go func() { _, err := svc.Retry(ctx, RetryRequest{RunID: runID, NodeIDs: []string{"b"}}); retried <- err }()
	<-entered

	stale := make(chan error, 1)
	go func() { _, err := svc.OnNodesTerminal(ctx, runID); stale <- err }()
	assertBlocked(t, stale, "a stale terminal report")

	close(release)
	if err := <-retried; err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := <-stale; !errors.Is(err, store.ErrRunStateConflict) {
		t.Fatalf("stale terminal report: %v, want it refused", err)
	}
	if state, _, _ := runState(t, pool, runID); state != "RUNNING" {
		t.Fatalf("run = %s after a stale terminal report; the retry was undone", state)
	}
}

// A cancel issued while a retry is mid-flight is serialized AFTER it: it waits,
// then cancels the retried run. It is never lost, and never interleaved.
func TestIntegrationCancelRacingARetryIsAppliedAfterIt(t *testing.T) {
	pool := retryPool(t)
	runID := failedRunFixture(t, pool)
	eng, entered, release := blockingEngine(pool)
	svc := newRetryService(pool, eng, &fakeEvents{})
	ctx := context.Background()

	retried := make(chan error, 1)
	go func() { _, err := svc.Retry(ctx, RetryRequest{RunID: runID, NodeIDs: []string{"b"}}); retried <- err }()
	<-entered

	cancelled := make(chan error, 1)
	go func() { _, err := svc.Cancel(ctx, runID); cancelled <- err }()
	assertBlocked(t, cancelled, "a cancel")

	close(release)
	if err := <-retried; err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := <-cancelled; err != nil {
		t.Fatalf("cancel after retry: %v", err)
	}
	if state, _, _ := runState(t, pool, runID); state != "CANCELLED" && state != "CANCELLING" {
		t.Fatalf("run = %s; the cancel issued during the retry was lost", state)
	}
}
