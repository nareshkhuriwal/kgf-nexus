// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { cleanup, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SchedulerTab } from './SchedulerTab';
import { renderInfra, serveInfra, status } from '../../../test/infraHarness';
import { cluster, components, config, instance } from '../../../test/infraFixtures';
import type { ClusterView } from '../../../api/platform';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const saved = config().scheduler;

function render(savedRevision = 3) {
  return renderInfra(
    <SchedulerTab namespace="acme" now={Date.now()} policy={saved} savedPolicy={saved} savedRevision={savedRevision} errors={{}} canConfigure dirty={false} onChange={() => undefined} />,
  );
}

function withState(name: string, state: string, over: Partial<ClusterView> = {}): ClusterView {
  const c = cluster({ id: name, name, ...over });
  c.scheduler_autoscaling = { ...c.scheduler_autoscaling!, state };
  return c;
}

describe('SchedulerTab', () => {
  it('shows desired against observed per cluster, with each state named', async () => {
    const drift = withState('aks-dr', 'drift');
    drift.scheduler_autoscaling!.observed = { ...drift.scheduler_autoscaling!.observed!, max_replicas: 10, current_replicas: 7 };
    drift.scheduler_autoscaling!.last_error = 'forbidden: cannot patch hpa';
    serveInfra({ clusters: { clusters: [withState('aks-prod', 'in_sync'), drift, withState('aks-qa', 'plan_only'), withState('aks-new', 'not_observed', { mode: 'apply' })] } });
    render();
    const table = await screen.findByRole('table', { name: /Scheduler autoscaling, desired against observed/ });
    const row = (name: string) => within(table).getByRole('rowheader', { name }).closest('tr')!;
    expect(within(row('aks-prod')).getByText('In sync')).toBeInTheDocument();
    expect(within(row('aks-dr')).getByText('Drift')).toBeInTheDocument();
    expect(within(row('aks-dr')).getByText('2–10 replicas @ 65% CPU')).toBeInTheDocument();
    expect(within(row('aks-dr')).getByText(/replicas 7 now/)).toBeInTheDocument();
    expect(within(row('aks-dr')).getByText('Last error: forbidden: cannot patch hpa')).toBeInTheDocument();
    expect(within(row('aks-qa')).getByText('Plan only')).toBeInTheDocument();
    expect(within(row('aks-new')).getByText('Not observed')).toBeInTheDocument();
    expect(within(row('aks-new')).getByText('Apply')).toBeInTheDocument();
  });

  it('says when a controller has not picked up the saved revision', async () => {
    serveInfra({ clusters: { clusters: [withState('aks-prod', 'applying')] } });
    render(5);
    expect(await screen.findByText('Revision 5 not picked up yet')).toBeInTheDocument();
    expect(screen.getByText('Applying', { selector: '[data-state]' })).toBeInTheDocument();
  });

  it('labels clusters whose control plane does not report autoscaling', async () => {
    const older = cluster();
    delete older.scheduler_autoscaling;
    serveInfra({ clusters: { clusters: [older] } });
    render();
    expect(await screen.findByText('Not reported by this control plane version.')).toBeInTheDocument();
    expect(screen.getByText(/Saved: 2–6 replicas @ 65% CPU/)).toBeInTheDocument();
  });

  it('explains Plan vs Apply, and an empty registry', async () => {
    serveInfra({ clusters: { clusters: [] } });
    render();
    expect(await screen.findByText('No cluster is registered')).toBeInTheDocument();
    expect(screen.getByText(/Saving does not change the cluster by itself/)).toBeInTheDocument();
  });

  it('lists the live scheduler replicas with node, version, uptime and detail', async () => {
    serveInfra();
    render();
    expect(await screen.findByText('scheduler-5f7b-a1')).toBeInTheDocument();
    expect(screen.getByText('claims per minute')).toBeInTheDocument();
    expect(screen.getAllByText('v0.2.0').length).toBeGreaterThan(0);
    expect(screen.getAllByText('1h 0m').length).toBeGreaterThan(0);
  });

  it('flags a scheduler replica that is not ready, and an absent scheduler', async () => {
    serveInfra({ components: components({ scheduler: [instance({ instance: 's-1', state: 'not_ready' })] }) });
    render();
    expect(await screen.findByText('Not ready')).toBeInTheDocument();
    cleanup();
    vi.unstubAllGlobals();
    serveInfra({ components: components({ scheduler: [] }) });
    render();
    expect(await screen.findByText('No scheduler replica has reported')).toBeInTheDocument();
    cleanup();
    vi.unstubAllGlobals();
    serveInfra({ components: status(404) });
    render();
    expect(await screen.findByText('This control plane does not report component instances (upgrade)')).toBeInTheDocument();
  });
});
