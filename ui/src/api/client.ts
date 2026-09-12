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
  CapacitySummary,
  LogLine,
  RunDetail,
  RunEvent,
  RunSummary,
  Worker,
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

export function setApiKey(key: string): void {
  const clean = key.trim();
  try {
    if (clean) window.localStorage.setItem(API_KEY_STORAGE, clean);
    else window.localStorage.removeItem(API_KEY_STORAGE);
  } catch {
    // Non-fatal: the key simply will not persist across reloads.
  }
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

function authHeaders(): Record<string, string> {
  const key = getApiKey();
  return key ? { Authorization: `Bearer ${key}` } : {};
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
    const key = getApiKey();
    if (!key) {
      return `Not authorised (HTTP ${status}). No API key is set — use “Set API key” in the header.`;
    }
    // 403 means the key was RECOGNISED but lacks the role, which is a different
    // fix from 401 (key not recognised at all). Saying "check your key" for a
    // 403 sends the operator to re-paste a key that was never the problem.
    if (status === 403) {
      return `Forbidden (HTTP 403). This key is valid but lacks permission — a worker key cannot read this; use an admin key.`;
    }
    return `Not authorised (HTTP 401). The API key in this browser was refused (${key.length} chars). Use “API key” in the header to replace it.`;
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

export interface RunListFilter {
  state?: string;
  spec?: string;
  limit?: number;
}

export const api = {
  async listRuns(namespace: string, filter: RunListFilter = {}): Promise<RunSummary[]> {
    const q = new URLSearchParams();
    if (filter.state) q.set('state', filter.state);
    if (filter.spec) q.set('spec', filter.spec);
    if (filter.limit) q.set('limit', String(filter.limit));
    const qs = q.toString();
    const payload = await request<unknown>(`${ns(namespace)}/runs${qs ? `?${qs}` : ''}`);
    const runs = asList<RunSummary>(payload, 'runs');
    // Newest first is the operator default. Sorting client-side as well as
    // asking the server for it means an unsorted handler still reads correctly.
    return [...runs].sort((a, b) => Date.parse(b.created_at ?? '') - Date.parse(a.created_at ?? ''));
  },

  getRun(namespace: string, runId: string): Promise<RunDetail> {
    return request<RunDetail>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}`);
  },

  async listRunAttempts(namespace: string, runId: string): Promise<Attempt[]> {
    const payload = await request<unknown>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}/attempts`);
    return asList<Attempt>(payload, 'attempts');
  },

  async listEvents(namespace: string, runId: string, sinceSeq?: number): Promise<RunEvent[]> {
    const q = sinceSeq !== undefined ? `?since_seq=${sinceSeq}` : '';
    const payload = await request<unknown>(
      `${ns(namespace)}/runs/${encodeURIComponent(runId)}/events${q}`,
    );
    return asList<RunEvent>(payload, 'events');
  },

  /** The only mutation in Phase 1 (architecture §11). */
  cancelRun(namespace: string, runId: string): Promise<void> {
    return request<void>(`${ns(namespace)}/runs/${encodeURIComponent(runId)}/cancel`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
  },

  async listWorkers(namespace: string): Promise<Worker[]> {
    const payload = await request<unknown>(`${ns(namespace)}/workers`);
    return asList<Worker>(payload, 'workers');
  },

  getCapacity(namespace: string): Promise<CapacitySummary> {
    return request<CapacitySummary>(`${ns(namespace)}/capacity`);
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

/** SSE URL for the live event tail (§8.2: the events endpoint serves both). */
export function eventStreamUrl(namespace: string, runId: string, sinceSeq?: number): string {
  const q = new URLSearchParams({ follow: 'true' });
  if (sinceSeq !== undefined) q.set('since_seq', String(sinceSeq));
  return apiUrl(`${ns(namespace)}/runs/${encodeURIComponent(runId)}/events?${q.toString()}`);
}

export function logStreamUrl(namespace: string, attemptId: string): string {
  return apiUrl(`${ns(namespace)}/attempts/${encodeURIComponent(attemptId)}/logs?follow=true`);
}
