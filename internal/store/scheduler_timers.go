// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// FiredTimer is a due timer claimed by this replica.
type FiredTimer struct {
	ID        uuid.UUID
	Kind      string
	SubjectID uuid.UUID
	FireAt    time.Time
	Payload   map[string]any
}

// RetryCreated describes a retry attempt row the scheduler created.
type RetryCreated struct {
	PrevAttemptID uuid.UUID
	NewAttemptID  uuid.UUID
	RunID         uuid.UUID
	NodeID        string
	AttemptNumber int32
}

// ClaimDueTimers atomically claims due timers for this replica.
//
// The claim is a single UPDATE ... RETURNING over a SKIP LOCKED subquery so that
// N replicas sweeping the same table never fire the same timer twice: a
// duplicate retry_backoff firing would create two PENDING attempts for one
// logical retry, and the UNIQUE(run_id, node_id, attempt_number) constraint
// would then make one of them fail noisily rather than silently — but at the
// cost of a wasted transaction and a confusing error. Claiming properly avoids
// both.
//
// claimed_by records this instance so an operator can see which replica owns a
// timer, and so a future sweep can reclaim timers abandoned by a crashed
// replica.
func (s *SchedulerStore) ClaimDueTimers(
	ctx context.Context, now time.Time, kinds []string, limit int,
) ([]FiredTimer, error) {
	if limit <= 0 {
		limit = 50
	}
	// Scoped to this replica's queue via the attempt the timer is about, for the
	// same reason the reconcilers are: a scheduler draining `gpu` must not fire
	// `cpu`'s retry backoffs. A timer whose subject is not an attempt of this
	// queue is left for the replica that owns it.
	//
	// The scoping is an EXISTS rather than a join so a timer whose subject row is
	// gone (a deleted run) is skipped here and reaped by the handler's
	// not-found path, instead of being claimed and re-released forever.
	const q = `
UPDATE timers t
   SET claimed_by = $1, claimed_at = $2
 WHERE t.id IN (
       SELECT id FROM timers ti
        WHERE ti.claimed_by IS NULL
          AND ti.fire_at <= $2
          AND ti.kind = ANY($3)
          AND EXISTS (SELECT 1 FROM task_attempts a
                       WHERE a.id = ti.subject_id AND a.queue = $5)
        ORDER BY ti.fire_at
        LIMIT $4
        FOR UPDATE SKIP LOCKED)
RETURNING t.id, t.kind, t.subject_id, t.fire_at, t.payload`

	rows, err := s.pool.Query(ctx, q, s.instanceID, now, kinds, limit, s.cfg.Queue)
	if err != nil {
		return nil, fmt.Errorf("claim due timers: %w", err)
	}
	defer rows.Close()

	var out []FiredTimer
	for rows.Next() {
		var ft FiredTimer
		var body []byte
		if err := rows.Scan(&ft.ID, &ft.Kind, &ft.SubjectID, &ft.FireAt, &body); err != nil {
			return nil, fmt.Errorf("scan fired timer: %w", err)
		}
		if len(body) > 0 {
			_ = json.Unmarshal(body, &ft.Payload)
		}
		out = append(out, ft)
	}
	return out, rows.Err()
}

