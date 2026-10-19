// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ClusterControllerPanel } from './ClusterControllerPanel';
import { ADMIN_INFRA, renderInfra, serveInfra, status, VIEW_ONLY } from '../../../test/infraHarness';
import { ago, cluster } from '../../../test/infraFixtures';
import type { ClusterView } from '../../../api/platform';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function withBindings(over: Partial<ClusterView> = {}): ClusterView {
  return cluster({
    expected_cluster_uid: 'uid-1',
    desired_revision: 3,
    bindings: [{ kubernetes_namespace: 'nexus-workers', worker_deployment: 'workers-cpu', pool: 'cpu', max_replicas: 10, max_scale_down_step: 1, scale_up_cooldown_seconds: 30, scale_down_cooldown_seconds: 300 }],
    status: { ...cluster().status!, observed_revision: 2 },
    observations: [
      { kind: 'WorkerDeployment', namespace: 'nexus-workers', name: 'workers-cpu', generation: 4, observed_generation: 4, spec_replicas: 3, ready_replicas: 2, ready: false, summary: {}, created_at: null, observed_at: ago(20_000) },
      { kind: 'HorizontalPodAutoscaler', namespace: 'kgf-nexus', name: 'scheduler', generation: 1, observed_generation: 1, spec_replicas: 2, ready_replicas: 2, ready: true, summary: { min_replicas: 2, max_replicas: 6, target_cpu_percent: 60, current_cpu_percent: null, scaling_active: true }, created_at: null, observed_at: ago(20_000) },
    ],
    events: [{ id: 1, kubernetes_namespace: 'nexus-workers', worker_deployment: 'workers-cpu', action: 'plan', mode: 'plan', applied: false, from_replicas: 3, to_replicas: 5, reason: 'pending resource demand', actor: 'controller-0', occurred_at: ago(20_000) }],
    ...over,
  });
}

function setup(clusters: ClusterView[], grants = ADMIN_INFRA, extra = {}) {
  const harness = serveInfra({ clusters: { clusters }, ...extra });
  renderInfra(<ClusterControllerPanel namespace="acme" savedPools={['cpu', 'gpu']} unsavedPools={['draft-pool']} scheduler={{ min_replicas: 1, max_replicas: 4, target_cpu_percent: 65 }} />, { grants });
  return harness;
}

