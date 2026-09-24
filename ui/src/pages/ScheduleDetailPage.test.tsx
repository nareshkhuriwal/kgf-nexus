import { PermissionContext } from '../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the schedule detail view and its backfills.
 *
 * The behaviours worth pinning are the ones an operator would be misled by if
 * they broke: a stored expression that no longer compiles must LEAD rather than
 * be a footnote (the schedule still says "enabled" everywhere else), delete must
 * be confirmed and must say what survives it, and cancelling a backfill must
 * state that the runs it already created keep going — that is the thing people
 * get wrong when they cancel one to stop the load.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ScheduleDetailPage } from './ScheduleDetailPage';
import { BackfillDetailPage } from './BackfillDetailPage';
import type { Backfill, Schedule } from '../api/types';

function wrapScheduleDetail(name = 'nightly-etl') {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <PermissionContext.Provider value={["schedules.view","schedules.create","schedules.update","schedules.delete","backfills.view","backfills.create","backfills.update","backfills.cancel","runs.create","workflows.view"]}><QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[`/schedules/${name}`]}>
        <Routes>
          <Route path="/schedules/:name" element={<ScheduleDetailPage namespace="default" />} />
          <Route path="/schedules" element={<div>schedule list</div>} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider></PermissionContext.Provider>,
  );
}

function wrapBackfillDetail(id = 'bf-1') {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <PermissionContext.Provider value={["schedules.view","schedules.create","schedules.update","schedules.delete","backfills.view","backfills.create","backfills.update","backfills.cancel","runs.create","workflows.view"]}><QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[`/backfills/${id}`]}>
        <Routes>
          <Route
            path="/backfills/:backfillId"
            element={<BackfillDetailPage namespace="default" />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider></PermissionContext.Provider>,
  );
}

function mockFetch(handler: (url: string, init?: RequestInit) => [number, unknown]) {
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    const [status, body] = handler(String(url), init);
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
}

const schedule = (over: Partial<Schedule> = {}): Schedule => ({
  id: 'sc-1111',
  namespace: 'default',
  name: 'nightly-etl',
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
  next_fires: ['2026-09-13T02:00:00Z', '2026-09-14T02:00:00Z'],
  ...over,
});

const backfill = (over: Partial<Backfill> = {}): Backfill => ({
  id: 'bf-1',
  schedule_id: 'sc-1111',
  namespace: 'default',
  from: '2026-09-01T00:00:00Z',
  to: '2026-09-11T00:00:00Z',
  cursor_at: '2026-09-06T00:00:00Z',
  max_active: 2,
  max_runs: 100,
  runs_created: 12,
  state: 'RUNNING',
  detail: {},
  created_at: '2026-09-12T10:00:00Z',
  updated_at: '2026-09-12T10:05:00Z',
  ended_at: null,
  created_by: 'ops',
  ...over,
});

