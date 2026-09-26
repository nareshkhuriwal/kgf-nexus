// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

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
	// Fractional GPUs (ADR-0029). omitempty, unlike the fields above: a vector
	// with no fractional GPU encodes to exactly the bytes it always did, so the
	// pull claim's `resource_request ? 'mig'` guards stay false for every
	// attempt that does not use one, and old readers see nothing new.
	Mig                 map[string]int64     `json:"mig,omitempty"`
	GPUShareMilli       int32                `json:"gpu_share_milli,omitempty"`
	GPUShareMemoryBytes int64                `json:"gpu_share_memory_bytes,omitempty"`
	GPUShares           map[string]shareJSON `json:"gpu_shares,omitempty"`
}

// shareJSON is one shared GPU device's entry.
type shareJSON struct {
	Milli       int64 `json:"milli"`
	MemoryBytes int64 `json:"memory_bytes"`
}

// shareJSONLoose reads a device entry in either number spelling, and in
// protojson's camelCase for the memory field.
type shareJSONLoose struct {
	Milli            json.Number `json:"milli"`
	MemoryBytes      json.Number `json:"memory_bytes"`
	MemoryBytesCamel json.Number `json:"memoryBytes"`
}

// vectorJSONLoose is the READ shape of the same columns.
//
// It exists because protojson renders every int64 as a JSON STRING
// ("cpu_millis": "8000") while this package writes them as JSON numbers, and
// both encoders write these columns. Unmarshalling a string into an int64 field
// is a hard error, so a strict read of a resource-manager-written
// workers.capacity row fails the whole claim transaction — the observed symptom
// was every PollTask returning Internal and a healthy fleet doing no work at
// all. json.Number accepts both spellings, so one reader covers both writers.
type vectorJSONLoose struct {
	CPUMillis      json.Number            `json:"cpu_millis"`
	MemoryBytes    json.Number            `json:"memory_bytes"`
	GPUCount       json.Number            `json:"gpu_count"`
	GPUKind        string                 `json:"gpu_kind"`
	GPUMemoryBytes json.Number            `json:"gpu_memory_bytes"`
	DiskBytes      json.Number            `json:"disk_bytes"`
	Custom         map[string]json.Number `json:"custom"`

	Mig                 map[string]json.Number    `json:"mig"`
	GPUShareMilli       json.Number               `json:"gpu_share_milli"`
	GPUShareMemoryBytes json.Number               `json:"gpu_share_memory_bytes"`
	GPUShares           map[string]shareJSONLoose `json:"gpu_shares"`
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

		GPUShareMilli:       v.GetGpuShareMilli(),
		GPUShareMemoryBytes: v.GetGpuShareMemoryBytes(),
	}
	if len(v.GetMig()) > 0 {
		out.Mig = v.GetMig()
	}
	if len(v.GetGpuShares()) > 0 {
		out.GPUShares = make(map[string]shareJSON, len(v.GetGpuShares()))
		for k, sh := range v.GetGpuShares() {
			out.GPUShares[k] = shareJSON{Milli: sh.GetMilli(), MemoryBytes: sh.GetMemoryBytes()}
		}
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
	var in vectorJSONLoose
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("decode resource vector %q: %w", string(b), err)
	}
	out := &nexusv1.ResourceVector{
		CpuMillis:      looseInt64(in.CPUMillis),
		MemoryBytes:    looseInt64(in.MemoryBytes),
		GpuCount:       int32(looseInt64(in.GPUCount)),
		GpuKind:        in.GPUKind,
		GpuMemoryBytes: looseInt64(in.GPUMemoryBytes),
		DiskBytes:      looseInt64(in.DiskBytes),
	}
	if len(in.Custom) > 0 {
		out.Custom = make(map[string]int64, len(in.Custom))
		for k, v := range in.Custom {
			out.Custom[k] = looseInt64(v)
		}
	}
	decodeFractionalGPU(&in, out)

	// The SAME columns are written in two dialects by two services, so the reader
	// must accept both or silently read zeros.
	//
	// task_attempts.resource_request is written by this package in the snake_case
	// shape vectorJSON declares, with int64 as JSON numbers. workers.capacity and
	// workers.committed are written by the resource-manager with protojson
	// (internal/store/rm_workers.go marshalVec), which is snake_case under
	// UseProtoNames but renders every int64 as a JSON STRING, and which is
	// camelCase without it.
	//
	// Both number spellings are handled above by reading through json.Number;
	// this overlay covers the camelCase spelling, which a protojson writer that
	// forgets UseProtoNames would produce. Decoding such bytes with the
	// snake_case shape alone finds none of its fields and reports a
	// zero-capacity worker WITHOUT any error, because json.Unmarshal does not
	// complain about fields it cannot find. The visible symptom is every attempt
	// stuck PENDING with "no ACTIVE worker matches queue/labels/executor" while a
	// fleet of idle, healthy workers sits there — a capacity ledger that reads as
	// empty is indistinguishable from a cluster with no workers.
	//
	// Fixing the writer instead would be a schema migration of live rows, and
	// would still leave every row written before the fix unreadable. A tolerant
	// reader heals both. This dialect split is reported rather than papered over:
	// one encoder should own these columns.
	applyProtoJSONVector(b, out)
	return out, nil
}

