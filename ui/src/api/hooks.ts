/**
 * TanStack Query bindings.
 *
 * Retry policy is deliberate: a 4xx is not retried (a 404 means the endpoint
 * does not exist yet, and retrying it four times just delays the honest error
 * message by several seconds), while a transport failure or 5xx gets two quick
 * retries because a service restarting under Compose is the common case.
 */

import { useEffect } from 'react';
import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryOptions,
} from '@tanstack/react-query';
import { ApiError, api, type RunListFilter } from './client';
import type { Attempt, RunEvent } from './types';
import { isTerminal } from '../lib/states';
import { pollInterval, type LiveTransport } from './live';

export const qk = {
  runs: (ns: string, f: RunListFilter) => ['runs', ns, f.state ?? '', f.spec ?? ''] as const,
  run: (ns: string, id: string) => ['run', ns, id] as const,
  runAttempts: (ns: string, id: string) => ['run-attempts', ns, id] as const,
  runEvents: (ns: string, id: string) => ['run-events', ns, id] as const,
  workers: (ns: string) => ['workers', ns] as const,
  capacity: (ns: string) => ['capacity', ns] as const,
};

export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) return false;
  return failureCount < 2;
}

const base = { retry: shouldRetry, staleTime: 1000 } satisfies Partial<UseQueryOptions>;

export function useRuns(namespace: string, filter: RunListFilter) {
  return useQuery({
    queryKey: qk.runs(namespace, filter),
    queryFn: () => api.listRuns(namespace, filter),
    // The run list has no event stream of its own (events are per-run), so it
    // polls unconditionally. 4s keeps a busy list feeling live without turning
    // an idle dashboard into a load generator.
    refetchInterval: 4000,
    ...base,
  });
}

export function useRun(namespace: string, runId: string, transport: LiveTransport) {
  const query = useQuery({
    queryKey: qk.run(namespace, runId),
    queryFn: () => api.getRun(namespace, runId),
    ...base,
  });
  const terminal = query.data ? isTerminal('run', query.data.state) : false;
  return { ...query, terminal, interval: pollInterval({ transport, terminal }) };
}

export function useRunPolling<T>(
  key: readonly unknown[],
  fn: () => Promise<T>,
  opts: { transport: LiveTransport; terminal: boolean; enabled?: boolean },
) {
  return useQuery({
    queryKey: key,
    queryFn: fn,
    enabled: opts.enabled ?? true,
    refetchInterval: pollInterval({ transport: opts.transport, terminal: opts.terminal }),
    ...base,
  });
}

export function useRunAttempts(
  namespace: string,
  runId: string,
  transport: LiveTransport,
  terminal: boolean,
) {
  return useRunPolling<Attempt[]>(
    qk.runAttempts(namespace, runId),
    () => api.listRunAttempts(namespace, runId),
    { transport, terminal },
  );
}

export function useRunEvents(
  namespace: string,
  runId: string,
  transport: LiveTransport,
  terminal: boolean,
) {
  return useRunPolling<RunEvent[]>(
    qk.runEvents(namespace, runId),
    () => api.listEvents(namespace, runId),
    { transport, terminal },
  );
}

export function useWorkers(namespace: string) {
  return useQuery({
    queryKey: qk.workers(namespace),
    queryFn: () => api.listWorkers(namespace),
    // Workers heartbeat every 5s (§11); polling faster than that shows nothing
    // new, polling much slower makes a dead worker look alive.
    refetchInterval: 5000,
    ...base,
  });
}

export function useCapacity(namespace: string) {
  return useQuery({
    queryKey: qk.capacity(namespace),
    queryFn: () => api.getCapacity(namespace),
    refetchInterval: 5000,
    ...base,
  });
}

/**
 * Cancel is the only Phase 1 mutation. No optimistic update: cancel is
 * asynchronous by design (the run goes CANCELLING, then CANCELLED once workers
 * ack, §5.2), so faking a terminal state would show the operator a lie. We
 * invalidate and let the server report the real intermediate state.
 */
export function useCancelRun(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (runId: string) => api.cancelRun(namespace, runId),
    onSuccess: (_data, runId) => {
      void qc.invalidateQueries({ queryKey: qk.run(namespace, runId) });
      void qc.invalidateQueries({ queryKey: qk.runEvents(namespace, runId) });
      void qc.invalidateQueries({ queryKey: ['runs', namespace] });
    },
    retry: false,
  });
}

/**
 * Re-reads a run's projections whenever the event stream ticks.
 *
 * This is the join between the live transport and the query cache: the stream
 * says "something changed", the server says what. Keeping the two separate is
 * why a dropped SSE message costs latency rather than correctness.
 */
export function useInvalidateOnRevision(
  revision: number,
  namespace: string,
  runId: string | undefined,
): void {
  const qc = useQueryClient();
  useEffect(() => {
    if (!runId || revision === 0) return;
    void qc.invalidateQueries({ queryKey: qk.run(namespace, runId) });
    void qc.invalidateQueries({ queryKey: qk.runAttempts(namespace, runId) });
    void qc.invalidateQueries({ queryKey: qk.runEvents(namespace, runId) });
  }, [qc, revision, namespace, runId]);
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  if (error instanceof Error) return error.message;
  return 'Unknown error.';
}
