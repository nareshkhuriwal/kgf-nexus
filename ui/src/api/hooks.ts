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

import { useEffect, useRef } from 'react';
import {
  keepPreviousData,
  useMutation,
  useQuery,
  useQueryClient,
  type UseQueryOptions,
} from '@tanstack/react-query';
import { ApiError, api, type AttentionFilter, type RunListFilter, type RunPage } from './client';
import type {
  ChannelRequest,
  DeliveryFilter,
  RuleRequest,
  SensorRequest,
  TriggerRequest,
  Attempt,
  Backfill,
  CreateRunRequest,
  CreateScheduleRequest,
  RunAction,
  RunEvent,
  RunSummary,
  Schedule,
  StartBackfillRequest,
} from './types';
import { isTerminal } from '../lib/states';
import { pollInterval, type LiveTransport } from './live';
import {
  platformApi,
  TASK_RUNNER_DEFAULT_LIMIT,
  type ClusterSpec,
  type PlatformConfig,
} from './platform';

export const qk = {
  // The cursor is part of the key: each page of a keyset listing is a DIFFERENT
  // resource, not a refresh of the same one. Leaving `before` out would make
  // page 2 overwrite page 1's cache entry, and the back button — which replays
  // a cursor this client has already seen — would then re-fetch rather than
  // read what it already holds. The limit joins it for the same reason: the
  // page-size control changes what a page IS.
  runs: (ns: string, f: RunListFilter) =>
    ['runs', ns, f.state ?? '', f.spec ?? '', f.before ?? '', f.limit ?? 0] as const,
  run: (ns: string, id: string) => ['run', ns, id] as const,
  runAttempts: (ns: string, id: string) => ['run-attempts', ns, id] as const,
  runEvents: (ns: string, id: string) => ['run-events', ns, id] as const,
  workers: (ns: string) => ['workers', ns] as const,
  worker: (ns: string, id: string) => ['worker', ns, id] as const,
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
  channels: (ns: string) => ['notification-channels', ns] as const,
  rules: (ns: string) => ['notification-rules', ns] as const,
  deliveries: (ns: string, f: DeliveryFilter) =>
    ['notification-deliveries', ns, f.state ?? '', f.channel ?? '', f.run_id ?? '', f.before ?? '', f.limit ?? 0] as const,
  delivery: (ns: string, id: string) => ['notification-delivery', ns, id] as const,
  triggers: (ns: string) => ['triggers', ns] as const,
  trigger: (ns: string, name: string) => ['trigger', ns, name] as const,
  triggerDeliveries: (ns: string, name: string) => ['trigger-deliveries', ns, name] as const,
  sensors: (ns: string) => ['sensors', ns] as const,
  sensor: (ns: string, name: string) => ['sensor', ns, name] as const,
  sensorObservations: (ns: string, name: string, baseline: boolean) =>
    ['sensor-observations', ns, name, baseline] as const,
  assets: (ns: string, q: string) => ['assets', ns, q] as const,
  asset: (ns: string, uri: string) => ['asset', ns, uri] as const,
  assetGraph: (ns: string, uri: string, depth: number) => ['asset-graph', ns, uri, depth] as const,
  // Infrastructure (Cluster & scaling). One prefix per resource so a save can
  // invalidate exactly what it changed.
  platformConfig: (ns: string) => ['platform-config', ns] as const,
  platformPlan: (ns: string) => ['platform-plan', ns] as const,
  clusters: (ns: string) => ['platform-clusters', ns] as const,
  platformComponents: (ns: string) => ['platform-components', ns] as const,
  taskRunners: (ns: string, limit: number) => ['platform-task-runners', ns, limit] as const,
};

export function shouldRetry(failureCount: number, error: unknown): boolean {
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) return false;
  return failureCount < 2;
}

const base = { retry: shouldRetry, staleTime: 1000 } satisfies Partial<UseQueryOptions>;

/**
 * A page of runs, with the keyset cursor the caller needs to reach the next one.
 *
 * Returns the whole `RunPage` rather than unwrapping to rows, because the cursor
 * is not incidental metadata — it is the only thing that makes the run list
 * pageable at all, and a hook that dropped it would put every caller back to
 * fetching a capped 100 and stopping. Callers that genuinely want only rows
 * (the overview's fixed windows) use `useRunRows` below and say so.
 *
 * `placeholderData: keepPreviousData` is what stops a page turn from flashing
 * the loading skeleton: moving to the next cursor is a new query key, so
 * without it the table would unmount, AsyncPanel would render `Loading`, and
 * the operator would watch the list they were reading disappear for a round
 * trip. Keeping the previous page on screen while the next one arrives is the
 * same stale-beats-blank rule the StaleWarning path applies.
 */
