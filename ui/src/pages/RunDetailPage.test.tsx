import { PermissionContext } from '../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the run detail page.
 *
 * During an incident the operator moves between four questions in seconds —
 * what broke, why, what did it say, who did what when — so the selected run,
 * node and attempt live in the URL rather than in component state. That is
 * what makes the view shareable and what stops a refresh from dropping the
 * operator back at the top of the graph. Pinned here: the selection
 * round-trips through the query string, a failed run opens on its root cause,
 * and cancel is confirmed before it kills in-flight work.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RunDetailPage } from './RunDetailPage';

/** Publishes the live query string so a test can assert what the URL holds. */
function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="search">{loc.search}</div>;
}

function wrap(entry: string) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <PermissionContext.Provider value={["runs.view","runs.create","runs.cancel","runs.retry","workflows.view","workflows.publish","workflows.delete","schedules.create","tasks.logs.view","tasks.results.view"]}><QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[entry]}>
        <LocationProbe />
        <Routes>
          <Route path="/runs/:runId" element={<RunDetailPage namespace="default" />} />
          <Route path="/runs" element={<div>run list</div>} />
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

/**
 * A failed run: extract broke, transform and load never ran. The run detail
 * endpoint serves nodes, edges AND attempts from one handler.
 */
const failedRun = {
  id: 'run-aaaaaaaaaaaa',
  namespace: 'default',
  spec_id: 'spec-1',
  spec_name: 'daily-etl',
  spec_version: 2,
  state: 'FAILED',
  created_at: '2026-09-12T10:00:00Z',
  started_at: '2026-09-12T10:00:01Z',
  ended_at: '2026-09-12T10:05:00Z',
  nodes: [
    { run_id: 'run-aaaaaaaaaaaa', node_id: 'extract', task_ref: 'tasks/extract', state: 'FAILED' },
    { run_id: 'run-aaaaaaaaaaaa', node_id: 'transform', task_ref: 'tasks/transform', state: 'UPSTREAM_FAILED' },
    { run_id: 'run-aaaaaaaaaaaa', node_id: 'load', task_ref: 'tasks/load', state: 'UPSTREAM_FAILED' },
  ],
  edges: [
    { from_node_id: 'extract', to_node_id: 'transform' },
    { from_node_id: 'transform', to_node_id: 'load' },
  ],
  attempts: [
    {
      id: 'a1-extract',
      run_id: 'run-aaaaaaaaaaaa',
      node_id: 'extract',
      attempt_number: 1,
      state: 'FAILED',
      error: { class: 'APPLICATION', message: 'source table missing' },
    },
    {
      id: 'a2-extract',
      run_id: 'run-aaaaaaaaaaaa',
      node_id: 'extract',
      attempt_number: 2,
      state: 'FAILED',
      error: { class: 'APPLICATION', message: 'source table missing' },
    },
  ],
};

/** Serves the run detail, and an empty event list, for any URL. */
function serveRun(run: unknown = failedRun) {
  mockFetch((url) => {
    if (url.includes('/events')) return [200, { events: [] }];
    if (url.includes('/logs')) return [200, { lines: [] }];
    return [200, run];
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
  // EventSource is not implemented in jsdom; the live hook falls back to
  // polling when it is absent, which is the path under test here.
  vi.stubGlobal('EventSource', undefined);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the run header', () => {
  it('identifies the run, its spec and its state', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');

    expect(await screen.findByText(/daily-etl/)).toBeInTheDocument();
    expect(screen.getAllByText('FAILED', { selector: '[data-state]' }).length).toBeGreaterThan(0);
  });

  it('offers a route back to the list', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');
    expect(await screen.findByRole('link', { name: /back to runs/i })).toHaveAttribute('href', '/runs');
  });

  it('surfaces an unreachable backend instead of a blank page', async () => {
    mockFetch(() => [500, { message: 'gateway down' }]);
    wrap('/runs/run-aaaaaaaaaaaa');

    expect(await screen.findByRole('alert', {}, { timeout: 5000 })).toHaveTextContent(
      /Could not load run/i,
    );
  });
});

