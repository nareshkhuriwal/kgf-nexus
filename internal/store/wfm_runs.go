package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// workflow_runs is OWNED by workflow-manager (ADR-0011).

// ErrRunStateConflict means the run was not in the expected state, so the
// requested transition was refused. Surfaced as 409 rather than retried
// silently: an operator who clicks pause on a run that just failed deserves to
// be told it did not apply.
var ErrRunStateConflict = errors.New("run is not in an expected state for this transition")

// ErrMaxActiveRuns means the workflow's concurrency cap is already met.
var ErrMaxActiveRuns = errors.New("max_active_runs reached for this workflow")

// CreateRunParams is the input to run creation.
type CreateRunParams struct {
	SpecID         string
	Namespace      string
	Priority       int32
	IdempotencyKey string // empty => unconstrained, matching the partial unique index
	TriggerKind    string
	TriggerDetail  []byte
	Params         []byte
	LogicalDate    *time.Time
	// RunTimeoutMS becomes an absolute deadline_at computed from DATABASE time,
	// not from this process's clock. With several replicas, per-process clocks
	// disagree; one clock keeps every deadline comparable (invariant 7).
	RunTimeoutMS int64
	// MaxActiveRuns of 0 means unlimited.
	MaxActiveRuns int32
}

// WFMCreateRun creates a run, deduplicating on the idempotency key and enforcing
// the workflow's max_active_runs cap.
//
// Both checks live in ONE transaction with the insert. The cap in particular is
// meaningless otherwise: read-count-then-insert lets N concurrent creations each
// observe N-1 active runs and all proceed, which is precisely the stampede the
// cap exists to prevent. Serializing on the spec's row makes the count and the
// insert atomic without a global lock.
//
// The `created` return distinguishes a new run from an idempotent replay.
func WFMCreateRun(ctx context.Context, db GWDB, p CreateRunParams) (row RunRow, created bool, err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return RunRow{}, false, fmt.Errorf("begin create run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if p.MaxActiveRuns > 0 {
		// Advisory lock keyed on the spec, held for the transaction. This
		// serializes only concurrent creations OF THE SAME WORKFLOW; unrelated
		// workflows never contend, so the cap costs nothing globally.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "nexus:run-create:"+p.SpecID); err != nil {
			return RunRow{}, false, fmt.Errorf("lock for max_active_runs: %w", err)
		}
		var active int32
		// Counted across ALL versions of the workflow name, not just this
		// spec_id: max_active_runs is a property of the workflow, and a cap that
		// resets on every spec edit would not be a cap at all.
		if err := tx.QueryRow(ctx, `
			SELECT count(*)::int
			  FROM workflow_runs r
			  JOIN workflow_specs s ON s.id = r.spec_id
			 WHERE s.namespace = (SELECT namespace FROM workflow_specs WHERE id = $1)
			   AND s.name      = (SELECT name      FROM workflow_specs WHERE id = $1)
			   AND r.state IN ('PENDING','RUNNING','PAUSING','PAUSED','CANCELLING')`,
			p.SpecID,
		).Scan(&active); err != nil {
			return RunRow{}, false, fmt.Errorf("count active runs: %w", err)
		}
		if active >= p.MaxActiveRuns {
			return RunRow{}, false, fmt.Errorf("%w: %d active, limit %d", ErrMaxActiveRuns, active, p.MaxActiveRuns)
		}
	}

	var idem *string
	if p.IdempotencyKey != "" {
		idem = &p.IdempotencyKey
	}
	var id string
	insertSQL := `
		INSERT INTO workflow_runs
		       (spec_id, namespace, priority, idempotency_key, trigger_kind,
		        trigger_detail, params, logical_date, deadline_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8,
		        CASE WHEN $9::bigint > 0
		             THEN now() + make_interval(secs => $9::bigint / 1000.0)
		             ELSE NULL END)
		RETURNING id::text`
	err = tx.QueryRow(ctx, insertSQL,
		p.SpecID, p.Namespace, p.Priority, idem, p.TriggerKind,
		jsonOrEmpty(p.TriggerDetail), jsonOrEmpty(p.Params), p.LogicalDate, p.RunTimeoutMS,
	).Scan(&id)

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && idem != nil {
		// uq_runs_idem fired: this exact (namespace, idempotency_key) already
		// created a run. Returning the existing run is the whole point — "submit
		// the same run twice" is safe at the outermost layer.
		_ = tx.Rollback(ctx)
		existing, getErr := WFMGetRunByIdempotencyKey(ctx, db, p.Namespace, p.IdempotencyKey)
		if getErr != nil {
			return RunRow{}, false, fmt.Errorf("run exists for idempotency key but re-read failed: %w", getErr)
		}
		return existing, false, nil
	}
	if err != nil {
		return RunRow{}, false, fmt.Errorf("insert run: %w", err)
	}
	r, err := scanRun(tx.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return RunRow{}, false, fmt.Errorf("read back created run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RunRow{}, false, fmt.Errorf("commit create run: %w", err)
	}
	return r, true, nil
}

