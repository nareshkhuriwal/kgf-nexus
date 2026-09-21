import { PermissionContext } from '../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the catalog list.
 *
 * What is pinned here is the reasoning, not the markup: that the list is one
 * row per workflow NAME, that search reaches the SERVER (a client-side filter
 * would silently become a lie once the namespace outgrows one page), and that
 * each row drills down to a URL that actually addresses the workflow.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { WorkflowsPage } from './WorkflowsPage';
import type { WorkflowSummary } from '../api/types';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <PermissionContext.Provider value={["runs.view","runs.create","runs.cancel","runs.retry","workflows.view","workflows.publish","workflows.delete","schedules.create","tasks.logs.view","tasks.results.view"]}><QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider></PermissionContext.Provider>,
  );
}

/** Records every URL requested, so "the search went to the server" is testable. */
function mockFetch(handler: (url: string) => [number, unknown]) {
  const urls: string[] = [];
  vi.stubGlobal('fetch', (url: string) => {
    urls.push(String(url));
    const [status, body] = handler(String(url));
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return urls;
}

const workflow = (over: Partial<WorkflowSummary> = {}): WorkflowSummary => ({
  id: 'spec-aaaa',
  namespace: 'default',
  name: 'ten-node-mixed',
  latest_version: 3,
  spec_hash: 'b12cd313da964e723a3236ca3bd60723',
  created_by: 'admin-f61-diag',
  updated_at: '2026-09-12T09:52:52Z',
  version_count: 3,
  first_created_at: '2026-09-12T08:44:15Z',
  ...over,
});

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the workflow catalog list', () => {
  it('shows one row per workflow with its latest version and version count', async () => {
    mockFetch(() => [
      200,
      { workflows: [workflow(), workflow({ name: 'example-etl', latest_version: 1, version_count: 1 })] },
    ]);
    wrap(<WorkflowsPage namespace="default" />);

    const row = (await screen.findByText('ten-node-mixed')).closest('tr')!;
    expect(within(row).getByText('v3')).toBeInTheDocument();
    expect(within(row).getByText('3 versions')).toBeInTheDocument();

    // A single-version workflow says so rather than offering a history link
    // that would show one row.
    const single = (await screen.findByText('example-etl')).closest('tr')!;
    expect(within(single).getByText('1 version')).toBeInTheDocument();
  });

  it('links each workflow to its own detail page', async () => {
    mockFetch(() => [200, { workflows: [workflow()] }]);
    wrap(<WorkflowsPage namespace="default" />);

    const link = await screen.findByRole('link', { name: 'ten-node-mixed' });
    expect(link).toHaveAttribute('href', '/workflows/ten-node-mixed');
  });

  /**
   * The point of the search control. Filtering the already-fetched rows would
   * pass a naive test and be wrong in production, so what is asserted is that
   * the TERM REACHED THE SERVER.
   */
  it('sends the search term to the server rather than filtering locally', async () => {
    const urls = mockFetch((url) => {
      const matched = url.includes('search=ten');
      return [200, { workflows: matched ? [workflow()] : [workflow(), workflow({ name: 'example-etl' })] }];
    });
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('example-etl');

    fireEvent.change(screen.getByLabelText('search workflows by name'), {
      target: { value: 'ten' },
    });

    await waitFor(() => {
      expect(urls.some((u) => u.includes('search=ten'))).toBe(true);
    });
    // And the server's answer is what renders: the unmatched row is gone.
    await waitFor(() => {
      expect(screen.queryByText('example-etl')).not.toBeInTheDocument();
    });
    expect(screen.getByText('ten-node-mixed')).toBeInTheDocument();
  });

  it('says the search matched nothing, rather than showing an empty namespace', async () => {
    mockFetch((url) => [200, { workflows: url.includes('search=') ? [] : [workflow()] }]);
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('ten-node-mixed');

    fireEvent.change(screen.getByLabelText('search workflows by name'), {
      target: { value: 'zzz' },
    });

    // The two empties are different situations and must not share a message.
    expect(await screen.findByText(/No workflow name contains/)).toBeInTheDocument();
  });

  /**
   * While the debounce settles, the count beside the search field belongs to the
   * PREVIOUS question. Showing a stale number there would claim the table has
   * been filtered when it has not, so the chip says what it is doing instead.
   */
  it('says it is searching rather than showing a count for the old query', async () => {
    mockFetch(() => [200, { workflows: [workflow()] }]);
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('ten-node-mixed');

    fireEvent.change(screen.getByLabelText('search workflows by name'), {
      target: { value: 'ten' },
    });

    expect(screen.getByText('searching…')).toBeInTheDocument();
    // And it goes away once the term settles and the server has answered.
    await waitFor(() => {
      expect(screen.queryByText('searching…')).not.toBeInTheDocument();
    });
  });

  it('explains how workflows get here when the namespace has none', async () => {
    mockFetch(() => [200, { workflows: [] }]);
    wrap(<WorkflowsPage namespace="default" />);

    expect(await screen.findByText('No workflows in this namespace')).toBeInTheDocument();
    expect(screen.getByText(/nexusctl spec apply/)).toBeInTheDocument();
  });

  it('shows an error instead of an empty catalog when the endpoint fails', async () => {
    // 404 rather than 500: a 4xx is not retried, so the panel reaches its
    // error state without the two backoff waits a 5xx deliberately costs.
    mockFetch(() => [404, { message: 'spec store unavailable' }]);
    wrap(<WorkflowsPage namespace="default" />);

    expect(await screen.findByText('Could not load workflows')).toBeInTheDocument();
    // The SERVER'S words, not a message the console invented about them.
    expect(screen.getByText(/spec store unavailable/)).toBeInTheDocument();
  });

  it('explains a permission denial as a role problem, not a bad key', async () => {
    // No server message, so the client's own 403 explanation is what renders —
    // and the fix it names (use an admin key) is different from a 401's.
    mockFetch(() => [403, '']);
    wrap(<WorkflowsPage namespace="default" />);

    expect(await screen.findByText('Could not load workflows')).toBeInTheDocument();
    expect(screen.getByText(/lacks permission/)).toBeInTheDocument();
  });
});
