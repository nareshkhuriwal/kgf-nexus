// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The page's pure logic: validation that mirrors the gateway's, health levels
 * that never claim more than a report says, and the live pool roll-up.
 */

import { describe, expect, it } from 'vitest';
import {
  changedSections,
  clustersHealth,
  componentHealth,
  controllerFreshness,
  controlPlaneHealth,
  countWorkers,
  distinctValues,
  fieldId,
  filterRunners,
  fromDraft,
  NO_RUNNER_FILTER,
  NOT_REPORTED,
  nextName,
  poolLive,
  runnersHealth,
  schedulerHealth,
  toDraft,
  unpooledWorkers,
  validateConfig,
  validatePools,
  validateProfiles,
  validateScheduler,
  workerHealth,
  worst,
} from './model';
import { ago, cluster, components, config, instance, pool, profile, runner, runnersReport, worker } from '../../../test/infraFixtures';
import type { ComponentsReport, RouteResult, TaskRunnersReport } from '../../../api/platform';

const now = Date.now();
const ok = (data: ComponentsReport): RouteResult<ComponentsReport> => ({ kind: 'ok', data });

describe('validateScheduler', () => {
  const base = { min_replicas: 2, max_replicas: 6, target_cpu_percent: 65, scale_down_seconds: 300, fair_share_weight: 1 };

  it('accepts the saved defaults', () => {
    expect(validateScheduler(base)).toEqual({});
  });

  it('holds min ≤ max and names the minimum in the message', () => {
    expect(validateScheduler({ ...base, min_replicas: 5, max_replicas: 3 })).toEqual({
      'scheduler.max_replicas': 'Must be at least the minimum (5).',
    });
  });

  it('enforces each range the gateway enforces', () => {
    const e = validateScheduler({ min_replicas: 0, max_replicas: 101, target_cpu_percent: 95, scale_down_seconds: 30, fair_share_weight: 0 });
    expect(Object.keys(e).sort()).toEqual([
      'scheduler.fair_share_weight',
      'scheduler.max_replicas',
      'scheduler.min_replicas',
      'scheduler.scale_down_seconds',
      'scheduler.target_cpu_percent',
    ]);
    expect(e['scheduler.target_cpu_percent']).toBe('Must be between 10 and 90.');
  });

  it('says a cleared or fractional field is not a number rather than out of range', () => {
    expect(validateScheduler({ ...base, min_replicas: Number.NaN })['scheduler.min_replicas']).toBe('Enter a number.');
    expect(validateScheduler({ ...base, target_cpu_percent: 50.5 })['scheduler.target_cpu_percent']).toBe('Enter a whole number.');
  });

  it('treats an absent fair-share weight as the default 1', () => {
    const { fair_share_weight: _omit, ...older } = base;
    expect(validateScheduler(older)).toEqual({});
  });
});

describe('validateProfiles', () => {
  const draft = (over = {}) => toDraft(config({ profiles: [profile(over)] })).profiles;

  it('requires an immutable image digest', () => {
    expect(validateProfiles(draft({ image: 'ghcr.io/acme/task:latest' }))['profiles.0.image']).toMatch(/sha256/);
    expect(validateProfiles(draft({ image: `x@sha256:${'A'.repeat(64)}` }))['profiles.0.image']).toMatch(/sha256/);
    expect(validateProfiles(draft())).toEqual({});
  });

  it('rejects duplicate and non-DNS names', () => {
    const profiles = [...draft(), ...draft(), ...draft({ name: 'Bad_Name' })];
    const e = validateProfiles(profiles);
    expect(e['profiles.1.name']).toBe('Profile 1 already uses this name.');
    expect(e['profiles.2.name']).toMatch(/lowercase/);
  });

  it('checks node selector rows, ignoring a blank row being added', () => {
    const [p] = draft();
    p!.node_selector = [
      { key: 'agentpool', value: 'gpu' },
      { key: 'agentpool', value: 'cpu' },
      { key: '', value: 'orphan' },
      { key: '', value: '' },
    ];
    const e = validateProfiles([p!]);
    expect(e['profiles.0.node_selector.1.key']).toBe('This key is already set above.');
    expect(e['profiles.0.node_selector.2.key']).toMatch(/Enter a label key/);
    expect(e['profiles.0.node_selector.3.key']).toBeUndefined();
  });

  it('only accepts the three taint effects', () => {
    const e = validateProfiles(draft({ tolerations: [{ key: 'sku', value: 'gpu', effect: 'NoRun' }] }));
    expect(e['profiles.0.tolerations.0.effect']).toMatch(/NoSchedule/);
  });
});

describe('validatePools', () => {
  it('requires one queue per pool, an existing profile and min ≤ max', () => {
    const e = validatePools(
      [pool(), pool({ name: 'cpu-2', profile: 'gone', min_workers: 4, max_workers: 2 })],
      [profile()],
    );
    expect(e['pools.1.queue']).toMatch(/already owns this queue/);
    expect(e['pools.1.profile']).toBe('Profile “gone” does not exist. Choose an existing pod profile.');
    expect(e['pools.1.max_workers']).toBe('Must be at least the minimum (4).');
  });

  it('validates the whole document in one call', () => {
    expect(validateConfig(toDraft(config()))).toEqual({});
  });
});

