// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Saved platform policy → Helm values for the kgf-nexus chart.
 *
 * Shared by the Scaling plan export and the Install on AKS wizard so the two
 * can never describe the same pool differently. PURE: no React, no fetching,
 * and every function is covered by helmValues.test.ts.
 *
 * The mapping, stated once:
 *  - A scheduler drains one queue, so there is one `schedulers[]` entry per
 *    distinct queue the pools use (plus `default`, which the chart requires).
 *    Each carries the saved autoscaling policy; in-cluster, the cluster
 *    controller applies that same policy to the HPA (plan/apply modes).
 *  - A worker pool becomes one `agents[]` entry. Its advertised capacity is
 *    the pod profile's task size times the pool's task slots per worker: an
 *    agent with 4 slots of a 2-CPU profile advertises 8 CPUs. Task pods run in
 *    the profile's Kubernetes namespace as its service account.
 */

import type { PlatformConfig, PlatformPlan, PodProfile, SchedulerPolicy, WorkerPool } from '../../../api/platform';
import { toYaml, type YamlMapping } from '../../../lib/yaml';

/** The GitHub owner whose releases this console points at by default. */
export const GITHUB_OWNER = 'nareshkhuriwal';
export const REPO_NAME = 'kgf-nexus';
/** The chart version (deploy/helm/kgf-nexus/Chart.yaml) the AKS install was built against. */
export const DEFAULT_CHART_VERSION = '0.3.0';

export const DOCS = {
  installAks: 'docs/INSTALL-AKS.md',
  exampleFull: 'deploy/helm/kgf-nexus/examples/aks-existing-cluster.yaml',
  examplePool: 'deploy/helm/kgf-nexus/examples/aks-worker-pool.yaml',
  clusterScaling: 'docs/CLUSTER-SCALING.md',
  tls: 'docs/TLS.md',
  operations: 'docs/OPERATIONS.md',
  values: 'deploy/helm/kgf-nexus/values.yaml',
  azurePostgresRoots: 'deploy/terraform/ca-bundles/azure-postgres-roots.pem',
} as const;

export function repoUrl(owner = GITHUB_OWNER): string {
  return `https://github.com/${owner}/${REPO_NAME}`;
}

/** A link to a file in the repository, optionally to a heading anchor. */
export function repoLink(path: string, { owner = GITHUB_OWNER, ref = 'main', anchor = '' } = {}): string {
  return `${repoUrl(owner)}/blob/${ref}/${path}${anchor ? `#${anchor}` : ''}`;
}

export function chartRef(owner = GITHUB_OWNER): string {
  return `oci://ghcr.io/${owner.toLowerCase()}/charts/${REPO_NAME}`;
}

export function imageValues(owner: string, tag: string, pullSecrets: string[] = []): YamlMapping {
  return {
    registry: 'ghcr.io/',
    repositoryPrefix: `${owner.toLowerCase()}/${REPO_NAME}/`,
    tag,
    pullSecrets: pullSecrets.length ? pullSecrets : undefined,
  };
}

/** Distinct queues, `default` first (the chart refuses a release without it). */
export function schedulerQueues(queues: string[]): string[] {
  return ['default', ...[...new Set(queues)].filter((q) => q !== 'default').sort()];
}

/** `scheduler` for the default queue, `scheduler-<queue>` otherwise (a DNS label). */
export function schedulerName(queue: string): string {
  return queue === 'default' ? 'scheduler' : `scheduler-${queue}`.slice(0, 63).replace(/-+$/, '');
}

export function schedulerEntries(
  policy: SchedulerPolicy,
  queues: string[],
  nodeSelector?: Record<string, string>,
): YamlMapping[] {
  return schedulerQueues(queues).map((queue) => ({
    name: schedulerName(queue),
    queue,
    replicas: policy.min_replicas,
    nodeSelector: nodeSelector && Object.keys(nodeSelector).length ? nodeSelector : undefined,
    autoscaling: {
      enabled: true,
      minReplicas: policy.min_replicas,
      maxReplicas: policy.max_replicas,
      targetCPUUtilizationPercentage: policy.target_cpu_percent,
      behavior: { scaleDown: { stabilizationWindowSeconds: policy.scale_down_seconds } },
    },
  }));
}