/** Serves the detail read plus an empty backfill list unless overridden. */
function serve(sched: Schedule, backfills: Backfill[] = []) {
  mockFetch((url) => {
    if (url.includes('/backfills')) return [200, { backfills }];
    return [200, sched];
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the schedule detail view', () => {
  it('shows the configuration and the upcoming fire times', async () => {
    serve(schedule());
    wrapScheduleDetail();

    expect(await screen.findByRole('heading', { name: 'nightly-etl' })).toBeInTheDocument();
    expect(screen.getByText('skip')).toBeInTheDocument();
    // Both upcoming fires are listed, not just the single next_fire_at that the
    // configuration panel repeats -- seeing the CADENCE is what tells an author
    // the expression means what they intended.
    const fires = screen.getByRole('list');
    expect(within(fires).getByText(/2026-09-13/)).toBeInTheDocument();
    expect(within(fires).getByText(/2026-09-14/)).toBeInTheDocument();
  });

  /**
   * A schedule whose stored expression stopped compiling still reads "enabled"
   * in every other control on the page, so this is the ONE place the truth
   * appears. It must be an alert, not a quiet empty preview.
   */
  it('leads with a stored cron that no longer compiles', async () => {
    serve(schedule({ next_fires: undefined, cron_error: 'unknown time zone Mars/Olympus' }));
    wrapScheduleDetail();

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/cannot fire/i);
    expect(alert).toHaveTextContent(/Mars\/Olympus/);
    // Enabling it would be refused by the gateway, so the page says so rather
    // than leaving the operator to discover it by clicking.
    expect(alert).toHaveTextContent(/enabling it will be refused/i);
  });

  it('reports an enabled schedule that is not armed', async () => {
    serve(schedule({ enabled: true, next_fire_at: null }));
    wrapScheduleDetail();

    const unarmed = await screen.findByText('not armed');
    expect(unarmed).toBeInTheDocument();
    // Warning-toned rather than plain text: "not armed" sitting bare in a
    // definition list reads as a value, when it is the finding. The tone comes
    // from the state palette, never from the operator-chosen accent, so a theme
    // change cannot quieten it.
    expect(unarmed).toHaveClass('warn');
    // And it keeps the explanation of WHY on the element, which is the part that
    // tells an operator whether to act or wait.
    expect(unarmed).toHaveAttribute('title', expect.stringContaining('died mid-fire'));
  });

  /**
   * Delete is irreversible and has no undo on the wire, so it is confirmed —
   * and the confirmation states what survives it, because that is exactly what
   * someone hesitating over the button needs to know.
   */
  it('confirms a delete and says the runs it created keep going', async () => {
    const calls: { url: string; method?: string }[] = [];
    mockFetch((url, init) => {
      calls.push({ url, method: init?.method });
      if (init?.method === 'DELETE') return [204, null];
      if (url.includes('/backfills')) return [200, { backfills: [] }];
      return [200, schedule()];
    });
    wrapScheduleDetail();

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(screen.getByText(/Runs it already created keep going/i)).toBeInTheDocument();
    // Nothing is deleted until the second click.
    expect(calls.some((c) => c.method === 'DELETE')).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: /yes, delete/i }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'DELETE' && c.url.endsWith('/schedules/nightly-etl'))).toBe(
        true,
      );
    });
  });

  /**
   * The state badge leads the name in the identity row. On a page reached
   * mid-incident "is this armed?" is read before "what is it called", so the
   * order is deliberate and worth pinning against a reflow that reverses it.
   */
  it('puts the state badge before the schedule name in the identity row', async () => {
    serve(schedule({ enabled: false }));
    wrapScheduleDetail();

    const heading = await screen.findByRole('heading', { name: 'nightly-etl' });
    const badge = screen.getByText('disabled');
    const row = heading.parentElement!;
    expect(row).toContainElement(badge);
    // DOCUMENT_POSITION_FOLLOWING: the badge precedes the heading in the DOM,
    // which is also the order a screen reader announces them in.
    expect(badge.compareDocumentPosition(heading) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('backs out of a delete without calling the endpoint', async () => {
    const calls: (string | undefined)[] = [];
    mockFetch((url, init) => {
      calls.push(init?.method);
      if (url.includes('/backfills')) return [200, { backfills: [] }];
      return [200, schedule()];
    });
    wrapScheduleDetail();

    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    fireEvent.click(screen.getByRole('button', { name: /keep it/i }));

    expect(screen.getByRole('button', { name: 'Delete' })).toBeInTheDocument();
    expect(calls).not.toContain('DELETE');
  });

  it('explains what a backfill is for when the schedule has none', async () => {
    serve(schedule());
    wrapScheduleDetail();

    const empties = await screen.findAllByRole('status');
    expect(empties.some((e) => /No backfills for this schedule/i.test(e.textContent ?? ''))).toBe(
      true,
    );
  });
});

describe('backfills on the schedule detail view', () => {
  it('measures progress against the interval, not the run count', async () => {
    serve(schedule(), [backfill()]);
    wrapScheduleDetail();

    // Cursor at 2026-09-06 in a 09-01 → 09-11 window is halfway through, which
    // is the number that tells an operator whether to wait. "12 runs created"
    // does not.
    expect(await screen.findByText(/50% · cursor at/i)).toBeInTheDocument();
    expect(screen.getByText('RUNNING', { selector: '[data-state]' })).toBeInTheDocument();
  });

  it('reads a null cursor on a live backfill as an exhausted interval', async () => {
    serve(schedule(), [backfill({ cursor_at: null })]);
    wrapScheduleDetail();

    expect(await screen.findByText(/interval exhausted/i)).toBeInTheDocument();
  });

  /**
   * The distinction operators get wrong: cancelling stops the backfill
   * GENERATING runs; it does not cancel the runs it already created. Someone
   * cancelling a runaway backfill to stop the load has a second job to do, and
   * the confirmation is where they find that out.
   */
  it('confirms a cancel and says the created runs are unaffected', async () => {
    const posted: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posted.push(url);
        return [200, backfill({ state: 'CANCELLED' })];
      }
      if (url.includes('/backfills')) return [200, { backfills: [backfill()] }];
      return [200, schedule()];
    });
    wrapScheduleDetail();

    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(screen.getByText(/already created keep going/i)).toBeInTheDocument();
    expect(posted).toHaveLength(0);

    fireEvent.click(screen.getByRole('button', { name: /yes, cancel/i }));
    await waitFor(() => {
      expect(posted.some((u) => u.endsWith('/backfills/bf-1/cancel'))).toBe(true);
    });
  });

  it('offers resume rather than pause on a paused backfill', async () => {
    serve(schedule(), [backfill({ state: 'PAUSED' })]);
    wrapScheduleDetail();

    expect(await screen.findByRole('button', { name: 'Resume' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();
  });

  it('offers no controls on a terminal backfill', async () => {
    serve(schedule(), [backfill({ state: 'COMPLETED', cursor_at: null, ended_at: '2026-09-12T11:00:00Z' })]);
    wrapScheduleDetail();

    await screen.findByText('COMPLETED', { selector: '[data-state]' });
    expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
  });

  it('posts the interval as RFC3339 when starting a backfill', async () => {
    let posted: Record<string, unknown> | null = null;
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posted = JSON.parse(String(init.body)) as Record<string, unknown>;
        return [201, backfill()];
      }
      if (url.includes('/backfills')) return [200, { backfills: [] }];
      return [200, schedule()];
    });
    wrapScheduleDetail();

    fireEvent.click(await screen.findByRole('button', { name: /new backfill/i }));
    fireEvent.change(screen.getByLabelText('backfill from'), {
      target: { value: '2026-09-01T00:00' },
    });
    fireEvent.change(screen.getByLabelText('backfill to'), {
      target: { value: '2026-09-02T00:00' },
    });
    fireEvent.click(screen.getByRole('button', { name: /start backfill/i }));

    await waitFor(() => expect(posted).not.toBeNull());
    // The gateway parses these strictly and rejects anything that is not
    // RFC3339, so the conversion from the datetime-local control is load-bearing.
    expect(String((posted as unknown as Record<string, string>).from)).toMatch(
      /^\d{4}-\d{2}-\d{2}T.*Z$/,
    );
    expect(posted).toMatchObject({ max_active: 2, max_runs: 100 });
  });

  it('refuses an inverted interval before sending it', async () => {
    serve(schedule());
    wrapScheduleDetail();

    fireEvent.click(await screen.findByRole('button', { name: /new backfill/i }));
    fireEvent.change(screen.getByLabelText('backfill from'), {
      target: { value: '2026-09-02T00:00' },
    });
    fireEvent.change(screen.getByLabelText('backfill to'), {
      target: { value: '2026-09-01T00:00' },
    });

    // An inverted interval produces a backfill that generates nothing, which
    // reads as a broken scheduler rather than as a typo.
    expect(await screen.findByText(/must be after/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /start backfill/i })).toBeDisabled();
  });
});

