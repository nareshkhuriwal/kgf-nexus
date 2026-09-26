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
	"github.com/jackc/pgx/v5/pgconn"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// The worker's side of an attempt's life. The gateway fences every one of these
// on the lease token BEFORE calling (invariant 3), so by the time execution
// reaches this file the caller has been proven to be the attempt's current
// owner. These functions therefore guard on STATE, not on ownership.
// RecordProgress (scheduler_progress.go) is the exception: it also stores data
// the worker supplied, so it re-checks the lease inside its own transaction.

// MarkStarted transitions DISPATCHED -> RUNNING.
//
// The worker's own started_at is deliberately not a parameter. Deadlines come
// from control-plane time (invariant 7): a worker whose clock ran ten minutes
// fast would otherwise appear to have blown its start_to_close budget the instant
// it began.
//
// The schedule_to_start timer is deleted in the same transaction as the
// transition. That timer means "this attempt waited too long for capacity", and
// the attempt has now started — leaving it armed would fire a spurious timeout
// against work that is running fine.
func (s *SchedulerStore) MarkStarted(ctx context.Context, attemptID uuid.UUID, now time.Time) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin mark-started tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// CAS on DISPATCHED. A report for an attempt already moved terminal (expired
	// and retried elsewhere, or cancelled) matches zero rows and is ignored rather
	// than resurrecting it, which §5.1 forbids.
	var runID uuid.UUID
	var nodeID string
	err = tx.QueryRow(ctx, `
UPDATE task_attempts
   SET state = 'RUNNING', started_at = $2, state_version = state_version + 1
 WHERE id = $1 AND state = 'DISPATCHED'
 RETURNING run_id, node_id`, attemptID, now).Scan(&runID, &nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not an error: at-least-once delivery means a duplicate ReportStarted is
		// expected, and the second one legitimately finds nothing to do.
		return nil
	}
	if err != nil {
		return fmt.Errorf("cas attempt %s to RUNNING: %w", attemptID, err)
	}

	if err := deleteTimersFor(ctx, tx, attemptID, domain.TimerScheduleToStart); err != nil {
		return err
	}
	if err := appendEvent(ctx, tx, runID, nodeID, &attemptID,
		"AttemptStarted", domain.ActorScheduler, map[string]any{
			"started_at": now.UTC().Format(time.RFC3339Nano),
		}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit mark-started tx: %w", err)
	}
	return nil
}

// DefaultAttemptLockTimeout is how long a report-driven terminal transition
// waits for the attempt row when ClaimConfig.AttemptLockTimeout is zero.
//
// The transactions it waits behind (MarkStarted, a lease reclaim, a cancel, a
// peer applying the same result) each touch a handful of rows and commit in
// milliseconds, so two seconds is generous. It is well inside the gateway's 5 s
// upstream call timeout, so a busy report fails with a status the agent retries
// rather than with the gateway's own deadline.
const DefaultAttemptLockTimeout = 2 * time.Second

// ErrAttemptBusy means another transaction held the attempt row past the lock
// timeout (or a deadlock was broken on it). Nothing was written. It is
// retryable: the report must be resent, never treated as delivered.
var ErrAttemptBusy = errors.New("attempt is locked by another transaction; retry the report")

