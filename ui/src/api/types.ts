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
  /**
   * The shape the gateway actually serialises today: a flat `workers` count and
   * a queue depth, rather than the worker_count/active_worker_count pair above.
   * Both spellings are declared because the console must read the deployed
   * gateway without waiting for the contract to be reconciled, and a field that
   * silently arrives as undefined renders as a confident em dash.
   *
   * `pending_attempts` is the only queue-depth number the platform exposes. It
   * counts attempts admitted and not yet dispatched; there is no queue-age
   * endpoint, so anything time-shaped about the queue has to be derived
   * client-side and labelled as such.
   */
  workers?: number;
  pending_attempts?: number;
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

/**
 * Schedules and backfills.
 *
 * Field names come from the gateway's schedulePayload/backfillPayload
 * (services/api-gateway/rest/schedules.go), not from the store structs: the
 * payload renames a few columns on the way out (cron_expr → `cron`, from_time →
 * `from`), and the wire name is the one the console has to speak.
 */

/**
 * What a schedule does when its previous run is still going.
 *
 * Declared as a list rather than a bare union so the create form can render the
 * options without a second copy of them drifting out of step with the type.
 */
export const OVERLAP_POLICIES = ['skip', 'allow', 'queue_one'] as const;
export type OverlapPolicy = (typeof OVERLAP_POLICIES)[number];

/** One-line explanation per policy, shown next to the control that sets it. */
export const OVERLAP_POLICY_HINTS: Record<OverlapPolicy, string> = {
  skip: 'If the previous run is still going, the tick is dropped. The safest default.',
  allow: 'Fire regardless. Two runs of this workflow may execute concurrently.',
  queue_one: 'Hold at most one tick back and fire it when the previous run ends.',
};

export const BACKFILL_STATES = ['RUNNING', 'PAUSED', 'COMPLETED', 'CANCELLED', 'FAILED'] as const;
export type BackfillState = (typeof BACKFILL_STATES)[number];

export interface Schedule {
  id: string;
  namespace: string;
  name: string;
  spec_name: string;
  /** Absent means "always the latest version of the spec". */
  spec_version?: number;
  cron: string;
  timezone: string;
  params?: Record<string, unknown> | null;
  priority: number;
  overlap_policy: OverlapPolicy | string;
  catchup_limit: number;
  enabled: boolean;
  /**
   * Reported as stored, INCLUDING null. A null does not simply mean "disabled":
   * it also covers a schedule claimed by a replica firing it right now, and one
   * left unarmed by a replica that died mid-fire. The gateway deliberately does
   * not paper over that, so neither does the console.
   */
  next_fire_at: string | null;
  last_fire_at: string | null;
  created_at: string;
  updated_at: string;
  created_by: string;
  /** Upcoming fire times, present on create and on detail reads. */
  next_fires?: string[];
  /**
   * Set instead of next_fires when a STORED expression no longer parses — a
   * schedule that looks armed and can never fire. It is the reason an operator
   * came looking, so it is never hidden.
   */
  cron_error?: string;
}

/** GET /schedules/preview — answers "is this expression good" with a 200 either way. */
export interface CronPreview {
  cron: string;
  timezone: string;
  valid: boolean;
  /** Present when valid is false: why the expression was rejected. */
  error?: string;
  next_fires?: string[];
}

export interface Backfill {
  id: string;
  schedule_id: string;
  /**
   * The owning schedule's name, when the gateway could resolve it. Absent
   * rather than empty when it could not, so "not supplied" is distinguishable
   * from a schedule genuinely named the empty string.
   */
  schedule_name?: string;
  namespace: string;
  from: string;
  to: string;
  /** Next logical tick to generate, or null once the interval is exhausted. */
  cursor_at: string | null;
  max_active: number;
  max_runs: number;
  runs_created: number;
  state: BackfillState | string;
  detail?: Record<string, unknown> | null;
  created_at: string;
  updated_at: string;
  ended_at: string | null;
  created_by: string;
}

export interface CreateScheduleRequest {
  name: string;
  spec_name: string;
  spec_version?: number | null;
  cron: string;
  timezone: string;
  params?: Record<string, unknown>;
  priority?: number;
  overlap_policy?: string;
  catchup_limit?: number;
  enabled?: boolean;
}

export interface StartBackfillRequest {
  from: string;
  to: string;
  max_active: number;
  max_runs: number;
}
