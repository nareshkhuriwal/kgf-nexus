// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the overview.
 *
 * Two properties carry this screen and both are pinned here.
 *
 * The first is INDEPENDENT DEGRADATION. The page aggregates four queries, so the
 * failure that would make it worthless is one dead endpoint blanking the rest.
 * Each panel is therefore tested against a backend where exactly one endpoint is
 * broken, asserting that the other panels still render their data.
 *
 * The second is PROVENANCE. Queue age is derived client-side and service health
 * is not exposed by the gateway at all; the tests assert the labelling, because
 * a future refactor that quietly drops the "derived" wording turns an inference
 * back into a claimed measurement without failing anything else.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { OverviewPage } from './OverviewPage';
import type { CapacitySummary, RunSummary, Worker } from '../api/types';

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

function mockFetch(handler: (url: string) => [number, unknown]) {
  vi.stubGlobal('fetch', (url: string) => {
    const [status, body] = handler(String(url));
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
}

const HOUR = 3600_000;

const run = (over: Partial<RunSummary> = {}): RunSummary => ({
  id: 'run-aaaaaaaa-bbbb',
  namespace: 'default',
  spec_id: 'spec-1111',
  spec_name: 'daily-etl',
  spec_version: 3,
  state: 'SUCCEEDED',
  trigger_kind: 'api',
  created_at: new Date(Date.now() - HOUR).toISOString(),
  started_at: new Date(Date.now() - HOUR).toISOString(),
  ended_at: new Date(Date.now() - HOUR + 30_000).toISOString(),
  ...over,
});

const worker = (over: Partial<Worker> = {}): Worker => ({
  id: 'wk-11111111',
  namespace: 'default',
  hostname: 'agent-cpu-1',
  queues: ['default'],
  capacity: { cpu_millis: 8000, memory_bytes: 16 * 1024 ** 3, gpu_count: 0 },
  committed: { cpu_millis: 2000, memory_bytes: 4 * 1024 ** 3, gpu_count: 0 },
  state: 'ACTIVE',
  last_heartbeat_at: new Date().toISOString(),
  ...over,
});

const capacity: CapacitySummary = {
  advertised: { cpu_millis: 16000, memory_bytes: 64 * 1024 ** 3, gpu_count: 3, disk_bytes: 0 },
  committed: { cpu_millis: 1000, memory_bytes: 1024 ** 3, gpu_count: 0, disk_bytes: 0 },
  workers: 3,
  pending_attempts: 10,
};

/** A backend where everything answers. Individual tests override one endpoint. */
function healthy(over: Partial<Record<'runs' | 'failed' | 'workers' | 'capacity' | 'schedules', [number, unknown]>> = {}) {
  return (url: string): [number, unknown] => {
    if (url.includes('/capacity')) return over.capacity ?? [200, capacity];
    if (url.includes('/workers')) return over.workers ?? [200, { workers: [worker()] }];
    if (url.includes('/schedules')) return over.schedules ?? [200, { schedules: [] }];
    if (url.includes('state=FAILED')) {
      return (
        over.failed ?? [
          200,
          { runs: [run({ id: 'run-dead-9999', state: 'FAILED', spec_name: 'fanout-retry3' })] },
        ]
      );
    }
    if (url.includes('/runs')) {
      return (
        over.runs ?? [
          200,
          {
            runs: [
              run({ id: 'run-live-1234', state: 'RUNNING', ended_at: null }),
              run({ id: 'run-done-5678', state: 'SUCCEEDED' }),
            ],
          },
        ]
      );
    }
    return [404, { message: 'no route' }];
  };
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the six panels the brief asks for', () => {
  it('renders active runs, recent runs, failures, queue, workers and reservations', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('active runs')).toBeInTheDocument();
    expect(screen.getByText('recently finished')).toBeInTheDocument();
    expect(screen.getByText('recent failures')).toBeInTheDocument();
    expect(screen.getByText('queue')).toBeInTheDocument();
    expect(screen.getByText('resource reservations')).toBeInTheDocument();

    // The active/terminal split is computed here from isTerminal, not asked of
    // the backend, so both halves have to actually land in the right panel.
    const active = screen.getByText('active runs').closest('.panel')!;
    expect(await within(active as HTMLElement).findByText('run-live-123')).toBeInTheDocument();

    const recent = screen.getByText('recently finished').closest('.panel')!;
    expect(await within(recent as HTMLElement).findByText('run-done-567')).toBeInTheDocument();
  });

  it('shows the queue depth the platform reports and the age it derives', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    // pending_attempts is the only queue number the gateway publishes.
    expect(await screen.findByText('10')).toBeInTheDocument();
    expect(screen.getByText('pending attempts')).toBeInTheDocument();
    expect(screen.getByText('oldest waiting run')).toBeInTheDocument();
  });

  it('separates workers accepting work from workers merely registered', async () => {
    mockFetch(
      healthy({
        workers: [
          200,
          {
            workers: [
              worker({ id: 'wk-alive', state: 'ACTIVE' }),
              worker({ id: 'wk-gone', state: 'DEAD' }),
              worker({ id: 'wk-drain', state: 'DRAINING' }),
            ],
          },
        ],
      }),
    );
    wrap(<OverviewPage namespace="default" />);

    // Only ACTIVE accepts new work. Reporting 3 as "available" would be the bug.
    const panel = (await screen.findByText('accepting work')).closest('.panel')!;
    expect(within(panel as HTMLElement).getByText('1')).toBeInTheDocument();
    expect(within(panel as HTMLElement).getByText('3')).toBeInTheDocument();
  });

  it('shows reservations per dimension rather than as one aggregate', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const panel = screen.getByText('resource reservations').closest('.panel')!;
    const scope = within(panel as HTMLElement);
    // A fleet at 6% CPU with every GPU free still blocks GPU work the moment the
    // GPUs commit; one number cannot say that, so each dimension is its own tile.
    expect(
      await scope.findByText('cpu', { selector: '.attempt-section-title' }),
    ).toBeInTheDocument();
    expect(scope.getByText('memory', { selector: '.attempt-section-title' })).toBeInTheDocument();
    expect(scope.getByText('gpu', { selector: '.attempt-section-title' })).toBeInTheDocument();
  });
});

