// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the navbar's slide-out destination panel.
 *
 * A drawer is the kind of control that looks finished in a screenshot and fails
 * in use, so almost everything pinned here is a way OUT of it: Escape, the
 * scrim, the close button, and — the one that is a real bug rather than an
 * annoyance — activating a destination, because a panel still covering the page
 * it just opened has actively broken the navigation it was asked to perform.
 *
 * The other half is the drawer's reason to exist. The navbar row drops the group
 * headings, and if the drawer ever stops rendering them it has become a second
 * copy of that row and should be deleted rather than fixed. The heading
 * assertions are therefore not cosmetic; they are the feature.
 *
 * Focus is asserted in both directions. Opening must move focus INTO the panel,
 * or the first Tab walks a bar the operator can no longer see behind the scrim;
 * dismissing must hand it back to the hamburger, or a keyboard user is dropped
 * at the top of the document with no idea where they were. Activating a
 * destination must NOT hand it back, which is the one case where restoring
 * focus would undo what was asked for.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { createRef } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { NavDrawer } from './NavDrawer';
import { visibleItems } from './navItems';

function wrap(ui: React.ReactNode, route = '/') {
  return render(<MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>);
}

function panel() {
  return screen.getByRole('dialog', { name: 'All destinations' });
}

describe('the navigation drawer', () => {
  it('renders nothing at all while closed', () => {
    wrap(<NavDrawer open={false} onClose={() => {}} canAdmin />);

    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryByRole('link', { name: 'Runs' })).toBeNull();
  });

  /**
   * A modal dialog, not a complementary region. The scrim makes the page behind
   * it genuinely untouchable, and telling a screen-reader user they may keep
   * browsing it would be a lie about the interface's state.
   */
  it('is a modal dialog with an accessible name', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />);

    const dialog = panel();
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    // The landmark still exists for anyone navigating by landmark, inside the
    // dialog rather than instead of it.
    expect(within(dialog).getByRole('navigation', { name: 'All destinations' })).toBeInTheDocument();
  });

  /** The whole reason this file exists rather than reusing the flat row. */
  it('shows the group headings the navbar row has to drop', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />);

    for (const label of ['Observe', 'Automate', 'Fleet', 'Govern']) {
      expect(within(panel()).getByText(label)).toBeInTheDocument();
    }
  });

  it('offers every destination the credential may see, as a link', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />);
    const nav = within(panel()).getByRole('navigation', { name: 'All destinations' });

    for (const item of visibleItems(true)) {
      expect(within(nav).getByRole('link', { name: item.label })).toHaveAttribute('href', item.to);
    }
  });

  /**
   * The gate lives on the ITEM in navItems.ts so every renderer filters
   * identically; a drawer that formed its own opinion would be a second
   * authorization decision. visibleGroups() drops the emptied group entirely,
   * so a reader never sees a "Govern" heading hinting at what they are missing.
   */
  it('omits the Govern group and its heading without admin_write', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin={false} />);

    expect(within(panel()).queryByText('Govern')).toBeNull();
    expect(screen.queryByRole('link', { name: 'Users' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Tokens' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Audit' })).toBeNull();
    // The rest of the panel is unaffected.
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
  });

  it('marks the current destination as active, sharing the rail rows', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />, '/workers');

    expect(screen.getByRole('link', { name: 'Workers' }).className).toContain('active');
    expect(screen.getByRole('link', { name: 'Overview' }).className).not.toContain('active');
  });

  // ── the ways out ──────────────────────────────────────────────────────────

  it('closes on Escape', () => {
    const onClose = vi.fn();
    wrap(<NavDrawer open onClose={onClose} canAdmin />);

    fireEvent.keyDown(document, { key: 'Escape' });

    expect(onClose).toHaveBeenCalled();
  });

  it('closes when the scrim is clicked', () => {
    const onClose = vi.fn();
    wrap(<NavDrawer open onClose={onClose} canAdmin />);

    fireEvent.mouseDown(document.querySelector('.navdrawer-scrim') as HTMLElement);

    expect(onClose).toHaveBeenCalled();
  });

  /**
   * A click that lands on the panel is not a dismissal. Without the target
   * check, clicking a heading or the panel's own padding would close the drawer
   * — which is the same class of bug as a dropdown that closes when you try to
   * read it.
   */
  it('does not close when the click lands inside the panel', () => {
    const onClose = vi.fn();
    wrap(<NavDrawer open onClose={onClose} canAdmin />);

    fireEvent.mouseDown(panel());

    expect(onClose).not.toHaveBeenCalled();
  });

  /** The scrim must never be the only way out: a touch user has no Escape key
      and no reason to believe a dimmed area is clickable. */
  it('offers a labelled close button as well as the scrim', () => {
    const onClose = vi.fn();
    wrap(<NavDrawer open onClose={onClose} canAdmin />);

    const close = screen.getByRole('button', { name: 'Close navigation' });
    expect(close).toHaveAttribute('title', 'Close navigation');
    fireEvent.click(close);

    expect(onClose).toHaveBeenCalled();
  });

  /**
   * The bug this component most plausibly ships with: a drawer left open over
   * the page the operator just asked for.
   */
  it('closes when a destination is activated', () => {
    const onClose = vi.fn();
    wrap(<NavDrawer open onClose={onClose} canAdmin />);

    fireEvent.click(screen.getByRole('link', { name: 'Schedules' }));

    expect(onClose).toHaveBeenCalled();
  });

  // ── focus ─────────────────────────────────────────────────────────────────

  it('moves focus into the panel when it opens', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />);

    expect(panel().contains(document.activeElement)).toBe(true);
  });

  it('returns focus to the trigger on Escape', () => {
    const trigger = document.createElement('button');
    document.body.append(trigger);
    const ref = createRef<HTMLElement>();
    // @ts-expect-error createRef gives a read-only current; assigning it here is
    // how the test stands in for the ref NavBar actually attaches to its button.
    ref.current = trigger;

    wrap(<NavDrawer open onClose={() => {}} returnFocusTo={ref} canAdmin />);
    fireEvent.keyDown(document, { key: 'Escape' });

    expect(document.activeElement).toBe(trigger);
    trigger.remove();
  });

  it('returns focus to the trigger from the close button', () => {
    const trigger = document.createElement('button');
    document.body.append(trigger);
    const ref = createRef<HTMLElement>();
    // @ts-expect-error see above.
    ref.current = trigger;

    wrap(<NavDrawer open onClose={() => {}} returnFocusTo={ref} canAdmin />);
    fireEvent.click(screen.getByRole('button', { name: 'Close navigation' }));

    expect(document.activeElement).toBe(trigger);
    trigger.remove();
  });

  /**
   * Activating a destination is the one dismissal that must NOT restore focus:
   * the page just opened owns focus now, and yanking it back to the hamburger
   * would undo the navigation the operator asked for. CommandPalette makes the
   * same distinction for the same reason.
   */
  it('does not pull focus back to the trigger when a destination is activated', () => {
    const trigger = document.createElement('button');
    document.body.append(trigger);
    const ref = createRef<HTMLElement>();
    // @ts-expect-error see above.
    ref.current = trigger;

    wrap(<NavDrawer open onClose={() => {}} returnFocusTo={ref} canAdmin />);
    fireEvent.click(screen.getByRole('link', { name: 'Runs' }));

    expect(document.activeElement).not.toBe(trigger);
    trigger.remove();
  });

  /**
   * It traps, unlike the account menu and like the palette, and for the
   * palette's reason: it covers the page, so Tab reaching a control the
   * operator can neither see nor click is worse than Tab doing nothing. The
   * trap is a real cycle rather than the palette's collapse-to-one-field,
   * because the panel's entire content is a list of links.
   */
  it('cycles Tab at the last stop back to the first', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />);

    const stops = within(panel()).getAllByRole<HTMLElement>('link');
    const close = screen.getByRole('button', { name: 'Close navigation' });
    const last = stops.at(-1) as HTMLElement;

    last.focus();
    fireEvent.keyDown(panel(), { key: 'Tab' });

    // The close button is the panel's first stop, so wrapping lands there.
    expect(document.activeElement).toBe(close);
  });

  it('cycles Shift+Tab at the first stop back to the last', () => {
    wrap(<NavDrawer open onClose={() => {}} canAdmin />);

    const close = screen.getByRole('button', { name: 'Close navigation' });
    const links = within(panel()).getAllByRole<HTMLElement>('link');

    close.focus();
    fireEvent.keyDown(panel(), { key: 'Tab', shiftKey: true });

    expect(document.activeElement).toBe(links.at(-1));
  });
});