describe('deep links: the selection lives in the URL', () => {
  /**
   * The first thing an operator opening a failed run wants is the node that
   * actually broke, so the page defaults the selection to the root cause —
   * and writes it to the URL, so what they then share points at it.
   */
  it('defaults the selection to the root cause and records it in the URL', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('node=extract');
    });
  });

  it('honours a node named in the URL rather than overriding it', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=load');

    // A pasted link must land where it says, even though `extract` is the root
    // cause the page would otherwise have picked.
    await waitFor(() => {
      expect(screen.getByText(/node · load/)).toBeInTheDocument();
    });
    expect(screen.getByTestId('search').textContent).toContain('node=load');
  });

  it('writes the selected node to the URL when one is picked in the graph', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');

    await screen.findByRole('button', { name: /^transform,/ });
    fireEvent.click(screen.getByRole('button', { name: /^transform,/ }));

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('node=transform');
    });
  });

  /**
   * Picking a new node must drop the old node's attempt from the URL: an
   * attempt id belongs to one node, and carrying it across would deep-link to
   * an attempt that is not in the panel being shown.
   */
  it('clears the selected attempt when the node changes', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=extract&attempt=a1-extract');

    await screen.findByRole('button', { name: /^transform,/ });
    fireEvent.click(screen.getByRole('button', { name: /^transform,/ }));

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('node=transform');
    });
    expect(screen.getByTestId('search').textContent).not.toContain('attempt=');
  });

  it('writes the selected attempt to the URL', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=extract');

    // The older attempt: picking it must be what lands in the URL.
    const card = (await screen.findByText('attempt #1')).closest('section')!;
    fireEvent.click(within(card).getByRole('button', { name: /logs/i }));

    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('attempt=a1-extract');
    });
  });

  it('honours an attempt named in the URL over the latest one', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=extract&attempt=a1-extract');

    // attempt #2 is the latest and would be the fallback, so naming #1 in the
    // URL must visibly win — otherwise the deep link is decorative.
    expect(await screen.findByText(/logs · attempt a1-extra/)).toBeInTheDocument();
    expect(screen.queryByText(/logs · attempt a2-extra/)).not.toBeInTheDocument();
  });
});

describe('the node panel follows the selection', () => {
  it('shows the selected node with its full attempt history', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=extract');

    expect(await screen.findByText('attempt #2')).toBeInTheDocument();
    // Both tries are present: collapsing them would hide that it was retried.
    expect(screen.getByText('attempt #1')).toBeInTheDocument();
  });

  it('explains what to do when nothing is selected', async () => {
    // A healthy run has no root cause, so nothing is auto-selected.
    serveRun({
      ...failedRun,
      state: 'RUNNING',
      nodes: [
        { run_id: 'run-aaaaaaaaaaaa', node_id: 'extract', task_ref: 'tasks/extract', state: 'RUNNING' },
      ],
      edges: [],
      attempts: [],
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    expect(await screen.findByText(/Pick a node in the graph/i)).toBeInTheDocument();
  });

  it('lets an operator walk upstream from the selected node', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=transform');

    // Walking up the dependency chain is how you get from collateral damage to
    // the real failure.
    fireEvent.click(await screen.findByRole('button', { name: 'extract' }));
    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('node=extract');
    });
  });
});

describe('cancel', () => {
  /**
   * Cancel is the only Phase 1 mutation and it kills in-flight work, so an
   * accidental click costs real compute. It is confirmed, and the confirmation
   * says what will happen.
   */
  it('does not cancel on the first click', async () => {
    const posts: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') posts.push(url);
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, { ...failedRun, state: 'RUNNING' }];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: /cancel run/i }));

    // The first click asks; it must not have killed anything yet.
    expect(await screen.findByText(/kills in-flight attempts/i)).toBeInTheDocument();
    expect(posts).toEqual([]);
  });

  it('cancels once confirmed', async () => {
    const posts: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posts.push(url);
        return [204, null];
      }
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, { ...failedRun, state: 'RUNNING' }];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: /cancel run/i }));
    fireEvent.click(await screen.findByRole('button', { name: /yes, cancel/i }));

    await waitFor(() => {
      expect(posts.some((u) => u.endsWith('/cancel'))).toBe(true);
    });
  });

  it('backs out without cancelling', async () => {
    const posts: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') posts.push(url);
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, { ...failedRun, state: 'RUNNING' }];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: /cancel run/i }));
    fireEvent.click(await screen.findByRole('button', { name: /keep running/i }));

    expect(screen.queryByText(/kills in-flight attempts/i)).not.toBeInTheDocument();
    expect(posts).toEqual([]);
  });

  /**
   * A terminal run cannot be cancelled, and the control says why rather than
   * silently doing nothing.
   */
  it('offers re-run and retry instead of cancel after failure', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');

    expect(await screen.findByRole('button', {name:'Re-run'})).toBeEnabled();
    expect(screen.getByRole('button',{name:'Retry failed tasks'})).toBeEnabled();
    expect(screen.queryByRole('button', {name:/cancel run/i})).toBeNull();
  });
});