describe('draft conversion and dirtiness', () => {
  it('round-trips a node selector through editable rows, dropping blank rows', () => {
    const c = config({ profiles: [profile({ node_selector: { a: '1', b: '2' } })] });
    const d = toDraft(c);
    expect(d.profiles[0]!.node_selector).toEqual([{ key: 'a', value: '1' }, { key: 'b', value: '2' }]);
    d.profiles[0]!.node_selector.push({ key: '', value: '' });
    expect(fromDraft(d)).toEqual(c);
    expect(changedSections(c, d).size).toBe(0);
  });

  it('names exactly the sections that changed', () => {
    const c = config();
    const d = toDraft(c);
    d.scheduler = { ...d.scheduler, max_replicas: 8 };
    d.pools = [{ ...d.pools[0]!, max_workers: 20 }];
    expect([...changedSections(c, d)].sort()).toEqual(['pools', 'scheduler']);
  });

  it('does not call a policy saved before fair-share weights dirty', () => {
    const c = config();
    delete c.scheduler.fair_share_weight;
    const d = toDraft(c);
    d.scheduler = { ...d.scheduler, fair_share_weight: 1 };
    expect(changedSections(c, d).size).toBe(0);
  });

  it('counts a cleared number as a change', () => {
    const c = config();
    const d = toDraft(c);
    d.scheduler = { ...d.scheduler, min_replicas: Number.NaN };
    expect(changedSections(c, d).has('scheduler')).toBe(true);
  });
});

describe('helpers', () => {
  it('builds stable DOM ids from paths', () => {
    expect(fieldId('pools.0.max_workers')).toBe('infra-pools-0-max-workers');
  });

  it('picks the next free name', () => {
    expect(nextName('pool', ['pool-1', 'pool-3'])).toBe('pool-2');
  });

  it('orders health from worst to best', () => {
    expect(worst(['healthy', 'stale', 'degraded'])).toBe('degraded');
    expect(worst(['healthy', 'idle'])).toBe('idle');
  });
});

describe('component health', () => {
  const group = (ready: number, total: number, stale = 0) => ({ component: 'scheduler', summary: { ready, total, stale }, instances: [] });

  it('never calls an unreported component healthy', () => {
    expect(componentHealth(group(0, 0))).toMatchObject({ level: 'not_reported', label: 'Not reported' });
  });

  it('distinguishes healthy, degraded, down and stale', () => {
    expect(componentHealth(group(2, 2)).level).toBe('healthy');
    expect(componentHealth(group(1, 2)).level).toBe('degraded');
    expect(componentHealth(group(0, 2)).level).toBe('down');
    expect(componentHealth(group(0, 2, 2)).level).toBe('stale');
  });

  it('reports an older control plane as not reported, with the upgrade hint', () => {
    const h = controlPlaneHealth({ kind: 'unsupported', status: 404 }, null);
    expect(h.level).toBe('not_reported');
    expect(h.hint).toMatch(/Upgrade/);
  });

  it('rolls up to the worst component and names it', () => {
    const report = components({ 'dag-engine': [instance({ state: 'not_ready' })] });
    const h = controlPlaneHealth(ok(report), null);
    expect(h.level).toBe('down');
    expect(h.detail).toBe('5 of 6 components fully ready');
    expect(h.hint).toBe('Look at: DAG engine.');
  });

  it('separates loading from failed', () => {
    expect(controlPlaneHealth(undefined, null).level).toBe('loading');
    expect(controlPlaneHealth(undefined, new Error('x')).level).toBe('unavailable');
  });
});

describe('scheduler health', () => {
  it('takes the replicas, then lets autoscaling drift lower it', () => {
    const report = ok(components());
    expect(schedulerHealth(report, null, [cluster()]).level).toBe('healthy');
    const drifted = cluster();
    drifted.scheduler_autoscaling!.state = 'drift';
    expect(schedulerHealth(report, null, [drifted])).toMatchObject({ level: 'degraded', label: 'Drift' });
  });

  it('ignores clusters that report no autoscaling (older gateway)', () => {
    const older = cluster();
    delete older.scheduler_autoscaling;
    expect(schedulerHealth(ok(components()), null, [older]).level).toBe('healthy');
  });
});

describe('worker health', () => {
  it('counts a quiet ACTIVE worker as stale, not active', () => {
    const c = countWorkers(
      [worker(), worker({ id: 'w2', last_heartbeat_at: ago(60_000) }), worker({ id: 'w3', state: 'DRAINING' }), worker({ id: 'w4', state: 'DEAD' })],
      now,
    );
    expect(c).toEqual({ total: 4, active: 1, stale: 1, draining: 1, quarantined: 0, dead: 1 });
  });

  it('is honest about access, absence and outage', () => {
    expect(workerHealth(undefined, null, false, now).label).toBe('No access');
    expect(workerHealth([], null, true, now).level).toBe('not_reported');
    expect(workerHealth([worker({ state: 'DRAINING' })], null, true, now).level).toBe('down');
    expect(workerHealth([worker(), worker({ id: 'w2', last_heartbeat_at: ago(60_000) })], null, true, now).level).toBe('degraded');
    expect(workerHealth([worker()], null, true, now)).toMatchObject({ level: 'healthy', detail: '1 active · 0 draining · 0 stale' });
  });
});

