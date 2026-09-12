// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the overview.
 *
 * Three properties carry this screen and all three are pinned here.
 *
 * The first is INDEPENDENT DEGRADATION. The page aggregates six queries, so the
 * failure that would make it worthless is one dead endpoint blanking the rest.
 * Each panel is therefore tested against a backend where exactly one endpoint is
 * broken, asserting that the other panels still render their data.
 *
 * The second is PROVENANCE. Queue age is derived client-side, and fleet health
 * measures gRPC connectivity rather than readiness; the tests assert the
 * labelling, because a future refactor that quietly drops the "derived" wording —
 * or relabels connectivity as health — turns a narrow claim into a broad one
 * without failing anything else.
 *
 * The third is the TRIAGE SEMANTICS the server owns and the console must not
 * re-invent: server order is preserved, `status` is not recomputed, and the two
 * states that are NOT faults (IDLE, unconfigured) are not drawn as faults. Each
 * has its own test, because each is a lie the page could tell while still
 * looking correct.
 */


import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { OverviewPage } from './OverviewPage';
import type {
  AttentionItem,
  AttentionList,
  CapacitySummary,
  FleetHealth,
  RunSummary,
  Worker,
} from '../api/types';

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

const attentionItem = (over: Partial<AttentionItem> = {}): AttentionItem => ({
  kind: 'run_failed',
  severity: 'warning',
  summary: 'Run failed on daily-etl',
  detail: 'The run reached FAILED.',
  link: '/runs/run-att-0001',
  run_id: 'run-att-0001',
  run_state: 'FAILED',
  spec_name: 'daily-etl',
  occurred_at: new Date(Date.now() - HOUR).toISOString(),
  ...over,
});

/**
 * The ranked list the server returns. The ORDER here is the assertion in the
 * first triage test: critical is not first, deliberately, so a console that
 * quietly sorted by severity would fail rather than accidentally agree.
 */
const attention: AttentionList = {
  count: 3,
  truncated: false,
  items: [
    attentionItem({
      summary: 'Retries exhausted on ten-node-mixed / train',
      kind: 'retry_exhausted',
      node_id: 'train',
      attempt_id: 'att-1111',
      reason: 'attempts_exhausted',
      link: '/runs/run-att-0001?attempt=att-1111',
    }),
    attentionItem({
      summary: 'Outcome indeterminate on billing-export / charge',
      kind: 'indeterminate_outcome',
      severity: 'critical',
      detail: 'The worker died mid-attempt; the platform cannot tell whether the charge landed.',
      node_id: 'charge',
      reason: 'indeterminate_outcome_requires_human',
      run_id: 'run-att-0002',
      link: '/runs/run-att-0002?attempt=att-2222',
      spec_name: 'billing-export',
    }),
    attentionItem({
      summary: 'Attempt blocked on capacity on ten-node-mixed / train_large',
      kind: 'blocked_on_capacity',
      run_id: 'run-att-0003',
      link: '/runs/run-att-0003?attempt=att-3333',
    }),
  ],
};

const fleet: FleetHealth = {
  status: 'ok',
  measures:
    'gRPC connectivity from this gateway, read without a network round trip; not each service’s own /readyz',
  upstreams: [
    { name: 'scheduler', addr: 'scheduler:9090', state: 'READY', configured: true },
    { name: 'dag-engine', addr: 'dag-engine:9090', state: 'IDLE', configured: true },
    { name: 'notifier', addr: '', state: 'UNCONFIGURED', configured: false },
  ],
};

