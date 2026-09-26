// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Assets: the server-searched list, one asset's producers, consumers and
 * materializations, and its lineage graph.
 */

import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AssetsPage } from './AssetsPage';
import { buildLineage } from '../components/AssetLineageGraph';
import type { Asset, AssetDetail, AssetGraph } from '../api/types';
import { ADMIN_PERMISSIONS } from '../test/permissions';
import { NS, forbidden, notConfigured, renderPage, stubApi, type Call } from '../test/eventsHarness';

const ORDERS = 's3://lake/orders/';
const REPORT = 's3://lake/report/';

const asset = (over: Partial<Asset> = {}): Asset => ({
  uri: ORDERS,
  name: 'orders',
  first_seen_at: '2026-09-01T00:00:00Z',
  last_declared_at: '2026-09-25T00:00:00Z',
  last_materialized_at: '2026-09-25T09:00:00Z',
  last_materialized_run_id: '12345678-aaaa-bbbb-cccc-dddddddddddd',
  producers: 1,
  consumers: 1,
  ...over,
});

const detailBody = (over: Partial<AssetDetail> = {}): AssetDetail => ({
  ...asset(),
  producer_tasks: [{ workflow: 'ingest', node_id: 'load', last_run_id: '12345678-aaaa-bbbb-cccc-dddddddddddd', last_recorded_at: '2026-09-25T09:00:00Z', runs: 9 }],
  consumer_tasks: [{ workflow: 'reporting', node_id: 'build', last_run_id: null, last_recorded_at: null, runs: 0 }],
  materializations: [
    { run_id: '12345678-aaaa-bbbb-cccc-dddddddddddd', node_id: 'load', workflow: 'ingest', materialized_at: '2026-09-25T09:00:00Z', run_state: 'SUCCEEDED' },
  ],
  latest_materialization: { run_id: '12345678-aaaa-bbbb-cccc-dddddddddddd', node_id: 'load', workflow: 'ingest', materialized_at: '2026-09-25T09:00:00Z', run_state: 'SUCCEEDED' },
  ...over,
});

const graph: AssetGraph = {
  nodes: [
    { id: `asset:${ORDERS}`, kind: 'asset', label: 'orders', uri: ORDERS },
    { id: `asset:${REPORT}`, kind: 'asset', label: 'report', uri: REPORT },
    { id: 'task:ingest/load', kind: 'task', label: 'ingest · load', workflow: 'ingest', node_id: 'load' },
    { id: 'task:reporting/build', kind: 'task', label: 'reporting · build', workflow: 'reporting', node_id: 'build' },
  ],
  edges: [
    { from: 'task:ingest/load', to: `asset:${ORDERS}` },
    { from: `asset:${ORDERS}`, to: 'task:reporting/build' },
    { from: 'task:reporting/build', to: `asset:${REPORT}` },
    // A duplicate and a dangling edge the graph must drop.
    { from: 'task:ingest/load', to: `asset:${ORDERS}` },
    { from: 'task:ghost/x', to: `asset:${ORDERS}` },
  ],
  truncated: false,
};

