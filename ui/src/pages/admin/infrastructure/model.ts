// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The Cluster & scaling page's logic, kept out of its components so it can be
 * tested without rendering anything.
 *
 *  - VALIDATION mirrors internal/platform/config.go Validate() and the cluster
 *    registry's checks, field by field, so an operator sees "maximum must be at
 *    least the minimum" beside the field instead of the server's one-line 400.
 *    The server stays the authority; this only says it earlier and in place.
 *    (The page this replaces found invalid fields by scanning the DOM for
 *    :invalid inputs, which broke whenever a section was not rendered.)
 *  - HEALTH turns raw reports into a level, a sentence, and what to do about
 *    it. A level is never only a colour: every badge carries its label.
 *  - "Not reported" is a value. Nothing here turns an absent report into 0.
 */

import type {
  AutoscalingState,
  ClusterStatus,
  ClusterView,
  ComponentGroup,
  ComponentsReport,
  InstanceState,
  PlatformConfig,
  PodProfile,
  RouteResult,
  SchedulerPolicy,
  TaskRunner,
  TaskRunnersReport,
  WorkerPool,
} from '../../../api/platform';
import { TOLERATION_EFFECTS } from '../../../api/platform';
import type { CapacitySummary, Worker } from '../../../api/types';
import type { Tone } from '../../../lib/states';
import { parseTs } from '../../../lib/format';
import { heartbeatStale } from '../../WorkersPage';

// ── validation ──────────────────────────────────────────────────────────────

/** Field path → message. Paths: `scheduler.min_replicas`, `pools.0.queue`, `profiles.1.tolerations.0.key`. */
export type FieldErrors = Record<string, string>;

/**
 * The editable form of the policy. Identical to the wire document except that
 * a profile's node selector is a list of rows rather than a map: a map cannot
 * hold the half-typed states an editor passes through (an empty key, two rows
 * briefly sharing one), and a row that vanished while the operator typed into
 * it would be a bug. `fromDraft` turns it back into the wire shape.
 */
export interface SelectorRow {
  key: string;
  value: string;
}
export interface ProfileDraft extends Omit<PodProfile, 'node_selector'> {
  node_selector: SelectorRow[];
}
export interface ConfigDraft extends Omit<PlatformConfig, 'profiles'> {
  profiles: ProfileDraft[];
}

export function toDraft(c: PlatformConfig): ConfigDraft {
  return {
    ...c,
    profiles: c.profiles.map((p) => ({
      ...p,
      node_selector: Object.entries(p.node_selector).map(([key, value]) => ({ key, value })),
    })),
  };
}

/** Blank selector rows (no key, no value) are dropped: they are an unfinished add, not data. */
export function fromDraft(d: ConfigDraft): PlatformConfig {
  return {
    ...d,
    profiles: d.profiles.map((p) => ({
      ...p,
      node_selector: Object.fromEntries(
        p.node_selector.filter((r) => r.key !== '' || r.value !== '').map((r) => [r.key, r.value]),
      ),
    })),
  };
}

const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const DNS_SUBDOMAIN = /^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$/;
const DIGEST = /@sha256:[a-f0-9]{64}$/;

export const isDnsLabel = (s: string) => s.length > 0 && s.length <= 63 && DNS_LABEL.test(s);
export const isDnsSubdomain = (s: string) => s.length > 0 && s.length <= 253 && DNS_SUBDOMAIN.test(s);

const DNS_LABEL_MSG = 'Use 1–63 lowercase letters, digits or hyphens, starting and ending with a letter or digit.';

/** A whole number in [min, max], or the reason it is not. */
export function intError(v: number, min: number, max: number): string | null {
  if (typeof v !== 'number' || Number.isNaN(v)) return 'Enter a number.';
  if (!Number.isInteger(v)) return 'Enter a whole number.';
  if (v < min || v > max) return `Must be between ${min} and ${max}.`;
  return null;
}

function put(errors: FieldErrors, path: string, message: string | null) {
  if (message && !(path in errors)) errors[path] = message;
}

export const SCHEDULER_LIMITS = {
  replicas: [1, 100],
  targetCpu: [10, 90],
  scaleDown: [60, 3600],
  weight: [1, 100],
} as const;