/** A backend where everything answers. Individual tests override one endpoint. */
function healthy(
  over: Partial<
    Record<
      'runs' | 'failed' | 'workers' | 'capacity' | 'schedules' | 'attention' | 'fleet',
      [number, unknown]
    >
  > = {},
) {
  return (url: string): [number, unknown] => {
    // Before /capacity: "/v1/health/fleet" contains neither, but ordering the
    // most specific routes first keeps this readable as the endpoint list grows.
    if (url.includes('/health/fleet')) return over.fleet ?? [200, fleet];
    if (url.includes('/attention')) return over.attention ?? [200, attention];
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
   * The endpoint is real now, but what it measures is still narrower than
   * "health": a dialled gRPC channel is not a ready service. The panel must keep
   * saying so, because a green row read as "the fleet is well" is exactly the
   * fabricated state the old reachability panel existed to avoid claiming.
   */
  it('presents fleet health as connectivity, not as service readiness', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('fleet connectivity')).toBeInTheDocument();
    expect(await screen.findByText(/Connectivity, not readiness/)).toBeInTheDocument();
    // The payload's own sentence, surfaced rather than paraphrased.
    expect(screen.getByText(/not each service’s own \/readyz/)).toBeInTheDocument();
    expect(screen.getByText(/does not prove the service behind it is serving/)).toBeInTheDocument();
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

  it('keeps every panel when the triage list alone fails', async () => {
    mockFetch(healthy({ attention: [500, { message: 'attention exploded' }] }));
    wrap(<OverviewPage namespace="default" />);

    const attn = screen.getByText('needs attention').closest('.panel')!;
    expect(
      await within(attn as HTMLElement).findByRole('alert', {}, { timeout: 5000 }),
    ).toBeInTheDocument();

    // This is the whole point of the page: the most prominent panel dying must
    // not cost the operator the context underneath it.
    expect(await screen.findByText('run-live-123')).toBeInTheDocument();
    expect(screen.getByText('pending attempts')).toBeInTheDocument();
    expect(await screen.findByText('scheduler')).toBeInTheDocument();
  }, 15000);

  it('keeps the triage list when fleet connectivity fails', async () => {
    mockFetch(healthy({ fleet: [500, { message: 'fleet exploded' }] }));
    wrap(<OverviewPage namespace="default" />);

    const fleetPanel = screen.getByText('fleet connectivity').closest('.panel')!;
    expect(
      await within(fleetPanel as HTMLElement).findByRole('alert', {}, { timeout: 5000 }),
    ).toBeInTheDocument();

    expect(
      await screen.findByText('Retries exhausted on ten-node-mixed / train'),
    ).toBeInTheDocument();
  }, 15000);
});

