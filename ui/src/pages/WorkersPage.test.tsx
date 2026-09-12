// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the worker fleet view.
 *
 * This screen exists to answer one question — "why is nothing being
 * dispatched?" — and the answer is either that no worker advertises the shape
 * of resource the task needs, or that the capacity is already committed. So
 * what is pinned here is that capacity is reported PER DIMENSION rather than as
 * one aggregate percentage, that a missing /capacity endpoint degrades to a
 * derived summary instead of hiding the fleet, and that a worker the control
 * plane still calls ACTIVE while its heartbeat has gone stale is called out.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { WorkersPage, deriveCapacity } from './WorkersPage';
import type { Worker } from '../api/types';

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

const worker = (over: Partial<Worker> = {}): Worker => ({
  id: 'wk-11111111-2222',
  namespace: 'default',
  hostname: 'agent-cpu-1',
  queues: ['default', 'batch'],
  capacity: { cpu_millis: 8000, memory_bytes: 16 * 1024 ** 3, gpu_count: 0 },
  committed: { cpu_millis: 2000, memory_bytes: 4 * 1024 ** 3, gpu_count: 0 },
  state: 'ACTIVE',
  agent_version: '0.1.0',
  last_heartbeat_at: new Date().toISOString(),
  ...over,
});

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the fleet table', () => {
  it('lists each worker with its state and the queues it serves', async () => {
    mockFetch((url) =>
      url.includes('/capacity') ? [200, { advertised: {}, committed: {} }] : [200, { workers: [worker()] }],
    );
    wrap(<WorkersPage namespace="default" />);

    expect(await screen.findByText('agent-cpu-1')).toBeInTheDocument();
    // Queues are the routing key: a task lands on a worker only if the queues
    // match, so this column is load-bearing for "why is nothing dispatched".
    expect(screen.getByText('default, batch')).toBeInTheDocument();
    expect(screen.getByText('ACTIVE', { selector: '[data-state]' })).toBeInTheDocument();
  });

  /**
   * A fleet at 40% CPU with every GPU committed is 100% blocked for GPU work.
   * One aggregate number hides exactly that, which is why committed/advertised
   * is shown per dimension.
   */
  it('reports committed against advertised for each resource dimension', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [
            200,
            {
              workers: [
                worker({
                  capacity: { cpu_millis: 8000, memory_bytes: 16 * 1024 ** 3, gpu_count: 4, gpu_kind: 'a100' },
                  committed: { cpu_millis: 2000, memory_bytes: 4 * 1024 ** 3, gpu_count: 4 },
                }),
              ],
            },
          ],
    );
    wrap(<WorkersPage namespace="default" />);

    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    // CPU has headroom while GPU is fully committed — the two must be legible
    // as separate facts, not averaged into one bar.
    expect(within(row).getByText('2 cpu / 8 cpu')).toBeInTheDocument();
    expect(within(row).getByText('4 / 4')).toBeInTheDocument();
  });

  /**
   * The interesting liveness case: the control plane still says ACTIVE but the
   * heartbeat stopped, which means the liveness sweep has not caught up. A
   * worker like this looks schedulable and is not.
   */
  it('flags a worker that is ACTIVE but has stopped heartbeating', async () => {
    const stale = new Date(Date.now() - 60_000).toISOString();
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ last_heartbeat_at: stale })] }],
    );
    wrap(<WorkersPage namespace="default" />);

    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    expect(within(row).getByText(/stale/i)).toBeInTheDocument();
  });

  it('does not flag a freshly heartbeating worker', async () => {
    mockFetch((url) =>
      url.includes('/capacity') ? [200, { advertised: {}, committed: {} }] : [200, { workers: [worker()] }],
    );
    wrap(<WorkersPage namespace="default" />);

    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    expect(within(row).queryByText(/stale/i)).not.toBeInTheDocument();
  });

  it('does not flag a DEAD worker as stale — its state already says so', async () => {
    const old = new Date(Date.now() - 600_000).toISOString();
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ state: 'DEAD', last_heartbeat_at: old })] }],
    );
    wrap(<WorkersPage namespace="default" />);

    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    expect(within(row).getByText('DEAD', { selector: '[data-state]' })).toBeInTheDocument();
    expect(within(row).queryByText(/stale/i)).not.toBeInTheDocument();
  });

  /**
   * Nothing can be dispatched at all with an empty fleet, so this empty state
   * carries the command that shows why no agent registered.
   */
  it('names the agent logs when no worker has registered', async () => {
    mockFetch((url) =>
      url.includes('/capacity') ? [200, { advertised: {}, committed: {} }] : [200, { workers: [] }],
    );
    wrap(<WorkersPage namespace="default" />);

    const empty = await screen.findByRole('status');
    expect(empty).toHaveTextContent(/docker compose logs/);
    expect(empty).toHaveTextContent(/Nothing can be dispatched/i);
  });
});