describe('partial backends degrade rather than blank', () => {
  /**
   * The node states come from the run endpoint and the attempts come from the
   * same handler; if the attempts are missing the graph must still render,
   * because node state alone already answers "what broke".
   */
  it('still renders the graph when the run carries no attempts', async () => {
    serveRun({ ...failedRun, attempts: [] });
    wrap('/runs/run-aaaaaaaaaaaa?node=extract');

    expect(await screen.findByRole('button', { name: /^extract, FAILED/ })).toBeInTheDocument();
    // Several empty panels are on screen (events is empty too), so this asks
    // for the attempts one by what it says rather than by being the only status.
    const empties = screen.getAllByRole('status');
    expect(empties.some((e) => /No attempts yet/i.test(e.textContent ?? ''))).toBe(true);
  });
});

/**
 * The way back is part of every state, not just the loaded one.
 *
 * The redesign fixed a real gap here: the loading branch rendered a bare
 * skeleton with no navigation, so a run that was slow to load — exactly when an
 * operator wants to give up and go back to the list — left them with no way out
 * but the browser's back button.
 */
describe('the way out survives every state', () => {
  it('offers the route back while the run is still loading', async () => {
    // A fetch that never settles holds the page in its loading branch.
    vi.stubGlobal('fetch', () => new Promise(() => {}));
    wrap('/runs/run-aaaaaaaaaaaa');

    expect(await screen.findByRole('link', { name: /back to runs/i })).toHaveAttribute('href', '/runs');
    // Still genuinely loading, not silently empty.
    expect(document.querySelector('[aria-busy="true"]')).toBeInTheDocument();
  });

  it('offers the route back when the run could not be loaded at all', async () => {
    mockFetch(() => [500, { message: 'gateway down' }]);
    wrap('/runs/run-aaaaaaaaaaaa');

    await screen.findByRole('alert', {}, { timeout: 5000 });
    expect(screen.getByRole('link', { name: /back to runs/i })).toHaveAttribute('href', '/runs');
    // The id is named even though the run behind it never arrived, so the
    // operator can tell WHICH run failed to load.
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('run-aaaa');
  });
});

describe('the run identifiers', () => {
  /**
   * These are needed verbatim when they are needed at all — pasted into a
   * nexusctl invocation — so the full value is present, not a shortened one.
   */
  it('states the full run and spec ids, not the shortened forms', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');

    await screen.findByText(/daily-etl/);
    const ids = document.querySelector('.run-ids')!;
    expect(within(ids as HTMLElement).getByText('run-aaaaaaaaaaaa')).toBeInTheDocument();
    expect(within(ids as HTMLElement).getByText('spec-1')).toBeInTheDocument();
  });

  it('says a run has no params rather than omitting the row', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');

    await screen.findByText(/daily-etl/);
    const ids = document.querySelector('.run-ids')!;
    // "params: none" is an answer; a missing row would leave the operator
    // wondering whether the console simply failed to show them.
    expect(ids.textContent).toMatch(/params/);
    expect(ids.textContent).toMatch(/none/);
  });
});

it('successful runs offer a fresh execution and never cancel or retry failed tasks', async () => {
 mockFetch((url) => url.includes('/events') ? [200,{events:[]}] : [200,{...failedRun,state:'SUCCEEDED'}]);
 wrap('/runs/run-aaaaaaaaaaaa');
 expect(await screen.findByRole('button',{name:'Re-run'})).toBeEnabled();
 expect(screen.queryByRole('button',{name:/cancel run/i})).toBeNull();
 expect(screen.queryByRole('button',{name:'Retry failed tasks'})).toBeNull();
});

