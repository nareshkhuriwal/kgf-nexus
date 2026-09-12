package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"google.golang.org/protobuf/encoding/protojson"
)

// ── OWNERSHIP NOTE: who may INSERT task_attempts ──────────────────────────────
//
// ADR-0011 assigns task_attempts to the SCHEDULER. It also assigns the "compute
// the ready frontier and create the initial PENDING attempts" responsibility to
// the DAG ENGINE (architecture §3.2, and §5.1 labels the PENDING state "created
// by DAG engine"). Read literally, those two statements conflict.
//
// They are reconciled by splitting the table along the axis that actually
// matters, which is not the table but the CONCERN:
//
//   - CREATION of a PENDING attempt row is a DEPENDENCY decision. It answers
//     "should this work exist at all?", which only the DAG engine can answer
//     because only it knows the graph and the trigger rules. That is this file.
//   - MUTATION of an existing attempt -- the state CAS to DISPATCHED, the lease,
//     the reservation, the worker assignment -- is a SCHEDULING decision, and it
//     must stay in the scheduler because it is the §5.3 atomic claim, the single
//     transaction the no-double-dispatch guarantee rests on.
//
// These never contend. The engine only ever INSERTs rows in PENDING and never
// UPDATEs any attempt; the scheduler only ever UPDATEs from PENDING onward and
// never INSERTs an initial attempt. No row is written by both services in the
// same state, so there is no lost update and no CAS to coordinate.
//
// Concretely, the DAG engine's grant on task_attempts must be INSERT ONLY (no
// UPDATE, no DELETE), which is expressible as a table-level Postgres grant and so
// satisfies ADR-0011's rule that ownership be enforced by the database rather
// than by code review. Retry attempts (attempt_number+1) are the scheduler's, not
// the engine's: those arise from retry policy, not from dependency logic.
//
// This is a genuine divergence from ADR-0011's table as written, so it is called
// out in the handover notes rather than resolved silently.

// InsertNodesAndEdges materializes a validated DAG for a run: one task_nodes row
// per task and one task_edges row per edge, in ONE transaction.
//
// One transaction because a partially materialized DAG is worse than none: the
// reconciler would compute a frontier over a graph missing edges and dispatch
// work whose dependencies it cannot see. Materialization is all-or-nothing.
//
// It is IDEMPOTENT (ON CONFLICT DO NOTHING). Materialization can be retried after
// a crash between the INSERT and the caller's acknowledgement, and at-least-once
// delivery means it WILL be. Nodes are never updated on conflict because a run
// pins its spec for its lifetime (ADR-0004): if a row already exists, it is by
// definition the right one.
func (s *DAGEngineStore) InsertNodesAndEdges(
	ctx context.Context, runID string, nodes []DENodeRow, edges []DEEdgeRow,
) (insertedNodes, insertedEdges int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin materialize tx for run %s: %w", runID, err)
	}
	defer func() {
		// Rollback on any early return. A committed tx makes this a no-op, so it
		// is safe unconditionally and cannot mask the real error.
		_ = tx.Rollback(ctx)
	}()

	const insNode = `
		INSERT INTO task_nodes
		  (run_id, node_id, task_ref, state, trigger_rule, max_attempts,
		   attempts_used, parent_node_id, map_index, spec_json)
		VALUES ($1::uuid, $2, $3, $4::node_state, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id, node_id) DO NOTHING`

	for _, n := range nodes {
		specJSON, merr := protojson.MarshalOptions{UseProtoNames: true}.Marshal(n.Spec)
		if merr != nil {
			return 0, 0, fmt.Errorf("marshal spec for node %s/%s: %w", runID, n.NodeID, merr)
		}
		state := n.State
		if state == "" {
			state = domain.NodePending
		}
		tag, ierr := tx.Exec(ctx, insNode, runID, n.NodeID, n.TaskRef, string(state),
			n.TriggerRule, n.MaxAttempts, n.AttemptsUsed, n.ParentNodeID, n.MapIndex, specJSON)
		if ierr != nil {
			return 0, 0, fmt.Errorf("insert node %s/%s: %w", runID, n.NodeID, ierr)
		}
		insertedNodes += tag.RowsAffected()
	}

	const insEdge = `
		INSERT INTO task_edges (run_id, from_node_id, to_node_id, output_to_input)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (run_id, from_node_id, to_node_id) DO NOTHING`

	for _, e := range edges {
		m := e.OutputToInput
		if m == nil {
			m = map[string]string{}
		}
		raw, merr := json.Marshal(m)
		if merr != nil {
			return 0, 0, fmt.Errorf("marshal output_to_input for edge %s->%s: %w", e.From, e.To, merr)
		}
		tag, ierr := tx.Exec(ctx, insEdge, runID, e.From, e.To, raw)
		if ierr != nil {
			return 0, 0, fmt.Errorf("insert edge %s %s->%s: %w", runID, e.From, e.To, ierr)
		}
		insertedEdges += tag.RowsAffected()
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit materialize tx for run %s: %w", runID, err)
	}
	return insertedNodes, insertedEdges, nil
}

