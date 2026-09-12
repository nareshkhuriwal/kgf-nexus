package schedules

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/cron"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func mustCron(t *testing.T, expr string) *cron.Schedule {
	t.Helper()
	c, err := cron.Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return c
}

// TestPreviousTickFindsTheTickThatJustCameDue covers the helper the no-catchup path
// depends on. cron.Next only moves forward, so finding the tick that just became due
// means stepping back and walking forward -- and an off-by-one here either re-fires
// the previous tick or skips the current one.
func TestPreviousTickFindsTheTickThatJustCameDue(t *testing.T) {
	cases := []struct {
		expr, now, want string
	}{
		{"*/15 * * * *", "2026-03-01T10:17:00Z", "2026-03-01T10:15:00Z"},
		// Exactly on a grid instant: that instant IS the due tick.
		{"*/15 * * * *", "2026-03-01T10:15:00Z", "2026-03-01T10:15:00Z"},
		{"0 2 * * *", "2026-03-01T09:00:00Z", "2026-03-01T02:00:00Z"},
		{"0 0 * * *", "2026-03-01T00:00:30Z", "2026-03-01T00:00:00Z"},
	}
	for _, c := range cases {
		got, ok := previousTick(mustCron(t, c.expr), at(c.now))
		if !ok {
			t.Errorf("%q at %s: no tick found", c.expr, c.now)
			continue
		}
		if !got.Equal(at(c.want)) {
			t.Errorf("%q at %s = %s, want %s", c.expr, c.now, got.Format(time.RFC3339), c.want)
		}
	}
}

// TestNoCatchupFiresOnlyTheCurrentTick is the default, and the safe one: a schedule
// paused for a month must not stampede the fleet when it is un-paused.
func TestNoCatchupFiresOnlyTheCurrentTick(t *testing.T) {
	s := New(nil, nil, DefaultConfig(), quiet())
	lastWeek := at("2026-02-22T00:00:00Z")
	sch := store.Schedule{CronExpr: "*/5 * * * *", CatchupLimit: 0, LastFireAt: &lastWeek}

	ticks := s.dueTicks(mustCron(t, sch.CronExpr), sch, at("2026-03-01T10:07:00Z"))
	if len(ticks) != 1 {
		t.Fatalf("catchup_limit=0 produced %d ticks, want 1; a week of missed 5-minute "+
			"ticks is ~2000 runs nobody asked for", len(ticks))
	}
	if !ticks[0].Equal(at("2026-03-01T10:05:00Z")) {
		t.Errorf("tick = %s, want the current grid instant 10:05", ticks[0].Format(time.RFC3339))
	}
}

// TestCatchupIsBoundedByBothLimits pins the two independent bounds. The schedule's
// catchup_limit is the operator's statement about whether missed work still matters;
// MaxCatchupPerPass stops one schedule monopolising a pass.
func TestCatchupIsBoundedByBothLimits(t *testing.T) {
	last := at("2026-03-01T09:00:00Z")
	now := at("2026-03-01T10:00:00Z") // 12 missed 5-minute ticks

	// The schedule's own limit binds.
	s := New(nil, nil, Config{MaxCatchupPerPass: 100}, quiet())
	sch := store.Schedule{CronExpr: "*/5 * * * *", CatchupLimit: 3, LastFireAt: &last}
	if got := s.dueTicks(mustCron(t, sch.CronExpr), sch, now); len(got) != 3 {
		t.Errorf("catchup_limit=3 produced %d ticks, want 3", len(got))
	}

	// The per-pass cap binds when it is tighter.
	s2 := New(nil, nil, Config{MaxCatchupPerPass: 2}, quiet())
	sch2 := store.Schedule{CronExpr: "*/5 * * * *", CatchupLimit: 100, LastFireAt: &last}
	got := s2.dueTicks(mustCron(t, sch2.CronExpr), sch2, now)
	if len(got) != 2 {
		t.Errorf("MaxCatchupPerPass=2 produced %d ticks, want 2", len(got))
	}
	// Oldest first: catching up out of order would run yesterday's data after today's.
	if len(got) == 2 && !got[0].Before(got[1]) {
		t.Errorf("catch-up ticks are not oldest-first: %v", got)
	}
	if len(got) > 0 && !got[0].Equal(at("2026-03-01T09:05:00Z")) {
		t.Errorf("first catch-up tick = %s, want 09:05 (the one right after last_fire_at)",
			got[0].Format(time.RFC3339))
	}
}

// TestCatchupNeverFiresTheFuture guards the obvious disaster: a catch-up walk that
// did not stop at `now` would generate runs for ticks that have not happened.
func TestCatchupNeverFiresTheFuture(t *testing.T) {
	last := at("2026-03-01T09:00:00Z")
	now := at("2026-03-01T09:12:00Z")
	s := New(nil, nil, Config{MaxCatchupPerPass: 50}, quiet())
	sch := store.Schedule{CronExpr: "*/5 * * * *", CatchupLimit: 50, LastFireAt: &last}

	for _, tick := range s.dueTicks(mustCron(t, sch.CronExpr), sch, now) {
		if tick.After(now) {
			t.Errorf("tick %s is in the future (now = %s)", tick.Format(time.RFC3339), now.Format(time.RFC3339))
		}
	}
}

// TestLastFireAheadOfGridStillFires covers a schedule whose last_fire_at is ahead of
// the grid -- a clock change or a manual edit. Without the fallback it would go
// permanently quiet, which is the failure an operator cannot diagnose.
func TestLastFireAheadOfGridStillFires(t *testing.T) {
	future := at("2026-03-02T00:00:00Z")
	s := New(nil, nil, DefaultConfig(), quiet())
	sch := store.Schedule{CronExpr: "*/5 * * * *", CatchupLimit: 5, LastFireAt: &future}

	got := s.dueTicks(mustCron(t, sch.CronExpr), sch, at("2026-03-01T10:07:00Z"))
	if len(got) != 1 {
		t.Fatalf("produced %d ticks, want 1; a schedule whose last_fire_at is ahead of "+
			"the grid must still fire rather than go silent", len(got))
	}
	if !got[0].Equal(at("2026-03-01T10:05:00Z")) {
		t.Errorf("tick = %s, want 10:05", got[0].Format(time.RFC3339))
	}
}

func TestConfigDefaultsAreApplied(t *testing.T) {
	s := New(nil, nil, Config{}, nil)
	d := DefaultConfig()
	if s.cfg.Interval != d.Interval || s.cfg.BatchSize != d.BatchSize ||
		s.cfg.StalledSweepInterval != d.StalledSweepInterval ||
		s.cfg.MaxCatchupPerPass != d.MaxCatchupPerPass {
		t.Errorf("zero Config was not filled with defaults: %+v", s.cfg)
	}
	// A zero MaxCatchupPerPass would make catch-up fire nothing, silently.
	if s.cfg.MaxCatchupPerPass == 0 {
		t.Error("MaxCatchupPerPass is 0; catch-up would silently never fire")
	}
}
