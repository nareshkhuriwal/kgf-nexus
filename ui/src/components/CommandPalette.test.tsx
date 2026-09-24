// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Behaviour tests for the command palette.
 *
 * A palette is a keyboard instrument, so almost every assertion here is about a
 * key: the one that opens it, the ones that move the cursor, the one that runs
 * a row, and the one that closes it and gives focus back. Those are the ways
 * this control fails while still looking finished in a screenshot -- a palette
 * that opens but strands a keyboard user is worse than no palette.
 *
 * The remote-failure case is here for a different reason: an empty list under a
 * search field reads as "no matches", which is the opposite of what a failed
 * request means, and an operator acting on that confusion during an incident
 * goes looking for a workflow they were just told does not exist.
 */

import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CommandPalette } from './CommandPalette';
import type { Command } from '../lib/commands';
import { WithPermissions } from '../test/permissions';

/** Cmd+K, as the browser delivers it. */
function pressCmdK(target: Document | Element = document) {
  fireEvent.keyDown(target, { key: 'k', metaKey: true });
}

function open() {
  pressCmdK();
  return screen.getByRole('dialog');
}

function dialog() {
  return screen.getByRole('dialog');
}

function field() {
  return screen.getByRole('combobox');
}

function optionLabels() {
  return screen.getAllByRole('option').map((o) => o.textContent ?? '');
}

/** The option the cursor is on, as assistive technology would resolve it. */
function activeLabel(): string {
  const id = field().getAttribute('aria-activedescendant');
  expect(id).toBeTruthy();
  return document.getElementById(id!)?.textContent ?? '';
}

const fixture: Command[] = [
  { id: 'go.overview', title: 'Overview', group: 'Go to', hint: '/', run: (c) => c.navigate('/') },
  { id: 'go.runs', title: 'Runs', group: 'Go to', hint: '/runs', run: (c) => c.navigate('/runs') },
  {
    id: 'go.workers',
    title: 'Workers',
    group: 'Go to',
    hint: '/workers',
    run: (c) => c.navigate('/workers'),
  },
];

function setup(over: Partial<React.ComponentProps<typeof CommandPalette>> = {}) {
  const onNavigate = vi.fn();
  // The palette filters commands and search groups by permission, so without a
  // provider the list is empty except for the commands that require none.
  const utils = render(
    <WithPermissions>
      <CommandPalette
        namespace="default"
        onNavigate={onNavigate}
        commands={fixture}
        {...over}
      />
    </WithPermissions>,
  );
  return { onNavigate, ...utils };
}

/** Records requested URLs, so "the search reached the server" is testable. */
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

beforeEach(() => {
  vi.useRealTimers();
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = '';
});

