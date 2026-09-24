// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Worker quarantine (§5.5, §10): the poison-host circuit breaker, owned by the
// resource manager because it owns `workers` and is therefore the only service
// that can move workers.state atomically with the episode row that explains it.
//
// DEFINITION used throughout: a worker is QUARANTINED when it receives no new
// dispatches because its own attempts -- across DIFFERENT tasks -- keep failing
// with infrastructure-class errors (ERROR_CLASS_INFRASTRUCTURE, or the attempt
// was LOST / ERROR_CLASS_WORKER_LOST while the worker stayed alive), or because
// an operator said so. Work already running on it is left to finish: quarantine
// stops FEEDING a host, it does not kill what it holds (that is cancel/drain).
//
// Placement needs no change to honour it: every claim query admits only
// state = 'ACTIVE' workers, so QUARANTINED is excluded from dispatch by the
// same predicate that already excludes DRAINING and DEAD.

// QuarantineTrigger says why an episode began.
type QuarantineTrigger string

const (
	QuarantineAuto      QuarantineTrigger = "auto"
	QuarantineManual    QuarantineTrigger = "manual"
	QuarantineInherited QuarantineTrigger = "inherited"
)

// Quarantine is one episode row.
type Quarantine struct {
	ID            string
	WorkerID      string
	Namespace     string
	Hostname      string
	Trigger       QuarantineTrigger
	Reason        string
	Evidence      map[string]any
	Actor         string
	StartedAt     time.Time
	ExpiresAt     *time.Time
	ReleasedAt    *time.Time
	ReleasedBy    *string
	ReleaseReason *string
}

// ErrNotQuarantinable is returned when a worker is in a state quarantine does
// not apply to (DEAD, DRAINING, or already QUARANTINED).
var ErrNotQuarantinable = errors.New("worker is not ACTIVE")