func jsonOrEmpty(b []byte) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
}

// WFMGetRunByIdempotencyKey resolves a prior submission.
func WFMGetRunByIdempotencyKey(ctx context.Context, q GWQuerier, namespace, key string) (RunRow, error) {
	r, err := scanRun(q.QueryRow(ctx, runSelect+` WHERE r.namespace = $1 AND r.idempotency_key = $2`, namespace, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, ErrNotFound
	}
	if err != nil {
		return RunRow{}, fmt.Errorf("get run by idempotency key: %w", err)
	}
	return r, nil
}

// WFMTransitionRun applies a state change with a compare-and-set on the CURRENT
// state, returning the updated row.
//
// The CAS (`state = ANY(from)`) rather than a blind UPDATE is what makes the
// §5.2 machine real under concurrency: the timer reconciler, an operator's cancel,
// and the DAG engine's completion signal can all arrive within the same
// millisecond, and only one may win. A blind UPDATE would let a run deadline
// overwrite a CANCELLED state, resurrecting a run an operator already stopped.
//
// state_version increments on every accepted transition, giving clients a cheap
// way to detect that their view is stale.
func WFMTransitionRun(ctx context.Context, q GWQuerier, runID string, from []string, to string) (RunRow, error) {
	var id string
	err := q.QueryRow(ctx, `
		UPDATE workflow_runs
		   SET state = $3::run_state,
		       state_version = state_version + 1,
		       -- started_at is set once, on the first entry into RUNNING; a resume
		       -- must not rewrite it or the run's wall-clock duration silently
		       -- shrinks every time it is paused.
		       started_at = CASE WHEN $3 = 'RUNNING' AND started_at IS NULL
		                         THEN now() ELSE started_at END,
		       ended_at   = CASE WHEN $3 IN ('SUCCEEDED','FAILED','CANCELLED','TIMED_OUT')
		                         THEN now() ELSE ended_at END
		 WHERE id = $1
		   AND state::text = ANY($2)
		RETURNING id::text`,
		runID, from, to,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, ErrRunStateConflict
	}
	if err != nil {
		return RunRow{}, fmt.Errorf("transition run %s to %s: %w", runID, to, err)
	}

	r, err := scanRun(q.QueryRow(ctx, runSelect+` WHERE r.id = $1`, id))
	if err != nil {
		return RunRow{}, fmt.Errorf("read back transitioned run: %w", err)
	}
	return r, nil
}

// WFMCountRunAttemptsByState counts a run's attempts grouped by state. Used to
// decide whether a PAUSING run has finished draining or a CANCELLING run has been
// fully acked: both are "are there any in-flight attempts left" questions.
func WFMCountRunAttemptsByState(ctx context.Context, q GWQuerier, runID string) (map[string]int, error) {
	rows, err := q.Query(ctx, `
		SELECT state::text, count(*)::int
		  FROM task_attempts
		 WHERE run_id = $1
		 GROUP BY state`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("count run attempts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("scan attempt state count: %w", err)
		}
		out[state] = n
	}
	return out, rows.Err()
}

// WFMCountRunNodesByState is the node-level equivalent, used to decide run
// terminality: SUCCEEDED requires every node terminal with none failed.
func WFMCountRunNodesByState(ctx context.Context, q GWQuerier, runID string) (map[string]int, error) {
	rows, err := q.Query(ctx, `
		SELECT state::text, count(*)::int
		  FROM task_nodes
		 WHERE run_id = $1
		 GROUP BY state`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("count run nodes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("scan node state count: %w", err)
		}
		out[state] = n
	}
	return out, rows.Err()
}

// WFMListRunsNeedingDeadline finds active runs whose deadline_at has passed and
// which still have no run_deadline timer, so the reconciler can arm one.
//
// A run's deadline is a durable timer row rather than a process-local
// time.AfterFunc, because a control-plane restart must not silently extend every
// in-flight run's SLA to infinity.
func WFMListRunsNeedingDeadline(ctx context.Context, q GWQuerier, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id::text
		  FROM workflow_runs r
		 WHERE r.deadline_at IS NOT NULL
		   AND r.state IN ('PENDING','RUNNING','PAUSING','PAUSED')
		   AND NOT EXISTS (
		         SELECT 1 FROM timers t
		          WHERE t.subject_id = r.id AND t.kind = 'run_deadline')
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list runs needing deadline timer: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan run id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// WFMListStalledRuns finds runs that have stopped progressing and will never be
// looked at again: every node is terminal, nothing is in flight, and yet the RUN
// is still non-terminal.
//
// Such a run is unreachable from every direction. A reconcile happens only when
// an attempt reports or an operator intervenes, and a run with no live attempts
// will never produce either -- so the conclusion the engine already computed is
// simply never applied. Observed in the wild as a run sitting at PAUSING for
// seven hours with all three of its nodes terminal, and as the F67 case where a
// terminal publish was cancelled after the node work had committed.
//
// This is the BACKSTOP, not the fix. Each individual cause is worth fixing at its
// own call site and several have been (F62, F67, b97d1ac). What this adds is the
// guarantee those point fixes cannot give on their own: whatever the reason a run
// stops being reconciled, something eventually notices. A nonzero result is
// therefore a SIGNAL that an upstream path is broken, not routine housekeeping --
// callers should say so loudly rather than repairing quietly.
//
// minAge exists so this never races a healthy run. A run mid-transition can
// legitimately show every node terminal for the moment between the last attempt
// reporting and the engine publishing the outcome; sweeping that would replace a
// correct conclusion with a guessed one. Anything still in this state after a
// grace period is genuinely stuck.
func WFMListStalledRuns(ctx context.Context, q GWQuerier, minAge time.Duration, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.Query(ctx, `
		SELECT r.id::text
		  FROM workflow_runs r
		 WHERE r.state IN ('PENDING','RUNNING','PAUSING','CANCELLING')
		   -- Nothing is in flight. task_attempts is the scheduler's table but the
		   -- workflow-manager may READ any table (ADR-0011); it is writes that must
		   -- go through the owner, and this only reads.
		   AND NOT EXISTS (
		         SELECT 1 FROM task_attempts a
		          WHERE a.run_id = r.id
		            AND a.state IN ('PENDING','DISPATCHED','RUNNING'))
		   -- The DAG is materialized and every node has settled. The nodes check
		   -- must not be skipped: a run whose graph does not exist yet also has no
		   -- live attempts, and concluding THAT run terminal would kill it before
		   -- it ever started.
		   AND EXISTS (SELECT 1 FROM task_nodes n WHERE n.run_id = r.id)
		   AND NOT EXISTS (
		         SELECT 1 FROM task_nodes n
		          WHERE n.run_id = r.id
		            AND n.state NOT IN
		                ('SUCCEEDED','FAILED','SKIPPED','UPSTREAM_FAILED','CANCELLED'))
		   -- Age from the last thing that HAPPENED, which is the most recent
		   -- attempt ending. workflow_runs has no updated_at, and created_at is
		   -- the wrong clock: a long healthy run would look stale the whole time
		   -- it was working, while a run that stalls immediately after starting
		   -- would look fresh. COALESCE covers the run whose attempts were all
		   -- created but never ran.
		   AND COALESCE(
		         (SELECT max(a.ended_at) FROM task_attempts a WHERE a.run_id = r.id),
		         r.started_at, r.created_at) < now() - $1::interval
		 ORDER BY r.created_at
		 LIMIT $2`,
		minAge.String(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list stalled runs: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan stalled run id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// WFMGetRunForUpdate loads a run's state and deadline inside the caller's
// transaction, taking a row lock so a concurrent transition cannot interleave.
func WFMGetRunForUpdate(ctx context.Context, q GWQuerier, runID string) (state string, namespace string, deadline *time.Time, err error) {
	err = q.QueryRow(ctx, `
		SELECT state::text, namespace, deadline_at
		  FROM workflow_runs
		 WHERE id = $1
		 FOR UPDATE`,
		runID,
	).Scan(&state, &namespace, &deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil, ErrNotFound
	}
	if err != nil {
		return "", "", nil, fmt.Errorf("lock run %s: %w", runID, err)
	}
	return state, namespace, deadline, nil
}

// WFMGetRun loads a run WITHOUT a namespace predicate.
//
// Deliberately distinct from GatewayGetRun, which always scopes by namespace so a
// caller authenticated for one tenant cannot read another's run by guessing a
// UUID. This one is for INTERNAL callers that already hold the run id from their
// own prior write and have no principal to scope against; exposing it on the REST
// path would defeat the tenancy boundary, so the two are separate functions rather
// than one with an optional argument.
func WFMGetRun(ctx context.Context, q GWQuerier, runID string) (RunRow, error) {
	r, err := scanRun(q.QueryRow(ctx, runSelect+` WHERE r.id = $1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, ErrNotFound
	}
	if err != nil {
		return RunRow{}, fmt.Errorf("get run %s: %w", runID, err)
	}
	return r, nil
}
