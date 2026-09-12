// Package schedules turns cron schedules into runs, and runs bounded backfills.
//
// WHERE THIS LIVES AND WHY. The fire loop is in workflow-manager because it already
// owns workflow_specs, workflow_runs and timers (ADR-0011), and a schedule's whole
// job is creating a run from a spec at a time. Putting it anywhere else would make
// run creation a cross-service call on the one path that must stay simple.
//
// THE DURABILITY MODEL, which is the part worth understanding. Nothing here holds a
// schedule in memory. A schedule's next fire time is a column, the claim is an
// UPDATE, and the tick's identity is a row with a unique constraint. So a replica
// can be killed at any instant and the worst outcome is a schedule that needs
// re-arming (which SweepStalled recovers) or a tick that is retried (which the
// unique constraint deduplicates). That is the same property the timer wheel has,
// and for the same reason: in-memory scheduler state is how cron daemons lose work
// on restart.
package schedules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/cron"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// RunCreator is the run-creation surface this package needs.
//
// An interface rather than the concrete runs.Service so the fire logic is testable
// without a database, and so the dependency direction stays one-way: schedules know
// how to ask for a run, and runs know nothing about schedules.
type RunCreator interface {
	Create(ctx context.Context, req CreateRunRequest) (runID string, created bool, err error)
}

// CreateRunRequest mirrors the fields runs.CreateRequest needs from a schedule.
type CreateRunRequest struct {
	Namespace      string
	SpecName       string
	SpecVersion    *int32
	IdempotencyKey string
	TriggerKind    string
	TriggerDetail  map[string]any
	Params         map[string]any
	LogicalDate    time.Time
	Priority       int32
}

// Trigger kinds recorded on the runs a schedule creates, so an operator reading a
// run can tell a scheduled tick from a manual submission from a backfill.
const (
	TriggerSchedule = "schedule"
	TriggerBackfill = "backfill"
)

// Config tunes the loops.
type Config struct {
	// Interval is how often due schedules are claimed. A minute is cron's
	// resolution, so polling faster buys nothing; polling this often bounds how
	// late a tick can be to roughly one interval.
	Interval time.Duration
	// BatchSize caps schedules claimed per pass, so one replica cannot take the
	// whole fleet's work and then sit on it.
	BatchSize int
	// StalledSweepInterval is how often enabled-but-unarmed schedules are
	// recovered. Rare on purpose: it only matters after a replica died mid-fire.
	StalledSweepInterval time.Duration
	// MaxCatchupPerPass bounds how many missed ticks one pass fires for one
	// schedule, independent of the schedule's own catchup_limit. A second bound
	// because a schedule with a generous catchup_limit should still not monopolise
	// a pass.
	MaxCatchupPerPass int
}

// DefaultConfig is deliberately unhurried. Scheduling is not latency-sensitive: a
// tick that fires ten seconds late is indistinguishable from one that fires on time
// for every workload a cron schedule is appropriate for.
func DefaultConfig() Config {
	return Config{
		Interval:             15 * time.Second,
		BatchSize:            20,
		StalledSweepInterval: 5 * time.Minute,
		MaxCatchupPerPass:    10,
	}
}

// Service fires schedules.
type Service struct {
	db   store.GWDB
	runs RunCreator
	cfg  Config
	log  *slog.Logger
	// now is injectable so tests can drive time rather than sleep through it.
	now func() time.Time
}

// New builds the service.
func New(db store.GWDB, runs RunCreator, cfg Config, log *slog.Logger) *Service {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultConfig().Interval
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultConfig().BatchSize
	}
	if cfg.StalledSweepInterval <= 0 {
		cfg.StalledSweepInterval = DefaultConfig().StalledSweepInterval
	}
	if cfg.MaxCatchupPerPass <= 0 {
		cfg.MaxCatchupPerPass = DefaultConfig().MaxCatchupPerPass
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, runs: runs, cfg: cfg, log: log, now: time.Now}
}

