// Package store holds the pgx query layer. Each service adds only its own
// file; these rm_*.go files belong to the RESOURCE MANAGER, which owns the
// `workers` and `reservations` tables (ADR-0011).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// WorkerState mirrors the frozen `worker_state` enum (§5.5).
//
// QUARANTINED exists in the schema but is NOT issued in Phase 1. It is the
// circuit breaker for the poison host — a worker with a broken driver or a bad
// disk that accepts tasks, fails them instantly, and is immediately handed more,
// chewing through the retry budget of every task in the queue in seconds. The
// constant is defined and the transition table permits it so that Phase 2 adds
// the detector, not a migration.
type WorkerState string

const (
	WorkerActive      WorkerState = "ACTIVE"
	WorkerDraining    WorkerState = "DRAINING"
	WorkerQuarantined WorkerState = "QUARANTINED"
	WorkerDead        WorkerState = "DEAD"
)

// Worker is a row of the capacity ledger.
type Worker struct {
	ID            string
	Namespace     string
	Hostname      string
	Queues        []string
	Executors     []string
	Labels        map[string]string
	Capacity      *nexusv1.ResourceVector
	Committed     *nexusv1.ResourceVector
	State         WorkerState
	AgentVersion  string
	RegisteredAt  time.Time
	LastHeartbeat time.Time
}

// ErrWorkerNotFound is returned when a worker_id does not exist, so callers can
// map it to gRPC NOT_FOUND rather than a generic internal error — a worker that
// was reaped should get a legible answer, not a 500.
var ErrWorkerNotFound = errors.New("store: worker not found")

// RMStore is the resource manager's data access.
type RMStore struct {
	pool *pgxpool.Pool
}

// NewRMStore wraps a pool. The pool, not this type, owns connection lifecycle.
func NewRMStore(pool *pgxpool.Pool) *RMStore { return &RMStore{pool: pool} }

// Pool exposes the underlying pool for health checks and for tests.
func (s *RMStore) Pool() *pgxpool.Pool { return s.pool }

// RegisterWorker inserts a new worker row and returns its per-session id.
//
// Every registration is a NEW ROW, even from a host that has registered before.
// Identity is per-SESSION, not per-host (§5.5): a restarted agent that reused
// its predecessor's id could present a stale lease token that still matched, and
// resurrect ownership of work that has already been re-dispatched. Keying worker
// identity on hostname is a genuinely nasty class of bug, and a fresh row per
// session is what makes it structurally impossible.
func (s *RMStore) RegisterWorker(ctx context.Context, w Worker, sessionTokenHash string) (string, error) {
	capJSON, err := marshalVec(w.Capacity)
	if err != nil {
		return "", err
	}
	labels, err := json.Marshal(orEmptyMap(w.Labels))
	if err != nil {
		return "", fmt.Errorf("store: marshal labels: %w", err)
	}
	// The columns are NOT NULL, and a nil Go slice marshals to SQL NULL rather
	// than an empty array. A worker that advertises no executors or no queues is
	// legitimate input (a misconfigured agent, a test), and it should register and
	// then simply match no work — not fail with a constraint violation that reads
	// like a platform bug.
	queues := w.Queues
	if queues == nil {
		queues = []string{}
	}
	executors := w.Executors
	if executors == nil {
		executors = []string{}
	}

	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO workers (namespace, hostname, queues, executors, labels,
		                     capacity, committed, state, agent_version, session_token_hash)
		VALUES ($1, $2, $3, $4, $5, $6, '{}'::jsonb, 'ACTIVE', $7, $8)
		RETURNING id`,
		w.Namespace, w.Hostname, queues, executors, labels, capJSON,
		w.AgentVersion, sessionTokenHash,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("store: register worker %s: %w", w.Hostname, err)
	}
	return id, nil
}

// GetWorker loads one worker row.
func (s *RMStore) GetWorker(ctx context.Context, workerID string) (*Worker, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, namespace, hostname, queues, executors, labels, capacity, committed,
		       state, agent_version, registered_at, last_heartbeat_at
		  FROM workers WHERE id = $1`, workerID)
	w, err := scanWorker(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrWorkerNotFound, workerID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get worker %s: %w", workerID, err)
	}
	return w, nil
}