// QuarantineWorker moves an ACTIVE worker to QUARANTINED and opens an episode,
// in ONE transaction. applied is false when the worker was not ACTIVE -- a
// concurrent detector on another replica won, or an operator got there first --
// which callers treat as "already handled", not as an error.
//
// expiresAt nil means indefinite: only an operator (or re-activation) ends it.
func (s *RMStore) QuarantineWorker(
	ctx context.Context, workerID string, trigger QuarantineTrigger,
	reason, actor string, expiresAt *time.Time, evidence map[string]any,
) (applied bool, err error) {
	ev, err := json.Marshal(orEmptyAny(evidence))
	if err != nil {
		return false, fmt.Errorf("store: marshal quarantine evidence: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: begin quarantine tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ns, host string
	err = tx.QueryRow(ctx, `
		UPDATE workers SET state = 'QUARANTINED'
		 WHERE id = $1 AND state = 'ACTIVE'
		RETURNING namespace, hostname`, workerID).Scan(&ns, &host)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: quarantine worker %s: %w", workerID, err)
	}
	// The partial unique index (one open episode per worker) is the backstop
	// against a second episode; the state CAS above already serializes us.
	if _, err := tx.Exec(ctx, `
		INSERT INTO worker_quarantines
		  (worker_id, namespace, hostname, trigger_kind, reason, evidence, actor, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		workerID, ns, host, string(trigger), reason, ev, actor, expiresAt); err != nil {
		return false, fmt.Errorf("store: open quarantine episode for %s: %w", workerID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit quarantine of %s: %w", workerID, err)
	}
	return true, nil
}

// ReleaseQuarantine returns a QUARANTINED worker to ACTIVE and closes every
// open episode for its HOST, in one transaction.
//
// The host, not just the session: a manual release is an operator's statement
// that the host is healthy, and leaving an older session's episode open would
// quietly re-quarantine the next session the host registers. applied is false
// when the worker was not QUARANTINED (released already, or DEAD).
func (s *RMStore) ReleaseQuarantine(ctx context.Context, workerID, actor, reason string) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: begin release tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ns, host string
	err = tx.QueryRow(ctx, `
		UPDATE workers SET state = 'ACTIVE'
		 WHERE id = $1 AND state = 'QUARANTINED'
		RETURNING namespace, hostname`, workerID).Scan(&ns, &host)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: release worker %s: %w", workerID, err)
	}
	if err := closeHostEpisodes(ctx, tx, ns, host, actor, reason); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit release of %s: %w", workerID, err)
	}
	return true, nil
}

func closeHostEpisodes(ctx context.Context, tx pgx.Tx, ns, host, actor, reason string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE worker_quarantines
		   SET released_at = now(), released_by = $3, release_reason = $4
		 WHERE namespace = $1 AND hostname = $2 AND released_at IS NULL`,
		ns, host, actor, reason); err != nil {
		return fmt.Errorf("store: close quarantine episodes for %s/%s: %w", ns, host, err)
	}
	return nil
}

// ExpireQuarantines releases every episode whose expires_at has passed.
//
// A live QUARANTINED worker goes back to ACTIVE; an episode whose worker has
// since died is simply closed (there is nothing to reactivate, and leaving it
// open would make the host's next session inherit a quarantine that already
// ended). Each episode is its own transaction, so one bad row cannot block the
// rest, and a replica racing us finds nothing left to do.
func (s *RMStore) ExpireQuarantines(ctx context.Context, now time.Time) ([]Quarantine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT q.id, q.worker_id, q.namespace, q.hostname
		  FROM worker_quarantines q
		 WHERE q.released_at IS NULL AND q.expires_at IS NOT NULL AND q.expires_at <= $1
		 ORDER BY q.expires_at
		 LIMIT 500`, now)
	if err != nil {
		return nil, fmt.Errorf("store: list expired quarantines: %w", err)
	}
	var due []Quarantine
	for rows.Next() {
		var q Quarantine
		if err := rows.Scan(&q.ID, &q.WorkerID, &q.Namespace, &q.Hostname); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan expired quarantine: %w", err)
		}
		due = append(due, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate expired quarantines: %w", err)
	}

	var released []Quarantine
	for _, q := range due {
		ok, err := s.expireOne(ctx, q, now)
		if err != nil {
			return released, err
		}
		if ok {
			released = append(released, q)
		}
	}
	return released, nil
}

func (s *RMStore) expireOne(ctx context.Context, q Quarantine, now time.Time) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("store: begin expiry tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tag, err := tx.Exec(ctx, `
		UPDATE worker_quarantines
		   SET released_at = $2, released_by = 'system:quarantine_expiry', release_reason = 'expired'
		 WHERE id = $1 AND released_at IS NULL`, q.ID, now)
	if err != nil {
		return false, fmt.Errorf("store: close expired quarantine %s: %w", q.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	// Only reactivate when no OTHER open episode still holds the worker (e.g. an
	// indefinite manual quarantine layered over an expiring automatic one cannot
	// happen per worker, but an inherited one may outlive its source's row).
	if _, err := tx.Exec(ctx, `
		UPDATE workers SET state = 'ACTIVE'
		 WHERE id = $1 AND state = 'QUARANTINED'
		   AND NOT EXISTS (SELECT 1 FROM worker_quarantines
		                    WHERE worker_id = $1 AND released_at IS NULL)`, q.WorkerID); err != nil {
		return false, fmt.Errorf("store: reactivate worker %s: %w", q.WorkerID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("store: commit expiry of %s: %w", q.ID, err)
	}
	return true, nil
}

// InheritHostQuarantine quarantines a freshly registered session when its host
// has an open episode, carrying the remaining expiry over. Returns whether it
// did. Worker identity is per session (§5.5); without this, restarting the agent
// on a poison host would launder it straight back into the pool.
func (s *RMStore) InheritHostQuarantine(ctx context.Context, workerID string) (bool, error) {
	var ns, host string
	if err := s.pool.QueryRow(ctx, `SELECT namespace, hostname FROM workers WHERE id = $1`, workerID).
		Scan(&ns, &host); err != nil {
		return false, fmt.Errorf("store: load worker %s for inheritance: %w", workerID, err)
	}
	var srcID string
	var expires *time.Time
	var reason string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, expires_at, reason FROM worker_quarantines
		 WHERE namespace = $1 AND hostname = $2 AND released_at IS NULL
		   AND worker_id <> $3 AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY started_at DESC LIMIT 1`, ns, host, workerID).Scan(&srcID, &expires, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: look up host quarantine for %s/%s: %w", ns, host, err)
	}
	return s.QuarantineWorker(ctx, workerID, QuarantineInherited,
		"host "+host+" is quarantined: "+reason, "system:quarantine_inheritance", expires,
		map[string]any{"inherited_from_episode": srcID})
}

// PoisonConfig tunes the detector.
type PoisonConfig struct {
	// Window is how far back the evidence reaches.
	Window time.Duration
	// MinFailures is the number of infrastructure-class failures in the window
	// that makes a worker a candidate.
	MinFailures int
	// MinDistinctTasks is how many DIFFERENT task_refs those failures must span.
	// This is what makes the verdict worker-correlated rather than task-
	// correlated (§10: a poison TASK fails on every worker and must exhaust its
	// own retries, not take the fleet with it).
	MinDistinctTasks int
	// MinFailureRatio is the fraction of the worker's terminal attempts in the
	// window that must be infrastructure failures.
	MinFailureRatio float64
}

// PoisonCandidate is a worker the evidence says to quarantine.
type PoisonCandidate struct {
	WorkerID      string
	Namespace     string
	Hostname      string
	InfraFailures int
	Terminal      int
	DistinctTasks int
	Classes       []string
}