export function useRuns(namespace: string, filter: RunListFilter) {
  return useQuery({
    queryKey: qk.runs(namespace, filter),
    queryFn: () => api.listRuns(namespace, filter),
    placeholderData: keepPreviousData,
    // The run list has no event stream of its own (events are per-run), so it
    // polls unconditionally. 4s keeps a busy list feeling live without turning
    // an idle dashboard into a load generator.
    refetchInterval: 4000,
    ...base,
  });
}

/**
 * The rows of a page of runs, for callers with no cursor to hold.
 *
 * The overview asks for a fixed window ("the most recent 40 runs") and renders
 * a preview of it; there is no "next page" on a dashboard panel, so handing it
 * a cursor it would have to ignore is worse than not handing it one. Shaped as
 * a query whose `data` is `RunSummary[]` so it drops straight into AsyncPanel,
 * which is what every consumer of it already does.
 *
 * `select` rather than a second queryFn on purpose: both hooks then share one
 * cache entry per filter, so the overview's two run queries and any run list on
 * the same key do not fetch twice.
 */
export function useRunRows(namespace: string, filter: RunListFilter) {
  return useQuery({
    queryKey: qk.runs(namespace, filter),
    queryFn: () => api.listRuns(namespace, filter),
    select: (page: RunPage) => page.runs,
    refetchInterval: 4000,
    ...base,
  });
}

export function useRun(namespace: string, runId: string, transport: LiveTransport) {
  const query = useQuery({
    queryKey: qk.run(namespace, runId),
    queryFn: () => api.getRun(namespace, runId),
    // Polled on the same schedule as the run's attempts and events, and for
    // the same reason: the event stream is a hint, not the only way the page
    // learns of change. Without this the run (its state, and the nodes a map
    // expands into) refreshed ONLY on an SSE message, so on the polling
    // fallback the header sat on RUNNING and the graph never grew its map
    // children while the attempts query, polling the same endpoint, already
    // had them. Stops once the run is terminal.
    refetchInterval: (q) =>
      pollInterval({ transport, terminal: q.state.data ? isTerminal('run', q.state.data.state) : false }),
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
  const query = useQuery({
    queryKey: key,
    queryFn: fn,
    enabled: opts.enabled ?? true,
    refetchInterval: pollInterval({ transport: opts.transport, terminal: opts.terminal }),
    ...base,
  });
  // Polling stops the moment the run is seen terminal, but this query's last
  // poll may predate the final transitions (up to one interval earlier). One
  // last read on the flip, or a run could show FAILED over an attempt frozen
  // at RUNNING forever -- which is what the e2e suite caught.
  const wasTerminal = useRef(opts.terminal);
  const { refetch } = query;
  useEffect(() => {
    if (opts.terminal && !wasTerminal.current && (opts.enabled ?? true)) void refetch();
    wasTerminal.current = opts.terminal;
  }, [opts.terminal, opts.enabled, refetch]);
  return query;
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

export function useWorkers(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.workers(namespace),
    queryFn: () => api.listWorkers(namespace),
    enabled,
    // Workers heartbeat every 5s (§11); polling faster than that shows nothing
    // new, polling much slower makes a dead worker look alive.
    refetchInterval: 5000,
    ...base,
  });
}

/**
 * One worker plus the tasks running on it.
 *
 * Polled at the same 5s as the fleet listing, and for the same reason: the
 * heartbeat interval is 5s (§11), so a faster poll shows nothing new while a
 * slower one lets a worker that has stopped taking work look busy. Sharing the
 * interval also keeps the detail page and the table it was reached from from
 * drifting into visibly different answers.
 *
 * Not gated on the worker being ACTIVE. A DEAD worker's page is exactly where
 * an operator lands after a task was lost, and refusing to poll it would freeze
 * the view at whatever the first read happened to catch.
 */
export function useWorker(namespace: string, workerId: string) {
  return useQuery({
    queryKey: qk.worker(namespace, workerId),
    queryFn: () => api.getWorker(namespace, workerId),
    enabled: workerId !== '',
    refetchInterval: 5000,
    ...base,
  });
}

export function useCapacity(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.capacity(namespace),
    queryFn: () => api.getCapacity(namespace),
    enabled,
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
 * One hook for every run lifecycle transition.
 *
 * The four share a cache-invalidation story exactly -- each changes the run,
 * its events and its place in the list -- so they share a hook. Four copies
 * would eventually invalidate three different sets.
 *
 * retry: false, like useCancelRun. These are not idempotent from the
 * OPERATOR's point of view: a retry that silently fired twice because the
 * first response was slow is a second execution of real work, and the
 * automatic retry a query client does for reads is wrong for them.
 */
export function useRunAction(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ runId, action }: { runId: string; action: RunAction }) =>
      api.runAction(namespace, runId, action),
    onSuccess: (_data, { runId }) => {
      void qc.invalidateQueries({ queryKey: qk.run(namespace, runId) });
      void qc.invalidateQueries({ queryKey: qk.runEvents(namespace, runId) });
      void qc.invalidateQueries({ queryKey: ['runs', namespace] });
      // The triage panel counts failed and stalled runs, so a transition that
      // resolves one must not leave it listed on the overview.
      void qc.invalidateQueries({ queryKey: ['attention', namespace] });
    },
    retry: false,
  });
}

