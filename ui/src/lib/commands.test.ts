// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The registry is data, so it is testable without rendering anything — which is
 * the point of keeping it out of the component. What is pinned here is the
 * behaviour an operator depends on: that every advertised destination exists,
 * that a visible word outranks an invisible keyword, and that a pasted run id
 * is recognised in either of the two forms it arrives in.
 */

import { describe, expect, it, vi } from 'vitest';
import {
  COMMANDS,
  PER_GROUP,
  SEARCH_GROUPS,
  asRunId,
  classifyGroupError,
  filterCommands,
  matchesText,
  runPath,
  schedulePath,
  searchEverything,
  shouldSearchRemote,
  subsequenceScore,
  workerPath,
  workflowPath,
  type CappedHits,
  type CommandContext,
  type SearchHit,
  type SearchSources,
} from './commands';

function ctx(): CommandContext & { navigate: ReturnType<typeof vi.fn> } {
  return { navigate: vi.fn() };
}

describe('the command registry', () => {
  it('offers every destination the brief names', () => {
    const routes = COMMANDS.filter((c) => c.group === 'Go to').map((c) => c.hint);
    expect(routes).toEqual([
      '/',
      '/runs',
      '/workflows',
      '/schedules',
      '/triggers',
      '/sensors',
      '/notifications',
      '/assets',
      '/workers',
      '/admin/users',
      '/admin/tokens',
      '/admin/audit',
    ]);
  });

  it('gives every command a unique id, since ids key the option elements', () => {
    const ids = COMMANDS.map((c) => c.id);
    expect(new Set(ids).size).toBe(ids.length);
  });

  it('navigates through the injected context rather than a router of its own', () => {
    const c = ctx();
    COMMANDS.find((x) => x.id === 'go.runs')!.run(c);
    expect(c.navigate).toHaveBeenCalledWith('/runs');
  });

  /**
   * The palette is the navbar's search, so it offers destinations and nothing
   * else. Theme, accent and sign-out were removed from it: they turned the
   * search field into a settings menu. Pinned as an absence so they do not creep
   * back -- appearance lives on the Settings page, sign-out in the account menu.
   */
  it('offers only destinations: no appearance or account commands', () => {
    expect(COMMANDS.every((c) => c.group === 'Go to')).toBe(true);
    const ids = COMMANDS.map((c) => c.id);
    expect(ids.some((id) => /^(theme|accent|account)\./.test(id))).toBe(false);
  });

  /**
   * The palette used to carry "Navigation: Sidebar" / "Navigation: Top bar".
   *
   * They are gone with the layout preference itself: the bar and the rail render
   * together now, so both commands would have run a setter that changed nothing.
   * Pinned as an absence rather than just deleted, because the palette is
   * exactly where a removed preference tends to survive — nobody looks at the
   * registry when they delete a settings control.
   */
  it('offers no navigation-layout command, since the shell has one layout', () => {
    expect(COMMANDS.filter((c) => c.id.startsWith('nav.'))).toEqual([]);
    expect(COMMANDS.map((c) => c.title).filter((t) => t.startsWith('Navigation:'))).toEqual([]);
  });
});

describe('matching', () => {
  it('matches a scattered subsequence, not just a prefix', () => {
    expect(subsequenceScore('Sign out', 'sgo')).not.toBeNull();
    expect(subsequenceScore('Sign out', 'zzz')).toBeNull();
  });

  it('ranks a direct substring ahead of a scattered match', () => {
    const direct = subsequenceScore('Workers', 'work')!;
    const scattered = subsequenceScore('Workflows', 'wrk')!;
    expect(direct).toBeLessThan(scattered);
  });

  it('returns the whole registry in declared order for an empty query', () => {
    expect(filterCommands('').map((h) => h.command.id)).toEqual(COMMANDS.map((c) => c.id));
  });

  it('narrows to the matching commands', () => {
    const ids = filterCommands('sched').map((h) => h.command.id);
    expect(ids).toContain('go.schedules');
    expect(ids).not.toContain('go.workers');
  });

  /**
   * Matching on a word the operator can SEE must beat matching on a hidden
   * synonym, or the top row is unexplainable from the screen.
   */
  it('puts a title match above a keyword-only match', () => {
    const ids = filterCommands('runs').map((h) => h.command.id);
    expect(ids[0]).toBe('go.runs');
  });

  it('finds a command by a keyword that is not in its title', () => {
    const ids = filterCommands('cron').map((h) => h.command.id);
    expect(ids).toContain('go.schedules');
  });
});

