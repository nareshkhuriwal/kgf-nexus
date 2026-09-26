// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// The usage ledger (migration 0027, ADR-0021) is OWNED by the scheduler.
//
// One row per attempt that held a reservation, written by recordUsage inside
// the SAME transaction as the attempt's terminal transition. The three callers
// are the only paths by which a dispatched attempt ends:
//
//	ApplyResult       worker report: SUCCEEDED, FAILED, TIMED_OUT, LOST
//	expireOne         lease expiry or worker death: LOST
//	cancelOneAttempt  operator or run cancel: CANCELLED
//
// Idempotent twice over. The terminal CAS in each caller lets exactly one
// transition happen, so a duplicate or late report never reaches this code; and
// UNIQUE(attempt_id) with ON CONFLICT DO NOTHING makes a second insert a no-op
// even for a future caller that forgot the CAS.

// OutboxTopicUsageRecorded announces one ledger row. The relay publishes it as
// nexus.usage.recorded.<namespace> (the metering export hook).
const OutboxTopicUsageRecorded = "usage_recorded"

// recordUsageSQL derives the whole row from the attempt as it stands after the
// terminal UPDATE, and writes the outbox announcement in the same statement
// (the outbox.go convention: no extra round trip, cannot be forgotten).
//
// Derived quantities are exact NUMERIC: EXTRACT(EPOCH FROM interval) is numeric
// on Postgres 14+, and the inputs are integers.
//
//   - wall_seconds: ended - started, or ended - dispatched if it never started.
//   - reserved_seconds: ended - dispatched, the time the reservation held
//     capacity (dispatch reserves; the terminal transition releases).
//   - *_seconds: the reserved vector times reserved_seconds.
//
// The executor is the one the task declared (both spec dialects, as the claim
// reads it); the pool is the namespace policy's pool serving the queue, if any.
const recordUsageSQL = `
WITH src AS (
  SELECT a.id, a.tenant_id, r.namespace, a.run_id, a.node_id, a.attempt_number,
         a.state::text AS outcome, a.queue,
         (SELECT qp.pool FROM namespace_queue_pools qp
           WHERE qp.tenant_id = a.tenant_id AND qp.queue = a.queue) AS pool,
         a.worker_id, w.hostname,
         NULLIF(COALESCE(n.spec_json #>> '{runtime,executor}', n.spec_json #>> '{executor}', ''), '') AS executor,
         COALESCE((a.resource_request ->> 'cpu_millis')::bigint, 0)   AS cpu_millis,
         COALESCE((a.resource_request ->> 'memory_bytes')::bigint, 0) AS memory_bytes,
         COALESCE((a.resource_request ->> 'gpu_count')::int, 0)       AS gpu_count,
         NULLIF(a.resource_request ->> 'gpu_kind', '')                AS gpu_kind,
         CASE WHEN jsonb_typeof(a.resource_request -> 'custom') = 'object'
              THEN a.resource_request -> 'custom' ELSE '{}'::jsonb END AS custom,
         a.dispatched_at, a.started_at,
         GREATEST(a.ended_at, a.dispatched_at) AS ended_at,
         a.metrics
    FROM task_attempts a
    JOIN workflow_runs r ON r.id = a.run_id
    LEFT JOIN task_nodes n ON n.run_id = a.run_id AND n.node_id = a.node_id
    LEFT JOIN workers    w ON w.id = a.worker_id
   WHERE a.id = $1
     AND a.dispatched_at IS NOT NULL
     AND a.ended_at IS NOT NULL
     AND a.state IN ('SUCCEEDED','FAILED','TIMED_OUT','LOST','CANCELLED')
), calc AS (
  SELECT src.*,
         GREATEST(0, EXTRACT(EPOCH FROM (ended_at - GREATEST(COALESCE(started_at, dispatched_at), dispatched_at)))) AS wall_s,
         EXTRACT(EPOCH FROM (ended_at - dispatched_at)) AS reserved_s
    FROM src
), ins AS (
  INSERT INTO usage_ledger (
      attempt_id, tenant_id, namespace, run_id, node_id, attempt_number, outcome,
      queue, pool, worker_id, worker_hostname, executor,
      cpu_millis, memory_bytes, gpu_count, gpu_kind, custom,
      dispatched_at, started_at, ended_at,
      wall_seconds, reserved_seconds, cpu_core_seconds, memory_gib_seconds, gpu_seconds,
      measured_cpu_seconds, measured_peak_memory_bytes, measured_wall_ms,
      measured_oom_killed, measured_exit_code)
  SELECT id, tenant_id, namespace, run_id, node_id, attempt_number, outcome,
         queue, pool, worker_id, hostname, executor,
         cpu_millis, memory_bytes, gpu_count, gpu_kind, custom,
         dispatched_at, started_at, ended_at,
         wall_s, reserved_s,
         cpu_millis::numeric * reserved_s / 1000,
         memory_bytes::numeric * reserved_s / 1073741824,
         gpu_count::numeric * reserved_s,
         (metrics ->> 'cpu_seconds')::double precision,
         (metrics ->> 'peak_memory_bytes')::bigint,
         (metrics ->> 'wall_ms')::bigint,
         CASE WHEN metrics ? 'oom_killed' THEN (metrics ->> 'oom_killed')::int <> 0 END,
         (metrics ->> 'exit_code')::int
    FROM calc
  ON CONFLICT (attempt_id) DO NOTHING
  RETURNING id, attempt_id, tenant_id, namespace, run_id, node_id, attempt_number, outcome,
            queue, pool, ended_at, reserved_seconds, cpu_core_seconds, memory_gib_seconds, gpu_seconds
), ob AS (
  INSERT INTO outbox (topic, namespace, routing_key, payload, trace_context)
  SELECT 'usage_recorded', namespace, run_id::text,
         jsonb_build_object(
           'ledger_id', id, 'attempt_id', attempt_id, 'tenant_id', tenant_id,
           'namespace', namespace, 'run_id', run_id, 'node_id', node_id,
           'attempt_number', attempt_number, 'outcome', outcome, 'queue', queue,
           'pool', pool, 'ended_at', ended_at, 'reserved_seconds', reserved_seconds,
           'cpu_core_seconds', cpu_core_seconds, 'memory_gib_seconds', memory_gib_seconds,
           'gpu_seconds', gpu_seconds),
         $2
    FROM ins
)
SELECT outcome FROM ins`

