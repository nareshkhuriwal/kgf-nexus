// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The Cluster & scaling page as a whole: live status from reports only,
 * honest "not reported" states, the tab set, permission gating, and the
 * draft → validate → save flow (state-based, compare-and-set).
 *
 * The gateway is faked at `fetch` (test/infraHarness.tsx), so the typed
 * client, its normalisers and the hooks all run for real.
 */

import { act, cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { InfrastructurePage } from './InfrastructurePage';
import { ADMIN_INFRA, renderInfra, serveInfra, status, VIEW_ONLY } from '../../test/infraHarness';
import { config, pool, profile } from '../../test/infraFixtures';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const page = (opts: Parameters<typeof renderInfra>[1] = {}) => renderInfra(<InfrastructurePage namespace="default" canAdmin={(opts.grants ?? ADMIN_INFRA).includes('infrastructure.view')} />, opts);
const tab = (name: RegExp | string) => screen.getByRole('tab', { name });
const location = () => screen.getByTestId('location').textContent;

describe('access', () => {
  it('refuses without infrastructure.view', () => {
    serveInfra();
    page({ grants: [] });
    expect(screen.getByText(/Not permitted: view infrastructure/)).toBeInTheDocument();
  });

  it('is read-only without infrastructure.configure, and never fetches workers without workers.view', async () => {
    const { calls } = serveInfra();
    page({ grants: VIEW_ONLY, route: '/admin/infrastructure?tab=scheduler' });
    const min = await screen.findByRole('spinbutton', { name: 'Minimum replicas' });
    expect(min).toBeDisabled();
    expect(screen.queryByRole('button', { name: 'Save configuration' })).toBeNull();
    expect(screen.getByText(/You have read-only access/)).toBeInTheDocument();
    fireEvent.click(tab('Worker pools'));
    expect(await screen.findByRole('heading', { name: 'Pool policies' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add worker pool' })).toBeNull();
    expect(screen.getByText(/Worker counts need the workers.view permission/)).toBeInTheDocument();
    expect(calls.some((c) => c.path.endsWith('/workers'))).toBe(false);
  });
});

describe('live status', () => {
  it('summarises every layer from what the platform reports', async () => {
    serveInfra();
    page();
    const strip = await screen.findByRole('list', { name: 'Live status' });
    await waitFor(() => expect(within(strip).getAllByText('Healthy')).toHaveLength(3));
    expect(within(strip).getByText('1 running')).toBeInTheDocument();
    const services = screen.getByRole('article', { name: 'Services' });
    expect(within(services).getByText('6')).toBeInTheDocument();
    expect(within(services).getByText('/ 6 ready')).toBeInTheDocument();
    const scheduler = screen.getByRole('article', { name: 'Scheduler' });
    expect(within(scheduler).getByText('/ 2 replicas ready')).toBeInTheDocument();
    expect(within(scheduler).getByText('In sync')).toBeInTheDocument();
    expect(within(screen.getByRole('article', { name: 'Worker agents' })).getByText('2 tasks waiting for capacity')).toBeInTheDocument();
    expect(await screen.findByText('scheduler-5f7b-a1')).toBeInTheDocument();
  });

  it('says plainly when the control plane does not report instances (404), without an error wall', async () => {
    serveInfra({ components: status(404), runners: status(404) });
    page();
    expect(await screen.findByText('This control plane does not report component instances (upgrade)')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    const tile = screen.getByRole('article', { name: 'Services' });
    expect(within(tile).getAllByText('Not reported').length).toBeGreaterThan(0);
    expect(within(tile).getByText(/What to do:/)).toBeInTheDocument();
  });

  it('keeps the last report on screen, marked stale, when a refresh fails', async () => {
    const { server } = serveInfra();
    page();
    expect(await screen.findByText('scheduler-5f7b-a1')).toBeInTheDocument();
    server.components = status(500);
    fireEvent.click(screen.getByRole('button', { name: 'Refresh live status' }));
    expect(await screen.findByText('Showing stale data — the last refresh failed')).toBeInTheDocument();
    expect(screen.getByText('scheduler-5f7b-a1')).toBeInTheDocument();
  });
});

describe('tabs', () => {
  it('deep-links, and moves with the arrow keys', async () => {
    serveInfra();
    page({ route: '/admin/infrastructure?tab=scheduler' });
    expect(tab('Scheduler')).toHaveAttribute('aria-selected', 'true');
    expect(await screen.findByRole('tabpanel', { name: 'Scheduler' })).toBeInTheDocument();
    fireEvent.keyDown(tab('Scheduler'), { key: 'ArrowRight' });
    expect(tab('Worker pools')).toHaveAttribute('aria-selected', 'true');
    expect(tab('Worker pools')).toHaveFocus();
    expect(location()).toBe('/admin/infrastructure?tab=pools');
    fireEvent.keyDown(tab('Worker pools'), { key: 'Home' });
    expect(tab('Overview')).toHaveAttribute('aria-selected', 'true');
    expect(location()).toBe('/admin/infrastructure');
  });

  it('falls back to the overview for an unknown tab', () => {
    serveInfra();
    page({ route: '/admin/infrastructure?tab=nope' });
    expect(tab('Overview')).toHaveAttribute('aria-selected', 'true');
  });
});

describe('editing and saving', () => {
  it('validates from state, points at the field, and saves compare-and-set without touching quotas', async () => {
    const { calls } = serveInfra({ config: { ...config(), quotas: { locked: true } } });
    page({ route: '/admin/infrastructure?tab=scheduler' });
    const max = await screen.findByRole('spinbutton', { name: 'Maximum replicas' });
    expect(screen.queryByRole('button', { name: 'Save configuration' })).toBeNull();
    fireEvent.change(max, { target: { value: '1' } });
    expect(screen.getByText('Must be at least the minimum (2).')).toBeInTheDocument();
    expect(max).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByText(/Unsaved: scheduler policy/)).toBeInTheDocument();
    expect(tab(/Scheduler/)).toHaveTextContent('(unsaved changes)');
    expect(tab(/Scheduler/)).toHaveTextContent('(1 field to fix)');

    fireEvent.click(tab('Overview'));
    fireEvent.click(screen.getByRole('button', { name: 'Save configuration' }));
    expect(await screen.findByText('Fix 1 field before saving')).toBeInTheDocument();
    expect(tab(/Scheduler/)).toHaveAttribute('aria-selected', 'true');
    await waitFor(() => expect(screen.getByRole('spinbutton', { name: 'Maximum replicas' })).toHaveFocus());
    expect(calls.some((c) => c.method === 'PUT')).toBe(false);

    fireEvent.change(screen.getByRole('spinbutton', { name: 'Maximum replicas' }), { target: { value: '8' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save configuration' }));
    expect(await screen.findByText(/Revision 4 saved and audited/)).toBeInTheDocument();
    const put = calls.find((c) => c.method === 'PUT')!;
    expect(put.path).toBe('/v1/namespaces/default/platform/config');
    expect(put.body).toMatchObject({ revision: 3, scheduler: { min_replicas: 2, max_replicas: 8 } });
    expect(put.body).not.toHaveProperty('quotas');
    expect(screen.queryByRole('button', { name: 'Save configuration' })).toBeNull();
  });

  it('surfaces a revision conflict and lets the operator load the latest', async () => {
    const { server, calls } = serveInfra({ putConfig: status(409, { error: 'revision_conflict', message: 'revision 3 is stale' }) });
    page({ route: '/admin/infrastructure?tab=scheduler' });
    fireEvent.change(await screen.findByRole('spinbutton', { name: 'Maximum replicas' }), { target: { value: '9' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save configuration' }));
    expect(await screen.findByText('Not saved: the configuration changed since you loaded it')).toBeInTheDocument();
    expect(screen.getByRole('spinbutton', { name: 'Maximum replicas' })).toHaveValue(9);
    server.config = config({ revision: 5, scheduler: { min_replicas: 3, max_replicas: 7, target_cpu_percent: 60, scale_down_seconds: 300, fair_share_weight: 1 } });
    fireEvent.click(screen.getByRole('button', { name: 'Load the latest revision' }));
    expect(await screen.findByText(/Loaded revision 5/)).toBeInTheDocument();
    expect(screen.getByRole('spinbutton', { name: 'Maximum replicas' })).toHaveValue(7);
    expect(calls.filter((c) => c.method === 'GET' && c.path.endsWith('/config')).length).toBeGreaterThan(1);
  });

  it('shows the server’s refusal when saving fails for another reason', async () => {
    serveInfra({ putConfig: status(403, { error: 'forbidden', message: 'changing the fair-share weight needs a platform operator' }) });
    page({ route: '/admin/infrastructure?tab=scheduler' });
    fireEvent.change(await screen.findByRole('spinbutton', { name: 'Fair-share weight' }), { target: { value: '3' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save configuration' }));
    expect(await screen.findByText(/changing the fair-share weight needs a platform operator/)).toBeInTheDocument();
  });

  it('asks before discarding edits', async () => {
    serveInfra();
    page({ route: '/admin/infrastructure?tab=scheduler' });
    fireEvent.change(await screen.findByRole('spinbutton', { name: 'Target CPU' }), { target: { value: '80' } });
    fireEvent.click(screen.getByRole('button', { name: 'Discard changes' }));
    const dialog = screen.getByRole('alertdialog', { name: 'Discard unsaved changes?' });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Discard changes' }));
    expect(screen.getByRole('spinbutton', { name: 'Target CPU' })).toHaveValue(65);
    expect(screen.queryByText(/Unsaved:/)).toBeNull();
  });

  it('keeps edits across tab switches', async () => {
    serveInfra();
    page({ route: '/admin/infrastructure?tab=scheduler' });
    fireEvent.change(await screen.findByRole('spinbutton', { name: 'Target CPU' }), { target: { value: '80' } });
    fireEvent.click(tab('Task runners'));
    fireEvent.click(tab(/Scheduler/));
    expect(screen.getByRole('spinbutton', { name: 'Target CPU' })).toHaveValue(80);
  });

  it('warns before the browser tab closes with unsaved edits', async () => {
    serveInfra();
    page({ route: '/admin/infrastructure?tab=scheduler' });
    fireEvent.change(await screen.findByRole('spinbutton', { name: 'Target CPU' }), { target: { value: '80' } });
    const e = new Event('beforeunload', { cancelable: true });
    act(() => {
      window.dispatchEvent(e);
    });
    expect(e.defaultPrevented).toBe(true);
  });

  it('shows the fair-share weight, defaulting to 1 for a policy saved before the field existed', async () => {
    const older = config();
    delete older.scheduler.fair_share_weight;
    serveInfra({ config: older });
    page({ route: '/admin/infrastructure?tab=scheduler' });
    expect(await screen.findByRole('spinbutton', { name: 'Fair-share weight' })).toHaveValue(1);
    expect(screen.getByText(/Changing the weight requires a platform operator/)).toBeInTheDocument();
    expect(screen.queryByText(/Unsaved:/)).toBeNull();
  });

  it('shows a stored fair-share weight', async () => {
    serveInfra({ config: config({ scheduler: { min_replicas: 1, max_replicas: 4, target_cpu_percent: 65, scale_down_seconds: 300, fair_share_weight: 3 } }) });
    page({ route: '/admin/infrastructure?tab=scheduler' });
    expect(await screen.findByRole('spinbutton', { name: 'Fair-share weight' })).toHaveValue(3);
  });
});

describe('worker pools and profiles', () => {
  it('keeps linked pools on a renamed profile and protects profiles in use', async () => {
    serveInfra({ config: config({ pools: [], profiles: [] }) });
    page({ route: '/admin/infrastructure?tab=pools' });
    expect(await screen.findByRole('button', { name: 'Add worker pool' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Add pod profile' }));
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Profile name' })).toHaveFocus());
    fireEvent.click(screen.getByRole('button', { name: 'Add worker pool' }));
    expect(screen.getByRole('combobox', { name: 'Pod profile' })).toHaveValue('profile-1');
    fireEvent.change(screen.getByRole('textbox', { name: 'Profile name' }), { target: { value: 'aks-dev-small' } });
    expect(screen.getByRole('combobox', { name: 'Pod profile' })).toHaveValue('aks-dev-small');
    expect(screen.getByRole('button', { name: 'Remove profile aks-dev-small' })).toBeDisabled();
    expect(screen.getByText(/Used by pool-1: reassign or remove that pool first/)).toBeInTheDocument();
    // A new profile has no image digest yet: it says so in place.
    expect(screen.getByText(/Pin the image by digest/)).toBeInTheDocument();
  });

  it('edits node selector rows without losing a half-typed row', async () => {
    serveInfra({ config: config({ profiles: [profile({ node_selector: { 'kubernetes.azure.com/agentpool': 'cpu' } })], pools: [pool()] }) });
    page({ route: '/admin/infrastructure?tab=pools' });
    fireEvent.click(await screen.findByRole('button', { name: 'Add node selector' }));
    const key = screen.getByRole('textbox', { name: 'Label key 2' });
    await waitFor(() => expect(key).toHaveFocus());
    fireEvent.change(key, { target: { value: 'kubernetes.azure.com/agentpool' } });
    expect(screen.getByText('This key is already set above.')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Remove node selector 2' }));
    expect(screen.queryByText('This key is already set above.')).toBeNull();
    expect(screen.queryByText(/Unsaved:/)).toBeNull();
  });
});

describe('clusters', () => {
  it('offers only saved pools for binding, explaining the unsaved ones', async () => {
    serveInfra();
    page({ route: '/admin/infrastructure?tab=pools' });
    fireEvent.click(await screen.findByRole('button', { name: 'Add worker pool' }));
    fireEvent.click(tab('Clusters'));
    const select = await screen.findByRole('combobox', { name: 'Worker pool' });
    const options = within(select).getAllByRole('option');
    expect(options.map((o) => o.textContent)).toEqual(['cpu', 'pool-1 (unsaved — save the configuration first)']);
    expect(options[1]).toBeDisabled();
  });
});
