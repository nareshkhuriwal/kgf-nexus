//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package runs

// F109's open half, against real Postgres and the scheduler's real
// CancelRunAttempts: a run whose attempt-cancel fan-out failed stays
// CANCELLING, and ReconcileCancelling re-issues the fan-out until every
// attempt is terminal, so the attempts end CANCELLED (not FAILED through a
// later result) and the run reaches CANCELLED. The re-issue is idempotent and
// safe to race from several workflow-manager replicas.

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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/internal/testdb"
)

// flakyScheduler is the scheduler's real CancelRunAttempts behind a switch
// that fails the first `failures` calls, as an unreachable or busy scheduler
// would (codes.Unavailable).
type flakyScheduler struct {
	st       *store.SchedulerStore
	failures atomic.Int32
}

func (f *flakyScheduler) CancelRunAttempts(ctx context.Context, runID string) error {
	if f.failures.Add(-1) >= 0 {
		return errors.New("scheduler unavailable")
	}
	id, err := uuid.Parse(runID)
	if err != nil {
		return err
	}
	_, err = f.st.CancelRunAttempts(ctx, id, time.Now().UTC())
	return err
}

func newCancelService(pool *pgxpool.Pool, failures int32) *Service {
	sched := &flakyScheduler{st: store.NewSchedulerStore(pool, store.ClaimConfig{}, "test")}
	sched.failures.Store(failures)
	return New(pool, &fakeEvents{}, &fakeEngine{pool: pool}, sched,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// cancellableRun seeds a RUNNING run with n nodes, each with one RUNNING
// attempt.
func cancellableRun(t *testing.T, pool *pgxpool.Pool, n int) string {
	t.Helper()
	ctx := context.Background()
	ns := fmt.Sprintf("cancel-%d", time.Now().UnixNano())
	testdb.RegisterTenant(t, pool, ns)
	var specID, runID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'cancel-fixture', 1, 'h-'||gen_random_uuid()::text, '{}'::jsonb, 'test')
		RETURNING id::text`, ns).Scan(&specID); err != nil {
		t.Fatalf("seed spec: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, state, trigger_kind, started_at)
		VALUES ($1, $2, 'RUNNING', 'manual', now()) RETURNING id::text`, specID, ns).Scan(&runID); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for i := 0; i < n; i++ {
		node := fmt.Sprintf("n%d", i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_nodes (run_id, node_id, task_ref, state, spec_json)
			VALUES ($1, $2, $2, 'RUNNING', '{}'::jsonb)`, runID, node); err != nil {
			t.Fatalf("seed node: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO task_attempts (run_id, node_id, attempt_number, state, resource_request, idempotency_key, started_at)
			VALUES ($1::uuid, $2, 1, 'RUNNING', '{}'::jsonb, $1::text || '-' || $2, now())`, runID, node); err != nil {
			t.Fatalf("seed attempt: %v", err)
		}
	}
	t.Cleanup(func() {
		c := context.Background()
		_, _ = pool.Exec(c, `DELETE FROM timers WHERE subject_id = $1::uuid`, runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_runs WHERE id = $1::uuid`, runID)
		_, _ = pool.Exec(c, `DELETE FROM workflow_specs WHERE id = $1::uuid`, specID)
	})
	return runID
}

func attemptStates(t *testing.T, pool *pgxpool.Pool, runID string) map[string]int {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT state::text, count(*)::int FROM task_attempts WHERE run_id = $1::uuid GROUP BY 1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func cancelledEvents(t *testing.T, pool *pgxpool.Pool, runID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM run_events WHERE run_id = $1::uuid AND kind = 'AttemptCancelled'`,
		runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func listed(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// TestCancellingRunIsReissuedUntilEveryAttemptIsCancelled: the first fan-out
// fails, leaving the run CANCELLING with live attempts -- before the fix
// nothing ever tried again. One reconcile finishes the job; further ones are
// no-ops.
func TestCancellingRunIsReissuedUntilEveryAttemptIsCancelled(t *testing.T) {
	pool := retryPool(t)
	runID := cancellableRun(t, pool, 3)
	svc := newCancelService(pool, 1)
	ctx := context.Background()

	run, err := svc.Cancel(ctx, runID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if run.State != "CANCELLING" || attemptStates(t, pool, runID)["RUNNING"] != 3 {
		t.Fatalf("after a failed fan-out: run %s attempts %v, want CANCELLING with 3 RUNNING",
			run.State, attemptStates(t, pool, runID))
	}

	ids, err := store.WFMListCancellingRuns(ctx, pool, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !listed(ids, runID) {
		t.Fatalf("WFMListCancellingRuns does not list the CANCELLING run")
	}

	if err := svc.ReconcileCancelling(ctx, runID); err != nil {
		t.Fatalf("ReconcileCancelling: %v", err)
	}
	if got := attemptStates(t, pool, runID); got["CANCELLED"] != 3 || len(got) != 1 {
		t.Errorf("attempts = %v, want all 3 CANCELLED", got)
	}
	if st, _, _ := runState(t, pool, runID); st != "CANCELLED" {
		t.Errorf("run = %s, want CANCELLED", st)
	}

	// Idempotent: the run is no longer listed, and reconciling it anyway
	// changes nothing and appends nothing.
	before := cancelledEvents(t, pool, runID)
	if err := svc.ReconcileCancelling(ctx, runID); err != nil {
		t.Fatalf("second ReconcileCancelling: %v", err)
	}
	if after := cancelledEvents(t, pool, runID); after != before {
		t.Errorf("a repeated reconcile appended %d AttemptCancelled events", after-before)
	}
	if ids, _ = store.WFMListCancellingRuns(ctx, pool, 1000); listed(ids, runID) {
		t.Error("a CANCELLED run is still listed for reconciliation")
	}
}

// TestReconcileCancellingKeepsFailingUntilTheSchedulerAnswers: a fan-out that
// fails again is reported and leaves the run CANCELLING for the next sweep,
// never finalized over live attempts.
func TestReconcileCancellingKeepsFailingUntilTheSchedulerAnswers(t *testing.T) {
	pool := retryPool(t)
	runID := cancellableRun(t, pool, 2)
	svc := newCancelService(pool, 3)
	ctx := context.Background()

	if _, err := svc.Cancel(ctx, runID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.ReconcileCancelling(ctx, runID); err == nil {
			t.Fatalf("reconcile %d: nil error while the scheduler is failing", i)
		}
		if st, _, _ := runState(t, pool, runID); st != "CANCELLING" {
			t.Fatalf("run = %s with live attempts, want CANCELLING", st)
		}
	}
	if err := svc.ReconcileCancelling(ctx, runID); err != nil {
		t.Fatalf("reconcile after recovery: %v", err)
	}
	if st, _, _ := runState(t, pool, runID); st != "CANCELLED" {
		t.Errorf("run = %s, want CANCELLED", st)
	}
}

// TestConcurrentCancelReconcilesCancelEachAttemptOnce races the re-issue from
// several replicas on a run with many attempts. Every attempt ends CANCELLED
// with exactly one AttemptCancelled event, and the run ends CANCELLED.
func TestConcurrentCancelReconcilesCancelEachAttemptOnce(t *testing.T) {
	pool := retryPool(t)
	const attempts = 12
	runID := cancellableRun(t, pool, attempts)
	svc := newCancelService(pool, 1)
	ctx := context.Background()
	if _, err := svc.Cancel(ctx, runID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- svc.ReconcileCancelling(ctx, runID)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		// A replica may wait out a peer past the lock timeout; that is the
		// retryable ErrAttemptBusy, and the next sweep covers it.
		if err != nil && !errors.Is(err, store.ErrAttemptBusy) {
			t.Fatalf("ReconcileCancelling: %v", err)
		}
	}
	// One more pass, as the next sweep would make.
	if err := svc.ReconcileCancelling(ctx, runID); err != nil {
		t.Fatalf("final ReconcileCancelling: %v", err)
	}

	if got := attemptStates(t, pool, runID); got["CANCELLED"] != attempts || len(got) != 1 {
		t.Errorf("attempts = %v, want all %d CANCELLED", got, attempts)
	}
	if n := cancelledEvents(t, pool, runID); n != attempts {
		t.Errorf("%d AttemptCancelled events for %d attempts; an attempt was cancelled twice or not at all", n, attempts)
	}
	if s, _, _ := runState(t, pool, runID); s != "CANCELLED" {
		t.Errorf("run = %s, want CANCELLED", s)
	}
}
