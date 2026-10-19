// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Infrastructure: platform policy, the demand preview, the cluster registry,
 * and the live inventory of control-plane instances and task runners.
 *
 * Everything the Cluster & scaling page reads goes through here, typed, so the
 * page holds no raw paths and no inline shapes. Two rules shape the module:
 *
 *  1. Responses are NORMALISED at the edge. A gateway older than a field (the
 *     plan's `controllers`, a cluster's `scheduler_autoscaling`) omits it, and
 *     the console must render "not reported" for that rather than crash on an
 *     undefined three components deep. Optional fields stay optional in the
 *     types so a caller is forced to say what it shows when they are absent.
 *  2. A route this control plane does not serve (404/501) is an ANSWER for the
 *     two live-inventory reads, not an error: an older gateway does not report
 *     component instances or task placement, and the page says "upgrade" rather
 *     than painting an error wall. That is `RouteResult`, below.
 *
 * Every call uses authRequest, like the other admin surfaces: these routes are
 * guarded by infrastructure.* permissions that a session carries.
 */

import { ApiError } from './client';
import { authApi, authRequest, type ScopedToken } from './auth';

// ── platform policy (GET/PUT /platform/config) ─────────────────────────────

export interface SchedulerPolicy {
  min_replicas: number;
  max_replicas: number;
  target_cpu_percent: number;
  scale_down_seconds: number;
  /** ADR-0017. Absent on a policy saved before the field existed: means 1. */
  fair_share_weight?: number;
}

export type TolerationEffect = 'NoSchedule' | 'PreferNoSchedule' | 'NoExecute';
export const TOLERATION_EFFECTS: readonly TolerationEffect[] = ['NoSchedule', 'PreferNoSchedule', 'NoExecute'];

export interface Toleration {
  key: string;
  value: string;
  effect: TolerationEffect | string;
}

export type ProfileCloud = 'aws' | 'azure' | 'kubernetes';

export interface PodProfile {
  name: string;
  cloud: ProfileCloud | string;
  kubernetes_namespace: string;
  service_account: string;
  image: string;
  cpu_millis: number;
  memory_mib: number;
  gpu_count: number;
  ephemeral_storage_mib: number;
  node_selector: Record<string, string>;
  tolerations: Toleration[];
  deadline_seconds: number;
  grace_seconds: number;
}

export interface WorkerPool {
  name: string;
  queue: string;
  profile: string;
  min_workers: number;
  max_workers: number;
  tasks_per_worker: number;
  idle_seconds: number;
  max_scale_up_step: number;
  enabled: boolean;
}

export interface PlatformConfig {
  revision: number;
  scheduler: SchedulerPolicy;
  pools: WorkerPool[];
  profiles: PodProfile[];
  /**
   * Namespace quotas ride in this document but have their own route and
   * permission (ADR-0021). Carried through untouched for display; never sent
   * back on a save (see saveConfig).
   */
  quotas?: unknown;
}

// ── demand preview (GET /platform/plan) ─────────────────────────────────────

export interface PlanDemand {
  pending_tasks: number;
  running_tasks: number;
  cpu_millis: number;
  memory_mib: number;
  gpu_count: number;
  current_workers: number;
  idle_seconds: number;
  unschedulable_tasks: number;
}

export interface PlanDecision {
  pool: string;
  current: number;
  desired: number;
  reason: string;
  limited: boolean;
  demand: PlanDemand;
}

/** One registered cluster as the plan endpoint summarises it. */
export interface PlanController {
  cluster: string;
  registry_mode: string;
  reporting: boolean;
  effective_mode?: string;
  last_report_at?: string | null;
}

export interface PlatformPlan {
  revision: number;
  mode: string;
  /** The server's own statement of whether anything acts on this plan. */
  message: string;
  decisions: PlanDecision[];
  task_job_templates: Record<string, unknown>;
  /** Absent on a gateway older than the cluster registry: normalised to []. */
  controllers: PlanController[];
}

// ── cluster registry (GET/PUT/DELETE /platform/clusters) ────────────────────

