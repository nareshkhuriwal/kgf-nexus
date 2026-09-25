//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Races between a worker's terminal report and the other writers of the
// attempt row (CONTRACT-FINDINGS F104), against real Postgres.
//
// The failure these pin: ApplyResult used to lock the attempt with SKIP
// LOCKED and read "no row" as "already handled". Any transaction holding the
// row -- most often the worker's own ReportStarted -- made a result that
// arrived in the same instant be skipped, acknowledged to the agent as
// accepted, and then reclaimed as LOST at lease expiry.

// dispatchedAttempt is a real dispatch: DISPATCHED, leased, reserved, timed.
type dispatchedAttempt struct {
	f      *fixture
	st     *store.SchedulerStore
	srv    *ReportServer
	id     uuid.UUID
	worker uuid.UUID
	token  string
}

func newDispatchedAttempt(t *testing.T, f *fixture, st *store.SchedulerStore, worker uuid.UUID, nodeID string) *dispatchedAttempt {
	t.Helper()
	spec := nodeSpec{
		TaskRef: "test.task", Version: 1, MaxAttempts: 3, Idempotency: "required",
		ScheduleToStartMs: 3_600_000, StartToCloseMs: 3_600_000,
	}
	id := f.addAttempt(nodeID, 100, vec{CPUMillis: 100, MemoryBytes: 1 << 20}, spec, time.Now().UTC())
	rep := dispatchOne(t, st)
	if rep.Dispatch.AttemptID != id {
		t.Fatalf("dispatched %s, want %s", rep.Dispatch.AttemptID, id)
	}
	token, _, ok := f.leaseFor(id)
	if !ok {
		t.Fatalf("attempt %s has no lease after dispatch", id)
	}
	return &dispatchedAttempt{
		f: f, st: st, id: id, worker: worker, token: token,
		srv: NewReportServer(st, slog.New(slog.NewTextHandler(io.Discard, nil))),
	}
}

// apply sends a SUCCEEDED report through the scheduler's RPC handler, the
// path the gateway calls, so the error-to-status mapping is covered too.
func (d *dispatchedAttempt) apply(ctx context.Context) (*ApplyResultResponse, error) {
	raw, err := protojson.Marshal(&nexusv1.ReportResultRequest{
		AttemptId: d.id.String(), LeaseToken: d.token,
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	})
	if err != nil {
		return nil, err
	}
	return d.srv.ApplyResult(ctx, &ApplyResultRequest{ResultJSON: raw})
}