describe('search classification', () => {
  it('recognises a dashed run id', () => {
    const id = '3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31';
    expect(asRunId(id)).toBe(id);
  });

  it('recognises a bare 32-hex run id and restores its dashes', () => {
    expect(asRunId('3f4a1c2e9b7d4a6f8c1e2d5b7a9f0c31')).toBe(
      '3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31',
    );
  });

  it('does not mistake a workflow name for a run id', () => {
    expect(asRunId('ten-node-mixed')).toBeNull();
    expect(asRunId('')).toBeNull();
  });

  /** A one-character query matches most of a namespace: all cost, no answer. */
  it('withholds the remote search until the query says something', () => {
    expect(shouldSearchRemote('e')).toBe(false);
    expect(shouldSearchRemote('  ')).toBe(false);
    expect(shouldSearchRemote('etl')).toBe(true);
    // A run id is answered locally, so it never costs a catalog request.
    expect(shouldSearchRemote('3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31')).toBe(false);
  });

  it('builds routes that survive a name needing escaping', () => {
    expect(workflowPath('a/b')).toBe('/workflows/a%2Fb');
    expect(runPath('3f4a1c2e')).toBe('/runs/3f4a1c2e');
  });
});

/**
 * The global search POLICY, tested with no DOM.
 *
 * Kept here rather than only in CommandPalette.test.tsx because none of these
 * facts are about rendering: which sources are asked, whether they are asked at
 * once, and what a 403 MEANS are decisions about the backend. Testing them
 * through a rendered dialog would make every one of them depend on a debounce
 * and a React state flush, which is how a policy test slowly turns into a
 * flaky timing test.
 */

function hit(id: string): SearchHit {
  return { id, label: id, path: `/${id}` };
}

function sources(over: Partial<SearchSources> = {}): SearchSources {
  const none = async (): Promise<SearchHit[]> => [];
  const noneCapped = async (): Promise<CappedHits> => ({ hits: [], truncated: false });
  return {
    workflows: none,
    runs: none,
    schedules: noneCapped,
    workers: noneCapped,
    users: noneCapped,
    ...over,
  };
}

function reject(status: number, message = 'boom') {
  return () => Promise.reject(Object.assign(new Error(message), { status }));
}

describe('search group classification', () => {
  /**
   * The distinction the whole feature turns on. A 403 is the console working:
   * the credential is valid and is being refused. Painting that red would teach
   * operators to ignore the colour that means something is actually broken.
   */
  it('separates a refusal from a breakage', () => {
    expect(classifyGroupError(Object.assign(new Error('x'), { status: 403 }))).toEqual({
      status: 'forbidden',
    });
    expect(classifyGroupError(Object.assign(new Error('x'), { status: 401 }))).toEqual({
      status: 'forbidden',
    });
    expect(classifyGroupError(Object.assign(new Error('down'), { status: 500 }))).toEqual({
      status: 'failed',
      message: 'down',
    });
  });

  /** A rejection with no status at all is still reportable, never swallowed. */
  it('reports an unrecognisable rejection as a failure rather than ignoring it', () => {
    expect(classifyGroupError('not an error')).toEqual({
      status: 'failed',
      message: 'The search request failed.',
    });
  });
});

describe('entity matching', () => {
  /**
   * Substring, NOT the subsequence matcher the command registry uses. A
   * subsequence match over hundreds of open-ended entity names returns almost
   * all of them, which would bury the real match under coincidences and make
   * the group heading a lie.
   */
  it('matches on a substring and not on a scattered subsequence', () => {
    expect(matchesText('etl-nightly', 'night')).toBe(true);
    expect(matchesText('ETL-Nightly', 'etl')).toBe(true);
    expect(matchesText('etl-nightly', 'enl')).toBe(false);
  });

  it('treats an absent field as a non-match rather than throwing', () => {
    expect(matchesText(undefined, 'x')).toBe(false);
    expect(matchesText(null, 'x')).toBe(false);
  });
});