export interface ClusterBinding {
  kubernetes_namespace: string;
  worker_deployment: string;
  pool: string;
  max_replicas: number;
  max_scale_down_step: number;
  scale_up_cooldown_seconds: number;
  scale_down_cooldown_seconds: number;
}

export interface ClusterStatus {
  observed_revision: number;
  observed_cluster_uid: string;
  identity_verified: boolean;
  kubernetes_version: string;
  controller_identity: string;
  effective_mode: string;
  effective_mode_reason: string;
  last_report_at: string | null;
  last_error: string;
  last_error_at: string | null;
}

export interface ClusterObservation {
  kind: string;
  namespace: string;
  name: string;
  generation: number;
  observed_generation: number;
  spec_replicas: number | null;
  ready_replicas: number | null;
  ready: boolean;
  summary: Record<string, unknown>;
  created_at: string | null;
  observed_at: string;
}

export interface ClusterScalingState {
  kubernetes_namespace: string;
  worker_deployment: string;
  last_scale_at: string | null;
  last_scale_direction: string;
  idle_since: string | null;
  observed_platform_revision: number;
  last_decision: { reason?: string; holds?: string[]; decision?: { reason?: string } } | null;
  updated_at: string;
}

export interface ClusterScalingEvent {
  id: number;
  kubernetes_namespace: string;
  worker_deployment: string;
  action: string;
  mode: string;
  applied: boolean;
  from_replicas: number | null;
  to_replicas: number | null;
  reason: string;
  actor: string;
  occurred_at: string;
  /**
   * Structured context; for scheduler HPA rows (worker_deployment "hpa/<name>",
   * actions hpa_plan / hpa_apply / hpa_refused) the settings before and after.
   */
  detail?: Record<string, unknown>;
}

export interface ClusterReconcileError {
  target: string;
  message: string;
  actor: string;
  occurred_at: string;
}

/** Scheduler HPA reconciliation states (C5). */
export type AutoscalingState = 'in_sync' | 'drift' | 'plan_only' | 'applying' | 'not_observed';

export interface AutoscalingDesired {
  min_replicas: number;
  max_replicas: number;
  target_cpu_percent: number;
  scale_down_stabilization_seconds: number;
  config_revision: number;
}

export interface AutoscalingObserved {
  min_replicas: number | null;
  max_replicas: number | null;
  target_cpu_percent: number | null;
  scale_down_stabilization_seconds: number | null;
  current_replicas: number | null;
  desired_replicas: number | null;
  hpa_name: string;
  deployment: string;
}

/** One scheduler HPA the controller reconciles (one per autoscaled scheduler). */
export interface AutoscalingTarget {
  observed: AutoscalingObserved | null;
  state: AutoscalingState | (string & {});
  /**
   * false: the HPA lacks the nexus.khuriwalgroup.com/autoscaling-owner=
   * cluster-controller annotation, so the controller will not change it and
   * reports any difference as drift. null: not reported.
   */
  managed: boolean | null;
  /** What the controller did or would do, e.g. "set min 1→2, max 4→6; …". */
  reason: string;
  last_applied_at: string | null;
  last_error: string | null;
}

export interface SchedulerAutoscaling {
  desired: AutoscalingDesired | null;
  /** The top-level fields describe the WORST target; `targets` has each one. */
  observed: AutoscalingObserved | null;
  state: AutoscalingState | (string & {});
  last_applied_at: string | null;
  last_error: string | null;
  targets: AutoscalingTarget[];
}

/** Remaining quota in one task namespace; -1 means not constrained. */
export interface HeadroomEntry {
  namespace: string;
  cpu_millis: number | null;
  memory_bytes: number | null;
  gpu_count: number | null;
  pods: number | null;
  extended: Record<string, number>;
}

export type ClusterMode = 'plan' | 'apply';

export interface ClusterView {
  id: string;
  name: string;
  provider: string;
  provider_resource_id: string;
  expected_cluster_uid: string;
  control_plane_namespace: string;
  mode: ClusterMode;
  desired_revision: number;
  bindings: ClusterBinding[];
  updated_at: string;
  updated_by: string;
  status: ClusterStatus | null;
  observations: ClusterObservation[];
  scaling: ClusterScalingState[];
  events: ClusterScalingEvent[];
  errors: ClusterReconcileError[];
  jobs_shown: number;
  /** C5. Undefined on an older gateway: "not reported", never "none". */
  labels?: Record<string, string>;
  /** Keyed by task namespace (see headroomEntries). */
  headroom?: Record<string, unknown>;
  headroom_observed_at?: string | null;
  scheduler_autoscaling?: SchedulerAutoscaling | null;
}

