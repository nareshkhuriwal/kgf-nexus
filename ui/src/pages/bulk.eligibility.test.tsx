// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The per-table bulk actions on schedules, runs and workers.
 *
 * One file rather than three, because what is pinned is a rule the three share
 * and each expresses differently: THE ACTION IS THE ONE THAT IS TRUE FOR THE
 * ENTITY, and a row the action cannot reach is not offered for selection in the
 * first place.
 *
 *   - Schedules are DELETED. A schedule is a stored rule, not a record of
 *     anything that happened, so removing it is meaningful and every row is a
 *     candidate.
 *   - Runs are CANCELLED, never deleted: a run is immutable execution history
 *     the audit trail references. Only a NON-TERMINAL run is cancellable, so a
 *     SUCCEEDED run gets no checkbox — rather than being selectable and then
 *     reported as a failure for a row that was never a candidate.
 *   - Workers are DRAINED, never deleted: the row is a live agent and would
 *     re-register on its next heartbeat. Drain is REVERSIBLE, so unlike the
 *     other three it takes no confirmation — a dialog in front of something
 *     reversible is how operators learn to click through the ones that matter.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { SchedulesPage } from './SchedulesPage';
import { RunListPage } from './RunListPage';
import { WorkersPage } from './WorkersPage';
import type { RunSummary, Schedule, Worker } from '../api/types';

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

const schedule = (name: string, over: Partial<Schedule> = {}): Schedule => ({
  id: `sc-${name}`,
  namespace: 'default',
  name,
  spec_name: 'example-etl',
  cron: '0 2 * * *',
  timezone: 'UTC',
  params: {},
  priority: 0,
  overlap_policy: 'skip',
  catchup_limit: 0,
  enabled: true,
  next_fire_at: '2026-09-13T02:00:00Z',
  last_fire_at: null,
  created_at: '2026-09-12T10:00:00Z',
  updated_at: '2026-09-12T10:00:00Z',
  created_by: 'ops',
  ...over,
});

const run = (id: string, state: string): RunSummary =>
  ({
    id,
    namespace: 'default',
    spec_id: 'spec-aaaa',
    spec_name: 'example-etl',
    spec_version: 1,
    state,
    created_at: '2026-09-12T10:00:00Z',
    started_at: '2026-09-12T10:00:01Z',
    ended_at: null,
    trigger_kind: 'manual',
  }) as RunSummary;

const worker = (id: string, state: string): Worker =>
  ({
    id,
    namespace: 'default',
    hostname: `agent-${id}`,
    queues: ['default'],
    capacity: { cpu_millis: 8000, memory_bytes: 16 * 1024 ** 3, gpu_count: 0 },
    committed: { cpu_millis: 0, memory_bytes: 0, gpu_count: 0 },
    state,
    agent_version: '0.1.0',
    last_heartbeat_at: new Date().toISOString(),
  }) as Worker;

/**
 * Serves one collection and records every mutating request against it.
 *
 * `match` picks the mutation out of the URL, so each screen's test says which
 * verb and route it expects rather than the helper assuming one.
 */
function mockApi(
  list: unknown,
  match: RegExp,
  method: string,
  opts: { failFor?: string[] } = {},
) {
  const called: string[] = [];
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    const u = String(url);
    const m = match.exec(u);
    if (m && init?.method === method) {
      const id = decodeURIComponent(m[1]!);
      called.push(id);
      if (opts.failFor?.includes(id)) {
        return Promise.resolve(
          new Response(JSON.stringify({ error: 'conflict', message: 'already terminal' }), {
            status: 409,
            headers: { 'Content-Type': 'application/json' },
          }),
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ ok: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      );
    }
    return Promise.resolve(
      new Response(JSON.stringify(list), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return { called };
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('schedules: bulk delete', () => {
  it('offers DELETE and issues one request per schedule', async () => {
    const calls = mockApi(
      { schedules: [schedule('nightly-etl'), schedule('hourly-roll')] },
      /\/schedules\/([^/?]+)$/,
      'DELETE',
    );
    wrap(<SchedulesPage namespace="default" />);
    await screen.findByText('nightly-etl');

    fireEvent.click(
      await screen.findByRole('checkbox', { name: 'Select all 2 loaded schedules' }),
    );
    fireEvent.click(await screen.findByRole('button', { name: 'Delete 2 schedules' }));

    // Destructive: confirmation first, stating the exact count and noun.
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
    expect(screen.getByText('Delete 2 schedules?')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Yes, delete 2 schedules$/ }));

    await waitFor(() => expect(calls.called.sort()).toEqual(['hourly-roll', 'nightly-etl']));
  });

  /** A disabled schedule is still a stored row that DELETE removes. */
  it('offers a disabled schedule for deletion too', async () => {
    mockApi(
      { schedules: [schedule('nightly-etl', { enabled: false, next_fire_at: null })] },
      /\/schedules\/([^/?]+)$/,
      'DELETE',
    );
    wrap(<SchedulesPage namespace="default" />);
    await screen.findByText('nightly-etl');

    expect(screen.getByRole('checkbox', { name: 'Select nightly-etl' })).toBeInTheDocument();
  });

  it('says what goes with a schedule, because the tick history is not recoverable', async () => {
    mockApi({ schedules: [schedule('nightly-etl')] }, /\/schedules\/([^/?]+)$/, 'DELETE');
    wrap(<SchedulesPage namespace="default" />);
    await screen.findByText('nightly-etl');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select nightly-etl' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Delete 1 schedule' }));

    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toHaveTextContent(/fire history/i);
    // And the reassurance that matters: the runs it already made are not touched.
    expect(dialog).toHaveTextContent(/already created are not affected/i);
  });
});

