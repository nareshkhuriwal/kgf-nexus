// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

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

/** worker_state enum. QUARANTINED = taken out of placement as a poison host (§5.5). */
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
  /** Dynamic expansion (§5.4): set on a child, naming its map template. */
  parent_node_id?: string | null;
  /** Dynamic expansion: the child's position in the mapped collection. */
  map_index?: number | null;
  /**
   * Why the node failed without running, e.g. a fan-in refused for exceeding
   * the platform limit (§5.4). Absent for ordinary failures, whose reason is on
   * the attempt.
   */
  failure_reason?: string | null;
}

/** One dynamic expansion of a map template (§5.4). */
export interface RunExpansion {
  template_node_id: string;
  source_node_id: string;
  source_output: string;
  item_count: number;
  max_fanout: number;
  /** EXPANDED, or FAILED when refused (e.g. max_fanout_exceeded). */
  outcome: 'EXPANDED' | 'FAILED';
  reason?: string | null;
  detail?: string | null;
  created_at?: string;
}

/** One quarantine episode of a worker (§5.5). */
export interface WorkerQuarantine {
  id: string;
  /** auto | manual | inherited */
  trigger: string;
  reason: string;
  evidence?: Record<string, unknown> | null;
  actor: string;
  started_at: string;
  expires_at?: string | null;
  released_at?: string | null;
  released_by?: string | null;
  release_reason?: string | null;
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
  /** Dynamic-expansion records; absent from gateways that predate them. */
  expansions?: RunExpansion[];
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
  /** Set when this attempt reused a memoized result instead of executing (§6.4). */
  cache_hit_of?: string | null;
  /**
   * The latest status line the WORKER's executor reported through
   * ReportProgress (F96), e.g. "pod Pending: Unschedulable: 0/3 nodes are
   * available: insufficient cpu". Not the same thing as pending_reason, which
   * is the SCHEDULER's reason for not placing the attempt. Sanitized by the
   * scheduler (one line, bounded); null when the worker never sent one, and
   * absent from gateways that predate the field.
   */
  progress_message?: string | null;
  /** Latest reported completion in [0, 1]; null when none was reported. */
  progress_fraction?: number | null;
  /** Control-plane time of the latest progress report. */
  progress_at?: string | null;
  /** Control-plane time the progress message last changed. */
  progress_message_at?: string | null;
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
  /**
   * How many attempts this worker is executing (DISPATCHED or RUNNING).
   *
   * Counted by the gateway with a correlated subquery rather than read from a
   * denormalised counter column, deliberately — a drifting counter is exactly
   * the kind of thing that misleads an operator mid-incident. Optional because
   * a gateway predating the field simply omits it, and 0 is a claim we would
   * then be making on its behalf.
   */
  active_attempts?: number;
}

/**
 * One task attributed to a worker.
 *
 * Not `Attempt`. The two describe the same row and are read from opposite
 * directions: on a run page the run is ambient and `task_ref` sits on the node
 * beside the attempt, whereas read from a WORKER both of those are the answer —
 * "this worker is running ml.train, in run 673321b5" — and neither is available
 * from the attempt row alone. The gateway joins task_nodes to supply task_ref,
 * so this type carries it as required rather than optional.
 */
export interface WorkerAttempt {
  id: string;
  run_id: string;
  node_id: string;
  /** The task this node executes, e.g. `ml.train:v2`. */
  task_ref: string;
  attempt_number: number;
  state: AttemptState;
  queue?: string;
  scheduled_at?: string | null;
  started_at?: string | null;
  ended_at?: string | null;
}

/**
 * GET /v1/namespaces/{ns}/workers/{id} — one worker plus the tasks on it.
 *
 * Two lists rather than one, because they answer different questions and a
 * single merged list would answer neither well:
 *
 * - `running_attempts` is DISPATCHED + RUNNING, the same predicate the fleet
 *   row's `active_attempts` counts. The two cannot disagree by construction,
 *   which is what stops an operator hunting for a task that is not there.
 * - `recent_attempts` is the terminal tail, bounded by the server. It is the
 *   only thing a DEAD worker can say about itself: its running set is
 *   necessarily empty, and "what was it doing when it went" is the whole
 *   question asked of a worker that just died.
 *
 * Both are optional: a gateway that predates this route serves the plain worker
 * payload, and the console must render that as "not reported" rather than as an
 * idle worker.
 */
