// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package store holds the pgx query layer. Each service owns only its own files
// here; this one and its siblings (dagengine_*.go) belong to the DAG engine.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"google.golang.org/protobuf/encoding/protojson"
)

// DAGEngineStore is the DAG engine's data access. It writes ONLY task_nodes and
// task_edges, the tables ADR-0011 assigns it, plus task_attempts INSERTs — see
// the ownership note on InsertPendingAttempt for why attempt CREATION is the
// engine's and attempt MUTATION is the scheduler's.
//
// Everything else (workflow_runs, workflow_specs, task_attempts state changes)
// is read-only from here. Reads are unrestricted by ADR-0011; writes are not.
type DAGEngineStore struct {
	pool *pgxpool.Pool
}

// NewDAGEngineStore wraps a pool. The pool is injected rather than constructed
// here so the service owns connection lifetime and a test can hand in its own.
func NewDAGEngineStore(pool *pgxpool.Pool) *DAGEngineStore {
	return &DAGEngineStore{pool: pool}
}

// Pool exposes the underlying pool for health checks.
func (s *DAGEngineStore) Pool() *pgxpool.Pool { return s.pool }

// ErrDAGEngineRunNotFound distinguishes "no such run" from a transport failure, so a
// caller can map it to NOT_FOUND rather than INTERNAL.
var ErrDAGEngineRunNotFound = errors.New("run not found")

// DERunRow is the subset of workflow_runs the engine needs. The engine never
// writes this table (workflow-manager owns it); it reads the pinned spec and the
// current state so it can respect operator interventions it does not own.
type DERunRow struct {
	Params       []byte
	ID           string
	SpecID       string
	Namespace    string
	State        domain.RunState
	Priority     int32
	StateVersion int64
}

// DENodeRow mirrors a task_nodes row.
type DENodeRow struct {
	RunID        string
	NodeID       string
	TaskRef      string
	State        domain.NodeState
	TriggerRule  string
	MaxAttempts  int32
	AttemptsUsed int32
	ParentNodeID *string
	MapIndex     *int32
	Spec         *nexusv1.TaskSpec
	// RetryFloor is the highest attempt_number an operator retry superseded
	// (migration 0026). Attempts at or below it are history: LoadAttempts does
	// not return them, and the retry budget counts from above it.
	RetryFloor int32
	// FreeRetries counts RATE_LIMITED retries not charged to the budget
	// (migration 0036, ADR-0032 §3); the exhaustion test subtracts it.
	FreeRetries int32
}

// DEEdgeRow mirrors a task_edges row.
type DEEdgeRow struct {
	RunID         string
	From          string
	To            string
	OutputToInput map[string]string
}

// DEAttemptRow is the subset of task_attempts the engine reads. The engine reads
// attempts to learn what actually happened; the scheduler owns their mutation.
type DEAttemptRow struct {
	ID            string
	RunID         string
	NodeID        string
	AttemptNumber int32
	State         domain.AttemptState
	OutputRef     []byte // raw JSONB; the engine forwards it without interpreting
	ErrorJSON     []byte
}