export function validateScheduler(s: SchedulerPolicy): FieldErrors {
  const e: FieldErrors = {};
  const [rMin, rMax] = SCHEDULER_LIMITS.replicas;
  put(e, 'scheduler.min_replicas', intError(s.min_replicas, rMin, rMax));
  put(e, 'scheduler.max_replicas', intError(s.max_replicas, rMin, rMax));
  if (!e['scheduler.min_replicas'] && !e['scheduler.max_replicas'] && s.max_replicas < s.min_replicas) {
    e['scheduler.max_replicas'] = `Must be at least the minimum (${s.min_replicas}).`;
  }
  put(e, 'scheduler.target_cpu_percent', intError(s.target_cpu_percent, ...SCHEDULER_LIMITS.targetCpu));
  put(e, 'scheduler.scale_down_seconds', intError(s.scale_down_seconds, ...SCHEDULER_LIMITS.scaleDown));
  put(e, 'scheduler.fair_share_weight', intError(s.fair_share_weight ?? 1, ...SCHEDULER_LIMITS.weight));
  return e;
}

export function validateProfiles(profiles: ProfileDraft[]): FieldErrors {
  const e: FieldErrors = {};
  const seen = new Map<string, number>();
  profiles.forEach((p, i) => {
    const at = (f: string) => `profiles.${i}.${f}`;
    if (!isDnsLabel(p.name)) put(e, at('name'), DNS_LABEL_MSG);
    else if (seen.has(p.name)) put(e, at('name'), `Profile ${seen.get(p.name)! + 1} already uses this name.`);
    else seen.set(p.name, i);
    if (!['aws', 'azure', 'kubernetes'].includes(p.cloud)) put(e, at('cloud'), 'Choose AWS EKS, Azure AKS or other Kubernetes.');
    if (!isDnsLabel(p.kubernetes_namespace)) put(e, at('kubernetes_namespace'), DNS_LABEL_MSG);
    if (!isDnsLabel(p.service_account)) put(e, at('service_account'), DNS_LABEL_MSG);
    if (p.image.length > 512 || !DIGEST.test(p.image)) {
      put(e, at('image'), 'Pin the image by digest: registry/name@sha256:<64 lowercase hex characters>.');
    }
    put(e, at('cpu_millis'), intError(p.cpu_millis, 100, 1_000_000));
    put(e, at('memory_mib'), intError(p.memory_mib, 128, 4_194_304));
    put(e, at('gpu_count'), intError(p.gpu_count, 0, 128));
    put(e, at('ephemeral_storage_mib'), intError(p.ephemeral_storage_mib, 128, 4_194_304));
    put(e, at('deadline_seconds'), intError(p.deadline_seconds, 30, 604_800));
    put(e, at('grace_seconds'), intError(p.grace_seconds, 1, 600));
    const keys = new Set<string>();
    let rows = 0;
    p.node_selector.forEach(({ key, value }, j) => {
      if (key === '' && value === '') return;
      rows++;
      if (key.trim() === '' || key.length > 253) put(e, at(`node_selector.${j}.key`), 'Enter a label key (up to 253 characters).');
      else if (keys.has(key)) put(e, at(`node_selector.${j}.key`), 'This key is already set above.');
      keys.add(key);
      if (value.length > 63) put(e, at(`node_selector.${j}.value`), 'Label values are at most 63 characters.');
    });
    if (rows > 32) put(e, at('node_selector'), 'At most 32 node selector entries.');
    if (p.tolerations.length > 32) put(e, at('tolerations'), 'At most 32 tolerations.');
    p.tolerations.forEach((t, j) => {
      if (t.key.trim() === '' || t.key.length > 253) put(e, at(`tolerations.${j}.key`), 'Enter the taint key (up to 253 characters).');
      if (t.value.length > 63) put(e, at(`tolerations.${j}.value`), 'Taint values are at most 63 characters.');
      if (!(TOLERATION_EFFECTS as readonly string[]).includes(t.effect)) {
        put(e, at(`tolerations.${j}.effect`), 'Choose NoSchedule, PreferNoSchedule or NoExecute.');
      }
    });
  });
  if (profiles.length > 100) put(e, 'profiles', 'At most 100 pod profiles per namespace.');
  return e;
}

