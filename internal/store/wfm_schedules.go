package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"strings"

	"github.com/jackc/pgx/v5"
)

// schedules, schedule_backfills and schedule_runs are OWNED by workflow-manager
// (ADR-0011, migration 0006). It already owns workflow_specs, workflow_runs and
// timers, and a schedule's whole job is creating runs from a spec on a timer.

// ErrScheduleNotFound is returned when no schedule matches.
var ErrScheduleNotFound = errors.New("schedule not found")

// ErrTickAlreadyFired means this (schedule, logical tick) already produced a run.
//
// Not an error condition so much as the deduplication working: timers are
// at-least-once, so a redelivered fire MUST find the tick already claimed rather
// than create a second run for it.
var ErrTickAlreadyFired = errors.New("schedule tick already fired")

// Schedule is a standing intention to create runs.
type Schedule struct {
	ID            string
	Namespace     string
	Name          string
	SpecName      string
	SpecVersion   *int32
	CronExpr      string
	Timezone      string
	Params        map[string]any
	Priority      int32
	OverlapPolicy string
	CatchupLimit  int32
	Enabled       bool
	NextFireAt    *time.Time
	LastFireAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CreatedBy     string
}

// Overlap policies. Stored as text rather than an enum type so adding one is a
// CHECK-constraint change rather than an ALTER TYPE, which ADR-0011 rule 5 treats
// as a blocking migration.
const (
	OverlapSkip     = "skip"
	OverlapAllow    = "allow"
	OverlapQueueOne = "queue_one"
)

// Backfill states.
const (
	BackfillRunning   = "RUNNING"
	BackfillPaused    = "PAUSED"
	BackfillCompleted = "COMPLETED"
	BackfillCancelled = "CANCELLED"
	BackfillFailed    = "FAILED"
)

// Backfill is a bounded historical catch-up job.
type Backfill struct {
	ID          string
	ScheduleID  string
	Namespace   string
	FromTime    time.Time
	ToTime      time.Time
	CursorAt    *time.Time
	MaxActive   int32
	MaxRuns     int32
	RunsCreated int32
	State       string
	Detail      map[string]any
	CreatedAt   time.Time
	UpdatedAt   time.Time
	EndedAt     *time.Time
	CreatedBy   string
}

const scheduleCols = `id::text, namespace, name, spec_name, spec_version, cron_expr,
	timezone, params, priority, overlap_policy, catchup_limit, enabled,
	next_fire_at, last_fire_at, created_at, updated_at, created_by`

func scanSchedule(row pgx.Row) (Schedule, error) {
	var s Schedule
	var params []byte
	if err := row.Scan(&s.ID, &s.Namespace, &s.Name, &s.SpecName, &s.SpecVersion,
		&s.CronExpr, &s.Timezone, &params, &s.Priority, &s.OverlapPolicy,
		&s.CatchupLimit, &s.Enabled, &s.NextFireAt, &s.LastFireAt,
		&s.CreatedAt, &s.UpdatedAt, &s.CreatedBy); err != nil {
		return Schedule{}, err
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &s.Params)
	}
	return s, nil
}

