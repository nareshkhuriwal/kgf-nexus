package store

import (
	"context"
	"fmt"
	"time"
)

// timers is OWNED by workflow-manager (ADR-0011). EVERY deadline in the system is
// a row here, so a control-plane restart loses nothing — the failure mode of
// in-memory timer wheels (architecture §3.2).

// Timer is one durable deadline.
type Timer struct {
	ID        string
	Kind      string
	SubjectID string
	FireAt    time.Time
	Payload   []byte
}

// WFMArmTimer inserts a deadline.
//
// fire_at is computed by the DATABASE from now() plus the caller's offset rather
// than by the caller, so every deadline in the table is on one clock. With N
// workflow-manager replicas, per-process clock skew would otherwise make two
// timers armed "30s from now" fire meaningfully apart (invariant 7).
func WFMArmTimer(ctx context.Context, q GWQuerier, kind, subjectID string, after time.Duration, payload []byte) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO timers (kind, subject_id, fire_at, payload)
		VALUES ($1, $2, now() + make_interval(secs => $3), $4::jsonb)
		RETURNING id::text`,
		kind, subjectID, after.Seconds(), jsonOrEmpty(payload),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("arm %s timer for %s: %w", kind, subjectID, err)
	}
	return id, nil
}

// WFMArmTimerAt inserts a deadline at an ABSOLUTE instant, for the case where the
// deadline was already computed and stored by the database (a run's deadline_at).
func WFMArmTimerAt(ctx context.Context, q GWQuerier, kind, subjectID string, fireAt time.Time, payload []byte) (string, error) {
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO timers (kind, subject_id, fire_at, payload)
		VALUES ($1, $2, $3, $4::jsonb)
		RETURNING id::text`,
		kind, subjectID, fireAt, jsonOrEmpty(payload),
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("arm %s timer at instant for %s: %w", kind, subjectID, err)
	}
	return id, nil
}

// WFMClaimDueTimers claims a batch of fired timers for this reconciler instance.
//
// FOR UPDATE SKIP LOCKED is the whole mechanism behind running N reconciler
// replicas with no leader election: each transaction takes row locks on the rows
// it claims and SKIPs rows another replica already holds, so two replicas never
// process the same timer and neither blocks the other. Without SKIP LOCKED the
// replicas would serialize on the same head-of-queue rows and the throughput of
// N replicas would equal the throughput of one.
//
// The claim marks claimed_by/claimed_at rather than deleting, so a crash between
// claim and dispatch leaves an auditable record; WFMReleaseStaleTimerClaims
// recovers those.
// kinds restricts the claim to the timer kinds the caller can actually apply. An
// empty slice means "every kind", which is what the tests and any single-reconciler
// deployment want.
//
// Filtering matters because `timers` has TWO claimants: this reconciler and the
// scheduler's own timer loop, which natively handles retry_backoff and
// schedule_to_start. Both used to claim every due row, so whichever won the race
// took the timer — and when this service won a retry_backoff it could only fail,
// because applying it means writing task_attempts, which the scheduler owns. The
// timer then sat claimed until the stale-claim sweeper released it, and the retry
// the author asked for never materialized. Claiming only what we can apply leaves
// the owner's rows for the owner (ADR-0011).
func WFMClaimDueTimers(ctx context.Context, db GWDB, claimant string, limit int, kinds ...string) ([]Timer, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim timers: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// $3 = NULL selects every kind, so one query serves both the filtered and the
	// unfiltered caller without a second code path to keep in step.
	var kindFilter any
	if len(kinds) > 0 {
		kindFilter = kinds
	}
	rows, err := tx.Query(ctx, `
		WITH due AS (
		    SELECT id
		      FROM timers
		     WHERE fire_at <= now()
		       AND claimed_by IS NULL
		       AND ($3::text[] IS NULL OR kind = ANY($3::text[]))
		     ORDER BY fire_at
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		)
		UPDATE timers t
		   SET claimed_by = $1, claimed_at = now()
		  FROM due
		 WHERE t.id = due.id
		RETURNING t.id::text, t.kind, t.subject_id::text, t.fire_at, t.payload::text`,
		claimant, limit, kindFilter,
	)
	if err != nil {
		return nil, fmt.Errorf("claim due timers: %w", err)
	}

	var out []Timer
	for rows.Next() {
		var t Timer
		if err := rows.Scan(&t.ID, &t.Kind, &t.SubjectID, &t.FireAt, &t.Payload); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claimed timer: %w", err)
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed timers: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim timers: %w", err)
	}
	return out, nil
}

// WFMDeleteTimer removes a timer that has been fully handled.
func WFMDeleteTimer(ctx context.Context, q GWQuerier, timerID string) error {
	if _, err := q.Exec(ctx, `DELETE FROM timers WHERE id = $1`, timerID); err != nil {
		return fmt.Errorf("delete timer %s: %w", timerID, err)
	}
	return nil
}

// WFMCancelTimers removes still-pending timers for a subject.
//
// Called when the subject reaches a state that makes the deadline moot: a
// cancelled run needs no run_deadline, and an attempt that reported success needs
// no start_to_close. Leaving them armed is not merely untidy — a later fire would
// try to transition a terminal subject, and the CAS would reject it, generating
// confusing noise on a path operators read during incidents.
func WFMCancelTimers(ctx context.Context, q GWQuerier, subjectID string, kinds ...string) (int64, error) {
	if len(kinds) == 0 {
		tag, err := q.Exec(ctx, `DELETE FROM timers WHERE subject_id = $1 AND claimed_by IS NULL`, subjectID)
		if err != nil {
			return 0, fmt.Errorf("cancel all timers for %s: %w", subjectID, err)
		}
		return tag.RowsAffected(), nil
	}
	tag, err := q.Exec(ctx, `
		DELETE FROM timers
		 WHERE subject_id = $1 AND kind = ANY($2) AND claimed_by IS NULL`,
		subjectID, kinds,
	)
	if err != nil {
		return 0, fmt.Errorf("cancel timers for %s: %w", subjectID, err)
	}
	return tag.RowsAffected(), nil
}

// WFMReleaseStaleTimerClaims un-claims timers whose claimant died mid-dispatch.
//
// This is the counterpart to claiming rather than deleting: a reconciler that
// crashes after claiming would otherwise strand its batch forever, and the
// deadlines in it would never fire — silently, which is the worst way for a
// deadline system to fail. Returning them to the pool after a grace period makes
// the loss self-healing at the cost of at-least-once timer delivery, which every
// handler here is written to tolerate.
func WFMReleaseStaleTimerClaims(ctx context.Context, q GWQuerier, olderThan time.Duration) (int64, error) {
	tag, err := q.Exec(ctx, `
		UPDATE timers
		   SET claimed_by = NULL, claimed_at = NULL
		 WHERE claimed_by IS NOT NULL
		   AND claimed_at < now() - make_interval(secs => $1)`,
		olderThan.Seconds(),
	)
	if err != nil {
		return 0, fmt.Errorf("release stale timer claims: %w", err)
	}
	return tag.RowsAffected(), nil
}

// WFMCountPendingTimers reports the unclaimed backlog, exposed for readiness and
// metrics. A growing backlog means deadlines are firing late, which is a silent
// correctness problem rather than a mere performance one.
func WFMCountPendingTimers(ctx context.Context, q GWQuerier) (due int64, total int64, err error) {
	err = q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE fire_at <= now() AND claimed_by IS NULL),
		       count(*)
		  FROM timers`,
	).Scan(&due, &total)
	if err != nil {
		return 0, 0, fmt.Errorf("count pending timers: %w", err)
	}
	return due, total, nil
}
