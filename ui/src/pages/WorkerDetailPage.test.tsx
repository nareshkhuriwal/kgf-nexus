// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the worker drill-down.
 *
 * The fleet table answers "how much capacity is there"; this page exists to
 * answer the question it raises and cannot answer — "that worker is busy, busy
 * with WHAT?" So what is pinned here is the drill-down itself: that the tasks
 * on a worker are listed with the run each belongs to and a link through to it,
 * that an idle worker reads as idle rather than as a failure, that a DEAD
 * worker's task list is presented as history, and that the liveness threshold
 * this page applies is the SAME one the fleet table applies.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WithPermissions } from '../test/permissions';
import { render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WorkerDetailPage } from './WorkerDetailPage';
import type { WorkerAttempt, WorkerDetail } from '../api/types';

const WORKER_ID = 'wk-84239dec-1f45';

/**
 * Rendered under a real route so useParams resolves exactly as it will in the
 * app. A hand-passed prop would test a component the router never mounts.
 */
function wrap(path = `/workers/${WORKER_ID}`) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <WithPermissions><MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/workers/:workerId" element={<WorkerDetailPage namespace="default" />} />
        </Routes>
      </MemoryRouter></WithPermissions>
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

const attempt = (over: Partial<WorkerAttempt> = {}): WorkerAttempt => ({
  id: 'at-1',
  run_id: 'run-673321b5-3cf0',
  node_id: 'train',
  task_ref: 'ml.train:v2',
  attempt_number: 1,
  state: 'RUNNING',
  queue: 'gpu',
  scheduled_at: new Date(Date.now() - 90_000).toISOString(),
  started_at: new Date(Date.now() - 60_000).toISOString(),
  ended_at: null,
  ...over,
});

const detail = (over: Partial<WorkerDetail> = {}): WorkerDetail => ({
  id: WORKER_ID,
  namespace: 'default',
  hostname: 'agent-gpu-1',
  queues: ['gpu', 'default'],
  executors: ['docker'],
  labels: { zone: 'us-east-1a' },
  capacity: { cpu_millis: 8000, memory_bytes: 16 * 1024 ** 3, gpu_count: 4, gpu_kind: 'a100' },
  committed: { cpu_millis: 4000, memory_bytes: 8 * 1024 ** 3, gpu_count: 4 },
  state: 'ACTIVE',
  agent_version: '0.1.0',
  registered_at: new Date(Date.now() - 3_600_000).toISOString(),
  last_heartbeat_at: new Date().toISOString(),
  active_attempts: 1,
  running_attempts: [attempt()],
  recent_attempts: [],
  ...over,
});

/**
 * The empty/notice state of one named panel.
 *
 * Scoped by panel rather than by a bare getByRole('status'), because the page
 * legitimately renders more than one: "running now" and "recently finished" can
 * both be empty at once, which is exactly what a freshly registered worker
 * looks like. A global role query would be ambiguous there, and resolving the
 * ambiguity by taking the first match would silently assert against whichever
 * panel happened to render first.
 */
async function panelStatus(heading: string) {
  const head = await screen.findByText(heading, { selector: '.panel-head span' });
  // closest() is typed as Element; within() needs HTMLElement. The cast is safe
  // because .panel is always a div, and a missing ancestor would fail on the
  // non-null assertion rather than silently querying the wrong subtree.
  const panel = head.closest('.panel') as HTMLElement;
  return within(panel).getByRole('status');
}