// WFMCreateSchedule inserts a schedule.
//
// nextFireAt is computed by the caller, because the cron arithmetic lives in
// internal/cron and the store deliberately holds no schedule semantics — it would
// otherwise be the second place that decides when a schedule is due.
func WFMCreateSchedule(ctx context.Context, db GWDB, s Schedule, nextFireAt time.Time) (Schedule, error) {
	params, err := json.Marshal(emptyIfNil(s.Params))
	if err != nil {
		return Schedule{}, fmt.Errorf("encode schedule params: %w", err)
	}
	row := db.QueryRow(ctx, `
		INSERT INTO schedules (namespace, name, spec_name, spec_version, cron_expr,
		                       timezone, params, priority, overlap_policy,
		                       catchup_limit, enabled, next_fire_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+scheduleCols,
		s.Namespace, s.Name, s.SpecName, s.SpecVersion, s.CronExpr,
		orDefault(s.Timezone, "UTC"), params, s.Priority,
		orDefault(s.OverlapPolicy, OverlapSkip), s.CatchupLimit, s.Enabled,
		nextFireAt, s.CreatedBy)
	out, err := scanSchedule(row)
	if err != nil {
		return Schedule{}, fmt.Errorf("insert schedule %s/%s: %w", s.Namespace, s.Name, err)
	}
	return out, nil
}

// WFMGetSchedule reads one schedule by namespace and name.
func WFMGetSchedule(ctx context.Context, db GWDB, namespace, name string) (Schedule, error) {
	s, err := scanSchedule(db.QueryRow(ctx,
		`SELECT `+scheduleCols+` FROM schedules WHERE namespace = $1 AND name = $2`,
		namespace, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, ErrScheduleNotFound
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("get schedule %s/%s: %w", namespace, name, err)
	}
	return s, nil
}

// WFMGetScheduleByID reads one schedule by id, for the timer handler.
func WFMGetScheduleByID(ctx context.Context, db GWDB, id string) (Schedule, error) {
	s, err := scanSchedule(db.QueryRow(ctx,
		`SELECT `+scheduleCols+` FROM schedules WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, ErrScheduleNotFound
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("get schedule %s: %w", id, err)
	}
	return s, nil
}