// LoadRun reads the run row. It does NOT lock: the engine's own writes are
// guarded by the node-state CAS in ApplyNodeState, and taking a run lock here
// would serialize every reconcile of the same run across replicas for no
// correctness gain.
func (s *DAGEngineStore) LoadRun(ctx context.Context, runID string) (*DERunRow, error) {
	const q = `SELECT id::text, spec_id::text, namespace, state::text, priority, state_version, params
	             FROM workflow_runs WHERE id = $1::uuid`
	var r DERunRow
	var state string
	err := s.pool.QueryRow(ctx, q, runID).Scan(
		&r.ID, &r.SpecID, &r.Namespace, &state, &r.Priority, &r.StateVersion, &r.Params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("load run %s: %w", runID, ErrDAGEngineRunNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	r.State = domain.RunState(state)
	return &r, nil
}

// LoadSpec reads and parses the run's PINNED spec. A run pins its spec version
// for its lifetime (ADR-0004), so the engine must always resolve the spec through
// the run rather than by (namespace, name) -- editing a workflow must never
// mutate a run in flight.
func (s *DAGEngineStore) LoadSpec(ctx context.Context, specID string) (*nexusv1.WorkflowSpec, error) {
	const q = `SELECT spec_json FROM workflow_specs WHERE id = $1::uuid`
	var raw []byte
	err := s.pool.QueryRow(ctx, q, specID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("load spec %s: %w", specID, ErrDAGEngineRunNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("load spec %s: %w", specID, err)
	}
	var spec nexusv1.WorkflowSpec
	// DiscardUnknown so a spec written by a NEWER service version still parses:
	// ADR-0011 requires services to stay compatible across one schema version,
	// and a rolling deploy will have mixed versions writing this table.
	// normalizeSpecEnums first: this document is the author's, so its enum values
	// may use the short authoring spellings the JSON Schema accepts, which protojson
	// discards WITHOUT error (see normalizeSpecEnums).
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normalizeSpecEnums(raw), &spec); err != nil {
		return nil, fmt.Errorf("parse spec %s: %w", specID, err)
	}
	return &spec, nil
}

// LoadNodes reads every node of a run.
func (s *DAGEngineStore) LoadNodes(ctx context.Context, runID string) ([]DENodeRow, error) {
	const q = `SELECT run_id::text, node_id, task_ref, state::text, trigger_rule,
	                  max_attempts, attempts_used, parent_node_id, map_index, spec_json, retry_floor, free_retries
	             FROM task_nodes WHERE run_id = $1::uuid ORDER BY node_id`
	rows, err := s.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("load nodes for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []DENodeRow
	for rows.Next() {
		var n DENodeRow
		var state string
		var raw []byte
		if err := rows.Scan(&n.RunID, &n.NodeID, &n.TaskRef, &state, &n.TriggerRule,
			&n.MaxAttempts, &n.AttemptsUsed, &n.ParentNodeID, &n.MapIndex, &raw, &n.RetryFloor, &n.FreeRetries); err != nil {
			return nil, fmt.Errorf("scan node row: %w", err)
		}
		n.State = domain.NodeState(state)
		var ts nexusv1.TaskSpec
		// Node rows are written by dagengine_writes with UseProtoNames, so they
		// normally need no rewriting; normalizing anyway costs one prefix check and
		// covers rows written before this fix.
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(normalizeSpecEnums(raw), &ts); err != nil {
			return nil, fmt.Errorf("parse spec for node %s/%s: %w", runID, n.NodeID, err)
		}
		n.Spec = &ts
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate node rows: %w", err)
	}
	return out, nil
}

// LoadEdges reads every edge of a run.
func (s *DAGEngineStore) LoadEdges(ctx context.Context, runID string) ([]DEEdgeRow, error) {
	const q = `SELECT run_id::text, from_node_id, to_node_id, output_to_input
	             FROM task_edges WHERE run_id = $1::uuid ORDER BY from_node_id, to_node_id`
	rows, err := s.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("load edges for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []DEEdgeRow
	for rows.Next() {
		var e DEEdgeRow
		var raw []byte
		if err := rows.Scan(&e.RunID, &e.From, &e.To, &raw); err != nil {
			return nil, fmt.Errorf("scan edge row: %w", err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &e.OutputToInput); err != nil {
				return nil, fmt.Errorf("parse output_to_input for edge %s->%s: %w", e.From, e.To, err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate edge rows: %w", err)
	}
	return out, nil
}

// LoadAttempts reads the CURRENT attempts of a run, newest attempt_number last
// per node.
//
// Current means above the node's retry_floor. Attempts an operator retry
// superseded are still in task_attempts -- history is immutable (invariant 9)
// and the run view shows them -- but they no longer describe the node: the
// engine derives node state from the latest attempt, and a superseded FAILED
// attempt with its budget spent would otherwise put a retried node straight
// back to FAILED (CONTRACT-FINDINGS F100).
func (s *DAGEngineStore) LoadAttempts(ctx context.Context, runID string) ([]DEAttemptRow, error) {
	// Expansion CHILDREN's output_ref is deliberately NOT loaded. A run can have
	// max_fanout children with up to the inline threshold each -- hundreds of MiB
	// read on every reconcile -- and nothing but fan-in assembly needs them, which
	// streams them with a size bound instead (StreamChildOutputs).
	const q = `SELECT a.id::text, a.run_id::text, a.node_id, a.attempt_number, a.state::text,
	                  CASE WHEN n.parent_node_id IS NULL THEN a.output_ref END, a.error
	             FROM task_attempts a
	             JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
	            WHERE a.run_id = $1::uuid
	              AND a.attempt_number > n.retry_floor
	            ORDER BY a.node_id, a.attempt_number`
	rows, err := s.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("load attempts for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []DEAttemptRow
	for rows.Next() {
		var a DEAttemptRow
		var state string
		if err := rows.Scan(&a.ID, &a.RunID, &a.NodeID, &a.AttemptNumber, &state,
			&a.OutputRef, &a.ErrorJSON); err != nil {
			return nil, fmt.Errorf("scan attempt row: %w", err)
		}
		a.State = domain.AttemptState(state)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attempt rows: %w", err)
	}
	return out, nil
}
