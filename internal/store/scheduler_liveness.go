// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// DeadWorker is a worker the sweeper declared DEAD.
type DeadWorker struct {
	ID       uuid.UUID
	Hostname string
	// LastHeartbeat is reported so the log shows how far past the window it was,
	// which distinguishes a clean process death from a slow network.
	LastHeartbeat time.Time
	// ExpiredAttempts are the attempts it was holding, now LOST.
	ExpiredAttempts []LostAttempt
	// AlreadyDead is true when this pass did not declare the worker DEAD:
	// something else did (the resource-manager's sweeper, or an earlier pass
	// that could not finish) and this pass only expired the attempts it still
	// held. Callers count only the others as newly dead workers.
	AlreadyDead bool
}

// SweepDeadWorkers implements §5.5: a worker whose last heartbeat is older than
// the liveness window becomes DEAD, and every attempt it held is expired.
//
// This runs alongside the lease reconciler rather than instead of it, because
// they cover different failures at different speeds. Lease expiry catches a
// worker that died mid-task (per-attempt, fine-grained). The liveness sweep
// catches the fleet-level event fast and, critically, prevents the scheduler
// from continuing to place work on a host that is gone — a lease-only design
// would keep dispatching to a dead worker for as long as its capacity looked
// free.
//
// It does NOT wait for each lease to expire naturally before reclaiming the
// worker's capacity. Waiting would leave a dead machine's GPUs committed for a
// further lease TTL while the workflow stalls.
//
// NOTE ON OWNERSHIP: `workers` is a resource-manager table (ADR-0011), and in
// the target topology this sweep belongs there or behind its API. It lives here
// because the MVP has no resource-manager process yet AND because the transition
// must be atomic with expiring the worker's leases (which the scheduler does
// own) — a worker marked DEAD whose leases are still live would be dispatched-to
// by no one and recovered by no one. Flagged in the report.
func (s *SchedulerStore) SweepDeadWorkers(
	ctx context.Context, now time.Time, limit int,
) ([]DeadWorker, error) {
	if limit <= 0 {
		limit = 50
	}
	cutoff := now.Add(-s.cfg.LivenessWindow)

	// DRAINING workers are swept too: a worker that stops heartbeating while
	// draining is just as dead, and leaving it DRAINING forever would leak the
	// capacity of whatever it still held.
	//
	// Scoped to workers subscribed to this replica's queue, for the same reason
	// the lease reconciler is: a queue is the unit of scaling, and every replica
	// sweeping the whole fleet would have them all contending on every worker
	// row. A worker serving several queues is swept by whichever replica notices
	// first, which is correct — the DEAD transition is idempotent under its own
	// heartbeat re-check.
	rows, err := s.pool.Query(ctx, `
SELECT id, hostname, last_heartbeat_at
  FROM workers
 -- QUARANTINED workers still hold leases for the work they were running when
 -- quarantined, so one that goes silent must be declared DEAD like any other.
 WHERE state IN ('ACTIVE','DRAINING','QUARANTINED')
   AND last_heartbeat_at <= $1
   AND $3 = ANY(queues)
 ORDER BY last_heartbeat_at
 LIMIT $2`, cutoff, limit, s.cfg.Queue)
	if err != nil {
		return nil, fmt.Errorf("select stale workers: %w", err)
	}
	var list []staleWorker
	for rows.Next() {
		var st staleWorker
		if err := rows.Scan(&st.id, &st.host, &st.hb); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan stale worker: %w", err)
		}
		list = append(list, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stale workers: %w", err)
	}

	// Per-worker and per-attempt failures are collected, not returned on the
	// first one. A worker is committed DEAD before its attempts are expired, so
	// aborting here used to strand every remaining attempt of it -- and every
	// other stale worker in the list -- until lease expiry (F106). The pass
	// below picks the stranded ones up next time.
	var (
		out  []DeadWorker
		errs []error
		seen = make(map[uuid.UUID]bool, len(list))
	)
	for _, st := range list {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			return out, errors.Join(errs...)
		}
		seen[st.id] = true
		dead, err := s.killWorker(ctx, now, st.id, st.host, st.hb, cutoff)
		if err != nil {
			errs = append(errs, fmt.Errorf("declare worker %s dead: %w", st.id, err))
		}
		if dead != nil {
			out = append(out, *dead)
		}
	}

	// Workers someone else already declared DEAD but whose attempts are still
	// DISPATCHED/RUNNING. Two processes mark workers DEAD: this sweep and the
	// resource-manager's (services/resource-manager/.../sweeper.go), which
	// releases the capacity and deliberately leaves the attempts to the
	// scheduler. When it wins, the select above no longer sees the worker, and
	// before F106 nothing expired its attempts until each lease ran out -- up to
	// a full lease TTL of extra recovery. The same query also retries attempts a
	// previous pass of this sweep failed to expire.
	orphans, err := s.deadWorkersHoldingAttempts(ctx, limit)
	if err != nil {
		errs = append(errs, err)
		return out, errors.Join(errs...)
	}
	for _, st := range orphans {
		if seen[st.id] {
			continue // declared DEAD above; its failures are already reported
		}
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		dead := &DeadWorker{ID: st.id, Hostname: st.host, LastHeartbeat: st.hb, AlreadyDead: true}
		if err := s.expireHeldAttempts(ctx, now, dead); err != nil {
			errs = append(errs, fmt.Errorf("expire attempts of already-DEAD worker %s: %w", st.id, err))
		}
		if len(dead.ExpiredAttempts) > 0 {
			out = append(out, *dead)
		}
	}
	return out, errors.Join(errs...)
}