/** The writable part of a cluster, exactly what PUT accepts (unknown fields are refused). */
export interface ClusterSpec {
  provider: string;
  provider_resource_id: string;
  expected_cluster_uid: string;
  control_plane_namespace: string;
  mode: ClusterMode;
  /** 0 registers a new cluster; otherwise the revision the operator loaded. */
  desired_revision: number;
  bindings: ClusterBinding[];
}

export interface ClusterList {
  clusters: ClusterView[];
  observed_at?: string;
}

// ── live inventory (C3) ─────────────────────────────────────────────────────

/** The fixed order the gateway reports control-plane components in. */
export const CONTROL_PLANE_COMPONENTS = [
  'api-gateway',
  'workflow-manager',
  'dag-engine',
  'scheduler',
  'resource-manager',
  'event-service',
] as const;
export type ControlPlaneComponent = (typeof CONTROL_PLANE_COMPONENTS)[number];

export type InstanceState = 'ready' | 'not_ready' | 'stale';

export interface ComponentInstance {
  instance: string;
  pod_namespace: string;
  node: string;
  cluster: string;
  version: string;
  started_at: string;
  last_seen_at: string;
  state: InstanceState | (string & {});
  /** Free-form and component-specific (queue for a scheduler, and so on). */
  detail: Record<string, unknown>;
}

export interface ComponentGroup {
  component: ControlPlaneComponent | (string & {});
  summary: { ready: number; total: number; stale: number };
  instances: ComponentInstance[];
}

export interface ComponentsReport {
  generated_at: string;
  stale_after_seconds: number;
  /**
   * Whether pod namespace, node, cluster and the full detail are shown to this
   * caller. When false (anyone but a platform operator, unless the gateway runs
   * with NEXUS_COMPONENT_TOPOLOGY=infrastructure-viewers) instances carry an
   * opaque name, those fields are "", and detail keeps only readiness,
   * shutting_down and heartbeat_interval_seconds.
   */
  topology_visible: boolean;
  components: ComponentGroup[];
}

export type RunnerExecutor = 'kubernetes' | 'docker' | 'subprocess' | '';

export interface TaskRunner {
  attempt_id: string;
  run_id: string;
  node_id: string;
  workflow: string;
  worker_id: string;
  worker_host: string;
  /** '' when the agent did not report which executor ran it. */
  executor: RunnerExecutor | (string & {});
  /** Placement fields are '' when not reported (null timestamps too). Never read '' as a value. */
  cluster: string;
  k8s_namespace: string;
  pod: string;
  k8s_node: string;
  /** DISPATCHED or RUNNING. */
  state: string;
  /** '' while DISPATCHED: the worker has not confirmed the start yet. */
  started_at: string;
  progress_message: string;
  placement_reported_at: string;
}

export interface TaskRunnersReport {
  generated_at: string;
  /** `running` counts every in-flight attempt: DISPATCHED and RUNNING. */
  summary: { running: number; by_executor: Record<string, number>; by_cluster: Record<string, number> };
  runners: TaskRunner[];
  /** The limit this report was requested with, so a count can say "at least". */
  limit: number;
}

export const TASK_RUNNER_DEFAULT_LIMIT = 200;
export const TASK_RUNNER_MAX_LIMIT = 1000;

/**
 * A read against a route an older control plane may not serve.
 *
 * `unsupported` is a normal outcome, not a failure: the page renders "this
 * control plane does not report X (upgrade)" for it. Any other error still
 * throws and is shown as an error.
 */
export type RouteResult<T> = { kind: 'ok'; data: T } | { kind: 'unsupported'; status: number };

// ── normalisation ───────────────────────────────────────────────────────────

