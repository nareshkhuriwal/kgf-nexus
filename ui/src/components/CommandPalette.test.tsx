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
  { id: 'account.signout', title: 'Sign out', group: 'Account', run: (c) => c.signOut() },
];

function setup(over: Partial<React.ComponentProps<typeof CommandPalette>> = {}) {
  const onNavigate = vi.fn();
  const onSignOut = vi.fn();
  const utils = render(
    <CommandPalette
      namespace="default"
      onNavigate={onNavigate}
      onSignOut={onSignOut}
      commands={fixture}
      {...over}
    />,
  );
  return { onNavigate, onSignOut, ...utils };
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
      'Sign out',
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
    expect(activeLabel()).toContain('Sign out');
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
    const { onSignOut } = setup();
    open();
    fireEvent.click(screen.getByText('Sign out'));
    expect(onSignOut).toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('narrows the list as the operator types', () => {
    setup();
    open();
    fireEvent.change(field(), { target: { value: 'run' } });

    const labels = optionLabels().join(' ');
    expect(labels).toContain('Runs');
    expect(labels).not.toContain('Sign out');
  });

  it('says so when nothing matches, rather than showing a blank panel', () => {
    setup();
    open();
    fireEvent.change(field(), { target: { value: 'zzzzzz' } });
    expect(screen.getByText(/nothing matches/i)).toBeInTheDocument();
  });
});

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
    ]);

    const { onNavigate } = setup();
    open();
    fireEvent.change(field(), { target: { value: 'etl' } });

    await waitFor(() => expect(screen.getByText('etl-nightly')).toBeInTheDocument());
    // Searched SERVER-side: a client filter would silently lie once the
    // namespace outgrows one page.
    expect(urls.some((u) => u.includes('search=etl'))).toBe(true);

    fireEvent.click(screen.getByText('etl-nightly'));
    expect(onNavigate).toHaveBeenCalledWith('/workflows/etl-nightly');
  });

  /**
   * The case this whole branch exists for: a failure must be NAMED. An empty
   * list under a search field asserts "no such workflow", which is a different
   * and actionable fact from "I could not ask".
   */
  it('says the search failed rather than implying there were no matches', async () => {
    mockFetch(() => [500, { message: 'catalog is down' }]);

    setup();
    open();
    fireEvent.change(field(), { target: { value: 'etl' } });

    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toMatch(/search failed/i);
    expect(alert.textContent).toMatch(/catalog is down/i);
    // The static commands stay usable: they are still true.
    expect(screen.queryByText(/nothing matches/i)).not.toBeInTheDocument();
  });

  it('does not spend a request on a one-character query', async () => {
    const urls = mockFetch(() => [200, { workflows: [] }]);
    setup();
    open();
    fireEvent.change(field(), { target: { value: 'e' } });
    // Waited past the debounce inside act(), so the settle is a state update the
    // test owns rather than one React warns about after the fact.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 400));
    });
    expect(urls).toHaveLength(0);
  });
});
