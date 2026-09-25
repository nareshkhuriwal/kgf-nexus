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
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// ReconcileExpiredLeases moves attempts whose lease has expired to LOST,
// releases their capacity, and evaluates retry.
//
// LOST, never FAILED (invariant 2): an expired lease means we do not KNOW
// whether the work ran. Recording FAILED would assert knowledge we do not have,
// and would let §6.5 auto-retry a task that declared it cannot safely run twice.
//
// Each attempt is its own transaction rather than one big batch. A single
// transaction over hundreds of expiries would hold locks on hundreds of worker
// rows, blocking the dispatch path (the thing that actually matters for
// latency), and one malformed row would roll back every good recovery.
//
// Returns the attempts it transitioned, so the caller can log and publish.
func (s *SchedulerStore) ReconcileExpiredLeases(
	ctx context.Context, now time.Time, limit int,
) ([]LostAttempt, error) {
	ids, err := s.expiredLeaseAttempts(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	var (
		out  []LostAttempt
		errs []error
	)
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		lost, err := s.expireOne(ctx, now, id, domain.EventLeaseExpired, "lease_expired")
		if err != nil {
			// One bad attempt must not stop recovery of the rest: a stuck
			// reconciler leaks capacity fleet-wide, which is far worse than one
			// unreconciled attempt that the next pass will retry. This used to
			// say so and then return on the first error anyway (F108); the
			// errors are now joined and returned after every attempt is tried.
			errs = append(errs, fmt.Errorf("expire lease for attempt %s: %w", id, err))
			continue
		}
		if lost != nil {
			out = append(out, *lost)
		}
	}
	return out, errors.Join(errs...)
}

