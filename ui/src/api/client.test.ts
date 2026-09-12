/**
 * Tests for the REST client's wire contract.
 *
 * These exist because two bugs of exactly the same shape shipped undetected: a
 * request to a route the gateway does not serve, and a cursor parameter the
 * gateway does not read. Neither throws, neither logs, and both look like "the
 * backend has no data yet" from inside the UI — the failure mode this whole
 * client was written to avoid. Types cannot catch them, because both sides
 * type-check fine while disagreeing about the string in the URL. So the URLs the
 * client builds are asserted directly, against a mocked fetch.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ApiError, api, eventStreamUrl } from './client';

/** Records every URL requested, so a test can assert on what was NOT called. */
let urls: string[];

function mockFetch(body: unknown, init: { status?: number } = {}) {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    urls.push(String(input));
    const status = init.status ?? 200;
    return Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      text: () => Promise.resolve(typeof body === 'string' ? body : JSON.stringify(body)),
    } as Response);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

beforeEach(() => {
  urls = [];
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('listRunAttempts', () => {
  // The regression guard for bug 1. `/runs/{id}/attempts` is not registered on
  // the gateway, so requesting it 404s and the query layer (correctly) does not
  // retry a 4xx — leaving the DAG, the node panel and the log tail permanently
  // empty. Asserting the absence of that path is the point of this test.
  it('never requests a separate attempts collection', async () => {
    mockFetch({ id: 'r1', nodes: [], edges: [], attempts: [] });
    await api.listRunAttempts('default', 'r1');
    expect(urls).toHaveLength(1);
    expect(urls.some((u) => u.includes('/attempts'))).toBe(false);
  });

  it('reads attempts out of the run detail response', async () => {
    mockFetch({
      id: 'r1',
      nodes: [{ node_id: 'a', state: 'FAILED' }],
      attempts: [
        { id: 'a1', node_id: 'a', attempt_number: 1, state: 'FAILED' },
        { id: 'a2', node_id: 'a', attempt_number: 2, state: 'SUCCEEDED' },
      ],
    });
    const attempts = await api.listRunAttempts('default', 'r1');
    expect(urls[0]).toBe('/v1/namespaces/default/runs/r1');
    expect(attempts.map((a) => a.id)).toEqual(['a1', 'a2']);
  });

  // A run with no attempts yet is the normal state of a freshly submitted run,
  // not an error, so it must not surface as one.
  it('yields an empty list when the run has no attempts yet', async () => {
    mockFetch({ id: 'r1', nodes: [], edges: [] });
    await expect(api.listRunAttempts('default', 'r1')).resolves.toEqual([]);
  });

  it('percent-encodes the run id so a crafted id cannot escape the path', async () => {
    mockFetch({ attempts: [] });
    await api.listRunAttempts('default', 'r1/../../secret');
    expect(urls[0]).toBe('/v1/namespaces/default/runs/r1%2F..%2F..%2Fsecret');
  });
});

describe('listEvents', () => {
  // The regression guard for bug 2. The gateway reads `after_seq` and ignores
  // unknown parameters silently, so `since_seq` meant every poll replayed the
  // entire history while appearing to work.
  it('sends the cursor as after_seq', async () => {
    mockFetch({ events: [] });
    await api.listEvents('default', 'r1', 99);
    expect(urls[0]).toContain('after_seq=99');
    expect(urls[0]).not.toContain('since_seq');
  });

  it('omits the cursor entirely when none is given', async () => {
    mockFetch({ events: [] });
    await api.listEvents('default', 'r1');
    expect(urls[0]).toBe('/v1/namespaces/default/runs/r1/events');
  });

  // Seq 0 is a legitimate cursor meaning "everything". Sending nothing for it
  // would work by accident today and break the moment the default changes.
  it('sends a zero cursor rather than treating it as absent', async () => {
    mockFetch({ events: [] });
    await api.listEvents('default', 'r1', 0);
    expect(urls[0]).toContain('after_seq=0');
  });
});

describe('eventStreamUrl', () => {
  // EventSource cannot set an Accept header, so SSE has to be negotiated in the
  // query string. Without this the gateway serves paged JSON and EventSource
  // reconnects forever, which the UI reports as "polling" — silently degraded.
  it('negotiates SSE with stream=true', () => {
    const url = eventStreamUrl('default', 'r1');
    expect(url).toContain('stream=true');
  });

  // `follow=true` was the old spelling and the gateway never read it. Asserting
  // its absence stops it from being reintroduced as a "harmless" extra.
  it('does not rely on the parameter the gateway ignores', () => {
    expect(eventStreamUrl('default', 'r1')).not.toContain('follow');
  });

  it('carries a resume cursor as after_seq', () => {
    const url = eventStreamUrl('default', 'r1', 42);
    expect(url).toContain('after_seq=42');
    expect(url).not.toContain('since_seq');
  });
});

describe('error translation', () => {
  it('reports an unreachable gateway distinctly from a server failure', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))),
    );
    const err = await api.listRuns('default').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).isUnreachable).toBe(true);
  });

  // A 404 on a read is the "not built yet" signal the whole client is built
  // around; it must stay distinguishable from a 500 so retry policy can differ.
  it('flags a 404 as not-implemented rather than a transport failure', async () => {
    mockFetch('', { status: 404 });
    const err = await api.listRuns('default').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).isNotImplemented).toBe(true);
    expect((err as ApiError).isUnreachable).toBe(false);
  });

  it('prefers a server-supplied message over the generic status text', async () => {
    mockFetch(JSON.stringify({ message: 'run not found' }), { status: 404 });
    const err = await api.listRuns('default').catch((e: unknown) => e);
    expect((err as ApiError).message).toContain('run not found');
  });
});

describe('listRuns', () => {
  // Tolerating a bare array, `{items}` and `{runs}` is deliberate (the paging
  // shape is not frozen), so all three must keep working.
  it('accepts every envelope shape the gateway might use', async () => {
    for (const body of [
      [{ id: 'r1', created_at: '2026-01-01T00:00:00Z' }],
      { items: [{ id: 'r1', created_at: '2026-01-01T00:00:00Z' }] },
      { runs: [{ id: 'r1', created_at: '2026-01-01T00:00:00Z' }] },
    ]) {
      mockFetch(body);
      const runs = await api.listRuns('default');
      expect(runs.map((r) => r.id)).toEqual(['r1']);
    }
  });

  it('sorts newest first so the operator default does not depend on the handler', async () => {
    mockFetch({
      runs: [
        { id: 'old', created_at: '2026-01-01T00:00:00Z' },
        { id: 'new', created_at: '2026-06-01T00:00:00Z' },
      ],
    });
    const runs = await api.listRuns('default');
    expect(runs.map((r) => r.id)).toEqual(['new', 'old']);
  });
});
