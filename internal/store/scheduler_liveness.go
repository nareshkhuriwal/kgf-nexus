package store

import (
	"context"
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
 WHERE state IN ('ACTIVE','DRAINING')
   AND last_heartbeat_at <= $1
   AND $3 = ANY(queues)
 ORDER BY last_heartbeat_at
 LIMIT $2`, cutoff, limit, s.cfg.Queue)
	if err != nil {
		return nil, fmt.Errorf("select stale workers: %w", err)
	}
	type stale struct {
		id   uuid.UUID
		host string
		hb   time.Time
	}
	var list []stale
	for rows.Next() {
		var st stale
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

	var out []DeadWorker
	for _, st := range list {
		dead, err := s.killWorker(ctx, now, st.id, st.host, st.hb, cutoff)
		if err != nil {
			return out, fmt.Errorf("declare worker %s dead: %w", st.id, err)
		}
		if dead != nil {
			out = append(out, *dead)
		}
	}
	return out, nil
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
   AND state IN ('ACTIVE','DRAINING')
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

	// Expire everything it held. Each attempt is its own transaction (see
	// ReconcileExpiredLeases) so one bad row cannot block the rest.
	attemptIDs, err := s.attemptsHeldBy(ctx, workerID)
	if err != nil {
		return dead, err
	}
	for _, id := range attemptIDs {
		lost, err := s.expireOne(ctx, now, id, domain.EventWorkerDead, "worker_dead")
		if err != nil {
			return dead, fmt.Errorf("expire attempt %s of dead worker: %w", id, err)
		}
		if lost != nil {
			dead.ExpiredAttempts = append(dead.ExpiredAttempts, *lost)
		}
	}
	return dead, nil
}

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