const isObj = (v: unknown): v is Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v);
const str = (v: unknown): string => (typeof v === 'string' ? v : '');
const num = (v: unknown, fallback = 0): number => (typeof v === 'number' && Number.isFinite(v) ? v : fallback);
const numOrNull = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) ? v : null);
const strOrNull = (v: unknown): string | null => (typeof v === 'string' && v !== '' ? v : null);
const arr = <T>(v: unknown): T[] => (Array.isArray(v) ? (v as T[]) : []);
const strMap = (v: unknown): Record<string, string> => {
  const out: Record<string, string> = {};
  if (isObj(v)) for (const [k, x] of Object.entries(v)) out[k] = typeof x === 'string' ? x : String(x);
  return out;
};

export function normalizeConfig(raw: unknown): PlatformConfig {
  const c = isObj(raw) ? raw : {};
  const s = isObj(c.scheduler) ? c.scheduler : {};
  const scheduler: SchedulerPolicy = {
    min_replicas: num(s.min_replicas, 1),
    max_replicas: num(s.max_replicas, 4),
    target_cpu_percent: num(s.target_cpu_percent, 65),
    scale_down_seconds: num(s.scale_down_seconds, 300),
  };
  if (typeof s.fair_share_weight === 'number') scheduler.fair_share_weight = s.fair_share_weight;
  const out: PlatformConfig = {
    revision: num(c.revision),
    scheduler,
    pools: arr<Record<string, unknown>>(c.pools).map((p) => ({
      name: str(p.name),
      queue: str(p.queue),
      profile: str(p.profile),
      min_workers: num(p.min_workers),
      max_workers: num(p.max_workers),
      tasks_per_worker: num(p.tasks_per_worker, 1),
      idle_seconds: num(p.idle_seconds, 300),
      max_scale_up_step: num(p.max_scale_up_step, 1),
      enabled: p.enabled === true,
    })),
    profiles: arr<Record<string, unknown>>(c.profiles).map((p) => ({
      name: str(p.name),
      cloud: str(p.cloud) || 'kubernetes',
      kubernetes_namespace: str(p.kubernetes_namespace),
      service_account: str(p.service_account),
      image: str(p.image),
      cpu_millis: num(p.cpu_millis),
      memory_mib: num(p.memory_mib),
      gpu_count: num(p.gpu_count),
      ephemeral_storage_mib: num(p.ephemeral_storage_mib),
      node_selector: strMap(p.node_selector),
      tolerations: arr<Record<string, unknown>>(p.tolerations).map((t) => ({
        key: str(t.key),
        value: str(t.value),
        effect: str(t.effect),
      })),
      deadline_seconds: num(p.deadline_seconds),
      grace_seconds: num(p.grace_seconds),
    })),
  };
  if (c.quotas !== undefined) out.quotas = c.quotas;
  return out;
}

export function normalizePlan(raw: unknown): PlatformPlan {
  const p = isObj(raw) ? raw : {};
  return {
    revision: num(p.revision),
    mode: str(p.mode) || 'preview',
    message: str(p.message),
    decisions: arr<Record<string, unknown>>(p.decisions).map((d) => {
      const demand = isObj(d.demand) ? d.demand : {};
      return {
        pool: str(d.pool),
        current: num(d.current),
        desired: num(d.desired),
        reason: str(d.reason),
        limited: d.limited === true,
        demand: {
          pending_tasks: num(demand.pending_tasks),
          running_tasks: num(demand.running_tasks),
          cpu_millis: num(demand.cpu_millis),
          memory_mib: num(demand.memory_mib),
          gpu_count: num(demand.gpu_count),
          current_workers: num(demand.current_workers),
          idle_seconds: num(demand.idle_seconds),
          unschedulable_tasks: num(demand.unschedulable_tasks),
        },
      };
    }),
    task_job_templates: isObj(p.task_job_templates) ? p.task_job_templates : {},
    controllers: arr<Record<string, unknown>>(p.controllers).map((c) => ({
      cluster: str(c.cluster),
      registry_mode: str(c.registry_mode),
      reporting: c.reporting === true,
      ...(typeof c.effective_mode === 'string' ? { effective_mode: c.effective_mode } : {}),
      last_report_at: strOrNull(c.last_report_at),
    })),
  };
}