// recordUsageWithUnitsSQL is recordUsageSQL with the attempt's reported usage
// units (attempt_usage, migration 0036, ADR-0032 §2) merged into `custom`, and
// announced as usage_units on the outbox row. Same statement, so the units are
// recorded in the same transaction as the terminal transition. Usage keys all
// contain ':' and no custom resource name can, so `||` never overwrites a
// reserved resource. Every terminal caller locked the attempt row FOR UPDATE
// before this statement, and RecordUsage holds FOR NO KEY UPDATE on it while it
// writes, so the units read here are final: a report either committed before
// the transition or is refused as stale after it.
var recordUsageWithUnitsSQL = strings.NewReplacer(
	`THEN a.resource_request -> 'custom' ELSE '{}'::jsonb END AS custom,`,
	`THEN a.resource_request -> 'custom' ELSE '{}'::jsonb END
           || COALESCE((SELECT u.units FROM attempt_usage u WHERE u.attempt_id = a.id), '{}'::jsonb) AS custom,`,
	`queue, pool, ended_at, reserved_seconds, cpu_core_seconds, memory_gib_seconds, gpu_seconds
), ob AS (`,
	`queue, pool, ended_at, reserved_seconds, cpu_core_seconds, memory_gib_seconds, gpu_seconds,
            (SELECT COALESCE(jsonb_object_agg(e.key, e.value), '{}'::jsonb)
               FROM jsonb_each(custom) e WHERE strpos(e.key, ':') > 0) AS usage_units
), ob AS (`,
	`'gpu_seconds', gpu_seconds)`,
	`'gpu_seconds', gpu_seconds, 'usage_units', usage_units)`,
).Replace(recordUsageSQL)

// recordUsage writes the attempt's ledger row in the caller's transaction and
// returns its outcome, or "" when nothing was written: the attempt never held a
// reservation (it was cancelled or timed out while PENDING), or its row already
// exists.
func (s *SchedulerStore) recordUsage(ctx context.Context, tx pgx.Tx, attemptID uuid.UUID) (string, error) {
	if s.meteringMissing {
		return "", nil
	}
	q := recordUsageSQL
	if !s.workloadMissing {
		q = recordUsageWithUnitsSQL
	}
	var outcome string
	err := tx.QueryRow(ctx, q, attemptID, tracing.NullableTraceparent(ctx)).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("record usage for attempt %s: %w", attemptID, err)
	}
	return outcome, nil
}

// executionMetricsJSON is the on-disk shape of task_attempts.metrics: the
// worker's measured ExecutionMetrics, snake_case with numbers as numbers for
// the same reason as vectorJSON (ad-hoc SQL reads it, and the ledger casts it).
type executionMetricsJSON struct {
	CPUSeconds      float64 `json:"cpu_seconds"`
	PeakMemoryBytes int64   `json:"peak_memory_bytes"`
	WallMs          int64   `json:"wall_ms"`
	OOMKilled       int32   `json:"oom_killed"`
	ExitCode        int32   `json:"exit_code"`
}

// encodeExecutionMetrics returns nil when the report carried no metrics, so the
// column stays NULL -- "not measured" -- rather than a row of zeros.
func encodeExecutionMetrics(m *nexusv1.ExecutionMetrics) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(executionMetricsJSON{
		CPUSeconds: m.GetCpuSeconds(), PeakMemoryBytes: m.GetPeakMemoryBytes(),
		WallMs: m.GetWallMs(), OOMKilled: m.GetOomKilled(), ExitCode: m.GetExitCode(),
	})
	if err != nil {
		return nil, fmt.Errorf("encode execution metrics: %w", err)
	}
	return b, nil
}