describe('re-run and retry confirm in the console, not the browser', () => {
  /**
   * Both start real compute, so neither fires on the first click. The
   * confirmation is the console's own dialog: window.confirm cannot be
   * styled, says "OK" for an action that spends money, and is silently
   * suppressed by a browser setting.
   */
  it('asks before starting a new run, then POSTs once confirmed', async () => {
    const posts: string[] = [];
    const confirmSpy = vi.spyOn(window, 'confirm');
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posts.push(url);
        return [201, { id: 'run-bbbbbbbbbbbb', state: 'PENDING' }];
      }
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, failedRun];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: 'Re-run' }));
    const dialog = await screen.findByRole('alertdialog', { name: /start a new run/i });
    expect(dialog).toHaveTextContent(/original parameters/i);
    expect(posts).toEqual([]);
    expect(confirmSpy).not.toHaveBeenCalled();

    fireEvent.click(within(dialog).getByRole('button', { name: 'Start new run' }));
    await waitFor(() => {
      expect(posts.some((u) => u.endsWith('/runs'))).toBe(true);
    });
    confirmSpy.mockRestore();
  });

  it('backs out of a re-run without starting anything', async () => {
    const posts: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') posts.push(url);
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, failedRun];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: 'Re-run' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(posts).toEqual([]);
  });

  it('asks before retrying failed tasks, and says succeeded tasks are untouched', async () => {
    const posts: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posts.push(url);
        return [204, null];
      }
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, failedRun];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: 'Retry failed tasks' }));
    const dialog = await screen.findByRole('alertdialog', { name: /retry the failed tasks/i });
    expect(dialog).toHaveTextContent(/succeeded keep\s+their results/i);
    expect(posts).toEqual([]);

    fireEvent.click(within(dialog).getByRole('button', { name: 'Retry failed tasks' }));
    await waitFor(() => {
      expect(posts.some((u) => u.endsWith('/retry'))).toBe(true);
    });
  });

  it('retries the whole run with an empty selection', async () => {
    const bodies: string[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        bodies.push(String(init.body));
        return [200, { ...failedRun, state: 'RUNNING' }];
      }
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, failedRun];
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    fireEvent.click(await screen.findByRole('button', { name: 'Retry failed tasks' }));
    const dialog = await screen.findByRole('alertdialog', { name: /retry the failed tasks/i });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Retry failed tasks' }));
    await waitFor(() => expect(bodies).toEqual(['{}']));
  });

  it('retries only the selected task, after saying what else re-runs', async () => {
    const posts: { url: string; body: string }[] = [];
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        posts.push({ url, body: String(init.body) });
        return [200, { ...failedRun, state: 'RUNNING' }];
      }
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, failedRun];
    });
    // extract is the root cause and therefore the default selection.
    wrap('/runs/run-aaaaaaaaaaaa?node=extract');

    fireEvent.click(await screen.findByRole('button', { name: 'Retry this task' }));
    const dialog = await screen.findByRole('alertdialog', { name: /retry extract/i });
    expect(dialog).toHaveTextContent(/downstream/i);
    expect(posts).toEqual([]);

    fireEvent.click(within(dialog).getByRole('button', { name: 'Retry this task' }));
    await waitFor(() => expect(posts).toHaveLength(1));
    const [sent] = posts;
    expect(sent?.url).toMatch(/\/runs\/run-aaaaaaaaaaaa\/retry$/);
    expect(JSON.parse(sent?.body ?? 'null')).toEqual({ nodes: ['extract'] });
  });

  it('offers no per-task retry for a task that succeeded', async () => {
    mockFetch((url) => {
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, {
        ...failedRun,
        nodes: [
          { ...failedRun.nodes[0], state: 'SUCCEEDED' },
          { ...failedRun.nodes[1], state: 'FAILED' },
          failedRun.nodes[2],
        ],
      }];
    });
    wrap('/runs/run-aaaaaaaaaaaa?node=extract');

    await screen.findByRole('button', { name: 'Retry failed tasks' });
    expect(screen.queryByRole('button', { name: 'Retry this task' })).toBeNull();
  });

  it('shows the gateway refusal when a task cannot be retried', async () => {
    mockFetch((url, init) => {
      if (init?.method === 'POST') {
        return [409, {
          error: 'retry_refused',
          message: 'node transform is UPSTREAM_FAILED and would fail again at once: it depends on extract',
        }];
      }
      if (url.includes('/events')) return [200, { events: [] }];
      return [200, failedRun];
    });
    wrap('/runs/run-aaaaaaaaaaaa?node=transform');

    fireEvent.click(await screen.findByRole('button', { name: 'Retry this task' }));
    const dialog = await screen.findByRole('alertdialog', { name: /retry transform/i });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Retry this task' }));
    expect(await screen.findByText(/would fail again at once/i)).toBeInTheDocument();
  });
});

