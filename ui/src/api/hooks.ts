// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

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
import { ApiError, api, type AttentionFilter, type RunListFilter } from './client';
import type {
  Attempt,
  Backfill,
  CreateRunRequest,
  CreateScheduleRequest,
  RunEvent,
  RunSummary,
  Schedule,
  StartBackfillRequest,
} from './types';
import { isTerminal } from '../lib/states';
import { pollInterval, type LiveTransport } from './live';

export const qk = {
  runs: (ns: string, f: RunListFilter) => ['runs', ns, f.state ?? '', f.spec ?? ''] as const,
  run: (ns: string, id: string) => ['run', ns, id] as const,
  runAttempts: (ns: string, id: string) => ['run-attempts', ns, id] as const,
  runEvents: (ns: string, id: string) => ['run-events', ns, id] as const,
  workers: (ns: string) => ['workers', ns] as const,
  capacity: (ns: string) => ['capacity', ns] as const,
  schedules: (ns: string) => ['schedules', ns] as const,
  schedule: (ns: string, name: string) => ['schedule', ns, name] as const,
  cronPreview: (ns: string, cron: string, tz: string) => ['cron-preview', ns, cron, tz] as const,
  backfills: (ns: string, name: string) => ['backfills', ns, name] as const,
  backfill: (ns: string, id: string) => ['backfill', ns, id] as const,
  workflows: (ns: string, search: string) => ['workflows', ns, search] as const,
  specVersions: (ns: string, name: string) => ['spec-versions', ns, name] as const,
  specDef: (ns: string, id: string) => ['spec-def', ns, id] as const,
  attention: (ns: string, f: AttentionFilter) =>
    ['attention', ns, f.limit ?? 0, f.blocked_for ?? '', f.failed_within ?? '', f.stalled_for ?? ''] as const,
  // No namespace segment: /v1/health/fleet is one control plane, so every
  // namespace shares the cache entry rather than refetching on a switch.
  fleetHealth: () => ['fleet-health'] as const,
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


// ── schedules ───────────────────────────────────────────────────────────────

/**
 * A schedule's next_fire_at advances on its own, without anyone touching the
 * console, so the list is polled. 10s rather than the run list's 4s: the
 * soonest anything can change is the next cron tick, and a schedule table is a
 * management view, not an incident view.
 */
const SCHEDULE_POLL_MS = 10_000;

export function useSchedules(namespace: string) {
  return useQuery({
    queryKey: qk.schedules(namespace),
    queryFn: () => api.listSchedules(namespace),
    refetchInterval: SCHEDULE_POLL_MS,
    ...base,
  });
}

export function useSchedule(namespace: string, name: string) {
  return useQuery({
    queryKey: qk.schedule(namespace, name),
    queryFn: () => api.getSchedule(namespace, name),
    enabled: name !== '',
    refetchInterval: SCHEDULE_POLL_MS,
    ...base,
  });
}

/**
 * Live cron feedback for the create form.
 *
 * Debounced by the CALLER passing a settled expression rather than here, so this
 * hook stays a plain query: a half-typed "0 2 * *" is a valid request that
 * answers valid:false, and firing one per keystroke would make the form flicker
 * between "invalid" and "invalid" while someone types.
 *
 * retry is off entirely. A bad expression is a 200 with valid:false, so the only
 * things left to retry are transport failures -- and a preview that silently
 * retries for six seconds while the operator types is worse than one that says
 * nothing yet.
 */
export function useCronPreview(namespace: string, cron: string, timezone: string, count = 5) {
  return useQuery({
    queryKey: qk.cronPreview(namespace, cron, timezone),
    queryFn: () => api.previewCron(namespace, cron, timezone, count),
    enabled: cron.trim() !== '',
    retry: false,
    // Preview answers are pure functions of (cron, timezone) apart from the
    // clock they start from, so they are worth caching for the length of a form
    // session -- flipping back to a previously-tried expression is instant.
    staleTime: 30_000,
  });
}

export function useCreateSchedule(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateScheduleRequest) => api.createSchedule(namespace, body),
    onSuccess: (created: Schedule) => {
      void qc.invalidateQueries({ queryKey: qk.schedules(namespace) });
      void qc.invalidateQueries({ queryKey: qk.schedule(namespace, created.name) });
    },
    retry: false,
  });
}