// FindPoisonWorkers returns ACTIVE workers whose recent attempts meet every
// threshold in cfg. Read-only; the caller decides (fleet cap) and acts.
//
// Infrastructure-class means the worker, not the task, is the likely cause:
// ERROR_CLASS_INFRASTRUCTURE (executor/runtime/result-store failure on that
// host) and ERROR_CLASS_WORKER_LOST (the worker lost the attempt while staying
// registered). Application errors, validation errors, timeouts and OOMs are the
// task's own behaviour and never count.
func (s *RMStore) FindPoisonWorkers(ctx context.Context, now time.Time, cfg PoisonConfig) ([]PoisonCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT w.id::text, w.namespace, w.hostname,
		       count(*) FILTER (WHERE x.infra)                         AS infra,
		       count(*)                                                AS terminal,
		       count(DISTINCT n.task_ref) FILTER (WHERE x.infra)       AS tasks,
		       array_remove(array_agg(DISTINCT x.class) FILTER (WHERE x.infra), NULL) AS classes
		  FROM workers w
		  JOIN task_attempts a ON a.worker_id = w.id
		                      AND a.ended_at IS NOT NULL
		                      AND a.ended_at > $1
		  JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
		  CROSS JOIN LATERAL (
		      SELECT a.error->>'class' AS class,
		             (a.error->>'class' IN ('ERROR_CLASS_INFRASTRUCTURE','ERROR_CLASS_WORKER_LOST')
		              OR a.state = 'LOST') AS infra
		  ) x
		 WHERE w.state = 'ACTIVE'
		   AND a.state IN ('SUCCEEDED','FAILED','TIMED_OUT','LOST','CANCELLED')
		 GROUP BY w.id, w.namespace, w.hostname
		HAVING count(*) FILTER (WHERE x.infra) >= $2
		   AND count(DISTINCT n.task_ref) FILTER (WHERE x.infra) >= $3
		   AND count(*) FILTER (WHERE x.infra)::float8 / count(*) >= $4
		 ORDER BY infra DESC, w.id`,
		now.Add(-cfg.Window), cfg.MinFailures, cfg.MinDistinctTasks, cfg.MinFailureRatio)
	if err != nil {
		return nil, fmt.Errorf("store: find poison workers: %w", err)
	}
	defer rows.Close()
	var out []PoisonCandidate
	for rows.Next() {
		var c PoisonCandidate
		if err := rows.Scan(&c.WorkerID, &c.Namespace, &c.Hostname,
			&c.InfraFailures, &c.Terminal, &c.DistinctTasks, &c.Classes); err != nil {
			return nil, fmt.Errorf("store: scan poison candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FleetCounts reports ACTIVE and QUARANTINED workers in a namespace, for the
// detector's fleet cap.
func (s *RMStore) FleetCounts(ctx context.Context, namespace string) (active, quarantined int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'ACTIVE'),
		       count(*) FILTER (WHERE state = 'QUARANTINED')
		  FROM workers WHERE namespace = $1`, namespace).Scan(&active, &quarantined)
	if err != nil {
		return 0, 0, fmt.Errorf("store: fleet counts for %s: %w", namespace, err)
	}
	return active, quarantined, nil
}

// RecentAutoQuarantines counts a host's automatic episodes since `since`, for
// escalating the duration of a host that keeps relapsing.
func (s *RMStore) RecentAutoQuarantines(ctx context.Context, namespace, hostname string, since time.Time) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM worker_quarantines
		 WHERE namespace = $1 AND hostname = $2 AND trigger_kind = 'auto' AND started_at > $3`,
		namespace, hostname, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count recent quarantines for %s/%s: %w", namespace, hostname, err)
	}
	return n, nil
}

// ListQuarantines returns a worker's episodes, newest first.
func (s *RMStore) ListQuarantines(ctx context.Context, workerID string) ([]Quarantine, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, worker_id::text, namespace, hostname, trigger_kind, reason, evidence,
		       actor, started_at, expires_at, released_at, released_by, release_reason
		  FROM worker_quarantines WHERE worker_id = $1 ORDER BY started_at DESC LIMIT 50`, workerID)
	if err != nil {
		return nil, fmt.Errorf("store: list quarantines for %s: %w", workerID, err)
	}
	defer rows.Close()
	var out []Quarantine
	for rows.Next() {
		var q Quarantine
		var trig string
		var ev []byte
		if err := rows.Scan(&q.ID, &q.WorkerID, &q.Namespace, &q.Hostname, &trig, &q.Reason, &ev,
			&q.Actor, &q.StartedAt, &q.ExpiresAt, &q.ReleasedAt, &q.ReleasedBy, &q.ReleaseReason); err != nil {
			return nil, fmt.Errorf("store: scan quarantine: %w", err)
		}
		q.Trigger = QuarantineTrigger(trig)
		_ = json.Unmarshal(ev, &q.Evidence)
		out = append(out, q)
	}
	return out, rows.Err()
}

func orEmptyAny(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