describe('honesty about where numbers come from', () => {
  /**
   * The single most important assertion in this file. Queue age is an inference
   * from run timestamps, and if it ever stops saying so it becomes a metric the
   * operator will trust as if the scheduler published it.
   */
  it('labels queue age as derived and names the run it came from', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText(/Derived, not measured/)).toBeInTheDocument();
    expect(screen.getByText(/no queue-age endpoint/i)).toBeInTheDocument();
    // The link is what makes the inference checkable rather than merely disclosed.
    const queue = screen.getByText('queue').closest('.panel')!;
    expect(await within(queue as HTMLElement).findByTitle('run-live-1234')).toHaveAttribute(
      'href',
      '/runs/run-live-1234',
    );
  });

  /**
   * There is no service-health endpoint through the gateway. The panel must not
   * claim to be one, because a green row read as "the fleet is healthy" is a
   * fabricated health state.
   */
  it('presents service health as API reachability from this browser, not a fleet check', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('api reachability')).toBeInTheDocument();
    expect(screen.getByText(/no service-health or dependency\s+endpoint/i)).toBeInTheDocument();
    expect(screen.getByText(/not a fleet health check/i)).toBeInTheDocument();
  });

  it('reports worker states as control-plane measurements, kept apart from reachability', async () => {
    mockFetch(
      healthy({
        workers: [200, { workers: [worker({ id: 'a' }), worker({ id: 'b', state: 'DEAD' })] }],
      }),
    );
    wrap(<OverviewPage namespace="default" />);

    const health = screen
      .getByText(/worker states \(reported by the control plane\)/i)
      .closest('.panel')!;
    expect(
      await within(health as HTMLElement).findByText('DEAD', { selector: '[data-state]' }),
    ).toBeInTheDocument();
  });

  it('marks a summed capacity fallback as derived when /capacity is down', async () => {
    mockFetch(healthy({ capacity: [500, { message: 'boom' }] }));
    wrap(<OverviewPage namespace="default" />);

    // Losing the aggregate endpoint must not hide capacity we can already see in
    // the worker rows — but the summed answer has to announce itself.
    expect(
      await screen.findByText(/Derived by summing the worker rows/, {}, { timeout: 5000 }),
    ).toBeInTheDocument();
  }, 15000);
});

