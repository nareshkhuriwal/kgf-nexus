// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the run list.
 *
 * What this screen owes an operator, and therefore what is pinned here: the
 * filters round-trip through the URL so a filtered view can be pasted into a
 * chat window; a failed refresh keeps the table on screen with a warning
 * rather than replacing it; and an empty list teaches the command that fills
 * it. Everything else on this page — column order, badge styling, the layout
 * of the header — is the redesign's to change.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { RunListPage } from './RunListPage';

/** Publishes the live query string so a test can assert what the URL holds. */
function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="search">{loc.search}</div>;
}

function wrap(ui: ReactNode, initialEntry = '/runs') {
  // retry:false so an error state renders immediately rather than after backoff.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <LocationProbe />
        <Routes>
          <Route path="/runs" element={ui} />
          <Route path="/runs/:runId" element={<div>run detail</div>} />
        </Routes>
      </MemoryRouter>
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

const run = (over: Record<string, unknown> = {}) => ({
  id: 'run-aaaaaaaaaaaa',
  namespace: 'default',
  spec_id: 'spec-1111',
  spec_name: 'daily-etl',
  spec_version: 3,
  state: 'FAILED',
  trigger_kind: 'schedule',
  created_at: '2026-09-12T10:00:00Z',
  started_at: '2026-09-12T10:00:05Z',
  ended_at: '2026-09-12T10:02:05Z',
  node_counts: { SUCCEEDED: 4, FAILED: 1, UPSTREAM_FAILED: 2 },
  ...over,
});

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('run list rendering', () => {
  it('lists runs with their state and links each to its detail view', async () => {
    mockFetch(() => [200, { runs: [run()] }]);
    wrap(<RunListPage namespace="default" />);

    expect(await screen.findByText('daily-etl')).toBeInTheDocument();
    // The link target is the operator's route into the run; a redesign may
    // restyle the cell but must not drop the navigation.
    expect(screen.getByRole('link', { name: /run-aaaa/ })).toHaveAttribute(
      'href',
      '/runs/run-aaaaaaaaaaaa',
    );
  });

  /**
   * CONTRACT.md invariant 2: the node tally must keep FAILED and
   * UPSTREAM_FAILED apart, because "this broke" and "this never ran because
   * something else broke" send an operator to different places.
   */
  it('tallies FAILED and UPSTREAM_FAILED as separate states', async () => {
    mockFetch(() => [200, { runs: [run()] }]);
    wrap(<RunListPage namespace="default" />);

    await screen.findByText('daily-etl');
    // Scoped to the row: the run's own state badge also reads FAILED, and the
    // claim under test is about the per-node tally.
    const row = screen.getByRole('link', { name: /run-aaaa/ }).closest('tr')!;
    const badges = within(row).getAllByText(/FAILED/, { selector: '[data-state]' });
    const failed = badges.find((b) => b.getAttribute('data-state') === 'FAILED')!;
    const upstream = badges.find((b) => b.getAttribute('data-state') === 'UPSTREAM FAILED')!;
    expect(failed).toBeInTheDocument();
    expect(upstream).toBeInTheDocument();
    // Distinct tones is the machine-readable form of "these do not look alike".
    expect(failed.getAttribute('data-tone')).not.toBe(upstream.getAttribute('data-tone'));
  });
});

describe('filters live in the URL', () => {
  /**
   * The whole point of URL-as-state: an operator pastes "the failed runs of
   * daily-etl" into chat and the recipient sees the same view. That only holds
   * if the filters both WRITE to and READ from the query string.
   */
  it('writes the state filter to the query string', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />);
    await screen.findByRole('combobox');

    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'FAILED' } });

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('state=FAILED');
    });
  });

  it('writes the spec filter to the query string', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />);
    await screen.findByRole('searchbox');

    fireEvent.change(screen.getByRole('searchbox'), { target: { value: 'daily-etl' } });

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('spec=daily-etl');
    });
  });

  it('reads both filters back out of the URL, so a pasted link reproduces the view', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />, '/runs?state=FAILED&spec=daily-etl');

    // The controls are populated from the URL, not from component state.
    expect(await screen.findByRole('combobox')).toHaveValue('FAILED');
    expect(screen.getByRole('searchbox')).toHaveValue('daily-etl');
  });

  it('sends the filters to the server rather than filtering in the browser', async () => {
    const seen: string[] = [];
    mockFetch((url) => {
      seen.push(url);
      return [200, { runs: [] }];
    });
    wrap(<RunListPage namespace="default" />, '/runs?state=FAILED&spec=daily-etl');

    await waitFor(() => expect(seen.length).toBeGreaterThan(0));
    expect(seen.some((u) => u.includes('state=FAILED') && u.includes('spec=daily-etl'))).toBe(true);
  });

  it('clears both filters, and the query string with them', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />, '/runs?state=FAILED&spec=daily-etl');

    fireEvent.click(await screen.findByRole('button', { name: /clear filters/i }));

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toBe('');
    });
    expect(screen.getByRole('combobox')).toHaveValue('');
    expect(screen.getByRole('searchbox')).toHaveValue('');
  });

  it('offers no clear control when nothing is filtered', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />);
    await screen.findByRole('combobox');
    expect(screen.queryByRole('button', { name: /clear filters/i })).not.toBeInTheDocument();
  });
});