// CreatePendingRetry materializes the retry when a retry_backoff timer fires.
//
// The new attempt is a NEW ROW with attempt_number+1 (invariant 9). The terminal
// attempt is read but NEVER updated: history is not rewritten, which is what
// makes the event log a truthful record and makes "show me every attempt"
// trivially answerable (§5.1).
//
// The idempotency key is copied verbatim from the previous attempt rather than
// recomputed, for two reinforcing reasons. First, it is the same logical work,
// so by invariant 1 it must be the same key. Second, copying cannot drift: a
// recomputation would depend on this service agreeing with the DAG engine about
// map_index and task_version forever, and any disagreement would silently break
// deduplication — the failure mode being "the customer is charged twice", which
// no test in this service would catch.
//
// Returns nil when the retry already exists, which makes the timer handler
// idempotent under at-least-once timer delivery.
func (s *SchedulerStore) CreatePendingRetry(
	ctx context.Context, now time.Time, timer FiredTimer,
) (*RetryCreated, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin retry tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	prevID := timer.SubjectID

	var (
		runID         uuid.UUID
		nodeID        string
		attemptNumber int32
		state         string
		queue         string
		priority      int32
		reqJSON       []byte
		idemKey       string
		cacheKey      *string
		inputRefs     []byte
		prevWorker    *uuid.UUID
		specJSON      []byte
	)
	if err := tx.QueryRow(ctx, `
SELECT a.run_id, a.node_id, a.attempt_number, a.state, a.queue, a.priority,
       a.resource_request, a.idempotency_key, a.cache_key, a.input_refs,
       a.worker_id, n.spec_json
  FROM task_attempts a
  JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
 WHERE a.id = $1`, prevID,
	).Scan(&runID, &nodeID, &attemptNumber, &state, &queue, &priority,
		&reqJSON, &idemKey, &cacheKey, &inputRefs, &prevWorker, &specJSON); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The attempt is gone (run deleted). Consume the timer rather than
			// leaving it to be retried forever.
			return nil, s.finishTimer(ctx, tx, timer.ID)
		}
		return nil, fmt.Errorf("load previous attempt %s: %w", prevID, err)
	}

	// Only retry a genuinely terminal, retryable attempt. If the attempt is not
	// terminal, a late worker report won the race and the work is proceeding
	// normally — creating a second live attempt for it would be a real double
	// dispatch, which no constraint downstream would catch.
	if !domain.AttemptState(state).IsRetryable() {
		return nil, s.finishTimer(ctx, tx, timer.ID)
	}

	var spec TaskSpecView
	if len(specJSON) > 0 {
		_ = json.Unmarshal(specJSON, &spec)
	}

	nextNumber := attemptNumber + 1
	if n, ok := timer.Payload["next_attempt_number"].(float64); ok && int32(n) > nextNumber {
		nextNumber = int32(n)
	}

	// ON CONFLICT DO NOTHING on UNIQUE(run_id, node_id, attempt_number) makes
	// this idempotent: a duplicated timer delivery creates no second attempt.
	//
	// The CTE writes the retry's task_ready outbox row in the same statement, so
	// an idle worker is woken for the retry exactly when it becomes claimable.
	var newID uuid.UUID
	err = tx.QueryRow(ctx, `
WITH a AS (
INSERT INTO task_attempts (
        run_id, node_id, attempt_number, state, queue, priority,
        resource_request, idempotency_key, cache_key, input_refs, scheduled_at)
VALUES ($1,$2,$3,'PENDING',$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (run_id, node_id, attempt_number) DO NOTHING
RETURNING id, run_id, queue, scheduled_at
),`+outboxTaskReadyCTE+`
SELECT id FROM a`,
		runID, nodeID, nextNumber, queue, priority,
		reqJSON, idemKey, cacheKey, inputRefs, now,
	).Scan(&newID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, s.finishTimer(ctx, tx, timer.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("insert retry attempt: %w", err)
	}

	// Charge the retry budget in the SAME transaction as the attempt row.
	//
	// task_nodes.attempts_used is what the §5.1 RETRY predicate compares against
	// max_attempts (domain.ShouldRetry), and the dag-engine only bumps it for the
	// attempts IT creates. Retries are created here, so without this the counter
	// stayed at 1 however many times a node was retried and max_attempts capped
	// nothing: a task failing a retryable class would be retried forever, held back
	// only by whatever else happened to make the run terminal. Same transaction as
	// the INSERT so the budget cannot drift from the attempts that actually exist,
	// and ON CONFLICT above means a duplicated timer delivery charges it once.
	//
	// A retry after an announced eviction is charged to the SEPARATE
	// preemption budget instead (ADR-0030 §3): the timer payload says which,
	// because the decision was made -- against the right budget -- in the
	// transaction that recorded the PREEMPTED outcome.
	budgetCol := "attempts_used"
	if charge, _ := timer.Payload["charge"].(string); charge == chargePreemption && !s.placementMissing {
		budgetCol = "preemptions_used"
	}
	if _, err := tx.Exec(ctx, `
UPDATE task_nodes SET `+budgetCol+` = `+budgetCol+` + 1
 WHERE run_id = $1 AND node_id = $2`, runID, nodeID); err != nil {
		return nil, fmt.Errorf("charge retry budget for %s/%s: %w", runID, nodeID, err)
	}

	// retry_on_different_worker: the poison-host defence (§5.5). Recorded as
	// pending_reason rather than enforced in the claim query because excluding a
	// worker is a soft preference — if it is the only worker that fits, running
	// there beats not running at all.
	if spec.RetryPolicy().GetRetryOnDifferentWorker() && prevWorker != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE task_attempts SET pending_reason = $2 WHERE id = $1`,
			newID, "prefers_different_worker: "+prevWorker.String()); err != nil {
			return nil, fmt.Errorf("record different-worker preference: %w", err)
		}
	}

	if err := appendEvent(ctx, tx, runID, nodeID, &newID,
		domain.EventAttemptScheduled, domain.ActorScheduler, map[string]any{
			"attempt_number":      nextNumber,
			"previous_attempt_id": prevID.String(),
			"previous_state":      state,
			// Recorded so an operator can verify the invariant from the log
			// alone, without recomputing sha256 by hand.
			"idempotency_key_unchanged": true,
		}); err != nil {
		return nil, err
	}

	// schedule_to_start is per-attempt: the new attempt gets its own clock, so a
	// retry is not instantly timed out by the previous attempt's queueing time.
	if ms := spec.ScheduleToStartMs; ms > 0 {
		if err := insertTimer(ctx, tx, domain.TimerScheduleToStart, newID,
			now.Add(time.Duration(ms)*time.Millisecond), nil); err != nil {
			return nil, err
		}
	}

	if err := s.finishTimerNoCommit(ctx, tx, timer.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit retry tx: %w", err)
	}
	return &RetryCreated{
		PrevAttemptID: prevID, NewAttemptID: newID,
		RunID: runID, NodeID: nodeID, AttemptNumber: nextNumber,
	}, nil
}

// finishTimer consumes a timer and commits. A fired timer is DELETED rather
// than flagged: the timers table is a work queue, and keeping fired rows would
// grow the index that the "due timers" partial index exists to keep small.
func (s *SchedulerStore) finishTimer(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	if err := s.finishTimerNoCommit(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit timer consumption: %w", err)
	}
	return nil
}

func (s *SchedulerStore) finishTimerNoCommit(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	if _, err := tx.Exec(ctx, `DELETE FROM timers WHERE id = $1`, id); err != nil {
		return fmt.Errorf("delete fired timer: %w", err)
	}
	return nil
}

// ReleaseTimer returns a claimed timer to the pool, for the case where handling
// failed and a later pass should try again.
func (s *SchedulerStore) ReleaseTimer(ctx context.Context, id uuid.UUID) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE timers SET claimed_by = NULL, claimed_at = NULL WHERE id = $1`, id,
	); err != nil {
		return fmt.Errorf("release timer %s: %w", id, err)
	}
	return nil
}

