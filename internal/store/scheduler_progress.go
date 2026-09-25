// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// ProgressReport is one ReportProgress call as the scheduler receives it.
//
// Message and Fraction are exactly what the worker sent. They are untrusted and
// are sanitized and validated here, not by the caller, so no path can store a
// raw value by forgetting to.
type ProgressReport struct {
	AttemptID uuid.UUID
	// LeaseToken is the token the worker presented. The gateway has already
	// fenced on it (invariant 3); it is checked AGAIN here, inside the write,
	// because the lease can be reclaimed between the gateway's check and this
	// transaction. Empty means the caller is a gateway that predates the field
	// (a rolling deploy): the heartbeat is still honoured as before, but nothing
	// is stored, because nothing here can prove the report is current.
	LeaseToken string
	Message    string
	Fraction   float64
}

// ProgressOutcome says what RecordProgress did.
type ProgressOutcome struct {
	// StaleLease is true when the presented token is not the attempt's live
	// lease (reclaimed, expired, or superseded by a newer lease epoch). Nothing
	// was written; the worker should stop, exactly as on any other attempt RPC.
	StaleLease bool
	// Stored is true when the message and/or fraction were written.
	Stored bool
	// EventRecorded is true when an AttemptProgress run event was appended.
	EventRecorded bool
	// FractionRejected is true when a non-zero fraction was refused as invalid
	// (NaN, infinite, or outside [0, 1]). The rest of the report still applied.
	FractionRejected bool
	// Truncated is true when the message was longer than the cap.
	Truncated bool
}

// progressEventKind is the run_events kind for a recorded progress change.
const progressEventKind = "AttemptProgress"