export function validatePools(pools: WorkerPool[], profiles: Pick<PodProfile, 'name'>[]): FieldErrors {
  const e: FieldErrors = {};
  const names = new Map<string, number>();
  const queues = new Map<string, number>();
  const profileNames = new Set(profiles.map((p) => p.name));
  pools.forEach((p, i) => {
    const at = (f: string) => `pools.${i}.${f}`;
    if (!isDnsLabel(p.name)) put(e, at('name'), DNS_LABEL_MSG);
    else if (names.has(p.name)) put(e, at('name'), `Pool ${names.get(p.name)! + 1} already uses this name.`);
    else names.set(p.name, i);
    if (!isDnsLabel(p.queue)) put(e, at('queue'), DNS_LABEL_MSG);
    else if (queues.has(p.queue)) put(e, at('queue'), `Pool ${queues.get(p.queue)! + 1} already owns this queue; each pool owns one queue.`);
    else queues.set(p.queue, i);
    if (!profileNames.has(p.profile)) {
      put(e, at('profile'), p.profile ? `Profile “${p.profile}” does not exist. Choose an existing pod profile.` : 'Choose a pod profile.');
    }
    put(e, at('min_workers'), intError(p.min_workers, 0, 10_000));
    put(e, at('max_workers'), intError(p.max_workers, 0, 10_000));
    if (!e[at('min_workers')] && !e[at('max_workers')] && p.max_workers < p.min_workers) {
      e[at('max_workers')] = `Must be at least the minimum (${p.min_workers}).`;
    }
    put(e, at('tasks_per_worker'), intError(p.tasks_per_worker, 1, 1000));
    put(e, at('idle_seconds'), intError(p.idle_seconds, 60, 86_400));
    put(e, at('max_scale_up_step'), intError(p.max_scale_up_step, 1, 100));
  });
  if (pools.length > 100) put(e, 'pools', 'At most 100 worker pools per namespace.');
  return e;
}

export function validateConfig(c: ConfigDraft): FieldErrors {
  return { ...validateScheduler(c.scheduler), ...validatePools(c.pools, c.profiles), ...validateProfiles(c.profiles) };
}

/** A stable DOM id for a field path, so an error summary can focus the field. */
export function fieldId(path: string): string {
  return `infra-${path.replace(/[^A-Za-z0-9]+/g, '-')}`;
}

export type ConfigSection = 'scheduler' | 'pools' | 'profiles';

export function sectionOf(path: string): ConfigSection {
  if (path.startsWith('scheduler')) return 'scheduler';
  return path.startsWith('profiles') ? 'profiles' : 'pools';
}

/** Which sections differ between the saved document and the draft. */
export function changedSections(base: PlatformConfig | null, draft: ConfigDraft | null): Set<ConfigSection> {
  const out = new Set<ConfigSection>();
  if (!base || !draft) return out;
  const wire = fromDraft(draft);
  // NaN (a cleared number field) serialises as null, so it still reads as a change.
  const eq = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
  const scheduler = (s: SchedulerPolicy) => ({ ...s, fair_share_weight: s.fair_share_weight ?? 1 });
  if (!eq(scheduler(base.scheduler), scheduler(wire.scheduler))) out.add('scheduler');
  if (!eq(base.pools, wire.pools)) out.add('pools');
  if (!eq(base.profiles, wire.profiles)) out.add('profiles');
  return out;
}

export const PROFILE_DEFAULTS: Omit<ProfileDraft, 'name'> = {
  cloud: 'azure',
  kubernetes_namespace: 'nexus-tasks',
  service_account: 'nexus-task',
  image: '',
  cpu_millis: 2000,
  memory_mib: 4096,
  gpu_count: 0,
  ephemeral_storage_mib: 1024,
  node_selector: [],
  tolerations: [],
  deadline_seconds: 3600,
  grace_seconds: 30,
};

export const POOL_DEFAULTS: Omit<WorkerPool, 'name' | 'queue' | 'profile'> = {
  min_workers: 0,
  max_workers: 10,
  tasks_per_worker: 1,
  idle_seconds: 300,
  max_scale_up_step: 2,
  enabled: false,
};

/** The first `${prefix}-N` not already taken. */
export function nextName(prefix: string, taken: Iterable<string>): string {
  const used = new Set(taken);
  for (let n = 1; ; n++) if (!used.has(`${prefix}-${n}`)) return `${prefix}-${n}`;
}

// ── health ──────────────────────────────────────────────────────────────────

export type HealthLevel = 'healthy' | 'idle' | 'degraded' | 'down' | 'stale' | 'not_reported' | 'loading' | 'unavailable';

export interface Health {
  level: HealthLevel;
  /** Short status text shown in the badge (never colour alone). */
  label: string;
  /** One sentence of what is true. */
  detail: string;
  /** What to do about it; present when the level is not healthy/idle/loading. */
  hint?: string;
}

export const HEALTH_VISUAL: Record<HealthLevel, { tone: Tone; glyph: string }> = {
  healthy: { tone: 'success', glyph: '✓' },
  idle: { tone: 'neutral', glyph: '○' },
  degraded: { tone: 'warning', glyph: '!' },
  down: { tone: 'danger', glyph: '✕' },
  stale: { tone: 'warning', glyph: '◌' },
  not_reported: { tone: 'muted', glyph: '?' },
  loading: { tone: 'neutral', glyph: '…' },
  unavailable: { tone: 'muted', glyph: '–' },
};

