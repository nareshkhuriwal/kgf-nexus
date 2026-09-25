// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the durable log pane (ADR-0020).
 *
 * What these pin: the log's STATE is the server's and is shown as-is (a pane
 * that guesses reads "empty" for a crashed agent); following is cursor-based,
 * so nothing repeats and nothing is skipped; the server search and the local
 * filter are distinct and labelled; and truncation, drops and unconfigured
 * storage are said out loud rather than implied by an empty pane.
 */

import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LogTail } from './LogTail';

type Handler = (url: URL) => [number, unknown];
let calls: URL[] = [];

function mockFetch(handler: Handler) {
  calls = [];
  vi.stubGlobal('fetch', (input: string, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost');
    calls.push(url);
    const auth = new Headers(init?.headers).get('Authorization');
    if (!auth) return Promise.resolve(new Response('{"message":"unauthenticated"}', { status: 401 }));
    const [status, body] = handler(url);
    return Promise.resolve(
      new Response(typeof body === 'string' ? body : JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
}

function status(state: string, extra: Record<string, unknown> = {}) {
  return {
    state,
    attempt_state: 'RUNNING',
    lines: 0,
    bytes: 0,
    chunks: 0,
    truncated_lines: 0,
    truncated_bytes: 0,
    dropped_lines: 0,
    compacted: false,
    detail: `detail for ${state}`,
    ...extra,
  };
}

function line(n: number, message = `line ${n}`, stream = 'stdout') {
  return { n, ts: '2026-09-25T00:00:00Z', stream, message };
}

function page(state: string, lines: ReturnType<typeof line>[], extra: Record<string, unknown> = {}) {
  const next = lines.length ? lines[lines.length - 1]!.n + 1 : 1;
  return { attempt_id: 'att-1', log: status(state, { lines: next - 1, ...extra }), lines, next_line: next, has_more: false };
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe('the log state is the server’s, stated not guessed', () => {
  it('shows a finished attempt’s complete log without polling', async () => {
    mockFetch(() => [200, page('complete', [line(1, 'done')])]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    expect(await screen.findByText('done')).toBeInTheDocument();
    expect(screen.getByText('complete')).toBeInTheDocument();
    expect(screen.queryByText('polling')).not.toBeInTheDocument();
    const n = calls.length;
    await new Promise((r) => setTimeout(r, 200));
    expect(calls.length).toBe(n);
    expect(calls[0]!.searchParams.get('tail')).toBe('500');
  });

  it('explains a partial log from a crashed agent with the server’s words', async () => {
    mockFetch(() => [200, page('partial', [line(1, 'before the crash')], {
      detail: 'The attempt ended without its log being finalized.',
    })]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    expect(await screen.findByText('before the crash')).toBeInTheDocument();
    expect(screen.getByText('partial')).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('ended without its log being finalized');
  });

  it('shows “no log” with the reason instead of an empty pane', async () => {
    mockFetch(() => [200, page('none', [], { detail: 'No log was recorded for this attempt.' })]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    expect(await screen.findByText('No log was recorded for this attempt.')).toBeInTheDocument();
    expect(screen.getByText('no log')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Download full log' })).toBeDisabled();
  });

  it('states truncation and dropped lines', async () => {
    mockFetch(() => [200, page('complete', [line(1)], { truncated_lines: 1234, dropped_lines: 7 })]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    const banner = await screen.findByRole('status');
    expect(banner).toHaveTextContent('1,234 lines were omitted');
    expect(banner).toHaveTextContent('7 lines were dropped');
  });

  it('marks the platform’s own lines distinctly', async () => {
    mockFetch(() => [200, page('complete', [
      line(1),
      { n: 2, stream: 'nexus', message: '[nexus] lines 2-9 are unavailable: object missing', kind: 'gap' } as never,
    ])]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);

    const gap = await screen.findByText(/lines 2-9 are unavailable/);
    expect(gap.closest('.log-line')).toHaveClass('nexus');
  });
});

describe('following', () => {
  it('polls from the cursor, appending without repeats, and says it is polling', async () => {
    let n = 0;
    mockFetch((url) => {
      if (url.searchParams.has('tail')) return [200, page('live', [line(1), line(2)])];
      n += 1;
      const from = Number(url.searchParams.get('from_line'));
      return [200, n === 1 ? { ...page('live', [line(from)]), next_line: from + 1 } : page('live', [])];
    });
    render(<LogTail namespace="default" attemptId="att-1" live />);

    expect(await screen.findByText('polling')).toBeInTheDocument();
    expect(await screen.findByText('line 3', {}, { timeout: 4000 })).toBeInTheDocument();
    const follows = calls.filter((c) => c.searchParams.has('from_line'));
    expect(follows[0]!.searchParams.get('from_line')).toBe('3');
    await waitFor(() => expect(calls.filter((c) => c.searchParams.has('from_line')).length).toBeGreaterThan(1), {
      timeout: 4000,
    });
    expect(calls.filter((c) => c.searchParams.has('from_line'))[1]!.searchParams.get('from_line')).toBe('4');
    expect(screen.getAllByText('line 3')).toHaveLength(1);
  });

  it('sends the credential on every request (EventSource could not)', async () => {
    mockFetch(() => [200, page('complete', [line(1, 'authorized')])]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);
    expect(await screen.findByText('authorized')).toBeInTheDocument();
  });
});

describe('two searches, never confused', () => {
  it('server search scans the whole log and can be continued', async () => {
    mockFetch((url) => {
      if (url.searchParams.has('q')) {
        const from = url.searchParams.get('from_line');
        return [200, {
          attempt_id: 'att-1',
          log: status('complete'),
          search: from
            ? { matches: [line(900, 'boom again')], next_line: 1001, scanned_lines: 1, scanned_bytes: 1, complete: true }
            : { matches: [line(40, 'boom')], next_line: 500, scanned_lines: 499, scanned_bytes: 1, complete: false, stopped_by: 'bytes' },
        }];
      }
      return [200, page('complete', [line(1, 'only the tail is loaded')])];
    });
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);
    await screen.findByText('only the tail is loaded');

    fireEvent.change(screen.getByLabelText('Search whole log'), { target: { value: 'bo+m' } });
    fireEvent.click(screen.getByLabelText('Regex'));
    fireEvent.click(screen.getByRole('button', { name: 'Search' }));

    expect(await screen.findByText('boom')).toBeInTheDocument();
    expect(screen.getByText(/stopped at line 499 \(scan size limit reached\)/)).toBeInTheDocument();
    const q = calls.find((c) => c.searchParams.has('q'))!;
    expect(q.searchParams.get('q')).toBe('bo+m');
    expect(q.searchParams.get('regex')).toBe('true');
    expect(q.searchParams.get('case')).toBe('insensitive');

    fireEvent.click(screen.getByRole('button', { name: 'Continue search from line 500' }));
    expect(await screen.findByText('boom again')).toBeInTheDocument();
    expect(screen.getByText(/searched the whole log/)).toBeInTheDocument();
  });

  it('shows the server’s explanation of an invalid pattern', async () => {
    mockFetch((url) =>
      url.searchParams.has('q')
        ? [400, { error: 'invalid_argument', message: 'bad log query: invalid regular expression' }]
        : [200, page('complete', [line(1)])],
    );
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);
    await screen.findByText('line 1');
    fireEvent.change(screen.getByLabelText('Search whole log'), { target: { value: '(' } });
    fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid regular expression');
  });

  it('the local filter only narrows loaded lines, and says so', async () => {
    mockFetch(() => [200, page('complete', [line(1, 'request complete', 'stdout'), line(2, 'request failed', 'stderr')])]);
    render(<LogTail namespace="default" attemptId="att-filter" live={false} />);
    await screen.findByText('request complete');
    fireEvent.change(screen.getByLabelText('Stream'), { target: { value: 'stderr' } });
    expect(screen.queryByText('request complete')).not.toBeInTheDocument();
    expect(screen.getByText('request failed')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Filter loaded lines'), { target: { value: 'missing' } });
    expect(screen.getByText('No loaded lines match these filters.')).toBeInTheDocument();
    expect(calls.some((c) => c.searchParams.has('q'))).toBe(false);
  });
});

describe('failures are stated honestly', () => {
  it('says log storage is not configured on a 501', async () => {
    mockFetch(() => [501, { error: 'logs_unavailable', message: 'no log backend' }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);
    expect(await screen.findByText(/Log storage is not configured on this gateway/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('a 404 is “not in this namespace”, not an emergency', async () => {
    mockFetch(() => [404, { message: 'not found' }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);
    expect(await screen.findByText(/not found in this namespace/)).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('shows a real server failure', async () => {
    mockFetch(() => [500, { message: 'log store is down' }]);
    render(<LogTail namespace="default" attemptId="att-1" live={false} />);
    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not load logs/i);
  });
});