// Run polls until the context is cancelled.
//
// Returns nil on cancellation: a graceful shutdown is a successful outcome, and
// reporting it as an error makes a clean deploy look like a failure in the logs.
func (s *Service) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	sweep := time.NewTicker(s.cfg.StalledSweepInterval)
	defer sweep.Stop()

	s.log.InfoContext(ctx, "schedule fire loop started",
		"interval", s.cfg.Interval, "batch_size", s.cfg.BatchSize)

	for {
		select {
		case <-ctx.Done():
			s.log.InfoContext(ctx, "schedule fire loop stopped")
			return nil
		case <-sweep.C:
			if n, err := s.SweepStalled(ctx); err != nil {
				s.log.ErrorContext(ctx, "stalled-schedule sweep failed", "error", err)
			} else if n > 0 {
				// WARN, not INFO: reaching this means a replica died mid-fire, and
				// every schedule it held stopped firing until now.
				s.log.WarnContext(ctx, "re-armed schedules that were left unscheduled by a crashed replica",
					"count", n)
			}
		case <-tick.C:
			// An error is logged and retried next tick rather than stopping the
			// loop. This loop is the only thing that makes any schedule fire;
			// stopping it on a transient database error would turn a blip into an
			// outage in which nothing scheduled ever runs again.
			if _, err := s.FirePass(ctx); err != nil {
				s.log.ErrorContext(ctx, "schedule pass failed; retrying next tick", "error", err)
			}
		}
	}
}

// FirePass claims the due schedules and fires each. Exported so tests can drive it
// deterministically rather than waiting on wall-clock ticks.
func (s *Service) FirePass(ctx context.Context, namespaces ...string) (int, error) {
	now := s.now().UTC()
	due, err := store.WFMClaimDueSchedules(ctx, s.db, now, s.cfg.BatchSize, namespaces...)
	if err != nil {
		return 0, fmt.Errorf("claim due schedules: %w", err)
	}

	fired := 0
	for _, sch := range due {
		n, ferr := s.fire(ctx, sch, now)
		if ferr != nil {
			// One bad schedule must not stop the others. A schedule whose cron no
			// longer parses, or whose spec was deleted, is a configuration problem
			// for its owner -- not a reason the rest of the fleet stops firing.
			s.log.ErrorContext(ctx, "schedule fire failed",
				"schedule_id", sch.ID, "namespace", sch.Namespace, "name", sch.Name,
				"error", ferr)
		}
		fired += n
	}
	return fired, nil
}

// fire runs one claimed schedule's due ticks and re-arms it.
//
// The schedule arrives with next_fire_at already NULL (the claim did that), so this
// function OWNS re-arming it. Every return path must arm, which is why the arming
// is deferred rather than written at each exit: an early return that forgot would
// silently retire the schedule forever.
func (s *Service) fire(ctx context.Context, sch store.Schedule, now time.Time) (int, error) {
	expr, err := cron.Parse(sch.CronExpr)
	if err != nil {
		// Do NOT re-arm: an unparseable cron cannot produce a next fire time, and
		// guessing one would busy-loop the schedule. It stays unscheduled and the
		// stalled sweep will report it, which is the honest outcome -- the operator
		// has to fix the expression.
		return 0, fmt.Errorf("schedule %s/%s has an unparseable cron %q: %w",
			sch.Namespace, sch.Name, sch.CronExpr, err)
	}

	// Re-arm on every path. Computed from `now` rather than from the tick just
	// fired, so a schedule that was paused for a week resumes on the current grid
	// instead of marching through history one pass at a time.
	var armErr error
	defer func() {
		next, nerr := expr.Next(now)
		if nerr != nil {
			s.log.ErrorContext(ctx, "cannot compute the next fire time; schedule will stay unscheduled until fixed",
				"schedule_id", sch.ID, "cron", sch.CronExpr, "error", nerr)
			return
		}
		if aerr := store.WFMArmSchedule(ctx, s.db, sch.ID, next, nil); aerr != nil {
			armErr = aerr
			s.log.ErrorContext(ctx, "schedule fired but could not be re-armed; the stalled sweep will recover it",
				"schedule_id", sch.ID, "error", aerr)
		}
	}()

	ticks := s.dueTicks(expr, sch, now)
	if len(ticks) == 0 {
		return 0, armErr
	}

	fired := 0
	for _, tick := range ticks {
		ok, err := s.fireTick(ctx, sch, tick, nil)
		if err != nil {
			return fired, err
		}
		if ok {
			fired++
		}
	}
	return fired, armErr
}