describe('empty states', () => {
  /**
   * An empty list is a dead end unless it says how to fill one. The real
   * nexusctl command is the payload here — the wording around it is not.
   */
  it('names the command that creates a run when there are none at all', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />);

    const empty = await screen.findByRole('status');
    expect(empty).toHaveTextContent('nexusctl run create');
  });

  /**
   * With a filter applied the same emptiness means something different: the
   * runs may exist and be hidden. Telling the operator to create one would be
   * wrong advice.
   */
  it('distinguishes "no runs" from "no runs match the filter"', async () => {
    mockFetch(() => [200, { runs: [] }]);
    wrap(<RunListPage namespace="default" />, '/runs?state=FAILED');

    const empty = await screen.findByRole('status');
    expect(empty).toHaveTextContent(/filter/i);
    expect(empty).not.toHaveTextContent('nexusctl run create');
  });
});

describe('a failed refresh does not destroy what is on screen', () => {
  /**
   * The explicit rule this page implements: once runs are rendered, a refresh
   * that fails leaves them rendered and adds a warning. During an incident a
   * stale table beats an error box, provided the staleness is stated.
   */
  it('keeps the table and adds a stale warning when a refresh fails', async () => {
    let calls = 0;
    mockFetch(() => {
      calls += 1;
      return calls === 1 ? [200, { runs: [run()] }] : [500, { message: 'gateway exploded' }];
    });
    const { container } = wrap(<RunListPage namespace="default" />);

    await screen.findByText('daily-etl');

    // Force the refetch the 4s poll would eventually do.
    fireEvent.click(container); // no-op click keeps RTL act() bookkeeping tidy
    await waitFor(
      () => {
        expect(screen.getByRole('alert')).toHaveTextContent(/stale/i);
      },
      { timeout: 8000 },
    );

    // The data is STILL THERE. This is the assertion the redesign must not break.
    expect(screen.getByText('daily-etl')).toBeInTheDocument();
  }, 15000);

  it('shows an error instead of an empty table when the FIRST load fails', async () => {
    mockFetch(() => [500, { message: 'gateway exploded' }]);
    wrap(<RunListPage namespace="default" />);

    const alert = await screen.findByRole('alert', {}, { timeout: 5000 });
    expect(alert).toHaveTextContent(/Could not load runs/i);
    // Not the stale path: there is nothing stale to show.
    expect(alert).not.toHaveTextContent(/stale/i);
  });
});

describe('run rows', () => {
  it('renders a run whose optional fields are all absent without crashing', async () => {
    // A half-built gateway omits spec_name, node_counts and the timestamps.
    mockFetch(() => [
      200,
      {
        runs: [
          {
            id: 'run-bare',
            namespace: 'default',
            spec_id: 'spec-9999',
            state: 'PENDING',
            created_at: '2026-09-12T10:00:00Z',
          },
        ],
      },
    ]);
    wrap(<RunListPage namespace="default" />);

    const row = (await screen.findByRole('link', { name: /run-bare/ })).closest('tr')!;
    expect(within(row).getByText('PENDING', { selector: '[data-state]' })).toBeInTheDocument();
  });
});

