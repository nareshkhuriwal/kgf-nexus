// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is the gateway's generic miss for read projections, mapped to 404.
var ErrNotFound = errors.New("not found")

// ErrInvalidRequest means an owning service rejected the request DOCUMENT rather
// than failing to process it: the HTTP and the JSON were fine, the content is
// not. Mapped to 422, the same status submitSpec already uses for a spec that
// parses but does not validate.
var ErrInvalidRequest = errors.New("invalid request")

// Every query in this file is READ-ONLY against tables the gateway does not own.
// ADR-0011 permits exactly this: the gateway reads all tables as projections and
// writes only api_keys and idempotency_keys. Anything mutating goes through the
// owning service's gRPC API.

// SpecRow is a stored, content-addressed spec version.
type SpecRow struct {
	ID        string
	Namespace string
	Name      string
	Version   int32
	SpecHash  string
	SpecJSON  []byte
	CreatedBy string
	CreatedAt time.Time
}

// GatewayGetSpecByHash finds an existing version by content address. This is what
// makes a byte-identical resubmit a NO-OP returning the existing version rather
// than a redundant new one (ADR-0004).
func GatewayGetSpecByHash(ctx context.Context, q GWQuerier, namespace, name, hash string) (SpecRow, error) {
	var s SpecRow
	err := q.QueryRow(ctx, `
		SELECT id::text, namespace, name, version, spec_hash, spec_json::text, created_by, created_at
		  FROM workflow_specs
		 WHERE namespace = $1 AND name = $2 AND spec_hash = $3`,
		namespace, name, hash,
	).Scan(&s.ID, &s.Namespace, &s.Name, &s.Version, &s.SpecHash, &s.SpecJSON, &s.CreatedBy, &s.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SpecRow{}, ErrNotFound
	}
	if err != nil {
		return SpecRow{}, fmt.Errorf("get spec by hash: %w", err)
	}
	return s, nil
}

// CatalogEntry is one WORKFLOW in the catalog: its newest version, plus the
// facts that distinguish it from the others at a glance.
type CatalogEntry struct {
	SpecRow
	// VersionCount is how many versions of this workflow exist. A workflow on
	// version 7 has a different character from one submitted once, and the
	// catalog's job is to make that visible without a second request per row.
	VersionCount int32
	// FirstCreatedAt is when the workflow first appeared, as opposed to
	// SpecRow.CreatedAt, which is when the LATEST version was submitted. Both
	// matter: one is age, the other is freshness.
	FirstCreatedAt time.Time
}