/**
 * Enable and disable, which are one mutation with a flag on the wire.
 *
 * No optimistic update: enabling RECOMPUTES next_fire_at server-side (restoring
 * a week-old fire time would fire a burst of catch-up nobody asked for), so the
 * only honest next_fire_at is the one the server returns.
 */
export function useSetScheduleEnabled(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, enabled }: { name: string; enabled: boolean }) =>
      api.setScheduleEnabled(namespace, name, enabled),
    onSuccess: (updated: Schedule) => {
      void qc.invalidateQueries({ queryKey: qk.schedules(namespace) });
      void qc.invalidateQueries({ queryKey: qk.schedule(namespace, updated.name) });
    },
    retry: false,
  });
}

export function useDeleteSchedule(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteSchedule(namespace, name),
    onSuccess: (_data, name) => {
      void qc.invalidateQueries({ queryKey: qk.schedules(namespace) });
      qc.removeQueries({ queryKey: qk.schedule(namespace, name) });
    },
    retry: false,
  });
}

// ── backfills ───────────────────────────────────────────────────────────────

/**
 * A backfill is long-running and has no event stream of its own, so progress is
 * polled. 3s matches the polling-transport interval the run views use: this is
 * the one schedule-side view an operator actually watches tick.
 */
const BACKFILL_POLL_MS = 3000;

/** Terminal backfills never change again, so they stop costing requests. */
function backfillInterval(rows: Backfill[] | undefined): number | false {
  if (!rows) return BACKFILL_POLL_MS;
  return rows.some((b) => !isTerminal('backfill', b.state)) ? BACKFILL_POLL_MS : false;
}

export function useBackfills(namespace: string, scheduleName: string) {
  return useQuery({
    queryKey: qk.backfills(namespace, scheduleName),
    queryFn: () => api.listBackfills(namespace, scheduleName),
    enabled: scheduleName !== '',
    refetchInterval: (query) => backfillInterval(query.state.data),
    ...base,
  });
}

export function useBackfill(namespace: string, id: string) {
  return useQuery({
    queryKey: qk.backfill(namespace, id),
    queryFn: () => api.getBackfill(namespace, id),
    enabled: id !== '',
    refetchInterval: (query) => backfillInterval(query.state.data ? [query.state.data] : undefined),
    ...base,
  });
}

export function useStartBackfill(namespace: string, scheduleName: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: StartBackfillRequest) =>
      api.startBackfill(namespace, scheduleName, body),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: qk.backfills(namespace, scheduleName) });
      // A backfill creates runs, so the run list is stale the moment it starts.
      void qc.invalidateQueries({ queryKey: ['runs', namespace] });
    },
    retry: false,
  });
}

/**
 * pause / resume / cancel.
 *
 * Cancel stops GENERATING runs; it does not cancel the runs already created.
 * The screens say so before the click rather than after, which is why this hook
 * does not pretend the distinction away with an optimistic terminal state.
 */
export function useBackfillAction(namespace: string, scheduleName: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, action }: { id: string; action: 'pause' | 'resume' | 'cancel' }) =>
      api.backfillAction(namespace, id, action),
    onSuccess: (updated: Backfill) => {
      void qc.invalidateQueries({ queryKey: qk.backfills(namespace, scheduleName) });
      void qc.invalidateQueries({ queryKey: qk.backfill(namespace, updated.id) });
    },
    retry: false,
  });
}

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  if (error instanceof Error) return error.message;
  return 'Unknown error.';
}


