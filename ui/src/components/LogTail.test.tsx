/**
 * Render tests for the streaming log tail.
 *
 * The reasoning in the component's header is that a log pane which has
 * silently stopped following looks exactly like a task that has stopped
 * logging, and those are very different situations — so the transport is
 * DISPLAYED rather than assumed, and it falls back to polling rather than
 * sitting in a dead stream. The other stated rule is that the buffer is capped,
 * because an unbounded log pane is how the console becomes the thing that falls
 * over during the incident it is meant to explain.
 *
 * EventSource does not exist in jsdom, so these tests drive the polling path,
 * which is the fallback the component is built around.
 */

import { render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LogTail } from './LogTail';

function mockFetch(handler: (url: string) => [number, unknown]) {
  vi.stubGlobal('fetch', (url: string) => {
    const [status, body] = handler(String(url));
    return Promise.resolve(
      new Response(typeof body === 'string' ? body : JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
  // Absent EventSource is the documented fallback trigger.
  vi.stubGlobal('EventSource', undefined);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the transport is stated, never assumed', () => {
  /**
   * A terminal attempt's log is complete, so following it forever would poll a
   * log that can never change — and would tell the operator it is "live" when
   * nothing more can arrive.
   */
  it('reports a finished attempt as complete rather than live', async () => {
    mockFetch(() => [200, { lines: [{ message: 'done' }] }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    expect(await screen.findByText('complete')).toBeInTheDocument();
    expect(screen.queryByText('streaming')).not.toBeInTheDocument();
  });

  it('reports that it fell back to polling when no stream is available', async () => {
    mockFetch(() => [200, { lines: [{ message: 'hello' }] }]);
    render(<LogTail namespace="default" attemptId="att-1" live />);

    // Saying "polling" rather than "streaming" is the honest answer, and is what
    // stops a silent pane from reading as a silent task.
    expect(await screen.findByText('polling')).toBeInTheDocument();
  });

  it('stops following a terminal attempt after one read', async () => {
    let calls = 0;
    mockFetch(() => {
      calls += 1;
      return [200, { lines: [{ message: 'done' }] }];
    });
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    await screen.findByText('done');
    const after = calls;
    // No interval is installed for a complete log: nothing can change.
    await new Promise((r) => setTimeout(r, 200));
    expect(calls).toBe(after);
  });
});

describe('log content', () => {
  it('renders the fetched lines', async () => {
    mockFetch(() => [200, { lines: [{ message: 'first' }, { message: 'second' }] }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    expect(await screen.findByText('first')).toBeInTheDocument();
    expect(screen.getByText('second')).toBeInTheDocument();
  });

  it('exposes the log as a live region so a screen reader follows it', async () => {
    mockFetch(() => [200, { lines: [{ message: 'first' }] }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    await screen.findByText('first');
    expect(screen.getByRole('log')).toBeInTheDocument();
  });

  /**
   * "The task produced no output" and "the worker has not started writing yet"
   * look identical from an empty pane, so the empty state distinguishes them.
   */
  it('explains an empty log differently for a live and a finished attempt', async () => {
    // An empty BODY, which is what the gateway returns for a log with no lines.
    // NB: a JSON `{"lines":[]}` body does NOT reach this state — see the
    // parseLogPayload note in the suite below.
    mockFetch(() => [200, '']);
    const { rerender } = render(<LogTail namespace="default" attemptId="att-1" live />);
    // The message is split across text nodes, so assert on the visible text.
    await waitFor(() => {
      expect(document.body.textContent).toMatch(/has not opened a log stream yet/i);
    });

    rerender(<LogTail namespace="default" attemptId="att-1" live={false} />);
    await waitFor(() => {
      expect(document.body.textContent).toMatch(/produced no output/i);
    });
  });
});

describe('KNOWN BUG: an empty JSON log body renders as a log line', () => {
  /**
   * CHARACTERIZATION ONLY — this pins what the code DOES, not what it should do.
   *
   * parseLogPayload (api/client.ts) only accepts the parsed JSON when the
   * extracted array is non-empty: `if (arr.length > 0)`. A valid but empty
   * `{"lines":[]}` therefore falls through to the raw-text path and the
   * operator is shown the literal JSON as if the task had logged it. The
   * gateway returning an empty body avoids it, which is why nothing has
   * noticed.
   *
   * Reported, deliberately not fixed here. When it IS fixed this test should
   * fail and be replaced by the empty-state assertion above.
   */
  it('shows the raw JSON instead of the empty state (current behaviour)', async () => {
    mockFetch(() => [200, { lines: [] }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    await waitFor(() => {
      expect(screen.getByText('{"lines":[]}')).toBeInTheDocument();
    });
    expect(document.body.textContent).not.toMatch(/produced no output/i);
  });
});

describe('a missing log endpoint is not an emergency', () => {
  /**
   * A 404 on a not-yet-started attempt is expected during Phase 1 bring-up.
   * Shouting about it would train operators to ignore the log pane's errors.
   */
  it('downgrades a 404 to an empty log rather than an error box', async () => {
    mockFetch(() => [404, { message: 'no such attempt' }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    await waitFor(() => {
      expect(screen.getByText(/produced no output/i)).toBeInTheDocument();
    });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('does show a real server failure', async () => {
    mockFetch(() => [500, { message: 'log store is down' }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not load logs/i);
  });
});
