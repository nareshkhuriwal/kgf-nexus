//go:build integration

package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newSchedule(t *testing.T, ctx context.Context, f *fixture, name, cron string) Schedule {
	t.Helper()
	s, err := WFMCreateSchedule(ctx, f.pool, Schedule{
		Namespace: f.ns, Name: name, SpecName: "fixture", CronExpr: cron,
		Priority: 100, Enabled: true, CreatedBy: "integration-test",
		Params: map[string]any{"k": "v"},
	}, time.Now().UTC().Add(-time.Minute)) // already due
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	t.Cleanup(func() { _ = WFMDeleteSchedule(ctx, f.pool, f.ns, name) })
	return s
}

func TestScheduleRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))

	s := newSchedule(t, ctx, f, "daily", "0 2 * * *")
	if s.OverlapPolicy != OverlapSkip {
		t.Errorf("overlap_policy = %q, want the safe default %q", s.OverlapPolicy, OverlapSkip)
	}
	if s.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC; the column must never lie about what was used", s.Timezone)
	}
	if s.Params["k"] != "v" {
		t.Errorf("params round-tripped as %v", s.Params)
	}

	got, err := WFMGetSchedule(ctx, f.pool, f.ns, "daily")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != s.ID {
		t.Errorf("get returned %s, want %s", got.ID, s.ID)
	}

	if _, err := WFMGetSchedule(ctx, f.pool, f.ns, "nope"); err == nil {
		t.Error("get of a missing schedule succeeded")
	}
}

// TestDisablingClearsTheFireTime pins why disable NULLs next_fire_at rather than
// leaving it: a stale fire time keeps the row in the due index forever, so every
// claim has to re-check `enabled` and a disabled schedule still costs work on the
// hot path.
func TestDisablingClearsTheFireTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))
	newSchedule(t, ctx, f, "toggle", "*/5 * * * *")

	off, err := WFMSetScheduleEnabled(ctx, f.pool, f.ns, "toggle", false, nil)
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if off.Enabled {
		t.Error("schedule still enabled after disable")
	}
	if off.NextFireAt != nil {
		t.Errorf("next_fire_at = %v after disable, want NULL so the row leaves the due index", off.NextFireAt)
	}

	// A disabled schedule must never be claimed, however overdue it looks.
	claimed, err := WFMClaimDueSchedules(ctx, f.pool, time.Now().UTC().Add(time.Hour), 10, f.ns)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, c := range claimed {
		if c.Name == "toggle" {
			t.Error("a disabled schedule was claimed")
		}
	}

	next := time.Now().UTC().Add(5 * time.Minute)
	on, err := WFMSetScheduleEnabled(ctx, f.pool, f.ns, "toggle", true, &next)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if on.NextFireAt == nil || !on.NextFireAt.Truncate(time.Second).Equal(next.Truncate(time.Second)) {
		t.Errorf("next_fire_at = %v after enable, want %v", on.NextFireAt, next)
	}
}

// TestClaimIsExactlyOnceAcrossReplicas is the property that lets several
// workflow-manager replicas run the fire loop with no leader election. Two replicas
// claiming the same due schedule would create two runs for one tick.
func TestClaimIsExactlyOnceAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))
	s := newSchedule(t, ctx, f, "contended", "*/5 * * * *")

	const replicas = 8
	var (
		mu     sync.Mutex
		claims int
		wg     sync.WaitGroup
	)
	wg.Add(replicas)
	for i := 0; i < replicas; i++ {
		go func() {
			defer wg.Done()
			got, err := WFMClaimDueSchedules(ctx, f.pool, time.Now().UTC(), 10, f.ns)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, c := range got {
				if c.ID == s.ID {
					claims++
				}
			}
		}()
	}
	wg.Wait()

	if claims != 1 {
		t.Errorf("schedule was claimed %d times, want exactly 1; more than one means "+
			"two replicas fire the same tick", claims)
	}

	// The claim NULLs next_fire_at, so the schedule has left the due index and the
	// handler now owns arming the next tick.
	after, err := WFMGetScheduleByID(ctx, f.pool, s.ID)
	if err != nil {
		t.Fatalf("get after claim: %v", err)
	}
	if after.NextFireAt != nil {
		t.Errorf("next_fire_at = %v after claim, want NULL", after.NextFireAt)
	}
}

// TestTickClaimDeduplicatesRedelivery is what makes at-least-once timer delivery
// safe. A redelivered fire must find the tick taken rather than create a second run.
func TestTickClaimDeduplicatesRedelivery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))
	s := newSchedule(t, ctx, f, "dedup", "*/5 * * * *")

	tick := time.Now().UTC().Truncate(time.Minute)
	runA := f.newRun(t)
	if err := WFMClaimScheduleTick(ctx, f.pool, s.ID, tick, runA, nil); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// A second delivery of the same tick, with a DIFFERENT run id, must be refused.
	runB := f.newRun(t)
	err := WFMClaimScheduleTick(ctx, f.pool, s.ID, tick, runB, nil)
	if err == nil {
		t.Fatal("a redelivered tick was claimed twice; the schedule would double-fire")
	}
	if !isTickAlreadyFired(err) {
		t.Errorf("error = %v, want ErrTickAlreadyFired so the caller can treat it as normal", err)
	}

	// A DIFFERENT tick of the same schedule is independent.
	if err := WFMClaimScheduleTick(ctx, f.pool, s.ID, tick.Add(5*time.Minute), runB, nil); err != nil {
		t.Errorf("a later tick was refused: %v", err)
	}

	exists, err := WFMScheduleTickExists(ctx, f.pool, s.ID, tick)
	if err != nil {
		t.Fatalf("tick exists: %v", err)
	}
	if !exists {
		t.Error("WFMScheduleTickExists did not see a claimed tick")
	}
}

