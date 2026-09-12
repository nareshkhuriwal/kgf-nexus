package store

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ReasonStaleLeaseToken is the exact string the frozen contract requires in
// Ack{accepted:false, reason:...}. It is a constant because worker agents match
// on it to decide to abandon an attempt, so a typo here would be a silent
// protocol break (CONTRACT.md invariant 3).
const ReasonStaleLeaseToken = "stale_lease_token"

// ErrLeaseNotFound means the attempt has no live lease at all — expired and
// reclaimed, or never dispatched. Indistinguishable from a stale token as far as
// the WORKER is concerned: both mean "stop, you are not the owner".
var ErrLeaseNotFound = errors.New("no lease for attempt")

// ErrStaleLeaseToken means a lease exists but the presented token is not it.
var ErrStaleLeaseToken = errors.New(ReasonStaleLeaseToken)

// LeaseView is the lease as the gateway needs to see it for fencing decisions.
type LeaseView struct {
	ID        string
	AttemptID string
	WorkerID  string
	ExpiresAt time.Time
	Epoch     int32
}

// GatewayVerifyLease is THE fencing check (ADR-0010, invariant 3). Every attempt
// RPC passes through here before anything is applied.
//
// Two properties that are easy to get wrong and are load-bearing:
//
//  1. The token comparison is constant-time. Lease tokens are unguessable
//     secrets; a byte-at-a-time == would leak a prefix oracle to any worker
//     that can call the RPC in a loop, letting it forge ownership of an attempt
//     it was never granted.
//
//  2. Expiry is evaluated against DATABASE time (now()), never the caller's
//     clock and never the gateway process's clock. Worker clocks are never
//     authoritative (invariant 7), and with several gateway replicas even their
//     own clocks disagree enough to accept a lease one replica considers dead.
//     One clock — Postgres's — makes the decision globally consistent.
//
// Fencing cannot stop a partitioned worker from EXECUTING; nothing can, over a
// network. It guarantees only that the zombie's report is rejected rather than
// applied. Idempotency (ADR-0006) covers the other half, the duplicate side
// effect. Neither mechanism alone is sufficient.
func GatewayVerifyLease(ctx context.Context, q GWQuerier, attemptID, presentedToken string) (LeaseView, error) {
	var lv LeaseView
	var storedToken string
	var expired bool
	err := q.QueryRow(ctx, `
		SELECT id::text, attempt_id::text, worker_id::text, token, expires_at, epoch,
		       (expires_at <= now()) AS expired
		  FROM leases
		 WHERE attempt_id = $1`,
		attemptID,
	).Scan(&lv.ID, &lv.AttemptID, &lv.WorkerID, &storedToken, &lv.ExpiresAt, &lv.Epoch, &expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return LeaseView{}, ErrLeaseNotFound
	}
	if err != nil {
		return LeaseView{}, fmt.Errorf("load lease for attempt %s: %w", attemptID, err)
	}

	if subtle.ConstantTimeCompare([]byte(storedToken), []byte(presentedToken)) != 1 {
		return LeaseView{}, ErrStaleLeaseToken
	}
	// An expired-but-matching token is still stale: the expiry timer may not have
	// reclaimed the row yet, but the attempt is already eligible to be declared
	// LOST and retried elsewhere. Accepting the report here would race that
	// retry and could mark SUCCEEDED an attempt whose replacement is running.
	if expired {
		return LeaseView{}, ErrStaleLeaseToken
	}
	return lv, nil
}

// GatewayWorkerActiveAttempts lists the attempts the CONTROL PLANE believes this
// worker is running. Compared against the worker's heartbeat ground truth to
// find leaked reservations.
func GatewayWorkerActiveAttempts(ctx context.Context, q GWQuerier, workerID string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text
		  FROM task_attempts
		 WHERE worker_id = $1
		   AND state IN ('DISPATCHED','RUNNING')`,
		workerID,
	)
	if err != nil {
		return nil, fmt.Errorf("list worker active attempts: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan attempt id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GatewayReservedAttempts lists attempts for which the capacity ledger still
// holds a reservation against this worker. This is the set reconciled against
// heartbeat ground truth; anything here that the worker does not acknowledge is
// a leak.
func GatewayReservedAttempts(ctx context.Context, q GWQuerier, workerID string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT attempt_id::text
		  FROM reservations
		 WHERE worker_id = $1`,
		workerID,
	)
	if err != nil {
		return nil, fmt.Errorf("list worker reservations: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan reservation attempt id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GatewayRenewLeases performs the BATCHED renewal for one worker.
//
// Batching is not an optimization detail, it is the design (ADR-0010): at 10k
// concurrent tasks with 5s renewal, per-attempt renewal is 2k RPS of pure
// overhead. One statement per worker collapses that to per-worker cost.
//
// Renewal is guarded by BOTH the token and the worker id, and computes the new
// expiry from database now(), so a worker cannot extend an attempt it does not
// own and cannot extend it further than the control plane's own clock allows.
// Attempts whose renewal does not land are returned as revoked; the worker must
// stop work on them.
func GatewayRenewLeases(ctx context.Context, q GWQuerier, workerID string, renewals map[string]string, ttl time.Duration) (granted []LeaseGrantRow, revoked []string, err error) {
	for attemptID, token := range renewals {
		var g LeaseGrantRow
		g.AttemptID = attemptID
		scanErr := q.QueryRow(ctx, `
			UPDATE leases
			   SET expires_at = now() + make_interval(secs => $3),
			       renewed_at = now()
			 WHERE attempt_id = $1
			   AND worker_id  = $2
			   AND token      = $4
			   AND expires_at > now()
			RETURNING expires_at, epoch`,
			attemptID, workerID, ttl.Seconds(), token,
		).Scan(&g.ExpiresAt, &g.Epoch)
		if errors.Is(scanErr, pgx.ErrNoRows) {
			// No live lease matched this (attempt, worker, token). Either it
			// expired, was reclaimed, or belongs to a different session. Telling
			// the worker to stop is always the safe answer: another attempt may
			// already be running, and any result it reports would be rejected on
			// token mismatch anyway.
			revoked = append(revoked, attemptID)
			continue
		}
		if scanErr != nil {
			return nil, nil, fmt.Errorf("renew lease for attempt %s: %w", attemptID, scanErr)
		}
		granted = append(granted, g)
	}
	return granted, revoked, nil
}

// LeaseGrantRow is one renewed lease, carrying a CONTROL-PLANE absolute deadline.
type LeaseGrantRow struct {
	AttemptID string
	ExpiresAt time.Time
	Epoch     int32
}