/**
 * The redesign added a tone-coloured left rule to rows that need attention.
 *
 * It is REDUNDANT with the state badge already in the row — the point is that a
 * screenful can be triaged by edge colour without reading a label. So what is
 * pinned here is the rule the design rests on: the marker is derived from the
 * state's own tone, and it is never the accent, because the accent is
 * operator-switchable and would make a FAILED run restyleable into a calm one.
 */
describe('rows needing attention are marked beyond their badge', () => {
  const rowOf = async (name: RegExp) =>
    (await screen.findByRole('link', { name })).closest('tr')!;

  it('marks a FAILED run with the danger rule', async () => {
    mockFetch(() => [200, { runs: [run({ state: 'FAILED', node_counts: {} })] }]);
    wrap(<RunListPage namespace="default" />);
    expect(await rowOf(/run-aaaa/)).toHaveClass('row-failed');
  });

  it('marks a TIMED_OUT run too, because it shares the danger tone', async () => {
    // Derived from lib/states rather than a second list of names in the page, so
    // a state added there cannot end up with no rule.
    mockFetch(() => [200, { runs: [run({ state: 'TIMED_OUT', node_counts: {} })] }]);
    wrap(<RunListPage namespace="default" />);
    expect(await rowOf(/run-aaaa/)).toHaveClass('row-failed');
  });

  it('separates a stopped run from a failed one', async () => {
    // CANCELLED did not do what was asked, but it is not the thing to look at
    // first; collapsing the two would waste the one marker that means "start here".
    mockFetch(() => [200, { runs: [run({ state: 'CANCELLED', node_counts: {} })] }]);
    wrap(<RunListPage namespace="default" />);
    const row = await rowOf(/run-aaaa/);
    expect(row).toHaveClass('row-blocked');
    expect(row).not.toHaveClass('row-failed');
  });

  /**
   * The case the marker earns its keep on: the run's own badge says SUCCEEDED,
   * so the edge is the ONLY thing on the row saying a node inside it failed.
   */
  it('marks a succeeded run that contains a failed node', async () => {
    mockFetch(() => [
      200,
      { runs: [run({ state: 'SUCCEEDED', node_counts: { SUCCEEDED: 7, FAILED: 1 } })] },
    ]);
    wrap(<RunListPage namespace="default" />);
    expect(await rowOf(/run-aaaa/)).toHaveClass('row-failed');
  });

  it('leaves a healthy run unmarked, so the marker still means something', async () => {
    mockFetch(() => [200, { runs: [run({ state: 'SUCCEEDED', node_counts: { SUCCEEDED: 8 } })] }]);
    wrap(<RunListPage namespace="default" />);
    const row = await rowOf(/run-aaaa/);
    expect(row).not.toHaveClass('row-failed');
    expect(row).not.toHaveClass('row-blocked');
  });
});

describe('the result count', () => {
  /**
   * A count of zero and a count we do not have yet are different claims. While
   * the first load is in flight the page must not assert "0 runs".
   */
  it('does not claim zero before the first response arrives', async () => {
    mockFetch(() => [200, { runs: [run()] }]);
    const { container } = wrap(<RunListPage namespace="default" />);
    expect(container.querySelector('.filter-count-n')!.textContent).not.toBe('0');
    await screen.findByText('daily-etl');
    expect(container.querySelector('.filter-count-n')!.textContent).toBe('1');
  });

  /** With a filter on, the count is a count of MATCHES, and says so. */
  it('labels the count as matches when a filter is applied', async () => {
    mockFetch(() => [200, { runs: [run()] }]);
    const { container } = wrap(<RunListPage namespace="default" />, '/runs?state=FAILED');
    await screen.findByText('daily-etl');
    expect(container.querySelector('.filter-count')!.textContent).toMatch(/matching runs/);
  });
});