describe('opening and closing', () => {
  it('stays out of the way until Cmd+K', () => {
    setup();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    open();
    expect(dialog()).toHaveAttribute('aria-modal', 'true');
  });

  it('opens on Ctrl+K too, for the operators not on a Mac', () => {
    setup();
    fireEvent.keyDown(document, { key: 'k', ctrlKey: true });
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  /**
   * Cmd+K carries a modifier, so it cannot collide with typing. Suppressing it
   * inside a field would mean the shortcut works everywhere EXCEPT where an
   * operator's hands already are.
   */
  it('still opens while the operator is typing in another field', () => {
    setup();
    const other = document.createElement('input');
    document.body.appendChild(other);
    other.focus();
    pressCmdK(other);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  /** Cmd+F must keep opening browser find; a palette that steals it broke it. */
  it('ignores every shortcut that is not Cmd+K', () => {
    setup();
    fireEvent.keyDown(document, { key: 'f', metaKey: true });
    fireEvent.keyDown(document, { key: 'k' });
    fireEvent.keyDown(document, { key: 'k', metaKey: true, shiftKey: true });
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('puts the cursor in the field, so the first keystroke lands there', () => {
    setup();
    open();
    expect(document.activeElement).toBe(field());
  });

  it('closes on Escape and gives focus back to where it came from', () => {
    setup();
    const trigger = document.createElement('button');
    document.body.appendChild(trigger);
    trigger.focus();

    pressCmdK(trigger);
    expect(screen.getByRole('dialog')).toBeInTheDocument();

    fireEvent.keyDown(dialog(), { key: 'Escape' });

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.activeElement).toBe(trigger);
  });

  it('dismisses when the backdrop is clicked', () => {
    const { container } = setup();
    open();
    const backdrop = container.querySelector('.cmdk-backdrop')!;
    fireEvent.mouseDown(backdrop);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('does not dismiss when the click starts inside the dialog', () => {
    setup();
    open();
    fireEvent.mouseDown(dialog());
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  /** Tab must not reach the page behind an overlay the operator cannot see. */
  it('traps focus in the field while open', () => {
    setup();
    open();
    fireEvent.keyDown(dialog(), { key: 'Tab' });
    expect(document.activeElement).toBe(field());
  });
});

describe('the list', () => {
  it('lists every command when nothing is typed', () => {
    setup();
    open();
    expect(optionLabels().map((l) => l.replace(/\/.*$/, '').trim())).toEqual([
      'Overview',
      'Runs',
      'Workers',
    ]);
  });

  it('moves the selection with the arrow keys', () => {
    setup();
    open();
    expect(activeLabel()).toContain('Overview');

    fireEvent.keyDown(dialog(), { key: 'ArrowDown' });
    expect(activeLabel()).toContain('Runs');

    fireEvent.keyDown(dialog(), { key: 'ArrowDown' });
    expect(activeLabel()).toContain('Workers');

    fireEvent.keyDown(dialog(), { key: 'ArrowUp' });
    expect(activeLabel()).toContain('Runs');
  });

  it('wraps past the ends rather than sticking', () => {
    setup();
    open();
    fireEvent.keyDown(dialog(), { key: 'ArrowUp' });
    expect(activeLabel()).toContain('Workers');
    fireEvent.keyDown(dialog(), { key: 'ArrowDown' });
    expect(activeLabel()).toContain('Overview');
  });

  it('marks exactly one option selected, for assistive technology', () => {
    setup();
    open();
    fireEvent.keyDown(dialog(), { key: 'ArrowDown' });
    const selected = screen.getAllByRole('option').filter(
      (o) => o.getAttribute('aria-selected') === 'true',
    );
    expect(selected).toHaveLength(1);
    expect(selected[0]?.textContent).toContain('Runs');
  });

  it('runs the selected command on Enter and closes', () => {
    const { onNavigate } = setup();
    open();
    fireEvent.keyDown(dialog(), { key: 'ArrowDown' });
    fireEvent.keyDown(dialog(), { key: 'Enter' });

    expect(onNavigate).toHaveBeenCalledWith('/runs');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('runs a command the mouse clicks', () => {
    const { onNavigate } = setup();
    open();
    fireEvent.click(screen.getByText('Workers'));
    expect(onNavigate).toHaveBeenCalledWith('/workers');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('narrows the list as the operator types', () => {
    setup();
    open();
    fireEvent.change(field(), { target: { value: 'run' } });

    const labels = optionLabels().join(' ');
    expect(labels).toContain('Runs');
    expect(labels).not.toContain('Workers');
  });

  it('says so when nothing matches, rather than showing a blank panel', () => {
    setup();
    open();
    fireEvent.change(field(), { target: { value: 'zzzzzz' } });
    expect(screen.getByText(/nothing matches/i)).toBeInTheDocument();
  });
});

/**
 * The global search.
 *
 * WHAT CHANGED, AND WHY THESE TESTS LOOK DIFFERENT FROM THE ONES THEY REPLACE.
 * This palette used to make exactly one request -- the workflow catalog -- so
 * its tests drove it through a fetch mock and asserted a single panel-wide
 * error banner. It now fans out across five sources with different
 * capabilities, and the two assertions that pinned the OLD shape had to change
 * rather than be kept:
 *
 *  - "asks the catalog endpoint ... and offers what comes back" still exists,
 *    still through a real fetch mock, because the production wiring genuinely
 *    must reach `search=` server-side. It now also asserts the result lands
 *    under a Workflows heading rather than a generic "Search" one.
 *  - "says the search failed rather than implying there were no matches" is
 *    now PER GROUP. A single banner would be a lie once one group can fail
 *    while four succeed, so the assertion moved from "the panel says it failed"
 *    to "the group that failed says so and the groups that worked still render".
 *
 * Everything else drives the palette through injected sources instead of a
 * fetch mock. That is not test convenience: a fetch mock has to infer which
 * group a URL belongs to, so a test for "the users group 403s" would actually
 * be a test of URL matching, and it would keep passing if the fan-out started
 * sending the users query to the wrong endpoint.
 */

import type { CappedHits, SearchHit, SearchSources } from '../lib/commands';

/** A source set where every group answers with nothing, to be overridden. */
function emptySources(over: Partial<SearchSources> = {}): SearchSources {
  const noHits = async (): Promise<SearchHit[]> => [];
  const noCapped = async (): Promise<CappedHits> => ({ hits: [], truncated: false });
  return {
    workflows: noHits,
    runs: noHits,
    schedules: noCapped,
    workers: noCapped,
    users: noCapped,
    ...over,
  };
}

function hit(id: string, label: string): SearchHit {
  return { id, label, path: `/${id}` };
}

/** Rejects the way the API client does, so status-based classification is real. */
function refuse(status: number, message: string) {
  return () => Promise.reject(Object.assign(new Error(message), { status }));
}

/** The palette debounces at 250ms; this waits past it and lets state settle. */
async function settle(ms = 400) {
  await act(async () => {
    await new Promise((r) => setTimeout(r, ms));
  });
}

/** The group headings currently on screen, in render order. */
function headings(): string[] {
  return Array.from(document.querySelectorAll('.cmdk-group')).map((h) => h.textContent ?? '');
}

describe('search', () => {
  it('offers to open a run when the query is a run id', () => {
    setup();
    open();
    const id = '3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31';
    fireEvent.change(field(), { target: { value: id } });

    expect(screen.getByText(`Open run ${id}`)).toBeInTheDocument();
  });

  it('navigates to the run when that row is activated', () => {
    const { onNavigate } = setup();
    open();
    const id = '3f4a1c2e-9b7d-4a6f-8c1e-2d5b7a9f0c31';
    fireEvent.change(field(), { target: { value: id } });
    fireEvent.keyDown(dialog(), { key: 'Enter' });
    expect(onNavigate).toHaveBeenCalledWith(`/runs/${id}`);
  });

  /** A paste is answered from the query string alone: no request, no wait. */
  it('resolves a run id locally without asking any source', async () => {
    const asked: string[] = [];
    const sources = emptySources({
      workflows: async () => {
        asked.push('workflows');
        return [];
      },
    });
    setup({ sources });
    open();
    fireEvent.change(field(), { target: { value: '3f4a1c2e9b7d4a6f8c1e2d5b7a9f0c31' } });
    await settle();

    expect(screen.getByText(/Open run 3f4a1c2e-9b7d/)).toBeInTheDocument();
    expect(asked).toHaveLength(0);
  });

  it('asks the catalog endpoint for a non-uuid query and offers what comes back', async () => {
    const urls = mockFetch(() => [
      200,
      {
        workflows: [
          {
            id: 'spec-a',
            namespace: 'default',
            name: 'etl-nightly',
            latest_version: 4,
            spec_hash: 'h',
            created_by: 'admin',
            updated_at: '2026-09-12T09:00:00Z',
            version_count: 4,
            first_created_at: '2026-09-01T09:00:00Z',
          },
        ],
      },
      // Deliberately the production wiring: the one assertion that would still
      // pass if searchSources sent the workflow query to the wrong endpoint.
    ]);

    const { onNavigate } = setup();
    open();
    fireEvent.change(field(), { target: { value: 'etl' } });

    await waitFor(() => expect(screen.getAllByText('etl-nightly').length).toBeGreaterThan(0));
    // Searched SERVER-side: a client filter would silently lie once the
    // namespace outgrows one page.
    expect(urls.some((u) => u.includes('search=etl'))).toBe(true);
    expect(headings()).toContain('Workflows');

    fireEvent.click(screen.getAllByText('etl-nightly')[0]!);
    expect(onNavigate).toHaveBeenCalledWith('/workflows/etl-nightly');
  });

  /**
   * The grouping requirement, stated as the thing it buys: twenty mixed rows
   * from five sources are only scannable if the eye can skip four of them.
   */
  it('files every entity type under its own heading', async () => {
    setup({
      sources: emptySources({
        workflows: async () => [hit('workflow.a', 'etl-nightly')],
        runs: async () => [hit('run.a', 'etl-nightly · RUNNING')],
        schedules: async () => ({ hits: [hit('schedule.a', 'nightly-2am')], truncated: false }),
        workers: async () => ({ hits: [hit('worker.a', 'box-7.dc1')], truncated: false }),
        users: async () => ({ hits: [hit('user.a', 'Ada Lovelace')], truncated: false }),
      }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'nightly' } });
    await settle();

    // Fixed order, not arrival order: a panel whose headings reshuffle between
    // keystrokes cannot be used with muscle memory.
    expect(headings().slice(0, 5)).toEqual([
      'Workflows',
      'Runs',
      'Schedules',
      'Workers',
      'Users',
    ]);
    expect(screen.getByText('box-7.dc1')).toBeInTheDocument();
    expect(screen.getByText('Ada Lovelace')).toBeInTheDocument();
  });

  /**
   * THE ONE THAT MATTERS MOST.
   *
   * A 403 from the admin-gated users endpoint must not delete the workflow
   * matches that already arrived, and the users group must not look empty --
   * "no user called that" and "you may not look" are opposite facts, and an
   * operator who acts on the confusion concludes a colleague does not exist.
   */
  it('degrades one refused group without emptying the others', async () => {
    setup({
      sources: emptySources({
        workflows: async () => [hit('workflow.a', 'etl-nightly')],
        users: refuse(403, 'Forbidden (HTTP 403).'),
      }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'etl' } });
    await settle();

    // The group that worked is intact.
    expect(screen.getByText('etl-nightly')).toBeInTheDocument();
    // The group that was refused says so, under its own heading, and is not
    // silently absent.
    expect(headings()).toContain('Users');
    expect(screen.getByText(/may not read users/i)).toBeInTheDocument();
    // And it is not dressed as a breakage: a permission boundary is the console
    // working correctly.
    expect(document.querySelector('.cmdk-note.is-danger')).toBeNull();
  });

  /** A broken source IS dressed as a breakage, and names what broke. */
  it('reports a failed group as failed rather than as empty', async () => {
    setup({
      sources: emptySources({
        workflows: async () => [hit('workflow.a', 'etl-nightly')],
        schedules: refuse(500, 'The scheduler is down.'),
      }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'etl' } });
    await settle();

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/could not search schedules/i);
    expect(alert.textContent).toMatch(/scheduler is down/i);
    expect(alert.className).toContain('is-danger');
    // The other groups survived it, which Promise.all would not have allowed.
    expect(screen.getByText('etl-nightly')).toBeInTheDocument();
  });

  /**
   * "Nothing matches" is an assertion about the world. With a source refused it
   * has not been established, so it must not be made.
   */
  it('withholds “nothing matches” while any group could not be searched', async () => {
    setup({
      sources: emptySources({ users: refuse(403, 'Forbidden (HTTP 403).') }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'zzzzzz' } });
    await settle();

    expect(screen.queryByText(/nothing matches/i)).toBeNull();
    expect(screen.getByText(/may not read users/i)).toBeInTheDocument();
  });

  /**
   * The subtler half of the same honesty rule. A capped group that found
   * nothing has not established that nothing exists -- it has established that
   * nothing in the first page matched. It prints no note of its own (a caveat
   * under every silent group would drown the panel), so the claim is withheld
   * instead, which is the only place it could have misled anyone.
   */
  it('withholds “nothing matches” when a capped group looked at one page and missed', async () => {
    setup({
      sources: emptySources({
        users: async () => ({ hits: [], truncated: true }),
      }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'zzzzzz' } });
    await settle();

    expect(screen.queryByText(/nothing matches/i)).toBeNull();
  });

  /** And it IS claimed when every group genuinely answered in full. */
  it('says nothing matches once every group answered completely', async () => {
    setup({ sources: emptySources() });
    open();
    fireEvent.change(field(), { target: { value: 'zzzzzz' } });
    await settle();

    expect(screen.getByText(/nothing matches/i)).toBeInTheDocument();
  });

  /**
   * The capped groups must not imply they saw every row. The users, schedules
   * and worker endpoints have no search parameter, so a match past the cap was
   * never visible and claiming completeness would be the same class of lie as
   * an empty list for a failed request.
   */
  it('admits when a client-filtered group only matched a capped page', async () => {
    setup({
      sources: emptySources({
        users: async () => ({ hits: [hit('user.a', 'Ada Lovelace')], truncated: true }),
      }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'ada' } });
    await settle();

    expect(screen.getByText(/Matched within the first \d+ users/i)).toBeInTheDocument();
    expect(screen.getByText('Ada Lovelace')).toBeInTheDocument();
  });

  /**
   * Concurrency, asserted as OVERLAP rather than as "allSettled was called".
   *
   * Sequential awaits would make the total latency the sum of five endpoints
   * and would let the slowest group delay the fastest. The test holds every
   * source open at once and checks that all five were entered before any of
   * them returned -- which a waterfall cannot do.
   */
  it('fans out concurrently rather than waiting on each source in turn', async () => {
    let entered = 0;
    let maxConcurrent = 0;
    const release: Array<() => void> = [];
    const gate = async () => {
      entered += 1;
      maxConcurrent = Math.max(maxConcurrent, entered);
      await new Promise<void>((r) => release.push(r));
      entered -= 1;
    };

    setup({
      sources: {
        workflows: async () => {
          await gate();
          return [];
        },
        runs: async () => {
          await gate();
          return [];
        },
        schedules: async () => {
          await gate();
          return { hits: [], truncated: false };
        },
        workers: async () => {
          await gate();
          return { hits: [], truncated: false };
        },
        users: async () => {
          await gate();
          return { hits: [], truncated: false };
        },
      },
    });
    open();
    fireEvent.change(field(), { target: { value: 'etl' } });
    await settle();

    expect(maxConcurrent).toBe(5);
    await act(async () => {
      release.forEach((r) => r());
      await Promise.resolve();
    });
  });

  /**
   * Typing is faster than the network, and a fan-out makes that worse: five
   * in-flight requests per keystroke, any of which can land after the operator
   * has moved on. A stale result overwriting a fresh one shows matches for a
   * prefix that is no longer on screen.
   */
  it('never lets a slow response for an older query overwrite a newer one', async () => {
    let resolveSlow: (rows: SearchHit[]) => void = () => {};
    const sources = emptySources({
      workflows: (_ns, q) => {
        if (q === 'et') {
          return new Promise<SearchHit[]>((r) => {
            resolveSlow = r;
          });
        }
        return Promise.resolve([hit('workflow.new', 'etl-nightly')]);
      },
    });

    setup({ sources });
    open();
    fireEvent.change(field(), { target: { value: 'et' } });
    await settle();
    // The newer query supersedes the one still in flight.
    fireEvent.change(field(), { target: { value: 'etl' } });
    await settle();
    expect(screen.getByText('etl-nightly')).toBeInTheDocument();

    // The stale answer lands last, and must be discarded.
    await act(async () => {
      resolveSlow([hit('workflow.stale', 'STALE-RESULT')]);
      await new Promise((r) => setTimeout(r, 50));
    });

    expect(screen.queryByText('STALE-RESULT')).toBeNull();
    expect(screen.getByText('etl-nightly')).toBeInTheDocument();
  });

  /**
   * The gate matters five times as much as it did: one keystroke is now five
   * requests, so an ungated field would put five per character on the gateway.
   */
  it('does not spend a request on a one-character query', async () => {
    let asked = 0;
    setup({
      sources: emptySources({
        workflows: async () => {
          asked += 1;
          return [];
        },
        users: async () => {
          asked += 1;
          return { hits: [], truncated: false };
        },
      }),
    });
    open();
    fireEvent.change(field(), { target: { value: 'e' } });
    // Waited past the debounce inside act(), so the settle is a state update the
    // test owns rather than one React warns about after the fact.
    await settle();
    expect(asked).toBe(0);
  });

  /** And the gate holds through the real wiring too: no URL is ever requested. */
  it('spends no fetch at all on a one-character query', async () => {
    const urls = mockFetch(() => [200, { workflows: [] }]);
    setup();
    open();
    fireEvent.change(field(), { target: { value: 'e' } });
    await settle();
    expect(urls).toHaveLength(0);
  });
});