/** Only the worker route answers; anything else 404s so a stray call is loud. */
function serve(body: unknown, status = 200) {
  mockFetch((url) => (url.includes('/workers/') ? [status, body] : [404, { message: 'unexpected' }]));
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the tasks running on a worker', () => {
  /**
   * The whole point of the page. "Worker X is busy" is only actionable once it
   * becomes "with run Y", so the task, the run and the link to it are what is
   * pinned.
   */
  it('lists each running task with its run, and links through to the run', async () => {
    serve(detail());
    wrap();

    const task = await screen.findByText('ml.train:v2');
    const row = task.closest('tr')!;

    // The task ref names the work; node_id alone names a position in a DAG the
    // reader is not looking at.
    expect(within(row).getByText('train')).toBeInTheDocument();
    expect(within(row).getByText('RUNNING', { selector: '[data-state]' })).toBeInTheDocument();
    expect(within(row).getByText('gpu')).toBeInTheDocument();

    // The link through to the run is the operator's next click.
    const link = within(row).getByRole('link');
    expect(link).toHaveAttribute('href', '/runs/run-673321b5-3cf0');
  });

  it('identifies the worker it is reporting on', async () => {
    serve(detail());
    wrap();

    expect(await screen.findByRole('heading', { name: 'agent-gpu-1' })).toBeInTheDocument();
    expect(screen.getAllByText('ACTIVE', { selector: '[data-state]' }).length).toBeGreaterThan(0);
  });

  /**
   * An attempt handed to a worker that never started it is a stall with a
   * specific cause, and one of the main things this page is opened to find. A
   * dash there would read as missing data rather than as a finding.
   */
  it('calls out a DISPATCHED attempt the worker has not started', async () => {
    serve(
      detail({
        running_attempts: [attempt({ state: 'DISPATCHED', started_at: null })],
      }),
    );
    wrap();

    const row = (await screen.findByText('ml.train:v2')).closest('tr')!;
    expect(within(row).getByText(/not started/i)).toBeInTheDocument();
  });

  it('shows how long a running task has been going', async () => {
    serve(detail());
    wrap();

    const row = (await screen.findByText('ml.train:v2')).closest('tr')!;
    // Started 60s ago, so the elapsed column reads in minutes-and-seconds.
    expect(within(row).getByText(/^\d+m \d+s$/)).toBeInTheDocument();
  });

  /** Reaching the fleet again must not require the back button. */
  it('offers a way back to the fleet', async () => {
    serve(detail());
    wrap();

    await screen.findByText('ml.train:v2');
    expect(screen.getByRole('link', { name: /back to workers/i })).toHaveAttribute('href', '/workers');
  });
});

describe('an idle worker reads as idle, not as broken', () => {
  /**
   * "This worker is running nothing right now" is a real and common answer, and
   * frequently the finding: an idle worker beside a queue that is not moving
   * means the mismatch is in queues or resource shape rather than fleet size.
   * It must never be dressed as a failure.
   */
  it('states that the worker is idle and does not raise an error', async () => {
    serve(detail({ active_attempts: 0, running_attempts: [] }));
    wrap();

    const empty = await panelStatus('running now');
    expect(empty).toHaveTextContent(/idle/i);
    // The distinction that matters: role=status, not role=alert, and no error box.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/Could not load/i);
  });

  /** The empty state earns its space by naming where to look instead. */
  it('points at the queue and resource-shape mismatch rather than at the fleet size', async () => {
    serve(detail({ active_attempts: 0, running_attempts: [] }));
    wrap();

    const empty = await panelStatus('running now');
    expect(empty).toHaveTextContent(/queues it serves or the resource shape/i);
  });

  /**
   * A DEAD worker's running set is necessarily empty, so the same zero rows
   * mean something different — and "what was it doing when it went" is the
   * question actually being asked.
   */
  it("presents a DEAD worker's task list as history rather than as idleness", async () => {
    serve(
      detail({
        state: 'DEAD',
        active_attempts: 0,
        running_attempts: [],
        last_heartbeat_at: new Date(Date.now() - 600_000).toISOString(),
        recent_attempts: [
          attempt({ id: 'at-old', state: 'FAILED', ended_at: new Date().toISOString() }),
        ],
      }),
    );
    wrap();

    const empty = await panelStatus('running now');
    expect(empty).toHaveTextContent(/DEAD/);
    expect(empty).toHaveTextContent(/what it did before it went/i);
    // And the history is actually on screen, which is the only thing a dead
    // worker can tell you about itself.
    expect(screen.getByText('FAILED', { selector: '[data-state]' })).toBeInTheDocument();
  });
});

