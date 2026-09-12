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
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
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

    fireEvent.click(await screen.findByRole('button', { name: 'Disable' }));

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
   * The whole point of the preview endpoint: a cron expression is write-only to
   * most people, so the form must show the instants it will actually fire at
   * BEFORE anything is created.
   */
  it('previews real fire times for the expression being typed', async () => {
    mockFetch((url) => {
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
      url.includes('/schedules/preview')
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
    fireEvent.change(screen.getByPlaceholderText('example-etl'), {
      target: { value: 'example-etl' },
    });
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