function normalizeObserved(raw: unknown): AutoscalingObserved | null {
  if (!isObj(raw)) return null;
  return {
    min_replicas: numOrNull(raw.min_replicas),
    max_replicas: numOrNull(raw.max_replicas),
    target_cpu_percent: numOrNull(raw.target_cpu_percent),
    scale_down_stabilization_seconds: numOrNull(raw.scale_down_stabilization_seconds),
    current_replicas: numOrNull(raw.current_replicas),
    desired_replicas: numOrNull(raw.desired_replicas),
    hpa_name: str(raw.hpa_name),
    deployment: str(raw.deployment),
  };
}

function normalizeAutoscaling(raw: unknown): SchedulerAutoscaling | null {
  if (!isObj(raw)) return null;
  const d = isObj(raw.desired) ? raw.desired : null;
  return {
    desired: d
      ? {
          min_replicas: num(d.min_replicas),
          max_replicas: num(d.max_replicas),
          target_cpu_percent: num(d.target_cpu_percent),
          scale_down_stabilization_seconds: num(d.scale_down_stabilization_seconds),
          config_revision: num(d.config_revision),
        }
      : null,
    observed: normalizeObserved(raw.observed),
    state: str(raw.state) || 'not_observed',
    last_applied_at: strOrNull(raw.last_applied_at),
    last_error: strOrNull(raw.last_error),
    targets: arr<unknown>(raw.targets)
      .filter(isObj)
      .map((t) => ({
        observed: normalizeObserved(t.observed),
        state: str(t.state) || 'not_observed',
        managed: typeof t.managed === 'boolean' ? t.managed : null,
        reason: str(t.reason),
        last_applied_at: strOrNull(t.last_applied_at),
        last_error: strOrNull(t.last_error),
      })),
  };
}

/** A cluster's quota headroom as rows, one per task namespace; unparseable values are null. */
export function headroomEntries(headroom: Record<string, unknown> | undefined): HeadroomEntry[] {
  return Object.entries(headroom ?? {})
    .filter((e): e is [string, Record<string, unknown>] => isObj(e[1]))
    .map(([namespace, h]) => ({
      namespace,
      cpu_millis: numOrNull(h.cpu_millis),
      memory_bytes: numOrNull(h.memory_bytes),
      gpu_count: numOrNull(h.gpu_count),
      pods: numOrNull(h.pods),
      extended: Object.fromEntries(Object.entries(isObj(h.extended) ? h.extended : {}).filter((x): x is [string, number] => typeof x[1] === 'number')),
    }))
    .sort((a, b) => a.namespace.localeCompare(b.namespace));
}

export function normalizeCluster(raw: unknown): ClusterView {
  const c = isObj(raw) ? raw : {};
  const s = isObj(c.status) ? c.status : null;
  const view: ClusterView = {
    id: str(c.id),
    name: str(c.name),
    provider: str(c.provider),
    provider_resource_id: str(c.provider_resource_id),
    expected_cluster_uid: str(c.expected_cluster_uid),
    control_plane_namespace: str(c.control_plane_namespace),
    mode: c.mode === 'apply' ? 'apply' : 'plan',
    desired_revision: num(c.desired_revision),
    bindings: arr<ClusterBinding>(c.bindings),
    updated_at: str(c.updated_at),
    updated_by: str(c.updated_by),
    status: s
      ? {
          observed_revision: num(s.observed_revision),
          observed_cluster_uid: str(s.observed_cluster_uid),
          identity_verified: s.identity_verified === true,
          kubernetes_version: str(s.kubernetes_version),
          controller_identity: str(s.controller_identity),
          effective_mode: str(s.effective_mode),
          effective_mode_reason: str(s.effective_mode_reason),
          last_report_at: strOrNull(s.last_report_at),
          last_error: str(s.last_error),
          last_error_at: strOrNull(s.last_error_at),
        }
      : null,
    observations: arr<Record<string, unknown>>(c.observations).map((o) => ({
      kind: str(o.kind),
      namespace: str(o.namespace),
      name: str(o.name),
      generation: num(o.generation),
      observed_generation: num(o.observed_generation),
      spec_replicas: numOrNull(o.spec_replicas),
      ready_replicas: numOrNull(o.ready_replicas),
      ready: o.ready === true,
      summary: isObj(o.summary) ? o.summary : {},
      created_at: strOrNull(o.created_at),
      observed_at: str(o.observed_at),
    })),
    scaling: arr<ClusterScalingState>(c.scaling),
    events: arr<Record<string, unknown>>(c.events).map((e) => ({
      ...(e as unknown as ClusterScalingEvent),
      ...(isObj(e.detail) ? { detail: e.detail } : { detail: undefined }),
    })),
    errors: arr<ClusterReconcileError>(c.errors),
    jobs_shown: num(c.jobs_shown),
  };
  // Only carried when the server sent them: absent is "this control plane does
  // not report it", which the UI must be able to tell apart from "empty".
  if (isObj(c.labels)) view.labels = strMap(c.labels);
  if (isObj(c.headroom)) view.headroom = c.headroom;
  if ('headroom_observed_at' in c) view.headroom_observed_at = strOrNull(c.headroom_observed_at);
  if ('scheduler_autoscaling' in c) view.scheduler_autoscaling = normalizeAutoscaling(c.scheduler_autoscaling);
  return view;
}