describe('a missing /capacity endpoint degrades rather than hides', () => {
  /**
   * Phase 1 ships services that are still being written. The aggregate endpoint
   * being absent must not cost the operator information already present in the
   * worker rows — so the page sums them itself and SAYS that it did.
   */
  it('derives the summary from the worker rows and labels it as derived', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [501, { message: 'not implemented' }]
        : [200, { workers: [worker()] }],
    );
    wrap(<WorkersPage namespace="default" />);

    await screen.findByText('agent-cpu-1');
    // The disclosure matters as much as the number: an operator must not read a
    // derived aggregate as the server's own answer.
    // The note is split across text nodes by an inline <code>, so assert on the
    // document's visible text rather than hunting a single element.
    // useCapacity sets its own retry: shouldRetry, which retries a 501 twice
    // before settling. Waiting it out asserts the REAL retry policy rather than
    // a test-only one, exactly as the admin suite does for a 5xx.
    await waitFor(
      () => {
        expect(document.body.textContent).toMatch(/Derived by summing the worker rows/i);
      },
      { timeout: 5000 },
    );
  });

  it('falls back to an error only when the worker rows are missing too', async () => {
    mockFetch(() => [500, { message: 'everything is down' }]);
    wrap(<WorkersPage namespace="default" />);

    await waitFor(
      () => {
        const alerts = screen.getAllByRole('alert');
        expect(alerts.some((a) => /capacity summary/i.test(a.textContent ?? ''))).toBe(true);
      },
      { timeout: 5000 },
    );
  });
});

describe('deriveCapacity', () => {
  /**
   * Counting a DEAD worker's CPUs would claim headroom that does not exist,
   * which is the opposite of what this screen is for.
   */
  it('excludes non-schedulable workers from advertised capacity', () => {
    const summary = deriveCapacity([
      worker({ id: 'a', state: 'ACTIVE', capacity: { cpu_millis: 4000 }, committed: { cpu_millis: 1000 } }),
      worker({ id: 'b', state: 'DRAINING', capacity: { cpu_millis: 2000 }, committed: { cpu_millis: 500 } }),
      worker({ id: 'c', state: 'DEAD', capacity: { cpu_millis: 8000 }, committed: { cpu_millis: 0 } }),
    ]);

    // ACTIVE + DRAINING only. A DRAINING worker still holds its current work,
    // so its capacity is real even though it accepts nothing new.
    expect(summary.advertised.cpu_millis).toBe(6000);
    expect(summary.committed.cpu_millis).toBe(1500);
    // The fleet counts still include the dead one: it exists, it just cannot run anything.
    expect(summary.worker_count).toBe(3);
    expect(summary.active_worker_count).toBe(1);
  });
});

/**
 * The redesign added the fleet's headline figures above the table.
 *
 * They exist for the second question an operator asks once dispatch has stalled
 * — "how much of the fleet is even there" — and the two figures that answer it
 * were previously buried in a ten-column table. What is pinned here is that the
 * two which can BE a problem are toned only when they are non-zero (a figure
 * that is always coloured is a figure nobody sees any more), and that the tone
 * comes from the state palette rather than the operator-switchable accent.
 */
describe('the fleet figures', () => {
  const figure = (label: string) => {
    const el = [...document.querySelectorAll('.fleet-figure')].find((f) =>
      new RegExp(label, 'i').test(f.querySelector('.fleet-figure-label')?.textContent ?? ''),
    )!;
    return el.querySelector('.fleet-figure-n')!;
  };

  it('counts registered, active, dead and gone-quiet workers', async () => {
    const stale = new Date(Date.now() - 60_000).toISOString();
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [
            200,
            {
              workers: [
                worker({ id: 'a', hostname: 'agent-cpu-1' }),
                worker({ id: 'b', hostname: 'agent-cpu-2', state: 'DEAD' }),
                worker({ id: 'c', hostname: 'agent-cpu-3', last_heartbeat_at: stale }),
              ],
            },
          ],
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-cpu-1');

    expect(figure('registered').textContent).toBe('3');
    // c is ACTIVE-but-quiet, so it still counts as active: the control plane says
    // it is, and the "gone quiet" figure beside it is what contradicts that.
    expect(figure('active').textContent).toBe('2');
    expect(figure('dead').textContent).toBe('1');
    expect(figure('gone quiet').textContent).toBe('1');
  });

  /**
   * A fleet with nothing wrong has not achieved anything worth colouring. If
   * zero were toned, the tone would stop meaning "look here".
   */
  it('does not tone a zero', async () => {
    mockFetch((url) =>
      url.includes('/capacity') ? [200, { advertised: {}, committed: {} }] : [200, { workers: [worker()] }],
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-cpu-1');

    expect(figure('dead').textContent).toBe('0');
    expect(figure('dead').className).not.toMatch(/alarm|warn/);
    expect(figure('gone quiet').className).not.toMatch(/alarm|warn/);
  });

  it('tones a non-zero dead count with state colour, never the accent', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ state: 'DEAD' })] }],
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByText('agent-cpu-1');

    // `alarm` resolves to --tone-danger-fg in index.css. The assertion that
    // matters is the negative one: nothing here may reach for the accent, which
    // an operator can change in settings.
    expect(figure('dead').className).toMatch(/alarm/);
    expect(figure('dead').className).not.toMatch(/accent/);
  });

  /**
   * Rendering the figures before the rows arrive would put "0 registered" on
   * screen as though it were an answer. It is not: it is the absence of one.
   */
  it('states no figures at all until the fleet has been fetched', async () => {
    mockFetch((url) =>
      url.includes('/capacity') ? [200, { advertised: {}, committed: {} }] : [200, { workers: [] }],
    );
    wrap(<WorkersPage namespace="default" />);
    await screen.findByRole('status');
    // The empty state speaks for an empty fleet; a row of zeroes would not.
    expect(document.querySelectorAll('.fleet-figure').length).toBe(0);
  });
});