// TestConcurrentTickClaimsYieldOneWinner drives the dedup concurrently, which is
// the shape a multi-replica fleet actually produces.
func TestConcurrentTickClaimsYieldOneWinner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))
	s := newSchedule(t, ctx, f, "race", "*/5 * * * *")
	tick := time.Now().UTC().Truncate(time.Minute)

	const writers = 8
	runs := make([]string, writers)
	for i := range runs {
		runs[i] = f.newRun(t)
	}

	var (
		mu      sync.Mutex
		winners int
		wg      sync.WaitGroup
	)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			if err := WFMClaimScheduleTick(ctx, f.pool, s.ID, tick, runs[i], nil); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d writers claimed one tick, want exactly 1", winners)
	}
}

// TestActiveRunCountDrivesOverlapPolicy checks the input the skip policy reads. A
// miscount here either blocks a schedule forever or lets it overlap when it must not.
func TestActiveRunCountDrivesOverlapPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))
	s := newSchedule(t, ctx, f, "overlap", "*/5 * * * *")

	if n, err := WFMActiveRunCountForSchedule(ctx, f.pool, s.ID); err != nil || n != 0 {
		t.Fatalf("active count on a fresh schedule = (%d, %v), want (0, nil)", n, err)
	}

	runID := f.newRun(t)
	if err := WFMClaimScheduleTick(ctx, f.pool, s.ID, time.Now().UTC().Truncate(time.Minute), runID, nil); err != nil {
		t.Fatalf("claim tick: %v", err)
	}
	// A fresh run is PENDING, which counts as active.
	if n, err := WFMActiveRunCountForSchedule(ctx, f.pool, s.ID); err != nil || n != 1 {
		t.Fatalf("active count with a PENDING run = (%d, %v), want (1, nil)", n, err)
	}

	// A terminal run must stop counting, or the schedule never fires again.
	if _, err := f.pool.Exec(ctx,
		`UPDATE workflow_runs SET state='SUCCEEDED', ended_at=now() WHERE id=$1::uuid`, runID); err != nil {
		t.Fatalf("terminate run: %v", err)
	}
	if n, err := WFMActiveRunCountForSchedule(ctx, f.pool, s.ID); err != nil || n != 0 {
		t.Errorf("active count with a SUCCEEDED run = (%d, %v), want (0, nil); a schedule "+
			"whose finished runs still count would never fire again", n, err)
	}
}

// TestRearmFindsStalledSchedules covers the worst failure this feature can have: a
// replica claims a schedule, dies before arming the next tick, and the row is left
// enabled but absent from the due index. An operator sees an active schedule that
// silently never fires.
func TestRearmFindsStalledSchedules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t, ctx, testPool(t))
	s := newSchedule(t, ctx, f, "stalled", "*/5 * * * *")

	// Simulate the crash: claimed (next_fire_at NULL) but still enabled.
	if _, err := WFMClaimDueSchedules(ctx, f.pool, time.Now().UTC(), 10, f.ns); err != nil {
		t.Fatalf("claim: %v", err)
	}

	stalled, err := WFMRearmStalledSchedules(ctx, f.pool, 50, f.ns)
	if err != nil {
		t.Fatalf("rearm sweep: %v", err)
	}
	var found bool
	for _, c := range stalled {
		if c.ID == s.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the sweep did not find a schedule left enabled with no next_fire_at; " +
			"such a schedule stops firing forever and nothing else would notice")
	}

	// Arming it returns it to the due index.
	next := time.Now().UTC().Add(5 * time.Minute)
	if err := WFMArmSchedule(ctx, f.pool, s.ID, next, &next); err != nil {
		t.Fatalf("arm: %v", err)
	}
	again, err := WFMRearmStalledSchedules(ctx, f.pool, 50, f.ns)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	for _, c := range again {
		if c.ID == s.ID {
			t.Error("an armed schedule is still reported as stalled")
		}
	}
}

// newRun inserts a run for this fixture's spec and returns its id.
//
// Schedule tests need SEVERAL runs (the dedup tests hand a different run id to each
// competing writer), so the fixture's single pre-made run is not enough.
func (f *fixture) newRun(t *testing.T) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx, `
		INSERT INTO workflow_runs (spec_id, namespace, trigger_kind)
		VALUES ($1::uuid, $2, 'schedule')
		RETURNING id::text`, f.specID, f.ns).Scan(&id); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return id
}

func isTickAlreadyFired(err error) bool {
	return err != nil && err.Error() == ErrTickAlreadyFired.Error()
}