/**
 * Drain a worker.
 *
 * Invalidates capacity as well as the worker list: draining removes a worker
 * from the schedulable pool, and a capacity panel still counting it would
 * explain a queue that stops moving as a mystery.
 */
export function useDrainWorker(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (workerId: string) => api.drainWorker(namespace, workerId),
    onSuccess: (_data, workerId) => {
      void qc.invalidateQueries({ queryKey: qk.workers(namespace) });
      void qc.invalidateQueries({ queryKey: qk.capacity(namespace) });
      // The drilled-down worker too, now that drain is offered from its own
      // page: leaving that entry stale would show ACTIVE next to a button the
      // operator has just used, which reads as the click having failed.
      void qc.invalidateQueries({ queryKey: qk.worker(namespace, workerId) });
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

export function useUpdateSchedule(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: CreateScheduleRequest) => api.updateSchedule(namespace, body),
    onSuccess: (updated: Schedule) => {
      qc.setQueryData(qk.schedule(namespace, updated.name), updated);
      void qc.invalidateQueries({ queryKey: qk.schedules(namespace) });
      void qc.invalidateQueries({ queryKey: qk.schedule(namespace, updated.name) });
    }, retry: false,
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
 * Delete a workflow and every version of it.
 *
 * retry: false, and here that is not merely the house rule. The gateway refuses
 * a referenced workflow with a 409 naming the blockers, which `shouldRetry`
 * would correctly decline to retry anyway — but a 5xx partway through a delete
 * is exactly the case where a second attempt could land on a half-removed
 * workflow, so the console asks once and reports what happened.
 *
 * Invalidates the catalog under EVERY search term, not just the current one:
 * the key carries the term (`qk.workflows(ns, search)`), so a targeted
 * invalidation would leave the operator's other tabs — and their own back
 * button — showing a workflow that no longer exists. `['workflows', ns]` is the
 * prefix every one of those keys starts with.
 */
export function useDeleteSpec(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteSpec(namespace, name),
    onSuccess: (_data, name) => {
      void qc.invalidateQueries({ queryKey: ['workflows', namespace] });
      qc.removeQueries({ queryKey: qk.specVersions(namespace, name) });
    },
    retry: false,
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

export function useAttention(namespace: string, filter: AttentionFilter = {}, enabled = true) {
  return useQuery({
    enabled,
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

// ── events: notifications, triggers, sensors, assets (ADR-0028) ─────────────

/**
 * 501 `not_configured`: the gateway was started without the events feature.
 *
 * Distinguished from a 404 on purpose. ApiError.isNotImplemented folds the two
 * together, which is right for "the endpoint is not built yet", but here a 404
 * is a real answer (unknown asset, deleted trigger) and must not be rendered as
 * "this server does not have the feature".
 */
export function isNotConfigured(error: unknown): boolean {
  return error instanceof ApiError && error.status === 501;
}

/** A 403: the credential lacks the permission. Rendered as Forbidden, not an error. */
export function isForbidden(error: unknown): boolean {
  return error instanceof ApiError && error.status === 403;
}

/** Configuration lists change only when someone edits them; 15s is plenty. */
const EVENTS_CONFIG_POLL_MS = 15_000;
/** Delivery and observation lists are activity logs; they move faster. */
const EVENTS_ACTIVITY_POLL_MS = 5_000;

/** No retry for a 501: the feature is off, and asking again will not turn it on. */
const eventsBase = {
  ...base,
  retry: (failureCount: number, error: unknown) =>
    !isNotConfigured(error) && shouldRetry(failureCount, error),
} satisfies Partial<UseQueryOptions>;

export function useChannels(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.channels(namespace),
    queryFn: () => api.listChannels(namespace),
    refetchInterval: EVENTS_CONFIG_POLL_MS,
    enabled,
    ...eventsBase,
  });
}

export function useRules(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.rules(namespace),
    queryFn: () => api.listRules(namespace),
    refetchInterval: EVENTS_CONFIG_POLL_MS,
    enabled,
    ...eventsBase,
  });
}

export function useDeliveries(namespace: string, filter: DeliveryFilter, enabled = true) {
  return useQuery({
    queryKey: qk.deliveries(namespace, filter),
    queryFn: () => api.listDeliveries(namespace, filter),
    placeholderData: keepPreviousData,
    refetchInterval: EVENTS_ACTIVITY_POLL_MS,
    enabled,
    ...eventsBase,
  });
}

/**
 * One delivery, polled while it is still pending: this is how a "Send test"
 * button learns whether the message actually left. Polling stops as soon as the
 * delivery reaches delivered or dead, since neither state changes again.
 */
export function useDelivery(namespace: string, id: string) {
  return useQuery({
    queryKey: qk.delivery(namespace, id),
    queryFn: () => api.getDelivery(namespace, id),
    enabled: id !== '',
    refetchInterval: (q) => (q.state.data && q.state.data.state !== 'pending' ? false : 1500),
    ...eventsBase,
  });
}

function invalidateNotifications(qc: ReturnType<typeof useQueryClient>, namespace: string) {
  void qc.invalidateQueries({ queryKey: qk.channels(namespace) });
  void qc.invalidateQueries({ queryKey: qk.rules(namespace) });
  void qc.invalidateQueries({ queryKey: ['notification-deliveries', namespace] });
}

export function useSaveChannel(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, body }: { name?: string; body: ChannelRequest }) =>
      name ? api.updateChannel(namespace, name, body) : api.createChannel(namespace, body),
    onSuccess: () => invalidateNotifications(qc, namespace),
  });
}

