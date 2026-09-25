// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AttemptAssignmentRow is what a worker was (or would be) handed for one
// attempt, read back for replay (ADR-0022): the node's TaskSpec and the
// attempt's resolved inputs, exactly as the scheduler reads them when it builds
// a TaskAssignment (assignmentDetails). The lease token is deliberately not
// selected -- it is a credential, and replay is not the attempt.
type AttemptAssignmentRow struct {
	AttemptID      string
	RunID          string
	NodeID         string
	AttemptNumber  int32
	State          string
	IdempotencyKey string
	// SpecJSON is task_nodes.spec_json: protojson with proto names.
	SpecJSON []byte
	// InputRefsJSON is task_attempts.input_refs: input name -> ResultRef
	// (protojson; inline payloads are base64).
	InputRefsJSON []byte
	ParentNodeID  *string
	MapIndex      *int32
	SpecName      string
	SpecVersion   int32
}

// GatewayGetAttemptAssignment loads one attempt's assignment, SCOPED BY
// NAMESPACE in the predicate: an attempt of another namespace is ErrNotFound,
// indistinguishable from one that does not exist. runID "" matches any run of
// the namespace (the REST "-" wildcard).
func GatewayGetAttemptAssignment(ctx context.Context, q GWQuerier, namespace, runID, attemptID string) (AttemptAssignmentRow, error) {
	var a AttemptAssignmentRow
	err := q.QueryRow(ctx, `
		SELECT a.id::text, a.run_id::text, a.node_id, a.attempt_number, a.state::text,
		       a.idempotency_key, COALESCE(n.spec_json, '{}'::jsonb)::text,
		       COALESCE(a.input_refs, '{}'::jsonb)::text, n.parent_node_id, n.map_index,
		       s.name, s.version
		  FROM task_attempts a
		  JOIN workflow_runs r ON r.id = a.run_id
		  JOIN workflow_specs s ON s.id = r.spec_id
		  LEFT JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
		 WHERE a.id = $1::uuid AND r.namespace = $2 AND ($3 = '' OR a.run_id = $3::uuid)
		   AND NOT EXISTS (SELECT 1 FROM run_deletions d WHERE d.run_id = r.id)`,
		attemptID, namespace, runID,
	).Scan(&a.AttemptID, &a.RunID, &a.NodeID, &a.AttemptNumber, &a.State,
		&a.IdempotencyKey, &a.SpecJSON, &a.InputRefsJSON, &a.ParentNodeID, &a.MapIndex,
		&a.SpecName, &a.SpecVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptAssignmentRow{}, ErrNotFound
	}
	if err != nil {
		return AttemptAssignmentRow{}, fmt.Errorf("get attempt assignment: %w", err)
	}
	return a, nil
}
