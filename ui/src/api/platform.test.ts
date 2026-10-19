// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The typed infrastructure client: what reaches the wire, and how responses
 * from older or partial gateways are normalised rather than trusted.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';
import { headroomEntries, normalizeCluster, normalizeComponents, normalizeConfig, normalizePlan, platformApi } from './platform';
import { ApiError } from './client';

afterEach(() => vi.unstubAllGlobals());

function stub(status: number, body: unknown) {
  const calls: { url: string; init?: RequestInit }[] = [];
  vi.stubGlobal('fetch', async (url: string, init?: RequestInit) => {
    calls.push({ url: String(url), init });
    return new Response(status === 204 ? null : JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
  });
  return calls;
}

describe('normalisation', () => {
  it('fills a config that omits collections, keeping an absent fair-share weight absent', () => {
    const c = normalizeConfig({ revision: 2, scheduler: { min_replicas: 1, max_replicas: 3, target_cpu_percent: 50, scale_down_seconds: 120 }, pools: null });
    expect(c.pools).toEqual([]);
    expect(c.profiles).toEqual([]);
    expect(c.scheduler).not.toHaveProperty('fair_share_weight');
  });

  it('turns a profile node selector into strings and tolerations into full objects', () => {
    const c = normalizeConfig({ profiles: [{ name: 'p', node_selector: { a: 1 }, tolerations: [{ key: 'sku' }] }] });
    expect(c.profiles[0]!.node_selector).toEqual({ a: '1' });
    expect(c.profiles[0]!.tolerations).toEqual([{ key: 'sku', value: '', effect: '' }]);
  });

  it('gives an older plan an empty controllers list and message', () => {
    const p = normalizePlan({ revision: 1, decisions: [{ pool: 'cpu', current: 1, desired: 2 }] });
    expect(p.controllers).toEqual([]);
    expect(p.message).toBe('');
    expect(p.decisions[0]!.demand.pending_tasks).toBe(0);
  });

  it('leaves C5 cluster fields absent when the server omits them, and present when it sends them', () => {
    const older = normalizeCluster({ name: 'a', status: null });
    expect(older).not.toHaveProperty('labels');
    expect(older).not.toHaveProperty('headroom');
    expect(older).not.toHaveProperty('scheduler_autoscaling');
    expect(older.observations).toEqual([]);
    const newer = normalizeCluster({ name: 'a', labels: {}, headroom: {}, scheduler_autoscaling: null });
    expect(newer.labels).toEqual({});
    expect(newer.scheduler_autoscaling).toBeNull();
    const observed = normalizeCluster({ scheduler_autoscaling: { state: 'drift', desired: { min_replicas: 1 }, observed: { max_replicas: 9 } } });
    expect(observed.scheduler_autoscaling).toMatchObject({ state: 'drift', desired: { min_replicas: 1, max_replicas: 0 }, observed: { max_replicas: 9, min_replicas: null }, targets: [] });
  });

  it('reads per-HPA targets, keeping an unreported ownership unknown', () => {
    const c = normalizeCluster({
      scheduler_autoscaling: {
        state: 'drift',
        targets: [
          { state: 'drift', managed: false, reason: 'not controller-owned', observed: { hpa_name: 'scheduler', target_cpu_percent: null } },
          { state: 'in_sync', observed: { hpa_name: 'scheduler-gpu' } },
          'junk',
        ],
      },
    });
    expect(c.scheduler_autoscaling!.targets.map((t) => [t.observed?.hpa_name, t.state, t.managed])).toEqual([
      ['scheduler', 'drift', false],
      ['scheduler-gpu', 'in_sync', null],
    ]);
    expect(c.scheduler_autoscaling!.targets[0]!.observed!.target_cpu_percent).toBeNull();
  });

  it('turns headroom into rows per task namespace, keeping -1 for "not constrained"', () => {
    expect(
      headroomEntries({ 'nexus-tasks': { cpu_millis: 6000, memory_bytes: 1024, gpu_count: -1, pods: 40, extended: { 'nvidia.com/mig-1g.10gb': 2, bad: 'x' } }, broken: 3 }),
    ).toEqual([{ namespace: 'nexus-tasks', cpu_millis: 6000, memory_bytes: 1024, gpu_count: -1, pods: 40, extended: { 'nvidia.com/mig-1g.10gb': 2 } }]);
  });

  it('assumes an older components report shows the topology, and honours false', () => {
    expect(normalizeComponents({ components: [] }).topology_visible).toBe(true);
    expect(normalizeComponents({ topology_visible: false, components: [] }).topology_visible).toBe(false);
  });
});

describe('platformApi', () => {
  it('saves without the quotas, so this page can never change them', async () => {
    const calls = stub(200, { revision: 4, scheduler: {}, pools: [], profiles: [] });
    await platformApi.saveConfig('team a', { revision: 3, scheduler: { min_replicas: 1, max_replicas: 2, target_cpu_percent: 50, scale_down_seconds: 60 }, pools: [], profiles: [], quotas: { locked: true } });
    expect(calls[0]!.url).toBe('/v1/namespaces/team%20a/platform/config');
    expect(calls[0]!.init!.method).toBe('PUT');
    expect(JSON.parse(calls[0]!.init!.body as string)).not.toHaveProperty('quotas');
  });

  it('reads a 404 from the inventory routes as "unsupported", and anything else as an error', async () => {
    stub(404, { error: 'not_found' });
    await expect(platformApi.getComponents('ns')).resolves.toEqual({ kind: 'unsupported', status: 404 });
    stub(501, {});
    await expect(platformApi.listTaskRunners('ns')).resolves.toEqual({ kind: 'unsupported', status: 501 });
    stub(500, { message: 'boom' });
    await expect(platformApi.getComponents('ns')).rejects.toBeInstanceOf(ApiError);
  });

  it('clamps the task-runner limit to what the server accepts', async () => {
    const calls = stub(200, { runners: [] });
    const r = await platformApi.listTaskRunners('ns', 5000);
    expect(calls[0]!.url).toBe('/v1/namespaces/ns/platform/task-runners?limit=1000');
    expect(r).toMatchObject({ kind: 'ok', data: { limit: 1000, summary: { running: 0 } } });
  });

  it('deletes a cluster at the revision the operator saw', async () => {
    const calls = stub(204, null);
    await platformApi.deleteCluster('ns', 'aks prod', 7);
    expect(calls[0]!.url).toBe('/v1/namespaces/ns/platform/clusters/aks%20prod?revision=7');
  });

  it('mints a worker token with role worker and scope worker_rpc only', async () => {
    const calls = stub(201, { id: 't', token: 'secret' });
    await platformApi.mintWorkerToken('ns', { name: 'agents', expires_in_days: 30 });
    expect(JSON.parse(calls[0]!.init!.body as string)).toEqual({ name: 'agents', role: 'worker', scopes: ['worker_rpc'], expires_in_days: 30 });
  });
});
