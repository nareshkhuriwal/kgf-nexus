// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"
)

// The durable run-reconcile queue (migration 0039, CONTRACT-FINDINGS F160).
//
// Rows are written by database triggers in the same transaction as the fact
// that needs a reconcile (a terminal attempt, a run state change that reopens
// the graph). The dag-engine owns the table and is its only consumer: it
// claims rows under a lease, reconciles each named run once, and deletes the
// rows only after a reconcile that began after they committed has succeeded.
//
// The ordering is what makes deletion safe. A row visible to a SELECT was
// committed before it, so the attempts it announces are visible to any
// reconcile that starts afterwards. Deleting BY ID exactly the rows read
// before the reconcile began can therefore never discard a fact the reconcile
// did not see; a high-water mark over id could (ids are handed out at INSERT
// and become visible at COMMIT, in a different order -- see 0016).

// ReconcileClaim is one run's share of a claimed batch.
type ReconcileClaim struct {
	RunID string
	IDs   []int64
	// Attempts is the highest failure count among the claimed rows, for the
	// consumer's backoff.
	Attempts int
}

// ClaimReconcileRequests leases up to limit due requests to owner and returns
// them grouped by run, oldest run first.
//
// SKIP LOCKED makes concurrent claims from several replicas disjoint without
// waiting on each other, and the lease (claimed_until) keeps a claimed row
// invisible to them afterwards, once the claim's own transaction has
// committed. A replica that dies holding a lease returns its rows by expiry.
//
// Two replicas can still hold rows for the SAME run -- one claimed a burst, the
// other the rows that arrived after it. Both then reconcile the run, which is
// safe (every write a reconcile makes is compare-and-set or ON CONFLICT DO
// NOTHING) and merely redundant.
func (s *DAGEngineStore) ClaimReconcileRequests(ctx context.Context, owner string, lease time.Duration, limit int) ([]ReconcileClaim, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE run_reconcile_requests q
		   SET claimed_by = $1, claimed_until = now() + $2::interval
		 WHERE q.id IN (
		         SELECT id FROM run_reconcile_requests
		          WHERE available_at <= now()
		            AND (claimed_until IS NULL OR claimed_until < now())
		          ORDER BY id
		          LIMIT $3
		          FOR UPDATE SKIP LOCKED)
		RETURNING q.id, q.run_id::text, q.attempts`,
		owner, lease.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim reconcile requests: %w", err)
	}
	defer rows.Close()

	byRun := map[string]*ReconcileClaim{}
	var order []string
	first := map[string]int64{}
	for rows.Next() {
		var (
			id       int64
			runID    string
			attempts int
		)
		if err := rows.Scan(&id, &runID, &attempts); err != nil {
			return nil, fmt.Errorf("scan reconcile request: %w", err)
		}
		c, ok := byRun[runID]
		if !ok {
			c = &ReconcileClaim{RunID: runID}
			byRun[runID] = c
			order = append(order, runID)
			first[runID] = id
		}
		c.IDs = append(c.IDs, id)
		if id < first[runID] {
			first[runID] = id
		}
		if attempts > c.Attempts {
			c.Attempts = attempts
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim reconcile requests: %w", err)
	}
	// RETURNING has no defined order; oldest request first keeps a steady
	// backlog fair.
	sortByFirst(order, first)
	out := make([]ReconcileClaim, 0, len(order))
	for _, r := range order {
		out = append(out, *byRun[r])
	}
	return out, nil
}

func sortByFirst(runs []string, first map[string]int64) {
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && first[runs[j]] < first[runs[j-1]]; j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}

// PendingReconcileRequestIDs snapshots the ids of the requests queued for a
// run, claimed or not, up to limit. Taken immediately BEFORE a reconcile, it
// is exactly the set that reconcile may acknowledge once it succeeds.
//
// Claimed rows are included on purpose: a reconcile that started after they
// committed covers them whichever replica holds the lease, and acknowledging
// them spares that replica a redundant pass (its own delete then finds
// nothing, which is fine).
func (s *DAGEngineStore) PendingReconcileRequestIDs(ctx context.Context, runID string, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 10000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM run_reconcile_requests WHERE run_id = $1::uuid ORDER BY id LIMIT $2`,
		runID, limit)
	if err != nil {
		return nil, fmt.Errorf("snapshot reconcile requests of run %s: %w", runID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan reconcile request id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// AckReconcileRequests deletes consumed requests by id and reports how many
// were still there.
func (s *DAGEngineStore) AckReconcileRequests(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM run_reconcile_requests WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, fmt.Errorf("acknowledge %d reconcile requests: %w", len(ids), err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseReconcileRequests returns requests whose reconcile failed, not
// before backoff from now, counting the failure and recording its cause.
//
// Requests are never dropped for failing: a run that needs a reconcile still
// needs one after the tenth failure, and giving up is exactly how runs got
// stranded. The backoff bounds what a persistently failing run costs.
func (s *DAGEngineStore) ReleaseReconcileRequests(ctx context.Context, ids []int64, backoff time.Duration, cause string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(cause) > 1024 {
		cause = cause[:1024]
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE run_reconcile_requests
		   SET claimed_by = NULL, claimed_until = NULL,
		       available_at = now() + $2::interval,
		       attempts = attempts + 1, last_error = $3
		 WHERE id = ANY($1)`,
		ids, backoff.String(), cause); err != nil {
		return fmt.Errorf("release %d reconcile requests: %w", len(ids), err)
	}
	return nil
}

// ReconcileQueueStats reports the backlog: how many requests are queued and
// how old the oldest is. Both are alarms rather than tuning inputs -- a
// healthy engine keeps the queue near empty, so a growing depth or age means
// reconciles are failing or the engine is not consuming.
func (s *DAGEngineStore) ReconcileQueueStats(ctx context.Context) (depth int64, oldest time.Duration, err error) {
	var ageSec float64
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
		  FROM run_reconcile_requests`).Scan(&depth, &ageSec); err != nil {
		return 0, 0, fmt.Errorf("reconcile queue stats: %w", err)
	}
	return depth, time.Duration(ageSec * float64(time.Second)), nil
}