// dueTicks lists the logical ticks this schedule owes, oldest first.
//
// Catch-up is bounded by catchup_limit (0 = only the current tick) AND by
// MaxCatchupPerPass. Two bounds because they guard different things: the schedule's
// limit is the operator's statement about whether missed work still matters, and the
// per-pass cap stops one schedule monopolising a pass. A five-minute schedule paused
// for a month has ~8,600 missed ticks, and firing them all at the moment someone
// un-pauses it would stampede the fleet with work nobody asked for.
func (s *Service) dueTicks(expr *cron.Schedule, sch store.Schedule, now time.Time) []time.Time {
	// With no catch-up, the tick that just came due is the only one that matters.
	if sch.CatchupLimit == 0 || sch.LastFireAt == nil {
		prev, ok := previousTick(expr, now)
		if !ok {
			return nil
		}
		return []time.Time{prev}
	}

	limit := int(sch.CatchupLimit)
	if limit > s.cfg.MaxCatchupPerPass {
		limit = s.cfg.MaxCatchupPerPass
	}

	var out []time.Time
	cursor := *sch.LastFireAt
	for len(out) < limit {
		next, err := expr.Next(cursor)
		if err != nil || next.After(now) {
			break
		}
		out = append(out, next)
		cursor = next
	}
	// If catch-up found nothing but a tick is genuinely due, fall back to it: a
	// schedule whose last_fire_at is ahead of the grid (clock change, manual edit)
	// must still fire rather than go quiet.
	if len(out) == 0 {
		if prev, ok := previousTick(expr, now); ok {
			out = append(out, prev)
		}
	}
	return out
}

// previousTick finds the most recent grid instant at or before now.
//
// cron.Next only moves forward, so the way to find the tick that just became due is
// to step back and ask for the next one. A day is enough of a window for any
// expression whose interval is shorter, and an expression with a longer interval has
// no missed tick to find in the first place.
func previousTick(expr *cron.Schedule, now time.Time) (time.Time, bool) {
	candidate, err := expr.Next(now.Add(-24 * time.Hour))
	if err != nil {
		return time.Time{}, false
	}
	var last time.Time
	found := false
	for !candidate.After(now) {
		last, found = candidate, true
		next, nerr := expr.Next(candidate)
		if nerr != nil {
			break
		}
		candidate = next
	}
	return last, found
}

// fireTick creates one run for one logical tick, honouring the overlap policy.
//
// Reports whether a run was created. A false with no error is the normal outcome of
// a skip or a redelivery, not a failure.
func (s *Service) fireTick(ctx context.Context, sch store.Schedule, tick time.Time, backfillID *string) (bool, error) {
	// Cheap pre-check before any work: a tick already fired is the common case on a
	// redelivery, and attempting the whole create just to have the claim refuse it
	// would create a run we then have to reason about.
	if exists, err := store.WFMScheduleTickExists(ctx, s.db, sch.ID, tick); err != nil {
		return false, err
	} else if exists {
		return false, nil
	}

	if backfillID == nil {
		allowed, reason, err := s.overlapAllows(ctx, sch)
		if err != nil {
			return false, err
		}
		if !allowed {
			// Logged rather than silently dropped: "my schedule did not run" is
			// otherwise unanswerable, and the reason is the answer.
			s.log.InfoContext(ctx, "schedule tick skipped by overlap policy",
				"schedule_id", sch.ID, "namespace", sch.Namespace, "name", sch.Name,
				"logical_at", tick.Format(time.RFC3339), "policy", sch.OverlapPolicy,
				"reason", reason)
			return false, nil
		}
	}

	triggerKind := TriggerSchedule
	if backfillID != nil {
		triggerKind = TriggerBackfill
	}
	detail := map[string]any{
		"schedule_id":   sch.ID,
		"schedule_name": sch.Name,
		"cron":          sch.CronExpr,
		"logical_at":    tick.Format(time.RFC3339),
	}
	if backfillID != nil {
		detail["backfill_id"] = *backfillID
	}

	// The idempotency key is (schedule, tick) so a retried create at the run layer
	// converges on the same run as the tick claim below. Both guards are wanted:
	// this one dedupes inside run creation, the claim dedupes across replicas.
	idem := fmt.Sprintf("schedule:%s:%s", sch.ID, tick.UTC().Format(time.RFC3339))

	runID, _, err := s.runs.Create(ctx, CreateRunRequest{
		Namespace:      sch.Namespace,
		SpecName:       sch.SpecName,
		SpecVersion:    sch.SpecVersion,
		IdempotencyKey: idem,
		TriggerKind:    triggerKind,
		TriggerDetail:  detail,
		Params:         sch.Params,
		LogicalDate:    tick,
		Priority:       sch.Priority,
	})
	if err != nil {
		return false, fmt.Errorf("create run for %s/%s tick %s: %w",
			sch.Namespace, sch.Name, tick.Format(time.RFC3339), err)
	}

	if err := store.WFMClaimScheduleTick(ctx, s.db, sch.ID, tick, runID, backfillID); err != nil {
		if errors.Is(err, store.ErrTickAlreadyFired) {
			// Another replica won the race. The run we just created is the SAME run
			// theirs converged on, because the idempotency key is the tick -- so
			// there is nothing to clean up and nothing went wrong.
			return false, nil
		}
		return false, err
	}

	s.log.InfoContext(ctx, "schedule fired a run",
		"schedule_id", sch.ID, "namespace", sch.Namespace, "name", sch.Name,
		"run_id", runID, "logical_at", tick.Format(time.RFC3339), "trigger", triggerKind)
	return true, nil
}