describe('the worker row is marked beyond its badge', () => {
  it('rules a DEAD worker in the danger tone', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ state: 'DEAD' })] }],
    );
    wrap(<WorkersPage namespace="default" />);
    expect((await screen.findByText('agent-cpu-1')).closest('tr')).toHaveClass('row-failed');
  });

  /**
   * The disagreement between "ACTIVE" and a stopped heartbeat IS the finding, so
   * the row carries a warning rule even though its state badge looks healthy.
   */
  it('rules an ACTIVE-but-quiet worker in the warning tone', async () => {
    const stale = new Date(Date.now() - 60_000).toISOString();
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ last_heartbeat_at: stale })] }],
    );
    wrap(<WorkersPage namespace="default" />);
    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    expect(row).toHaveClass('row-warning');
    expect(row).not.toHaveClass('row-failed');
  });

  it('leaves a healthy worker unruled', async () => {
    mockFetch((url) =>
      url.includes('/capacity') ? [200, { advertised: {}, committed: {} }] : [200, { workers: [worker()] }],
    );
    wrap(<WorkersPage namespace="default" />);
    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    expect(row.className).toBe('');
  });
});

/**
 * The fleet table became a drill-down.
 *
 * The table answers "how much capacity is there"; it cannot answer the question
 * it raises, which is "that worker is busy — with WHAT?" So each row now links
 * to the worker's own page. What is pinned here is that the link is a real
 * anchor with an href: a click handler on the <tr> would be unreachable by
 * keyboard, impossible to middle-click into a new tab, and would show the
 * operator no destination on hover — all three of which are used when triaging
 * a fleet.
 */
describe('a row drills into the worker', () => {
  it('links the host cell to that worker’s detail view', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ id: 'wk-abc123', hostname: 'agent-cpu-1' })] }],
    );
    wrap(<WorkersPage namespace="default" />);

    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    const link = within(row).getByRole('link');
    // An href, not an onClick: keyboard and middle-click must both work.
    expect(link).toHaveAttribute('href', '/workers/wk-abc123');
    expect(link).toContainElement(screen.getByText('agent-cpu-1'));
  });

  it('encodes an id that needs it, rather than emitting a broken path', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [200, { workers: [worker({ id: 'wk/odd id', hostname: 'agent-cpu-1' })] }],
    );
    wrap(<WorkersPage namespace="default" />);

    const row = (await screen.findByText('agent-cpu-1')).closest('tr')!;
    expect(within(row).getByRole('link')).toHaveAttribute('href', '/workers/wk%2Fodd%20id');
  });

  /**
   * The count the drill-down expands. Three distinct claims kept distinct: a
   * number, `idle` for zero, and a dash when the gateway did not report the
   * field at all — rendering that last case as 0 would invent an answer.
   */
  it('shows the running count, and says idle rather than 0', async () => {
    mockFetch((url) =>
      url.includes('/capacity')
        ? [200, { advertised: {}, committed: {} }]
        : [
            200,
            {
              workers: [
                worker({ id: 'a', hostname: 'agent-busy', active_attempts: 3 }),
                worker({ id: 'b', hostname: 'agent-idle', active_attempts: 0 }),
                worker({ id: 'c', hostname: 'agent-silent' }),
              ],
            },
          ],
    );
    wrap(<WorkersPage namespace="default" />);

    const rowFor = async (host: string) => (await screen.findByText(host)).closest('tr')!;
    expect(within(await rowFor('agent-busy')).getByText('3')).toBeInTheDocument();
    // An idle worker beside a queue that is not moving IS the finding, so it is
    // a word rather than a digit lost among ten columns.
    expect(within(await rowFor('agent-idle')).getByText('idle')).toBeInTheDocument();
    // active_attempts absent: "not reported", which is not the same claim as 0.
    expect(within(await rowFor('agent-silent')).queryByText('idle')).not.toBeInTheDocument();
  });
});