// GatewayListCatalog returns one row per workflow NAME, carrying its newest
// version.
//
// One row per name rather than per version, because the catalog answers "what
// workflows exist here", and a list that repeats a workflow once per version
// buries fifteen workflows under a hundred rows. Version history is a
// drill-down, served by GatewayListSpecVersions.
//
// DISTINCT ON is the right tool and is Postgres-specific: it keeps the first row
// of each name group under the ORDER BY, which with (name, version DESC) is
// exactly "the newest version of each workflow" in one pass. The alternative --
// a window function or a correlated subquery per name -- reads worse and plans
// no better.
//
// spec_json is deliberately NOT selected. It is the largest column in the table
// and the catalog list never renders a definition; fetching it for every row
// would make the list cost scale with total workflow SIZE rather than count.
// GatewayGetSpec serves the definition when something actually needs it.
func GatewayListCatalog(ctx context.Context, q GWQuerier, namespace, search string, limit int) ([]CatalogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	// The search is a case-insensitive substring on the workflow name. Matching
	// on the name alone, not the definition: an operator looking for "retrain"
	// means the workflow called retrain, and a body match would return every
	// workflow that happens to mention it in a task ref.
	rows, err := q.Query(ctx, `
		SELECT DISTINCT ON (s.name)
		       s.id::text, s.namespace, s.name, s.version, s.spec_hash,
		       s.created_by, s.created_at,
		       (SELECT count(*)::int FROM workflow_specs v
		         WHERE v.namespace = s.namespace AND v.name = s.name),
		       (SELECT min(v.created_at) FROM workflow_specs v
		         WHERE v.namespace = s.namespace AND v.name = s.name)
		  FROM workflow_specs s
		 WHERE s.namespace = $1
		   AND ($2 = '' OR s.name ILIKE '%' || $2 || '%')
		 ORDER BY s.name, s.version DESC
		 LIMIT $3`,
		namespace, search, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list catalog: %w", err)
	}
	defer rows.Close()

	var out []CatalogEntry
	for rows.Next() {
		var c CatalogEntry
		if err := rows.Scan(&c.ID, &c.Namespace, &c.Name, &c.Version, &c.SpecHash,
			&c.CreatedBy, &c.CreatedAt, &c.VersionCount, &c.FirstCreatedAt); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GatewayListSpecVersions returns versions newest-first.
func GatewayListSpecVersions(ctx context.Context, q GWQuerier, namespace, name string, limit int) ([]SpecRow, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, namespace, name, version, spec_hash, created_by, created_at
		  FROM workflow_specs
		 WHERE namespace = $1 AND name = $2
		 ORDER BY version DESC
		 LIMIT $3`,
		namespace, name, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list spec versions: %w", err)
	}
	defer rows.Close()

	var out []SpecRow
	for rows.Next() {
		var s SpecRow
		if err := rows.Scan(&s.ID, &s.Namespace, &s.Name, &s.Version, &s.SpecHash, &s.CreatedBy, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan spec version: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RunRow is a run as the REST API reports it.
type RunRow struct {
	ID           string
	SpecID       string
	SpecName     string
	SpecVersion  int32
	Namespace    string
	State        string
	Priority     int32
	TriggerKind  string
	Params       []byte
	DeadlineAt   *time.Time
	CreatedAt    time.Time
	StartedAt    *time.Time
	EndedAt      *time.Time
	StateVersion int64
}

const runSelect = `
	SELECT r.id::text, r.spec_id::text, s.name, s.version, r.namespace, r.state::text,
	       r.priority, r.trigger_kind, r.params::text, r.deadline_at,
	       r.created_at, r.started_at, r.ended_at, r.state_version
	  FROM workflow_runs r
	  JOIN workflow_specs s ON s.id = r.spec_id`

func scanRun(row pgx.Row) (RunRow, error) {
	var r RunRow
	err := row.Scan(&r.ID, &r.SpecID, &r.SpecName, &r.SpecVersion, &r.Namespace, &r.State,
		&r.Priority, &r.TriggerKind, &r.Params, &r.DeadlineAt,
		&r.CreatedAt, &r.StartedAt, &r.EndedAt, &r.StateVersion)
	return r, err
}

// GatewayGetRun loads one run. Namespace is part of the predicate rather than
// checked after the fact, so a caller authenticated for namespace A cannot read a
// run in namespace B by guessing its UUID.
func GatewayGetRun(ctx context.Context, q GWQuerier, namespace, runID string) (RunRow, error) {
	r, err := scanRun(q.QueryRow(ctx, runSelect+` WHERE r.namespace = $1 AND r.id = $2`, namespace, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunRow{}, ErrNotFound
	}
	if err != nil {
		return RunRow{}, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// RunFilter narrows a run listing.
type RunFilter struct {
	States   []string
	SpecName string
	Limit    int
	Before   *time.Time
}

// GatewayListRuns lists runs newest-first, keyset-paginated on created_at.
//
// Keyset rather than OFFSET: the listing is over an append-heavy table, and
// OFFSET both degrades linearly with depth and silently skips or repeats rows
// when new runs arrive mid-pagination.
func GatewayListRuns(ctx context.Context, q GWQuerier, namespace string, f RunFilter) ([]RunRow, error) {
	var (
		conds = []string{"r.namespace = $1"}
		args  = []any{namespace}
	)
	if len(f.States) > 0 {
		args = append(args, f.States)
		conds = append(conds, fmt.Sprintf("r.state::text = ANY($%d)", len(args)))
	}
	if f.SpecName != "" {
		args = append(args, f.SpecName)
		conds = append(conds, fmt.Sprintf("s.name = $%d", len(args)))
	}
	if f.Before != nil {
		args = append(args, *f.Before)
		conds = append(conds, fmt.Sprintf("r.created_at < $%d", len(args)))
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)

	sql := runSelect + " WHERE " + strings.Join(conds, " AND ") +
		fmt.Sprintf(" ORDER BY r.created_at DESC LIMIT $%d", len(args))

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []RunRow
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NodeRow is a node's current state within a run.
type NodeRow struct {
	NodeID       string
	TaskRef      string
	State        string
	TriggerRule  string
	MaxAttempts  int32
	AttemptsUsed int32
	MapIndex     *int32
}

// GatewayListRunNodes returns node states for a run detail view.
func GatewayListRunNodes(ctx context.Context, q GWQuerier, runID string) ([]NodeRow, error) {
	rows, err := q.Query(ctx, `
		SELECT node_id, task_ref, state::text, trigger_rule, max_attempts, attempts_used, map_index
		  FROM task_nodes
		 WHERE run_id = $1
		 ORDER BY node_id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list run nodes: %w", err)
	}
	defer rows.Close()

	var out []NodeRow
	for rows.Next() {
		var n NodeRow
		if err := rows.Scan(&n.NodeID, &n.TaskRef, &n.State, &n.TriggerRule, &n.MaxAttempts, &n.AttemptsUsed, &n.MapIndex); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// EdgeRow is a dependency edge as the REST API reports it.
type EdgeRow struct {
	From          string
	To            string
	OutputToInput []byte
}

// GatewayListRunEdges returns the dependency edges of a run.
//
// The detail view cannot draw a graph from nodes alone: with no edges every node
// has indegree zero, so the layered layout assigns them all to depth 0 and the
// DAG renders as a single flat column of apparent roots. That failure mode is
// silent -- it looks like a wide graph rather than a missing field -- which is
// why this read exists next to the node read rather than being left to a second
// round trip.
//
// Ordered so the layout is stable across polls; a graph that reshuffles on every
// refresh is unusable during an incident.
func GatewayListRunEdges(ctx context.Context, q GWQuerier, runID string) ([]EdgeRow, error) {
	rows, err := q.Query(ctx, `
		SELECT from_node_id, to_node_id, output_to_input::text
		  FROM task_edges
		 WHERE run_id = $1
		 ORDER BY from_node_id, to_node_id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list run edges: %w", err)
	}
	defer rows.Close()

	var out []EdgeRow
	for rows.Next() {
		var e EdgeRow
		if err := rows.Scan(&e.From, &e.To, &e.OutputToInput); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AttemptRow is an attempt as the REST API reports it.
type AttemptRow struct {
	ID            string
	RunID         string
	NodeID        string
	AttemptNumber int32
	State         string
	Queue         string
	WorkerID      *string
	PendingReason *string
	Error         []byte
	Metrics       []byte
	ScheduledAt   time.Time
	StartedAt     *time.Time
	EndedAt       *time.Time
}

// GatewayListRunAttempts returns every attempt of a run, including terminal ones.
// History is never mutated (invariant 9), so "show me every attempt" is a plain
// ordered read.
func GatewayListRunAttempts(ctx context.Context, q GWQuerier, runID string) ([]AttemptRow, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, run_id::text, node_id, attempt_number, state::text, queue,
		       worker_id::text, pending_reason, error::text, metrics::text,
		       scheduled_at, started_at, ended_at
		  FROM task_attempts
		 WHERE run_id = $1
		 ORDER BY node_id, attempt_number`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list run attempts: %w", err)
	}
	defer rows.Close()

	var out []AttemptRow
	for rows.Next() {
		var a AttemptRow
		if err := rows.Scan(&a.ID, &a.RunID, &a.NodeID, &a.AttemptNumber, &a.State, &a.Queue,
			&a.WorkerID, &a.PendingReason, &a.Error, &a.Metrics,
			&a.ScheduledAt, &a.StartedAt, &a.EndedAt); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GatewayGetAttempt loads one attempt, scoped by namespace via its run.
func GatewayGetAttempt(ctx context.Context, q GWQuerier, namespace, attemptID string) (AttemptRow, error) {
	var a AttemptRow
	err := q.QueryRow(ctx, `
		SELECT a.id::text, a.run_id::text, a.node_id, a.attempt_number, a.state::text, a.queue,
		       a.worker_id::text, a.pending_reason, a.error::text, a.metrics::text,
		       a.scheduled_at, a.started_at, a.ended_at
		  FROM task_attempts a
		  JOIN workflow_runs r ON r.id = a.run_id
		 WHERE a.id = $1 AND r.namespace = $2`,
		attemptID, namespace,
	).Scan(&a.ID, &a.RunID, &a.NodeID, &a.AttemptNumber, &a.State, &a.Queue,
		&a.WorkerID, &a.PendingReason, &a.Error, &a.Metrics,
		&a.ScheduledAt, &a.StartedAt, &a.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AttemptRow{}, ErrNotFound
	}
	if err != nil {
		return AttemptRow{}, fmt.Errorf("get attempt: %w", err)
	}
	return a, nil
}

// EventRow is one entry of the append-only history.
type EventRow struct {
	ID         int64
	RunID      string
	Seq        int32
	NodeID     *string
	AttemptID  *string
	Kind       string
	Payload    []byte
	Actor      string
	OccurredAt time.Time
}

// GatewayListRunEvents reads history after a sequence cursor.
//
// afterSeq (not a timestamp) is the cursor because run_events carries a
// UNIQUE(run_id, seq) and is append-only: sequence gives a total order with no
// ties and no clock dependency, which is exactly what an SSE live tail needs to
// resume without gaps or duplicates across a reconnect.
func GatewayListRunEvents(ctx context.Context, q GWQuerier, runID string, afterSeq int32, limit int) ([]EventRow, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := q.Query(ctx, `
		SELECT id, run_id::text, seq, node_id, attempt_id::text, kind, payload::text, actor, occurred_at
		  FROM run_events
		 WHERE run_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`,
		runID, afterSeq, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list run events: %w", err)
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.ID, &e.RunID, &e.Seq, &e.NodeID, &e.AttemptID, &e.Kind, &e.Payload, &e.Actor, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// WorkerRow is a worker as the fleet view reports it.
type WorkerRow struct {
	ID              string
	Namespace       string
	Hostname        string
	Queues          []string
	Executors       []string
	Labels          []byte
	Capacity        []byte
	Committed       []byte
	State           string
	AgentVersion    string
	RegisteredAt    time.Time
	LastHeartbeatAt time.Time
	ActiveAttempts  int32
}

// GatewayListWorkers is the fleet view. active_attempts comes from a correlated
// count rather than a trusted counter column, because a denormalized counter is
// exactly the kind of thing that drifts and then misleads an operator during an
// incident.
func GatewayListWorkers(ctx context.Context, q GWQuerier, namespace string) ([]WorkerRow, error) {
	rows, err := q.Query(ctx, `
		SELECT w.id::text, w.namespace, w.hostname, w.queues, w.executors,
		       w.labels::text, w.capacity::text, w.committed::text, w.state::text,
		       w.agent_version, w.registered_at, w.last_heartbeat_at,
		       (SELECT count(*) FROM task_attempts a
		         WHERE a.worker_id = w.id AND a.state IN ('DISPATCHED','RUNNING'))::int
		  FROM workers w
		 WHERE w.namespace = $1
		 ORDER BY w.hostname, w.registered_at DESC`,
		namespace,
	)
	if err != nil {
		return nil, fmt.Errorf("list workers: %w", err)
	}
	defer rows.Close()

	var out []WorkerRow
	for rows.Next() {
		var w WorkerRow
		if err := rows.Scan(&w.ID, &w.Namespace, &w.Hostname, &w.Queues, &w.Executors,
			&w.Labels, &w.Capacity, &w.Committed, &w.State,
			&w.AgentVersion, &w.RegisteredAt, &w.LastHeartbeatAt, &w.ActiveAttempts); err != nil {
			return nil, fmt.Errorf("scan worker: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GatewayGetWorker loads one worker, namespace-scoped.
//
// active_attempts is counted here with the SAME correlated subquery the fleet
// listing uses, rather than the constant 0 this projection started with. The
// count was harmless while the only caller was drainWorker's existence check,
// which discards the row — but the worker detail view renders it, and a detail
// page reporting 0 running tasks next to a fleet table reporting 3 would send an
// operator looking for a bug in the scheduler instead of in this line.
func GatewayGetWorker(ctx context.Context, q GWQuerier, namespace, workerID string) (WorkerRow, error) {
	var w WorkerRow
	err := q.QueryRow(ctx, `
		SELECT w.id::text, w.namespace, w.hostname, w.queues, w.executors, w.labels::text,
		       w.capacity::text, w.committed::text, w.state::text, w.agent_version,
		       w.registered_at, w.last_heartbeat_at,
		       (SELECT count(*) FROM task_attempts a
		         WHERE a.worker_id = w.id AND a.state IN ('DISPATCHED','RUNNING'))::int
		  FROM workers w
		 WHERE w.namespace = $1 AND w.id = $2`,
		namespace, workerID,
	).Scan(&w.ID, &w.Namespace, &w.Hostname, &w.Queues, &w.Executors, &w.Labels,
		&w.Capacity, &w.Committed, &w.State, &w.AgentVersion,
		&w.RegisteredAt, &w.LastHeartbeatAt, &w.ActiveAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRow{}, ErrNotFound
	}
	if err != nil {
		return WorkerRow{}, fmt.Errorf("get worker: %w", err)
	}
	return w, nil
}

// WorkerAttemptRow is one task attributed to a worker, as the worker detail view
// reports it.
//
// It is not AttemptRow. AttemptRow is shaped for a RUN detail view, where the run
// is the page and so `run_id` is ambient and `task_ref` already sits on the node
// row beside it. Read from a worker, both of those facts are the answer rather
// than the context: "this worker is running ml.train, in run 673321b5" is the
// sentence an operator needs, and neither half of it is available from the
// attempt row alone. Hence the join to task_nodes for task_ref, and hence a
// distinct struct rather than widening AttemptRow with fields the run view would
// carry unused.
type WorkerAttemptRow struct {
	ID            string
	RunID         string
	NodeID        string
	TaskRef       string
	AttemptNumber int32
	State         string
	Queue         string
	ScheduledAt   time.Time
	StartedAt     *time.Time
	EndedAt       *time.Time
}

// workerAttemptSelect is shared by the active and the recent read below so the
// two can never project different columns for the same struct — the failure mode
// being a scan that compiles, runs, and silently fills TaskRef from Queue.
const workerAttemptSelect = `
	SELECT a.id::text, a.run_id::text, a.node_id, n.task_ref, a.attempt_number,
	       a.state::text, a.queue, a.scheduled_at, a.started_at, a.ended_at
	  FROM task_attempts a
	  JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id`

func scanWorkerAttempts(rows pgx.Rows, op string) ([]WorkerAttemptRow, error) {
	defer rows.Close()
	out := make([]WorkerAttemptRow, 0)
	for rows.Next() {
		var a WorkerAttemptRow
		if err := rows.Scan(&a.ID, &a.RunID, &a.NodeID, &a.TaskRef, &a.AttemptNumber,
			&a.State, &a.Queue, &a.ScheduledAt, &a.StartedAt, &a.EndedAt); err != nil {
			return nil, fmt.Errorf("scan %s: %w", op, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GatewayWorkerRunningAttempts lists what a worker is executing RIGHT NOW.
//
// DISPATCHED and RUNNING, matching GatewayListWorkers' active_attempts count
// exactly, so the number in the fleet table and the length of this list can
// never disagree and send an operator hunting for a task that is not there.
// DISPATCHED is included deliberately: the attempt is the worker's
// responsibility from the moment it is handed over, and an attempt stuck in
// DISPATCHED — assigned but never started — is precisely the symptom an operator
// opens this view to find.
//
// The predicate is served by idx_attempts_worker_active, the partial index on
// worker_id WHERE state IN ('DISPATCHED','RUNNING') that the schema already
// carries for the reconciliation path. No LIMIT: this set is bounded by the
// worker's own capacity, so it is inherently small.
//
// Namespace is enforced through the worker rather than the run: the caller has
// already resolved the worker inside the namespace, and a worker's attempts
// cannot belong to another tenant — attempts are placed on workers registered in
// the run's own namespace. Scoping again through workflow_runs would add a join
// that can only ever be true.
func GatewayWorkerRunningAttempts(ctx context.Context, q GWQuerier, workerID string) ([]WorkerAttemptRow, error) {
	rows, err := q.Query(ctx, workerAttemptSelect+`
	 WHERE a.worker_id = $1
	   AND a.state IN ('DISPATCHED','RUNNING')
	 ORDER BY a.started_at DESC NULLS FIRST, a.scheduled_at DESC`,
		workerID,
	)
	if err != nil {
		return nil, fmt.Errorf("list worker running attempts: %w", err)
	}
	return scanWorkerAttempts(rows, "worker running attempt")
}

// GatewayWorkerRecentAttempts lists what a worker has finished, most recent first.
//
// This is what makes the view answer anything at all about a DEAD worker, whose
// running set is necessarily empty: "what was this worker doing before it went"
// is the question asked of a worker that just died, and an empty page would
// answer it with silence.
//
// Bounded by a LIMIT, and the limit is not negotiable upward without an index:
// there is no index on worker_id for terminal states (only the active partial
// one), so this is an ordered scan of the worker's history. A busy worker in the
// live fleet has hundreds of attempts (observed: 432 on one agent), which is
// still trivial to sort — but an unbounded read here would grow without ceiling
// over a worker's lifetime, and a detail page is not a history export.
func GatewayWorkerRecentAttempts(ctx context.Context, q GWQuerier, workerID string, limit int) ([]WorkerAttemptRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, workerAttemptSelect+`
	 WHERE a.worker_id = $1
	   AND a.state NOT IN ('DISPATCHED','RUNNING')
	 ORDER BY coalesce(a.ended_at, a.started_at, a.scheduled_at) DESC
	 LIMIT $2`,
		workerID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list worker recent attempts: %w", err)
	}
	return scanWorkerAttempts(rows, "worker recent attempt")
}