/** Worst first: what an aggregate takes when its parts disagree. */
const SEVERITY: HealthLevel[] = ['down', 'degraded', 'stale', 'not_reported', 'unavailable', 'loading', 'idle', 'healthy'];
export function worst(levels: HealthLevel[]): HealthLevel {
  for (const l of SEVERITY) if (levels.includes(l)) return l;
  return 'healthy';
}

export const INSTANCE_VISUAL: Record<InstanceState, { label: string; tone: Tone; glyph: string; hint: string }> = {
  ready: { label: 'Ready', tone: 'success', glyph: '●', hint: 'Reporting and ready to serve.' },
  not_ready: { label: 'Not ready', tone: 'danger', glyph: '✕', hint: 'Reporting, but failing its readiness check.' },
  stale: { label: 'Stale', tone: 'warning', glyph: '◌', hint: 'Has not reported within the freshness window; it may be gone.' },
};

export function instanceVisual(state: string) {
  return (
    INSTANCE_VISUAL[state as InstanceState] ?? {
      label: state ? state.toUpperCase() : 'UNKNOWN',
      tone: 'neutral' as Tone,
      glyph: '?',
      hint: 'The server reported a state this console does not recognise.',
    }
  );
}

export const AUTOSCALING_VISUAL: Record<AutoscalingState, { label: string; tone: Tone; glyph: string; hint: string }> = {
  in_sync: { label: 'In sync', tone: 'success', glyph: '✓', hint: 'The scheduler HPA matches the saved policy.' },
  drift: {
    label: 'Drift',
    tone: 'warning',
    glyph: '≠',
    hint: 'Apply mode: the controller reconciled this revision and the HPA still differs — the last error says why (often an HPA it does not own).',
  },
  plan_only: {
    label: 'Plan only',
    tone: 'queued',
    glyph: '◇',
    hint: 'The HPA differs from the saved policy and the controller runs in plan (dry-run) mode: it reports what it would change and changes nothing.',
  },
  applying: {
    label: 'Applying',
    tone: 'active',
    glyph: '↻',
    hint: 'Apply mode: the controller has not reconciled this revision yet, or is moving the HPA in bounded steps (replica bounds at most ×2 or ÷2 per patch, at most one patch a minute).',
  },
  not_observed: {
    label: 'Not observed',
    tone: 'neutral',
    glyph: '?',
    hint: 'No scheduler HPA has been observed in this cluster yet.',
  },
};

export function autoscalingVisual(state: string) {
  return (
    AUTOSCALING_VISUAL[state as AutoscalingState] ?? {
      label: state || 'Unknown',
      tone: 'neutral' as Tone,
      glyph: '?',
      hint: 'The server reported a state this console does not recognise.',
    }
  );
}

const AUTOSCALING_LEVEL: Record<AutoscalingState, HealthLevel> = {
  in_sync: 'healthy',
  applying: 'healthy',
  plan_only: 'idle',
  drift: 'degraded',
  not_observed: 'not_reported',
};

export const COMPONENT_LABEL: Record<string, string> = {
  'api-gateway': 'API gateway',
  'workflow-manager': 'Workflow manager',
  'dag-engine': 'DAG engine',
  scheduler: 'Scheduler',
  'resource-manager': 'Resource manager',
  'event-service': 'Event service',
};

/**
 * A component's instances split into the ones that matter now and history.
 *
 * A replaced pod's row stays for up to 24h as `stale`. While a ready
 * replacement exists those rows are history, not a fault; when nothing is
 * ready they are the evidence and stay in view.
 */
export function splitHistory<T extends { state: string }>(instances: T[]): { current: T[]; history: T[] } {
  const anyReady = instances.some((i) => i.state === 'ready');
  if (!anyReady) return { current: instances, history: [] };
  return { current: instances.filter((i) => i.state !== 'stale'), history: instances.filter((i) => i.state === 'stale') };
}

/** Instances still reporting (ready or not ready); stale rows are excluded. */
export const reporting = (g: ComponentGroup) => Math.max(0, g.summary.total - g.summary.stale);

/** Every reporting instance ready, and at least one of them. */
export const fullyReady = (g: ComponentGroup) => g.summary.ready > 0 && g.summary.ready >= reporting(g);