// decodeFractionalGPU copies the ADR-0029 fields out of the loose shape.
func decodeFractionalGPU(in *vectorJSONLoose, out *nexusv1.ResourceVector) {
	if len(in.Mig) > 0 {
		out.Mig = make(map[string]int64, len(in.Mig))
		for k, v := range in.Mig {
			out.Mig[k] = looseInt64(v)
		}
	}
	out.GpuShareMilli = int32(looseInt64(in.GPUShareMilli))
	out.GpuShareMemoryBytes = looseInt64(in.GPUShareMemoryBytes)
	if len(in.GPUShares) > 0 {
		out.GpuShares = make(map[string]*nexusv1.GPUShare, len(in.GPUShares))
		for k, v := range in.GPUShares {
			mem := looseInt64(v.MemoryBytes)
			if mem == 0 {
				mem = looseInt64(v.MemoryBytesCamel)
			}
			out.GpuShares[k] = &nexusv1.GPUShare{Milli: looseInt64(v.Milli), MemoryBytes: mem}
		}
	}
}

// looseInt64 reads an int64 that may have been written as a JSON number or, by
// protojson, as a JSON string. An unparseable value yields 0 rather than an
// error: the alternative is failing a whole claim transaction over one
// malformed dimension, and a zero dimension is already the meaning of an absent
// one.
func looseInt64(n json.Number) int64 {
	if n == "" {
		return 0
	}
	if v, err := n.Int64(); err == nil {
		return v
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

// applyProtoJSONVector overlays the protojson spelling onto an already-decoded
// vector. Only fields the snake_case pass did not find are filled in, so a
// document in the documented dialect is untouched.
func applyProtoJSONVector(b []byte, out *nexusv1.ResourceVector) {
	var pj struct {
		CPUMillis      *json.Number     `json:"cpuMillis"`
		MemoryBytes    *json.Number     `json:"memoryBytes"`
		GPUCount       *json.Number     `json:"gpuCount"`
		GPUKind        string           `json:"gpuKind"`
		GPUMemoryBytes *json.Number     `json:"gpuMemoryBytes"`
		DiskBytes      *json.Number     `json:"diskBytes"`
		Custom         map[string]int64 `json:"custom"`

		GPUShareMilli       *json.Number              `json:"gpuShareMilli"`
		GPUShareMemoryBytes *json.Number              `json:"gpuShareMemoryBytes"`
		GPUShares           map[string]shareJSONLoose `json:"gpuShares"`
	}
	// A document that is not this dialect simply yields no fields; that is the
	// snake_case case, already handled above.
	if err := json.Unmarshal(b, &pj); err != nil {
		return
	}
	if v, ok := numInt64(pj.CPUMillis); ok && out.CpuMillis == 0 {
		out.CpuMillis = v
	}
	if v, ok := numInt64(pj.MemoryBytes); ok && out.MemoryBytes == 0 {
		out.MemoryBytes = v
	}
	if v, ok := numInt64(pj.GPUCount); ok && out.GpuCount == 0 {
		out.GpuCount = int32(v)
	}
	if pj.GPUKind != "" && out.GpuKind == "" {
		out.GpuKind = pj.GPUKind
	}
	if v, ok := numInt64(pj.GPUMemoryBytes); ok && out.GpuMemoryBytes == 0 {
		out.GpuMemoryBytes = v
	}
	if v, ok := numInt64(pj.DiskBytes); ok && out.DiskBytes == 0 {
		out.DiskBytes = v
	}
	if len(pj.Custom) > 0 && len(out.Custom) == 0 {
		out.Custom = pj.Custom
	}
	if v, ok := numInt64(pj.GPUShareMilli); ok && out.GpuShareMilli == 0 {
		out.GpuShareMilli = int32(v)
	}
	if v, ok := numInt64(pj.GPUShareMemoryBytes); ok && out.GpuShareMemoryBytes == 0 {
		out.GpuShareMemoryBytes = v
	}
	if len(pj.GPUShares) > 0 && len(out.GpuShares) == 0 {
		tmp := &nexusv1.ResourceVector{}
		decodeFractionalGPU(&vectorJSONLoose{GPUShares: pj.GPUShares}, tmp)
		out.GpuShares = tmp.GpuShares
	}
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
	// Allocate from workflow_runs.event_seq, per CONTRACT.md's EVENT SEQ rule.
	//
	// This used to be `max(seq)+1` under an advisory lock, which looked safe and
	// was not. The lock serializes only the callers that TAKE it, and this is not
	// the only writer: event-service appends through its own owner API, which
	// allocates from the event_seq counter and takes no advisory lock. The two
	// allocators then collided outright -- observed live as
	//
	//   duplicate key value violates unique constraint "run_events_run_id_seq_key"
	//
	// on a run whose counter read 2 while its actual max(seq) was 11, because
	// max(seq)+1 never advanced event_seq. 31 runs had drifted apart this way.
	// The UPDATE takes a row lock on the run for the rest of the transaction,
	// which serializes every allocator on one mechanism instead of two.
	var seq int32
	if err := tx.QueryRow(ctx,
		`UPDATE workflow_runs SET event_seq = event_seq + 1 WHERE id = $1 RETURNING event_seq`,
		runID,
	).Scan(&seq); err != nil {
		return fmt.Errorf("allocate run event seq for %s: %w", kind, err)
	}
	// The outbox row rides in the same statement (see outbox.go): this helper is
	// how the §5.3 claim records AttemptDispatched, so the announcement commits
	// with the dispatch and costs no extra round trip under the claim's locks.
	if _, err := tx.Exec(ctx, `
WITH ev AS (
  INSERT INTO run_events (run_id, seq, node_id, attempt_id, kind, payload, actor)
  VALUES ($1, $2, $3, $4, $5, $6, $7)
  RETURNING id
)
INSERT INTO outbox (topic, ref_id, trace_context) SELECT 'run_event', id, $8 FROM ev`,
		runID, seq, nullString(nodeID), attemptID, kind, body, actor, tracing.NullableTraceparent(ctx),
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

// protojsonMarshal encodes a protobuf message for a JSONB column.
//
// protojson rather than encoding/json: a protobuf message's Go struct carries
// unexported state fields, and encoding/json would either serialize garbage or
// omit the real ones. These are REFERENCES (ResultRef, TaskError), not payloads —
// the payload itself never transits the control plane (invariant 6).
// UseProtoNames keeps stored JSONB in snake_case, matching the proto source,
// the schema comments and operator SQL. See marshalVec in rm_workers.go for why
// the camelCase default is actively harmful here: a missing JSONB key is SQL
// NULL, not an error, so the mismatch fails silently.
func protojsonMarshal(m proto.Message) ([]byte, error) {
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal proto message: %w", err)
	}
	return b, nil
}