// overlapAllows applies the schedule's overlap policy.
func (s *Service) overlapAllows(ctx context.Context, sch store.Schedule) (bool, string, error) {
	if sch.OverlapPolicy == store.OverlapAllow {
		return true, "", nil
	}
	active, err := store.WFMActiveRunCountForSchedule(ctx, s.db, sch.ID)
	if err != nil {
		return false, "", err
	}
	if active == 0 {
		return true, "", nil
	}
	switch sch.OverlapPolicy {
	case store.OverlapQueueOne:
		// queue_one holds at most one pending fire. Implemented as "skip while
		// something is active" plus catch-up on the next pass: the tick is not lost
		// because last_fire_at has not advanced past it, so the next pass sees it
		// still owed. That is the difference from `skip`, which abandons it.
		return false, fmt.Sprintf("%d run(s) still active; this tick will be retried while it remains owed", active), nil
	default:
		return false, fmt.Sprintf("%d run(s) still active", active), nil
	}
}

// SweepStalled re-arms enabled schedules that have no next_fire_at.
//
// See WFMRearmStalledSchedules for why this is necessary rather than defensive: such
// a schedule looks active to an operator and never fires again.
func (s *Service) SweepStalled(ctx context.Context, namespaces ...string) (int, error) {
	stalled, err := store.WFMRearmStalledSchedules(ctx, s.db, s.cfg.BatchSize, namespaces...)
	if err != nil {
		return 0, fmt.Errorf("list stalled schedules: %w", err)
	}
	now := s.now().UTC()
	armed := 0
	for _, sch := range stalled {
		expr, perr := cron.Parse(sch.CronExpr)
		if perr != nil {
			// Left unscheduled deliberately: an unparseable cron has no next fire
			// time, and the schedule is reported every sweep until someone fixes it.
			s.log.WarnContext(ctx, "stalled schedule has an unparseable cron and cannot be re-armed",
				"schedule_id", sch.ID, "namespace", sch.Namespace, "name", sch.Name,
				"cron", sch.CronExpr, "error", perr)
			continue
		}
		next, nerr := expr.Next(now)
		if nerr != nil {
			s.log.WarnContext(ctx, "stalled schedule has no reachable fire time",
				"schedule_id", sch.ID, "cron", sch.CronExpr, "error", nerr)
			continue
		}
		if aerr := store.WFMArmSchedule(ctx, s.db, sch.ID, next, nil); aerr != nil {
			s.log.ErrorContext(ctx, "could not re-arm a stalled schedule", "schedule_id", sch.ID, "error", aerr)
			continue
		}
		armed++
	}
	return armed, nil
}