func (s *SchedulerStore) expiredLeaseAttempts(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 100
	}
	// Only DISPATCHED/RUNNING attempts are candidates. A lease row outliving a
	// terminal attempt is harmless garbage, and forcing a transition on an
	// already-terminal attempt would violate §5.1.
	//
	// Scoped to this replica's queue. A scheduler draining `gpu` must not
	// reconcile `cpu`: queues are the unit of independent scaling (§4.3), and a
	// fleet-wide reconciler would make every replica contend on every other
	// queue's expiries, which is the coordination ADR-0002 exists to avoid.
	const q = `
SELECT l.attempt_id
  FROM leases l
  JOIN task_attempts a ON a.id = l.attempt_id
 WHERE l.expires_at <= $1
   AND a.state IN ('DISPATCHED','RUNNING')
   AND a.queue = $3
 ORDER BY l.expires_at
 LIMIT $2`
	rows, err := s.pool.Query(ctx, q, now, limit, s.cfg.Queue)
	if err != nil {
		return nil, fmt.Errorf("select expired leases: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired lease: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// expireOne performs the whole recovery for one attempt in one transaction:
// LOST, release reservation, decrement workers.committed, release the
// concurrency slot, delete stale timers, emit events, and create the retry
// attempt if policy allows.
//
// Doing all of it atomically is what stops a crash mid-recovery from leaving
// capacity committed to an attempt that is already terminal.
func (s *SchedulerStore) expireOne(
	ctx context.Context, now time.Time, attemptID uuid.UUID, eventKind, reason string,
) (*LostAttempt, error) {
	// READ COMMITTED for the same reason as Claim: SKIP LOCKED below must skip a
	// row a peer replica already recovered, not abort with a serialization
	// failure. Safety here comes from the row lock plus the state CAS.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin expiry tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-read under a row lock: between the scan and here another replica may
	// already have recovered this attempt. SKIP LOCKED makes that a no-op
	// instead of a duplicate release that would corrupt the capacity ledger.
	//
	// Skipping is right here and wrong in ApplyResult (F104) because this
	// caller is a sweep: an attempt skipped because a MarkStarted or a result
	// held its row is re-selected on the next pass while its lease is still
	// expired, or its worker still DEAD, and a result that won the row has
	// made the reclaim unnecessary.
	const lockQ = `
SELECT a.id, a.run_id, a.node_id, a.attempt_number, a.state, a.worker_id,
       a.resource_request, a.scheduled_at, a.priority, a.queue,
       n.spec_json, n.attempts_used, n.max_attempts, r.deadline_at, n.retry_floor
  FROM task_attempts a
  JOIN task_nodes    n ON n.run_id = a.run_id AND n.node_id = a.node_id
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.id = $1
   AND a.state IN ('DISPATCHED','RUNNING')
 FOR UPDATE OF a SKIP LOCKED`

	var (
		att          Candidate
		state        string
		workerID     *uuid.UUID
		reqJSON      []byte
		specJSON     []byte
		attemptsUsed int32
		nodeMax      int32
		runDeadline  *time.Time
	)
	if err := tx.QueryRow(ctx, lockQ, attemptID).Scan(
		&att.ID, &att.RunID, &att.NodeID, &att.AttemptNumber, &state, &workerID,
		&reqJSON, &att.ScheduledAt, &att.Priority, &att.Queue,
		&specJSON, &attemptsUsed, &nodeMax, &runDeadline, &att.RetryFloor,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lock attempt: %w", err)
	}

	req, err := decodeVector(reqJSON)
	if err != nil {
		return nil, err
	}
	att.ResourceRequest = req
	var spec TaskSpecView
	if len(specJSON) > 0 {
		_ = json.Unmarshal(specJSON, &spec)
	}
	att.Spec = &spec

	if err := s.releaseCapacity(ctx, tx, attemptID, workerID, req); err != nil {
		return nil, err
	}

	// Terminal transition. The CAS on the non-terminal states is redundant
	// under the row lock and kept because §5.1 admits no transition out of a
	// terminal state, ever, and this is the code path most likely to race.
	tag, err := tx.Exec(ctx, `
UPDATE task_attempts
   SET state = 'LOST', ended_at = $2, pending_reason = NULL,
       error = jsonb_build_object('class','ERROR_CLASS_WORKER_LOST','reason',$3::text),
       state_version = state_version + 1
 WHERE id = $1 AND state IN ('DISPATCHED','RUNNING')`,
		attemptID, now, reason)
	if err != nil {
		return nil, fmt.Errorf("transition attempt to LOST: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}

	// The lease is gone with the attempt. Deleting rather than keeping it means
	// any late report from a zombie worker finds no lease, so its token cannot
	// match and its result is rejected (ADR-0010).
	if _, err := tx.Exec(ctx, `DELETE FROM leases WHERE attempt_id = $1`, attemptID); err != nil {
		return nil, fmt.Errorf("delete expired lease: %w", err)
	}
	// Deadline timers for a terminal attempt would fire against history.
	if err := deleteTimersFor(ctx, tx, attemptID,
		domain.TimerScheduleToStart, domain.TimerStartToClose, domain.TimerHeartbeat); err != nil {
		return nil, err
	}

	payload := map[string]any{"reason": reason}
	if workerID != nil {
		payload["worker_id"] = workerID.String()
	}
	if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &attemptID,
		domain.EventAttemptLost, domain.ActorScheduler, payload); err != nil {
		return nil, err
	}
	if eventKind != domain.EventAttemptLost {
		if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &attemptID,
			eventKind, domain.ActorScheduler, payload); err != nil {
			return nil, err
		}
	}

	// LOST is billed like any other end: the reservation held capacity from
	// dispatch until now, whatever became of the work (ADR-0021). No measured
	// metrics exist for it, and the ledger says so with NULLs.
	usage, err := s.recordUsage(ctx, tx, attemptID)
	if err != nil {
		return nil, err
	}

	lost := LostAttempt{
		AttemptID: attemptID, RunID: att.RunID, NodeID: att.NodeID,
		AttemptNumber: att.AttemptNumber,
	}
	if workerID != nil {
		lost.WorkerID = *workerID
	}

	// attempts_used from task_nodes is the DAG engine's counter and may lag; the
	// attempt_number we just terminated is authoritative for "how many tries
	// have there been", so take the larger -- counted from the node's
	// retry_floor, because an operator retry (migration 0026, F100) resets the
	// budget and the attempts below the floor were spent by the superseded tries.
	// Without the subtraction a retried node's first LOST attempt would be judged
	// out of budget and never auto-retried.
	used := attemptsUsed
	if n := att.AttemptNumber - att.RetryFloor; n > used {
		used = n
	}
	maxAttempts := nodeMax
	if spec.MaxAttempts > 0 {
		maxAttempts = spec.MaxAttempts
	}

	decision, retryAt, err := s.evaluateAndScheduleRetry(
		ctx, tx, now, &att, used, maxAttempts, runDeadline)
	if err != nil {
		return nil, err
	}
	lost.Decision = decision
	lost.RetryAt = retryAt
	lost.UsageRecorded = usage != ""

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit expiry tx: %w", err)
	}
	s.notifyUsage(usage)
	return &lost, nil
}

// releaseCapacity deletes the reservation, decrements workers.committed and
// frees the concurrency slot.
//
// This is the mirror of the ADR-0011 exception in commitDispatch: the scheduler
// wrote the reservation atomically with the lease, so it must be able to undo
// both atomically when the lease dies. Splitting the release across a gRPC call
// to the resource manager would leave a window in which the attempt is terminal
// but its capacity is still committed, which is exactly the capacity leak the
// lease design exists to prevent.
//
// The decrement reads workers.committed under a row lock and writes the clamped
// difference, rather than doing arithmetic in SQL, because the subtraction must
// use domain.SubVec: it clamps at zero, so a double release (two replicas
// racing) degrades to a no-op instead of driving committed negative and
// permanently over-committing the worker.
func (s *SchedulerStore) releaseCapacity(
	ctx context.Context, tx pgx.Tx, attemptID uuid.UUID, workerID *uuid.UUID,
	fallbackReq *nexusv1.ResourceVector,
) error {
	var resJSON []byte
	err := tx.QueryRow(ctx,
		`DELETE FROM reservations WHERE attempt_id = $1 RETURNING resources`,
		attemptID).Scan(&resJSON)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No reservation: either already released, or the attempt never held
		// one. Either way there is nothing to give back — releasing the
		// fallback request here would CREATE capacity that was never committed.
		resJSON = nil
	case err != nil:
		return fmt.Errorf("delete reservation: %w", err)
	}

	if workerID != nil && resJSON != nil {
		released, err := decodeVector(resJSON)
		if err != nil {
			return err
		}
		var comJSON []byte
		if err := tx.QueryRow(ctx,
			`SELECT committed FROM workers WHERE id = $1 FOR UPDATE`, *workerID,
		).Scan(&comJSON); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("lock worker for release: %w", err)
			}
		} else {
			committed, err := decodeVector(comJSON)
			if err != nil {
				return err
			}
			next, err := encodeVector(domain.SubVec(committed, released))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE workers SET committed = $2 WHERE id = $1`, *workerID, next,
			); err != nil {
				return fmt.Errorf("decrement worker committed: %w", err)
			}
		}
	}

	// A held semaphore slot outliving its attempt would permanently shrink the
	// effective limit.
	if _, err := tx.Exec(ctx,
		`DELETE FROM concurrency_slots WHERE attempt_id = $1`, attemptID,
	); err != nil {
		return fmt.Errorf("release concurrency slot: %w", err)
	}
	return nil
}

// evaluateAndScheduleRetry applies domain.ShouldRetry and, when it permits a
// retry, schedules it via a retry_backoff timer.
//
// It creates a TIMER, not the new attempt row, because the backoff must be
// respected: inserting a PENDING attempt immediately would have the dispatch
// loop pick it up on the next tick and hammer a dependency that is very likely
// still unhealthy. The timer path (CreatePendingRetry) is where the new row is
// born, and it always allocates attempt_number+1 as a NEW row — the terminal
// attempt is never mutated (invariant 9).
func (s *SchedulerStore) evaluateAndScheduleRetry(
	ctx context.Context, tx pgx.Tx, now time.Time, att *Candidate,
	attemptsUsed, maxAttempts int32, runDeadline *time.Time,
) (domain.RetryDecision, time.Time, error) {
	policy := att.Spec.RetryPolicy()
	// schedule_to_close is the end-to-end SLA across ALL retries, measured from
	// the first attempt's scheduling against control-plane time (invariant 7).
	stcExceeded := false
	if runDeadline != nil && now.After(*runDeadline) {
		stcExceeded = true
	}
	if ms := att.Spec.ScheduleToCloseMs; ms > 0 {
		if now.After(att.ScheduledAt.Add(time.Duration(ms) * time.Millisecond)) {
			stcExceeded = true
		}
	}

	decision := domain.ShouldRetry(
		nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST,
		att.Spec.IdempotencyModeProto(),
		attemptsUsed, maxAttempts, stcExceeded, policy)

	if decision.Warn {
		// We owe the operator this warning: under best_effort idempotency the
		// retry may genuinely execute the work a second time (§6.5).
		if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &att.ID,
			domain.EventPossibleDuplicateExecution, domain.ActorScheduler, map[string]any{
				"reason": decision.Reason, "retried_attempt_number": att.AttemptNumber,
			}); err != nil {
			return decision, time.Time{}, err
		}
	}

	if !decision.Retry {
		kind := domain.EventRetryExhausted
		if decision.Reason == domain.ReasonIndeterminateRequiresHuman {
			// A distinct event kind so the UI can offer `attempt retry --force`
			// rather than presenting the run as simply broken: this is a
			// decision we are deferring to a human, not a failure.
			kind = domain.EventIndeterminateOutcome
		}
		if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &att.ID,
			kind, domain.ActorScheduler, map[string]any{
				"reason": decision.Reason, "attempts_used": attemptsUsed,
				"max_attempts": maxAttempts,
			}); err != nil {
			return decision, time.Time{}, err
		}
		return decision, time.Time{}, nil
	}

	delay := domain.Backoff(policy, att.AttemptNumber-att.RetryFloor)
	fireAt := now.Add(delay)
	if err := insertTimer(ctx, tx, domain.TimerRetryBackoff, att.ID, fireAt, map[string]any{
		"run_id": att.RunID.String(), "node_id": att.NodeID,
		"next_attempt_number": att.AttemptNumber + 1,
		"reason":              decision.Reason,
	}); err != nil {
		return decision, time.Time{}, err
	}
	if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &att.ID,
		domain.EventRetryScheduled, domain.ActorScheduler, map[string]any{
			"reason": decision.Reason, "backoff_ms": delay.Milliseconds(),
			"next_attempt_number": att.AttemptNumber + 1,
			"fire_at":             fireAt.UTC().Format(time.RFC3339Nano),
		}); err != nil {
		return decision, time.Time{}, err
	}
	return decision, fireAt, nil
}