// ── workflow catalog ────────────────────────────────────────────────────────

/**
 * The catalog list.
 *
 * Not polled. A workflow appears here only when someone submits a spec through
 * the SDK or CLI, which is an authoring act minutes or days apart -- a timer
 * would spend requests to show the same 15 rows. The search term is part of the
 * query key, so each settled term caches independently and backspacing through
 * a word is instant rather than a second round trip per character.
 *
 * The CALLER debounces the term, matching the cron preview: this stays a plain
 * query so a component that already has a settled value does not pay for a
 * second delay.
 */
export function useWorkflows(namespace: string, search: string) {
  return useQuery({
    queryKey: qk.workflows(namespace, search),
    queryFn: () => api.listWorkflows(namespace, search),
    // Long enough that typing back and forth across a search term is free, short
    // enough that a spec submitted from the CLI shows up without a hard reload.
    staleTime: 30_000,
    retry: shouldRetry,
  });
}

export function useSpecVersions(namespace: string, name: string) {
  return useQuery({
    queryKey: qk.specVersions(namespace, name),
    queryFn: () => api.listSpecVersions(namespace, name),
    enabled: name !== '',
    staleTime: 30_000,
    retry: shouldRetry,
  });
}

/**
 * One spec definition.
 *
 * A spec is immutable and content-addressed: the bytes behind an id can never
 * change, so this is cached for the session rather than re-fetched. Flipping
 * between versions to compare them is then instant, which is the whole point of
 * putting version history next to the definition.
 */
export function useSpecDefinition(namespace: string, specId: string) {
  return useQuery({
    queryKey: qk.specDef(namespace, specId),
    queryFn: () => api.getSpecDefinition(namespace, specId),
    enabled: specId !== '',
    staleTime: Infinity,
    retry: shouldRetry,
  });
}

/**
 * Submit a run from the catalog.
 *
 * retry is off, as with every other mutation here: a run submission is not
 * idempotent without an Idempotency-Key, and the console deliberately does not
 * send one (see api.createRun), so a silent retry could create a second run.
 */
export function useCreateRun(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateRunRequest) => api.createRun(namespace, body),
    onSuccess: (created: RunSummary) => {
      void qc.invalidateQueries({ queryKey: ['runs', namespace] });
      if (created?.id) void qc.invalidateQueries({ queryKey: qk.run(namespace, created.id) });
    },
    retry: false,
  });
}


// ── attention & fleet connectivity ──────────────────────────────────────────

/**
 * The triage list.
 *
 * 10s rather than the run list's 4s. The list is a RANKED AGGREGATE over runs,
 * attempts and capacity waits, so it is the most expensive read on the overview,
 * and nothing it reports becomes actionable inside four seconds — a retry budget
 * that ran out stays run out. Polling it at incident speed would spend the
 * gateway's time to re-render the same nineteen rows.
 */
const ATTENTION_POLL_MS = 10_000;

export function useAttention(namespace: string, filter: AttentionFilter = {}) {
  return useQuery({
    queryKey: qk.attention(namespace, filter),
    queryFn: () => api.getAttention(namespace, filter),
    refetchInterval: ATTENTION_POLL_MS,
    ...base,
  });
}

/**
 * Fleet connectivity.
 *
 * Cheap server-side — the gateway reads its own gRPC channel states without a
 * network round trip — but cheap is not free: it is still an HTTP request per
 * tick, and a channel does not change state on a 4s cadence. 15s is slow enough
 * to cost nothing and fast enough that an upstream dropping to
 * TRANSIENT_FAILURE surfaces while the operator is still looking at the page.
 */
const FLEET_POLL_MS = 15_000;

export function useFleetHealth() {
  return useQuery({
    queryKey: qk.fleetHealth(),
    queryFn: () => api.getFleetHealth(),
    refetchInterval: FLEET_POLL_MS,
    ...base,
  });
}