// RecordProgress applies one task-level progress report.
//
// Two independent effects, both fenced on the lease:
//
//  1. The latest message and fraction are stored in attempt_progress, for any
//     DISPATCHED or RUNNING attempt, whether or not the task declared a
//     heartbeat timeout. (The Kubernetes executor reports "pod Pending:
//     Unschedulable: ..." while the pod is still being placed, which is the
//     whole point of storing it.) A changed message may also append an
//     AttemptProgress run event, deduplicated and rate-limited by
//     domain.ProgressEventDue so a chatty worker cannot flood the append-only
//     history.
//  2. The heartbeat timer is re-armed, for a RUNNING attempt whose spec has a
//     heartbeat budget, as before.
//
// This never locks, UPDATEs or foreign-keys the task_attempts row. The
// terminal-transition paths lock that row with SKIP LOCKED and treat "no row"
// as "already handled", so holding it here, even briefly, could make a result
// that arrives in the same instant be silently skipped. Migration 0024 explains
// the side table this writes instead.
func (s *SchedulerStore) RecordProgress(ctx context.Context, rep ProgressReport, now time.Time) (ProgressOutcome, error) {
	var out ProgressOutcome

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, fmt.Errorf("begin progress tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The fence: the lease row as of this statement, with no row lock. A lock
	// here (FOR KEY SHARE, to hold off the lease DELETE until this commits) was
	// tried and deadlocked against ApplyResult in TestProgressNeverBlocksTheResult:
	// ApplyResult deletes the attempt's timers and then its lease, while this
	// transaction would have held the lease and then waited on the heartbeat
	// timer. The residual race it would have closed is harmless: a report that
	// was current when read may commit just after the lease ends, and it then
	// records the attempt's last status. It cannot reach any other attempt,
	// because the row is keyed by attempt id and a retry is a new attempt, and a
	// report from an OLDER lease generation of this attempt is refused below by
	// the epoch check under the attempt_progress row lock.
	fenced := rep.LeaseToken != ""
	var epoch int32
	if fenced {
		var (
			token   string
			expired bool
		)
		err = tx.QueryRow(ctx, `
SELECT token, epoch, (expires_at <= now())
  FROM leases WHERE attempt_id = $1`, rep.AttemptID).Scan(&token, &epoch, &expired)
		if errors.Is(err, pgx.ErrNoRows) {
			out.StaleLease = true
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("load lease for attempt %s progress: %w", rep.AttemptID, err)
		}
		// The same test as GatewayVerifyLease, and in constant time for the
		// same reason: the token is a credential. An expired-but-matching token
		// is stale too, because the attempt is already eligible to be LOST.
		if expired || subtle.ConstantTimeCompare([]byte(token), []byte(rep.LeaseToken)) != 1 {
			out.StaleLease = true
			return out, nil
		}
	}

	// The attempt itself is only READ: no FOR UPDATE, for the reason above.
	var (
		runID    uuid.UUID
		nodeID   string
		state    string
		workerID *uuid.UUID
		specJSON []byte
	)
	err = tx.QueryRow(ctx, `
SELECT a.run_id, a.node_id, a.state::text, a.worker_id, n.spec_json
  FROM task_attempts a
  JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
 WHERE a.id = $1`, rep.AttemptID).Scan(&runID, &nodeID, &state, &workerID, &specJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		// No such attempt. The gateway's fence would have rejected this already;
		// reaching here means a deleted run, and there is nothing to record.
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("load attempt %s for progress: %w", rep.AttemptID, err)
	}
	if state != string(domain.AttemptDispatched) && state != string(domain.AttemptRunning) {
		// Progress for an attempt that is not in flight is stale by state (a
		// cancelled attempt keeps its lease until the worker acknowledges).
		// Recording it would describe work that is over, and re-arming a timer
		// for it would keep a deadline alive forever.
		return out, nil
	}

	// Lock order: timers BEFORE the run row that appendEvent takes. Every
	// terminal path (ApplyResult, lease reclaim, cancel, timer expiry) deletes
	// the attempt's timers before it appends its event, so taking them in the
	// same order here is what keeps a progress burst and a result from
	// deadlocking each other.
	if state == string(domain.AttemptRunning) {
		var spec TaskSpecView
		if len(specJSON) > 0 {
			_ = json.Unmarshal(specJSON, &spec)
		}
		// The heartbeat timer is re-armed rather than merely touched, because the
		// deadline is a row in `timers` and nothing else in the system knows how
		// to move it. Delete-then-insert inside one transaction keeps the attempt
		// from being briefly unguarded. No budget declared means no heartbeat
		// timeout to push out.
		if spec.HeartbeatMs > 0 {
			if err := deleteTimersFor(ctx, tx, rep.AttemptID, domain.TimerHeartbeat); err != nil {
				return ProgressOutcome{}, err
			}
			if err := insertTimer(ctx, tx, domain.TimerHeartbeat, rep.AttemptID,
				now.Add(time.Duration(spec.HeartbeatMs)*time.Millisecond), nil); err != nil {
				return ProgressOutcome{}, err
			}
		}
	}

	if fenced {
		if err := storeProgress(ctx, tx, rep, now, runID, nodeID, workerID, epoch, &out); err != nil {
			return ProgressOutcome{}, err
		}
		if out.StaleLease {
			// Rolled back, heartbeat included: a superseded lease earns nothing.
			return out, nil
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return ProgressOutcome{}, fmt.Errorf("commit progress tx: %w", err)
	}
	return out, nil
}

// storeProgress upserts the attempt's progress row and, when due, appends the
// AttemptProgress event. It sets out.StaleLease when a newer lease epoch has
// already written the row.
func storeProgress(
	ctx context.Context, tx pgx.Tx, rep ProgressReport, now time.Time,
	runID uuid.UUID, nodeID string, workerID *uuid.UUID, epoch int32, out *ProgressOutcome,
) error {
	msg, truncated := domain.SanitizeProgressMessage(rep.Message)
	out.Truncated = truncated
	frac, fracOK, fracInvalid := domain.NormalizeProgressFraction(rep.Fraction)
	out.FractionRejected = fracInvalid

	// Create the row if this is the attempt's first report, then lock it. The
	// lock is on attempt_progress only, and it is what makes deduplication and
	// the event cap exact when two reports for one attempt race.
	if _, err := tx.Exec(ctx, `
INSERT INTO attempt_progress (attempt_id, run_id, lease_epoch, reported_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (attempt_id) DO NOTHING`, rep.AttemptID, runID, epoch, now); err != nil {
		return fmt.Errorf("create progress row for %s: %w", rep.AttemptID, err)
	}

	var (
		storedEpoch int32
		prevMsg     *string
		lastEventAt *time.Time
		eventCount  int
		suppressed  int
	)
	if err := tx.QueryRow(ctx, `
SELECT lease_epoch, message, last_event_at, event_count, events_suppressed
  FROM attempt_progress WHERE attempt_id = $1 FOR UPDATE`, rep.AttemptID).Scan(
		&storedEpoch, &prevMsg, &lastEventAt, &eventCount, &suppressed); err != nil {
		return fmt.Errorf("lock progress row for %s: %w", rep.AttemptID, err)
	}
	if storedEpoch > epoch {
		// A newer lease generation already reported. This report was fenced on
		// the lease that was current when it was read, but it must not
		// overwrite what the newer holder wrote.
		out.StaleLease = true
		return nil
	}

	prev := ""
	if prevMsg != nil {
		prev = *prevMsg
	}
	changed := msg != "" && msg != prev
	due := domain.ProgressEventDue(prev, msg, lastEventAt, eventCount, now)

	newEventAt, newCount, newSuppressed := lastEventAt, eventCount, suppressed
	if due {
		t := now
		newEventAt, newCount, newSuppressed = &t, eventCount+1, 0
	} else if changed {
		newSuppressed = suppressed + 1
	}

	if _, err := tx.Exec(ctx, `
UPDATE attempt_progress
   SET lease_epoch        = $2,
       reported_at        = $3,
       message            = COALESCE($4, message),
       message_changed_at = CASE WHEN $5 THEN $3 ELSE message_changed_at END,
       fraction           = CASE WHEN $6 THEN $7 ELSE fraction END,
       last_event_at      = $8,
       event_count        = $9,
       events_suppressed  = $10
 WHERE attempt_id = $1`,
		rep.AttemptID, epoch, now, nullString(msg), changed, fracOK, frac,
		newEventAt, newCount, newSuppressed); err != nil {
		return fmt.Errorf("store progress for %s: %w", rep.AttemptID, err)
	}
	out.Stored = msg != "" || fracOK

	if due {
		payload := map[string]any{
			"message": msg,
			"phase":   domain.ProgressPhase(msg),
		}
		if prev != "" {
			payload["previous_phase"] = domain.ProgressPhase(prev)
		}
		if fracOK {
			payload["fraction"] = frac
		}
		// Say what the rate limit skipped, so the history is not mistaken for
		// a complete record of every status the worker sent.
		if suppressed > 0 {
			payload["suppressed_changes"] = suppressed
		}
		if newCount == domain.MaxProgressEventsPerAttempt {
			payload["last_recorded"] = true
		}
		actor := domain.ActorScheduler
		if workerID != nil {
			actor = domain.ActorWorkerPrefix + workerID.String()
		}
		if err := appendEvent(ctx, tx, runID, nodeID, &rep.AttemptID,
			progressEventKind, actor, payload); err != nil {
			return err
		}
		out.EventRecorded = true
	}
	return nil
}