describe('the triage list the operator came for', () => {
  /**
   * The single most important assertion about this panel. The server's ranking is
   * deterministic and computed where the data is; a console that re-sorted would
   * substitute a local heuristic and make two browsers disagree about what is
   * most urgent. The fixture deliberately puts a WARNING before a CRITICAL, so a
   * severity sort would fail here rather than coincidentally pass.
   */
  it('renders items in the order the server returned them', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    await screen.findByText('Retries exhausted on ten-node-mixed / train');
    const panel = screen.getByText('needs attention').closest('.panel')!;
    const rows = within(panel as HTMLElement)
      .getAllByRole('listitem')
      .map((li) => li.querySelector('.ov-attn-summary')!.textContent);

    expect(rows).toEqual([
      'Retries exhausted on ten-node-mixed / train',
      'Outcome indeterminate on billing-export / charge',
      'Attempt blocked on capacity on ten-node-mixed / train_large',
    ]);
  });

  it('distinguishes critical from warning by more than one signal', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const crit = (
      await screen.findByText('Outcome indeterminate on billing-export / charge')
    ).closest('li')!;
    const warn = screen
      .getByText('Retries exhausted on ten-node-mixed / train')
      .closest('li')!;

    expect(crit.className).toContain('critical');
    expect(warn.className).toContain('warning');
    // Never colour alone: the severity word is in the row, and the dot carries
    // its own class, so the distinction survives a monochrome reader.
    expect(within(crit as HTMLElement).getByText('critical')).toBeInTheDocument();
    expect(within(warn as HTMLElement).getByText('warning')).toBeInTheDocument();
    expect(crit.querySelector('.ov-attn-dot.critical')).not.toBeNull();
    expect(warn.querySelector('.ov-attn-dot.warning')).not.toBeNull();
  });

  it('renders the detail sentence and the server-resolved link verbatim', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const row = (await screen.findByText('Retries exhausted on ten-node-mixed / train')).closest(
      'li',
    )!;
    expect(within(row as HTMLElement).getByText('The run reached FAILED.')).toBeInTheDocument();
    // Verbatim, including the attempt query string: the server decided whether
    // this item is about the run or one attempt inside it.
    expect(within(row as HTMLElement).getByText('open')).toHaveAttribute(
      'href',
      '/runs/run-att-0001?attempt=att-1111',
    );
  });

  /**
   * The force-retry affordance is gated on the REASON, not the kind. An
   * indeterminate_outcome KIND says the platform could not tell what happened;
   * only this REASON says the platform has decided a human must choose. Offering
   * a destructive re-run off the kind would offer it for items nobody was asked
   * about, so both halves are asserted.
   */
  it('offers the force-retry guidance only for the reason that asks for a human', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const human = (
      await screen.findByText('Outcome indeterminate on billing-export / charge')
    ).closest('li')!;
    expect(within(human as HTMLElement).getByText(/--force/)).toBeInTheDocument();

    // Same severity class of problem, different reason: no force affordance.
    const exhausted = screen
      .getByText('Retries exhausted on ten-node-mixed / train')
      .closest('li')!;
    expect(within(exhausted as HTMLElement).queryByText(/--force/)).toBeNull();
  });

  it('offers no force-retry when the kind matches but the reason does not', async () => {
    // The trap this test exists for: kind === 'indeterminate_outcome' with a
    // reason the server did NOT flag for a human.
    mockFetch(
      healthy({
        attention: [
          200,
          {
            count: 1,
            truncated: false,
            items: [
              attentionItem({
                kind: 'indeterminate_outcome',
                severity: 'critical',
                summary: 'Outcome indeterminate, resolved automatically',
                reason: 'worker_reconnected',
              }),
            ],
          },
        ],
      }),
    );
    wrap(<OverviewPage namespace="default" />);

    await screen.findByText('Outcome indeterminate, resolved automatically');
    expect(screen.queryByText(/--force/)).toBeNull();
  });

  /**
   * An empty triage list is the best news this page can deliver. It must not
   * render with the console's "no data" vocabulary, because an operator who
   * cannot tell "we checked, you are fine" from "we have nothing to show you"
   * cannot trust the panel when it is empty — which is most of the time.
   */
  it('reads an empty list as good news rather than as missing data', async () => {
    mockFetch(healthy({ attention: [200, { count: 0, truncated: false, items: [] }] }));
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('Nothing needs attention')).toBeInTheDocument();
    expect(screen.getByText(/No run has failed or stalled/)).toBeInTheDocument();

    const panel = screen.getByText('needs attention').closest('.panel')!;
    // Not an error, and not the ∅ absence treatment.
    expect(within(panel as HTMLElement).queryByRole('alert')).toBeNull();
    expect(panel.querySelector('.notice.empty')).toBeNull();
    expect(panel.querySelector('.ov-allclear')).not.toBeNull();
  });

  it('offers a view-all affordance only when the server says the list is truncated', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);
    await screen.findByText('Retries exhausted on ten-node-mixed / train');
    expect(screen.queryByText(/view all runs needing attention/)).toBeNull();

    vi.unstubAllGlobals();
    mockFetch(healthy({ attention: [200, { ...attention, truncated: true }] }));
    wrap(<OverviewPage namespace="default" />);
    expect(await screen.findByText(/view all runs needing attention/)).toBeInTheDocument();
  });

  it('renders a 403 as a permission answer rather than a retryable failure', async () => {
    mockFetch(healthy({ attention: [403, { message: 'admin only' }] }));
    wrap(<OverviewPage namespace="default" />);

    // A 403 cannot be retried away, so it must not arrive with a Retry button.
    expect(
      await screen.findByText(/Not permitted: the triage list/, {}, { timeout: 5000 }),
    ).toBeInTheDocument();
    const panel = screen.getByText('needs attention').closest('.panel')!;
    expect(within(panel as HTMLElement).queryByRole('button', { name: 'Retry' })).toBeNull();
  }, 15000);
});

