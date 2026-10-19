// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Wire-shaped fixtures for the infrastructure page and its pure modules.
 *
 * Each builder returns what the GATEWAY sends (C3/C5 shapes included), so the
 * same objects serve the unit tests and the fetch-stubbed component tests.
 */

import type {
  ClusterView,
  ComponentGroup,
  ComponentInstance,
  ComponentsReport,
  PlatformConfig,
  PlatformPlan,
  PodProfile,
  TaskRunner,
  TaskRunnersReport,
  WorkerPool,
} from '../api/platform';
import { CONTROL_PLANE_COMPONENTS } from '../api/platform';
import type { Worker } from '../api/types';

export const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
export const recent = () => ago(5_000);

export const DIGEST = `ghcr.io/acme/task@sha256:${'a'.repeat(64)}`;

export function profile(over: Partial<PodProfile> = {}): PodProfile {
  return {
    name: 'cpu-small',
    cloud: 'azure',
    kubernetes_namespace: 'nexus-tasks',
    service_account: 'nexus-task',
    image: DIGEST,
    cpu_millis: 2000,
    memory_mib: 4096,
    gpu_count: 0,
    ephemeral_storage_mib: 10240,
    node_selector: {},
    tolerations: [],
    deadline_seconds: 3600,
    grace_seconds: 30,
    ...over,
  };
}

export function pool(over: Partial<WorkerPool> = {}): WorkerPool {
  return {
    name: 'cpu',
    queue: 'default',
    profile: 'cpu-small',
    min_workers: 1,
    max_workers: 10,
    tasks_per_worker: 2,
    idle_seconds: 300,
    max_scale_up_step: 2,
    enabled: true,
    ...over,
  };
}

export function config(over: Partial<PlatformConfig> = {}): PlatformConfig {
  return {
    revision: 3,
    scheduler: { min_replicas: 2, max_replicas: 6, target_cpu_percent: 65, scale_down_seconds: 300, fair_share_weight: 1 },
    profiles: [profile()],
    pools: [pool()],
    ...over,
  };
}

export function plan(over: Partial<PlatformPlan> = {}): PlatformPlan {
  return {
    revision: 3,
    mode: 'preview',
    message: 'This preview is computed by the gateway and is not applied.',
    decisions: [
      {
        pool: 'cpu',
        current: 1,
        desired: 3,
        reason: 'pending resource demand',
        limited: false,
        demand: { pending_tasks: 4, running_tasks: 2, cpu_millis: 8000, memory_mib: 16384, gpu_count: 0, current_workers: 1, idle_seconds: 0, unschedulable_tasks: 1 },
      },
    ],
    task_job_templates: { 'cpu-small': { kind: 'Job' } },
    controllers: [{ cluster: 'aks-prod', registry_mode: 'plan', reporting: true, effective_mode: 'plan', last_report_at: recent() }],
    ...over,
  };
}

export function cluster(over: Partial<ClusterView> = {}): ClusterView {
  return {
    id: 'c1',
    name: 'aks-prod',
    provider: 'aks',
    provider_resource_id: '',
    expected_cluster_uid: '',
    control_plane_namespace: 'kgf-nexus',
    mode: 'plan',
    desired_revision: 3,
    bindings: [],
    updated_at: recent(),
    updated_by: 'admin',
    status: {
      observed_revision: 3,
      observed_cluster_uid: 'uid-1',
      identity_verified: true,
      kubernetes_version: 'v1.31.2',
      controller_identity: 'controller-0',
      effective_mode: 'plan',
      effective_mode_reason: 'registry mode is plan (dry-run)',
      last_report_at: recent(),
      last_error: '',
      last_error_at: null,
    },
    observations: [],
    scaling: [],
    events: [],
    errors: [],
    jobs_shown: 0,
    labels: { 'nexus.io/region': 'westeurope' },
    headroom: { cpu_millis: 12000, gpu_count: 2 },
    scheduler_autoscaling: {
      desired: { min_replicas: 2, max_replicas: 6, target_cpu_percent: 65, scale_down_stabilization_seconds: 300, config_revision: 3 },
      observed: {
        min_replicas: 2,
        max_replicas: 6,
        target_cpu_percent: 65,
        scale_down_stabilization_seconds: 300,
        current_replicas: 2,
        desired_replicas: 2,
        hpa_name: 'scheduler',
        deployment: 'scheduler',
      },
      state: 'in_sync',
      last_applied_at: ago(60_000),
      last_error: null,
    },
    ...over,
  };
}

export function instance(over: Partial<ComponentInstance> = {}): ComponentInstance {
  return {
    instance: 'api-gateway-7d9c-x2',
    pod_namespace: 'kgf-nexus',
    node: 'aks-system-1234-vmss000001',
    cluster: 'aks-prod',
    version: 'v0.2.0',
    started_at: ago(3_600_000),
    last_seen_at: recent(),
    state: 'ready',
    detail: {},
    ...over,
  };
}

/** Every component with ready instances (two schedulers), unless overridden. */
export function components(over: Partial<Record<string, ComponentInstance[]>> = {}): ComponentsReport {
  const groups: ComponentGroup[] = CONTROL_PLANE_COMPONENTS.map((name) => {
    const instances =
      over[name] ??
      (name === 'scheduler'
        ? [
            instance({ instance: 'scheduler-5f7b-a1', detail: { queue: 'default', claims_per_minute: 120 } }),
            instance({ instance: 'scheduler-5f7b-b2', detail: { queue: 'default' } }),
          ]
        : [instance({ instance: `${name}-6c8d-z9` })]);
    return {
      component: name,
      summary: {
        ready: instances.filter((i) => i.state === 'ready').length,
        total: instances.length,
        stale: instances.filter((i) => i.state === 'stale').length,
      },
      instances,
    };
  });
  return { generated_at: recent(), stale_after_seconds: 30, components: groups };
}

export function runner(over: Partial<TaskRunner> = {}): TaskRunner {
  return {
    attempt_id: 'att-1',
    run_id: 'run-1',
    node_id: 'extract',
    workflow: 'etl.daily',
    worker_id: 'wk-1',
    worker_host: 'agent-cpu-0',
    executor: 'kubernetes',
    cluster: 'aks-prod',
    k8s_namespace: 'nexus-tasks',
    pod: 'extract-att-1-abcde',
    k8s_node: 'aks-cpu-1234-vmss000002',
    state: 'RUNNING',
    started_at: ago(90_000),
    progress_message: '40% — reading partitions',
    placement_reported_at: ago(80_000),
    ...over,
  };
}

export function runnersReport(runners: TaskRunner[]): Omit<TaskRunnersReport, 'limit'> {
  const by = (f: (r: TaskRunner) => string) =>
    runners.reduce<Record<string, number>>((m, r) => ({ ...m, [f(r)]: (m[f(r)] ?? 0) + 1 }), {});
  return {
    generated_at: recent(),
    summary: { running: runners.length, by_executor: by((r) => r.executor), by_cluster: by((r) => r.cluster) },
    runners,
  };
}

export function worker(over: Partial<Worker> = {}): Worker {
  return {
    id: 'wk-1',
    namespace: 'default',
    hostname: 'agent-cpu-0',
    queues: ['default'],
    executors: ['kubernetes'],
    capacity: { cpu_millis: 4000, memory_bytes: 8 * 1024 ** 3, gpu_count: 0 },
    committed: { cpu_millis: 2000, memory_bytes: 4 * 1024 ** 3, gpu_count: 0 },
    state: 'ACTIVE',
    agent_version: 'v0.2.0',
    last_heartbeat_at: new Date().toISOString(),
    active_attempts: 1,
    ...over,
  };
}
