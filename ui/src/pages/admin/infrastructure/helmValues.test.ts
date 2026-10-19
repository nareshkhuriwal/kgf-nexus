// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import {
  agentForPool,
  chartRef,
  planValuesYaml,
  poolCapacity,
  repoLink,
  schedulerEntries,
  schedulerName,
  schedulerQueues,
} from './helmValues';
import { readYaml } from '../../../test/readYaml';
import { config, plan, pool, profile } from '../../../test/infraFixtures';

describe('references', () => {
  it('points at the owner’s chart and repository, lower-casing for GHCR', () => {
    expect(chartRef('Acme')).toBe('oci://ghcr.io/acme/charts/kgf-nexus');
    expect(chartRef()).toBe('oci://ghcr.io/nareshkhuriwal/charts/kgf-nexus');
    expect(repoLink('docs/INSTALL-AKS.md', { anchor: 'prerequisites' })).toBe(
      'https://github.com/nareshkhuriwal/kgf-nexus/blob/main/docs/INSTALL-AKS.md#prerequisites',
    );
  });
});

describe('schedulers', () => {
  it('runs one scheduler per queue, default first and always present', () => {
    expect(schedulerQueues(['gpu', 'batch', 'gpu'])).toEqual(['default', 'batch', 'gpu']);
    expect(schedulerName('default')).toBe('scheduler');
    expect(schedulerName('gpu')).toBe('scheduler-gpu');
    expect(schedulerName('q'.repeat(70))).toHaveLength(63);
  });

  it('carries the saved autoscaling policy into every scheduler', () => {
    const [s] = schedulerEntries({ min_replicas: 2, max_replicas: 5, target_cpu_percent: 60, scale_down_seconds: 600 }, [], {
      'kubernetes.azure.com/agentpool': 'system',
    });
    expect(s).toEqual({
      name: 'scheduler',
      queue: 'default',
      replicas: 2,
      nodeSelector: { 'kubernetes.azure.com/agentpool': 'system' },
      autoscaling: {
        enabled: true,
        minReplicas: 2,
        maxReplicas: 5,
        targetCPUUtilizationPercentage: 60,
        behavior: { scaleDown: { stabilizationWindowSeconds: 600 } },
      },
    });
  });
});

describe('agents', () => {
  it('advertises the profile’s task size times the pool’s task slots', () => {
    expect(poolCapacity(pool({ tasks_per_worker: 4 }), profile({ cpu_millis: 2000, memory_mib: 1024, gpu_count: 1, ephemeral_storage_mib: 2048 }))).toEqual({
      cpuMillis: 8000,
      memoryBytes: 4 * 1024 ** 3,
      gpuCount: 4,
      diskBytes: 8 * 1024 ** 3,
    });
  });

  it('maps a pool to a kubernetes-executor agent running tasks as its profile', () => {
    expect(agentForPool(pool(), profile(), 3)).toMatchObject({
      name: 'cpu',
      replicas: 3,
      queues: 'default',
      executors: 'kubernetes',
      labels: 'pool=cpu,accelerator=none',
      kubernetes: { namespace: 'nexus-tasks', serviceAccount: 'nexus-task' },
    });
  });
});

describe('planValuesYaml', () => {
  it('exports parseable values with the plan’s replicas and says where each number came from', () => {
    const cfg = config({ pools: [pool(), pool({ name: 'gpu', queue: 'gpu', profile: 'missing', enabled: false })] });
    const text = planValuesYaml(cfg, plan(), { namespace: 'acme', exportedAt: '2026-10-19T10:00:00Z' });
    const tree = readYaml(text) as { schedulers: { name: string }[]; agents: { name: string; replicas: number }[] };
    expect(tree.schedulers.map((s) => s.name)).toEqual(['scheduler', 'scheduler-gpu']);
    expect(tree.agents.map((a) => [a.name, a.replicas])).toEqual([
      ['cpu', 3],
      ['gpu', 1],
    ]);
    expect(text).toContain('# KGF Nexus scaling policy, namespace "acme", saved revision 3, plan revision 3.');
    expect(text).toContain('the demand plan suggests 3 (now 1)');
    expect(text).toContain('the pool minimum (1); the plan has no decision for this pool.');
    expect(text).toContain('Pod profile "missing" is missing');
    expect(text).toContain('This pool is disabled in scaling plans.');
  });

  it('exports without a plan', () => {
    const tree = readYaml(planValuesYaml(config(), undefined, { namespace: 'acme', exportedAt: 'now' })) as { agents: { replicas: number }[] };
    expect(tree.agents[0]!.replicas).toBe(1);
  });
});