describe('runs: bulk cancel', () => {
  /**
   * A run is immutable history. There is no DELETE for one and there must not
   * be: the audit trail references it.
   */
  it('offers CANCEL, never delete', async () => {
    mockApi({ runs: [run('run-1', 'RUNNING')] }, /\/runs\/([^/?]+)\/cancel$/, 'POST');
    wrap(<RunListPage namespace="default" />);
    await screen.findByText(/run-1/);

    fireEvent.click(screen.getByRole('checkbox', { name: /^Select run run-1/ }));

    expect(await screen.findByRole('button', { name: 'Cancel 1 run' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
  });

  /**
   * THE ELIGIBILITY RULE. A SUCCEEDED run cannot be cancelled, so it gets no
   * checkbox — rather than being selectable and then reported as a failure for
   * a row that was never a candidate.
   */
  it('does not offer a terminal run for selection', async () => {
    mockApi(
      { runs: [run('run-1', 'RUNNING'), run('run-2', 'SUCCEEDED'), run('run-3', 'FAILED')] },
      /\/runs\/([^/?]+)\/cancel$/,
      'POST',
    );
    wrap(<RunListPage namespace="default" />);
    await screen.findByText(/run-1/);

    expect(screen.getByRole('checkbox', { name: /^Select run run-1/ })).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: /^Select run run-2/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: /^Select run run-3/ })).not.toBeInTheDocument();
  });

  it('counts only the cancellable runs in select-all', async () => {
    const calls = mockApi(
      { runs: [run('run-1', 'RUNNING'), run('run-2', 'SUCCEEDED'), run('run-3', 'PENDING')] },
      /\/runs\/([^/?]+)\/cancel$/,
      'POST',
    );
    wrap(<RunListPage namespace="default" />);
    await screen.findByText(/run-1/);

    // Two of three rows are cancellable, and the control says two. The count
    // says "loaded" because /runs pages and never totals.
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Select all 2 loaded runs' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel 2 runs' }));
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Yes, cancel 2 runs$/ }));

    // The SUCCEEDED run was never asked about.
    await waitFor(() => expect(calls.called.sort()).toEqual(['run-1', 'run-3']));
  });

  /** Cancelling cannot be undone, so it confirms and says so. */
  it('confirms before cancelling, and says it cannot be undone', async () => {
    mockApi({ runs: [run('run-1', 'RUNNING')] }, /\/runs\/([^/?]+)\/cancel$/, 'POST');
    wrap(<RunListPage namespace="default" />);
    await screen.findByText(/run-1/);

    fireEvent.click(screen.getByRole('checkbox', { name: /^Select run run-1/ }));
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel 1 run' }));

    const dialog = await screen.findByRole('alertdialog');
    expect(dialog).toHaveTextContent(/cannot be undone/i);
  });

  /**
   * The row's run id is a <Link> to the run. Clicking the checkbox must select
   * without navigating — a checkbox inside a linked row is a known trap.
   */
  it('selects without following the row’s link', async () => {
    mockApi({ runs: [run('run-1', 'RUNNING')] }, /\/runs\/([^/?]+)\/cancel$/, 'POST');
    wrap(<RunListPage namespace="default" />);
    await screen.findByText(/run-1/);

    const box = screen.getByRole('checkbox', { name: /^Select run run-1/ }) as HTMLInputElement;
    fireEvent.click(box);

    expect(box.checked).toBe(true);
    // Still on the run LIST: the table is on screen and the bar is offering the
    // list action, which a navigation to the detail view would have replaced.
    expect(await screen.findByRole('button', { name: 'Cancel 1 run' })).toBeInTheDocument();
  });

  /** Partial failure continues: the run that worked stays cancelled. */
  it('reports per row when one run refuses and completes the rest', async () => {
    const calls = mockApi(
      { runs: [run('run-1', 'RUNNING'), run('run-2', 'RUNNING')] },
      /\/runs\/([^/?]+)\/cancel$/,
      'POST',
      { failFor: ['run-2'] },
    );
    wrap(<RunListPage namespace="default" />);
    await screen.findByText(/run-1/);

    fireEvent.click(await screen.findByRole('checkbox', { name: 'Select all 2 loaded runs' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel 2 runs' }));
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Yes, cancel 2 runs$/ }));

    await screen.findByText('1 succeeded');
    expect(screen.getByText('1 failed')).toBeInTheDocument();
    // Both were attempted — it did not stop at the first refusal — and the
    // failure names the reason rather than a bare status.
    expect(calls.called.sort()).toEqual(['run-1', 'run-2']);
    const failures = document.querySelector('.bulk-bar-failures')!;
    expect(failures).toHaveTextContent(/already terminal/);
  });
});

