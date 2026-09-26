//go:build integration

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package schedules

// F119: backfill pause, resume and cancel are compare-and-set transitions.
// Only RUNNING->PAUSED, PAUSED->RUNNING and {RUNNING,PAUSED}->CANCELLED are
// operator transitions; everything else is refused with
// store.ErrBackfillStateConflict and changes nothing. The driver's own
// COMPLETED/FAILED transitions apply only to a backfill that is still RUNNING,
// so a cancel or pause that lands while the driver holds the backfill is not
// overwritten.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func (f *bfFixture) startBackfill(t *testing.T) store.Backfill {
	t.Helper()
	bf, err := f.svc.StartBackfill(context.Background(), BackfillRequest{
		Namespace: f.ns, ScheduleID: f.sched.ID,
		From:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		MaxActive: 2, MaxRuns: 100, CreatedBy: "test",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return bf
}

// forceState puts a backfill in any state directly, as the driver or an
// earlier operator action would have left it.
func (f *bfFixture) forceState(t *testing.T, id, state string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE schedule_backfills
		   SET state = $2,
		       ended_at = CASE WHEN $2 IN ('COMPLETED','CANCELLED','FAILED') THEN now() ELSE NULL END
		 WHERE id = $1::uuid`, id, state); err != nil {
		t.Fatalf("force state %s: %v", state, err)
	}
}

func (f *bfFixture) action(ctx context.Context, action, id string) (store.Backfill, error) {
	switch action {
	case "pause":
		return f.svc.PauseBackfill(ctx, f.ns, id)
	case "resume":
		return f.svc.ResumeBackfill(ctx, f.ns, id)
	default:
		return f.svc.CancelBackfill(ctx, f.ns, id)
	}
}

// TestBackfillTransitionsAreGuarded is the full from-state x action table.
// Before the fix every cell succeeded, so resuming a CANCELLED backfill set it
// RUNNING again and cleared ended_at, and the driver resumed generating runs.
func TestBackfillTransitionsAreGuarded(t *testing.T) {
	t.Parallel()
	allowed := map[string]map[string]string{ // action -> from -> to
		"pause":  {store.BackfillRunning: store.BackfillPaused},
		"resume": {store.BackfillPaused: store.BackfillRunning},
		"cancel": {store.BackfillRunning: store.BackfillCancelled, store.BackfillPaused: store.BackfillCancelled},
	}
	states := []string{store.BackfillRunning, store.BackfillPaused,
		store.BackfillCompleted, store.BackfillCancelled, store.BackfillFailed}

	f := newBFFixture(t, "0 * * * *")
	ctx := context.Background()
	for _, action := range []string{"pause", "resume", "cancel"} {
		for _, from := range states {
			t.Run(action+"/"+from, func(t *testing.T) {
				bf := f.startBackfill(t)
				f.forceState(t, bf.ID, from)
				before, err := store.WFMGetBackfill(ctx, f.pool, f.ns, bf.ID)
				if err != nil {
					t.Fatal(err)
				}

				got, err := f.action(ctx, action, bf.ID)
				to, ok := allowed[action][from]
				if ok {
					if err != nil {
						t.Fatalf("%s from %s: %v, want %s", action, from, err, to)
					}
					if got.State != to {
						t.Errorf("state = %s, want %s", got.State, to)
					}
					return
				}

				if !errors.Is(err, store.ErrBackfillStateConflict) {
					t.Fatalf("%s from %s: err = %v, want ErrBackfillStateConflict", action, from, err)
				}
				var conflict *store.BackfillStateConflict
				if !errors.As(err, &conflict) || conflict.Current != from {
					t.Errorf("conflict = %+v, want current state %s", conflict, from)
				}
				after, err := store.WFMGetBackfill(ctx, f.pool, f.ns, bf.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.State != before.State || (after.EndedAt == nil) != (before.EndedAt == nil) {
					t.Errorf("a refused %s changed the backfill: %s ended=%v -> %s ended=%v",
						action, before.State, before.EndedAt, after.State, after.EndedAt)
				}
			})
		}
	}

	// An unknown id in this namespace is still NotFound, not a conflict.
	if _, err := f.svc.ResumeBackfill(ctx, f.ns, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrBackfillNotFound) {
		t.Errorf("resume of a missing backfill: %v, want ErrBackfillNotFound", err)
	}
}

// TestCancelledBackfillStaysCancelled is the reported symptom end to end: a
// resume after cancel is refused and a later pass creates nothing.
func TestCancelledBackfillStaysCancelled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")
	bf := f.startBackfill(t)
	if _, err := f.svc.CancelBackfill(ctx, f.ns, bf.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := f.svc.ResumeBackfill(ctx, f.ns, bf.ID); !errors.Is(err, store.ErrBackfillStateConflict) {
		t.Fatalf("resume after cancel: %v, want a conflict", err)
	}
	if n, err := f.svc.BackfillPass(ctx, f.ns); err != nil || n != 0 {
		t.Errorf("pass after a refused resume created %d runs (err %v); a cancel must be final", n, err)
	}
}

// TestDriverCompletionDoesNotOverwriteCancel: the driver's COMPLETED and FAILED
// transitions are from RUNNING only. A cancel that commits while a pass holds
// the backfill used to be overwritten by the pass's completion.
func TestDriverCompletionDoesNotOverwriteCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")
	bf := f.startBackfill(t)
	if _, err := f.svc.CancelBackfill(ctx, f.ns, bf.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	// What a pass that claimed bf before the cancel does next.
	if err := f.svc.completeBackfill(ctx, bf, "interval exhausted"); err != nil {
		t.Fatalf("complete after cancel: %v, want a no-op", err)
	}
	got, err := store.WFMGetBackfill(ctx, f.pool, f.ns, bf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.BackfillCancelled {
		t.Errorf("state = %s after a late driver completion, want CANCELLED", got.State)
	}
}

// TestConcurrentBackfillTransitionsAreLinearizable races pauses, resumes and
// cancels on one backfill. With compare-and-set transitions the successful
// calls form a legal serial history: pause and resume alternate starting from
// RUNNING, exactly one cancel wins, nothing succeeds after it, and every
// refusal is a conflict. Before the fix every call succeeded.
func TestConcurrentBackfillTransitionsAreLinearizable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newBFFixture(t, "0 * * * *")

	for round := 0; round < 5; round++ {
		bf := f.startBackfill(t)

		const perAction = 8
		type result struct {
			action string
			err    error
		}
		results := make(chan result, 3*perAction)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, action := range []string{"pause", "resume", "cancel"} {
			for i := 0; i < perAction; i++ {
				wg.Add(1)
				go func(action string) {
					defer wg.Done()
					<-start
					_, err := f.action(ctx, action, bf.ID)
					results <- result{action: action, err: err}
				}(action)
			}
		}
		close(start)
		wg.Wait()
		close(results)

		wins := map[string]int{}
		for r := range results {
			if r.err != nil {
				if !errors.Is(r.err, store.ErrBackfillStateConflict) {
					t.Fatalf("round %d: %s failed with %v, want only conflicts", round, r.action, r.err)
				}
				continue
			}
			wins[r.action]++
		}
		if wins["cancel"] != 1 {
			t.Errorf("round %d: %d cancels succeeded, want exactly 1", round, wins["cancel"])
		}
		if d := wins["pause"] - wins["resume"]; d != 0 && d != 1 {
			t.Errorf("round %d: %d pauses and %d resumes succeeded; they must alternate from RUNNING",
				round, wins["pause"], wins["resume"])
		}
		got, err := store.WFMGetBackfill(ctx, f.pool, f.ns, bf.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != store.BackfillCancelled || got.EndedAt == nil {
			t.Errorf("round %d: final state %s ended=%v, want CANCELLED with ended_at", round, got.State, got.EndedAt)
		}
	}
}
