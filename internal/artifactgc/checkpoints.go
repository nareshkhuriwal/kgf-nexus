// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package artifactgc

import (
	"context"
	"fmt"
	"strings"
)

// Checkpoint objects (ADR-0032 §4, migration 0036).
//
// Agents write each checkpoint to a NEW object
// <namespace>/checkpoints/<idempotency_key>/<attempt_id>/<seq> and then move
// the idempotency key's pointer in task_checkpoints to it, forward only. Before
// this file the sweep did not know the table, so every checkpoint -- including
// the one a retrying shard is about to resume from -- looked like an orphan.
//
// Which checkpoint keys are live: the POINTER of every node that has not
// SUCCEEDED. That covers a node still running, one waiting for a retry, and a
// failed node an operator may retry later (§18), all of which resume from it.
//
// Everything else under a checkpoints/ prefix is an ordinary orphan, subject to
// the same grace period and list-then-query ordering as outputs:
//
//   - superseded checkpoints (the pointer moved past them);
//   - uploads a stale attempt made whose registration was fenced;
//   - the pointer of a SUCCEEDED node: the work is done and its outputs, not
//     its partial state, are what downstream reads;
//   - everything of a deleted run (the pointer row cascades with the run).
//
// The ordering argument holds for the same reason as for outputs and logs: the
// agent writes the object strictly before the RPC that references it.
var checkpointRefSources = []string{"task_checkpoints.object_key"}

// LiveCheckpointKeys returns the checkpoint objects a resume could still read.
func LiveCheckpointKeys(ctx context.Context, q Querier) (map[string]struct{}, error) {
	live := make(map[string]struct{})
	rows, err := q.Query(ctx, `
		SELECT c.object_key
		  FROM task_checkpoints c
		  LEFT JOIN task_nodes n ON n.run_id = c.run_id AND n.node_id = c.node_id
		 WHERE n.state IS DISTINCT FROM 'SUCCEEDED'`)
	if err != nil {
		// An error is never "no references" (LiveKeys' rule).
		return nil, fmt.Errorf("artifactgc: enumerate live checkpoint keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("artifactgc: scan live checkpoint key: %w", err)
		}
		live[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifactgc: iterate live checkpoint keys: %w", err)
	}
	return live, nil
}

// IsCheckpointKey reports whether a key is under a namespace's checkpoints/
// prefix.
func IsCheckpointKey(key string) bool {
	ns, rest, ok := strings.Cut(key, "/")
	return ok && ns != "" && strings.HasPrefix(rest, "checkpoints/")
}