// NewAttempt is the engine's request to create one initial PENDING attempt.
type NewAttempt struct {
	RunID    string
	NodeID   string
	Queue    string
	Priority int32
	// Resources comes from the task spec's ResourceRequest.request: that is the
	// figure the scheduler subtracts from worker capacity (§4.3).
	Resources *nexusv1.ResourceVector
	// IdempotencyKey MUST come from domain.IdempotencyKey and MUST exclude
	// attempt_number, so every retry of the same logical work shares one key
	// (CONTRACT.md invariant 1).
	IdempotencyKey string
	CacheKey       *string
	// InputRefs is the resolved upstream output mapping, as JSONB.
	InputRefs []byte
	MapIndex  *int32
}

// ApplyNodeStateAndCreateAttempts applies node-state transitions and creates the
// PENDING attempts for the new frontier, in ONE transaction.
//
// One transaction because the pair must be atomic in both directions. Marking a
// node READY without its attempt row leaves work that never runs and a run that
// never completes; creating an attempt without marking the node leaves the next
// reconcile free to create a SECOND attempt for the same node, which is a
// double-dispatch the scheduler's CAS cannot catch because the two attempts are
// distinct rows.
//
// Node updates use a CAS on the expected current state. Several reconcilers may
// race on the same run (there is no leader election, by design -- ADR-0011), and
// the CAS is what makes the loser's write a no-op instead of a regression from
// SUCCEEDED back to READY. A CAS miss is NOT an error: it means another replica
// got there first, which is the expected outcome under concurrency.
//
// THE CAS IS ALSO THE DISPATCH GATE, and it must be: a node's PENDING->READY CAS
// is the single serialization point that decides WHICH reconciler owns starting
// that node. A reconciler whose CAS missed lost that race, so it must not create
// the attempt either -- otherwise both replicas insert, and the node runs twice.
//
// That is not hypothetical. It was observed on a live chaos run: two attempts for
// one node, created 0.7ms apart, sharing an idempotency key but holding two
// distinct lease ids. The ON CONFLICT below could not catch it, because computing
// attempt_number as MAX+1 INSIDE the insert is not atomic under READ COMMITTED
// (the isolation level this pool uses): both statements read MAX=0, the second
// blocks on the unique index, and on release it RE-EVALUATES the subquery against
// the now-committed row, yielding 2. A conflict never fires; a second attempt
// number is minted instead. So the attempt number is computed ONCE, before the
// insert, and passed as a parameter -- which is what finally gives the unique
// constraint something to conflict ON, as a backstop behind the CAS gate.
//
// The gate is keyed on nodes whose transition TO READY succeeded in THIS
// transaction, because that is exactly the set this reconciler won the right to
// dispatch. Propagation transitions (SKIPPED / UPSTREAM_FAILED) never carry
// attempts, so they are irrelevant here.
func (s *DAGEngineStore) ApplyNodeStateAndCreateAttempts(
	ctx context.Context, runID string, transitions []NodeTransition, attempts []NewAttempt,
) (Applied, error) {
	var res Applied
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("begin reconcile tx for run %s: %w", runID, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const casNode = `
		UPDATE task_nodes SET state = $3::node_state
		 WHERE run_id = $1::uuid AND node_id = $2 AND state = $4::node_state`

	// The nodes this reconciler won the right to start: those whose PENDING->READY
	// CAS it actually applied. Only these may have an attempt created below.
	wonDispatch := make(map[string]bool, len(transitions))

	for _, t := range transitions {
		tag, uerr := tx.Exec(ctx, casNode, runID, t.NodeID, string(t.To), string(t.From))
		if uerr != nil {
			return res, fmt.Errorf("cas node %s/%s %s->%s: %w", runID, t.NodeID, t.From, t.To, uerr)
		}
		if tag.RowsAffected() == 0 {
			res.NodeCASMisses = append(res.NodeCASMisses, t.NodeID)
			continue
		}
		if t.To == domain.NodeReady {
			wonDispatch[t.NodeID] = true
		}
		res.NodesUpdated++
	}

	// attempts_used is incremented with the attempt row so the retry budget can
	// never drift from the number of attempts that actually exist.
	// Read in its own statement so the value is FIXED before the insert. Folding
	// this subquery into the INSERT re-evaluates it after a unique-violation block
	// releases, which mints a fresh number instead of conflicting -- see the
	// function comment. Separated, a racing pair both compute the same number and
	// exactly one survives the constraint.
	const nextAttemptNum = `
		SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM task_attempts
		 WHERE run_id = $1::uuid AND node_id = $2`

	const insAttempt = `
		INSERT INTO task_attempts
		  (run_id, node_id, attempt_number, state, queue, priority,
		   resource_request, idempotency_key, cache_key, input_refs)
		VALUES ($1::uuid, $2, $3,
		        'PENDING'::attempt_state, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (run_id, node_id, attempt_number) DO NOTHING
		RETURNING id::text, attempt_number`

	const bumpUsed = `
		UPDATE task_nodes SET attempts_used = attempts_used + 1
		 WHERE run_id = $1::uuid AND node_id = $2`

	for _, a := range attempts {
		if !wonDispatch[a.NodeID] {
			// Another reconciler won this node's READY CAS and is creating the
			// attempt in its own transaction. Recorded as a conflict, not an error:
			// under concurrency this is the expected outcome, and it is the same
			// signal the ON CONFLICT path reports.
			res.AttemptConflicts = append(res.AttemptConflicts, a.NodeID)
			continue
		}
		resJSON, eerr := encodeVector(a.Resources)
		if eerr != nil {
			return res, fmt.Errorf("encode resources for %s/%s: %w", runID, a.NodeID, eerr)
		}
		inputs := a.InputRefs
		if len(inputs) == 0 {
			inputs = []byte(`{}`)
		}
		queue := a.Queue
		if queue == "" {
			// MVP is single-queue, but the column is NOT NULL and the scheduler
			// matches on it, so an empty queue would make the attempt invisible to
			// every worker rather than merely mis-routed.
			queue = "default"
		}
		var next int32
		if nerr := tx.QueryRow(ctx, nextAttemptNum, runID, a.NodeID).Scan(&next); nerr != nil {
			return res, fmt.Errorf("next attempt number for %s/%s: %w", runID, a.NodeID, nerr)
		}

		var id string
		var num int32
		qerr := tx.QueryRow(ctx, insAttempt, runID, a.NodeID, next, queue, a.Priority,
			resJSON, a.IdempotencyKey, a.CacheKey, inputs).Scan(&id, &num)
		if errors.Is(qerr, pgx.ErrNoRows) {
			// The ON CONFLICT fired: this attempt number already exists, so a
			// concurrent reconciler created it. Not an error; the work exists.
			res.AttemptConflicts = append(res.AttemptConflicts, a.NodeID)
			continue
		}
		if qerr != nil {
			return res, fmt.Errorf("insert attempt for %s/%s: %w", runID, a.NodeID, qerr)
		}
		if _, uerr := tx.Exec(ctx, bumpUsed, runID, a.NodeID); uerr != nil {
			return res, fmt.Errorf("bump attempts_used for %s/%s: %w", runID, a.NodeID, uerr)
		}
		res.AttemptsCreated = append(res.AttemptsCreated, CreatedAttempt{
			AttemptID: id, NodeID: a.NodeID, AttemptNumber: num,
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("commit reconcile tx for run %s: %w", runID, err)
	}
	return res, nil
}

// NodeTransition is a CAS-guarded node state change.
type NodeTransition struct {
	NodeID string
	From   domain.NodeState
	To     domain.NodeState
}

// CreatedAttempt identifies a freshly inserted PENDING attempt.
type CreatedAttempt struct {
	AttemptID     string
	NodeID        string
	AttemptNumber int32
}

// Applied reports what a reconcile actually changed. The misses and conflicts are
// returned rather than swallowed so the caller can log them: under concurrency
// they are normal, but a sustained high rate means reconcilers are fighting and
// is worth an alert.
type Applied struct {
	NodesUpdated     int
	NodeCASMisses    []string
	AttemptsCreated  []CreatedAttempt
	AttemptConflicts []string
}

// CountNodes reports how many nodes a run has materialized, so the service can
// tell "not yet materialized" from "materialized and empty" without loading every
// row.
func (s *DAGEngineStore) CountNodes(ctx context.Context, runID string) (int, error) {
	const q = `SELECT count(*) FROM task_nodes WHERE run_id = $1::uuid`
	var n int
	if err := s.pool.QueryRow(ctx, q, runID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count nodes for run %s: %w", runID, err)
	}
	return n, nil
}
