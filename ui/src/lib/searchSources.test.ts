// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The wiring between the search policy and the five real endpoints.
 *
 * commands.test.ts pins what the fan-out DOES with five answers; this file pins
 * that the five questions are the right ones, which is a different kind of bug
 * and the one a fetch mock is actually good at catching. Every assertion here
 * is about the URL that went out or the shape that came back, because the ways
 * this layer fails are: asking the wrong endpoint, dropping a parameter the
 * endpoint needs, and -- the one with teeth -- quietly claiming a complete
 * search over a capped fetch.
 */

import { afterEach, describe, expect, it, vi } from 'vitest';
import { CLIENT_FILTER_CAP } from './commands';
import { searchSources } from './searchSources';

/** Routes by URL, and records every URL asked for. */
function mockFetch(handler: (url: string) => [number, unknown]) {
  const urls: string[] = [];
  vi.stubGlobal('fetch', (url: string) => {
    urls.push(String(url));
    const [status, body] = handler(String(url));
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return urls;
}

function workflow(name: string, id = name) {
  return {
    id,
    namespace: 'default',
    name,
    latest_version: 3,
    spec_hash: 'h',
    created_by: 'admin',
    updated_at: '2026-09-12T09:00:00Z',
    version_count: 3,
    first_created_at: '2026-09-01T09:00:00Z',
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('workflows', () => {
  /** The only group with a real server-side search; it must actually use it. */
  it('pushes the query to the catalog rather than filtering here', async () => {
    const urls = mockFetch(() => [200, { workflows: [workflow('etl-nightly')] }]);
    const hits = await searchSources.workflows('default', 'etl');

    expect(urls[0]).toContain('/specs?');
    expect(urls[0]).toContain('search=etl');
    expect(hits).toEqual([
      { id: 'workflow.etl-nightly', label: 'etl-nightly', hint: 'v3', path: '/workflows/etl-nightly' },
    ]);
  });
});

describe('runs', () => {
  /**
   * The runs endpoint has no free-text search -- it filters by SPEC NAME -- so
   * this group is a two-step, and the test that matters is that the second step
   * really carries the spec through rather than fetching the whole run table
   * and hoping.
   */
  it('resolves the query to spec names and asks for those specs’ runs', async () => {
    const urls = mockFetch((url) => {
      if (url.includes('/specs')) return [200, { workflows: [workflow('etl-nightly')] }];
      return [
        200,
        {
          runs: [
            {
              id: 'run-1',
              namespace: 'default',
              spec_id: 'etl-nightly',
              spec_name: 'etl-nightly',
              state: 'RUNNING',
              created_at: '2026-09-13T00:00:00Z',
            },
          ],
        },
      ];
    });

    const hits = await searchSources.runs('default', 'etl');

    expect(urls.some((u) => u.includes('/runs?') && u.includes('spec=etl-nightly'))).toBe(true);
    expect(hits[0]).toMatchObject({
      id: 'run.run-1',
      label: 'etl-nightly · RUNNING',
      path: '/runs/run-1',
    });
  });

  /** No spec matched means no runs to ask for: zero extra requests, not a blind list. */
  it('asks for no runs at all when nothing matched the catalog', async () => {
    const urls = mockFetch(() => [200, { workflows: [] }]);
    expect(await searchSources.runs('default', 'zzz')).toEqual([]);
    expect(urls.some((u) => u.includes('/runs'))).toBe(false);
  });

  /** A dead runs endpoint is a FAILED group, not an empty one. */
  it('rejects when every spec’s runs fetch failed', async () => {
    mockFetch((url) => {
      if (url.includes('/specs')) return [200, { workflows: [workflow('etl-nightly')] }];
      return [500, { message: 'runs table is down' }];
    });
    await expect(searchSources.runs('default', 'etl')).rejects.toThrow(/runs table is down/);
  });
});

describe('schedules', () => {
  it('matches the schedule name and the spec it fires', async () => {
    mockFetch(() => [
      200,
      {
        schedules: [
          {
            id: 's1',
            namespace: 'default',
            name: 'nightly-2am',
            spec_name: 'etl-nightly',
            cron: '0 2 * * *',
            timezone: 'UTC',
            priority: 0,
            overlap_policy: 'SKIP',
            catchup_limit: 0,
            enabled: true,
            next_fire_at: null,
            last_fire_at: null,
            created_at: '2026-09-01T00:00:00Z',
            updated_at: '2026-09-01T00:00:00Z',
          },
        ],
      },
    ]);

    // By its own name...
    expect((await searchSources.schedules('default', 'nightly-2')).hits).toHaveLength(1);
    // ...and by the workflow it fires, which is as common a question.
    const bySpec = await searchSources.schedules('default', 'etl');
    expect(bySpec.hits[0]).toMatchObject({ label: 'nightly-2am', path: '/schedules/nightly-2am' });
  });

  /**
   * The honesty requirement. The endpoint has no search parameter, so the match
   * happened over a capped page -- and a match past the cap was never visible.
   * Claiming completeness here is the same class of lie as an empty list for a
   * failed request.
   */
  it('admits the fetch was capped rather than implying it saw every schedule', async () => {
    const many = Array.from({ length: CLIENT_FILTER_CAP }, (_, i) => ({
      id: `s${i}`,
      namespace: 'default',
      name: `nightly-${i}`,
      spec_name: 'etl',
      cron: '0 2 * * *',
      timezone: 'UTC',
      priority: 0,
      overlap_policy: 'SKIP',
      catchup_limit: 0,
      enabled: true,
      next_fire_at: null,
      last_fire_at: null,
      created_at: '2026-09-01T00:00:00Z',
      updated_at: '2026-09-01T00:00:00Z',
    }));
    const urls = mockFetch(() => [200, { schedules: many }]);

    const res = await searchSources.schedules('default', 'nightly');
    expect(urls[0]).toContain(`limit=${CLIENT_FILTER_CAP}`);
    expect(res.truncated).toBe(true);
  });
});

describe('workers', () => {
  /**
   * The incident case this group exists for: the alert names a HOST and the
   * operator has nothing else. Queue names match too, because "nothing on the
   * gpu queue is moving" is the other way the question arrives.
   */
  it('finds a worker by hostname, by id and by queue', async () => {
    mockFetch(() => [
      200,
      {
        workers: [
          {
            id: 'wk-7',
            namespace: 'default',
            hostname: 'box-7.dc1',
            queues: ['gpu', 'default'],
            capacity: {},
            state: 'ACTIVE',
            last_heartbeat_at: '2026-09-13T00:00:00Z',
          },
        ],
      },
    ]);

    expect((await searchSources.workers('default', 'box-7')).hits).toHaveLength(1);
    expect((await searchSources.workers('default', 'wk-7')).hits).toHaveLength(1);
    expect((await searchSources.workers('default', 'gpu')).hits[0]).toMatchObject({
      label: 'box-7.dc1',
      path: '/workers/wk-7',
    });
  });

  /**
   * The endpoint takes no parameters at all, so there is no cap to have hit and
   * "not truncated" is a real claim rather than an optimistic one. Worth an
   * assertion because the easy mistake is to copy the schedules branch and
   * report a truncation that cannot happen.
   */
  it('never claims truncation, because the fleet endpoint has no cap', async () => {
    mockFetch(() => [200, { workers: [] }]);
    expect((await searchSources.workers('default', 'x')).truncated).toBe(false);
  });
});

describe('users', () => {
  it('matches on display name and on email, since either is how a colleague is known', async () => {
    const urls = mockFetch(() => [
      200,
      {
        users: [
          {
            id: 'u1',
            namespace: 'default',
            email: 'ada@example.com',
            display_name: 'Ada Lovelace',
            role: 'admin',
            created_at: '2026-01-01T00:00:00Z',
            disabled_at: null,
            disabled: false,
          },
        ],
      },
    ]);

    expect((await searchSources.users('default', 'lovelace')).hits).toHaveLength(1);
    const byEmail = await searchSources.users('default', 'ada@example');
    expect(byEmail.hits[0]).toMatchObject({ label: 'Ada Lovelace', hint: 'admin' });
    // No search parameter exists on this endpoint, so a cap is the only knob --
    // and it is SENT, because a caller that does not choose the number cannot
    // know whether it was reached.
    expect(urls[0]).toContain(`limit=${CLIENT_FILTER_CAP}`);
  });

  it('says so when a user is disabled, because it changes what the operator does next', async () => {
    mockFetch(() => [
      200,
      {
        users: [
          {
            id: 'u1',
            namespace: 'default',
            email: 'bob@example.com',
            display_name: 'Bob',
            role: 'operator',
            created_at: '2026-01-01T00:00:00Z',
            disabled_at: '2026-06-01T00:00:00Z',
            disabled: true,
          },
        ],
      },
    ]);
    expect((await searchSources.users('default', 'bob')).hits[0]?.hint).toBe('operator · disabled');
  });

  /**
   * The admin gate, surfaced as a REJECTION with its status intact -- which is
   * the whole contract between this file and classifyGroupError. If the status
   * were swallowed here the users group would render as a red "search failed"
   * for every non-admin on every keystroke, instead of a calm note about
   * permission.
   */
  it('propagates a 403 with its status, so the fan-out can call it a refusal', async () => {
    mockFetch(() => [403, { message: 'admin only' }]);
    await expect(searchSources.users('default', 'ada')).rejects.toMatchObject({ status: 403 });
  });
});