// holdRun takes the run row lock in its own transaction and returns the
// release. Every attempt transition appends a run event, and appendEvent
// allocates its seq with an UPDATE of that row, so a MarkStarted or a lease
// reclaim started while this is held stalls AFTER it has locked the attempt:
// exactly the open-transaction window the race needs, produced by the real
// store code rather than a hand-written imitation of it.
func holdRun(t *testing.T, f *fixture) (release func()) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin run holder: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM workflow_runs WHERE id = $1 FOR UPDATE`, f.runID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("lock run: %v", err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = tx.Commit(ctx) }) }
	t.Cleanup(release)
	return release
}

// waitAttemptLocked blocks until some transaction holds the attempt's row
// lock, probing with NOWAIT so the probe never queues behind it.
func waitAttemptLocked(t *testing.T, f *fixture, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin probe: %v", err)
		}
		_, err = tx.Exec(ctx, `SELECT 1 FROM task_attempts WHERE id = $1 FOR UPDATE NOWAIT`, id)
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return
		}
		if err != nil {
			t.Fatalf("probe attempt lock: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("attempt %s was never locked", id)
}

// TestResultIsNotSkippedWhileMarkStartedHoldsTheAttempt is F104's race: the
// worker's ReportStarted transaction still holds the attempt when its result
// arrives. The result must be applied once that transaction commits, not
// skipped and acknowledged.
func TestResultIsNotSkippedWhileMarkStartedHoldsTheAttempt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	worker := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(nil)
	d := newDispatchedAttempt(t, f, st, worker, "short")

	release := holdRun(t, f)
	started := make(chan error, 1)
	go func() { started <- st.MarkStarted(ctx, d.id, time.Now().UTC()) }()
	waitAttemptLocked(t, f, d.id)

	type result struct {
		resp *ApplyResultResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := d.apply(ctx)
		done <- result{resp, err}
	}()

	// Let the result reach the lock before MarkStarted commits. Before the fix
	// it did not wait: it returned Applied=false here, immediately.
	time.Sleep(150 * time.Millisecond)
	release()

	if err := <-started; err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("ApplyResult: %v", r.err)
	}
	if !r.resp.Applied {
		t.Fatal("the result was skipped while MarkStarted held the attempt; the agent was told it was accepted and the attempt would expire as LOST")
	}
	if s := f.attemptState(d.id); s != "SUCCEEDED" {
		t.Fatalf("attempt state = %s, want SUCCEEDED", s)
	}
	if _, _, ok := f.leaseFor(d.id); ok {
		t.Error("the lease survived the terminal transition")
	}
	if got := f.workerCommitted(worker).CPUMillis; got != 0 {
		t.Errorf("committed cpu = %d, want 0 after the result", got)
	}
	kinds := f.eventKinds(d.id)
	if !contains(kinds, "AttemptStarted") || !contains(kinds, "AttemptSucceeded") {
		t.Errorf("events = %v, want AttemptStarted then AttemptSucceeded", kinds)
	}
}

// TestResultWaitingOnAReclaimStaysFenced is the other side of waiting instead
// of skipping: when the transaction holding the row is the lease reconciler
// moving the attempt to LOST, the waiting result must see LOST and do nothing.
// Nothing may be applied twice or released twice.
func TestResultWaitingOnAReclaimStaysFenced(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	worker := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(nil)
	d := newDispatchedAttempt(t, f, st, worker, "reclaimed")
	f.expireLease(d.id)

	release := holdRun(t, f)
	reclaimed := make(chan []store.LostAttempt, 1)
	go func() {
		lost, err := st.ReconcileExpiredLeases(ctx, time.Now().UTC(), 10)
		if err != nil {
			t.Errorf("reconcile: %v", err)
		}
		reclaimed <- lost
	}()
	waitAttemptLocked(t, f, d.id)

	type result struct {
		resp *ApplyResultResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := d.apply(ctx)
		done <- result{resp, err}
	}()
	time.Sleep(150 * time.Millisecond)
	release()

	if lost := <-reclaimed; len(lost) != 1 {
		t.Fatalf("reclaimed %d attempts, want 1", len(lost))
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("ApplyResult: %v", r.err)
	}
	if r.resp.Applied {
		t.Fatal("a result was applied over an attempt the reconciler had already moved to LOST")
	}
	if s := f.attemptState(d.id); s != "LOST" {
		t.Fatalf("attempt state = %s, want LOST", s)
	}
	if got := f.workerCommitted(worker).CPUMillis; got != 0 {
		t.Errorf("committed cpu = %d, want exactly 0 (released once)", got)
	}
	var terminal int
	for _, k := range f.eventKinds(d.id) {
		if k == "AttemptSucceeded" || k == domain.EventAttemptLost {
			terminal++
		}
	}
	if terminal != 1 {
		t.Errorf("terminal events = %d (%v), want exactly 1", terminal, f.eventKinds(d.id))
	}
}

// TestResultBusyPastTheLockTimeoutIsRetryable bounds the wait. A holder that
// outlasts it must produce a retryable error -- the agent keeps the result in
// its WAL and resends -- never Applied=false, which the gateway would
// acknowledge as delivered.
func TestResultBusyPastTheLockTimeoutIsRetryable(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	worker := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(func(c *store.ClaimConfig) { c.AttemptLockTimeout = 100 * time.Millisecond })
	d := newDispatchedAttempt(t, f, st, worker, "busy")

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM task_attempts WHERE id = $1 FOR UPDATE`, d.id); err != nil {
		t.Fatalf("hold attempt: %v", err)
	}

	start := time.Now()
	_, err = d.apply(ctx)
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("ApplyResult under a held lock = %v (code %s), want Unavailable so the agent retries", err, code)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("waited %s; the lock timeout did not bound the wait", waited)
	}
	// The store surfaces the sentinel too, for callers that are not the RPC.
	_, serr := st.ApplyResult(ctx, time.Now().UTC(), &nexusv1.ReportResultRequest{
		AttemptId: d.id.String(), LeaseToken: d.token,
		Outcome: nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED,
	})
	if !errors.Is(serr, store.ErrAttemptBusy) {
		t.Fatalf("store error = %v, want ErrAttemptBusy", serr)
	}
	if s := f.attemptState(d.id); s != "DISPATCHED" {
		t.Fatalf("attempt state = %s, want untouched DISPATCHED", s)
	}

	// The agent's resend, once the holder is gone, lands.
	if err := holder.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("release holder: %v", err)
	}
	resp, err := d.apply(ctx)
	if err != nil || !resp.Applied {
		t.Fatalf("resend = %+v, %v; want applied", resp, err)
	}
	if s := f.attemptState(d.id); s != "SUCCEEDED" {
		t.Fatalf("attempt state = %s, want SUCCEEDED", s)
	}
}