export function componentHealth(g: ComponentGroup): Health {
  const { ready, total, stale } = g.summary;
  const live = reporting(g);
  const name = COMPONENT_LABEL[g.component] ?? g.component;
  const previous = ready > 0 && stale > 0 ? ` · ${stale} previous instance${stale === 1 ? '' : 's'}` : '';
  if (total === 0) {
    return {
      level: 'not_reported',
      label: 'Not reported',
      detail: `No ${name} instance has reported.`,
      hint: `Check that ${name} pods are running (kubectl get pods) and can reach the database.`,
    };
  }
  if (ready === 0 && live === 0) {
    return {
      level: 'stale',
      label: 'Stale',
      detail: `0 ready; ${stale} instance${stale === 1 ? '' : 's'} stopped reporting.`,
      hint: `${name} instances stopped reporting. Check their pods, restarts and logs.`,
    };
  }
  if (ready === 0) {
    return { level: 'down', label: 'Down', detail: `0/${live} ready`, hint: `No ${name} instance is ready. Check its pods and logs.` };
  }
  if (ready < live) {
    return {
      level: 'degraded',
      label: 'Degraded',
      detail: `${ready}/${live} ready${previous}`,
      hint: `Some ${name} instances report not ready. Inspect them below.`,
    };
  }
  return { level: 'healthy', label: 'Healthy', detail: `${ready}/${live} ready${previous}` };
}

export const UPGRADE_HINT =
  'This control plane does not report component instances. Upgrade KGF Nexus to see live replicas here; until then, use kubectl get pods.';

export function controlPlaneHealth(result: RouteResult<ComponentsReport> | undefined, error: unknown): Health {
  if (result === undefined) {
    return error
      ? { level: 'unavailable', label: 'Unavailable', detail: 'Could not load component instances.', hint: 'See the error below; the rest of the page still works.' }
      : { level: 'loading', label: 'Loading', detail: 'Loading component instances…' };
  }
  if (result.kind === 'unsupported') {
    return { level: 'not_reported', label: 'Not reported', detail: 'Component instances are not reported by this control plane.', hint: UPGRADE_HINT };
  }
  const groups = result.data.components;
  if (!groups.length) return { level: 'not_reported', label: 'Not reported', detail: 'No component has reported.', hint: UPGRADE_HINT };
  const healths = groups.map(componentHealth);
  const level = worst(healths.map((h) => h.level));
  const ready = groups.filter(fullyReady).length;
  const bad = groups.filter((_, i) => healths[i]!.level !== 'healthy').map((g) => COMPONENT_LABEL[g.component] ?? g.component);
  return {
    level,
    label: level === 'healthy' ? 'Healthy' : healths.find((h) => h.level === level)!.label,
    detail: `${ready} of ${groups.length} components fully ready`,
    ...(bad.length ? { hint: `Look at: ${bad.join(', ')}.` } : {}),
  };
}

export function schedulerGroup(result: RouteResult<ComponentsReport> | undefined): ComponentGroup | null {
  if (result?.kind !== 'ok') return null;
  return result.data.components.find((g) => g.component === 'scheduler') ?? null;
}

/** Worst scheduler-autoscaling state across clusters; null when no cluster reports one. */
export function autoscalingRollup(clusters: ClusterView[] | undefined): { state: string; clusters: number } | null {
  const states = (clusters ?? []).map((c) => c.scheduler_autoscaling?.state).filter((s): s is string => !!s);
  if (!states.length) return null;
  const order = ['drift', 'not_observed', 'applying', 'plan_only', 'in_sync'];
  const state = order.find((s) => states.includes(s)) ?? states[0]!;
  return { state, clusters: states.length };
}

export function schedulerHealth(result: RouteResult<ComponentsReport> | undefined, error: unknown, clusters: ClusterView[] | undefined): Health {
  const g = schedulerGroup(result);
  const rollup = autoscalingRollup(clusters);
  const replicas: Health = g
    ? componentHealth(g)
    : result?.kind === 'unsupported'
      ? { level: 'not_reported', label: 'Not reported', detail: 'Replicas not reported by this control plane.', hint: UPGRADE_HINT }
      : controlPlaneHealth(result, error);
  if (!rollup) return replicas;
  const scaling = AUTOSCALING_LEVEL[rollup.state as AutoscalingState] ?? 'not_reported';
  const level = worst([replicas.level, scaling === 'not_reported' ? 'healthy' : scaling]);
  if (level === replicas.level) return replicas;
  return {
    level,
    label: autoscalingVisual(rollup.state).label,
    detail: replicas.detail,
    hint: autoscalingVisual(rollup.state).hint,
  };
}

