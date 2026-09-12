//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package schedules

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NEXUS_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:test@localhost:55432/nexus"
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Skipf("test database unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fakeRuns records the runs a backfill asked for, and creates real workflow_runs rows
// so the active-run counting the cap depends on has something to count.
type fakeRuns struct {
	pool   *pgxpool.Pool
	ns     string
	specID string

	mu   sync.Mutex
	made []time.Time
}

func (f *fakeRuns) Create(ctx context.Context, req CreateRunRequest) (string, bool, error) {
	var id string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, trigger_kind, logical_date)
		VALUES ($1::uuid, $2, $3, $4) RETURNING id::text`,
		f.specID, f.ns, req.TriggerKind, req.LogicalDate).Scan(&id); err != nil {
		return "", false, err
	}
	f.mu.Lock()
	f.made = append(f.made, req.LogicalDate)
	f.mu.Unlock()
	return id, true, nil
}

func (f *fakeRuns) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.made)
}

// bfFixture is one isolated world: unique namespace, a spec, and a schedule.
type bfFixture struct {
	pool   *pgxpool.Pool
	ns     string
	specID string
	sched  store.Schedule
	runs   *fakeRuns
	svc    *Service
}

func newBFFixture(t *testing.T, cronExpr string) *bfFixture {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)
	ns := "bf-" + uuid.NewString()[:8]

	var specID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
		VALUES ($1, 'bf-spec', 1, $2, '{}'::jsonb, 'integration-test')
		RETURNING id::text`, ns, "h-"+ns).Scan(&specID); err != nil {
		t.Fatalf("insert spec: %v", err)
	}

	runs := &fakeRuns{pool: pool, ns: ns, specID: specID}
	svc := New(pool, runs, Config{BatchSize: 10, MaxCatchupPerPass: 10}, quiet())

	sch, err := store.WFMCreateSchedule(ctx, pool, store.Schedule{
		Namespace: ns, Name: "bf-sched", SpecName: "bf-spec", CronExpr: cronExpr,
		Priority: 100, Enabled: true, CreatedBy: "integration-test",
	}, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_runs WHERE namespace = $1`, ns)
		_, _ = pool.Exec(ctx, `DELETE FROM schedules WHERE namespace = $1`, ns)
		_, _ = pool.Exec(ctx, `DELETE FROM workflow_specs WHERE namespace = $1`, ns)
	})
	return &bfFixture{pool: pool, ns: ns, specID: specID, sched: sch, runs: runs, svc: svc}
}

// terminate marks every run of this namespace terminal, simulating work finishing so
// the cap frees up.
func (f *bfFixture) terminate(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE workflow_runs SET state='SUCCEEDED', ended_at=now()
		  WHERE namespace = $1 AND state NOT IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')`,
		f.ns); err != nil {
		t.Fatalf("terminate runs: %v", err)
	}
}

func (f *bfFixture) get(t *testing.T, id string) store.Backfill {
	t.Helper()
	bf, err := store.WFMGetBackfill(context.Background(), f.pool, f.ns, id)
	if err != nil {
		t.Fatalf("get backfill: %v", err)
	}
	return bf
}

// TestBackfillRespectsItsOwnConcurrencyCap is the requirement §3.2 actually states:
// "its own concurrency cap, so a backfill cannot starve live traffic". Without the
// cap a three-month backfill would saturate the same capacity the live schedule needs.
func TestBackfillRespectsItsOwnConcurrencyCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *") // hourly

	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC), // 24 ticks
		MaxActive: 3, MaxRuns: 100, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start backfill: %v", err)
	}

	// One pass must stop at max_active, not run the whole interval.
	n, err := f.svc.BackfillPass(ctx, f.ns)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if n != 3 {
		t.Fatalf("pass 1 created %d runs, want 3 (max_active); an uncapped backfill "+
			"would have generated all 24 and starved live traffic", n)
	}

	// While those runs are still active, further passes must create nothing.
	for i := 0; i < 3; i++ {
		if n, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
			t.Fatalf("blocked pass: %v", err)
		} else if n != 0 {
			t.Errorf("a pass created %d runs while the cap was full, want 0", n)
		}
	}

	// As work finishes the cap frees and the backfill continues.
	f.terminate(t)
	if n, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("pass after drain: %v", err)
	} else if n != 3 {
		t.Errorf("pass after drain created %d runs, want 3", n)
	}

	got := f.get(t, bf.ID)
	if got.RunsCreated != 6 {
		t.Errorf("runs_created = %d, want 6", got.RunsCreated)
	}
	if got.State != store.BackfillRunning {
		t.Errorf("state = %s, want RUNNING (18 ticks remain)", got.State)
	}
}

