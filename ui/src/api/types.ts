/**
 * Wire types for the Nexus REST API (architecture §8.2).
 *
 * These are hand-written against the FROZEN contract — the proto enums in
 * proto/nexus/v1/*.proto and the column names in
 * services/migrations/0001_init.up.sql — deliberately, not generated from the
 * Go services. The services are built concurrently; generating from their
 * source would couple the UI to whichever half-finished handler happened to
 * exist. The contract is the contract.
 *
 * State values are the bare canonical names (PENDING, UPSTREAM_FAILED, ...),
 * matching the Postgres enum labels rather than the protobuf
 * `NODE_STATE_`-prefixed spelling. The DB enum is what the JSON projection
 * serialises, and `normalizeState` below tolerates the prefixed form anyway so
 * a gateway that marshals protobuf enums directly still renders correctly.
 */

export const RUN_STATES = [
  'PENDING',
  'RUNNING',
  'PAUSING',
  'PAUSED',
  'SUCCEEDED',
  'FAILED',
  'CANCELLING',
  'CANCELLED',
  'TIMED_OUT',
] as const;
export type RunState = (typeof RUN_STATES)[number];

export const NODE_STATES = [
  'PENDING',
  'READY',
  'RUNNING',
  'SUCCEEDED',
  'FAILED',
  'SKIPPED',
  'UPSTREAM_FAILED',
  'CANCELLED',
] as const;
export type NodeState = (typeof NODE_STATES)[number];

export const ATTEMPT_STATES = [
  'PENDING',
  'DISPATCHED',
  'RUNNING',
  'SUCCEEDED',
  'FAILED',
  'TIMED_OUT',
  'LOST',
  'CANCELLED',
] as const;
export type AttemptState = (typeof ATTEMPT_STATES)[number];

/** worker_state enum. QUARANTINED exists in the schema but is Phase 2. */
export const WORKER_STATES = ['ACTIVE', 'DRAINING', 'QUARANTINED', 'DEAD'] as const;
export type WorkerState = (typeof WORKER_STATES)[number];

export const ERROR_CLASSES = [
  'TRANSIENT',
  'TIMEOUT',
  'WORKER_LOST',
  'VALIDATION',
  'PERMISSION_DENIED',
  'APPLICATION',
  'OOM',
  'NOT_FOUND',
  'INFRASTRUCTURE',
] as const;
export type ErrorClass = (typeof ERROR_CLASSES)[number];

/** ResourceVector (common.proto). Absent dimensions mean zero, not unknown. */
export interface ResourceVector {
  cpu_millis?: number;
  memory_bytes?: number;
  gpu_count?: number;
  gpu_kind?: string;
  gpu_memory_bytes?: number;
  disk_bytes?: number;
  custom?: Record<string, number>;
}

export interface ResourceRequest {
  request?: ResourceVector;
  limit?: ResourceVector;
}

export interface TaskError {
  class?: ErrorClass | string;
  type?: string;
  message?: string;
  retryable?: boolean;
  detail_ref?: string;
}

export interface ExecutionMetrics {
  peak_memory_bytes?: number;
  cpu_seconds?: number;
  wall_ms?: number;
  oom_killed?: number;
  exit_code?: number;
}

export interface RunSummary {
  id: string;
  namespace: string;
  spec_id: string;
  spec_name?: string;
  spec_version?: number;
  state: RunState;
  priority?: number;
  trigger_kind?: string;
  created_at: string;
  started_at?: string | null;
  ended_at?: string | null;
  /** Per-node-state counts, so the list can show progress without N queries. */
  node_counts?: Partial<Record<NodeState, number>>;
}

export interface RunNode {
  run_id: string;
  node_id: string;
  task_ref: string;
  state: NodeState;
  trigger_rule?: string;
  max_attempts?: number;
  attempts_used?: number;
  /** Denormalised convenience field; UI never depends on it being present. */
  latest_attempt_id?: string | null;
  pending_reason?: string | null;
}

export interface RunEdge {
  from_node_id: string;
  to_node_id: string;
  output_to_input?: Record<string, string>;
}

/** GET /v1/namespaces/{ns}/runs/{id} — detail plus node states (§8.2). */
export interface RunDetail extends RunSummary {
  nodes: RunNode[];
  edges: RunEdge[];
  params?: Record<string, unknown>;
  deadline_at?: string | null;
  fail_fast?: boolean;
}

export interface Attempt {
  id: string;
  run_id: string;
  node_id: string;
  attempt_number: number;
  state: AttemptState;
  queue?: string;
  priority?: number;
  resource_request?: ResourceRequest;
  idempotency_key?: string;
  worker_id?: string | null;
  /** Hostname is far more useful than a UUID in an operator view. */
  worker_hostname?: string | null;
  /**
   * Structured explanation for a PENDING attempt, e.g.
   * "waiting_for_resources: gpu (need 4, free 3)". Without surfacing this the
   * UI shows an unexplained stall.
   */
  pending_reason?: string | null;
  error?: TaskError | null;
  metrics?: ExecutionMetrics | null;
  scheduled_at?: string | null;
  dispatched_at?: string | null;
  started_at?: string | null;
  ended_at?: string | null;
}

export interface RunEvent {
  id: number;
  run_id: string;
  seq: number;
  node_id?: string | null;
  attempt_id?: string | null;
  kind: string;
  payload?: Record<string, unknown>;
  /** scheduler | worker:<id> | user:<sub>. Every transition is attributable. */
  actor: string;
  occurred_at: string;
}

export interface Worker {
  id: string;
  namespace: string;
  hostname: string;
  queues: string[];
  executors?: string[];
  labels?: Record<string, string>;
  capacity: ResourceVector;
  committed?: ResourceVector;
  state: WorkerState;
  agent_version?: string;
  registered_at?: string;
  last_heartbeat_at: string;
  cpu_utilization?: number;
  memory_utilization?: number;
}

/** GET /v1/namespaces/{ns}/capacity — advertised vs committed (§8.2). */
export interface CapacitySummary {
  advertised: ResourceVector;
  committed: ResourceVector;
  worker_count?: number;
  active_worker_count?: number;
}

export interface LogLine {
  ts?: string;
  stream?: 'stdout' | 'stderr' | string;
  message: string;
}

export interface Paged<T> {
  items: T[];
  next_page_token?: string;
}