export interface WorkerCounts {
  total: number;
  active: number;
  stale: number;
  draining: number;
  quarantined: number;
  dead: number;
}

export function countWorkers(workers: Worker[], now: number): WorkerCounts {
  const c: WorkerCounts = { total: workers.length, active: 0, stale: 0, draining: 0, quarantined: 0, dead: 0 };
  for (const w of workers) {
    if (w.state === 'ACTIVE') {
      if (heartbeatStale(w, now)) c.stale++;
      else c.active++;
    } else if (w.state === 'DRAINING') c.draining++;
    else if (w.state === 'QUARANTINED') c.quarantined++;
    else if (w.state === 'DEAD') c.dead++;
  }
  return c;
}

export function workerHealth(workers: Worker[] | undefined, error: unknown, canView: boolean, now: number): Health {
  if (!canView) return { level: 'unavailable', label: 'No access', detail: 'Worker inventory needs the workers.view permission.' };
  if (workers === undefined) {
    return error
      ? { level: 'unavailable', label: 'Unavailable', detail: 'Could not load the worker inventory.', hint: 'Open the worker fleet for the error.' }
      : { level: 'loading', label: 'Loading', detail: 'Loading workers…' };
  }
  const c = countWorkers(workers, now);
  if (c.total === 0) {
    return {
      level: 'not_reported',
      label: 'None registered',
      detail: 'No worker agent has registered in this namespace.',
      hint: 'Install a worker pool (Install on AKS) and give its agents a worker token.',
    };
  }
  const detail = `${c.active} active · ${c.draining} draining · ${c.stale} stale${c.dead ? ` · ${c.dead} dead` : ''}`;
  if (c.active === 0) {
    return { level: 'down', label: 'No active workers', detail, hint: 'Nothing can run. Check the agent pods and their connection to the gateway (port 9091).' };
  }
  if (c.stale || c.dead || c.quarantined) {
    return { level: 'degraded', label: 'Degraded', detail, hint: 'Some agents stopped heartbeating or were quarantined. Open the worker fleet.' };
  }
  return { level: 'healthy', label: 'Healthy', detail };
}

export function runnersHealth(result: RouteResult<TaskRunnersReport> | undefined, error: unknown): Health {
  if (result === undefined) {
    return error
      ? { level: 'unavailable', label: 'Unavailable', detail: 'Could not load task runners.', hint: 'See the Task runners tab for the error.' }
      : { level: 'loading', label: 'Loading', detail: 'Loading task runners…' };
  }
  if (result.kind === 'unsupported') {
    return {
      level: 'not_reported',
      label: 'Not reported',
      detail: 'Task placement is not reported by this control plane.',
      hint: 'Upgrade KGF Nexus to see which pod and node runs each task.',
    };
  }
  const n = result.data.summary.running;
  return n === 0
    ? { level: 'idle', label: 'Idle', detail: 'No task is in flight right now.' }
    : { level: 'healthy', label: `${n} in flight`, detail: summarizeCounts(result.data.summary.by_executor, 'executor') };
}

export function summarizeCounts(counts: Record<string, number>, what: string): string {
  const parts = Object.entries(counts)
    .filter(([, n]) => n > 0)
    .sort((a, b) => b[1] - a[1])
    .map(([k, n]) => `${n} ${k || `${what} not reported`}`);
  return parts.length ? parts.join(' · ') : `No ${what} reported`;
}

/** Three missed 15-second controller cycles plus slack (docs/CLUSTER-SCALING.md). */
export const CONTROLLER_STALE_MS = 2 * 60 * 1000;

export type Freshness = 'reporting' | 'stale' | 'silent';

export function controllerFreshness(status: ClusterStatus | null, now: number): Freshness {
  const t = parseTs(status?.last_report_at);
  if (t === null) return 'silent';
  return now - t > CONTROLLER_STALE_MS ? 'stale' : 'reporting';
}

export const FRESHNESS_VISUAL: Record<Freshness, { label: string; tone: Tone; glyph: string }> = {
  reporting: { label: 'Reporting', tone: 'success', glyph: '●' },
  stale: { label: 'Stale', tone: 'warning', glyph: '◌' },
  silent: { label: 'Not reporting', tone: 'muted', glyph: '○' },
};