describe('the backfill detail view', () => {
  it('shows the cursor position and the runs created', async () => {
    mockFetch(() => [200, backfill()]);
    wrapBackfillDetail();

    expect(await screen.findByText(/50% through the interval/i)).toBeInTheDocument();
    expect(screen.getByText(/12 runs created/i)).toBeInTheDocument();
  });

  it('renders a failed backfill with the server-supplied reason', async () => {
    mockFetch(() => [
      200,
      backfill({ state: 'FAILED', detail: { error: 'spec example-etl not found' } }),
    ]);
    wrapBackfillDetail();

    expect(await screen.findByText('FAILED', { selector: '[data-state]' })).toBeInTheDocument();
    // The detail blob is where a FAILED backfill's reason lives, so it is
    // rendered rather than dropped as "free-form server data".
    expect(screen.getByText(/spec example-etl not found/)).toBeInTheDocument();
  });

  it('offers no pause or cancel once the backfill is terminal', async () => {
    mockFetch(() => [200, backfill({ state: 'CANCELLED', ended_at: '2026-09-12T11:00:00Z' })]);
    wrapBackfillDetail();

    await screen.findByText('CANCELLED', { selector: '[data-state]' });
    expect(screen.queryByRole('button', { name: 'Pause' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
  });

  it('reports a backfill that does not exist rather than rendering blank', async () => {
    mockFetch(() => [404, { message: 'no such backfill' }]);
    wrapBackfillDetail('missing');

    expect(await screen.findByRole('alert')).toHaveTextContent(/no such backfill/i);
  });
});


describe('editing a schedule', () => {
  it('prefills and saves the existing schedule without changing its identity', async () => {
    let current = schedule({ enabled: false, params: { shots: 20 }, spec_version: 2 });
    let saved: Record<string, unknown> | undefined;
    mockFetch((url, init) => {
      if (url.includes('/preview')) return [200, { valid: true, next_fires: [] }];
      if (url.includes('/backfills')) return [200, { backfills: [] }];
      if (url.includes('/workflows')) return [200, { workflows: [] }];
      if (init?.method === 'PUT') {
        expect(url).toContain('/schedules/nightly-etl');
        saved = JSON.parse(String(init.body));
        current = { ...current, ...saved } as Schedule;
      }
      return [200, current];
    });
    wrapScheduleDetail();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit schedule' }));
    expect(screen.getByDisplayValue('nightly-etl')).toHaveAttribute('readonly');
    expect(screen.getByDisplayValue('2')).toBeInTheDocument();
    fireEvent.change(screen.getByDisplayValue('0 2 * * *'), { target: { value: '0 4 * * *' } });
    await screen.findByDisplayValue('example-etl');
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(saved).toMatchObject({ name: 'nightly-etl', cron: '0 4 * * *', enabled: false, params: { shots: 20 }, spec_version: 2 }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Save changes' })).not.toBeInTheDocument());
  });

  it('retains the draft after a failed save and allows cancelling', async () => {
    let writes = 0;
    mockFetch((url, init) => {
      if (url.includes('/preview')) return [200, { valid: true, next_fires: [] }];
      if (url.includes('/backfills')) return [200, { backfills: [] }];
      if (url.includes('/workflows')) return [200, { workflows: [] }];
      if (init?.method === 'PUT') { writes++; return [500, { error: { message: 'Unavailable' } }]; }
      return [200, schedule()];
    });
    wrapScheduleDetail();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit schedule' }));
    fireEvent.change(screen.getByDisplayValue('0 2 * * *'), { target: { value: '0 5 * * *' } });
    await screen.findByDisplayValue('example-etl');
    fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await screen.findByRole('alert');
    expect(screen.getByDisplayValue('0 5 * * *')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(writes).toBe(1);
    expect(screen.queryByRole('button', { name: 'Save changes' })).not.toBeInTheDocument();
  });
});