// WFMListSchedules lists a namespace's schedules, newest first.
func WFMListSchedules(ctx context.Context, db GWDB, namespace string, limit int) ([]Schedule, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := db.Query(ctx,
		`SELECT `+scheduleCols+` FROM schedules WHERE namespace = $1
		 ORDER BY created_at DESC LIMIT $2`, namespace, limit)
	if err != nil {
		return nil, fmt.Errorf("list schedules in %s: %w", namespace, err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, serr := scanSchedule(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan schedule: %w", serr)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// WFMSetScheduleEnabled enables or disables a schedule.
//
// Disabling CLEARS next_fire_at, which is what takes the schedule out of the due
// index: leaving a stale next_fire_at on a disabled row would mean every due query
// keeps finding it and every fire has to re-check `enabled`. Re-enabling is the
// caller's cue to recompute, since a schedule disabled for a week has a
// next_fire_at deep in the past.
func WFMSetScheduleEnabled(ctx context.Context, db GWDB, namespace, name string, enabled bool, nextFireAt *time.Time) (Schedule, error) {
	s, err := scanSchedule(db.QueryRow(ctx, `
		UPDATE schedules
		   SET enabled = $3,
		       next_fire_at = CASE WHEN $3 THEN $4::timestamptz ELSE NULL END,
		       updated_at = now()
		 WHERE namespace = $1 AND name = $2
		RETURNING `+scheduleCols, namespace, name, enabled, nextFireAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return Schedule{}, ErrScheduleNotFound
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("set schedule %s/%s enabled=%v: %w", namespace, name, enabled, err)
	}
	return s, nil
}

// WFMDeleteSchedule removes a schedule. Its schedule_runs rows cascade, which is
// deliberate: they exist to deduplicate ticks for a schedule that still exists.
func WFMDeleteSchedule(ctx context.Context, db GWDB, namespace, name string) error {
	tag, err := db.Exec(ctx, `DELETE FROM schedules WHERE namespace = $1 AND name = $2`, namespace, name)
	if err != nil {
		return fmt.Errorf("delete schedule %s/%s: %w", namespace, name, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrScheduleNotFound
	}
	return nil
}

// WFMClaimDueSchedules atomically claims schedules whose next_fire_at has passed.
//
// The claim is the same trick the timer wheel uses and for the same reason: several
// workflow-manager replicas run this loop with no leader election, so FOR UPDATE
// SKIP LOCKED lets each take a disjoint set and neither block the other. Without
// SKIP LOCKED they would serialize on the same head-of-queue rows and N replicas
// would have the throughput of one.
//
// Claiming here means NULLing next_fire_at, so a claimed schedule leaves the due
// index immediately and cannot be claimed twice. The handler is then responsible
// for setting the next one — and if it crashes first, the schedule stops firing
// until something recomputes it, which is why WFMRearmStalledSchedules exists.
// namespaces optionally restricts the claim. Empty means every namespace, which is
// what a normal deployment wants: any replica may fire any schedule, and narrowing
// would leave a namespace unserved if the replica that owned it went away. The
// filter exists so a deployment CAN shard deliberately (a namespace with a huge
// schedule count, or one that must not share a failure domain), and so parallel
// tests can claim without stealing each other's rows.
func WFMClaimDueSchedules(ctx context.Context, db GWDB, now time.Time, limit int, namespaces ...string) ([]Schedule, error) {
	if limit <= 0 {
		limit = 20
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim schedules: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// $3 = NULL selects every namespace, so one query serves both callers without a
	// second code path to keep in step.
	var nsFilter any
	if len(namespaces) > 0 {
		nsFilter = namespaces
	}
	rows, err := tx.Query(ctx, `
		WITH due AS (
		    SELECT id FROM schedules
		     WHERE enabled AND next_fire_at IS NOT NULL AND next_fire_at <= $1
		       AND ($3::text[] IS NULL OR namespace = ANY($3::text[]))
		     ORDER BY next_fire_at
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		)
		UPDATE schedules s
		   SET next_fire_at = NULL, updated_at = now()
		  FROM due
		 WHERE s.id = due.id
		RETURNING `+prefixCols("s.", scheduleCols), now, limit, nsFilter)
	if err != nil {
		return nil, fmt.Errorf("claim due schedules: %w", err)
	}

	var out []Schedule
	for rows.Next() {
		s, serr := scanSchedule(rows)
		if serr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claimed schedule: %w", serr)
		}
		out = append(out, s)
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate claimed schedules: %w", rerr)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit schedule claim: %w", err)
	}
	return out, nil
}

// WFMArmSchedule sets a schedule's next fire time and records the tick it last
// fired, in one write so the two cannot disagree.
func WFMArmSchedule(ctx context.Context, db GWDB, id string, nextFireAt time.Time, lastFireAt *time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE schedules
		   SET next_fire_at = $2,
		       last_fire_at = COALESCE($3, last_fire_at),
		       updated_at = now()
		 WHERE id = $1::uuid`, id, nextFireAt, lastFireAt)
	if err != nil {
		return fmt.Errorf("arm schedule %s: %w", id, err)
	}
	return nil
}

// WFMRearmStalledSchedules recovers enabled schedules that have no next_fire_at.
//
// A schedule is left in that state by a replica that claimed it and then died
// before arming the next tick. Nothing else would ever notice: the row is enabled,
// so an operator sees an active schedule, but it is absent from the due index, so
// it never fires again. That is the worst shape of failure this feature can have —
// silent and permanent — so it gets an explicit sweep rather than relying on a
// human to spot a schedule that stopped.
func WFMRearmStalledSchedules(ctx context.Context, db GWDB, limit int, namespaces ...string) ([]Schedule, error) {
	if limit <= 0 {
		limit = 20
	}
	var nsFilter any
	if len(namespaces) > 0 {
		nsFilter = namespaces
	}
	rows, err := db.Query(ctx,
		`SELECT `+scheduleCols+` FROM schedules
		  WHERE enabled AND next_fire_at IS NULL
		    AND ($2::text[] IS NULL OR namespace = ANY($2::text[]))
		  ORDER BY updated_at
		  LIMIT $1`, limit, nsFilter)
	if err != nil {
		return nil, fmt.Errorf("list stalled schedules: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		s, serr := scanSchedule(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan stalled schedule: %w", serr)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// WFMActiveRunCountForSchedule counts this schedule's runs that are not terminal.
//
// The overlap policy's input. Joined through schedule_runs rather than filtering
// workflow_runs on trigger_detail, because a JSONB predicate cannot use an index
// and this runs on every fire.
func WFMActiveRunCountForSchedule(ctx context.Context, db GWDB, scheduleID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM schedule_runs sr
		  JOIN workflow_runs r ON r.id = sr.run_id
		 WHERE sr.schedule_id = $1::uuid
		   AND r.state IN ('PENDING','RUNNING','PAUSING','PAUSED','CANCELLING')`,
		scheduleID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active runs for schedule %s: %w", scheduleID, err)
	}
	return n, nil
}

// WFMClaimScheduleTick reserves one (schedule, logical tick) before a run exists.
//
// This is the deduplication that makes at-least-once timer delivery safe. It is
// deliberately a SEPARATE step from creating the run, and it comes FIRST: if the
// run were created first and the claim second, a crash between them would leave an
// orphan run that the next delivery would duplicate. Claiming first means a crash
// leaves a claim with no run, which the sweeper can see and resolve — an unused
// claim is recoverable, a duplicated production run is not.
//
// Returns ErrTickAlreadyFired when another replica or an earlier delivery has it.
func WFMClaimScheduleTick(ctx context.Context, db GWDB, scheduleID string, logicalAt time.Time, runID string, backfillID *string) error {
	tag, err := db.Exec(ctx, `
		INSERT INTO schedule_runs (schedule_id, logical_at, run_id, backfill_id)
		VALUES ($1::uuid, $2, $3::uuid, $4::uuid)
		ON CONFLICT (schedule_id, logical_at) DO NOTHING`,
		scheduleID, logicalAt, runID, backfillID)
	if err != nil {
		return fmt.Errorf("claim tick %s@%s: %w", scheduleID, logicalAt.Format(time.RFC3339), err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTickAlreadyFired
	}
	return nil
}

// WFMScheduleTickExists reports whether a tick already produced a run, so a
// backfill can skip ahead without attempting an insert per tick.
func WFMScheduleTickExists(ctx context.Context, db GWDB, scheduleID string, logicalAt time.Time) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM schedule_runs WHERE schedule_id = $1::uuid AND logical_at = $2)`,
		scheduleID, logicalAt).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check tick %s@%s: %w", scheduleID, logicalAt.Format(time.RFC3339), err)
	}
	return exists, nil
}

// emptyIfNil keeps a NOT NULL jsonb column from receiving a SQL NULL.
func emptyIfNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// prefixCols qualifies a column list for a RETURNING clause on an aliased table.
func prefixCols(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = prefix + strings.TrimSpace(strings.ReplaceAll(p, "\n", " "))
	}
	return strings.Join(parts, ", ")
}

// ── backfills ───────────────────────────────────────────────────────────────

// ErrBackfillNotFound is returned when no backfill matches.
var ErrBackfillNotFound = errors.New("backfill not found")

const backfillCols = `id::text, schedule_id::text, namespace, from_time, to_time,
	cursor_at, max_active, max_runs, runs_created, state, detail,
	created_at, updated_at, ended_at, created_by`

func scanBackfill(row pgx.Row) (Backfill, error) {
	var b Backfill
	var detail []byte
	if err := row.Scan(&b.ID, &b.ScheduleID, &b.Namespace, &b.FromTime, &b.ToTime,
		&b.CursorAt, &b.MaxActive, &b.MaxRuns, &b.RunsCreated, &b.State, &detail,
		&b.CreatedAt, &b.UpdatedAt, &b.EndedAt, &b.CreatedBy); err != nil {
		return Backfill{}, err
	}
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &b.Detail)
	}
	return b, nil
}

// WFMCreateBackfill starts a backfill over [from, to).
//
// cursorAt is supplied by the caller as the FIRST tick to generate, not as from_time:
// from_time is what the operator asked for, and the first tick on the cron grid at or
// after it is usually later. Keeping them distinct means the request stays legible in
// the row after the cursor has moved on.
func WFMCreateBackfill(ctx context.Context, db GWDB, b Backfill, cursorAt time.Time) (Backfill, error) {
	detail, err := json.Marshal(emptyIfNil(b.Detail))
	if err != nil {
		return Backfill{}, fmt.Errorf("encode backfill detail: %w", err)
	}
	out, err := scanBackfill(db.QueryRow(ctx, `
		INSERT INTO schedule_backfills (schedule_id, namespace, from_time, to_time,
		                                cursor_at, max_active, max_runs, state, detail, created_by)
		VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING `+backfillCols,
		b.ScheduleID, b.Namespace, b.FromTime, b.ToTime, cursorAt,
		b.MaxActive, b.MaxRuns, BackfillRunning, detail, b.CreatedBy))
	if err != nil {
		return Backfill{}, fmt.Errorf("insert backfill for schedule %s: %w", b.ScheduleID, err)
	}
	return out, nil
}

// WFMGetBackfill reads one backfill by id, scoped to a namespace.
//
// Namespace-scoped because knowing a UUID must not grant cross-tenant read: the
// namespace is the tenancy boundary (§3.2).
func WFMGetBackfill(ctx context.Context, db GWDB, namespace, id string) (Backfill, error) {
	b, err := scanBackfill(db.QueryRow(ctx,
		`SELECT `+backfillCols+` FROM schedule_backfills WHERE id = $1::uuid AND namespace = $2`,
		id, namespace))
	if errors.Is(err, pgx.ErrNoRows) {
		return Backfill{}, ErrBackfillNotFound
	}
	if err != nil {
		return Backfill{}, fmt.Errorf("get backfill %s: %w", id, err)
	}
	return b, nil
}

// WFMListBackfills lists a schedule's backfills, newest first.
func WFMListBackfills(ctx context.Context, db GWDB, namespace, scheduleID string, limit int) ([]Backfill, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := db.Query(ctx,
		`SELECT `+backfillCols+` FROM schedule_backfills
		  WHERE namespace = $1 AND schedule_id = $2::uuid
		  ORDER BY created_at DESC LIMIT $3`, namespace, scheduleID, limit)
	if err != nil {
		return nil, fmt.Errorf("list backfills for schedule %s: %w", scheduleID, err)
	}
	defer rows.Close()

	var out []Backfill
	for rows.Next() {
		b, serr := scanBackfill(rows)
		if serr != nil {
			return nil, fmt.Errorf("scan backfill: %w", serr)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// WFMClaimRunningBackfills leases backfills to advance, one replica at a time each.
//
// A LEASE, not a row lock. The first version selected FOR UPDATE SKIP LOCKED on the
// pool and returned the rows -- which does nothing, because the lock is released the
// moment the query completes and the driver then spends seconds generating runs with
// no protection at all. Six concurrent passes produced ten runs against a max_active
// of four, and the concurrency cap §3.2 requires was simply not enforced.
//
// Holding a transaction open for the whole advance would be the other obvious fix and
// is worse: the driver creates runs through the run service, so the transaction would
// span an RPC per tick and hold a row lock for as long as that takes.
//
// So the claim STAMPS the row instead: claimed_by/claimed_at inside one short
// transaction, and only rows whose claim is absent or expired are claimable. The lease
// is a bounded promise rather than a lock, so a replica that dies mid-advance blocks
// its backfill for at most one lease period -- and because the cursor is persisted and
// tick creation is deduplicated, the next claimant simply resumes.
func WFMClaimRunningBackfills(ctx context.Context, db GWDB, claimant string, lease time.Duration, limit int, namespaces ...string) ([]Backfill, error) {
	if limit <= 0 {
		limit = 10
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	var nsFilter any
	if len(namespaces) > 0 {
		nsFilter = namespaces
	}

	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim backfills: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		WITH claimable AS (
		    SELECT id FROM schedule_backfills
		     WHERE state = $1
		       AND (claimed_at IS NULL OR claimed_at < now() - $2::interval)
		       AND ($5::text[] IS NULL OR namespace = ANY($5::text[]))
		     ORDER BY created_at
		     LIMIT $4
		     FOR UPDATE SKIP LOCKED
		)
		UPDATE schedule_backfills b
		   SET claimed_by = $3, claimed_at = now(), updated_at = now()
		  FROM claimable
		 WHERE b.id = claimable.id
		RETURNING `+prefixCols("b.", backfillCols),
		BackfillRunning, lease.String(), claimant, limit, nsFilter)
	if err != nil {
		return nil, fmt.Errorf("claim running backfills: %w", err)
	}

	var out []Backfill
	for rows.Next() {
		b, serr := scanBackfill(rows)
		if serr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claimed backfill: %w", serr)
		}
		out = append(out, b)
	}
	rows.Close()
	if rerr := rows.Err(); rerr != nil {
		return nil, fmt.Errorf("iterate claimed backfills: %w", rerr)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit backfill claim: %w", err)
	}
	return out, nil
}

// WFMReleaseBackfill drops a lease so the next pass can pick the backfill up at once
// rather than waiting out the lease period.
func WFMReleaseBackfill(ctx context.Context, db GWDB, id string) error {
	_, err := db.Exec(ctx,
		`UPDATE schedule_backfills SET claimed_by = NULL, claimed_at = NULL WHERE id = $1::uuid`, id)
	if err != nil {
		return fmt.Errorf("release backfill %s: %w", id, err)
	}
	return nil
}

// WFMAdvanceBackfill moves the cursor and counts the runs created.
//
// runsCreated is an increment rather than an absolute, so two replicas that somehow
// both advanced cannot clobber each other's count into being lower than the runs that
// actually exist -- which would let a backfill exceed max_runs.
func WFMAdvanceBackfill(ctx context.Context, db GWDB, id string, cursorAt *time.Time, runsCreated int) error {
	_, err := db.Exec(ctx, `
		UPDATE schedule_backfills
		   SET cursor_at = $2,
		       runs_created = runs_created + $3,
		       updated_at = now()
		 WHERE id = $1::uuid`, id, cursorAt, runsCreated)
	if err != nil {
		return fmt.Errorf("advance backfill %s: %w", id, err)
	}
	return nil
}

// WFMSetBackfillState moves a backfill to a terminal or paused state.
//
// ended_at is set only for the terminal states, so a paused backfill that is later
// resumed does not carry a misleading end time.
func WFMSetBackfillState(ctx context.Context, db GWDB, namespace, id, state string, detail map[string]any) (Backfill, error) {
	body, err := json.Marshal(emptyIfNil(detail))
	if err != nil {
		return Backfill{}, fmt.Errorf("encode backfill detail: %w", err)
	}
	terminal := state == BackfillCompleted || state == BackfillCancelled || state == BackfillFailed
	b, err := scanBackfill(db.QueryRow(ctx, `
		UPDATE schedule_backfills
		   SET state = $3,
		       detail = CASE WHEN $4::jsonb = '{}'::jsonb THEN detail ELSE $4::jsonb END,
		       ended_at = CASE WHEN $5 THEN now() ELSE NULL END,
		       updated_at = now()
		 WHERE id = $1::uuid AND namespace = $2
		RETURNING `+backfillCols, id, namespace, state, body, terminal))
	if errors.Is(err, pgx.ErrNoRows) {
		return Backfill{}, ErrBackfillNotFound
	}
	if err != nil {
		return Backfill{}, fmt.Errorf("set backfill %s state %s: %w", id, state, err)
	}
	return b, nil
}

// WFMBackfillActiveRunCount counts this backfill's runs that are not terminal.
//
// THE STARVATION GUARD. §3.2 requires a backfill to have "its own concurrency cap, so
// a backfill cannot starve live traffic", and this is its input: the driver generates
// a tick only while this is below max_active. Counted per BACKFILL rather than per
// schedule, because the live schedule's own runs must not consume the backfill's
// budget nor the reverse -- the whole point is that catching up three months runs
// slower than, and independently of, the live schedule.
func WFMBackfillActiveRunCount(ctx context.Context, db GWDB, backfillID string) (int, error) {
	var n int
	err := db.QueryRow(ctx, `
		SELECT count(*)::int
		  FROM schedule_runs sr
		  JOIN workflow_runs r ON r.id = sr.run_id
		 WHERE sr.backfill_id = $1::uuid
		   AND r.state IN ('PENDING','RUNNING','PAUSING','PAUSED','CANCELLING')`,
		backfillID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count active runs for backfill %s: %w", backfillID, err)
	}
	return n, nil
}
