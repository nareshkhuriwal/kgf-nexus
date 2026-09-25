// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Thin REST client for architecture §8.2.
 *
 * Everything the UI knows about the backend lives here. Two properties matter
 * more than elegance:
 *
 *  1. A dead or half-built backend produces a typed ApiError with a readable
 *     message, never a thrown `undefined is not an object` three components
 *     deep. The MVP explicitly ships alongside services that are still being
 *     written, so "endpoint not implemented yet" is a first-class outcome.
 *  2. Responses are normalised to arrays. A handler that returns `null`, a bare
 *     array, or `{items:[...]}` all render the same, so the UI does not break
 *     on a paging decision that has not been made yet.
 */

import type {
  Attempt,
  AttentionList,
  Backfill,
  CapacitySummary,
  CreateRunRequest,
  CreateScheduleRequest,
  CronPreview,
  FleetHealth,
  LogLine,
  RunAction,
  RunDetail,
  RunEvent,
  RunSummary,
  Schedule,
  SpecDefinition,
  SpecVersion,
  StartBackfillRequest,
  Worker,
  WorkerQuarantine,
  WorkerDetail,
  WorkflowSummary,
} from './types';

/** Injected at build time so the same bundle can front any gateway. */
const RAW_BASE = (import.meta.env?.VITE_NEXUS_API_BASE as string | undefined) ?? '';
export const API_BASE = RAW_BASE.replace(/\/$/, '');

export const DEFAULT_NAMESPACE =
  (import.meta.env?.VITE_NEXUS_NAMESPACE as string | undefined) ?? 'default';

/**
 * Build-time key, used only as a fallback for a local Compose convenience build.
 *
 * Anything baked into the bundle is readable by everyone who loads the page, so
 * the runtime key below takes precedence: the operator supplies it in the
 * browser and it never leaves that browser.
 */
const BUILD_API_KEY = (import.meta.env?.VITE_NEXUS_API_KEY as string | undefined) ?? '';

const API_KEY_STORAGE = 'nexus.apiKey';

/**
 * The API key the console should present.
 *
 * Read on every request rather than captured once, so saving a key takes effect
 * immediately without a reload -- the console is otherwise stuck showing 401s
 * until the operator thinks to refresh, which reads as "my key is wrong".
 */
export function getApiKey(): string {
  try {
    // Trim on READ as well as on write. A key pasted from a terminal very often
    // carries a trailing space or newline, and a key stored by an earlier build
    // was never trimmed at all -- both produce a 401 that looks identical to a
    // wrong key, which is close to undiagnosable from the UI.
    const stored = window.localStorage.getItem(API_KEY_STORAGE)?.trim();
    if (stored) return stored;
  } catch {
    // Private browsing and blocked site-data both throw here. Falling through to
    // the build-time key keeps the console usable rather than failing outright.
  }
  return BUILD_API_KEY;
}

/**
 * Persist the key, returning whether it is actually readable afterwards.
 *
 * The write is verified by reading it back rather than assumed to have worked.
 * localStorage can silently fail to persist (blocked site data, private
 * browsing, storage pressure), and a header that reports success while nothing
 * was stored is the worst outcome: every request is refused while the console
 * insists a key is set, which is exactly the state this bug produced.
 */
export function setApiKey(key: string): boolean {
  const clean = key.trim();
  try {
    if (clean) window.localStorage.setItem(API_KEY_STORAGE, clean);
    else window.localStorage.removeItem(API_KEY_STORAGE);
  } catch {
    return false;
  }
  return getApiKey() === clean;
}

export class ApiError extends Error {
  readonly status: number;
  readonly url: string;
  readonly body: string;

  constructor(message: string, opts: { status: number; url: string; body?: string }) {
    super(message);
    this.name = 'ApiError';
    this.status = opts.status;
    this.url = opts.url;
    this.body = opts.body ?? '';
  }

  /**
   * status 0 means the request never reached a server (connection refused, DNS,
   * CORS). Distinguishing it from a 500 is the difference between "the backend
   * is down" and "the backend is broken", which are different operator actions.
   */
  get isUnreachable(): boolean {
    return this.status === 0;
  }

  /** A 404/501 on a read endpoint most likely means "not built yet" in Phase 1. */
  get isNotImplemented(): boolean {
    return this.status === 404 || this.status === 501;
  }
}