// lockAttemptForTransition runs lock -- a SELECT ... FOR UPDATE of the attempt
// row that must be the transaction's FIRST lock -- with a bounded wait, and
// restores the session's lock_timeout for the rest of the transaction.
//
// Why wait at all, when most of this package skips locked rows: SKIP LOCKED is
// right for a work queue, where a skipped row is picked up by the next pass.
// A report is not a queue item. Nothing re-delivers a skipped result on its
// own, so "someone else holds the row" must mean "wait for them", not "they
// handled it" (CONTRACT-FINDINGS F104).
//
// Why the wait is safe: it is the first lock, so this transaction holds
// nothing another can be waiting on and cannot close a deadlock cycle here.
// Postgres still reports any deadlock or timeout, and both become
// ErrAttemptBusy so the caller answers with a retryable status.
func (s *SchedulerStore) lockAttemptForTransition(ctx context.Context, tx pgx.Tx, lock func() error) error {
	d := s.cfg.AttemptLockTimeout
	if d <= 0 {
		d = DefaultAttemptLockTimeout
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`,
		fmt.Sprintf("%dms", d.Milliseconds())); err != nil {
		return fmt.Errorf("bound attempt lock wait: %w", err)
	}
	if err := lock(); err != nil {
		if isLockBusy(err) {
			return fmt.Errorf("%w: %v", ErrAttemptBusy, err)
		}
		return err
	}
	// Only the attempt lock is bounded. The rest of the transition (worker row,
	// run row) keeps the waits it always had, so this change cannot turn a
	// transition that used to succeed slowly into a failure.
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout TO DEFAULT`); err != nil {
		return fmt.Errorf("restore lock_timeout: %w", err)
	}
	return nil
}

// isLockBusy reports a lock wait that Postgres gave up on: lock_not_available
// (lock_timeout or NOWAIT) or deadlock_detected.
func isLockBusy(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "55P03" || pgErr.Code == "40P01"
}

// AppliedResult reports what ApplyResult did, so the caller can drive the DAG
// engine's reconcile without re-reading the row.
type AppliedResult struct {
	RunID  uuid.UUID
	NodeID string
	// State is the terminal attempt state written, or empty when the report was
	// ignored as stale.
	State string
	// Applied is false when the CAS found nothing to move — a duplicate report,
	// or an attempt already expired and retried elsewhere.
	Applied bool
	// Retry is set when the outcome was retryable and a retry was scheduled.
	Retry bool
	// UsageRecorded is true when the attempt's usage-ledger row was written in
	// this transaction (ADR-0021).
	UsageRecorded bool
}