describe('workers: bulk drain', () => {
  /**
   * A worker row is a LIVE AGENT. Deleting it is meaningless — it re-registers
   * on its next heartbeat — so the action is drain.
   */
  it('offers DRAIN, never delete', async () => {
    mockApi(
      { workers: [worker('w1', 'ACTIVE')] },
      /\/workers\/([^/?]+)\/drain$/,
      'POST',
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-w1');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select agent-w1' }));

    expect(await screen.findByRole('button', { name: 'Drain 1 worker' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
  });

  /**
   * DRAIN IS REVERSIBLE — the worker's own next heartbeat cycle undoes it — so
   * it must NOT confirm. A dialog in front of something reversible is how
   * operators learn to click through the ones that matter.
   */
  it('drains without a confirmation, because a drain can be undone', async () => {
    const calls = mockApi(
      { workers: [worker('w1', 'ACTIVE')] },
      /\/workers\/([^/?]+)\/drain$/,
      'POST',
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-w1');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select agent-w1' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Drain 1 worker' }));

    // Straight to the request: no alertdialog in between.
    await waitFor(() => expect(calls.called).toEqual(['w1']));
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
  });

  /** A DEAD worker is not listening, so draining it would record nothing. */
  it('does not offer a dead worker for selection', async () => {
    mockApi(
      { workers: [worker('w1', 'ACTIVE'), worker('w2', 'DEAD')] },
      /\/workers\/([^/?]+)\/drain$/,
      'POST',
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-w2');

    expect(screen.getByRole('checkbox', { name: 'Select agent-w1' })).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: 'Select agent-w2' })).not.toBeInTheDocument();
  });

  /**
   * /workers takes no `limit`, so the rows ARE the fleet and select-all may say
   * so plainly — no "loaded" qualifier, because there is nothing unloaded.
   */
  it('claims the whole fleet in select-all, because the endpoint sends all of it', async () => {
    mockApi(
      { workers: [worker('w1', 'ACTIVE'), worker('w2', 'ACTIVE')] },
      /\/workers\/([^/?]+)\/drain$/,
      'POST',
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-w1');

    expect(
      await screen.findByRole('checkbox', { name: 'Select all 2 workers' }),
    ).toBeInTheDocument();
    expect(screen.queryByRole('checkbox', { name: /loaded workers/ })).not.toBeInTheDocument();
  });

  /** The host cell is a <Link> to the worker detail; the checkbox must not follow it. */
  it('selects without following the row’s link', async () => {
    mockApi(
      { workers: [worker('w1', 'ACTIVE')] },
      /\/workers\/([^/?]+)\/drain$/,
      'POST',
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-w1');

    const box = screen.getByRole('checkbox', { name: 'Select agent-w1' }) as HTMLInputElement;
    fireEvent.click(box);

    expect(box.checked).toBe(true);
    expect(await screen.findByRole('button', { name: 'Drain 1 worker' })).toBeInTheDocument();
  });
});