/**
 * The credential every request presents.
 *
 * A SESSION token takes precedence over a stored API key, because logging in is
 * the more specific and more recent act: an operator who signs in means to act
 * as themselves even on a browser that still has a key saved from before. When
 * there is no session this is exactly the previous behaviour, which is what
 * keeps the API-key escape hatch (and CLI parity) working unchanged.
 *
 * Read on every request rather than captured once, so logging in or out takes
 * effect immediately without a reload.
 *
 * The session is read here directly rather than through api/auth.ts: that module
 * imports this one, and routing the lookup back through it would be a cycle.
 */
function currentToken(): string {
  try {
    const raw = window.localStorage.getItem('nexus.session');
    if (raw) {
      const parsed = JSON.parse(raw) as { token?: string; expiresAt?: string };
      // A locally-expired token is not presented: the server would refuse it, and
      // a 401 on first load reads as "my login broke" rather than "it ended".
      const live = !parsed.expiresAt || Date.parse(parsed.expiresAt) > Date.now();
      if (parsed.token && live) return parsed.token;
    }
  } catch {
    /* no session, unreadable storage, or malformed JSON: fall back to the key */
  }
  return getApiKey();
}

function authHeaders(): Record<string, string> {
  const token = currentToken();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

export function apiUrl(path: string): string {
  return `${API_BASE}${path}`;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const url = apiUrl(path);
  let res: Response;
  try {
    res = await fetch(url, {
      ...init,
      headers: { Accept: 'application/json', ...authHeaders(), ...(init?.headers ?? {}) },
    });
  } catch (cause) {
    throw new ApiError(
      `Cannot reach the Nexus API at ${url || '/'}. Is the api-gateway running?`,
      { status: 0, url, body: String(cause) },
    );
  }

  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new ApiError(serverMessage(res.status, body), { status: res.status, url, body });
  }

  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (text.trim() === '') return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError(`Server returned non-JSON (${res.status}) from ${url}.`, {
      status: res.status,
      url,
      body: text.slice(0, 400),
    });
  }
}

/** Prefers a server-supplied message; falls back to a status explanation. */
function serverMessage(status: number, body: string): string {
  try {
    const parsed = JSON.parse(body) as { message?: string; error?: string };
    const msg = parsed.message ?? parsed.error;
    if (msg) return `${msg} (HTTP ${status})`;
  } catch {
    /* body was not JSON; fall through to the generic text */
  }
  if (status === 401 || status === 403) {
    // Name the control that fixes it. "Check the API key" leaves an operator
    // hunting through config for something they can set in the header.
    const token = currentToken();
    if (!token) {
      return `Not authorised (HTTP ${status}). You are not signed in — log in, or set an API key in the header.`;
    }
    // 403 means the key was RECOGNISED but lacks the role, which is a different
    // fix from 401 (key not recognised at all). Saying "check your key" for a
    // 403 sends the operator to re-paste a key that was never the problem.
    if (status === 403) {
      return `Forbidden (HTTP 403). This key is valid but lacks permission — a worker key cannot read this; use an admin key.`;
    }
    return `Not authorised (HTTP 401). The credential in this browser was refused. Your session may have expired — log in again, or replace the API key in the header.`;
  }
  if (status === 404) return `Not found (HTTP 404). This endpoint may not be implemented yet.`;
  if (status === 501) return `Not implemented yet (HTTP 501).`;
  if (status >= 500) return `The server failed (HTTP ${status}).`;
  return `Request failed (HTTP ${status}).`;
}

/** Accepts `T[]`, `{items:T[]}`, `{<key>:T[]}` or null. */
function asList<T>(payload: unknown, ...keys: string[]): T[] {
  if (Array.isArray(payload)) return payload as T[];
  if (payload && typeof payload === 'object') {
    const obj = payload as Record<string, unknown>;
    for (const key of ['items', ...keys]) {
      if (Array.isArray(obj[key])) return obj[key] as T[];
    }
  }
  return [];
}

const ns = (namespace: string) => `/v1/namespaces/${encodeURIComponent(namespace)}`;

/**
 * What the run detail endpoint actually returns.
 *
 * The wire response carries attempts alongside the nodes and edges. RunDetail
 * does not declare them because no consumer of a run object should reach for the
 * attempt list through it — attempts have their own query, which is what keeps a
 * node panel from re-rendering the whole page when one attempt transitions. The
 * field is declared here so the one function that does read it is type-checked.
 */