const MiB = 1024 * 1024;

/** What one agent of this pool advertises: the profile's task size × its task slots. */
export function poolCapacity(pool: WorkerPool, profile: PodProfile | undefined) {
  const slots = Math.max(1, pool.tasks_per_worker);
  return {
    cpuMillis: (profile?.cpu_millis ?? 2000) * slots,
    memoryBytes: (profile?.memory_mib ?? 4096) * MiB * slots,
    gpuCount: (profile?.gpu_count ?? 0) * slots,
    diskBytes: Math.max(profile?.ephemeral_storage_mib ?? 10_240, 1024) * MiB * slots,
  };
}

export function agentForPool(pool: WorkerPool, profile: PodProfile | undefined, replicas: number): YamlMapping {
  const cap = poolCapacity(pool, profile);
  return {
    name: pool.name,
    replicas,
    queues: pool.queue,
    labels: `pool=${pool.name},accelerator=${cap.gpuCount > 0 ? 'nvidia' : 'none'}`,
    executors: 'kubernetes',
    cpuMillis: cap.cpuMillis,
    memoryBytes: cap.memoryBytes,
    gpuCount: cap.gpuCount,
    diskBytes: cap.diskBytes,
    kubernetes: profile
      ? { namespace: profile.kubernetes_namespace, serviceAccount: profile.service_account }
      : undefined,
  };
}

/**
 * The scaling plan as a values snippet: schedulers with the saved autoscaling
 * policy, and one agent per pool. Replicas are the plan's suggestion when the
 * plan has one for the pool, else the pool's minimum — and the file says which.
 */
export function planValuesYaml(
  config: PlatformConfig,
  plan: PlatformPlan | undefined,
  meta: { namespace: string; exportedAt: string },
): string {
  const profiles = new Map(config.profiles.map((p) => [p.name, p]));
  const decisions = new Map((plan?.decisions ?? []).map((d) => [d.pool, d]));
  const comments: Record<string, string[]> = {
    schedulers: [
      'One scheduler per queue the pools use. autoscaling is the saved scheduler policy; in apply mode',
      '(clusterController.apply.enabled and apply.schedulerAutoscaling) the cluster controller keeps the HPAs at it.',
    ],
  };
  const agents = config.pools.map((pool, i) => {
    const d = decisions.get(pool.name);
    const replicas = d ? d.desired : pool.min_workers;
    const notes = [
      d
        ? `replicas: the demand plan suggests ${d.desired} (now ${d.current}${d.limited ? ', capped by the pool maximum' : ''}).`
        : `replicas: the pool minimum (${pool.min_workers}); the plan has no decision for this pool.`,
      `Scaling bounds ${pool.min_workers}–${pool.max_workers} workers, ${pool.tasks_per_worker} task slot(s) each.`,
    ];
    if (!profiles.has(pool.profile)) notes.push(`Pod profile "${pool.profile}" is missing: capacity uses chart defaults.`);
    if (!pool.enabled) notes.push('This pool is disabled in scaling plans.');
    comments[`agents.${i}`] = notes;
    return agentForPool(pool, profiles.get(pool.profile), replicas);
  });
  return toYaml(
    {
      schedulers: schedulerEntries(config.scheduler, config.pools.map((p) => p.queue)),
      agents,
    },
    {
      header: [
        `KGF Nexus scaling policy, namespace "${meta.namespace}", saved revision ${config.revision}${plan ? `, plan revision ${plan.revision}` : ''}.`,
        `Exported ${meta.exportedAt}. Merge into your Helm values (helm upgrade --values ...);`,
        `see ${DOCS.installAks} and ${DOCS.examplePool}.`,
      ],
      comments,
    },
  );
}