export function clustersHealth(clusters: ClusterView[] | undefined, error: unknown, now: number): Health {
  if (clusters === undefined) {
    return error
      ? { level: 'unavailable', label: 'Unavailable', detail: 'Could not load the cluster registry.', hint: 'See the Clusters tab for the error.' }
      : { level: 'loading', label: 'Loading', detail: 'Loading clusters…' };
  }
  if (!clusters.length) {
    return {
      level: 'not_reported',
      label: 'None registered',
      detail: 'No cluster is registered; scaling is a preview only.',
      hint: 'Register a cluster (Install on AKS, step 3) and install the cluster controller.',
    };
  }
  const fresh = clusters.map((c) => controllerFreshness(c.status, now));
  const apply = clusters.filter((c) => c.mode === 'apply').length;
  const detail = `${clusters.length} registered · ${fresh.filter((f) => f === 'reporting').length} reporting · ${apply} in apply mode`;
  if (fresh.every((f) => f === 'silent')) {
    return { level: 'not_reported', label: 'Not reporting', detail, hint: 'No controller has reported. Enable clusterController in the Helm release.' };
  }
  if (fresh.some((f) => f !== 'reporting')) {
    return { level: 'stale', label: 'Stale', detail, hint: 'A controller stopped reporting. Check the nexus-cluster-controller pods.' };
  }
  if (clusters.some((c) => c.status?.last_error)) {
    return { level: 'degraded', label: 'Errors', detail, hint: 'A controller reported an error. Open the Clusters tab.' };
  }
  return { level: 'healthy', label: 'Healthy', detail };
}

// ── worker pools, live ──────────────────────────────────────────────────────

export interface PoolDeployment {
  cluster: string;
  namespace: string;
  name: string;
  ready: number | null;
  desired: number | null;
  observedAt: string | null;
}

export interface PoolLive {
  pool: WorkerPool;
  saved: boolean;
  counts: WorkerCounts;
  cpuMillis: number;
  memoryBytes: number;
  gpuCount: number;
  /** null when no matching worker reports active_attempts. */
  runningTasks: number | null;
  /** Controller-observed Deployments bound to this pool, across clusters. */
  deployments: PoolDeployment[];
  /** True when at least one registered cluster binds the pool. */
  bound: boolean;
}

export const servesQueue = (w: Worker, queue: string) => w.queues.includes(queue);

export function poolLive(
  pools: WorkerPool[],
  savedNames: ReadonlySet<string>,
  workers: Worker[] | undefined,
  clusters: ClusterView[] | undefined,
  now: number,
): PoolLive[] {
  return pools.map((pool) => {
    const matching = (workers ?? []).filter((w) => servesQueue(w, pool.queue));
    // Capacity and load count only workers that are heartbeating: a quiet one may already be gone.
    const live = matching.filter((w) => (w.state === 'ACTIVE' && !heartbeatStale(w, now)) || w.state === 'DRAINING');
    const reported = live.filter((w) => typeof w.active_attempts === 'number');
    const deployments: PoolDeployment[] = [];
    let bound = false;
    for (const c of clusters ?? []) {
      for (const b of c.bindings.filter((x) => x.pool === pool.name)) {
        bound = true;
        const o = c.observations.find(
          (x) => x.kind === 'WorkerDeployment' && x.namespace === b.kubernetes_namespace && x.name === b.worker_deployment,
        );
        deployments.push({
          cluster: c.name,
          namespace: b.kubernetes_namespace,
          name: b.worker_deployment,
          ready: o ? o.ready_replicas : null,
          desired: o ? o.spec_replicas : null,
          observedAt: o ? o.observed_at : null,
        });
      }
    }
    return {
      pool,
      saved: savedNames.has(pool.name),
      counts: countWorkers(matching, now),
      cpuMillis: live.reduce((n, w) => n + (w.capacity.cpu_millis ?? 0), 0),
      memoryBytes: live.reduce((n, w) => n + (w.capacity.memory_bytes ?? 0), 0),
      gpuCount: live.reduce((n, w) => n + (w.capacity.gpu_count ?? 0), 0),
      runningTasks: reported.length ? reported.reduce((n, w) => n + (w.active_attempts ?? 0), 0) : null,
      deployments,
      bound,
    };
  });
}

/** Workers that serve no pool's queue: running, but outside any scaling policy. */
export function unpooledWorkers(pools: WorkerPool[], workers: Worker[] | undefined): Worker[] {
  const queues = new Set(pools.map((p) => p.queue));
  return (workers ?? []).filter((w) => w.state !== 'DEAD' && !w.queues.some((q) => queues.has(q)));
}

export function pendingTasks(capacity: CapacitySummary | undefined): number | null {
  return typeof capacity?.pending_attempts === 'number' ? capacity.pending_attempts : null;
}