describe('task placement', () => {
  /**
   * "Which worker is running which task" must be answerable from one table,
   * not by clicking every node. The gateway joins the hostname onto each
   * attempt; the panel names it, links it to the fleet page, and puts what is
   * running now above what has finished.
   */
  const placedRun = {
    ...failedRun,
    state: 'RUNNING',
    ended_at: null,
    nodes: [
      { run_id: 'run-aaaaaaaaaaaa', node_id: 'extract', task_ref: 'tasks/extract', state: 'SUCCEEDED' },
      { run_id: 'run-aaaaaaaaaaaa', node_id: 'transform', task_ref: 'tasks/transform', state: 'RUNNING' },
      { run_id: 'run-aaaaaaaaaaaa', node_id: 'load', task_ref: 'tasks/load', state: 'PENDING' },
    ],
    attempts: [
      {
        id: 'a1-extract', run_id: 'run-aaaaaaaaaaaa', node_id: 'extract', attempt_number: 1, state: 'SUCCEEDED',
        worker_id: 'worker-1111-2222-3333', worker_hostname: 'cpu-node-1', queue: 'default',
        started_at: '2026-09-12T10:00:01Z', ended_at: '2026-09-12T10:00:31Z',
      },
      {
        id: 'a1-transform', run_id: 'run-aaaaaaaaaaaa', node_id: 'transform', attempt_number: 1, state: 'RUNNING',
        worker_id: 'worker-4444-5555-6666', worker_hostname: 'gpu-node-3', queue: 'gpu',
        started_at: '2026-09-12T10:00:40Z',
      },
      {
        id: 'a1-load', run_id: 'run-aaaaaaaaaaaa', node_id: 'load', attempt_number: 1, state: 'PENDING',
        pending_reason: 'waiting_for_resources: gpu (need 1, free 0)',
      },
    ],
  };

  it('names the worker for every placed task and links it to the fleet page', async () => {
    serveRun(placedRun);
    wrap('/runs/run-aaaaaaaaaaaa');

    const panel = (await screen.findByText('task placement')).closest<HTMLElement>('.panel')!;
    const gpu = within(panel).getByRole('link', { name: 'gpu-node-3' });
    expect(gpu).toHaveAttribute('href', '/workers/worker-4444-5555-6666');
    expect(within(panel).getByRole('link', { name: 'cpu-node-1' })).toBeInTheDocument();
    // The unplaced node says why it has no worker rather than showing a dash.
    expect(within(panel).getByText(/waiting: waiting_for_resources/)).toBeInTheDocument();
    // One task running, on one worker: the head says so.
    expect(within(panel).getByText('1 running on 1 worker')).toBeInTheDocument();
  });

  it('puts what is running now above what has finished', async () => {
    serveRun(placedRun);
    wrap('/runs/run-aaaaaaaaaaaa');

    const panel = (await screen.findByText('task placement')).closest<HTMLElement>('.panel')!;
    const rows = within(panel).getAllByRole('row').slice(1);
    expect(rows[0]).toHaveTextContent('transform');
    expect(rows[1]).toHaveTextContent('load');
    expect(rows[2]).toHaveTextContent('extract');
  });

  it('selects the node in the graph when its task is clicked', async () => {
    serveRun(placedRun);
    wrap('/runs/run-aaaaaaaaaaaa');

    const panel = (await screen.findByText('task placement')).closest<HTMLElement>('.panel')!;
    fireEvent.click(within(panel).getByRole('button', { name: 'extract' }));
    await waitFor(() => {
      expect(screen.getByTestId('search').textContent).toContain('node=extract');
    });
  });

  it('falls back to the worker id when the gateway sends no hostname', async () => {
    serveRun({
      ...placedRun,
      attempts: placedRun.attempts.map((a) => ({ ...a, worker_hostname: undefined })),
    });
    wrap('/runs/run-aaaaaaaaaaaa');

    const panel = (await screen.findByText('task placement')).closest<HTMLElement>('.panel')!;
    expect(within(panel).getByRole('link', { name: /worker-4444/ })).toHaveAttribute(
      'href',
      '/workers/worker-4444-5555-6666',
    );
  });
});

