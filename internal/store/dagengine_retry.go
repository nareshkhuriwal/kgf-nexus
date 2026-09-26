// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// Operator retry, the DAG engine's I/O half. Which nodes a retry resets is
// decided by the pure planner (internal/dag.PlanRetry); this file only makes
// one plan durable, atomically.

// ErrNodeRetryConflict means a node the retry planned to reset was no longer in
// the state the plan was computed from -- a concurrent retry, cancel or reconcile
// moved it first -- or had an attempt in flight. Nothing was reset. A distinct
// error so the caller can answer "conflict, re-read and try again" rather than
// "internal error".
var ErrNodeRetryConflict = errors.New("a node changed state while the retry was being applied")

// NodeRetryReset is one planned reset, applied as a compare-and-set on From.
type NodeRetryReset struct {
	NodeID string
	From   domain.NodeState
	To     domain.NodeState
}

// ApplyRetryResets resets a retry plan's nodes in ONE transaction.
//
// Each node is compare-and-set from the state the plan saw, and moves its
// retry_floor up to its highest existing attempt_number (migration 0026), so
// the engine stops deriving the node from attempts the retry superseded. The
// attempts themselves are not touched: history is immutable (invariant 9), the
// next attempt is attempt_number = MAX + 1, and it carries the same idempotency
// key as every earlier try because the key excludes attempt_number
// (invariant 1).
//
// attempts_used returns to 0, so a retried node gets its declared retry budget
// again. An operator retrying a node whose budget ran out is asking for that
// work to be tried again; granting it one attempt with no automatic retry
// would make the retry weaker than the original run for no stated reason.
//
// ALL OR NOTHING. A single CAS miss, or an attempt found in flight, rolls back
// every reset and returns ErrNodeRetryConflict. A half-applied retry -- the
// selected node reset but a dependent not, because a concurrent writer moved
// it -- would leave a graph that is neither the old outcome nor the requested
// retry, and nothing would ever reconcile it back to either.
func (s *DAGEngineStore) ApplyRetryResets(ctx context.Context, runID string, resets []NodeRetryReset) error {
	if len(resets) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin retry tx for run %s: %w", runID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids := make([]string, len(resets))
	for i, r := range resets {
		ids[i] = r.NodeID
	}

	const cas = `
		UPDATE task_nodes n
		   SET state = $3::node_state,
		       attempts_used = 0,
		       free_retries = 0,
		       preemptions_used = 0,
		       retry_floor = GREATEST(n.retry_floor, COALESCE(
		           (SELECT MAX(a.attempt_number) FROM task_attempts a
		             WHERE a.run_id = n.run_id AND a.node_id = n.node_id), 0))
		 WHERE n.run_id = $1::uuid AND n.node_id = $2 AND n.state = $4::node_state`
	for _, r := range resets {
		tag, uerr := tx.Exec(ctx, cas, runID, r.NodeID, string(r.To), string(r.From))
		if uerr != nil {
			return fmt.Errorf("reset node %s/%s for retry: %w", runID, r.NodeID, uerr)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: node %s is no longer %s", ErrNodeRetryConflict, r.NodeID, r.From)
		}
	}

	// Checked AFTER the node CASes, which row-lock the nodes: an attempt that
	// was in flight when the plan was made and is still in flight now would run
	// beside the retry's new attempt. The planner refuses in-flight nodes too;
	// this is the check that holds under concurrency.
	var busy string
	err = tx.QueryRow(ctx, `
		SELECT node_id FROM task_attempts
		 WHERE run_id = $1::uuid AND node_id = ANY($2)
		   AND state IN ('PENDING','DISPATCHED','RUNNING')
		 ORDER BY node_id LIMIT 1`, runID, ids).Scan(&busy)
	switch {
	case err == nil:
		return fmt.Errorf("%w: node %s has an attempt in flight", ErrNodeRetryConflict, busy)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("check in-flight attempts for run %s: %w", runID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit retry tx for run %s: %w", runID, err)
	}
	return nil
}

// InFlightNodes names the nodes of a run that have a non-terminal attempt.
func (s *DAGEngineStore) InFlightNodes(ctx context.Context, runID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT node_id FROM task_attempts
		 WHERE run_id = $1::uuid AND state IN ('PENDING','DISPATCHED','RUNNING')`, runID)
	if err != nil {
		return nil, fmt.Errorf("list in-flight nodes for run %s: %w", runID, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan in-flight node: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}