export function useDeleteChannel(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteChannel(namespace, name),
    onSuccess: () => invalidateNotifications(qc, namespace),
  });
}

export function useTestChannel(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.testChannel(namespace, name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['notification-deliveries', namespace] }),
  });
}

export function useSaveRule(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, body }: { name?: string; body: RuleRequest }) =>
      name ? api.updateRule(namespace, name, body) : api.createRule(namespace, body),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.rules(namespace) }),
  });
}

export function useDeleteRule(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteRule(namespace, name),
    onSuccess: () => void qc.invalidateQueries({ queryKey: qk.rules(namespace) }),
  });
}

export function useRedriveDelivery(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string | number) => api.redriveDelivery(namespace, id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ['notification-deliveries', namespace] }),
  });
}

export function useTriggers(namespace: string) {
  return useQuery({
    queryKey: qk.triggers(namespace),
    queryFn: () => api.listTriggers(namespace),
    refetchInterval: EVENTS_CONFIG_POLL_MS,
    ...eventsBase,
  });
}

export function useTrigger(namespace: string, name: string) {
  return useQuery({
    queryKey: qk.trigger(namespace, name),
    queryFn: () => api.getTrigger(namespace, name),
    enabled: name !== '',
    refetchInterval: EVENTS_CONFIG_POLL_MS,
    ...eventsBase,
  });
}

export function useTriggerDeliveries(namespace: string, name: string, enabled = true) {
  return useQuery({
    queryKey: qk.triggerDeliveries(namespace, name),
    queryFn: () => api.listTriggerDeliveries(namespace, name),
    enabled: enabled && name !== '',
    refetchInterval: EVENTS_ACTIVITY_POLL_MS,
    ...eventsBase,
  });
}