export function normalizeComponents(raw: unknown): ComponentsReport {
  const r = isObj(raw) ? raw : {};
  return {
    generated_at: str(r.generated_at),
    stale_after_seconds: num(r.stale_after_seconds, 30),
    // A build that predates the field always showed the topology.
    topology_visible: r.topology_visible !== false,
    components: arr<Record<string, unknown>>(r.components).map((g) => {
      const s = isObj(g.summary) ? g.summary : {};
      return {
        component: str(g.component),
        summary: { ready: num(s.ready), total: num(s.total), stale: num(s.stale) },
        instances: arr<Record<string, unknown>>(g.instances).map((i) => ({
          instance: str(i.instance),
          pod_namespace: str(i.pod_namespace),
          node: str(i.node),
          cluster: str(i.cluster),
          version: str(i.version),
          started_at: str(i.started_at),
          last_seen_at: str(i.last_seen_at),
          state: str(i.state),
          detail: isObj(i.detail) ? i.detail : {},
        })),
      };
    }),
  };
}

export function normalizeTaskRunners(raw: unknown, limit: number): TaskRunnersReport {
  const r = isObj(raw) ? raw : {};
  const s = isObj(r.summary) ? r.summary : {};
  const counts = (v: unknown): Record<string, number> => {
    const out: Record<string, number> = {};
    if (isObj(v)) for (const [k, n] of Object.entries(v)) if (typeof n === 'number') out[k] = n;
    return out;
  };
  const runners = arr<Record<string, unknown>>(r.runners).map((x) => ({
    attempt_id: str(x.attempt_id),
    run_id: str(x.run_id),
    node_id: str(x.node_id),
    workflow: str(x.workflow),
    worker_id: str(x.worker_id),
    worker_host: str(x.worker_host),
    executor: str(x.executor),
    cluster: str(x.cluster),
    k8s_namespace: str(x.k8s_namespace),
    pod: str(x.pod),
    k8s_node: str(x.k8s_node),
    state: str(x.state),
    started_at: str(x.started_at),
    progress_message: str(x.progress_message),
    placement_reported_at: str(x.placement_reported_at),
  }));
  return {
    generated_at: str(r.generated_at),
    summary: {
      running: num(s.running, runners.length),
      by_executor: counts(s.by_executor),
      by_cluster: counts(s.by_cluster),
    },
    runners,
    limit,
  };
}

// ── client ──────────────────────────────────────────────────────────────────

const root = (namespace: string) => `/v1/namespaces/${encodeURIComponent(namespace)}/platform`;
const json = (method: string, body: unknown): RequestInit => ({
  method,
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
});

/** 404/501: the route does not exist on this control plane. */
export function isUnsupportedRoute(error: unknown): error is ApiError {
  return error instanceof ApiError && error.isNotImplemented;
}