// ── task runners ────────────────────────────────────────────────────────────

/** Filter value meaning "the field was not reported" (an empty string on the wire). */
export const NOT_REPORTED = '__not_reported__';

export interface RunnerFilter {
  executor: string;
  cluster: string;
  node: string;
  state: string;
}

export const NO_RUNNER_FILTER: RunnerFilter = { executor: '', cluster: '', node: '', state: '' };

function matches(value: string, want: string): boolean {
  if (want === '') return true;
  if (want === NOT_REPORTED) return value === '';
  return value === want;
}

export function filterRunners(runners: TaskRunner[], f: RunnerFilter): TaskRunner[] {
  return runners.filter(
    (r) => matches(r.executor, f.executor) && matches(r.cluster, f.cluster) && matches(r.k8s_node, f.node) && matches(r.state, f.state),
  );
}

/** Distinct values of a field, sorted, with '' (not reported) last. */
export function distinctValues(runners: TaskRunner[], field: keyof TaskRunner): string[] {
  const values = [...new Set(runners.map((r) => r[field]))];
  return values.sort((a, b) => (a === '' ? 1 : b === '' ? -1 : a.localeCompare(b)));
}

// ── scaling events ──────────────────────────────────────────────────────────

/** "HPA scheduler" for the controller's HPA rows (worker_deployment "hpa/<name>"), else namespace/name. */
export function eventTarget(e: { kubernetes_namespace: string; worker_deployment: string }): { hpa: boolean; label: string } {
  if (e.worker_deployment.startsWith('hpa/')) return { hpa: true, label: `HPA ${e.worker_deployment.slice(4)}` };
  return { hpa: false, label: `${e.kubernetes_namespace}/${e.worker_deployment}` };
}

const isRecord = (v: unknown): v is Record<string, unknown> => v !== null && typeof v === 'object' && !Array.isArray(v);

/**
 * An event's before/after settings as one line ("min_replicas 1→2, max_replicas
 * 4→6"); keys that did not change are left out. Null when the event carries none.
 */
export function describeChange(detail: Record<string, unknown> | undefined): string | null {
  if (!detail || !isRecord(detail.before) || !isRecord(detail.after)) return null;
  const before = detail.before;
  const after = detail.after;
  const keys = [...new Set([...Object.keys(before), ...Object.keys(after)])];
  const fmt = (v: unknown) => (v === null || v === undefined ? 'unset' : typeof v === 'object' ? JSON.stringify(v) : String(v));
  const parts = keys.filter((k) => fmt(before[k]) !== fmt(after[k])).map((k) => `${k.replaceAll('_', ' ')} ${fmt(before[k])}→${fmt(after[k])}`);
  return parts.length ? parts.join(', ') : 'no change';
}

/** -1 in a headroom figure means the namespace is not constrained on it. */
export function headroomValue(n: number | null, fmt: (n: number) => string): string {
  if (n === null) return '—';
  if (n < 0) return 'unlimited';
  return fmt(n);
}

// ── instance detail ─────────────────────────────────────────────────────────

const n = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) ? v : null);

/**
 * The few detail values worth a glance in a table row (the rest stay one
 * click away): readiness when it is not "ok", shutting down, and for a
 * scheduler its queue, recent throughput, backlog and errors.
 */
export function detailSummary(component: string, d: Record<string, unknown>): string[] {
  const out: string[] = [];
  if (d.shutting_down === true) out.push('shutting down');
  if (typeof d.readiness === 'string' && d.readiness !== 'ok' && d.readiness !== '') out.push(`readiness: ${d.readiness}`);
  if (component === 'scheduler') {
    if (typeof d.queue === 'string' && d.queue) out.push(`queue ${d.queue}`);
    const window = n(d.recent_window_seconds);
    const dispatched = n(d.dispatched_recent);
    if (dispatched !== null) out.push(`${dispatched} dispatched${window ? ` / ${window}s` : ''}`);
    const pending = n(d.pending_attempts);
    if (pending !== null) out.push(`${pending} pending`);
    const oldest = n(d.oldest_pending_age_seconds);
    if (oldest !== null && oldest > 0) out.push(`oldest ${Math.round(oldest)}s`);
    const errors = n(d.claim_errors_total);
    if (errors) out.push(`${errors} claim errors`);
    const lost = n(d.lost_total);
    if (lost) out.push(`${lost} lost`);
  }
  if (component === 'api-gateway' && typeof d.placement_reporting === 'boolean') {
    out.push(d.placement_reporting ? 'placement reporting on' : 'placement reporting off');
  }
  return out;
}