describe('runner and cluster health', () => {
  const runners = (n: number): RouteResult<TaskRunnersReport> => ({
    kind: 'ok',
    data: { ...runnersReport(Array.from({ length: n }, (_, i) => runner({ attempt_id: `a${i}` }))), limit: 200 },
  });

  it('calls zero running tasks idle, not unhealthy', () => {
    expect(runnersHealth(runners(0), null)).toMatchObject({ level: 'idle', label: 'Idle' });
    expect(runnersHealth(runners(2), null)).toMatchObject({ level: 'healthy', label: '2 running', detail: '2 kubernetes' });
    expect(runnersHealth({ kind: 'unsupported', status: 404 }, null).level).toBe('not_reported');
  });

  it('judges controller freshness by the last report', () => {
    expect(controllerFreshness(null, now)).toBe('silent');
    expect(controllerFreshness(cluster().status, now)).toBe('reporting');
    expect(controllerFreshness({ ...cluster().status!, last_report_at: ago(10 * 60_000) }, now)).toBe('stale');
  });

  it('rolls clusters up honestly', () => {
    expect(clustersHealth([], null, now).level).toBe('not_reported');
    expect(clustersHealth([cluster({ status: null })], null, now).label).toBe('Not reporting');
    expect(clustersHealth([cluster(), cluster({ name: 'b', status: { ...cluster().status!, last_report_at: ago(600_000) } })], null, now).level).toBe('stale');
    expect(clustersHealth([cluster({ status: { ...cluster().status!, last_error: 'forbidden' } })], null, now).level).toBe('degraded');
    expect(clustersHealth([cluster()], null, now)).toMatchObject({ level: 'healthy', detail: '1 registered · 1 reporting · 0 in apply mode' });
  });
});

describe('poolLive', () => {
  it('rolls workers up by queue and joins controller-observed Deployments', () => {
    const c = cluster({
      bindings: [{ kubernetes_namespace: 'kgf-nexus-cpu', worker_deployment: 'cpu', pool: 'cpu', max_replicas: 10, max_scale_down_step: 1, scale_up_cooldown_seconds: 30, scale_down_cooldown_seconds: 300 }],
      observations: [
        { kind: 'WorkerDeployment', namespace: 'kgf-nexus-cpu', name: 'cpu', generation: 2, observed_generation: 2, spec_replicas: 3, ready_replicas: 2, ready: false, summary: {}, created_at: null, observed_at: ago(10_000) },
      ],
    });
    const [live] = poolLive(
      [pool()],
      new Set(['cpu']),
      [worker(), worker({ id: 'w2', active_attempts: 3 }), worker({ id: 'w3', queues: ['gpu'] })],
      [c],
      now,
    );
    expect(live!.counts.active).toBe(2);
    expect(live!.cpuMillis).toBe(8000);
    expect(live!.runningTasks).toBe(4);
    expect(live!.saved).toBe(true);
    expect(live!.bound).toBe(true);
    expect(live!.deployments).toEqual([
      { cluster: 'aks-prod', namespace: 'kgf-nexus-cpu', name: 'cpu', ready: 2, desired: 3, observedAt: expect.any(String) },
    ]);
  });

  it('reports running tasks as unknown when no worker reports them', () => {
    const [live] = poolLive([pool()], new Set(), [worker({ active_attempts: undefined })], [], now);
    expect(live!.runningTasks).toBeNull();
    expect(live!.saved).toBe(false);
  });

  it('finds workers outside every pool', () => {
    expect(unpooledWorkers([pool()], [worker(), worker({ id: 'x', queues: ['adhoc'] }), worker({ id: 'd', queues: ['adhoc'], state: 'DEAD' })]).map((w) => w.id)).toEqual(['x']);
  });
});

describe('runner filters', () => {
  const list = [runner(), runner({ attempt_id: 'b', executor: '', cluster: '', k8s_node: '' }), runner({ attempt_id: 'c', executor: 'docker', cluster: 'aks-dr' })];

  it('filters by each field, with "not reported" matching empty values only', () => {
    expect(filterRunners(list, NO_RUNNER_FILTER)).toHaveLength(3);
    expect(filterRunners(list, { ...NO_RUNNER_FILTER, executor: NOT_REPORTED }).map((r) => r.attempt_id)).toEqual(['b']);
    expect(filterRunners(list, { ...NO_RUNNER_FILTER, cluster: 'aks-dr' }).map((r) => r.attempt_id)).toEqual(['c']);
    expect(filterRunners(list, { ...NO_RUNNER_FILTER, node: 'aks-cpu-1234-vmss000002' }).map((r) => r.attempt_id)).toEqual(['att-1', 'c']);
  });

  it('lists distinct values with not-reported last', () => {
    expect(distinctValues(list, 'executor')).toEqual(['docker', 'kubernetes', '']);
  });
});