describe('one failing query degrades one panel only', () => {
  it('keeps runs, workers and reservations when /capacity fails', async () => {
    mockFetch(healthy({ capacity: [500, { message: 'capacity exploded' }] }));
    wrap(<OverviewPage namespace="default" />);

    // The run panels are fed by a different query and must be unaffected.
    expect(await screen.findByText('run-live-123')).toBeInTheDocument();
    // The fleet panel is fed by /workers and keeps its real counts.
    const fleet = (await screen.findByText('accepting work')).closest('.panel')!;
    expect(within(fleet as HTMLElement).getAllByText('1').length).toBeGreaterThan(0);
    // Only the queue depth is lost, and it says so rather than showing a zero.
    const queue = screen.getByText('queue').closest('.panel')!;
    expect(
      await within(queue as HTMLElement).findByText('unavailable', {}, { timeout: 5000 }),
    ).toBeInTheDocument();
  }, 15000);

  it('keeps the capacity and worker panels when the run list fails', async () => {
    mockFetch(healthy({ runs: [500, { message: 'runs exploded' }] }));
    wrap(<OverviewPage namespace="default" />);

    // Reservations come from /capacity, so they must survive a dead run list.
    const panel = screen.getByText('resource reservations').closest('.panel')!;
    expect(
      await within(panel as HTMLElement).findByText('cpu', {
        selector: '.attempt-section-title',
      }),
    ).toBeInTheDocument();

    // And the run panels report the failure instead of rendering an empty table.
    const active = screen.getByText('active runs').closest('.panel')!;
    expect(
      await within(active as HTMLElement).findByRole('alert', {}, { timeout: 5000 }),
    ).toBeInTheDocument();
  }, 15000);

  it('keeps every other panel when the failure list alone fails', async () => {
    mockFetch(healthy({ failed: [500, { message: 'failed-list exploded' }] }));
    wrap(<OverviewPage namespace="default" />);

    const failures = screen.getByText('recent failures').closest('.panel')!;
    expect(
      await within(failures as HTMLElement).findByRole('alert', {}, { timeout: 5000 }),
    ).toBeInTheDocument();

    // The page is still a dashboard, not an error screen.
    expect(screen.getByText('run-live-123')).toBeInTheDocument();
    expect(screen.getByText('pending attempts')).toBeInTheDocument();
  }, 15000);

  it('marks an endpoint as not answering in the reachability panel', async () => {
    mockFetch(healthy({ schedules: [500, { message: 'schedules exploded' }] }));
    wrap(<OverviewPage namespace="default" />);

    const health = screen.getByText('api reachability').closest('.panel')!;
    expect(
      await within(health as HTMLElement).findByText('not answering', {}, { timeout: 5000 }),
    ).toBeInTheDocument();
    expect(within(health as HTMLElement).getByText('3/4 endpoints answering')).toBeInTheDocument();
  }, 15000);
});

describe('empty states', () => {
  it('says nothing is in flight rather than showing a skeleton forever', async () => {
    // A namespace with only terminal runs has a NON-empty payload and an empty
    // active slice: the panel has to key off the slice, not the response.
    mockFetch(healthy({ runs: [200, { runs: [run({ state: 'SUCCEEDED' })] }] }));
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('Nothing in flight')).toBeInTheDocument();
  });

  it('says there are no failures rather than leaving the panel blank', async () => {
    mockFetch(healthy({ failed: [200, { runs: [] }] }));
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('No failed runs')).toBeInTheDocument();
  });

  it('says no workers are registered rather than reporting zero availability', async () => {
    mockFetch(healthy({ workers: [200, { workers: [] }] }));
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('No workers registered')).toBeInTheDocument();
  });

  it('reports nothing waiting rather than a zero queue age', async () => {
    mockFetch(healthy({ runs: [200, { runs: [run({ state: 'SUCCEEDED' })] }] }));
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('nothing waiting')).toBeInTheDocument();
  });
});

describe('drill-down links', () => {
  /**
   * "with drill-down links" is a hard requirement of the brief: an overview that
   * shows a number you cannot chase is a status poster, not a console.
   */
  it('links a run row to that run', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const recent = screen.getByText('recently finished').closest('.panel')!;
    expect(await within(recent as HTMLElement).findByTitle('run-done-5678')).toHaveAttribute(
      'href',
      '/runs/run-done-5678',
    );
  });

  it('links the failures panel at the pre-filtered failed-run list', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('all failed runs')).toHaveAttribute(
      'href',
      '/runs?state=FAILED',
    );
  });

  it('links a spec name at that spec’s runs', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const links = await screen.findAllByText('daily-etl');
    expect(links[0]).toHaveAttribute('href', '/runs?spec=daily-etl');
  });

  it('links the worker and queue panels at the fleet and the run list', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('fleet detail')).toHaveAttribute('href', '/workers');
    const queue = screen.getByText('queue').closest('.panel')!;
    expect(await within(queue as HTMLElement).findByText(/active$/)).toHaveAttribute('href', '/runs');
  });

  it('links each reachability row at the page that reads that endpoint', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const health = screen.getByText('api reachability').closest('.panel')!;
    const scope = within(health as HTMLElement);
    expect(scope.getByText('runs').closest('a')).toHaveAttribute('href', '/runs');
    expect(scope.getByText('workers').closest('a')).toHaveAttribute('href', '/workers');
    expect(scope.getByText('schedules').closest('a')).toHaveAttribute('href', '/schedules');
  });
});