describe('the fan-out', () => {
  it('asks every source and files each answer under its own group', async () => {
    const results = await searchEverything(
      'default',
      'etl',
      sources({
        workflows: async () => [hit('w1')],
        runs: async () => [hit('r1')],
        schedules: async () => ({ hits: [hit('s1')], truncated: false }),
        workers: async () => ({ hits: [hit('k1')], truncated: false }),
        users: async () => ({ hits: [hit('u1')], truncated: false }),
      }),
    );

    expect(Object.keys(results).sort()).toEqual([...SEARCH_GROUPS].sort());
    for (const group of SEARCH_GROUPS) {
      const r = results[group];
      expect(r?.status).toBe('ok');
    }
  });

  /**
   * Promise.all would reject on the first rejection, which here means one 403
   * from the admin-gated users endpoint deletes workflow matches that had
   * already arrived. Every group must be allowed to fail alone.
   */
  it('lets one group fail without losing the others', async () => {
    const results = await searchEverything(
      'default',
      'etl',
      sources({
        workflows: async () => [hit('w1')],
        users: reject(403),
        schedules: reject(500, 'scheduler down'),
      }),
    );

    expect(results.Workflows).toEqual({ status: 'ok', hits: [hit('w1')], truncated: false });
    expect(results.Users).toEqual({ status: 'forbidden' });
    expect(results.Schedules).toEqual({ status: 'failed', message: 'scheduler down' });
  });

  /** Nothing escapes: a group failure is data, not an exception for the caller. */
  it('never rejects, even when every source does', async () => {
    const all = reject(500, 'everything is down');
    const results = await searchEverything('default', 'etl', {
      workflows: all,
      runs: all,
      schedules: all,
      workers: all,
      users: all,
    });
    for (const group of SEARCH_GROUPS) {
      expect(results[group]?.status).toBe('failed');
    }
  });

  /**
   * Concurrency asserted as OVERLAP. A sequence of awaits would make total
   * latency the sum of five endpoints and let the slowest delay the fastest,
   * and it is invisible in a result-shape assertion -- so it is measured.
   */
  it('asks all five at once rather than one after another', async () => {
    let inFlight = 0;
    let peak = 0;
    const release: Array<() => void> = [];
    const gate = async () => {
      inFlight += 1;
      peak = Math.max(peak, inFlight);
      await new Promise<void>((r) => release.push(r));
      inFlight -= 1;
    };

    const pending = searchEverything('default', 'etl', {
      workflows: async () => (await gate(), []),
      runs: async () => (await gate(), []),
      schedules: async () => (await gate(), { hits: [], truncated: false }),
      workers: async () => (await gate(), { hits: [], truncated: false }),
      users: async () => (await gate(), { hits: [], truncated: false }),
    });

    // One turn is enough for five concurrently-entered functions to be parked.
    await new Promise((r) => setTimeout(r, 0));
    expect(peak).toBe(5);

    release.forEach((r) => r());
    await pending;
  });

  /** Twenty rows behind one cursor is a list nobody arrows through. */
  it('caps each group so five sources cannot flood the panel', async () => {
    const many = Array.from({ length: 20 }, (_, i) => hit(`w${i}`));
    const results = await searchEverything(
      'default',
      'etl',
      sources({
        workflows: async () => many,
        users: async () => ({ hits: many, truncated: true }),
      }),
    );
    expect(results.Workflows).toMatchObject({ status: 'ok' });
    expect((results.Workflows as { hits: SearchHit[] }).hits).toHaveLength(PER_GROUP);
    // Truncation survives the cap: it is a fact about the FETCH, not about how
    // many rows were shown.
    expect(results.Users).toMatchObject({ status: 'ok', truncated: true });
  });

  /**
   * A server-side search saw the whole table, so it must never claim to have
   * been capped -- the limit it was given bounds the matches returned, not the
   * rows considered. Claiming otherwise would make the honest truncation notice
   * meaningless by printing it everywhere.
   */
  it('never marks a server-searched group as truncated', async () => {
    const results = await searchEverything(
      'default',
      'etl',
      sources({ workflows: async () => [hit('w1')] }),
    );
    expect(results.Workflows).toMatchObject({ truncated: false });
  });

  it('hands the trimmed query to every source', async () => {
    const seen: string[] = [];
    await searchEverything(
      'default',
      '  etl  ',
      sources({
        workflows: async (_ns, q) => {
          seen.push(q);
          return [];
        },
        users: async (_ns, q) => {
          seen.push(q);
          return { hits: [], truncated: false };
        },
      }),
    );
    expect(seen).toEqual(['etl', 'etl']);
  });
});

describe('entity routes', () => {
  it('builds routes that survive a name needing escaping', () => {
    expect(schedulePath('a/b')).toBe('/schedules/a%2Fb');
    expect(workerPath('host:1')).toBe('/workers/host%3A1');
  });

  /**
   * Every route the search can produce must be a route the shell serves. A
   * search result that lands on the 404 page is worse than no result: it tells
   * the operator the thing exists and then loses it.
   */
  it('only produces routes the router actually has', () => {
    expect(workflowPath('x')).toMatch(/^\/workflows\//);
    expect(runPath('x')).toMatch(/^\/runs\//);
    expect(schedulePath('x')).toMatch(/^\/schedules\//);
    expect(workerPath('x')).toMatch(/^\/workers\//);
  });
});
