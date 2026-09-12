package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// vectorJSON is the on-disk shape of a ResourceVector in the JSONB columns
// (task_attempts.resource_request, workers.capacity/committed,
// reservations.resources).
//
// It is snake_case and hand-written rather than protojson because the column is
// read and written by several services and by operators running ad-hoc SQL;
// protojson's lowerCamelCase and its omission of zero values would make
// `SELECT capacity->>'gpu_count'` silently return NULL for a worker with no
// GPUs, which is a trap in exactly the queries people write at 3am.
type vectorJSON struct {
	CPUMillis      int64            `json:"cpu_millis"`
	MemoryBytes    int64            `json:"memory_bytes"`
	GPUCount       int32            `json:"gpu_count"`
	GPUKind        string           `json:"gpu_kind"`
	GPUMemoryBytes int64            `json:"gpu_memory_bytes"`
	DiskBytes      int64            `json:"disk_bytes"`
	Custom         map[string]int64 `json:"custom"`
}

// encodeVector serializes a ResourceVector for a JSONB column. A nil vector
// encodes as an all-zero object rather than SQL NULL, because the schema
// declares these columns NOT NULL and a zero request is meaningful ("costs
// nothing to place").
func encodeVector(v *nexusv1.ResourceVector) ([]byte, error) {
	out := vectorJSON{
		CPUMillis:      v.GetCpuMillis(),
		MemoryBytes:    v.GetMemoryBytes(),
		GPUCount:       v.GetGpuCount(),
		GPUKind:        v.GetGpuKind(),
		GPUMemoryBytes: v.GetGpuMemoryBytes(),
		DiskBytes:      v.GetDiskBytes(),
		Custom:         v.GetCustom(),
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode resource vector: %w", err)
	}
	return b, nil
}

// decodeVector parses a ResourceVector from a JSONB column. An empty or null
// column yields the zero vector: the schema defaults workers.committed to '{}',
// which legitimately means "nothing committed".
func decodeVector(b []byte) (*nexusv1.ResourceVector, error) {
	if len(b) == 0 || string(b) == "null" {
		return &nexusv1.ResourceVector{}, nil
	}
	var in vectorJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("decode resource vector %q: %w", string(b), err)
	}
	return &nexusv1.ResourceVector{
		CpuMillis:      in.CPUMillis,
		MemoryBytes:    in.MemoryBytes,
		GpuCount:       in.GPUCount,
		GpuKind:        in.GPUKind,
		GpuMemoryBytes: in.GPUMemoryBytes,
		DiskBytes:      in.DiskBytes,
		Custom:         in.Custom,
	}, nil
}

// appendEvent appends to the run_events log.
//
// run_events is append-only (invariant 8): this is the only write path, and
// there is deliberately no update or delete helper.
//
// seq must be a gapless-enough total order per run for the log to be replayable,
// and UNIQUE(run_id, seq) enforces uniqueness. Allocating it as max(seq)+1 races
// whenever two transactions append to the SAME run concurrently — which the
// scheduler does routinely, because a run's nodes are dispatched in parallel by
// different replicas. So the allocation takes a per-run advisory transaction
// lock first.
//
// An advisory lock rather than SELECT ... FOR UPDATE on the run row, because the
// scheduler does NOT own workflow_runs (ADR-0011) and locking another service's
// row inside the dispatch transaction would let event appends block the workflow
// manager. The lock is scoped to (run_id) so appends to different runs never
// contend, and it is released automatically at commit or rollback, so a crashed
// replica cannot wedge a run's event log.
//
// NOTE ON OWNERSHIP: ADR-0011 assigns run_events to the event-service. The
// scheduler must nevertheless emit AttemptDispatched inside the claim
// transaction, because invariant 4 names the run_event as one of the five facts
// that commit together. See the report: the contract is internally inconsistent
// here.
func appendEvent(
	ctx context.Context, tx pgx.Tx,
	runID uuid.UUID, nodeID string, attemptID *uuid.UUID,
	kind, actor string, payload map[string]any,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event payload for %s: %w", kind, err)
	}
	// hashtextextended gives a stable 64-bit key from the run id. A hash
	// collision between two runs costs a little needless serialization of their
	// event appends and nothing else, which is a fine price for never taking a
	// lock on a table another service owns.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, runID,
	); err != nil {
		return fmt.Errorf("lock run event sequence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO run_events (run_id, seq, node_id, attempt_id, kind, payload, actor)
VALUES ($1,
        COALESCE((SELECT max(seq)+1 FROM run_events WHERE run_id = $1), 1),
        $2, $3, $4, $5, $6)`,
		runID, nullString(nodeID), attemptID, kind, body, actor,
	); err != nil {
		return fmt.Errorf("append run_event %s: %w", kind, err)
	}
	return nil
}

// insertTimer writes a durable deadline. Every deadline in the system is a row
// here so a control-plane restart loses none, which is the failure mode of
// in-memory timer wheels (ADR-0002).
func insertTimer(
	ctx context.Context, tx pgx.Tx, kind string, subjectID uuid.UUID,
	fireAt time.Time, payload map[string]any,
) error {
	body := []byte("{}")
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal timer payload for %s: %w", kind, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO timers (kind, subject_id, fire_at, payload) VALUES ($1,$2,$3,$4)`,
		kind, subjectID, fireAt, body,
	); err != nil {
		return fmt.Errorf("insert %s timer: %w", kind, err)
	}
	return nil
}

// deleteTimersFor removes the deadline timers of an attempt that has reached a
// terminal state, so a start_to_close timer cannot fire against an attempt that
// is already LOST and generate a spurious second transition.
func deleteTimersFor(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, kinds ...string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM timers WHERE subject_id = $1 AND kind = ANY($2)`,
		subjectID, kinds,
	); err != nil {
		return fmt.Errorf("delete timers for %s: %w", subjectID, err)
	}
	return nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
