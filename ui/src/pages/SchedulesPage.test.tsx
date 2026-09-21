import { PermissionContext } from '../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the schedule list and the create form.
 *
 * What is pinned here is the reasoning the screens exist for, not their markup:
 * that an enabled schedule with no armed fire time is CALLED OUT rather than
 * rendered as an em dash (it is the case where a replica died mid-fire and the
 * schedule will never fire again), that the create form previews real fire times
 * before anything is saved, and that an expression the preview has already
 * rejected cannot be submitted.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { SchedulesPage } from './SchedulesPage';
import type { Schedule } from '../api/types';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <PermissionContext.Provider value={["schedules.view","schedules.create","schedules.update","schedules.delete","backfills.view","backfills.create","backfills.update","backfills.cancel","runs.create","workflows.view"]}><QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
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
  ...over,
});

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the schedule list', () => {
  it('shows each schedule with the cron, its zone and the next fire time', async () => {
    mockFetch(() => [200, { schedules: [schedule()] }]);
    wrap(<SchedulesPage namespace="default" />);

    const row = (await screen.findByText('nightly-etl')).closest('tr')!;
    expect(within(row).getByText('0 2 * * *')).toBeInTheDocument();
    // The zone is part of the expression's meaning, not decoration: the same
    // five fields are a different instant in every zone.
    expect(within(row).getByText('UTC')).toBeInTheDocument();
    expect(within(row).getByText('enabled')).toBeInTheDocument();
  });

  /**
   * The row that matters most. next_fire_at is reported as stored, and a null on
   * an ENABLED schedule does not mean "disabled" — it means a replica is firing
   * it at this instant, or one died mid-fire and left it unarmed. The last case
   * is a schedule that looks healthy and will never fire again.
   */
  it('flags an enabled schedule that is not armed rather than showing a dash', async () => {
    mockFetch(() => [200, { schedules: [schedule({ enabled: true, next_fire_at: null })] }]);
    wrap(<SchedulesPage namespace="default" />);

    const row = (await screen.findByText('nightly-etl')).closest('tr')!;
    expect(within(row).getByText(/not armed/i)).toBeInTheDocument();
  });

  it('does not flag a disabled schedule as unarmed — being disabled explains it', async () => {
    mockFetch(() => [200, { schedules: [schedule({ enabled: false, next_fire_at: null })] }]);
    wrap(<SchedulesPage namespace="default" />);

    const row = (await screen.findByText('nightly-etl')).closest('tr')!;
    expect(within(row).getByText('disabled')).toBeInTheDocument();
    expect(within(row).queryByText(/not armed/i)).not.toBeInTheDocument();
  });

  /**
   * A stored expression that no longer parses cannot fire at all, and the list
   * is where someone looks when a schedule stops. The gateway reports the reason
   * as cron_error instead of next_fires, so the column must not read as "no
   * upcoming fires, probably fine".
   */
  it('reports a schedule whose stored cron no longer compiles', async () => {
    mockFetch(() => [
      200,
      {
        schedules: [
          schedule({ next_fire_at: null, cron_error: 'unknown time zone Mars/Olympus' }),
        ],
      },
    ]);
    wrap(<SchedulesPage namespace="default" />);

    const row = (await screen.findByText('nightly-etl')).closest('tr')!;
    expect(within(row).getByText(/cron broken/i)).toBeInTheDocument();
  });

  /**
   * The zone is rendered as the qualifier under the expression rather than as a
   * column of its own, so a restyle that dropped it would still pass a test
   * asserting only on the cron. Pinning that both are in the SAME cell keeps
   * them from drifting into separate columns, where the pairing is lost.
   */
  it('keeps the zone in the same cell as the expression it qualifies', async () => {
    mockFetch(() => [200, { schedules: [schedule({ timezone: 'Asia/Kolkata' })] }]);
    wrap(<SchedulesPage namespace="default" />);

    const row = (await screen.findByText('nightly-etl')).closest('tr')!;
    const cronCell = within(row).getByText('0 2 * * *').closest('td')!;
    expect(within(cronCell).getByText('Asia/Kolkata')).toBeInTheDocument();
  });

  it('teaches rather than blanking when the namespace has no schedules', async () => {
    mockFetch(() => [200, { schedules: [] }]);
    wrap(<SchedulesPage namespace="default" />);

    const empty = await screen.findByRole('status');
    expect(empty).toHaveTextContent(/No schedules in this namespace/i);
    expect(empty).toHaveTextContent(/nexusctl schedule create/);
  });

  it('surfaces a server failure instead of rendering an empty table', async () => {
    mockFetch(() => [500, { message: 'schedule store is down' }]);
    wrap(<SchedulesPage namespace="default" />);

    // useSchedules carries the shared retry policy, which retries a 5xx twice
    // before settling. Waiting it out asserts the REAL policy rather than a
    // test-only one, exactly as the workers and admin suites do.
    await waitFor(
      () => {
        expect(screen.getByRole('alert')).toHaveTextContent(/schedule store is down/i);
      },
      { timeout: 5000 },
    );
  });

  it('disables an enabled schedule through the enable/disable route', async () => {
    const calls: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        calls.push(url);
        return [200, schedule({ enabled: false, next_fire_at: null })];
      }
      return [200, { schedules: [schedule()] }];
    });
    wrap(<SchedulesPage namespace="default" />);

    fireEvent.click(await screen.findByRole('button', { name: 'Disable schedule' }));

    await waitFor(() => {
      expect(calls.some((u) => u.endsWith('/schedules/nightly-etl/disable'))).toBe(true);
    });
  });
});