// TestCancelIsNotSkippedWhileMarkStartedHoldsTheAttempt is the same race on
// the cancel fan-out: a run cancel arriving while the worker's ReportStarted
// is committing used to skip that attempt, leaving it RUNNING under a
// cancelled run.
func TestCancelIsNotSkippedWhileMarkStartedHoldsTheAttempt(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	worker := f.addWorker(vec{CPUMillis: 4000, MemoryBytes: 8 << 30}, nil, nil)
	st := f.newStore(nil)
	d := newDispatchedAttempt(t, f, st, worker, "cancelled")

	release := holdRun(t, f)
	started := make(chan error, 1)
	go func() { started <- st.MarkStarted(ctx, d.id, time.Now().UTC()) }()
	waitAttemptLocked(t, f, d.id)

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := st.CancelRunAttempts(ctx, f.runID, time.Now().UTC())
		done <- result{n, err}
	}()
	time.Sleep(150 * time.Millisecond)
	release()

	if err := <-started; err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("CancelRunAttempts: %v", r.err)
	}
	if r.n != 1 {
		t.Fatalf("cancelled %d attempts, want 1; the attempt being started was skipped", r.n)
	}
	if s := f.attemptState(d.id); s != "CANCELLED" {
		t.Fatalf("attempt state = %s, want CANCELLED", s)
	}
	if got := f.workerCommitted(worker).CPUMillis; got != 0 {
		t.Errorf("committed cpu = %d, want 0 after cancel", got)
	}
}

// TestConcurrentStartAndResultNeverDropsTheResult races the real ReportStarted
// and ReportResult of many short tasks with no staging at all: every result
// must land. It is the production shape of F104 -- a task that finishes before
// its own start report has committed.
func TestConcurrentStartAndResultNeverDropsTheResult(t *testing.T) {
	t.Parallel()
	pool := newTestPool(t)
	f := newFixture(t, pool)
	ctx := context.Background()

	const n = 24
	worker := f.addWorker(vec{CPUMillis: 64000, MemoryBytes: 64 << 30}, nil, nil)
	st := f.newStore(nil)
	attempts := make([]*dispatchedAttempt, n)
	for i := range attempts {
		attempts[i] = newDispatchedAttempt(t, f, st, worker, nodeName("race", i))
	}

	var wg sync.WaitGroup
	gate := make(chan struct{})
	errs := make(chan error, 2*n)
	dropped := make(chan uuid.UUID, n)
	for _, d := range attempts {
		wg.Add(2)
		go func(d *dispatchedAttempt) {
			defer wg.Done()
			<-gate
			if err := st.MarkStarted(ctx, d.id, time.Now().UTC()); err != nil {
				errs <- err
			}
		}(d)
		go func(d *dispatchedAttempt) {
			defer wg.Done()
			<-gate
			resp, err := d.apply(ctx)
			if err != nil {
				errs <- err
				return
			}
			if !resp.Applied {
				dropped <- d.id
			}
		}(d)
	}
	close(gate)
	wg.Wait()
	close(errs)
	close(dropped)
	for err := range errs {
		t.Errorf("report failed: %v", err)
	}
	for id := range dropped {
		t.Errorf("result for %s was not applied", id)
	}
	for _, d := range attempts {
		if s := f.attemptState(d.id); s != "SUCCEEDED" {
			t.Errorf("attempt %s state = %s, want SUCCEEDED", d.id, s)
		}
	}
	if got := f.workerCommitted(worker).CPUMillis; got != 0 {
		t.Errorf("committed cpu = %d, want 0 after every result", got)
	}
}