// TouchHeartbeat records liveness. Separated from reconciliation so that a
// heartbeat whose reconciliation pass fails still counts as proof of life: the
// worker IS alive, and letting a bookkeeping error make it look dead would expire
// its leases and duplicate its work.
func (s *RMStore) TouchHeartbeat(ctx context.Context, workerID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE workers SET last_heartbeat_at = now()
		 WHERE id = $1 AND state IN ('ACTIVE','DRAINING')`, workerID)
	if err != nil {
		return fmt.Errorf("store: touch heartbeat %s: %w", workerID, err)
	}
	if tag.RowsAffected() == 0 {
		// Either the worker does not exist or it is already DEAD/QUARANTINED.
		// Both mean this session must re-register; the caller turns that into a
		// REREGISTER directive.
		return fmt.Errorf("%w: %s (or not in a live state)", ErrWorkerNotFound, workerID)
	}
	return nil
}

// SetWorkerState transitions a worker, enforcing the §5.5 transition table in
// SQL rather than in Go.
//
// In SQL because two resource-manager replicas can process the same event
// concurrently: a Go-side read-then-write would let both see ACTIVE and both
// write, and a DEAD worker could be resurrected to ACTIVE by a late heartbeat
// racing a liveness sweep. The predicate makes the illegal transition a
// zero-row update instead.
func (s *RMStore) SetWorkerState(ctx context.Context, workerID string, to WorkerState) (bool, error) {
	var allowedFrom []string
	switch to {
	case WorkerDraining:
		// Only a live worker can start draining. A DEAD one has nothing to drain.
		allowedFrom = []string{"ACTIVE", "QUARANTINED"}
	case WorkerDead:
		// DEAD is terminal and reachable from every non-terminal state.
		allowedFrom = []string{"ACTIVE", "DRAINING", "QUARANTINED"}
	case WorkerQuarantined:
		// Phase 2 hook: the transition is legal in the schema and here, but
		// nothing in Phase 1 issues it. See the WorkerState doc comment.
		allowedFrom = []string{"ACTIVE"}
	case WorkerActive:
		// Re-activation after quarantine (operator or auto-recovery). NEVER from
		// DEAD: a dead worker's session is gone, and the agent must re-register
		// with a new worker_id rather than have this row revived under it.
		allowedFrom = []string{"ACTIVE", "QUARANTINED"}
	default:
		return false, fmt.Errorf("store: unknown target worker state %q", to)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE workers SET state = $2::worker_state
		 WHERE id = $1 AND state::text = ANY($3)`, workerID, string(to), allowedFrom)
	if err != nil {
		return false, fmt.Errorf("store: set worker %s state to %s: %w", workerID, to, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListStaleWorkers returns live workers whose last heartbeat is older than the
// liveness window, for the DEAD sweep.
func (s *RMStore) ListStaleWorkers(ctx context.Context, window time.Duration) ([]*Worker, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, namespace, hostname, queues, executors, labels, capacity, committed,
		       state, agent_version, registered_at, last_heartbeat_at
		  FROM workers
		 WHERE state IN ('ACTIVE','DRAINING')
		   AND last_heartbeat_at < now() - $1::interval`,
		fmt.Sprintf("%d milliseconds", window.Milliseconds()))
	if err != nil {
		return nil, fmt.Errorf("store: list stale workers: %w", err)
	}
	defer rows.Close()

	var out []*Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan stale worker: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListWorkersForQueue returns live workers serving a queue, for the scheduler's
// placement scan (read-only; the scheduler never WRITES these rows except in the
// documented dispatch-transaction exception).
func (s *RMStore) ListWorkersForQueue(ctx context.Context, namespace, queue string) ([]*Worker, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, namespace, hostname, queues, executors, labels, capacity, committed,
		       state, agent_version, registered_at, last_heartbeat_at
		  FROM workers
		 WHERE namespace = $1 AND $2 = ANY(queues) AND state = 'ACTIVE'`,
		namespace, queue)
	if err != nil {
		return nil, fmt.Errorf("store: list workers for queue %s/%s: %w", namespace, queue, err)
	}
	defer rows.Close()

	var out []*Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan worker: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanWorker(r rowScanner) (*Worker, error) {
	var (
		w                 Worker
		labels, cap, comm []byte
	)
	if err := r.Scan(&w.ID, &w.Namespace, &w.Hostname, &w.Queues, &w.Executors,
		&labels, &cap, &comm, &w.State, &w.AgentVersion,
		&w.RegisteredAt, &w.LastHeartbeat); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(labels, &w.Labels); err != nil {
		return nil, fmt.Errorf("unmarshal labels: %w", err)
	}
	var err error
	if w.Capacity, err = unmarshalVec(cap); err != nil {
		return nil, err
	}
	if w.Committed, err = unmarshalVec(comm); err != nil {
		return nil, err
	}
	return &w, nil
}

// marshalVec renders a ResourceVector as the JSONB stored in the schema.
//
// protojson, not encoding/json: the vector has a map field and int64s, whose
// JSON shape only protojson renders in the canonical protobuf form. Mixing the
// two encoders across services would produce rows that one service writes and
// another cannot read.
//
// UseProtoNames is required, not cosmetic. Default protojson emits camelCase
// (cpuMillis), while the schema comments, the proto source, the docs and every
// hand-written operator query use snake_case (cpu_millis). A JSONB key that
// does not exist yields SQL NULL rather than an error, so the mismatch is
// silent: SUM((capacity ->> 'cpu_millis')::bigint) returned 0 for a fleet with
// real capacity, which reads as "no workers advertise anything" -- a scary and
// completely wrong conclusion. The database is an operator-facing surface and
// should speak the same names as its documentation.
func marshalVec(v *nexusv1.ResourceVector) ([]byte, error) {
	if v == nil {
		v = &nexusv1.ResourceVector{}
	}
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: marshal resource vector: %w", err)
	}
	return b, nil
}

func unmarshalVec(b []byte) (*nexusv1.ResourceVector, error) {
	v := &nexusv1.ResourceVector{}
	if len(b) == 0 || string(b) == "null" {
		return v, nil
	}
	// DiscardUnknown so a vector written by a newer service with an extra
	// dimension does not make this one fail to read the row at all. Ignoring an
	// unknown dimension under-counts; refusing to read makes the worker
	// invisible, which is strictly worse.
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(b, v); err != nil {
		return nil, fmt.Errorf("store: unmarshal resource vector %q: %w", string(b), err)
	}
	return v, nil
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