describe('ClusterControllerPanel', () => {
  it('says plainly when no cluster is registered', async () => {
    setup([]);
    expect(await screen.findByText(/No cluster is registered for this namespace/)).toBeInTheDocument();
  });

  it('separates desired from observed and labels what was not reported', async () => {
    setup([withBindings()]);
    expect(await screen.findByText(/Desired revision 3 · observed 2/)).toBeInTheDocument();
    expect(screen.getByText(/not yet reconciled/)).toBeInTheDocument();
    expect(screen.getByText('2 / 3')).toBeInTheDocument();
    expect(screen.getByText('No nodes reported.')).toBeInTheDocument();
    expect(screen.getByText(/Differs from saved policy 1–4 @ 65%/)).toBeInTheDocument();
    expect(screen.getByText('not reported / 60%')).toBeInTheDocument();
    expect(screen.getByText(/plan \(plan only\)/)).toBeInTheDocument();
  });

  it('shows labels, headroom and the scheduler autoscaling state (C5)', async () => {
    setup([withBindings()]);
    expect(await screen.findByText('nexus.io/region=westeurope')).toBeInTheDocument();
    const headroom = screen.getByRole('table', { name: 'Quota headroom per task namespace' });
    const row = within(headroom).getByRole('rowheader', { name: 'nexus-tasks' }).closest('tr')!;
    expect(within(row).getByText('6 cpu')).toBeInTheDocument();
    expect(within(row).getByText('24.0 GiB')).toBeInTheDocument();
    expect(within(row).getByText('unlimited')).toBeInTheDocument();
    expect(within(row).getByText('nvidia.com/mig-1g.10gb: 2')).toBeInTheDocument();
    expect(screen.getAllByText('In sync').length).toBeGreaterThan(0);
    expect(screen.getByText(/Desired 2–6 @ 65% \(revision 3\)/)).toBeInTheDocument();
    expect(within(screen.getByRole('list', { name: 'Scheduler HPAs in aks-prod' })).getByText('HPA scheduler')).toBeInTheDocument();
  });

  it('says when an older control plane does not report labels, headroom or autoscaling', async () => {
    const older = withBindings();
    delete older.labels;
    delete older.headroom;
    delete older.scheduler_autoscaling;
    setup([older]);
    expect(await screen.findAllByText('not reported by this control plane')).toHaveLength(2);
    expect(screen.getByText(/does not report scheduler autoscaling state/)).toBeInTheDocument();
  });

  it('never presents a silent or stale controller as live', async () => {
    setup([withBindings({ status: null })]);
    expect(await screen.findByText('No controller has reported')).toBeInTheDocument();
    expect(screen.getByText('Controller: not reporting')).toBeInTheDocument();
    expect(screen.getByText(/observed not reported/)).toBeInTheDocument();
    cleanup();
    vi.unstubAllGlobals();
    const stale = withBindings();
    stale.status!.last_report_at = ago(10 * 60_000);
    setup([stale]);
    expect(await screen.findByText(/— stale/)).toBeInTheDocument();
    expect(screen.getByText('Stale')).toBeInTheDocument();
  });

  it('requires an explicit confirmation before enabling apply', async () => {
    const { calls } = setup([withBindings()]);
    fireEvent.click(await screen.findByRole('button', { name: 'Enable apply…' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Allow the controller to change aks-prod?' });
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Confirm apply mode' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    const put = calls.find((c) => c.method === 'PUT')!;
    expect(put.path).toBe('/v1/namespaces/acme/platform/clusters/aks-prod');
    expect(put.body).toEqual({
      provider: 'aks',
      provider_resource_id: '',
      expected_cluster_uid: 'uid-1',
      control_plane_namespace: 'kgf-nexus',
      mode: 'apply',
      desired_revision: 3,
      bindings: withBindings().bindings,
    });
    expect(await screen.findByText(/applied only once the controller reports observing it/)).toBeInTheDocument();
  });

  it('cannot enable apply without the expected cluster UID', async () => {
    setup([withBindings({ expected_cluster_uid: '' })]);
    expect(await screen.findByRole('button', { name: 'Enable apply…' })).toBeDisabled();
  });

  it('binds only saved pools and validates the binding before sending it', async () => {
    const { calls } = setup([withBindings()]);
    const pool = await screen.findByRole('combobox', { name: 'Worker pool' });
    const options = within(pool).getAllByRole('option');
    expect(options.map((o) => [o.textContent, (o as HTMLOptionElement).disabled])).toEqual([
      ['cpu (already bound here)', true],
      ['gpu', false],
      ['draft-pool (unsaved — save the configuration first)', true],
    ]);
    expect(pool).toHaveValue('gpu');
    fireEvent.click(screen.getByRole('button', { name: 'Bind Deployment' }));
    expect(screen.getByText('A Kubernetes namespace (DNS label).')).toBeInTheDocument();
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);
    fireEvent.change(screen.getByRole('textbox', { name: 'Kubernetes namespace' }), { target: { value: 'nexus-pool-gpu' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'Worker Deployment' }), { target: { value: 'gpu' } });
    fireEvent.click(screen.getByRole('button', { name: 'Bind Deployment' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    const body = calls.find((c) => c.method === 'PUT')!.body as { bindings: { pool: string }[] };
    expect(body.bindings.map((b) => b.pool)).toEqual(['cpu', 'gpu']);
  });

  it('explains a conflicting registry change', async () => {
    setup([withBindings()], ADMIN_INFRA, { putCluster: status(409, { error: 'revision_conflict', message: 'stale' }) });
    fireEvent.click(await screen.findByRole('button', { name: 'Unbind nexus-workers/workers-cpu' }));
    expect(await screen.findByText(/The cluster changed since this page loaded/)).toBeInTheDocument();
  });

  it('confirms before unregistering, and not at all in apply mode', async () => {
    const { calls } = setup([withBindings()]);
    fireEvent.click(await screen.findByRole('button', { name: 'Unregister cluster' }));
    fireEvent.click(within(screen.getByRole('alertdialog', { name: 'Unregister aks-prod?' })).getByRole('button', { name: 'Unregister' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'DELETE')).toBe(true));
    expect(calls.find((c) => c.method === 'DELETE')!.search).toBe('?revision=3');
    cleanup();
    vi.unstubAllGlobals();
    setup([withBindings({ mode: 'apply' })]);
    expect(await screen.findByRole('button', { name: 'Unregister cluster' })).toBeDisabled();
  });

  it('validates a new registration and starts it in plan mode', async () => {
    const { calls } = setup([]);
    fireEvent.click(await screen.findByRole('button', { name: 'Register cluster' }));
    expect(screen.getByText(/A DNS label: it must equal/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: 'Cluster name' }), { target: { value: 'aks-dr' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'Provider resource ID (optional)' }), { target: { value: 'not-an-arm-id' } });
    fireEvent.click(screen.getByRole('button', { name: 'Register cluster' }));
    expect(screen.getByText('The managedClusters ARM ID.')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: 'Provider resource ID (optional)' }), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: 'Register cluster' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    expect(calls.find((c) => c.method === 'PUT')!.body).toMatchObject({ mode: 'plan', desired_revision: 0, bindings: [] });
  });

  it('hides every control from read-only infrastructure access', async () => {
    setup([withBindings()], VIEW_ONLY);
    await screen.findByText(/Desired revision 3/);
    expect(screen.queryByRole('button', { name: 'Enable apply…' })).toBeNull();
    expect(screen.queryByRole('button', { name: /Unbind/ })).toBeNull();
    expect(screen.queryByRole('region', { name: 'Register a cluster' })).toBeNull();
    expect(screen.queryByRole('combobox', { name: 'Worker pool' })).toBeNull();
  });
});

describe('ClusterControllerPanel scheduler HPAs', () => {
  it('lists each HPA target, flags one the controller does not own, and describes HPA events', async () => {
    const c = withBindings();
    const a = c.scheduler_autoscaling!;
    a.state = 'drift';
    a.targets = [
      { ...a.targets[0]!, state: 'drift', managed: false, reason: 'leaves HPA scheduler to Helm' },
      { ...a.targets[0]!, observed: { ...a.targets[0]!.observed!, hpa_name: 'scheduler-gpu' }, state: 'in_sync' },
    ];
    c.events = [
      { id: 9, kubernetes_namespace: 'kgf-nexus', worker_deployment: 'hpa/scheduler', action: 'hpa_update', mode: 'apply', applied: true, from_replicas: null, to_replicas: null, reason: 'bounded step', actor: 'controller-0', occurred_at: ago(30_000), detail: { before: { min_replicas: 1, max_replicas: 4 }, after: { min_replicas: 2, max_replicas: 6 } } },
    ];
    setup([c]);
    const list = await screen.findByRole('list', { name: 'Scheduler HPAs in aks-prod' });
    expect(within(list).getByText('Not controller-owned')).toBeInTheDocument();
    expect(within(list).getByText('leaves HPA scheduler to Helm')).toBeInTheDocument();
    expect(within(list).getByText('HPA scheduler-gpu')).toBeInTheDocument();
    expect(screen.getByText(/2 HPAs, worst shown/)).toBeInTheDocument();
    const event = screen.getByText('bounded step').closest('tr')!;
    expect(within(event).getByText('HPA scheduler')).toBeInTheDocument();
    expect(within(event).getByText('min replicas 1→2, max replicas 4→6')).toBeInTheDocument();
    expect(within(event).getByText('hpa_update (applied)')).toBeInTheDocument();
  });
});