// ApplyResult applies a terminal outcome reported by the worker.
//
// Everything commits in ONE transaction — attempt state, output, capacity
// release, concurrency slot, stale timers and the event — for the same reason
// dispatch does: a crash between "attempt is SUCCEEDED" and "its GPU is released"
// leaks that GPU until a reconciler notices, and the reconciler cannot tell a leak
// from a legitimately-held reservation without guessing.
//
// The outcome mapping is where invariant 2 lives. INDETERMINATE becomes LOST, not
// FAILED: the agent's WAL told it the task started but not how it ended, so
// claiming FAILED would assert knowledge nobody has, and would let §6.5 auto-retry
// work that declared it cannot safely run twice.
func (s *SchedulerStore) ApplyResult(
	ctx context.Context, now time.Time, req *nexusv1.ReportResultRequest,
) (AppliedResult, error) {
	attemptID, err := uuid.Parse(req.GetAttemptId())
	if err != nil {
		return AppliedResult{}, fmt.Errorf("apply result: attempt_id %q is not a uuid", req.GetAttemptId())
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AppliedResult{}, fmt.Errorf("begin apply-result tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-read under a row lock. Between the gateway's fence check and here, a
	// reconciler may have expired this attempt; the state filter makes that a
	// no-op rather than a double application.
	//
	// This WAITS for the lock (bounded) instead of using SKIP LOCKED. It used to
	// skip, and read "no row" as "already handled" -- so a result arriving while
	// the worker's own ReportStarted transaction held the row was skipped,
	// acknowledged to the agent as accepted, and later reclaimed as LOST
	// (CONTRACT-FINDINGS F104). Waiting is safe for fencing: under READ
	// COMMITTED, Postgres re-evaluates the state filter against the row version
	// the holder committed, so a reclaim or cancel that won the lock turns this
	// into a no-op exactly as skipping did, and a peer applying the same result
	// is seen as terminal. The wait cannot deadlock on this statement because it
	// is the transaction's first lock. See lockAttemptForTransition.
	lockQ := `
SELECT a.id, a.run_id, a.node_id, a.attempt_number, a.worker_id,
       a.resource_request, a.scheduled_at, a.priority, a.queue,
       n.spec_json, n.attempts_used, n.max_attempts, r.deadline_at, n.retry_floor,
       ` + s.preemptionsUsedCol() + `
  FROM task_attempts a
  JOIN task_nodes    n ON n.run_id = a.run_id AND n.node_id = a.node_id
  JOIN workflow_runs r ON r.id = a.run_id
 WHERE a.id = $1
   AND a.state IN ('DISPATCHED','RUNNING')
 FOR UPDATE OF a`

	var (
		cand                     Candidate
		workerID                 *uuid.UUID
		reqJSON, specJSON        []byte
		attemptsUsed, maxAttempt int32
		runDeadline              *time.Time
		preemptionsUsed          int32
	)
	err = s.lockAttemptForTransition(ctx, tx, func() error {
		return tx.QueryRow(ctx, lockQ, attemptID).Scan(
			&cand.ID, &cand.RunID, &cand.NodeID, &cand.AttemptNumber, &workerID,
			&reqJSON, &cand.ScheduledAt, &cand.Priority, &cand.Queue,
			&specJSON, &attemptsUsed, &maxAttempt, &runDeadline, &cand.RetryFloor, &preemptionsUsed)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Already terminal: a duplicate report (the expected consequence of
		// at-least-once delivery), or an attempt a reconciler or cancel moved
		// first. Never "locked by someone else" any more -- that waits above.
		return AppliedResult{Applied: false}, nil
	}
	if err != nil {
		return AppliedResult{}, fmt.Errorf("lock attempt %s for result: %w", attemptID, err)
	}

	if cand.ResourceRequest, err = decodeVector(reqJSON); err != nil {
		return AppliedResult{}, err
	}
	var spec TaskSpecView
	if len(specJSON) > 0 {
		_ = json.Unmarshal(specJSON, &spec)
	}
	cand.Spec = &spec

	state, errClass := outcomeToState(req.GetOutcome(), req.GetError())

	var outputJSON []byte
	if outs := req.GetOutputs(); len(outs) > 0 {
		// Stored as the map the DAG engine reads when resolving a downstream
		// node's inputs. Only the REFERENCE is stored; the payload itself lives in
		// MinIO and never transits the control plane (invariant 6).
		m := map[string]json.RawMessage{}
		for name, ref := range outs {
			b, merr := protojsonMarshal(ref)
			if merr != nil {
				return AppliedResult{}, fmt.Errorf("encode output %q: %w", name, merr)
			}
			m[name] = b
		}
		if outputJSON, err = json.Marshal(m); err != nil {
			return AppliedResult{}, fmt.Errorf("encode outputs: %w", err)
		}
	}

	// The measured ExecutionMetrics, which the worker has always sent and the
	// control plane used to drop (CONTRACT-FINDINGS F122). task_attempts.metrics
	// has existed since 0001; the usage ledger reads it below.
	metricsJSON, err := encodeExecutionMetrics(req.GetMetrics())
	if err != nil {
		return AppliedResult{}, err
	}

	var errJSON []byte
	if te := req.GetError(); te != nil {
		if errJSON, err = protojsonMarshal(te); err != nil {
			return AppliedResult{}, fmt.Errorf("encode task error: %w", err)
		}
	}

	// The CAS guard again, now on the terminal transition.
	tag, err := tx.Exec(ctx, `
UPDATE task_attempts
   SET state = $2::attempt_state, ended_at = $3, output_ref = $4, error = $5,
       metrics = COALESCE($6::jsonb, metrics),
       state_version = state_version + 1
 WHERE id = $1 AND state IN ('DISPATCHED','RUNNING')`,
		attemptID, state, now, nullableJSON(outputJSON), nullableJSON(errJSON), nullableJSON(metricsJSON))
	if err != nil {
		return AppliedResult{}, fmt.Errorf("cas attempt %s to %s: %w", attemptID, state, err)
	}
	if tag.RowsAffected() == 0 {
		return AppliedResult{Applied: false}, nil
	}

	// Capacity comes back the moment the attempt is terminal. Holding it until a
	// reconciler noticed would make every finished task a temporary capacity leak,
	// and at fleet scale that is the difference between a busy cluster and an idle
	// one.
	if err := s.releaseCapacity(ctx, tx, attemptID, workerID, cand.ResourceRequest); err != nil {
		return AppliedResult{}, err
	}

	// Every deadline that could still fire against this attempt is removed. A
	// start_to_close timer firing after SUCCEEDED would try to move a terminal
	// attempt, which §5.1 forbids.
	if err := deleteTimersFor(ctx, tx, attemptID,
		domain.TimerScheduleToStart, domain.TimerStartToClose,
		domain.TimerHeartbeat, domain.TimerLeaseExpiry); err != nil {
		return AppliedResult{}, err
	}

	// The lease is done. Deleting it rather than leaving it to expire means a
	// later report from a zombie holding this token is rejected immediately
	// instead of during the remaining TTL.
	if _, err := tx.Exec(ctx, `DELETE FROM leases WHERE attempt_id = $1`, attemptID); err != nil {
		return AppliedResult{}, fmt.Errorf("delete lease for attempt %s: %w", attemptID, err)
	}

	kind := "AttemptSucceeded"
	if state != string(domain.AttemptSucceeded) {
		kind = "AttemptFailed"
	}
	if err := appendEvent(ctx, tx, cand.RunID, cand.NodeID, &attemptID,
		kind, domain.ActorScheduler, map[string]any{
			"state":          state,
			"attempt_number": cand.AttemptNumber,
			"error_class":    errClass.String(),
		}); err != nil {
		return AppliedResult{}, err
	}

	out := AppliedResult{RunID: cand.RunID, NodeID: cand.NodeID, State: state, Applied: true}

	// The usage record, in the same transaction as the terminal transition and
	// the release of the reservation it accounts for (ADR-0021).
	usage, err := s.recordUsage(ctx, tx, attemptID)
	if err != nil {
		return AppliedResult{}, err
	}
	out.UsageRecorded = usage != ""

	// Retry evaluation runs in the SAME transaction as the terminal write, so a
	// crash cannot leave an attempt failed with no retry scheduled and no record
	// that one was owed.
	if state != string(domain.AttemptSucceeded) {
		cand.preemptionsUsed = preemptionsUsed
		decision, _, rerr := s.evaluateAndScheduleRetryWithClass(
			ctx, tx, now, &cand, errClass, attemptsUsed, maxAttempt, runDeadline)
		if rerr != nil {
			return AppliedResult{}, rerr
		}
		out.Retry = decision.Retry
	}

	if err := tx.Commit(ctx); err != nil {
		return AppliedResult{}, fmt.Errorf("commit apply-result tx: %w", err)
	}
	s.notifyUsage(usage)
	return out, nil
}

// evaluateAndScheduleRetryWithClass is evaluateAndScheduleRetry with the error
// class supplied by the worker rather than assumed to be WORKER_LOST.
//
// The distinction is the whole point of §4.3's error taxonomy: a VALIDATION
// failure must not be retried (the code is wrong and will be wrong again), while
// a TRANSIENT one should be. The lease-expiry path assumes WORKER_LOST because
// that is genuinely what it observed; this path knows better because the worker
// told it.
func (s *SchedulerStore) evaluateAndScheduleRetryWithClass(
	ctx context.Context, tx pgx.Tx, now time.Time, att *Candidate,
	class nexusv1.ErrorClass, attemptsUsed, maxAttempts int32, runDeadline *time.Time,
) (domain.RetryDecision, time.Time, error) {
	policy := att.Spec.RetryPolicy()

	stcExceeded := runDeadline != nil && now.After(*runDeadline)
	if ms := att.Spec.ScheduleToCloseMs; ms > 0 &&
		now.After(att.ScheduledAt.Add(time.Duration(ms)*time.Millisecond)) {
		stcExceeded = true
	}

	var decision domain.RetryDecision
	preempted := false
	if class == nexusv1.ErrorClass_ERROR_CLASS_PREEMPTED {
		// ADR-0030 §3: an eviction the agent announced spends the preemption
		// budget, not max_attempts, until that budget is gone.
		maxP := s.cfg.MaxPreemptions
		if p := att.Spec.Placement; p != nil && p.MaxPreemptions > 0 {
			maxP = p.MaxPreemptions
		}
		decision, preempted = domain.ShouldRetryPreempted(att.Spec.IdempotencyModeProto(),
			attemptsUsed, maxAttempts, att.preemptionsUsed, maxP, stcExceeded, policy)
	} else {
		decision = domain.ShouldRetry(class, att.Spec.IdempotencyModeProto(),
			attemptsUsed, maxAttempts, stcExceeded, policy)
	}

	if decision.Warn {
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
			kind = domain.EventIndeterminateOutcome
		}
		if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &att.ID,
			kind, domain.ActorScheduler, map[string]any{
				"reason": decision.Reason, "attempts_used": attemptsUsed,
				"max_attempts": maxAttempts, "error_class": class.String(),
			}); err != nil {
			return decision, time.Time{}, err
		}
		return decision, time.Time{}, nil
	}

	delay := domain.Backoff(policy, att.AttemptNumber-att.RetryFloor)
	payload := map[string]any{
		"run_id": att.RunID.String(), "node_id": att.NodeID,
		"next_attempt_number": att.AttemptNumber + 1,
		"reason":              decision.Reason,
	}
	if preempted {
		// The node is gone, not the task broken: a short fixed delay, and the
		// retry timer charges preemptions_used instead of attempts_used.
		delay = s.cfg.PreemptionRetryDelay
		payload["charge"] = chargePreemption
	}
	fireAt := now.Add(delay)
	if err := insertTimer(ctx, tx, domain.TimerRetryBackoff, att.ID, fireAt, payload); err != nil {
		return decision, time.Time{}, err
	}
	if err := appendEvent(ctx, tx, att.RunID, att.NodeID, &att.ID,
		domain.EventRetryScheduled, domain.ActorScheduler, map[string]any{
			"reason": decision.Reason, "backoff_ms": delay.Milliseconds(),
			"next_attempt_number": att.AttemptNumber + 1,
		}); err != nil {
		return decision, time.Time{}, err
	}
	return decision, fireAt, nil
}

// outcomeToState maps the wire outcome onto the attempt state, and recovers the
// error class the retry decision needs.
//
// INDETERMINATE maps to LOST rather than FAILED. That is invariant 2 and it is
// not a technicality: FAILED means we KNOW the work ran and did not succeed,
// while LOST means we do not know whether it ran at all — and the retry policy
// for a task declaring IDEMPOTENCY_MODE_NONE differs completely between the two.
func outcomeToState(o nexusv1.AttemptOutcome, te *nexusv1.TaskError) (string, nexusv1.ErrorClass) {
	class := te.GetClass()
	switch o {
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED:
		return string(domain.AttemptSucceeded), class
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT:
		if class == nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED {
			class = nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT
		}
		return string(domain.AttemptTimedOut), class
	case nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_INDETERMINATE:
		if class == nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED {
			class = nexusv1.ErrorClass_ERROR_CLASS_WORKER_LOST
		}
		return string(domain.AttemptLost), class
	default:
		// UNSPECIFIED collapses to FAILED. A worker that reported a terminal
		// result without saying which one has definitely stopped working on it,
		// and leaving the attempt non-terminal would hold its capacity forever.
		if class == nexusv1.ErrorClass_ERROR_CLASS_UNSPECIFIED {
			class = nexusv1.ErrorClass_ERROR_CLASS_APPLICATION
		}
		return string(domain.AttemptFailed), class
	}
}

// PendingCancelAttempts moves every in-flight attempt of a run to CANCELLED.
//
// Used by the run-level cancel fan-out. The attempts are moved here rather than
// by workflow-manager because task_attempts is the scheduler's table, and because
// the capacity release has to happen in the same transaction as the state change.
func (s *SchedulerStore) CancelRunAttempts(ctx context.Context, runID uuid.UUID, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM task_attempts WHERE run_id = $1 AND state IN ('PENDING','DISPATCHED','RUNNING')`,
		runID)
	if err != nil {
		return 0, fmt.Errorf("list attempts of run %s: %w", runID, err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan attempt of run %s: %w", runID, err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate attempts of run %s: %w", runID, err)
	}

	// One transaction per attempt, as ReconcileExpiredLeases does: a single
	// transaction over a large run would hold locks on every worker row it
	// touches and block the dispatch path, which is the latency that matters.
	//
	// One attempt that cannot be cancelled now (busy past the lock timeout, or
	// a transient error) does not stop the rest: they are all attempted, and
	// the errors are returned together so the caller retries the fan-out,
	// which is idempotent because already-cancelled attempts are filtered out.
	var (
		cancelled int
		errs      []error
	)
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		ok, err := s.cancelOneAttempt(ctx, id, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("cancel attempt %s: %w", id, err))
			continue
		}
		if ok {
			cancelled++
		}
	}
	return cancelled, errors.Join(errs...)
}

func (s *SchedulerStore) cancelOneAttempt(ctx context.Context, attemptID uuid.UUID, now time.Time) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin cancel tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		runID    uuid.UUID
		nodeID   string
		workerID *uuid.UUID
		reqJSON  []byte
	)
	// Waits for the row (bounded) rather than skipping it, for the reason
	// ApplyResult does: skipping an attempt whose ReportStarted, or whose
	// dispatch claim, was mid-commit left it running under a cancelled run
	// (CONTRACT-FINDINGS F104).
	err = s.lockAttemptForTransition(ctx, tx, func() error {
		return tx.QueryRow(ctx, `
SELECT run_id, node_id, worker_id, resource_request
  FROM task_attempts
 WHERE id = $1 AND state IN ('PENDING','DISPATCHED','RUNNING')
 FOR UPDATE`, attemptID).Scan(&runID, &nodeID, &workerID, &reqJSON)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock attempt %s for cancel: %w", attemptID, err)
	}

	tag, err := tx.Exec(ctx, `
UPDATE task_attempts
   SET state = 'CANCELLED', ended_at = $2, state_version = state_version + 1
 WHERE id = $1 AND state IN ('PENDING','DISPATCHED','RUNNING')`, attemptID, now)
	if err != nil {
		return false, fmt.Errorf("cas attempt %s to CANCELLED: %w", attemptID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	req, err := decodeVector(reqJSON)
	if err != nil {
		return false, err
	}
	if err := s.releaseCapacity(ctx, tx, attemptID, workerID, req); err != nil {
		return false, err
	}
	if err := deleteTimersFor(ctx, tx, attemptID,
		domain.TimerScheduleToStart, domain.TimerStartToClose,
		domain.TimerHeartbeat, domain.TimerLeaseExpiry, domain.TimerRetryBackoff); err != nil {
		return false, err
	}
	// The lease row is left in place on purpose, unlike in ApplyResult. The
	// worker has not acknowledged the cancel yet; the CANCEL directive rides its
	// next heartbeat, and until then a report from it should be fenced on a token
	// mismatch rather than on a missing lease, so the reason it gets back is
	// accurate.
	if err := appendEvent(ctx, tx, runID, nodeID, &attemptID,
		"AttemptCancelled", domain.ActorScheduler, map[string]any{
			"cancelled_at": now.UTC().Format(time.RFC3339Nano),
		}); err != nil {
		return false, err
	}
	// A cancelled attempt that was DISPATCHED or RUNNING held capacity until
	// now and is metered like any other end; one cancelled while PENDING
	// never did, and recordUsage writes nothing for it (ADR-0021).
	usage, err := s.recordUsage(ctx, tx, attemptID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit cancel tx: %w", err)
	}
	s.notifyUsage(usage)
	return true, nil
}

// nullableJSON maps an empty buffer to SQL NULL so a column declared nullable
// stores NULL rather than the literal bytes "null".
func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