export interface WorkerDetail extends Worker {
  running_attempts?: WorkerAttempt[];
  recent_attempts?: WorkerAttempt[];
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
  /** Line number, contiguous from 1 per attempt; the paging and resume cursor. */
  n?: number;
  ts?: string;
  /** stdout | stderr | combined (Kubernetes) | nexus (the platform's own lines). */
  stream?: 'stdout' | 'stderr' | 'combined' | 'nexus' | string;
  message: string;
  /** A line split at the length cap, or output without a trailing newline. */
  partial?: boolean;
  /** truncated | truncation_summary | dropped | gap, on the platform's lines. */
  kind?: string;
  /** For a gap line: the last line number it stands in for. */
  gap_end?: number;
}

/**
 * What the gateway knows about an attempt's durable log (ADR-0020).
 *
 * none: nothing recorded (not started, no output yet, or an agent/gateway older
 * than log shipping). live: shipping while the attempt runs. complete: the
 * agent finalized it. partial: the attempt ended without a final report (agent
 * crash, lost lease, object store outage) -- what was shipped is readable.
 * expired: the objects were removed by retention; the index remains.
 */
export type LogState = 'none' | 'live' | 'complete' | 'partial' | 'expired';

export interface LogStatus {
  state: LogState;
  attempt_state: string;
  lines: number;
  bytes: number;
  chunks: number;
  truncated_lines: number;
  truncated_bytes: number;
  dropped_lines: number;
  compacted: boolean;
  last_chunk_at?: string;
  finalized_at?: string;
  /** One honest sentence about this state, meant to be shown verbatim. */
  detail: string;
}

export interface LogPage {
  attempt_id: string;
  log: LogStatus;
  lines: LogLine[];
  first_line?: number;
  next_line: number;
  has_more: boolean;
  unreadable_chunks?: number;
}