function invalidateTrigger(qc: ReturnType<typeof useQueryClient>, namespace: string, name: string) {
  void qc.invalidateQueries({ queryKey: qk.triggers(namespace) });
  void qc.invalidateQueries({ queryKey: qk.trigger(namespace, name) });
}

export function useCreateTrigger(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: TriggerRequest) => api.createTrigger(namespace, body),
    onSuccess: (t) => invalidateTrigger(qc, namespace, t.name),
  });
}

export function useUpdateTrigger(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: TriggerRequest }) =>
      api.updateTrigger(namespace, name, body),
    onSuccess: (t) => invalidateTrigger(qc, namespace, t.name),
  });
}

export function useDeleteTrigger(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteTrigger(namespace, name),
    onSuccess: (_v, name) => {
      void qc.invalidateQueries({ queryKey: qk.triggers(namespace) });
      qc.removeQueries({ queryKey: qk.trigger(namespace, name) });
    },
  });
}

export function useRotateTriggerSecret(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, graceSeconds }: { name: string; graceSeconds: number }) =>
      api.rotateTriggerSecret(namespace, name, graceSeconds),
    onSuccess: (t) => invalidateTrigger(qc, namespace, t.name),
  });
}

export function useSensors(namespace: string) {
  return useQuery({
    queryKey: qk.sensors(namespace),
    queryFn: () => api.listSensors(namespace),
    // Status is live (lease, next poll, last error), so faster than config.
    refetchInterval: EVENTS_ACTIVITY_POLL_MS * 2,
    ...eventsBase,
  });
}

export function useSensor(namespace: string, name: string) {
  return useQuery({
    queryKey: qk.sensor(namespace, name),
    queryFn: () => api.getSensor(namespace, name),
    enabled: name !== '',
    refetchInterval: EVENTS_ACTIVITY_POLL_MS * 2,
    ...eventsBase,
  });
}

export function useSensorObservations(namespace: string, name: string, includeBaseline: boolean) {
  return useQuery({
    queryKey: qk.sensorObservations(namespace, name, includeBaseline),
    queryFn: () => api.listSensorObservations(namespace, name, { includeBaseline }),
    enabled: name !== '',
    placeholderData: keepPreviousData,
    refetchInterval: EVENTS_ACTIVITY_POLL_MS * 2,
    ...eventsBase,
  });
}

function invalidateSensor(qc: ReturnType<typeof useQueryClient>, namespace: string, name: string) {
  void qc.invalidateQueries({ queryKey: qk.sensors(namespace) });
  void qc.invalidateQueries({ queryKey: qk.sensor(namespace, name) });
}

export function useCreateSensor(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: SensorRequest) => api.createSensor(namespace, body),
    onSuccess: (s) => invalidateSensor(qc, namespace, s.name),
  });
}

export function useUpdateSensor(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, body }: { name: string; body: SensorRequest }) =>
      api.updateSensor(namespace, name, body),
    onSuccess: (s) => invalidateSensor(qc, namespace, s.name),
  });
}

export function useDeleteSensor(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.deleteSensor(namespace, name),
    onSuccess: (_v, name) => {
      void qc.invalidateQueries({ queryKey: qk.sensors(namespace) });
      qc.removeQueries({ queryKey: qk.sensor(namespace, name) });
    },
  });
}

export function useAssets(namespace: string, q: string) {
  return useQuery({
    queryKey: qk.assets(namespace, q),
    queryFn: () => api.listAssets(namespace, { q }),
    placeholderData: keepPreviousData,
    refetchInterval: EVENTS_CONFIG_POLL_MS * 2,
    ...eventsBase,
  });
}

export function useAsset(namespace: string, uri: string) {
  return useQuery({
    queryKey: qk.asset(namespace, uri),
    queryFn: () => api.getAsset(namespace, uri),
    enabled: uri !== '',
    refetchInterval: EVENTS_CONFIG_POLL_MS * 2,
    ...eventsBase,
  });
}

export function useAssetGraph(namespace: string, uri: string, depth: number, enabled = true) {
  return useQuery({
    queryKey: qk.assetGraph(namespace, uri, depth),
    queryFn: () => api.getAssetGraph(namespace, { uri: uri || undefined, depth: uri ? depth : undefined }),
    enabled,
    placeholderData: keepPreviousData,
    ...eventsBase,
  });
}


