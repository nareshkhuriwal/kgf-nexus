// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, expect, it, vi } from 'vitest';
import { ClusterControllerPanel, type Cluster } from './ClusterControllerPanel';
import { PermissionContext } from './Permissions';
import { authRequest } from '../api/auth';
vi.mock('../api/auth', () => ({ authRequest: vi.fn() }));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

const recent = () => new Date(Date.now() - 20_000).toISOString();
function cluster(over: Partial<Cluster> = {}): Cluster {
  return {
    id: 'c1', name: 'aks-dev', provider: 'aks', provider_resource_id: '', expected_cluster_uid: 'uid-1', control_plane_namespace: 'kgf-nexus',
    mode: 'plan', desired_revision: 3, updated_at: recent(), updated_by: 'admin',
    bindings: [{ kubernetes_namespace: 'nexus-workers', worker_deployment: 'workers-cpu', pool: 'cpu', max_replicas: 10, max_scale_down_step: 1, scale_up_cooldown_seconds: 30, scale_down_cooldown_seconds: 300 }],
    status: { observed_revision: 2, observed_cluster_uid: 'uid-1', identity_verified: true, kubernetes_version: 'v1.34.3', controller_identity: 'controller-0', effective_mode: 'plan', effective_mode_reason: 'registry mode is plan (dry-run)', last_report_at: recent(), last_error: '', last_error_at: null },
    observations: [
      { kind: 'WorkerDeployment', namespace: 'nexus-workers', name: 'workers-cpu', generation: 4, observed_generation: 4, spec_replicas: 3, ready_replicas: 2, ready: false, summary: {}, created_at: null, observed_at: recent() },
      { kind: 'HorizontalPodAutoscaler', namespace: 'kgf-nexus', name: 'scheduler', generation: 1, observed_generation: 1, spec_replicas: 2, ready_replicas: 2, ready: true, summary: { min_replicas: 2, max_replicas: 6, target_cpu_percent: 60, current_cpu_percent: null, scaling_active: true }, created_at: null, observed_at: recent() },
    ],
    scaling: [], events: [{ id: 1, kubernetes_namespace: 'nexus-workers', worker_deployment: 'workers-cpu', action: 'plan', mode: 'plan', applied: false, from_replicas: 3, to_replicas: 5, reason: 'pending resource demand', actor: 'controller-0', occurred_at: recent() }],
    errors: [], jobs_shown: 0, ...over,
  };
}

function setup(clusters: Cluster[], grants = ['infrastructure.view', 'infrastructure.configure']) {
  vi.mocked(authRequest).mockImplementation(async (_path, init) => (init?.method === 'PUT' ? { desired_revision: 4 } : { clusters }) as never);
  render(<PermissionContext.Provider value={grants}><QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <ClusterControllerPanel namespace="acme" pools={['cpu']} scheduler={{ min_replicas: 1, max_replicas: 4, target_cpu_percent: 65 }} />
  </QueryClientProvider></PermissionContext.Provider>);
}

it('says plainly when no cluster is registered', async () => {
  setup([]);
  expect(await screen.findByText(/No cluster is registered for this namespace/)).toBeTruthy();
});

it('separates desired from observed and labels what was not reported', async () => {
  setup([cluster()]);
  expect(await screen.findByText(/Desired revision 3 · observed 2/)).toBeTruthy();
  expect(screen.getByText(/not yet reconciled/)).toBeTruthy();
  expect(screen.getByText('2 / 3')).toBeTruthy();
  expect(screen.getByText('No nodes reported.')).toBeTruthy();
  expect(screen.getByText(/Differs from saved policy 1–4 @ 65%/)).toBeTruthy();
  expect(screen.getByText('not reported / 60%')).toBeTruthy();
  expect(screen.getByText(/plan \(plan only\)/)).toBeTruthy();
});

it('never presents a silent controller as live', async () => {
  setup([cluster({ status: null })]);
  expect(await screen.findByText('No controller has reported')).toBeTruthy();
  expect(screen.getByText('Controller: not reporting')).toBeTruthy();
  expect(screen.getByText(/observed not reported/)).toBeTruthy();
  const stale = cluster();
  stale.status!.last_report_at = new Date(Date.now() - 10 * 60_000).toISOString();
  cleanup();
  setup([stale]);
  expect(await screen.findByText(/— stale/)).toBeTruthy();
});

it('requires an explicit confirmation before enabling apply', async () => {
  setup([cluster()]);
  fireEvent.click(await screen.findByRole('button', { name: 'Enable apply…' }));
  expect(authRequest).toHaveBeenCalledTimes(1);
  fireEvent.click(screen.getByRole('button', { name: 'Confirm apply mode' }));
  await waitFor(() => expect(vi.mocked(authRequest).mock.calls.some(c => c[1]?.method === 'PUT')).toBe(true));
  const put = vi.mocked(authRequest).mock.calls.find(c => c[1]?.method === 'PUT')!;
  expect(put[0]).toBe('/v1/namespaces/acme/platform/clusters/aks-dev');
  expect(JSON.parse(put[1]!.body as string)).toMatchObject({ mode: 'apply', desired_revision: 3, expected_cluster_uid: 'uid-1' });
  expect(await screen.findByText(/applied only once the controller reports observing it/)).toBeTruthy();
});

it('hides every control from read-only infrastructure access', async () => {
  setup([cluster()], ['infrastructure.view']);
  await screen.findByText(/Desired revision 3/);
  expect(screen.queryByRole('button', { name: 'Enable apply…' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Unbind' })).toBeNull();
  expect(screen.queryByText('Register a cluster')).toBeNull();
});