describe('fleet connectivity semantics the server owns', () => {
  it('renders one row per upstream service with its address', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const panel = (await screen.findByText('fleet connectivity')).closest('.panel')!;
    const scope = within(panel as HTMLElement);
    expect(await scope.findByText('scheduler')).toBeInTheDocument();
    expect(scope.getByText('dag-engine')).toBeInTheDocument();
    expect(scope.getByText('scheduler:9090')).toBeInTheDocument();
    expect(scope.getByText('READY')).toBeInTheDocument();
  });

  /**
   * IDLE means the channel has not been dialled since startup, and
   * configured:false means nobody asked for that upstream. Painting either red
   * would make a correct single-queue deployment render as a broken one, and
   * would train operators to ignore the panel.
   */
  it('does not draw IDLE or an unconfigured upstream as a fault', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const idle = (await screen.findByText('dag-engine')).closest('.ov-upstream')!;
    expect(idle.className).not.toContain('critical');
    expect(idle.querySelector('.ov-attn-dot.critical')).toBeNull();
    expect(idle.querySelector('.ov-attn-dot.idle')).not.toBeNull();

    const unconfigured = screen.getByText('notifier').closest('.ov-upstream')!;
    expect(unconfigured.className).not.toContain('critical');
    // Its state field is not a claim about anything, so the row says why it is
    // quiet instead of reporting a channel state nobody asked for.
    expect(within(unconfigured as HTMLElement).getByText('not configured')).toBeInTheDocument();
  });

  it('does draw a genuinely failed channel as a fault', async () => {
    mockFetch(
      healthy({
        fleet: [
          200,
          {
            ...fleet,
            status: 'degraded',
            upstreams: [
              { name: 'scheduler', addr: 'scheduler:9090', state: 'TRANSIENT_FAILURE', configured: true },
            ],
          },
        ],
      }),
    );
    wrap(<OverviewPage namespace="default" />);

    const row = (await screen.findByText('scheduler')).closest('.ov-upstream')!;
    expect(row.className).toContain('critical');
    expect(row.querySelector('.ov-attn-dot.critical')).not.toBeNull();
  });

  /**
   * `status` is the gateway's verdict: it knows which upstreams this deployment
   * REQUIRES, and the console does not. Recomputing it from the rows would guess
   * that, and would guess wrong on exactly the deployments where it matters.
   */
  it('shows degraded when the server says degraded, even with every row benign', async () => {
    mockFetch(
      healthy({
        fleet: [
          200,
          {
            ...fleet,
            status: 'degraded',
            // Every row is READY or IDLE. A client-side rollup would call this
            // "ok" and contradict the server.
            upstreams: [
              { name: 'scheduler', addr: 'scheduler:9090', state: 'READY', configured: true },
              { name: 'dag-engine', addr: 'dag-engine:9090', state: 'IDLE', configured: true },
            ],
          },
        ],
      }),
    );
    wrap(<OverviewPage namespace="default" />);

    const panel = (await screen.findByText('fleet connectivity')).closest('.panel')!;
    expect(await within(panel as HTMLElement).findByText('degraded')).toBeInTheDocument();
  });

  it('does not invent an ok verdict from healthy-looking rows when status is absent', async () => {
    mockFetch(healthy({ fleet: [200, { upstreams: fleet.upstreams }] }));
    wrap(<OverviewPage namespace="default" />);

    const panel = (await screen.findByText('fleet connectivity')).closest('.panel')!;
    const scope = within(panel as HTMLElement);
    expect(await scope.findByText('unknown')).toBeInTheDocument();
    expect(scope.queryByText('ok')).toBeNull();
  });

  it('renders a 403 on fleet health as a permission answer', async () => {
    mockFetch(healthy({ fleet: [403, { message: 'admin only' }] }));
    wrap(<OverviewPage namespace="default" />);

    expect(
      await screen.findByText(/Not permitted: fleet connectivity/, {}, { timeout: 5000 }),
    ).toBeInTheDocument();
  }, 15000);

  it('keeps the worker-state tallies visible when fleet connectivity is down', async () => {
    // The two halves of this panel come from different endpoints on purpose:
    // channel states are a claim about connectivity, worker states are a real
    // control-plane measurement, and losing one must not cost the other.
    mockFetch(
      healthy({
        fleet: [500, { message: 'fleet exploded' }],
        workers: [200, { workers: [worker({ id: 'a' }), worker({ id: 'b', state: 'DEAD' })] }],
      }),
    );
    wrap(<OverviewPage namespace="default" />);

    const panel = screen.getByText('fleet connectivity').closest('.panel')!;
    expect(
      await within(panel as HTMLElement).findByText(
        'DEAD',
        { selector: '[data-state]' },
        { timeout: 5000 },
      ),
    ).toBeInTheDocument();
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

    // Scoped to a run table: the triage rows also name the spec, but as context
    // in a meta line rather than as a destination.
    const recent = screen.getByText('recently finished').closest('.panel')!;
    const link = await within(recent as HTMLElement).findByText('daily-etl');
    expect(link).toHaveAttribute('href', '/runs?spec=daily-etl');
  });

  it('links the worker and queue panels at the fleet and the run list', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    expect(await screen.findByText('fleet detail')).toHaveAttribute('href', '/workers');
    const queue = screen.getByText('queue').closest('.panel')!;
    expect(await within(queue as HTMLElement).findByText(/active$/)).toHaveAttribute('href', '/runs');
  });

  it('links a triage row at the exact attempt the server pointed to', async () => {
    mockFetch(healthy());
    wrap(<OverviewPage namespace="default" />);

    const row = (
      await screen.findByText('Attempt blocked on capacity on ten-node-mixed / train_large')
    ).closest('li')!;
    expect(within(row as HTMLElement).getByText('open')).toHaveAttribute(
      'href',
      '/runs/run-att-0003?attempt=att-3333',
    );
  });
});