interface RunDetailPayload extends RunDetail {
  attempts?: Attempt[];
}

export interface RunListFilter {
  state?: string;
  spec?: string;
  limit?: number;
  /**
   * The keyset cursor: "runs created strictly before this instant".
   *
   * An RFC3339 timestamp, and it is the gateway's own `next_before` echoed back
   * untouched rather than a value this client formats. The gateway parses it
   * with time.Parse(time.RFC3339) and 400s on anything it cannot read, so
   * re-deriving it here — from a run's created_at, say, or by rounding — is how
   * a page boundary silently moves and a run gets shown twice or skipped.
   */
  before?: string;
}

/**
 * A page of runs, plus the cursor that reaches the next one.
 *
 * This returns an envelope rather than a bare array because /runs is the ONE
 * endpoint in this gateway with real pagination, and the cursor was previously
 * being thrown away at exactly this line. `asList` normalises the rows and
 * discards everything around them, which is correct for the six endpoints that
 * send nothing around them and was quietly wrong for this one: `next_before`
 * arrived on every full page and never reached the UI, so the run list fetched
 * a capped 100 and stopped.
 *
 * `nextBefore` is undefined when there is no next page. The gateway only sets
 * the field when a FULL page came back (len(rows) == limit), so its absence is
 * the server's own statement that the list is exhausted — not an inference this
 * client draws from a short page.
 */
export interface RunPage {
  runs: RunSummary[];
  nextBefore?: string;
  total?: number;
}

/**
 * What a successful workflow delete removed.
 *
 * `versions_deleted` is carried rather than dropped because it is the only
 * remaining evidence of the size of what was destroyed — deleting a workflow
 * with nine versions and one with a single version are different acts, and
 * afterwards nothing can tell them apart.
 */
export interface SpecDeletion {
  namespace: string;
  name: string;
  versions_deleted: number;
  deleted: true;
}

/**
 * Turns a 409 `workflow_referenced` body into the sentence an operator acts on.
 *
 * The gateway names the blockers — `referencing_runs` as a count,
 * `referencing_schedules` as names — and the whole value of the refusal is
 * those two numbers: they say whether this is fixable (delete the schedule
 * first) or final (the run history is immutable and the workflow stays). A
 * message that said only "workflow is still referenced" would be a correct
 * refusal rendered useless, and in a bulk delete of ten it would be ten
 * identical useless lines.
 *
 * The schedules are NAMED, up to a few, rather than counted: "still referenced
 * by nightly-etl" is a destination, "still referenced by 1 schedule" is a
 * second search. Beyond three the list is elided, because a failure line in a
 * bounded report must stay one line.
 *
 * Falls back to the server's own message whenever the counts are absent — the
 * handler omits them when it cannot re-read the blockers, and it says so: the
 * REFUSAL is the answer, and a less detailed 409 beats turning it into a 500.
 */
function specReferencedMessage(e: ApiError): string {
  let runs: number | undefined;
  let schedules: string[] = [];
  let message = e.message;
  try {
    const parsed = JSON.parse(e.body) as {
      message?: string;
      referencing_runs?: unknown;
      referencing_schedules?: unknown;
    };
    if (typeof parsed.message === 'string' && parsed.message) message = parsed.message;
    if (typeof parsed.referencing_runs === 'number') runs = parsed.referencing_runs;
    if (Array.isArray(parsed.referencing_schedules)) {
      schedules = parsed.referencing_schedules.filter((s): s is string => typeof s === 'string');
    }
  } catch {
    /* Not JSON, or not the shape documented: the server's message stands. */
  }

  const blockers: string[] = [];
  // Only non-zero blockers are named. "0 runs" in a refusal reads as a
  // contradiction and sends the operator looking for the real reason.
  if (runs !== undefined && runs > 0) {
    blockers.push(`${runs} run${runs === 1 ? '' : 's'}`);
  }
  if (schedules.length > 0) {
    const named = schedules.slice(0, 3).join(', ');
    const rest = schedules.length - 3;
    blockers.push(
      `${schedules.length} schedule${schedules.length === 1 ? '' : 's'} (${named}${
        rest > 0 ? `, +${rest} more` : ''
      })`,
    );
  }
  if (blockers.length === 0) return message;
  return `still referenced by ${blockers.join(' and ')} — delete those first, or the workflow stays`;
}