function detailApi(extra?: (c: Call) => [number, unknown] | undefined) {
  return stubApi((c) => {
    const hit = extra?.(c);
    if (hit) return hit;
    if (c.path === `${NS}/assets/detail`) {
      return c.search.get('uri') === ORDERS ? [200, detailBody()] : [200, detailBody({ ...asset({ uri: REPORT, name: 'report' }) })];
    }
    if (c.path === `${NS}/assets/graph`) return [200, graph];
    return undefined;
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});
afterEach(() => {
  vi.unstubAllGlobals();
});

function show(url = '/assets', permissions: readonly string[] = ADMIN_PERMISSIONS) {
  return renderPage(<AssetsPage namespace="default" />, { path: '/assets', url, permissions });
}
const detailUrl = (uri: string) => `/assets?${new URLSearchParams({ uri })}`;

describe('the asset list', () => {
  it('lists assets with producer and consumer counts and the last materialization', async () => {
    stubApi((c) => (c.path === `${NS}/assets` ? [200, { assets: [asset()] }] : undefined));
    show();
    const link = await screen.findByRole('link', { name: 'orders' });
    expect(link).toHaveAttribute('href', detailUrl(ORDERS));
    const row = link.closest('tr')!;
    expect(within(row).getByText(ORDERS)).toBeInTheDocument();
    expect(within(row).getByRole('link', { name: 'run 12345678' })).toHaveAttribute('href', '/runs/12345678-aaaa-bbbb-cccc-dddddddddddd');
  });

  it('sends the search term to the server', async () => {
    const calls = stubApi((c) => (c.path === `${NS}/assets` ? [200, { assets: c.search.get('q') === 'ord' ? [asset()] : [asset(), asset({ uri: REPORT, name: 'report' })] }] : undefined));
    show();
    await screen.findByRole('link', { name: 'report' });
    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'ord' } });
    await waitFor(() => expect(calls.some((c) => c.search.get('q') === 'ord')).toBe(true));
    await waitFor(() => expect(screen.queryByRole('link', { name: 'report' })).not.toBeInTheDocument());
  });

  it('explains how assets appear when there are none', async () => {
    stubApi((c) => (c.path === `${NS}/assets` ? [200, { assets: [] }] : undefined));
    show();
    expect(await screen.findByText('No assets recorded yet')).toBeInTheDocument();
  });

  it('shows an error with a retry', async () => {
    stubApi(() => [400, { message: 'bad' }]);
    show();
    expect(await screen.findByText('Could not load assets')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('says lineage is off on a 501', async () => {
    stubApi(() => notConfigured);
    show();
    expect(await screen.findByText(/not enabled on this server/)).toBeInTheDocument();
  });

  it('renders a 403 as not permitted', async () => {
    stubApi(() => forbidden);
    show();
    expect(await screen.findByText(/Not permitted: view assets/)).toBeInTheDocument();
  });

  it('refuses the page without assets.view', () => {
    const calls = stubApi(() => undefined);
    show('/assets', ['runs.view']);
    expect(screen.getByText(/Not permitted: view assets/)).toBeInTheDocument();
    expect(calls).toHaveLength(0);
  });
});

describe('one asset', () => {
  it('shows producers, consumers and materializations with links to runs and workflows', async () => {
    detailApi();
    show(detailUrl(ORDERS));
    expect(await screen.findByRole('heading', { name: 'orders' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: '12345678-aaaa-bbbb-cccc-dddddddddddd' })).toHaveAttribute('href', '/runs/12345678-aaaa-bbbb-cccc-dddddddddddd');
    expect(screen.getAllByRole('link', { name: 'ingest' })[0]).toHaveAttribute('href', '/workflows/ingest');
    expect(screen.getByRole('link', { name: 'reporting' })).toHaveAttribute('href', '/workflows/reporting');
  });

  it('draws the lineage with the focus asset marked and the two node kinds apart', async () => {
    detailApi();
    const { container } = show(detailUrl(ORDERS));
    const focus = await screen.findByRole('button', { name: /Asset orders, the asset shown on this page/ });
    expect(focus).toHaveAttribute('aria-current', 'true');
    expect(container.querySelectorAll('[data-kind="asset"]')).toHaveLength(2);
    expect(container.querySelectorAll('[data-kind="task"]')).toHaveLength(2);
    // The duplicate and the dangling edge are dropped.
    expect(container.querySelectorAll('path.dag-edge')).toHaveLength(3);
  });

  it('opens a task node\'s workflow and another asset\'s page from the graph', async () => {
    detailApi();
    show(detailUrl(ORDERS));
    fireEvent.click(await screen.findByRole('button', { name: /Asset report, open this asset/ }));
    expect(await screen.findByRole('heading', { name: 'report' })).toBeInTheDocument();
    fireEvent.click(await screen.findByRole('button', { name: /Task build in workflow reporting/ }));
    expect(await screen.findByText('navigated away')).toBeInTheDocument();
  });

  it('asks the server for the chosen depth', async () => {
    const calls = detailApi();
    show(detailUrl(ORDERS));
    await screen.findByRole('heading', { name: 'orders' });
    fireEvent.change(screen.getByLabelText('Lineage depth'), { target: { value: '4' } });
    await waitFor(() =>
      expect(calls.some((c) => c.path.endsWith('/assets/graph') && c.search.get('depth') === '4' && c.search.get('uri') === ORDERS)).toBe(true),
    );
  });

  it('says an unknown asset is unknown rather than failing', async () => {
    stubApi(() => [404, { error: 'not_found', message: 'asset not found' }]);
    show(detailUrl('s3://nowhere/'));
    expect(await screen.findByText('Unknown asset')).toBeInTheDocument();
  });

  it('says so when an asset has never been materialized', async () => {
    detailApi((c) => (c.path.endsWith('/assets/detail') ? [200, detailBody({ latest_materialization: null, materializations: [] })] : undefined));
    show(detailUrl(ORDERS));
    expect(await screen.findByText('Never materialized')).toBeInTheDocument();
  });
});

describe('buildLineage', () => {
  it('layers tasks and assets left to right along the declared edges', () => {
    const l = buildLineage(graph);
    expect(l.edges).toHaveLength(3);
    expect(l.hasCycle).toBe(false);
    expect(l.layers.map((layer) => layer.length)).toEqual([1, 1, 1, 1]);
    expect(l.layers[0]).toEqual(['task:ingest/load']);
    expect(l.layers[3]).toEqual([`asset:${REPORT}`]);
  });
});