describe('the create form', () => {
  async function openForm() {
    wrap(<SchedulesPage namespace="default" />);
    fireEvent.click(await screen.findByRole('button', { name: /new schedule/i }));
  }

  /**
   * The catalog fetch the workflow picker makes, which every test in this block
   * now provokes. Answered with a real list rather than an empty one so the
   * picker settles into its select and the form under test is the one an
   * operator with workflows actually sees.
   */
  function catalog(): [number, unknown] {
    return [
      200,
      {
        workflows: [
          {
            id: 'sp-1',
            namespace: 'default',
            name: 'example-etl',
            latest_version: 1,
            spec_hash: 'abc',
            created_by: 'ops',
            updated_at: '2026-09-12T10:00:00Z',
            version_count: 1,
            first_created_at: '2026-09-01T10:00:00Z',
          },
          {
            id: 'sp-2',
            namespace: 'default',
            name: 'ten-node-mixed',
            latest_version: 3,
            spec_hash: 'def',
            created_by: 'ops',
            updated_at: '2026-09-12T10:00:00Z',
            version_count: 3,
            first_created_at: '2026-09-01T10:00:00Z',
          },
        ],
      },
    ];
  }

  /**
   * The whole point of the preview endpoint: a cron expression is write-only to
   * most people, so the form must show the instants it will actually fire at
   * BEFORE anything is created.
   */
  it('previews real fire times for the expression being typed', async () => {
    mockFetch((url) => {
      if (url.includes('/specs')) return catalog();
      if (url.includes('/schedules/preview')) {
        return [
          200,
          {
            cron: '0 2 * * *',
            timezone: 'UTC',
            valid: true,
            next_fires: ['2026-09-13T02:00:00Z', '2026-09-14T02:00:00Z'],
          },
        ];
      }
      return [200, { schedules: [] }];
    });
    await openForm();

    // Rendered through fmtTs, so assert on the date rather than the raw RFC3339.
    expect(await screen.findByText(/2026-09-13/)).toBeInTheDocument();
    expect(screen.getByText(/2026-09-14/)).toBeInTheDocument();
  });

  /**
   * A bad expression comes back as 200 with valid:false, because "that
   * expression is wrong" IS the answer. It must read as a verdict on the
   * expression, never as a failed request — and it must block submission,
   * since the gateway would refuse it anyway.
   */
  it('explains an invalid expression and refuses to submit it', async () => {
    mockFetch((url) => {
      if (url.includes('/specs')) return catalog();
      if (url.includes('/schedules/preview')) {
        return [
          200,
          {
            cron: 'nope',
            timezone: 'UTC',
            valid: false,
            error: 'cron expression has 1 fields, want 5',
          },
        ];
      }
      return [200, { schedules: [] }];
    });
    await openForm();

    fireEvent.change(screen.getByLabelText('cron expression'), { target: { value: 'nope' } });

    expect(await screen.findByText(/has 1 fields, want 5/i)).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /create schedule/i })).toBeDisabled();
    });
  });

  it('refuses to submit params that are not a JSON object', async () => {
    mockFetch((url) =>
      url.includes('/specs')
        ? catalog()
        : url.includes('/schedules/preview')
          ? [200, { cron: '0 2 * * *', timezone: 'UTC', valid: true, next_fires: [] }]
          : [200, { schedules: [] }],
    );
    await openForm();

    fireEvent.change(screen.getByLabelText('params'), { target: { value: '[1,2]' } });

    expect(await screen.findByText(/params must be a JSON object/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /create schedule/i })).toBeDisabled();
  });

  it('posts the expression, its zone and the overlap policy on create', async () => {
    let posted: Record<string, unknown> | null = null;
    mockFetch((url, init) => {
      if (url.includes('/specs')) return catalog();
      if (url.includes('/schedules/preview')) {
        return [200, { cron: '0 2 * * *', timezone: 'UTC', valid: true, next_fires: [] }];
      }
      if (init?.method === 'POST') {
        posted = JSON.parse(String(init.body)) as Record<string, unknown>;
        return [201, schedule({ name: 'from-form' })];
      }
      return [200, { schedules: [] }];
    });
    await openForm();

    fireEvent.change(await screen.findByPlaceholderText('nightly-etl'), {
      target: { value: 'from-form' },
    });
    // The spec is now CHOSEN from the catalog rather than typed. Waiting for the
    // option rather than the select is deliberate: the select renders before the
    // catalog lands, and selecting a value it has no option for silently sets
    // nothing — which is exactly the bug a picker is supposed to end.
    await screen.findByRole('option', { name: /example-etl/ });
    fireEvent.change(screen.getByLabelText('workflow'), { target: { value: 'example-etl' } });
    fireEvent.click(screen.getByRole('button', { name: /create schedule/i }));

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted).toMatchObject({
      name: 'from-form',
      spec_name: 'example-etl',
      cron: '0 2 * * *',
      timezone: 'UTC',
      overlap_policy: 'skip',
      enabled: true,
    });
    // Blank means "always the latest version of the spec", so the field must be
    // absent rather than sent as null or zero.
    expect(posted).not.toHaveProperty('spec_version');
  });
});

