// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { cleanup, fireEvent, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { OverviewTab } from './OverviewTab';
import { renderInfra, serveInfra, status } from '../../../test/infraHarness';
import { ago, cluster, components, instance, worker } from '../../../test/infraFixtures';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const render = (grants?: string[]) => {
  const onOpen = vi.fn();
  renderInfra(<OverviewTab namespace="acme" now={Date.now()} onOpen={onOpen} />, grants ? { grants } : {});
  return onOpen;
};
const tile = (name: string) => screen.getByRole('article', { name });

describe('OverviewTab', () => {
  it('names the unhealthy component and says what to do', async () => {
    serveInfra({ components: components({ 'dag-engine': [instance({ state: 'not_ready' }), instance({ instance: 'dag-2' })] }) });
    render();
    const services = tile('Services');
    expect(await within(services).findByText('Look at: DAG engine.')).toBeInTheDocument();
    expect(within(services).getByText('What to do:')).toBeInTheDocument();
    expect(within(services).getByText('1/2')).toBeInTheDocument();
    expect(within(services).getAllByText('Degraded').length).toBeGreaterThan(0);
  });

  it('flags quiet and dead workers without counting them as active', async () => {
    serveInfra({ workers: { workers: [worker(), worker({ id: 'w2', last_heartbeat_at: ago(60_000) }), worker({ id: 'w3', state: 'DEAD' })] } });
    render();
    const workers = tile('Worker agents');
    expect(await within(workers).findByText('1 active · 0 draining · 1 stale · 1 dead')).toBeInTheDocument();
    expect(within(workers).getByText('active of 3')).toBeInTheDocument();
  });

  it('says when worker access is missing instead of showing zero', async () => {
    const { calls } = serveInfra();
    render(['infrastructure.view']);
    expect(await within(tile('Worker agents')).findByText('needs workers.view')).toBeInTheDocument();
    expect(calls.some((c) => c.path.endsWith('/workers') || c.path.endsWith('/capacity'))).toBe(false);
  });

  it('points to the installer when no cluster is registered, and to Clusters when one is', async () => {
    serveInfra({ clusters: { clusters: [] } });
    const onOpen = render();
    const clusters = tile('Clusters');
    expect(await within(clusters).findByText('None registered')).toBeInTheDocument();
    fireEvent.click(within(clusters).getByRole('button', { name: 'Install on AKS →' }));
    expect(onOpen).toHaveBeenCalledWith('install');
    cleanup();
    vi.unstubAllGlobals();
    serveInfra({ clusters: { clusters: [cluster(), cluster({ id: 'c2', name: 'aks-dr', status: null })] } });
    const open2 = render();
    const list = await within(tile('Clusters')).findByRole('list', { name: 'Cluster controllers' });
    expect(within(list).getByText('Not reporting')).toBeInTheDocument();
    fireEvent.click(within(tile('Clusters')).getByRole('button', { name: 'Clusters →' }));
    expect(open2).toHaveBeenCalledWith('clusters');
  });

  it('separates a failed load from an unsupported route', async () => {
    serveInfra({ components: status(403, { error: 'forbidden', message: 'no' }), runners: status(404) });
    render();
    expect(await screen.findByText('Could not load control-plane instances')).toBeInTheDocument();
    expect(within(tile('Task runners')).getAllByText('Not reported').length).toBeGreaterThan(0);
    expect(within(tile('Services')).getAllByText('Unavailable').length).toBeGreaterThan(0);
  });

  it('tells apart "no instance has reported yet" from "not supported"', async () => {
    serveInfra({ components: components(Object.fromEntries(['api-gateway', 'workflow-manager', 'dag-engine', 'scheduler', 'resource-manager', 'event-service'].map((c) => [c, []]))) });
    render();
    expect(await screen.findByText('No instance has reported yet')).toBeInTheDocument();
    expect(within(tile('Services')).getAllByText('Not reported').length).toBeGreaterThan(0);
  });
});

describe('OverviewTab topology and history', () => {
  it('shows counts and health when the topology is hidden, saying who can see pods and nodes', async () => {
    const report = components();
    report.topology_visible = false;
    for (const g of report.components)
      g.instances.forEach((i, n) => Object.assign(i, { instance: `${g.component}-d7fd2b1c942${n}`, pod_namespace: '', node: '', cluster: '', detail: { readiness: 'ok', heartbeat_interval_seconds: 10 } }));
    serveInfra({ components: report });
    render();
    expect(await screen.findByText(/Pod, node and cluster are shown to platform operators/)).toBeInTheDocument();
    const row = screen.getByText('scheduler-d7fd2b1c9420').closest('tr')!;
    expect(within(row).getAllByText('hidden')).toHaveLength(2);
    expect(within(row).queryByText('not reported')).toBeNull();
    expect(within(tile('Services')).getAllByText('Healthy').length).toBeGreaterThan(0);
  });

  it('lists replaced pods as history, not as faults', async () => {
    serveInfra({
      components: components({
        scheduler: [instance({ instance: 'scheduler-new', state: 'ready' }), instance({ instance: 'scheduler-old', state: 'stale', last_seen_at: ago(3_600_000) })],
      }),
    });
    render();
    expect(await screen.findByText('1 previous instance — replaced pods that stopped reporting (kept up to 24h)')).toBeInTheDocument();
    expect(screen.getByText('scheduler-old').closest('tr')).toBeNull();
    expect(within(tile('Scheduler')).getByText('/ 1 replicas ready')).toBeInTheDocument();
    expect(within(tile('Services')).queryByText('Degraded')).toBeNull();
  });
});