// staleWorker is a worker row selected by a liveness pass.
type staleWorker struct {
	id   uuid.UUID
	host string
	hb   time.Time
}

// deadWorkersHoldingAttempts lists DEAD workers that still hold DISPATCHED or
// RUNNING attempts on this replica's queue.
//
// Expiring those attempts is fenced exactly as for a worker this sweep kills:
// DEAD is terminal (SetWorkerState never revives it; the agent must
// re-register under a new id), so the worker can never again be the attempt's
// legitimate owner, and expireOne's state CAS plus lease delete reject any
// report its old session still sends. Scoped by the ATTEMPT's queue, like the
// lease reconciler, so each replica recovers only its own queue's work; the
// EXISTS is served by idx_attempts_worker_active.
func (s *SchedulerStore) deadWorkersHoldingAttempts(ctx context.Context, limit int) ([]staleWorker, error) {
	rows, err := s.pool.Query(ctx, `
SELECT w.id, w.hostname, w.last_heartbeat_at
  FROM workers w
 WHERE w.state = 'DEAD'
   AND EXISTS (SELECT 1 FROM task_attempts a
                WHERE a.worker_id = w.id
                  AND a.state IN ('DISPATCHED','RUNNING')
                  AND a.queue = $2)
 ORDER BY w.last_heartbeat_at
 LIMIT $1`, limit, s.cfg.Queue)
	if err != nil {
		return nil, fmt.Errorf("select dead workers still holding attempts: %w", err)
	}
	defer rows.Close()
	var out []staleWorker
	for rows.Next() {
		var st staleWorker
		if err := rows.Scan(&st.id, &st.host, &st.hb); err != nil {
			return nil, fmt.Errorf("scan dead worker: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// killWorker performs the DEAD transition and expires the worker's attempts.
//
// The worker transition and its attempt expiries are separate transactions on
// purpose. Doing the worker row first and committing means no further work can
// be placed on it even if the attempt recovery below is interrupted, so the
// worst interruption leaves attempts to the lease reconciler rather than leaving
// a dead worker still receiving tasks.
func (s *SchedulerStore) killWorker(
	ctx context.Context, now time.Time, workerID uuid.UUID, hostname string,
	lastHB, cutoff time.Time,
) (*DeadWorker, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin worker death tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Re-check the heartbeat inside the transaction. A worker that heartbeat
	// between our scan and here is alive, and killing it would needlessly cause
	// duplicate execution of everything it was running (ADR-0010: a false expiry
	// is a real cost, not a harmless precaution).
	tag, err := tx.Exec(ctx, `
UPDATE workers
   SET state = 'DEAD'
 WHERE id = $1
   AND state IN ('ACTIVE','DRAINING','QUARANTINED')
   AND last_heartbeat_at <= $2`, workerID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("mark worker DEAD: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit worker death: %w", err)
	}

	dead := &DeadWorker{ID: workerID, Hostname: hostname, LastHeartbeat: lastHB}
	return dead, s.expireHeldAttempts(ctx, now, dead)
}

// expireHeldAttempts expires every DISPATCHED/RUNNING attempt the DEAD worker
// holds, appending each to dead.ExpiredAttempts.
//
// Each attempt is its own transaction (see ReconcileExpiredLeases), and a
// failure on one does not stop the rest: the errors are joined and returned
// after every attempt has been tried. An attempt that fails, or that expireOne
// skips because another transaction holds its row, is still held by a DEAD
// worker, so the next pass's deadWorkersHoldingAttempts selects it again.
func (s *SchedulerStore) expireHeldAttempts(ctx context.Context, now time.Time, dead *DeadWorker) error {
	attemptIDs, err := s.attemptsHeldBy(ctx, dead.ID)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range attemptIDs {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		lost, err := s.expireOne(ctx, now, id, domain.EventWorkerDead, "worker_dead")
		if err != nil {
			errs = append(errs, fmt.Errorf("expire attempt %s of dead worker: %w", id, err))
			continue
		}
		if lost != nil {
			dead.ExpiredAttempts = append(dead.ExpiredAttempts, *lost)
		}
	}
	return errors.Join(errs...)
}

// attemptsHeldBy lists all of the worker's in-flight attempts. Not scoped to
// this replica's queue: a DEAD worker's attempts on any queue are equally
// orphaned, and a peer replica recovering the same attempt is a no-op under
// expireOne's SKIP LOCKED and state CAS.
func (s *SchedulerStore) attemptsHeldBy(ctx context.Context, workerID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id FROM task_attempts
 WHERE worker_id = $1 AND state IN ('DISPATCHED','RUNNING')`, workerID)
	if err != nil {
		return nil, fmt.Errorf("select attempts held by worker: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan held attempt: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Ready reports whether the store can serve traffic, for /readyz.
//
// It runs a real query rather than only pinging, because a pool can be healthy
// while the schema is missing or the service's DB role lacks the grants
// ADR-0011 rule 1 requires. A replica that cannot claim should not be in the
// load balancer, and "ping succeeded" would hide that.
func (s *SchedulerStore) Ready(ctx context.Context) error {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM task_attempts WHERE state = 'PENDING' AND queue = $1`,
		s.cfg.Queue).Scan(&n); err != nil {
		return fmt.Errorf("readiness probe query: %w", err)
	}
	return nil
}

// ReapDeadWorkers deletes DEAD worker rows whose last heartbeat is older than
// retain, returning how many were removed.
//
// DEAD rows are kept for a while on purpose: immediately after a worker dies,
// its row is the evidence an operator needs to understand why attempts were
// reclaimed, and run_events reference the worker id. But the row is retained
// forever today, so every deploy, restart or scale-down leaves another one and
// the fleet view fills with corpses — the console showed 6 DEAD rows against 2
// live workers after a single afternoon, which buries the signal that matters.
//
// Deletion is deliberately conditional on the worker holding NOTHING. A DEAD
// row with a surviving lease or reservation means the capacity ledger has not
// finished unwinding, and deleting it would cascade those rows away and lose
// the accounting rather than settle it. Such a row is left for the next sweep,
// by which time lease expiry will have released it.
func (s *SchedulerStore) ReapDeadWorkers(
	ctx context.Context, now time.Time, retain time.Duration, limit int,
) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM workers w
		 WHERE w.id IN (
		       SELECT id FROM workers
		        WHERE state = 'DEAD'
		          AND last_heartbeat_at < $1
		          AND NOT EXISTS (SELECT 1 FROM leases       l WHERE l.worker_id = workers.id)
		          AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.worker_id = workers.id)
		        ORDER BY last_heartbeat_at
		        LIMIT $2
		       )`,
		now.Add(-retain), limit)
	if err != nil {
		return 0, fmt.Errorf("reap dead workers: %w", err)
	}
	return tag.RowsAffected(), nil
}