describe('liveness on the detail view agrees with the fleet table', () => {
  /**
   * The threshold and the predicate are imported from WorkersPage rather than
   * re-declared, so this asserts the shared cut rather than a second copy of
   * it. Two screens each holding their own number is how one comes to call a
   * worker quiet while the other calls it healthy.
   */
  it('flags a worker that is ACTIVE but has stopped heartbeating', async () => {
    serve(detail({ last_heartbeat_at: new Date(Date.now() - 60_000).toISOString() }));
    wrap();

    await screen.findByText('ml.train:v2');
    const flag = screen.getByText(/stopped heartbeating/i);
    expect(flag).toBeInTheDocument();
    // The finding is the DISAGREEMENT, so it says the scheduler still considers
    // the worker schedulable — that is what makes it actionable.
    expect(flag.closest('.notice')).toHaveTextContent(/still treats it as\s+schedulable/i);
  });

  it('does not flag a freshly heartbeating worker', async () => {
    serve(detail());
    wrap();

    await screen.findByText('ml.train:v2');
    expect(screen.queryByText(/stopped heartbeating/i)).not.toBeInTheDocument();
  });

  /** A DEAD worker's state already says so; flagging it twice dilutes the flag. */
  it('does not flag a DEAD worker as stale', async () => {
    serve(
      detail({
        state: 'DEAD',
        running_attempts: [],
        last_heartbeat_at: new Date(Date.now() - 600_000).toISOString(),
      }),
    );
    wrap();

    await panelStatus('running now');
    expect(screen.queryByText(/stopped heartbeating/i)).not.toBeInTheDocument();
  });
});

describe('capacity and the drain control', () => {
  /**
   * A worker at 40% CPU with every GPU committed is 100% blocked for GPU work.
   * The grid is reused from the fleet view precisely so the two screens cannot
   * answer this differently.
   */
  it('reports committed against advertised per dimension', async () => {
    serve(detail());
    wrap();

    await screen.findByText('ml.train:v2');
    expect(screen.getByText('4 cpu / 8 cpu')).toBeInTheDocument();
    // Fully committed GPU, which is the blockage a single average would hide.
    expect(screen.getByText('4 / 4')).toBeInTheDocument();
  });

  /**
   * Draining removes capacity from the fleet, so it is confirmed rather than
   * fired on the first click — and the prompt names the consequence that is
   * easy to get wrong, that running work is NOT killed.
   */
  it('confirms before draining and says what a drain does not do', async () => {
    serve(detail());
    wrap();

    await screen.findByText('ml.train:v2');
    screen.getByRole('button', { name: /^drain$/i }).click();

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /confirm drain/i })).toBeInTheDocument();
    });
    expect(document.body.textContent).toMatch(/finishes the 1 it holds/i);
    expect(screen.getByRole('button', { name: /keep it/i })).toBeInTheDocument();
  });

  /** An action with no effect teaches operators to distrust the ones that work. */
  it('offers no drain for a worker that cannot take work', async () => {
    serve(detail({ state: 'DEAD', running_attempts: [] }));
    wrap();

    await panelStatus('running now');
    expect(screen.queryByRole('button', { name: /drain/i })).not.toBeInTheDocument();
    expect(screen.getByText(/nothing to drain/i)).toBeInTheDocument();
  });
});

describe('failure and compatibility', () => {
  it('reports an unreachable worker as an error with a way back', async () => {
    serve({ error: 'not_found', message: 'not found' }, 404);
    wrap();

    const err = await screen.findByRole('alert');
    expect(err).toHaveTextContent(/Could not load worker/i);
    expect(screen.getByRole('link', { name: /back to workers/i })).toBeInTheDocument();
  });

  /**
   * A gateway predating this route serves the worker without either task list.
   * "Not reported" and "nothing is running" are different claims and an older
   * gateway must not make the second one on this one's behalf.
   */
  it('distinguishes a gateway that does not report tasks from an idle worker', async () => {
    const { running_attempts: _r, recent_attempts: _c, ...withoutLists } = detail();
    serve(withoutLists);
    wrap();

    const notice = await panelStatus('running now');
    expect(notice).toHaveTextContent(/does not report per-worker tasks/i);
    expect(notice).not.toHaveTextContent(/idle/i);
    // The worker's own capacity is still its own, so it is still shown.
    expect(screen.getByText('4 cpu / 8 cpu')).toBeInTheDocument();
  });
});