// ── infrastructure (Cluster & scaling) ──────────────────────────────────────

/**
 * Poll intervals for the infrastructure page, in one place so the page can say
 * what it is doing ("refreshes every 10s") from the same numbers it does it with.
 *
 * Each matches how fast its source can change: task runners turn over in
 * seconds; component instances report on a heartbeat a few times slower; the
 * cluster controller reports every 15s (docs/CLUSTER-SCALING.md), so polling it
 * faster would only re-read the same report. Polling stops while the browser
 * tab is hidden — TanStack Query's refetchIntervalInBackground defaults to
 * false — and refetchOnWindowFocus catches up on return.
 */
export const INFRA_POLL_MS = {
  components: 10_000,
  taskRunners: 5_000,
  clusters: 15_000,
  plan: 15_000,
} as const;

/** The saved policy. Not polled: it changes only when someone saves it. */
export function usePlatformConfig(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.platformConfig(namespace),
    queryFn: () => platformApi.getConfig(namespace),
    enabled,
    ...base,
  });
}

export function usePlatformPlan(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.platformPlan(namespace),
    queryFn: () => platformApi.getPlan(namespace),
    enabled,
    refetchInterval: INFRA_POLL_MS.plan,
    ...base,
  });
}

export function useClusters(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.clusters(namespace),
    queryFn: () => platformApi.listClusters(namespace),
    enabled,
    refetchInterval: INFRA_POLL_MS.clusters,
    ...base,
  });
}

/** Control-plane instances (C3). `kind: 'unsupported'` on an older gateway. */
export function usePlatformComponents(namespace: string, enabled = true) {
  return useQuery({
    queryKey: qk.platformComponents(namespace),
    queryFn: () => platformApi.getComponents(namespace),
    enabled,
    refetchInterval: INFRA_POLL_MS.components,
    ...base,
  });
}

/** Running task attempts with their placement (C3). */
export function useTaskRunners(namespace: string, limit = TASK_RUNNER_DEFAULT_LIMIT, enabled = true) {
  return useQuery({
    queryKey: qk.taskRunners(namespace, limit),
    queryFn: () => platformApi.listTaskRunners(namespace, limit),
    enabled,
    refetchInterval: INFRA_POLL_MS.taskRunners,
    ...base,
  });
}

/**
 * Save the policy. The response IS the new saved document, so it is written
 * straight into the cache rather than re-read; the plan and the cluster view
 * both derive from it and are invalidated.
 */
export function useSavePlatformConfig(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (config: PlatformConfig) => platformApi.saveConfig(namespace, config),
    onSuccess: (saved) => {
      qc.setQueryData(qk.platformConfig(namespace), saved);
      void qc.invalidateQueries({ queryKey: qk.platformPlan(namespace) });
      void qc.invalidateQueries({ queryKey: qk.clusters(namespace) });
    },
    retry: false,
  });
}

export function useSaveCluster(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, spec }: { name: string; spec: ClusterSpec }) => platformApi.saveCluster(namespace, name, spec),
    onSettled: () => {
      // Also on error: a 409 means the registry moved under us, and the fix is
      // to show the operator what it moved to.
      void qc.invalidateQueries({ queryKey: qk.clusters(namespace) });
      void qc.invalidateQueries({ queryKey: qk.platformPlan(namespace) });
    },
    retry: false,
  });
}

export function useDeleteCluster(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, revision }: { name: string; revision: number }) =>
      platformApi.deleteCluster(namespace, name, revision),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: qk.clusters(namespace) });
      void qc.invalidateQueries({ queryKey: qk.platformPlan(namespace) });
    },
    retry: false,
  });
}

/**
 * Mint an agent credential. The secret lives only in the caller's state: it is
 * never written to the query cache, so it cannot outlive the screen showing it.
 */
export function useMintWorkerToken(namespace: string) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (input: { name: string; expires_in_days: number }) => platformApi.mintWorkerToken(namespace, input),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['tokens', namespace] });
      void qc.invalidateQueries({ queryKey: ['audit', namespace] });
    },
    // Collected as soon as the caller resets it, so the secret in its result
    // does not linger in the mutation cache.
    gcTime: 0,
    retry: false,
  });
}