/**
 * The execution timeline shares the page's one selection: the URL. A bar
 * clicked in the timeline selects that node AND that attempt (so the attempt
 * panel and log tail follow), a node picked in the graph highlights its latest
 * attempt in the timeline, and a map child the graph has folded away still
 * resolves to a node with its attempts.
 */
describe('execution timeline selection', () => {
  const timeline = () => screen.getByRole('list', { name: /execution timeline/i });

  const fanRun = {
    ...failedRun,
    state: 'SUCCEEDED',
    nodes: [
      { run_id: 'run-aaaaaaaaaaaa', node_id: 'list', task_ref: 't/list', state: 'SUCCEEDED' },
      { run_id: 'run-aaaaaaaaaaaa', node_id: 'shard', task_ref: 't/shard', state: 'SUCCEEDED' },
      ...Array.from({ length: 60 }, (_, i) => ({
        run_id: 'run-aaaaaaaaaaaa',
        node_id: `shard[${i}]`,
        task_ref: 't/shard',
        state: 'SUCCEEDED',
        parent_node_id: 'shard',
        map_index: i,
      })),
    ],
    edges: [{ from_node_id: 'list', to_node_id: 'shard' }],
    attempts: [
      {
        id: 'at-list', run_id: 'run-aaaaaaaaaaaa', node_id: 'list', attempt_number: 1, state: 'SUCCEEDED',
        scheduled_at: '2026-09-12T10:00:01Z', started_at: '2026-09-12T10:00:02Z', ended_at: '2026-09-12T10:00:03Z',
      },
      ...Array.from({ length: 60 }, (_, i) => ({
        id: `at-shard-${i}`,
        run_id: 'run-aaaaaaaaaaaa',
        node_id: `shard[${i}]`,
        attempt_number: 1,
        state: 'SUCCEEDED',
        scheduled_at: '2026-09-12T10:00:04Z',
        dispatched_at: '2026-09-12T10:00:05Z',
        started_at: '2026-09-12T10:00:06Z',
        ended_at: '2026-09-12T10:00:09Z',
      })),
    ],
  };

  it('writes node and attempt to the URL when a bar is clicked', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa');
    await screen.findByRole('list', { name: /execution timeline/i });
    fireEvent.click(within(timeline()).getByRole('button', { name: /^Attempt 1 of extract/ }));
    await waitFor(() => {
      const search = screen.getByTestId('search').textContent ?? '';
      expect(search).toContain('node=extract');
      expect(search).toContain('attempt=a1-extract');
    });
    expect(within(timeline()).getByRole('button', { name: /^Attempt 1 of extract/ })).toHaveAttribute(
      'aria-pressed',
      'true',
    );
  });

  it('highlights the latest attempt of the node picked in the graph', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=transform');
    await screen.findByRole('list', { name: /execution timeline/i });
    expect(within(timeline()).getByRole('button', { name: /^No attempt for transform/ })).toHaveAttribute(
      'aria-pressed',
      'true',
    );

    fireEvent.click(screen.getByRole('button', { name: /^extract,/ }));
    await waitFor(() => {
      expect(within(timeline()).getByRole('button', { name: /^Attempt 2 of extract/ })).toHaveAttribute(
        'aria-pressed',
        'true',
      );
    });
  });

  it('restores a deep-linked attempt and table view', async () => {
    serveRun();
    wrap('/runs/run-aaaaaaaaaaaa?node=extract&attempt=a1-extract&timeline=table');
    const table = await screen.findByRole('table');
    expect(
      within(table).getByRole('button', { name: 'Select extract attempt 1' }).closest('tr'),
    ).toHaveAttribute('aria-current', 'true');
    fireEvent.click(screen.getByRole('button', { name: 'Chart' }));
    await waitFor(() => expect(screen.getByTestId('search').textContent).not.toContain('timeline='));
  });

  it('resolves a map child the graph keeps folded', async () => {
    serveRun(fanRun);
    wrap('/runs/run-aaaaaaaaaaaa');
    await screen.findByRole('list', { name: /execution timeline/i });
    fireEvent.click(within(timeline()).getByRole('button', { name: /^Attempt 1 of shard\[3\]/ }));
    await waitFor(() => expect(screen.getByTestId('search').textContent).toContain('node=shard%5B3%5D'));
    // The attempt panel names the child rather than asking for a node.
    expect(await screen.findByText('node · shard[3]')).toBeInTheDocument();
  });
});