export interface LogSearchResult {
  attempt_id: string;
  log: LogStatus;
  search: {
    matches: LogLine[];
    next_line: number;
    scanned_lines: number;
    scanned_bytes: number;
    complete: boolean;
    stopped_by?: 'matches' | 'bytes' | 'time';
    unreadable_chunks?: number;
  };
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

/**
 * The workflow catalog.
 *
 * Field names come from the gateway's catalog handlers, not from the store
 * structs. Two of these collections are deliberately different SHAPES of the
 * same table: `WorkflowSummary` is one row per workflow NAME (the answer to
 * "what exists here"), while `SpecVersion` is one row per submitted spec (the
 * answer to "what has this workflow been"). The list endpoint does not select
 * spec_json at all, so a definition is only ever reachable through
 * `SpecDefinition` — which is why the summary carries no `spec` field to
 * tempt a consumer into rendering half a graph.
 */

export interface WorkflowSummary {
  /** The spec id of the LATEST version, which is what a drill-down resolves. */
  id: string;
  namespace: string;
  name: string;
  latest_version: number;
  spec_hash: string;
  created_by: string;
  /** When the newest version landed, not when the workflow first appeared. */
  updated_at: string;
  version_count: number;
  first_created_at: string;
}

export interface SpecVersion {
  id: string;
  name: string;
  version: number;
  spec_hash: string;
  created_by: string;
  created_at: string;
}

/**
 * One task in a spec definition.
 *
 * Every field is optional because this is the AUTHORED document, not a
 * materialised projection: the SDK fills in whatever the author wrote and the
 * gateway passes spec_json through unmodified rather than re-marshalling it
 * (re-encoding would risk handing back bytes whose hash no longer matches the
 * spec_hash beside them). A task with no `retry` block genuinely has no retry
 * policy, and the console must render that as "not set" rather than inventing
 * a default the engine may not actually apply.
 */
export interface SpecTask {
  id: string;
  task_ref?: string;
  trigger_rule?: string;
  queue?: string;
  version?: number;
  resources?: ResourceRequest;
  retry?: {
    max_attempts?: number;
    strategy?: string;
    initial_ms?: number;
    max_ms?: number;
    multiplier?: number;
    jitter?: string;
    retry_on?: string[];
    no_retry_on?: string[];
    retry_on_different_worker?: boolean;
  };
  timeouts?: Record<string, number>;
  runtime?: { executor?: string; command?: string[]; args?: string[] };
  inputs?: Record<string, { type?: string; required?: boolean }>;
  outputs?: Record<string, { type?: string; required?: boolean }>;
  input_params?: Record<string, string>;
  cache?: { enabled?: boolean };
  /**
   * ADR-0015. Absent means interruptible (the ordinary SIGTERM → grace →
   * SIGKILL). max_uninterruptible_ms absent or 0 means "the worker's ceiling".
   */
  cancellation?: { uninterruptible?: boolean; max_uninterruptible_ms?: number };
  idempotency?: string;
  concurrency_key?: string;
  concurrency_limit?: number;
  node_selector?: Record<string, string>;
  prefer_same_worker_as?: string;
}

/** An edge in a spec definition. Identical shape to RunEdge, by construction. */
export interface SpecEdge {
  from_node_id: string;
  to_node_id: string;
  output_to_input?: Record<string, string>;
}

/**
 * One declared run parameter.
 *
 * This is the only machine-readable description of what a run submission may
 * carry, so the submit form derives its guidance from here rather than from a
 * hand-kept list that would drift from whatever the SDK last published.
 */
export interface SpecParam {
  type?: string;
  required?: boolean;
  default?: unknown;
  description?: string;
}

export interface SpecDocument {
  name?: string;
  namespace?: string;
  api_version?: string;
  description?: string;
  labels?: Record<string, string>;
  params?: Record<string, SpecParam>;
  run_timeout_ms?: number;
  max_active_runs?: number;
  tasks?: SpecTask[];
  edges?: SpecEdge[];
}

/** GET /specdefs/{id} — the definition, addressed by the spec id a run pins. */
export interface SpecDefinition {
  id: string;
  namespace: string;
  name: string;
  version: number;
  spec_hash: string;
  created_by: string;
  created_at: string;
  spec: SpecDocument;
}

/**
 * POST /runs. Either spec_id or spec_name is required; the catalog always sends
 * spec_id, because the catalog's whole premise is that the operator chose a
 * specific VERSION — resolving by name would silently run a newer one.
 */
export interface CreateRunRequest {
  spec_id?: string;
  spec_name?: string;
  params?: Record<string, unknown>;
  priority?: number;
  trigger_kind?: string;
}

// ── attention triage ────────────────────────────────────────────────────────

/**
 * Why an item is on the triage list. Open-ended on purpose: the server ranks and
 * classifies, and a kind this build has never heard of must still render its
 * summary rather than disappear, so the union carries a string escape hatch.
 */
export type AttentionKind =
  | 'indeterminate_outcome'
  | 'retry_exhausted'
  | 'blocked_on_capacity'
  | 'run_failed'
  | 'run_stalled'
  | (string & {});

/** Two levels, not five: an operator triaging mid-incident sorts into now/soon. */
export type AttentionSeverity = 'critical' | 'warning' | (string & {});

/**
 * One row of GET /v1/namespaces/{ns}/attention.
 *
 * `link` is resolved SERVER-side and used verbatim. The server knows whether an
 * item is about a run or one attempt within it, so composing the href here from
 * run_id and attempt_id would be the console re-deriving something it was
 * already told — and getting it wrong the first time the shape changes.
 *
 * The optional fields are optional per ITEM, not per deployment: a run_failed
 * row has no node, a blocked attempt has no retry reason. They are therefore
 * tested for PRESENCE (`!== undefined`) rather than truthiness, because
 * `node_id: ""` and `reason: ""` are server answers, not absences.
 */
export interface AttentionItem {
  kind: AttentionKind;
  severity: AttentionSeverity;
  summary: string;
  detail: string;
  link: string;
  run_id: string;
  run_state: RunState;
  spec_name?: string;
  occurred_at: string;
  node_id?: string;
  attempt_id?: string;
  reason?: string;
}

/**
 * The triage response.
 *
 * `items` arrives ranked and deterministic, so the console renders it in the
 * order given. Re-sorting client-side would put a cheap local heuristic in front
 * of the server's ranking and make two browsers disagree about what matters most.
 *
 * `count` describes the items RETURNED, and `truncated` says the namespace holds
 * more than the limit asked for — which is why the page needs a "view all"
 * affordance rather than a total it could subtract from.
 */
export interface AttentionList {
  count: number;
  truncated: boolean;
  items: AttentionItem[];
}

/**
 * The one `reason` that unlocks a force-retry affordance.
 *
 * Keyed off `reason`, never off `kind`: an indeterminate_outcome KIND says the
 * platform could not tell whether the work happened, while this REASON says the
 * platform has decided a human must choose. Gating a destructive re-run on the
 * kind would offer it for items the server never asked a human about.
 */
export const ATTENTION_REASON_NEEDS_HUMAN = 'indeterminate_outcome_requires_human';

// ── fleet connectivity ──────────────────────────────────────────────────────

/**
 * gRPC channel state as this gateway last observed it.
 *
 * IDLE is NOT a fault: a channel that has not been dialled since startup reports
 * IDLE, which is the normal state of an upstream nobody has needed yet.
 */
export type UpstreamState =
  | 'READY'
  | 'IDLE'
  | 'CONNECTING'
  | 'TRANSIENT_FAILURE'
  | 'SHUTDOWN'
  | 'UNCONFIGURED'
  | (string & {});

/**
 * One upstream row.
 *
 * `configured: false` is NOT a fault either — it means this deployment never
 * asked for that upstream. Counting either IDLE or unconfigured as broken would
 * make a healthy single-queue deployment render as a red dashboard.
 */
export interface FleetUpstream {
  name: string;
  addr: string;
  state: UpstreamState;
  configured: boolean;
}

/**
 * GET /v1/health/fleet — NOT namespaced, because there is one control plane.
 *
 * `status` is the server's verdict and the only thing that decides ok vs
 * degraded. The console does not recompute it from the rows: the gateway knows
 * which upstreams this deployment actually requires, and a client-side rollup
 * would have to guess that and would disagree on the deployments that matter.
 *
 * `measures` is the payload's own description of what it measured — gRPC
 * connectivity read from the local channel, not each service's own /readyz. It
 * is surfaced verbatim rather than paraphrased into "health", because the gap
 * between "the channel is up" and "the service is ready" is exactly what an
 * operator reading this panel mid-incident must not have hidden from them.
 */
export interface FleetHealth {
  status: 'ok' | 'degraded' | (string & {});
  upstreams: FleetUpstream[];
  measures?: string;
}

/**
 * The run lifecycle transitions the gateway exposes.
 *
 * Written as a union rather than free strings because the value goes straight
 * into a URL path: a typo would produce a 404 that reads like a missing run
 * rather than a missing route.
 *
 * RETRY has semantics worth stating wherever it is offered: the
 * workflow-manager restarts a FAILED run from its failed nodes, so successful
 * work is not repeated. An operator who reads "retry" as "run the whole thing
 * again" will make a different decision than the one this actually performs.
 */
export type RunAction = 'pause' | 'resume' | 'cancel' | 'retry';

// ── events: notifications, webhook triggers, sensors, assets (ADR-0028) ─────

export type ChannelKind = 'slack_webhook' | 'slack_bot' | 'teams' | 'pagerduty' | 'webhook' | 'email';

export interface ChannelSecretInfo {
  configured: boolean;
  /** `sealed` is stored encrypted by the gateway; `ref` names env:/file: on the server. */
  source: 'sealed' | 'ref' | '';
  hint?: string;
  ref?: string;
}

export interface NotificationChannel {
  id: string;
  name: string;
  kind: ChannelKind;
  config: Record<string, unknown>;
  secret: ChannelSecretInfo;
  rate_limit_per_minute: number;
  enabled: boolean;
  created_by: string;
  created_at: string;
  updated_at: string;
  revision: number;
}

/** Returned by POST only: a generated webhook signing secret is shown once. */
export interface CreatedChannel extends NotificationChannel {
  signing_secret?: string;
}

export interface ChannelRequest {
  name?: string;
  kind?: ChannelKind;
  config?: Record<string, unknown>;
  secret?: string;
  secret_ref?: string;
  rate_limit_per_minute?: number;
  enabled?: boolean;
  revision?: number;
}

export interface ChannelList {
  channels: NotificationChannel[];
  kinds: string[];
}

export interface NotificationRule {
  id: string;
  name: string;
  events: string[];
  workflows: string[];
  labels: Record<string, string>;
  sla_seconds?: number | null;
  enabled: boolean;
  channels: string[];
  created_by: string;
  created_at?: string;
  updated_at?: string;
  revision: number;
}

export interface RuleRequest {
  name?: string;
  events: string[];
  workflows: string[];
  labels: Record<string, string>;
  sla_seconds?: number;
  channels: string[];
  enabled: boolean;
  revision?: number;
}

export interface RuleList {
  rules: NotificationRule[];
  events: string[];
}

export type DeliveryState = 'pending' | 'delivered' | 'dead';

export interface NotificationDelivery {
  /** A bigint on the server: serialised as a JSON number. */
  id: number | string;
  channel: string;
  channel_kind: string;
  rule: string;
  event_type: string;
  action: string;
  run_id: string | null;
  payload: unknown;
  state: DeliveryState;
  attempts: number;
  max_attempts: number;
  next_attempt_at: string | null;
  last_error: string | null;
  last_status?: number | null;
  dedup_key: string;
  created_at: string;
  delivered_at: string | null;
  dead_at: string | null;
  redrive_of?: number | string | null;
}

export interface DeliveryFilter {
  state?: DeliveryState | '';
  channel?: string;
  run_id?: string;
  limit?: number;
  before?: string;
}

export interface DeliveryPage {
  deliveries: NotificationDelivery[];
  nextBefore?: string;
}

export interface WebhookTrigger {
  id: string;
  name: string;
  workflow: string;
  workflow_version?: number | null;
  params_mapping: Record<string, unknown>;
  idempotency_key_path: string;
  priority: number;
  tolerance_seconds: number;
  enabled: boolean;
  secret_rotated_at: string | null;
  previous_secret_expires_at: string | null;
  created_by: string;
  created_at: string;
  last_fired_at: string | null;
  fire_count: number;
  revision: number;
  path: string;
  url?: string;
  signature_scheme: string;
}

/** Create and rotate return the plaintext secret exactly once. */
export interface TriggerWithSecret extends WebhookTrigger {
  secret?: string;
}

export interface TriggerRequest {
  name?: string;
  workflow?: string;
  workflow_version?: number;
  params_mapping?: Record<string, unknown>;
  idempotency_key_path?: string;
  priority?: number;
  tolerance_seconds?: number;
  enabled?: boolean;
  revision?: number;
}

export interface TriggerDelivery {
  received_at: string;
  /** accepted | deduplicated | rejected */
  outcome: string;
  http_status: number;
  run_id: string | null;
  idempotency_key: string | null;
  detail: string | null;
  signature_digest: string;
}

export type SensorKind = 'object_store' | 'run_succeeded';

export interface SensorState {
  status: string;
  baseline_done: boolean;
  cursor: string | null;
  watermark: string | null;
  next_poll_at: string | null;
  lease_owner: string | null;
  lease_until: string | null;
  last_poll_at: string | null;
  last_success_at: string | null;
  last_error: string | null;
  consecutive_failures: number;
  observed_total: number;
  triggered_total: number;
  pending: number;
  definition_current: boolean;
}

export interface Sensor {
  id: string;
  name: string;
  kind: SensorKind;
  workflow: string;
  workflow_version?: number | null;
  config: Record<string, unknown>;
  params_mapping: Record<string, unknown>;
  poll_interval_seconds: number;
  backfill: boolean;
  priority: number;
  enabled: boolean;
  /** disabled | waiting | initializing | ok | error | stale */
  status: string;
  state?: SensorState | null;
  created_by: string;
  created_at?: string;
  updated_at?: string;
  revision: number;
}

export interface SensorRequest {
  name?: string;
  kind?: SensorKind;
  workflow?: string;
  config?: Record<string, unknown>;
  params_mapping?: Record<string, unknown>;
  poll_interval_seconds?: number;
  backfill?: boolean;
  enabled?: boolean;
  revision?: number;
}

export interface SensorObservation {
  item_key: string;
  item: unknown;
  /** pending | triggered | baseline | failed */
  outcome: string;
  run_id: string | null;
  attempts: number;
  last_error: string | null;
  observed_at: string;
  triggered_at: string | null;
}

export interface Asset {
  uri: string;
  name: string;
  first_seen_at: string;
  last_declared_at: string;
  last_materialized_at: string | null;
  last_materialized_run_id: string | null;
  producers: number;
  consumers: number;
}

export interface AssetTask {
  workflow: string;
  node_id: string;
  last_run_id: string | null;
  last_recorded_at: string | null;
  runs: number;
}

export interface AssetMaterialization {
  run_id: string;
  node_id: string;
  workflow: string;
  materialized_at: string;
  run_state: string;
}

export interface AssetDetail extends Asset {
  producer_tasks: AssetTask[];
  consumer_tasks: AssetTask[];
  materializations: AssetMaterialization[];
  latest_materialization: AssetMaterialization | null;
}

export interface AssetGraphNode {
  id: string;
  kind: 'asset' | 'task';
  label: string;
  uri?: string;
  workflow?: string;
  node_id?: string;
}

export interface AssetGraph {
  nodes: AssetGraphNode[];
  edges: { from: string; to: string }[];
  truncated: boolean;
}
