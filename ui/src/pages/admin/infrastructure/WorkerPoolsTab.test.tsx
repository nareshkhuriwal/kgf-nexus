// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { cleanup, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { WorkerPoolsTab } from './WorkerPoolsTab';
import { toDraft, validateConfig } from './model';
import { ADMIN_INFRA, renderInfra, serveInfra } from '../../../test/infraHarness';
import { ago, cluster, config, pool, worker } from '../../../test/infraFixtures';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const saved = config({ pools: [pool(), pool({ name: 'gpu', queue: 'gpu', enabled: false })] });

function render(grants = ADMIN_INFRA) {
  const draft = toDraft(saved);
  return renderInfra(
    <WorkerPoolsTab namespace="acme" now={Date.now()} draft={draft} base={saved} errors={validateConfig(draft)} canConfigure={grants.includes('infrastructure.configure')} update={() => undefined} />,
    { grants },
  );
}

describe('WorkerPoolsTab live status', () => {
  it('rolls workers up by queue and shows what each pool really has', async () => {
    serveInfra({
      workers: {
        workers: [
          worker(),
          worker({ id: 'w2', hostname: 'agent-cpu-1', state: 'DRAINING', active_attempts: 2 }),
          worker({ id: 'w3', hostname: 'agent-cpu-2', last_heartbeat_at: ago(60_000) }),
          worker({ id: 'w4', hostname: 'adhoc-1', queues: ['adhoc'] }),
        ],
      },
      clusters: {
        clusters: [
          cluster({
            bindings: [{ kubernetes_namespace: 'nexus-pool-cpu', worker_deployment: 'cpu', pool: 'cpu', max_replicas: 10, max_scale_down_step: 1, scale_up_cooldown_seconds: 30, scale_down_cooldown_seconds: 300 }],
            observations: [{ kind: 'WorkerDeployment', namespace: 'nexus-pool-cpu', name: 'cpu', generation: 1, observed_generation: 1, spec_replicas: 3, ready_replicas: 2, ready: false, summary: {}, created_at: null, observed_at: ago(10_000) }],
          }),
        ],
      },
    });
    render();
    const table = await screen.findByRole('table', { name: 'Live status of each saved worker pool' });
    const cpu = within(table).getByRole('rowheader', { name: /^cpu/ }).closest('tr')!;
    expect(await within(cpu).findByText('1 active')).toBeInTheDocument();
    expect(within(cpu).getByText('1 draining')).toBeInTheDocument();
    expect(within(cpu).getByText('1 stale')).toBeInTheDocument();
    expect(within(cpu).getByText('aks-prod: nexus-pool-cpu/cpu')).toBeInTheDocument();
    expect(within(cpu).getByText(/2 \/ 3 ready/)).toBeInTheDocument();
    expect(within(cpu).getByText('3')).toBeInTheDocument();
    const gpu = within(table).getByRole('rowheader', { name: /^gpu/ }).closest('tr')!;
    expect(within(gpu).getByText('no worker on this queue')).toBeInTheDocument();
    expect(within(gpu).getByText('not bound to a cluster')).toBeInTheDocument();
    expect(within(gpu).getByText(/disabled in plans/)).toBeInTheDocument();
    expect(screen.getByText(/1 worker serves queues no pool owns \(adhoc-1\)/)).toBeInTheDocument();
  });

  it('reports running tasks as not reported when no worker says', async () => {
    serveInfra({ workers: { workers: [worker({ active_attempts: undefined })] } });
    render();
    const table = await screen.findByRole('table', { name: 'Live status of each saved worker pool' });
    const cpu = within(table).getByRole('rowheader', { name: /^cpu/ }).closest('tr')!;
    expect(await within(cpu).findByText('1 active')).toBeInTheDocument();
    expect(within(cpu).getAllByText('not reported').length).toBeGreaterThan(0);
  });

  it('shows Deployments without worker counts when workers.view is missing', async () => {
    const { calls } = serveInfra();
    render(['infrastructure.view']);
    expect(await screen.findByText('Worker counts need the workers.view permission')).toBeInTheDocument();
    expect(screen.getAllByText('needs workers.view').length).toBe(2);
    expect(calls.some((c) => c.path.endsWith('/workers'))).toBe(false);
  });
});
