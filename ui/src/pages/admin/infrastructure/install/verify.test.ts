// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest';
import { verifyInstall, verifyProgress, type VerifyInputs } from './verify';
import { ago, cluster, components, instance, runner, runnersReport, worker } from '../../../../test/infraFixtures';

const base = (over: Partial<VerifyInputs> = {}): VerifyInputs => ({
  clusterName: 'aks-prod',
  pools: [
    { name: 'cpu', queue: 'default' },
    { name: 'gpu', queue: 'gpu' },
  ],
  components: { kind: 'ok', data: components() },
  clusters: [cluster()],
  workers: [worker(), worker({ id: 'g', queues: ['gpu'] })],
  canViewWorkers: true,
  runners: { kind: 'ok', data: { ...runnersReport([runner()]), limit: 200 } },
  now: Date.now(),
  ...over,
});

const byId = (v: VerifyInputs) => Object.fromEntries(verifyInstall(v).map((i) => [i.id, i]));

describe('verifyInstall', () => {
  it('turns every item green from reports alone', () => {
    const items = verifyInstall(base());
    expect(items.map((i) => [i.id, i.status])).toEqual([
      ['control-plane', 'ok'],
      ['schedulers', 'ok'],
      ['controller', 'ok'],
      ['workers-cpu', 'ok'],
      ['workers-gpu', 'ok'],
      ['runner', 'ok'],
    ]);
    expect(verifyProgress(items)).toEqual({ ok: 6, total: 6, done: true });
    expect(items[1]!.detail).toBe('2 of 2 replicas ready.');
    expect(items[5]!.detail).toBe('Task extract running on pod nexus-tasks/extract-att-1-abcde.');
  });

  it('waits rather than guesses before anything has reported', () => {
    const items = byId(base({ components: undefined, clusters: [], workers: [], runners: { kind: 'ok', data: { ...runnersReport([]), limit: 200 } } }));
    expect(items['control-plane']!.status).toBe('pending');
    expect(items.controller!.detail).toBe('Cluster aks-prod is not registered yet.');
    expect(items['workers-cpu']!.status).toBe('pending');
    expect(items.runner!.status).toBe('pending');
  });

  it('only counts instances in this cluster', () => {
    const report = components({ 'event-service': [instance({ cluster: 'aks-other' })] });
    const items = byId(base({ components: { kind: 'ok', data: report } }));
    expect(items['control-plane']).toMatchObject({ status: 'pending', detail: 'Waiting for: Event service.' });
  });

  it('counts unlabelled instances from an older build, and says so', () => {
    const report = components();
    for (const g of report.components) for (const i of g.instances) i.cluster = '';
    expect(byId(base({ components: { kind: 'ok', data: report } }))['control-plane']!.detail).toContain('counting all');
  });

  it('flags instances that are not ready', () => {
    const report = components({ scheduler: [instance({ state: 'ready' }), instance({ instance: 's2', state: 'not_ready' })] });
    const items = byId(base({ components: { kind: 'ok', data: report } }));
    expect(items['control-plane']!.status).toBe('problem');
    expect(items.schedulers).toMatchObject({ status: 'problem', detail: '1 of 2 replicas ready.' });
  });

  it('cannot verify what an older control plane does not report', () => {
    const items = byId(base({ components: { kind: 'unsupported', status: 404 }, runners: { kind: 'unsupported', status: 404 } }));
    expect(items['control-plane']!.status).toBe('unknown');
    expect(items.schedulers!.status).toBe('unknown');
    expect(items.runner!.status).toBe('unknown');
  });

  it('follows the controller from registered to reporting to stale', () => {
    expect(byId(base({ clusters: [cluster({ status: null })] })).controller!.status).toBe('pending');
    const stale = cluster();
    stale.status!.last_report_at = ago(10 * 60_000);
    expect(byId(base({ clusters: [stale] })).controller!.status).toBe('problem');
    const mismatch = cluster({ expected_cluster_uid: 'uid-x' });
    mismatch.status!.identity_verified = false;
    expect(byId(base({ clusters: [mismatch] })).controller!.detail).toMatch(/UID does not match/);
  });

  it('distinguishes no workers from silent workers, and respects permissions', () => {
    const quiet = byId(base({ workers: [worker({ last_heartbeat_at: ago(60_000) })] }));
    expect(quiet['workers-cpu']!.status).toBe('problem');
    expect(quiet['workers-gpu']!.status).toBe('pending');
    expect(byId(base({ canViewWorkers: false }))['workers-cpu']!.status).toBe('unknown');
  });
});

describe('verifyInstall against the live registry', () => {
  it('ignores a replaced pod’s stale row once its replacement is ready', () => {
    const report = components({ scheduler: [instance({ state: 'ready' }), instance({ instance: 'old', state: 'stale' })] });
    const items = byId(base({ components: { kind: 'ok', data: report } }));
    expect(items['control-plane']!.status).toBe('ok');
    expect(items.schedulers).toMatchObject({ status: 'ok', detail: '1 of 1 replicas ready.' });
  });

  it('counts every instance, and says why, when the topology is hidden from this account', () => {
    const report = components();
    report.topology_visible = false;
    for (const g of report.components) for (const i of g.instances) i.cluster = '';
    expect(byId(base({ components: { kind: 'ok', data: report } }))['control-plane']).toMatchObject({
      status: 'ok',
      detail: expect.stringContaining('cluster names are hidden from this account'),
    });
  });
});
