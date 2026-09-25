// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The run query polls while the run is live and stops once it is terminal.
 *
 * Found by the browser e2e suite: the run (its state and the nodes a map
 * expands into) used to refresh only on an SSE message, so on the polling
 * fallback the run header stayed RUNNING while the attempts query, polling
 * the same endpoint, already showed the run finished.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { useRun, useRunPolling } from './hooks';

let served: { state: string };
let calls = 0;

beforeEach(() => {
  vi.useFakeTimers();
  calls = 0;
  window.localStorage.setItem('nexus.apiKey', 'nxs_test');
  vi.stubGlobal('fetch', () => {
    calls += 1;
    return Promise.resolve(
      new Response(JSON.stringify({ id: 'r1', namespace: 'default', spec_id: 's', created_at: '2026-09-25T10:00:00Z', nodes: [], edges: [], ...served }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

let qc: QueryClient;
beforeEach(() => {
  // One client per test, not per render: a rerender must see the same cache.
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
});

function wrapper({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
}

async function flush(ms: number) {
  // Twice: the query layer schedules its notification on a timer AFTER the
  // fetch promise settles, so one advance can end between the two.
  for (const step of [ms, 1]) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(step);
    });
  }
}

describe('useRun', () => {
  it('polls a live run and picks up its terminal state without any event', async () => {
    served = { state: 'RUNNING' };
    const { result } = renderHook(() => useRun('default', 'r1', 'polling'), { wrapper });
    await flush(10);
    expect(result.current.data?.state).toBe('RUNNING');

    served = { state: 'SUCCEEDED' };
    await flush(3100);
    expect(result.current.data?.state).toBe('SUCCEEDED');
    expect(result.current.terminal).toBe(true);

    const after = calls;
    await flush(30_000);
    expect(calls).toBe(after);
  });

  it('re-reads the projections once when the run turns terminal, then stops', async () => {
    vi.useRealTimers();
    let reads = 0;
    const fn = () => Promise.resolve(++reads);
    const { rerender, result } = renderHook(
      ({ terminal }: { terminal: boolean }) => useRunPolling(['attempts-probe'], fn, { transport: 'polling', terminal }),
      { wrapper, initialProps: { terminal: false } },
    );
    await waitFor(() => expect(result.current.data).toBe(1));
    // The run is seen terminal between two polls: one final read, not zero.
    rerender({ terminal: true });
    await waitFor(() => expect(result.current.data).toBe(2));
    // And no polling after that.
    const cached = qc.getQueryCache().find({ queryKey: ['attempts-probe'] });
    expect((cached?.options as { refetchInterval?: unknown }).refetchInterval).toBe(false);
    expect(reads).toBe(2);
  });
});