// ExpireScheduleToStart handles a fired schedule_to_start timer: an attempt that
// never got capacity in time.
//
// TIMED_OUT, not LOST: we know for certain the work never started, because it
// never left PENDING. Distinguishing this from a LOST attempt is the whole point
// of the four-timeout model — this one says "fix the cluster", not "fix the
// code" (§4.3).
func (s *SchedulerStore) ExpireScheduleToStart(
	ctx context.Context, now time.Time, timer FiredTimer,
) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin schedule_to_start tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var runID uuid.UUID
	var nodeID string
	err = tx.QueryRow(ctx, `
UPDATE task_attempts
   SET state = 'TIMED_OUT', ended_at = $2, pending_reason = NULL,
       error = jsonb_build_object('class','ERROR_CLASS_TIMEOUT',
                                  'reason','schedule_to_start_exceeded'),
       state_version = state_version + 1
 WHERE id = $1 AND state = 'PENDING'
RETURNING run_id, node_id`, timer.SubjectID, now).Scan(&runID, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Already dispatched or already terminal: the timer lost the race, which
		// is the normal case for the overwhelming majority of these timers.
		return s.finishTimer(ctx, tx, timer.ID)
	}
	if err != nil {
		return fmt.Errorf("apply schedule_to_start timeout: %w", err)
	}

	if err := appendEvent(ctx, tx, runID, nodeID, &timer.SubjectID,
		"AttemptTimedOut", domain.ActorScheduler, map[string]any{
			"reason": "schedule_to_start_exceeded",
		}); err != nil {
		return err
	}
	return s.finishTimer(ctx, tx, timer.ID)
}