/**
 * Scheduling reachable from the WORKFLOW side.
 *
 * The bug this closes is that there was no route into scheduling from a
 * workflow at all: an operator looking at a definition had to navigate to
 * Schedules and retype the name they had just been reading, which is the trip
 * that produces the typo the gateway does not catch. The form's open state and
 * its chosen workflow therefore live in the URL, so a workflow's own page can
 * link straight into a pre-filled form.
 */
describe('the create form as a deep link', () => {
  function wrapAt(path: string) {
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    return render(
      <PermissionContext.Provider value={["schedules.view","schedules.create","schedules.update","schedules.delete","backfills.view","backfills.create","backfills.update","backfills.cancel","runs.create","workflows.view"]}><QueryClientProvider client={qc}>
        <MemoryRouter initialEntries={[path]}>
          <SchedulesPage namespace="default" />
        </MemoryRouter>
      </QueryClientProvider></PermissionContext.Provider>,
    );
  }

  function catalogOnly(url: string): [number, unknown] {
    if (url.includes('/specs')) {
      return [
        200,
        {
          workflows: [
            {
              id: 'sp-1',
              namespace: 'default',
              name: 'example-etl',
              latest_version: 1,
              spec_hash: 'abc',
              created_by: 'ops',
              updated_at: '2026-09-12T10:00:00Z',
              version_count: 1,
              first_created_at: '2026-09-01T10:00:00Z',
            },
            {
              id: 'sp-2',
              namespace: 'default',
              name: 'ten-node-mixed',
              latest_version: 3,
              spec_hash: 'def',
              created_by: 'ops',
              updated_at: '2026-09-12T10:00:00Z',
              version_count: 3,
              first_created_at: '2026-09-01T10:00:00Z',
            },
          ],
        },
      ];
    }
    if (url.includes('/schedules/preview')) {
      return [200, { cron: '0 2 * * *', timezone: 'UTC', valid: true, next_fires: [] }];
    }
    return [200, { schedules: [] }];
  }

  it('opens the form already on the workflow the link named', async () => {
    mockFetch(catalogOnly);
    wrapAt('/schedules?new=1&spec=ten-node-mixed');

    const select = (await screen.findByLabelText('workflow')) as HTMLSelectElement;
    await waitFor(() => expect(select.value).toBe('ten-node-mixed'));
  });

  it('opens the form with nothing chosen when the link carries no spec', async () => {
    mockFetch(catalogOnly);
    wrapAt('/schedules?new=1');

    const select = (await screen.findByLabelText('workflow')) as HTMLSelectElement;
    // Explicitly unchosen rather than defaulted to the first workflow: a
    // schedule created for the alphabetically-first spec in the namespace is
    // one nobody asked for.
    expect(select.value).toBe('');
  });

  /**
   * A `spec` with no `new` must not open a form. The two parameters answer
   * different questions — one opens the form, the other says which workflow it
   * starts on — and a stale spec left in a URL should do nothing rather than
   * popping open a form the operator did not ask for.
   */
  it('does not open the form for a spec parameter on its own', async () => {
    mockFetch(catalogOnly);
    wrapAt('/schedules?spec=ten-node-mixed');

    await screen.findByRole('button', { name: /new schedule/i });
    expect(screen.queryByLabelText('workflow')).not.toBeInTheDocument();
  });

  /**
   * The end of the round trip. The gateway validates the cron at SUBMIT time
   * and its rejection names the field and the offending token — far more use
   * than "invalid cron" — so that message has to reach the screen verbatim
   * rather than being flattened into a generic failure.
   *
   * This is a DIFFERENT path from the preview's valid:false, which the form
   * catches before submission. Reached here by a preview that says the
   * expression is fine and a gateway that rejects it anyway, which is exactly
   * what a preview served by a different code path could produce.
   */
  it("shows the gateway's field-and-token message when it rejects the cron", async () => {
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        return [400, { error: 'invalid_cron', message: 'hour field value 99 is out of range (0-23)' }];
      }
      return catalogOnly(url);
    });
    wrapAt('/schedules?new=1&spec=example-etl');

    fireEvent.change(await screen.findByPlaceholderText('nightly-etl'), {
      target: { value: 'from-link' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create schedule/i }));

    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(
        /hour field value 99 is out of range/i,
      );
    });
  });

  /**
   * The free-text path's round trip, and the reason the picker's escape hatch is
   * safe to keep.
   *
   * The picker prevents a typo for anyone choosing from the catalog, but it
   * deliberately falls back to a plain input — the catalog fetch is capped and
   * can fail, and making a valid spec name untypeable would turn a degraded read
   * into a blocked write. So a name nobody verified CAN still be submitted, and
   * the gateway is what catches it: it rejects an unknown spec_name with a 400
   * naming the field and the value, exactly as it does for a bad cron.
   *
   * What is pinned here is that the message survives the trip. `serverMessage`
   * prefers the body's `message` over a status-derived sentence, so the operator
   * reads the name they actually typed rather than "Bad Request" — which would
   * leave them with a rejected form and no idea which field was wrong.
   */
  it("shows the gateway's rejection of a spec name typed into the free-text path", async () => {
    let posted: Record<string, unknown> | null = null;
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posted = JSON.parse(String(init.body)) as Record<string, unknown>;
        return [
          400,
          {
            error: 'unknown_spec_name',
            message:
              'spec_name "ten-node-mixd" does not name a workflow in namespace "default"; a schedule resolves this name every time it fires, so one that matches nothing would arm, fire on time and produce no runs',
          },
        ];
      }
      // The catalog fails, which is what forces the picker into free text — the
      // one mode in which an unverified name can reach the gateway at all. A 403
      // rather than a 5xx because useWorkflows retries 5xx twice: the retries
      // would leave the query still settling when the assertions run, and the
      // point here is the picker's terminal failed state, not its retry policy.
      if (url.includes('/specs')) return [403, { error: 'forbidden', message: 'no catalog access' }];
      return catalogOnly(url);
    });
    wrapAt('/schedules?new=1');

    // A text input, not a select: the catalog could not be read, so the console
    // has established nothing about what exists and must let the name be typed.
    // Waited for rather than read once — the control starts as a select while the
    // catalog fetch is still in flight and only becomes an input once the fetch
    // has actually failed, so reading it immediately would catch the wrong one.
    await waitFor(() =>
      expect((screen.getByLabelText('workflow') as HTMLElement).tagName).toBe('INPUT'),
    );
    const workflow = screen.getByLabelText('workflow') as HTMLInputElement;
    fireEvent.change(workflow, { target: { value: 'ten-node-mixd' } });
    fireEvent.change(screen.getByPlaceholderText('nightly-etl'), {
      target: { value: 'typo-probe' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create schedule/i }));

    await waitFor(() => expect(posted).not.toBeNull());
    expect(posted).toMatchObject({ spec_name: 'ten-node-mixd' });

    // The SERVER's sentence, not a generic failure. The typed name must appear:
    // an error that does not quote it back leaves the operator diffing by eye.
    await waitFor(() => {
      const alerts = screen.getAllByRole('alert');
      expect(alerts.some((el) => /ten-node-mixd/.test(el.textContent ?? ''))).toBe(true);
      expect(alerts.some((el) => /does not name a workflow/.test(el.textContent ?? ''))).toBe(true);
    });
  });

  it('sends the linked workflow as spec_name and shows the schedule it created', async () => {
    let posted: Record<string, unknown> | null = null;
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posted = JSON.parse(String(init.body)) as Record<string, unknown>;
        return [201, schedule({ name: 'from-link', spec_name: 'ten-node-mixed' })];
      }
      if (url.includes('/schedules?') || url.endsWith('/schedules')) {
        // After the create, the list is invalidated and refetched — the new row
        // must actually appear, which is the operator's "did it take?".
        return [200, { schedules: posted ? [schedule({ name: 'from-link' })] : [] }];
      }
      return catalogOnly(url);
    });
    wrapAt('/schedules?new=1&spec=ten-node-mixed');

    fireEvent.change(await screen.findByPlaceholderText('nightly-etl'), {
      target: { value: 'from-link' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create schedule/i }));

    await waitFor(() => expect(posted).not.toBeNull());
    // The whole point of the deep link: the workflow chosen on the OTHER page is
    // the one that gets scheduled, with nobody retyping it.
    expect(posted).toMatchObject({ name: 'from-link', spec_name: 'ten-node-mixed' });

    // And it must actually turn up in the list, which is the operator's real
    // question after pressing create. The create invalidates the schedules
    // query, so the row arriving is the whole round trip working — not just the
    // POST being accepted.
    expect(await screen.findByText('from-link')).toBeInTheDocument();
  });
});
