// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The Install on AKS wizard, end to end against the fake gateway: step
 * navigation and focus, validation gating the generated commands, cluster
 * registration and binding, the once-only worker token, the live checklist,
 * and what view-only access can and cannot do.
 */

import { cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { InstallWizard } from './InstallWizard';
import { ADMIN_INFRA, renderInfra, serveInfra, status, VIEW_ONLY } from '../../../../test/infraHarness';
import { config, pool, profile } from '../../../../test/infraFixtures';

const saved = config({ pools: [pool(), pool({ name: 'gpu', queue: 'gpu', profile: 'gpu-a100' })], profiles: [profile(), profile({ name: 'gpu-a100', gpu_count: 1 })] });
const writeText = vi.fn().mockResolvedValue(undefined);

beforeEach(() => {
  writeText.mockClear();
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const render = (grants = ADMIN_INFRA) => renderInfra(<InstallWizard namespace="default" now={Date.now()} saved={saved} />, { grants });
const step = (name: RegExp) => fireEvent.click(within(screen.getByRole('navigation', { name: 'Install steps' })).getByRole('button', { name }));
const field = (name: string) => screen.getByRole('textbox', { name });

function fillRequired(cluster = 'aks-new') {
  step(/Configure/);
  fireEvent.change(field('Cluster name'), { target: { value: cluster } });
  fireEvent.change(field('Server host'), { target: { value: 'nexus-pg.postgres.database.azure.com' } });
  fireEvent.change(field('Storage account'), { target: { value: 'nexusresults01' } });
  fireEvent.change(field('Workload identity client ID'), { target: { value: '11111111-2222-3333-4444-555555555555' } });
}

describe('InstallWizard', () => {
  it('walks the steps, moving focus to each step’s heading', async () => {
    serveInfra();
    render();
    expect(screen.getByRole('heading', { name: 'Step 1 of 6: Prerequisites' })).toBeInTheDocument();
    expect(screen.getByText(/0 of 7 confirmed/)).toBeInTheDocument();
    fireEvent.click(screen.getAllByRole('checkbox')[0]!);
    expect(screen.getByText(/1 of 7 confirmed/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'docs/INSTALL-AKS.md' })).toHaveAttribute('href', 'https://github.com/nareshkhuriwal/kgf-nexus/blob/main/docs/INSTALL-AKS.md');
    fireEvent.click(screen.getByRole('button', { name: 'Next: Configure →' }));
    const heading = screen.getByRole('heading', { name: 'Step 2 of 6: Configure' });
    await waitFor(() => expect(heading).toHaveFocus());
    expect(within(screen.getByRole('navigation', { name: 'Install steps' })).getByRole('button', { name: /Configure/ })).toHaveAttribute('aria-current', 'step');
    fireEvent.click(screen.getByRole('button', { name: '← Back' }));
    expect(screen.getByRole('heading', { name: 'Step 1 of 6: Prerequisites' })).toBeInTheDocument();
    expect(screen.getAllByRole('checkbox')[0]).toBeChecked();
  });

  it('pre-fills pools from the saved policy and validates every field in place', () => {
    serveInfra();
    render();
    step(/Configure/);
    expect(screen.getAllByRole('textbox', { name: 'Name' }).map((i) => (i as HTMLInputElement).value)).toEqual(['cpu', 'gpu']);
    expect(screen.getAllByRole('textbox', { name: 'Namespace' }).map((i) => (i as HTMLInputElement).value)).toEqual(['nexus-pool-cpu', 'nexus-pool-gpu']);
    expect(screen.getAllByText('saved pool')).toHaveLength(2);
    expect(field('Cluster name')).toHaveAttribute('aria-invalid', 'true');
    fireEvent.change(field('Image tag'), { target: { value: 'latest' } });
    expect(screen.getByText(/Never "latest"/)).toBeInTheDocument();
    expect(within(screen.getByRole('navigation', { name: 'Install steps' })).getByText(/to fix/)).toBeInTheDocument();
  });

  it('generates commands only from a valid configuration, with nothing secret in them', () => {
    serveInfra();
    render();
    step(/Commands/);
    expect(screen.getByText(/Fix \d+ fields in step 2 first/)).toBeInTheDocument();
    fillRequired();
    step(/Commands/);
    const script = screen.getByRole('figure', { name: 'Everything, as one script' });
    const text = within(script).getByText(/#!\/usr\/bin\/env bash/).textContent!;
    expect(text).toContain('helm upgrade --install nexus oci://ghcr.io/nareshkhuriwal/charts/kgf-nexus');
    expect(text).toContain('--version 0.3.0');
    expect(text).toContain('--from-literal=worker-api-key="$NEXUS_WORKER_TOKEN"');
    expect(text).not.toMatch(/nxs_[a-z]+_[0-9a-f]{16}/);
    expect(screen.getByRole('button', { name: 'Download nexus-values.yaml' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Download nexus-pool-gpu-values.yaml' })).toBeInTheDocument();
    fireEvent.click(within(script).getByRole('button', { name: 'Copy Everything, as one script' }));
    expect(writeText).toHaveBeenCalledWith(text);
  });

  it('registers the cluster in plan mode', async () => {
    const { calls } = serveInfra();
    render();
    fillRequired('aks-new');
    step(/Register/);
    fireEvent.click(await screen.findByRole('button', { name: 'Register aks-new in plan mode' }));
    expect(await screen.findByText('Cluster aks-new registered in plan (dry-run) mode.')).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT')!;
    expect(put.path).toBe('/v1/namespaces/default/platform/clusters/aks-new');
    expect(put.body).toEqual({ provider: 'aks', provider_resource_id: '', expected_cluster_uid: '', control_plane_namespace: 'nexus', mode: 'plan', desired_revision: 0, bindings: [] });
  });

  it('binds the saved pools’ Deployments on an already registered cluster', async () => {
    const { calls } = serveInfra();
    render();
    fillRequired('aks-prod');
    step(/Register/);
    expect(await screen.findByText('Registered')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Bind 2 pool Deployments' }));
    await waitFor(() => expect(calls.some((c) => c.method === 'PUT')).toBe(true));
    const body = calls.find((c) => c.method === 'PUT')!.body as { bindings: { kubernetes_namespace: string; worker_deployment: string; pool: string }[] };
    expect(body.bindings.map((b) => [b.kubernetes_namespace, b.worker_deployment, b.pool])).toEqual([
      ['nexus-pool-cpu', 'cpu', 'cpu'],
      ['nexus-pool-gpu', 'gpu', 'gpu'],
    ]);
  });

  it('shows a minted worker token once, with a copy button and a warning', async () => {
    const { calls } = serveInfra();
    const { qc } = render();
    fillRequired('aks-new');
    step(/Worker token/);
    expect(field('Token name')).toHaveValue('nexus-agents-aks-new');
    fireEvent.click(screen.getByRole('button', { name: 'Mint worker token' }));
    const warning = await screen.findByRole('alert');
    expect(warning).toHaveTextContent('Copy this token now. It is shown once and cannot be shown again.');
    expect(screen.getByText('nxs_tok_SECRET123')).toBeInTheDocument();
    expect(calls.find((c) => c.method === 'POST')!.body).toEqual({ name: 'nexus-agents-aks-new', role: 'worker', scopes: ['worker_rpc'], expires_in_days: 90 });
    // The secret is held by the step alone, never by a query or mutation cache.
    await waitFor(() => expect(JSON.stringify(qc.getMutationCache().getAll().map((m) => m.state.data ?? null))).not.toContain('nxs_tok_SECRET123'));
    expect(JSON.stringify(qc.getQueryCache().getAll().map((q) => q.state.data ?? null))).not.toContain('nxs_tok_SECRET123');
    fireEvent.click(screen.getByRole('button', { name: 'Copy worker token' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('nxs_tok_SECRET123'));
    expect(await screen.findByText('Copied worker token to the clipboard.')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'I have stored it' }));
    expect(screen.queryByText('nxs_tok_SECRET123')).toBeNull();
    expect(screen.getByText(/was minted and its secret dismissed/)).toBeInTheDocument();
  });

  it('shows the server’s reason when minting is refused', async () => {
    serveInfra({ token: status(403, { error: 'ceiling_required', message: 'Set permissions (a ceiling within your own permissions)' }) });
    render();
    fillRequired();
    step(/Worker token/);
    fireEvent.click(screen.getByRole('button', { name: 'Mint worker token' }));
    expect(await screen.findByText('Could not mint the token')).toBeInTheDocument();
    expect(screen.getByText(/Set permissions \(a ceiling/)).toBeInTheDocument();
  });

  it('verifies the install from live reports', async () => {
    serveInfra();
    render();
    fillRequired('aks-prod');
    step(/Verify/);
    const list = await screen.findByRole('list', { name: 'Install checks' });
    expect(await screen.findByText('5 of 6 checks verified.')).toHaveAttribute('role', 'status');
    expect(within(list).getByText('Workers registered — pool gpu').closest('li')).toHaveTextContent('Waiting');
    expect(within(list).getByText('Control plane ready').closest('li')).toHaveTextContent('Verified');
  });

  it('lets view-only access read and generate, but not register or mint', async () => {
    serveInfra();
    render(VIEW_ONLY);
    fillRequired('aks-new');
    step(/Register/);
    expect(await screen.findByText(/Registering a cluster needs the infrastructure.configure permission/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Register aks-new/ })).toBeNull();
    step(/Worker token/);
    expect(screen.queryByRole('button', { name: 'Mint worker token' })).toBeNull();
    expect(screen.getByText(/needs both infrastructure.configure and tokens.manage/)).toBeInTheDocument();
    step(/Commands/);
    expect(screen.getByRole('figure', { name: 'Everything, as one script' })).toBeInTheDocument();
  });

  it('needs tokens.manage as well as configure to mint', () => {
    serveInfra();
    render(['infrastructure.view', 'infrastructure.configure']);
    step(/Worker token/);
    expect(screen.queryByRole('button', { name: 'Mint worker token' })).toBeNull();
  });
});
