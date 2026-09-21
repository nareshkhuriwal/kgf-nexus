// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Bulk cancellation on the run list.
 *
 * The bug this file exists for: on a namespace whose recent runs have all
 * finished, EVERY row is terminal, so no row is cancellable, so no checkbox is
 * drawn and the header checkbox is disabled. The feature is working exactly as
 * designed and is indistinguishable from a broken one — there was nothing on
 * screen saying why, and nothing in the tests pinning either half.
 *
 * So both halves are pinned here: that a cancellable run IS selectable, and
 * that a page of finished runs explains itself rather than presenting a dead
 * control.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { RunListPage } from './RunListPage';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

function run(id: string, state: string) {
  return {
    id,
    namespace: 'default',
    spec_id: 'spec-1',
    spec_name: 'daily-etl',
    spec_version: 1,
    state,
    trigger_kind: 'api',
    created_at: '2026-09-12T10:00:00Z',
    started_at: '2026-09-12T10:00:01Z',
    ended_at: state === 'RUNNING' ? null : '2026-09-12T10:00:09Z',
    node_counts: {},
  };
}

function mockRuns(runs: ReturnType<typeof run>[]) {
  vi.stubGlobal('fetch', () =>
    Promise.resolve(
      new Response(JSON.stringify({ runs }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('bulk cancel on the run list', () => {
  it('offers a checkbox for a run that can still be cancelled', async () => {
    mockRuns([run('r-run', 'RUNNING')]);
    wrap(<RunListPage namespace="default" />);

    const box = await screen.findByRole('checkbox', { name: /run r-run/i });
    fireEvent.click(box);

    expect(await screen.findByRole('button', { name: /cancel 1 run/i })).toBeTruthy();
  });

  /**
   * The screenshot case: every row SUCCEEDED. No row is a candidate, so no
   * checkbox is drawn — correct, and previously silent.
   */
  it('draws no row checkbox when every run has already finished', async () => {
    mockRuns([run('r-a', 'SUCCEEDED'), run('r-b', 'SUCCEEDED')]);
    wrap(<RunListPage namespace="default" />);

    await screen.findAllByText('daily-etl');
    expect(screen.queryByRole('checkbox', { name: /run r-a/i })).toBeNull();
  });

  it('says why nothing can be selected, rather than leaving a dead control', async () => {
    mockRuns([run('r-a', 'SUCCEEDED'), run('r-b', 'SUCCEEDED')]);
    wrap(<RunListPage namespace="default" />);

    // The explanation is what turns "the feature is broken" into "these runs
    // have finished".
    expect(await screen.findByText(/already finished/i)).toBeTruthy();
  });

  it('still selects the cancellable runs on a page that mixes both', async () => {
    mockRuns([run('r-done', 'SUCCEEDED'), run('r-live', 'RUNNING')]);
    wrap(<RunListPage namespace="default" />);

    expect(screen.queryByRole('checkbox', { name: /run r-done/i })).toBeNull();
    fireEvent.click(await screen.findByRole('checkbox', { name: /run r-live/i }));
    expect(await screen.findByRole('button', { name: /cancel 1 run/i })).toBeTruthy();
  });
});

it('lets admins select completed runs for deletion and shows row actions', async () => {
  mockRuns([run('done', 'SUCCEEDED')]);
  wrap(<RunListPage namespace="default" canAdmin />);
  fireEvent.click(await screen.findByRole('checkbox', {name: 'Select run done'}));
  expect(screen.getByRole('button', {name: /delete 1 run/i})).toBeTruthy();
  expect(screen.getByRole('link', {name: 'View run'}).getAttribute('href')).toBe('/runs/done');
});

it('changes the server page size in the actual runs request', async () => {
  const fetcher=vi.fn(()=>Promise.resolve(new Response(JSON.stringify({runs:[run('done','SUCCEEDED')]}),{status:200})));
  vi.stubGlobal('fetch',fetcher);
  wrap(<RunListPage namespace="default" canAdmin />);
  fireEvent.change(await screen.findByRole('combobox',{name:'runs per page'}),{target:{value:'50'}});
  await vi.waitFor(()=>expect(fetcher.mock.calls.some(c=>String((c as unknown[])[0]).includes('limit=50'))).toBe(true));
});