async function optionalRoute<T>(path: string, normalize: (raw: unknown) => T): Promise<RouteResult<T>> {
  try {
    return { kind: 'ok', data: normalize(await authRequest<unknown>(path)) };
  } catch (e) {
    if (isUnsupportedRoute(e)) return { kind: 'unsupported', status: e.status };
    throw e;
  }
}

/** Exactly the writable fields: PUT refuses unknown fields (DisallowUnknownFields). */
export function clusterSpecOf(c: Pick<ClusterView, keyof ClusterSpec>): ClusterSpec {
  return {
    provider: c.provider,
    provider_resource_id: c.provider_resource_id,
    expected_cluster_uid: c.expected_cluster_uid,
    control_plane_namespace: c.control_plane_namespace,
    mode: c.mode,
    desired_revision: c.desired_revision,
    bindings: c.bindings.map((b) => ({
      kubernetes_namespace: b.kubernetes_namespace,
      worker_deployment: b.worker_deployment,
      pool: b.pool,
      max_replicas: b.max_replicas,
      max_scale_down_step: b.max_scale_down_step,
      scale_up_cooldown_seconds: b.scale_up_cooldown_seconds,
      scale_down_cooldown_seconds: b.scale_down_cooldown_seconds,
    })),
  };
}

export const platformApi = {
  async getConfig(namespace: string): Promise<PlatformConfig> {
    return normalizeConfig(await authRequest<unknown>(`${root(namespace)}/config`));
  },

  /**
   * Saves the policy against the revision it was loaded at (409 if someone
   * saved in between). Quotas are left OUT of the body on purpose: a document
   * without them keeps the stored ones, so this page can never edit or clobber
   * a quota (they have their own route and permission, ADR-0021).
   */
  async saveConfig(namespace: string, config: PlatformConfig): Promise<PlatformConfig> {
    const { revision, scheduler, pools, profiles } = config;
    const saved = await authRequest<unknown>(
      `${root(namespace)}/config`,
      json('PUT', { revision, scheduler, pools, profiles }),
    );
    return normalizeConfig(saved);
  },

  async getPlan(namespace: string): Promise<PlatformPlan> {
    return normalizePlan(await authRequest<unknown>(`${root(namespace)}/plan`));
  },

  async listClusters(namespace: string): Promise<ClusterList> {
    const raw = await authRequest<unknown>(`${root(namespace)}/clusters`);
    const r = isObj(raw) ? raw : {};
    const list: ClusterList = { clusters: arr<unknown>(r.clusters).map(normalizeCluster) };
    if (typeof r.observed_at === 'string') list.observed_at = r.observed_at;
    return list;
  },

  saveCluster(namespace: string, name: string, spec: ClusterSpec): Promise<ClusterSpec & { name: string }> {
    return authRequest(`${root(namespace)}/clusters/${encodeURIComponent(name)}`, json('PUT', spec));
  },

  deleteCluster(namespace: string, name: string, revision: number): Promise<void> {
    return authRequest(
      `${root(namespace)}/clusters/${encodeURIComponent(name)}?revision=${encodeURIComponent(String(revision))}`,
      { method: 'DELETE' },
    );
  },

  getComponents(namespace: string): Promise<RouteResult<ComponentsReport>> {
    return optionalRoute(`${root(namespace)}/components`, normalizeComponents);
  },

  listTaskRunners(namespace: string, limit = TASK_RUNNER_DEFAULT_LIMIT): Promise<RouteResult<TaskRunnersReport>> {
    const n = Math.max(1, Math.min(TASK_RUNNER_MAX_LIMIT, Math.floor(limit)));
    return optionalRoute(`${root(namespace)}/task-runners?limit=${n}`, (raw) => normalizeTaskRunners(raw, n));
  },

  /**
   * A worker credential for agents: role worker, scope worker_rpc and nothing
   * else. The secret is in the response ONCE and never again.
   */
  mintWorkerToken(namespace: string, input: { name: string; expires_in_days: number }): Promise<ScopedToken> {
    return authApi.createToken(namespace, {
      name: input.name,
      role: 'worker',
      scopes: ['worker_rpc'],
      expires_in_days: input.expires_in_days,
    });
  },
};
