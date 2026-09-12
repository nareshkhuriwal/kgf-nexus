package schedules

import (
	"context"
	"fmt"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/cron"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// Bounded historical backfill.
//
// ARCHITECTURE §3.2 requires "generating a bounded set of runs over a historical
// interval with its own concurrency cap, so a backfill cannot starve live traffic".
// Both halves of that sentence are load-bearing and they guard different failures:
//
//   - BOUNDED, because a one-minute cron over three months is ~130,000 ticks. Without
//     a ceiling a mistyped interval is an outage, and the fleet would be executing
//     work nobody can stop faster than an operator can react.
//   - ITS OWN CONCURRENCY CAP, because the point of a backfill is that it runs SLOWER
//     than live traffic. A cap shared with the schedule would let three months of
//     history saturate the same budget the live schedule needs, which is precisely
//     the starvation the requirement names.
//
// WHY A CURSOR RATHER THAN GENERATING EVERY RUN UP FRONT. Creating 130,000 PENDING
// rows in one transaction would be a long lock on workflow_runs, and worse, an
// operator who then wanted to stop it would have to cancel every row individually.
// A cursor means the backfill holds at most max_active runs at any moment, pausing is
// a state change rather than a mass cancellation, and a replica that dies loses only
// the ticks it had not committed.

// BackfillRequest describes a requested backfill.
type BackfillRequest struct {
	Namespace  string
	ScheduleID string
	// From and To bound the LOGICAL times to generate, half-open: [From, To).
	// Half-open so two adjacent backfills cannot both generate the boundary tick.
	From      time.Time
	To        time.Time
	MaxActive int32
	MaxRuns   int32
	CreatedBy string
}

// Backfill limits. Both are ceilings on what a caller may ask for, not defaults.
const (
	// DefaultBackfillMaxActive is 1 on purpose: serial by default is the behaviour
	// least likely to surprise an operator who has not thought about the cap yet.
	DefaultBackfillMaxActive = 1
	// MaxBackfillActive bounds how much of the fleet one backfill may hold, however
	// large a number the caller asks for.
	MaxBackfillActive = 50
	// DefaultBackfillMaxRuns and MaxBackfillRuns bound total generation. The hard
	// ceiling is what makes a mistyped interval a rejected request rather than an
	// incident.
	DefaultBackfillMaxRuns = 1000
	MaxBackfillRuns        = 100000
)

// StartBackfill validates a request and creates the job.
//
// Validation happens HERE rather than in the driver because a rejected request should
// fail the operator's call, visibly, rather than becoming a FAILED row they have to go
// looking for.
func (s *Service) StartBackfill(ctx context.Context, req BackfillRequest) (store.Backfill, error) {
	sch, err := store.WFMGetScheduleByID(ctx, s.db, req.ScheduleID)
	if err != nil {
		return store.Backfill{}, err
	}
	if sch.Namespace != req.Namespace {
		// Namespaces are the tenancy boundary; knowing a schedule's UUID must not
		// grant the ability to generate runs in another tenant.
		return store.Backfill{}, fmt.Errorf("schedule %s belongs to namespace %s, not %s",
			req.ScheduleID, sch.Namespace, req.Namespace)
	}
	if !req.To.After(req.From) {
		return store.Backfill{}, fmt.Errorf("backfill interval is empty: to (%s) must be after from (%s)",
			req.To.Format(time.RFC3339), req.From.Format(time.RFC3339))
	}

	expr, err := cron.Parse(sch.CronExpr)
	if err != nil {
		return store.Backfill{}, fmt.Errorf("schedule %s/%s has an unparseable cron %q: %w",
			sch.Namespace, sch.Name, sch.CronExpr, err)
	}

	maxActive := req.MaxActive
	if maxActive <= 0 {
		maxActive = DefaultBackfillMaxActive
	}
	if maxActive > MaxBackfillActive {
		return store.Backfill{}, fmt.Errorf("max_active %d exceeds the ceiling of %d; a backfill "+
			"that can hold more of the fleet than this is indistinguishable from a denial of service",
			maxActive, MaxBackfillActive)
	}
	maxRuns := req.MaxRuns
	if maxRuns <= 0 {
		maxRuns = DefaultBackfillMaxRuns
	}
	if maxRuns > MaxBackfillRuns {
		return store.Backfill{}, fmt.Errorf("max_runs %d exceeds the ceiling of %d", maxRuns, MaxBackfillRuns)
	}

	// The first tick ON the grid at or after From. Distinct from From itself, which
	// is what the operator asked for and is usually between ticks.
	first, err := firstTickFrom(expr, req.From)
	if err != nil {
		return store.Backfill{}, fmt.Errorf("no cron tick falls in [%s, %s): %w",
			req.From.Format(time.RFC3339), req.To.Format(time.RFC3339), err)
	}
	if !first.Before(req.To) {
		return store.Backfill{}, fmt.Errorf("no cron tick falls in [%s, %s): the next tick of %q is %s",
			req.From.Format(time.RFC3339), req.To.Format(time.RFC3339), sch.CronExpr, first.Format(time.RFC3339))
	}

	bf, err := store.WFMCreateBackfill(ctx, s.db, store.Backfill{
		ScheduleID: sch.ID, Namespace: sch.Namespace,
		FromTime: req.From, ToTime: req.To,
		MaxActive: maxActive, MaxRuns: maxRuns, CreatedBy: req.CreatedBy,
		Detail: map[string]any{"cron": sch.CronExpr, "first_tick": first.Format(time.RFC3339)},
	}, first)
	if err != nil {
		return store.Backfill{}, err
	}

	s.log.InfoContext(ctx, "backfill started",
		"backfill_id", bf.ID, "schedule", sch.Namespace+"/"+sch.Name,
		"from", req.From.Format(time.RFC3339), "to", req.To.Format(time.RFC3339),
		"first_tick", first.Format(time.RFC3339),
		"max_active", maxActive, "max_runs", maxRuns)
	return bf, nil
}

// firstTickFrom returns the first grid instant at or after `from`.
//
// cron.Next is strictly-after, so asking it directly would skip a `from` that lands
// exactly on the grid -- and an operator backfilling "from 02:00" with a daily 02:00
// schedule means to include that tick. Stepping back one minute and asking for the
// next instant includes it without special-casing.
func firstTickFrom(expr *cron.Schedule, from time.Time) (time.Time, error) {
	return expr.Next(from.Add(-time.Minute))
}

// BackfillPass advances every claimable backfill by as many ticks as its cap allows.
//
// Exported so tests drive it deterministically rather than waiting on wall-clock ticks.
func (s *Service) BackfillPass(ctx context.Context, namespaces ...string) (int, error) {
	jobs, err := store.WFMClaimRunningBackfills(ctx, s.db, s.claimant, s.cfg.BackfillLease, s.cfg.BatchSize, namespaces...)
	if err != nil {
		return 0, fmt.Errorf("claim running backfills: %w", err)
	}
	total := 0
	for _, bf := range jobs {
		// Released as soon as this pass is done with it, so the next pass picks it up
		// on the following tick rather than waiting out the whole lease. The lease
		// exists to bound a CRASH, not to pace normal progress.
		n, aerr := s.advanceBackfill(ctx, bf)
		if rerr := store.WFMReleaseBackfill(ctx, s.db, bf.ID); rerr != nil {
			s.log.WarnContext(ctx, "could not release a backfill lease; it will expire on its own",
				"backfill_id", bf.ID, "error", rerr)
		}
		if aerr != nil {
			// One stuck backfill must not stop the others, and it is recorded as
			// FAILED with the reason rather than retried forever: a backfill whose
			// schedule was deleted or whose cron broke cannot make progress, and
			// silently retrying it would hide that from whoever started it.
			s.log.ErrorContext(ctx, "backfill failed; marking it so",
				"backfill_id", bf.ID, "namespace", bf.Namespace, "error", aerr)
			if _, serr := store.WFMSetBackfillState(ctx, s.db, bf.Namespace, bf.ID,
				store.BackfillFailed, map[string]any{"error": aerr.Error()}); serr != nil {
				s.log.ErrorContext(ctx, "could not mark a backfill failed", "backfill_id", bf.ID, "error", serr)
			}
		}
		total += n
	}
	return total, nil
}

// advanceBackfill generates ticks for one backfill until a bound stops it.
func (s *Service) advanceBackfill(ctx context.Context, bf store.Backfill) (int, error) {
	if bf.CursorAt == nil {
		// Nothing left to generate. Completing here rather than at the moment the
		// last tick was created means the transition is driven by observing the
		// cursor, which a resumed or re-claimed backfill reaches the same way.
		return 0, s.completeBackfill(ctx, bf, "interval exhausted")
	}
	if bf.RunsCreated >= bf.MaxRuns {
		return 0, s.completeBackfill(ctx, bf, fmt.Sprintf("reached max_runs (%d)", bf.MaxRuns))
	}

	sch, err := store.WFMGetScheduleByID(ctx, s.db, bf.ScheduleID)
	if err != nil {
		return 0, fmt.Errorf("load schedule %s: %w", bf.ScheduleID, err)
	}
	expr, err := cron.Parse(sch.CronExpr)
	if err != nil {
		return 0, fmt.Errorf("schedule %s/%s has an unparseable cron %q: %w",
			sch.Namespace, sch.Name, sch.CronExpr, err)
	}

	// THE CAP. Checked before generating anything and re-checked after every tick,
	// so a backfill never exceeds its budget even though each run is created in its
	// own transaction.
	active, err := store.WFMBackfillActiveRunCount(ctx, s.db, bf.ID)
	if err != nil {
		return 0, err
	}

	cursor := *bf.CursorAt
	created := 0
	remaining := int(bf.MaxRuns - bf.RunsCreated)

	for active < int(bf.MaxActive) && created < remaining && cursor.Before(bf.ToTime) {
		fired, ferr := s.fireTick(ctx, sch, cursor, &bf.ID)
		if ferr != nil {
			// Persist the progress made before the failure. Without this the next
			// pass would regenerate every tick since the last successful advance,
			// and the tick claim would absorb them -- correct, but it would burn a
			// pass doing nothing visible.
			if created > 0 {
				c := cursor
				_ = store.WFMAdvanceBackfill(ctx, s.db, bf.ID, &c, created)
			}
			return created, ferr
		}
		if fired {
			created++
			active++
		}

		next, nerr := expr.Next(cursor)
		if nerr != nil {
			// No further reachable tick: the interval is effectively exhausted.
			cursor = bf.ToTime
			break
		}
		cursor = next
	}

	// Persist the cursor exactly once per pass rather than once per tick: the tick
	// claim already makes regeneration harmless, so one write per pass is the cheaper
	// way to be crash-safe.
	var nextCursor *time.Time
	done := !cursor.Before(bf.ToTime)
	if !done {
		c := cursor
		nextCursor = &c
	}
	if err := store.WFMAdvanceBackfill(ctx, s.db, bf.ID, nextCursor, created); err != nil {
		return created, err
	}

	if done {
		bf.RunsCreated += int32(created)
		return created, s.completeBackfill(ctx, bf, "interval exhausted")
	}
	if created > 0 {
		s.log.InfoContext(ctx, "backfill advanced",
			"backfill_id", bf.ID, "namespace", bf.Namespace, "runs_created", created,
			"cursor_at", cursor.Format(time.RFC3339),
			"total_created", bf.RunsCreated+int32(created), "max_runs", bf.MaxRuns)
	}
	return created, nil
}

func (s *Service) completeBackfill(ctx context.Context, bf store.Backfill, reason string) error {
	if _, err := store.WFMSetBackfillState(ctx, s.db, bf.Namespace, bf.ID,
		store.BackfillCompleted, map[string]any{
			"reason":       reason,
			"runs_created": bf.RunsCreated,
		}); err != nil {
		return fmt.Errorf("complete backfill %s: %w", bf.ID, err)
	}
	s.log.InfoContext(ctx, "backfill completed",
		"backfill_id", bf.ID, "namespace", bf.Namespace,
		"runs_created", bf.RunsCreated, "reason", reason)
	return nil
}

// PauseBackfill stops generation without discarding progress.
//
// The cursor is left where it is, so resuming continues rather than restarting. That
// is the difference from cancelling, and it is why pausing does not touch ended_at.
func (s *Service) PauseBackfill(ctx context.Context, namespace, id string) (store.Backfill, error) {
	return store.WFMSetBackfillState(ctx, s.db, namespace, id, store.BackfillPaused,
		map[string]any{"reason": "paused by operator"})
}

// ResumeBackfill returns a paused backfill to RUNNING.
func (s *Service) ResumeBackfill(ctx context.Context, namespace, id string) (store.Backfill, error) {
	return store.WFMSetBackfillState(ctx, s.db, namespace, id, store.BackfillRunning,
		map[string]any{"reason": "resumed by operator"})
}

// CancelBackfill stops generation permanently.
//
// Runs ALREADY created are deliberately left alone. Cancelling them would be a
// second, much larger action -- and an operator who wants that has run cancellation
// for it. Stopping generation is the reversible part, so it is the part this does.
func (s *Service) CancelBackfill(ctx context.Context, namespace, id string) (store.Backfill, error) {
	bf, err := store.WFMSetBackfillState(ctx, s.db, namespace, id, store.BackfillCancelled,
		map[string]any{"reason": "cancelled by operator; runs already created are left running"})
	if err != nil {
		return store.Backfill{}, err
	}
	s.log.InfoContext(ctx, "backfill cancelled; already-created runs are untouched",
		"backfill_id", bf.ID, "namespace", bf.Namespace, "runs_created", bf.RunsCreated)
	return bf, nil
}