/**
 * Triage query knobs. The three windows are Go duration strings on the wire
 * ("15m", "1h") and are passed through untouched: the gateway owns the defaults
 * and the parsing, and a console that pre-formatted them would drift from it.
 */
export interface AttentionFilter {
  /** Server default 50, server maximum 200. */
  limit?: number;
  blocked_for?: string;
  failed_within?: string;
  stalled_for?: string;
}

export const api = {
  async listRuns(namespace: string, filter: RunListFilter = {}): Promise<RunPage> {
    const q = new URLSearchParams();
    if (filter.state) q.set('state', filter.state);
    if (filter.spec) q.set('spec', filter.spec);
    if (filter.limit) q.set('limit', String(filter.limit));
    if (filter.before) q.set('before', filter.before);
    const qs = q.toString();
    const payload = await request<unknown>(`${ns(namespace)}/runs${qs ? `?${qs}` : ''}`);
    const runs = asList<RunSummary>(payload, 'runs');
    // Newest first is the operator default. Sorting client-side as well as
    // asking the server for it means an unsorted handler still reads correctly.
    const sorted = [...runs].sort(
      (a, b) => Date.parse(b.created_at ?? '') - Date.parse(a.created_at ?? ''),
    );
    /**
     * The cursor is read only off an OBJECT payload, and only as a string.
     *
     * A bare-array response (which asList above still accepts, and which the
     * client's tolerance of unfrozen paging shapes exists for) has nowhere to
     * carry one, so it correctly yields `undefined` — no next page, the list is
     * what it is. Type-guarding rather than casting because a gateway that sent
     * a number or null here would otherwise produce `before=null` in a query
     * string and a 400 the operator sees as "the run list is broken".
     */
    let nextBefore: string | undefined;
    if (payload && typeof payload === 'object' && !Array.isArray(payload)) {
      const raw = (payload as Record<string, unknown>).next_before;
      if (typeof raw === 'string' && raw !== '') nextBefore = raw;
    }
    const rawTotal = payload && typeof payload === 'object' && !Array.isArray(payload)
      ? (payload as Record<string, unknown>).total : undefined;
    const total = typeof rawTotal === 'number' && Number.isSafeInteger(rawTotal) && rawTotal >= 0 ? rawTotal : undefined;
    return { runs: sorted, nextBefore, ...(total !== undefined ? { total } : {}) };
  },

  /**
   * Create a run.
   *
   * No Idempotency-Key header. The gateway supports one, but a console form is
   * a deliberate human act rather than a retrying client: reusing a key across
   * two intentional submissions would replay the FIRST run's response and leave
   * the operator believing they launched something they did not. A client that
   * retries automatically (the SDK, the CLI) is where that header belongs.
   */
  createRun(namespace: string, body: CreateRunRequest): Promise<RunSummary> {
    return request<RunSummary>(`${ns(namespace)}/runs`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  getRun(namespace: string, runId: string): Promise<RunDetail> {
    return request<RunDetailPayload>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}`);
  },

  /**
   * Attempt history, read out of the run detail response.
   *
   * There is no `/runs/{id}/attempts` route: the gateway deliberately serves the
   * run, its nodes and its attempts from one handler so a detail view costs one
   * request instead of three. Asking for a separate collection got a 404, which
   * the query layer correctly refuses to retry — so the DAG, the node panel and
   * the log tail all rendered empty while the data was already on the wire.
   */
  async listRunAttempts(namespace: string, runId: string): Promise<Attempt[]> {
    const payload = await request<unknown>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}`);
    return asList<Attempt>(payload, 'attempts');
  },

  async listEvents(namespace: string, runId: string, afterSeq?: number): Promise<RunEvent[]> {
    // after_seq, not since_seq: the gateway ignores an unknown parameter rather
    // than rejecting it, so the wrong spelling silently replayed the whole
    // history on every poll instead of erroring where anyone would notice.
    const q = afterSeq !== undefined ? `?after_seq=${afterSeq}` : '';
    const payload = await request<unknown>(
      `${ns(namespace)}/runs/${encodeURIComponent(runId)}/events${q}`,
    );
    return asList<RunEvent>(payload, 'events');
  },

  /**
   * One run lifecycle transition.
   *
   * All four share a route shape and a server-side handler, so they share one
   * method rather than four near-copies that would drift. The server answers
   * 409 with a REASON when the transition does not apply -- pausing a run that
   * just failed, say -- and the ApiError carries that message through, so a
   * caller can tell the operator what happened instead of a bare rejection.
   */
  runAction(namespace: string, runId: string, action: RunAction): Promise<RunSummary | void> {
    return request<RunSummary | void>(
      `${ns(namespace)}/runs/${encodeURIComponent(runId)}/${action}`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: '{}',
      },
    );
  },

  /**
   * Retry a FAILED or TIMED_OUT run. With `nodes`, exactly those nodes and their
   * downstream dependents that did not succeed get a new attempt; without, every
   * node that did not succeed does. Succeeded nodes never re-run. The gateway
   * refuses the whole request (409/422, nothing retried) when a node is unknown,
   * not FAILED/UPSTREAM_FAILED/CANCELLED, or would fail again because a failed
   * upstream is not also selected -- the message names what to do instead.
   */
  retryRun(namespace: string, runId: string, nodes?: readonly string[]): Promise<RunSummary> {
    return request<RunSummary>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}/retry`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(nodes && nodes.length > 0 ? { nodes } : {}),
    });
  },

  /** Kept as its own name: it is called from a list row as well as the detail page. */
  deleteRun(namespace: string, runId: string): Promise<void> {
    return request<void>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}`, {method: "DELETE"});
  },

  cancelRun(namespace: string, runId: string): Promise<void> {
    return request<void>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}/cancel`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
  },

  /**
   * Ask a worker to stop taking new work and finish what it holds.
   *
   * admin_write, not write: draining removes capacity from the fleet, which is
   * a fleet-shaping decision rather than a run-shaping one. The server answers
   * 200 once the intent is RECORDED -- the worker drains asynchronously, so a
   * 200 here means "asked", not "drained".
   */
  drainWorker(namespace: string, workerId: string): Promise<void> {
    return request<void>(`${ns(namespace)}/workers/${encodeURIComponent(workerId)}/drain`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
  },

  /** Take a worker out of placement. durationMs 0 = until released. */
  quarantineWorker(namespace: string, workerId: string, reason: string, durationMs = 0): Promise<void> {
    return request<void>(`${ns(namespace)}/workers/${encodeURIComponent(workerId)}/quarantine`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason, duration_ms: durationMs }),
    });
  },

  releaseWorker(namespace: string, workerId: string, reason = ''): Promise<void> {
    return request<void>(`${ns(namespace)}/workers/${encodeURIComponent(workerId)}/release`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason }),
    });
  },

  async listWorkerQuarantines(namespace: string, workerId: string): Promise<WorkerQuarantine[]> {
    const payload = await request<unknown>(`${ns(namespace)}/workers/${encodeURIComponent(workerId)}/quarantines`);
    return asList<WorkerQuarantine>(payload, 'quarantines');
  },

  /** Stop reusing memoized results of a task (or one cache key). */
  invalidateCache(
    namespace: string,
    sel: { task_ref?: string; task_version?: number; cache_key?: string; reason?: string },
  ): Promise<{ invalidation_id: number; invalidated_at: string }> {
    return request(`${ns(namespace)}/cache/invalidations`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(sel),
    });
  },

  async listWorkers(namespace: string): Promise<Worker[]> {
    const payload = await request<unknown>(`${ns(namespace)}/workers`);
    return asList<Worker>(payload, 'workers');
  },

  /**
   * One worker plus the tasks attributed to it.
   *
   * Served by a single handler, the same shape as `getRun`: the worker, its
   * running attempts and its recent ones arrive together, because the question
   * this address exists for is "what is this worker doing" and the worker row
   * alone cannot answer it. The alternative the old route set left open — walk
   * every run in the namespace and filter its attempts by worker_id in the
   * browser — is an unbounded fan-out to answer what the database does with one
   * partial index.
   */
  getWorker(namespace: string, workerId: string): Promise<WorkerDetail> {
    return request<WorkerDetail>(`${ns(namespace)}/workers/${encodeURIComponent(workerId)}`);
  },

  getCapacity(namespace: string): Promise<CapacitySummary> {
    return request<CapacitySummary>(`${ns(namespace)}/capacity`);
  },

  /**
   * The triage list: everything in this namespace that wants a human.
   *
   * The response is already ranked, so no ordering option is offered here — a
   * `sort` parameter would invite a caller to override a decision the server is
   * better placed to make, and two consoles disagreeing about what is most
   * urgent is worse than either ordering alone.
   *
   * The thresholds are the server's defaults unless a caller overrides them;
   * they are passed through rather than hardcoded because "how long is stalled"
   * is a per-deployment judgement, not a console constant.
   *
   * Normalised through asList like every other collection: a gateway that grows
   * paging and answers a bare array must not blank this panel. count/truncated
   * fall back to the items in hand, so the worst case is a missing "view all"
   * rather than a crash.
   */
  async getAttention(
    namespace: string,
    opts: AttentionFilter = {},
  ): Promise<AttentionList> {
    const q = new URLSearchParams();
    if (opts.limit !== undefined) q.set('limit', String(opts.limit));
    if (opts.blocked_for) q.set('blocked_for', opts.blocked_for);
    if (opts.failed_within) q.set('failed_within', opts.failed_within);
    if (opts.stalled_for) q.set('stalled_for', opts.stalled_for);
    const qs = q.toString();
    const payload = await request<unknown>(`${ns(namespace)}/attention${qs ? `?${qs}` : ''}`);
    const items = asList<AttentionList['items'][number]>(payload, 'attention');
    const obj = (payload ?? {}) as Partial<AttentionList>;
    return {
      items,
      count: typeof obj.count === 'number' ? obj.count : items.length,
      truncated: obj.truncated === true,
    };
  },

  /**
   * Fleet connectivity. NOT namespaced: there is one control plane behind every
   * namespace, so scoping this per namespace would imply a per-tenant fleet that
   * does not exist and would refetch the same answer once per namespace switch.
   */
  async getFleetHealth(): Promise<FleetHealth> {
    const payload = await request<unknown>('/v1/health/fleet');
    const obj = (payload ?? {}) as Partial<FleetHealth>;
    return {
      // Never recomputed from the rows: see FleetHealth. An absent status is
      // reported as unknown rather than optimistically as ok.
      status: typeof obj.status === 'string' ? obj.status : 'unknown',
      upstreams: asList<FleetHealth['upstreams'][number]>(payload, 'upstreams'),
      measures: obj.measures,
    };
  },

  async listSchedules(namespace: string, limit = 100): Promise<Schedule[]> {
    const payload = await request<unknown>(`${ns(namespace)}/schedules?limit=${limit}`);
    const schedules = asList<Schedule>(payload, 'schedules');
    // Name order, not creation order. A schedule list is something an operator
    // scans for a name they already have in mind, and the gateway does not
    // promise an order -- so the console picks the one that is scannable.
    return [...schedules].sort((a, b) => a.name.localeCompare(b.name));
  },

  getSchedule(namespace: string, name: string): Promise<Schedule> {
    return request<Schedule>(`${ns(namespace)}/schedules/${encodeURIComponent(name)}`);
  },

  createSchedule(namespace: string, body: CreateScheduleRequest): Promise<Schedule> {
    return request<Schedule>(`${ns(namespace)}/schedules`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  updateSchedule(namespace: string, body: CreateScheduleRequest): Promise<Schedule> {
    return request<Schedule>(`${ns(namespace)}/schedules/${encodeURIComponent(body.name)}`, {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
  },

  deleteSchedule(namespace: string, name: string): Promise<void> {
    return request<void>(`${ns(namespace)}/schedules/${encodeURIComponent(name)}`, {
      method: 'DELETE',
    });
  },

  setScheduleEnabled(namespace: string, name: string, enabled: boolean): Promise<Schedule> {
    const action = enabled ? 'enable' : 'disable';
    return request<Schedule>(`${ns(namespace)}/schedules/${encodeURIComponent(name)}/${action}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
  },

  /**
   * Evaluate a cron expression without creating anything.
   *
   * The endpoint answers 200 with `valid:false` for a bad expression rather than
   * 400, because "that expression is wrong" IS the answer to the question asked.
   * So a rejected expression must not be surfaced as a request failure here
   * either -- the caller reads `valid`, and only a transport or auth problem
   * throws.
   */
  previewCron(
    namespace: string,
    cron: string,
    timezone: string,
    count = 5,
  ): Promise<CronPreview> {
    const q = new URLSearchParams({ cron, count: String(count) });
    // Sent only when set: an empty timezone parameter and an absent one both mean
    // UTC to the gateway, and omitting it keeps the URL honest about what was asked.
    if (timezone) q.set('timezone', timezone);
    return request<CronPreview>(`${ns(namespace)}/schedules/preview?${q.toString()}`);
  },

  async listBackfills(namespace: string, scheduleName: string, limit = 50): Promise<Backfill[]> {
    const payload = await request<unknown>(
      `${ns(namespace)}/schedules/${encodeURIComponent(scheduleName)}/backfills?limit=${limit}`,
    );
    const backfills = asList<Backfill>(payload, 'backfills');
    return [...backfills].sort(
      (a, b) => Date.parse(b.created_at ?? '') - Date.parse(a.created_at ?? ''),
    );
  },

  getBackfill(namespace: string, id: string): Promise<Backfill> {
    return request<Backfill>(`${ns(namespace)}/backfills/${encodeURIComponent(id)}`);
  },

  startBackfill(
    namespace: string,
    scheduleName: string,
    body: StartBackfillRequest,
  ): Promise<Backfill> {
    return request<Backfill>(
      `${ns(namespace)}/schedules/${encodeURIComponent(scheduleName)}/backfills`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      },
    );
  },

  /**
   * pause / resume / cancel, which share a shape on the wire.
   *
   * Addressed by backfill ID rather than through its schedule: a backfill
   * outlives the interval that named it, and the gateway routes these three the
   * same way for the same reason.
   */
  backfillAction(
    namespace: string,
    id: string,
    action: 'pause' | 'resume' | 'cancel',
  ): Promise<Backfill> {
    return request<Backfill>(`${ns(namespace)}/backfills/${encodeURIComponent(id)}/${action}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
  },

  // ── workflow catalog ──────────────────────────────────────────────────────

  /**
   * One row per workflow NAME, newest version summarised.
   *
   * `search` is applied SERVER-side rather than by filtering a full list here:
   * the gateway matches the name case-insensitively against the whole table,
   * so a namespace with more workflows than one page still finds the match. An
   * empty search is sent as no parameter at all, which the gateway reads as
   * "everything".
   */
  async listWorkflows(
    namespace: string,
    search = '',
    limit = 200,
  ): Promise<WorkflowSummary[]> {
    const q = new URLSearchParams({ limit: String(limit) });
    const term = search.trim();
    if (term) q.set('search', term);
    const payload = await request<unknown>(`${ns(namespace)}/specs?${q.toString()}`);
    const rows = asList<WorkflowSummary>(payload, 'workflows');
    // Name order. A catalog is scanned for a name already in mind, and the
    // gateway does not promise an order -- same reasoning as the schedule list.
    return [...rows].sort((a, b) => a.name.localeCompare(b.name));
  },

  /**
   * Delete a workflow and every version of it.
   *
   * admin_write, and 200-with-a-count rather than 204: `versions_deleted` is
   * the only remaining evidence of how much was destroyed, and the gateway
   * declines to throw it away. This client returns it for the same reason.
   *
   * ── THE 409 IS THE INTERESTING CASE, AND IT IS WHY THIS METHOD IS NOT
   *    THREE LINES LIKE deleteSchedule ──────────────────────────────────────
   *
   * A workflow with run history or a live schedule pointing at it CANNOT be
   * deleted, and the gateway refuses with `workflow_referenced` plus the
   * blockers: `referencing_runs` (a count) and `referencing_schedules` (names).
   * That refusal is not a failure of the platform — it is the answer — and the
   * operator's next action depends entirely on WHICH blocker it is: a schedule
   * can be deleted first, run history cannot and means the workflow stays.
   *
   * The generic error path would lose that. `serverMessage` lifts only
   * `message` out of an error body, so a bulk delete of ten workflows where
   * three are referenced would report three identical sentences with no way to
   * tell which one is blocked by what — and the operator would go back to the
   * table to work it out, which is the report that gets ignored. So the
   * blockers are folded INTO the message here, at the one place that knows the
   * shape of this particular body, and everything downstream (runBulk's
   * per-row `error`, the bulk bar's failure list, an ErrorBox on a detail page)
   * renders an operator-readable reason with no special casing of its own.
   */
  async deleteSpec(namespace: string, name: string): Promise<SpecDeletion> {
    try {
      const payload = await request<Partial<SpecDeletion>>(
        `${ns(namespace)}/specs/${encodeURIComponent(name)}`,
        { method: 'DELETE' },
      );
      return {
        namespace,
        name,
        // Defaulted rather than trusted: a gateway that answered 204, or one
        // predating the count, must not surface "undefined versions deleted".
        versions_deleted: typeof payload?.versions_deleted === 'number'
          ? payload.versions_deleted
          : 0,
        deleted: true,
      };
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        throw new ApiError(specReferencedMessage(e), {
          status: e.status,
          url: e.url,
          body: e.body,
        });
      }
      throw e;
    }
  },

  /** Version history for one workflow name, newest first. */
  async listSpecVersions(
    namespace: string,
    name: string,
    limit = 100,
  ): Promise<SpecVersion[]> {
    const payload = await request<unknown>(
      `${ns(namespace)}/specs/${encodeURIComponent(name)}/versions?limit=${limit}`,
    );
    const rows = asList<SpecVersion>(payload, 'versions');
    // Sorted here as well as asked for, so the "latest" marker on this page is
    // never decided by response order.
    return [...rows].sort((a, b) => b.version - a.version);
  },

  /**
   * The definition, addressed by spec ID rather than name+version.
   *
   * `specdefs`, not `specs/{id}`: net/http rejects the latter as ambiguous with
   * `specs/{name}/versions`, since a workflow could legitimately be named "id".
   */
  getSpecDefinition(namespace: string, specId: string): Promise<SpecDefinition> {
    return request<SpecDefinition>(`${ns(namespace)}/specdefs/${encodeURIComponent(specId)}`);
  },

  /**
   * Non-streaming log fetch, used as the polling fallback and for the initial
   * backfill before the stream attaches.
   */
  async getLogs(namespace: string, attemptId: string, tail = 500): Promise<LogLine[]> {
    const url = `${ns(namespace)}/attempts/${encodeURIComponent(attemptId)}/logs?tail=${tail}&follow=false`;
    const fullUrl = apiUrl(url);
    let res: Response;
    try {
      res = await fetch(fullUrl, { headers: { ...authHeaders() } });
    } catch (cause) {
      throw new ApiError(`Cannot reach the log endpoint at ${fullUrl}.`, {
        status: 0,
        url: fullUrl,
        body: String(cause),
      });
    }
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      throw new ApiError(serverMessage(res.status, body), { status: res.status, url: fullUrl, body });
    }
    const text = await res.text();
    return parseLogPayload(text);
  },
};

/**
 * Logs may arrive as JSON, as NDJSON, or as plain text. Accepting all three
 * means the log tail works before the gateway's log format is finalised, which
 * it is not in Phase 1.
 */
export function parseLogPayload(text: string): LogLine[] {
  const trimmed = text.trim();
  if (trimmed === '') return [];
  if (trimmed.startsWith('[') || trimmed.startsWith('{')) {
    try {
      const parsed = JSON.parse(trimmed) as unknown;
      const arr = asList<LogLine | string>(parsed, 'lines', 'logs');
      if (arr.length > 0) {
        return arr.map((l) => (typeof l === 'string' ? { message: l } : l));
      }
    } catch {
      /* NDJSON: fall through to the line-by-line path */
    }
  }
  return trimmed.split('\n').map((line) => {
    const s = line.trim();
    if (s.startsWith('{')) {
      try {
        const o = JSON.parse(s) as LogLine;
        if (typeof o.message === 'string') return o;
      } catch {
        /* not JSON; treat as raw text */
      }
    }
    return { message: line };
  });
}

/**
 * SSE URL for the live event tail (§8.2: the events endpoint serves both).
 *
 * The mode is negotiated in the query string because EventSource cannot set an
 * Accept header; without `stream=true` the gateway answers with the paged JSON
 * history, which EventSource treats as a protocol error and retries forever.
 */
export function eventStreamUrl(namespace: string, runId: string, afterSeq?: number): string {
  const q = new URLSearchParams({ stream: 'true' });
  if (afterSeq !== undefined) q.set('after_seq', String(afterSeq));
  return apiUrl(`${ns(namespace)}/runs/${encodeURIComponent(runId)}/events?${q.toString()}`);
}

export function logStreamUrl(namespace: string, attemptId: string): string {
  return apiUrl(`${ns(namespace)}/attempts/${encodeURIComponent(attemptId)}/logs?follow=true`);
}