// TestBackfillStopsAtMaxRuns is the other bound: the ceiling that makes a mistyped
// interval a bounded job rather than an incident.
func TestBackfillStopsAtMaxRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "* * * * *") // every minute

	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), // ~44,640 ticks
		// A tiny budget against an enormous interval: max_runs must win.
		MaxActive: 5, MaxRuns: 7, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start backfill: %v", err)
	}

	for i := 0; i < 10; i++ {
		if _, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
		f.terminate(t)
		if f.get(t, bf.ID).State != store.BackfillRunning {
			break
		}
	}

	got := f.get(t, bf.ID)
	if got.RunsCreated > 7 {
		t.Errorf("runs_created = %d, exceeded max_runs of 7; a month of one-minute "+
			"ticks is ~44,000 runs and the ceiling is what stops that", got.RunsCreated)
	}
	if got.State != store.BackfillCompleted {
		t.Errorf("state = %s, want COMPLETED once max_runs is reached", got.State)
	}
	if f.runs.count() > 7 {
		t.Errorf("%d runs were actually created, want at most 7", f.runs.count())
	}
}

// TestBackfillCompletesAndGeneratesEveryTickExactlyOnce covers the happy path and the
// dedup together: a finite interval must produce each tick once and then complete.
func TestBackfillCompletesAndGeneratesEveryTickExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *") // hourly

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 1, 6, 0, 0, 0, time.UTC) // ticks at 00,01,02,03,04,05 = 6
	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID, From: from, To: to,
		MaxActive: 10, MaxRuns: 100, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start backfill: %v", err)
	}

	for i := 0; i < 5; i++ {
		if _, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
			t.Fatalf("pass: %v", err)
		}
		f.terminate(t)
		if f.get(t, bf.ID).State == store.BackfillCompleted {
			break
		}
	}

	got := f.get(t, bf.ID)
	if got.State != store.BackfillCompleted {
		t.Fatalf("state = %s, want COMPLETED", got.State)
	}
	if got.RunsCreated != 6 {
		t.Errorf("runs_created = %d, want 6 (hourly ticks in a 6-hour half-open interval)", got.RunsCreated)
	}
	// Half-open: the boundary tick at 06:00 must NOT be generated, or two adjacent
	// backfills would both produce it.
	for _, tick := range f.runs.made {
		if !tick.Before(to) {
			t.Errorf("generated tick %s is at or past the exclusive bound %s",
				tick.Format(time.RFC3339), to.Format(time.RFC3339))
		}
		if tick.Before(from) {
			t.Errorf("generated tick %s precedes from %s", tick.Format(time.RFC3339), from.Format(time.RFC3339))
		}
	}

	// Every logical tick appears exactly once in schedule_runs, which is the
	// constraint that makes a resumed or re-claimed backfill safe.
	var distinct, total int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(DISTINCT logical_at), count(*) FROM schedule_runs WHERE backfill_id = $1::uuid`,
		bf.ID).Scan(&distinct, &total); err != nil {
		t.Fatalf("count ticks: %v", err)
	}
	if distinct != 6 || total != 6 {
		t.Errorf("schedule_runs has %d rows over %d distinct ticks, want 6 and 6", total, distinct)
	}
}

// TestBackfillRunsAreTaggedAsBackfill matters for the operator view: a run created by
// catching up history must be distinguishable from one the live schedule fired.
func TestBackfillRunsAreTaggedAsBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")

	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC),
		MaxActive: 5, MaxRuns: 10, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("pass: %v", err)
	}

	var kinds []string
	rows, err := f.pool.Query(ctx, `
		SELECT r.trigger_kind FROM schedule_runs sr
		  JOIN workflow_runs r ON r.id = sr.run_id
		 WHERE sr.backfill_id = $1::uuid`, bf.ID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	if len(kinds) == 0 {
		t.Fatal("no runs were linked to the backfill")
	}
	for _, k := range kinds {
		if k != TriggerBackfill {
			t.Errorf("trigger_kind = %q, want %q so an operator can tell catch-up from live", k, TriggerBackfill)
		}
	}
}

// TestPauseStopsGenerationAndResumeContinues pins the difference between pausing and
// cancelling: a paused backfill keeps its cursor, so resuming continues rather than
// restarting from the beginning.
func TestPauseStopsGenerationAndResumeContinues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")

	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		MaxActive: 2, MaxRuns: 100, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("pass: %v", err)
	}
	f.terminate(t)

	paused, err := f.svc.PauseBackfill(ctx, f.ns, bf.ID)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if paused.State != store.BackfillPaused {
		t.Fatalf("state = %s, want PAUSED", paused.State)
	}
	// Pausing is reversible, so it must not stamp an end time.
	if paused.EndedAt != nil {
		t.Errorf("ended_at = %v on a paused backfill; it is resumable, not finished", paused.EndedAt)
	}
	cursorAtPause := paused.CursorAt
	before := f.runs.count()

	// A paused backfill is not claimable, so passes create nothing.
	for i := 0; i < 3; i++ {
		if n, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
			t.Fatalf("paused pass: %v", err)
		} else if n != 0 {
			t.Errorf("a paused backfill created %d runs", n)
		}
	}
	if f.runs.count() != before {
		t.Errorf("runs were created while paused: %d -> %d", before, f.runs.count())
	}

	resumed, err := f.svc.ResumeBackfill(ctx, f.ns, bf.ID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// The cursor survived, so work continues instead of restarting.
	if resumed.CursorAt == nil || cursorAtPause == nil || !resumed.CursorAt.Equal(*cursorAtPause) {
		t.Errorf("cursor moved across pause/resume: %v -> %v; resuming must continue, not restart",
			cursorAtPause, resumed.CursorAt)
	}
	if n, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("resumed pass: %v", err)
	} else if n == 0 {
		t.Error("a resumed backfill created nothing")
	}
}

// TestCancelLeavesCreatedRunsAlone pins a deliberate choice: cancelling stops
// GENERATION, not the runs already made. Cancelling those is a much larger action and
// an operator has run cancellation for it.
func TestCancelLeavesCreatedRunsAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")

	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		MaxActive: 2, MaxRuns: 100, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("pass: %v", err)
	}

	var liveBefore int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM schedule_runs WHERE backfill_id = $1::uuid`, bf.ID).Scan(&liveBefore); err != nil {
		t.Fatal(err)
	}
	if liveBefore == 0 {
		t.Fatal("no runs to test against")
	}

	cancelled, err := f.svc.CancelBackfill(ctx, f.ns, bf.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if cancelled.State != store.BackfillCancelled {
		t.Errorf("state = %s, want CANCELLED", cancelled.State)
	}
	if cancelled.EndedAt == nil {
		t.Error("a cancelled backfill has no ended_at; it is terminal and should be stamped")
	}

	var liveAfter int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM schedule_runs WHERE backfill_id = $1::uuid`, bf.ID).Scan(&liveAfter); err != nil {
		t.Fatal(err)
	}
	if liveAfter != liveBefore {
		t.Errorf("cancelling changed the run count %d -> %d; already-created runs must be left alone",
			liveBefore, liveAfter)
	}
	// And generation genuinely stopped.
	if n, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("post-cancel pass: %v", err)
	} else if n != 0 {
		t.Errorf("a cancelled backfill created %d more runs", n)
	}
}

// TestStartBackfillRejectsBadRequests: every one of these is something an operator can
// fix, so it must fail their call rather than becoming a FAILED row to discover later.
func TestStartBackfillRejectsBadRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 2 * * *") // daily at 02:00

	base := BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC),
		MaxActive: 2, MaxRuns: 10, CreatedBy: "test",
	}

	cases := map[string]func(*BackfillRequest){
		"inverted interval":       func(r *BackfillRequest) { r.To = r.From.Add(-time.Hour) },
		"empty interval":          func(r *BackfillRequest) { r.To = r.From },
		"max_active over ceiling": func(r *BackfillRequest) { r.MaxActive = MaxBackfillActive + 1 },
		"max_runs over ceiling":   func(r *BackfillRequest) { r.MaxRuns = MaxBackfillRuns + 1 },
		"wrong namespace":         func(r *BackfillRequest) { r.Namespace = "someone-else" },
		"unknown schedule":        func(r *BackfillRequest) { r.ScheduleID = uuid.NewString() },
		// A window between ticks: a daily 02:00 schedule has no tick in a 10-minute
		// window at midnight, and generating nothing silently would look like a
		// backfill that ran and did nothing.
		"no tick in the interval": func(r *BackfillRequest) {
			r.From = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
			r.To = time.Date(2026, 3, 1, 0, 10, 0, 0, time.UTC)
		},
	}
	for name, mutate := range cases {
		req := base
		mutate(&req)
		if _, err := f.svc.StartBackfill(ctx, req); err == nil {
			t.Errorf("%s: StartBackfill succeeded, want an error the operator can act on", name)
		}
	}
}

// TestBackfillFirstTickIncludesAnExactBoundary covers the off-by-one. cron.Next is
// strictly-after, so an operator backfilling "from 02:00" against a daily 02:00
// schedule must still get that tick.
func TestBackfillFirstTickIncludesAnExactBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 2 * * *")

	from := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC) // exactly on the grid
	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID, From: from,
		To:        time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC),
		MaxActive: 5, MaxRuns: 10, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if bf.CursorAt == nil || !bf.CursorAt.Equal(from) {
		t.Errorf("first cursor = %v, want %s; a from-time exactly on the grid must be included",
			bf.CursorAt, from.Format(time.RFC3339))
	}

	if _, err := f.svc.BackfillPass(ctx, f.ns); err != nil {
		t.Fatalf("pass: %v", err)
	}
	var got bool
	for _, tick := range f.runs.made {
		if tick.Equal(from) {
			got = true
		}
	}
	if !got {
		t.Errorf("the boundary tick %s was skipped; generated %v", from.Format(time.RFC3339), f.runs.made)
	}
}

// TestConcurrentBackfillPassesDoNotDoubleGenerate is the multi-replica case. Two
// replicas advancing one backfill would both read the same cursor, so the claim must
// give it to one at a time and the tick constraint must absorb any overlap.
func TestConcurrentBackfillPassesDoNotDoubleGenerate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")

	bf, err := f.svc.StartBackfill(ctx, BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		MaxActive: 4, MaxRuns: 100, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	const replicas = 6
	var wg sync.WaitGroup
	wg.Add(replicas)
	for i := 0; i < replicas; i++ {
		go func() {
			defer wg.Done()
			// Errors are tolerated: a replica that loses the claim race legitimately
			// does nothing. What must NOT happen is a duplicate tick.
			_, _ = f.svc.BackfillPass(ctx, f.ns)
		}()
	}
	wg.Wait()

	var distinct, total int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(DISTINCT logical_at), count(*) FROM schedule_runs WHERE backfill_id = $1::uuid`,
		bf.ID).Scan(&distinct, &total); err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != distinct {
		t.Errorf("%d rows over %d distinct ticks: a tick was generated twice", total, distinct)
	}
	if total > 4 {
		t.Errorf("%d runs created, exceeding max_active of 4 under concurrency", total)
	}
	if total == 0 {
		t.Error("six concurrent passes created nothing")
	}
	_ = fmt.Sprint()
}
