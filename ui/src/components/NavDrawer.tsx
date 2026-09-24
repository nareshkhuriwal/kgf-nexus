import { usePermission } from './Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The navigation as an off-canvas panel, for phone-width viewports.
 *
 * WHAT THIS FILE USED TO BE, AND WHY IT STILL EXISTS. It was the navbar
 * layout's slide-out list, justified on one thing the flat link row could not
 * do: carry the GROUP HEADINGS. That justification is gone — the link row is
 * gone with it, and the rail renders the headings permanently — so the obvious
 * move was to delete this file. It was kept instead, REPURPOSED, because the
 * restructure created a new problem that this panel is the right answer to.
 *
 * THE PROBLEM: at phone width the sidebar cannot be in the layout at all. Even
 * collapsed it is an 80px column, and 80px out of a 360px viewport is a quarter
 * of the screen spent on navigation while an operator is trying to read a run's
 * attempt history. Collapsing further to icons-only was rejected for the reason
 * Sidebar.tsx gives — a tooltip is not a label, and on touch there is no hover
 * to produce one, so an icons-only rail on a PHONE is the least readable place
 * it could possibly appear.
 *
 * THE ANSWER: below the phone breakpoint the rail leaves the flow entirely and
 * the hamburger opens this panel over the page instead. Navigation becomes two
 * taps at the width where the page needs all of itself, and stays one click at
 * every width where there is room for a rail. Nothing is unreachable at any
 * width, which is the property that matters.
 *
 * WHY NOT JUST SLIDE THE SIDEBAR ITSELF OVER THE PAGE. Because a panel covering
 * the page needs things a rail does not and must not have: a scrim that
 * swallows clicks, a focus trap, an Escape binding, a close button, and a modal
 * role that tells a screen-reader user the page behind is unavailable. That is
 * this file's entire content and it is already written and already tested.
 * Teaching the rail to be modal at one breakpoint would put all of it behind a
 * media query — the hardest kind of behaviour to test and the easiest to get
 * subtly wrong.
 *
 * WHY IT DOES NOT DUPLICATE THE DESTINATION LIST. visibleGroups() is the only
 * source, for the reason navItems.ts gives at length: the first thing to rot in
 * a multi-surface shell is a link added to one renderer and forgotten in
 * another, and an operator who cannot reach Schedules because their window is
 * narrow has been failed by this file, not by a stylesheet.
 *
 * It keeps the headings, unlike the collapsed rail that has no width for them:
 * a full-width panel has room, and the grouping is editorial content — Observe
 * is what happened, Automate is what will happen without anyone asking, Fleet
 * is what it runs on, Govern is who may touch any of it.
 *
 * ── the accessibility shape, and why ──────────────────────────────────────
 *
 * role="dialog" aria-modal="true", not a labelled complementary region. The
 * deciding fact is the scrim: this panel covers the page and swallows clicks on
 * everything behind it, so the content behind it is genuinely unavailable while
 * it is open. A complementary region would tell a screen-reader user they may
 * keep browsing the page they cannot actually touch, which is a lie about the
 * interface's state. The <nav> with its own accessible name lives INSIDE the
 * dialog, so the landmark still exists for anyone navigating by landmark.
 *
 * IT TRAPS FOCUS, unlike ProfileMenu and like CommandPalette, and for
 * CommandPalette's stated reason rather than out of symmetry: it covers the
 * page behind a modal scrim, and Tab reaching a control the operator cannot see
 * and cannot click is worse than Tab doing nothing. The menu does not trap
 * because it is a small layer you can still see around and dismiss by clicking
 * anything; this is not that.
 *
 * Escape closes and returns focus to the hamburger, exactly as ProfileMenu
 * does — a keyboard user dropped at the top of the document has lost their
 * place. Activating a destination closes it WITHOUT restoring focus, exactly as
 * CommandPalette does: the destination owns focus now, and yanking it back to
 * the trigger would undo the navigation that was just asked for. A drawer left
 * open over the page it just opened is the bug this is guarding.
 *
 * The scrim closes it, and is never the only way out: Escape does, every
 * destination does, and the panel carries its own labelled close button, which
 * is what a touch user with no keyboard and no obvious scrim reaches for.
 */

import { useEffect, useId, useRef } from 'react';
import { NavLink } from 'react-router-dom';
import { Logo } from './Logo';
import { IconClose } from './icons';
import { isEndMatch, visibleGroups } from './navItems';

export interface NavDrawerProps {
  open: boolean;
  /** Told to close. Owned by the caller so the hamburger's aria-expanded and
      the panel cannot disagree about whether it is open. */
  onClose: () => void;
  /**
   * Where focus goes when Escape or the close button dismisses the drawer.
   *
   * Passed in rather than captured from document.activeElement on open,
   * because the answer is known statically — it is the hamburger, which is the
   * only thing that can open this — and reading activeElement would hand focus
   * to whatever happened to be focused if the drawer were ever opened from
   * code rather than from a click.
   */
  returnFocusTo?: React.RefObject<HTMLElement | null>;
  /** The gateway's answer on admin_write. Gates the Govern group, exactly as
      the rail does; this file forms no authorization opinion. */
  canAdmin: boolean;
}

export function NavDrawer({ open, onClose, returnFocusTo, canAdmin }: NavDrawerProps) {
  const panelRef = useRef<HTMLDivElement>(null);
  const titleId = useId();
  const can = usePermission();
  const groups = visibleGroups(canAdmin, can);

  /**
   * Dismiss on Escape, and hand focus back.
   *
   * Bound to the document rather than to the panel so it fires wherever focus
   * has ended up — including on the scrim itself, which is not a descendant of
   * the panel and would therefore never see a panel-scoped handler.
   */
  useEffect(() => {
    if (!open) return;
    function onKeyDown(e: KeyboardEvent) {
      if (e.key !== 'Escape') return;
      e.preventDefault();
      onClose();
      returnFocusTo?.current?.focus();
    }
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [open, onClose, returnFocusTo]);

  /**
   * Move focus INTO the panel when it opens.
   *
   * Without this the drawer slides in while focus is still on the hamburger
   * behind the scrim, so the first Tab walks the bar the operator can no longer
   * see. The first focusable child is taken from the DOM rather than tracked,
   * so a control added to the panel later is still the right landing spot.
   */
  useEffect(() => {
    if (!open) return;
    focusables(panelRef.current)[0]?.focus();
  }, [open]);

  if (!open) return null;

  /**
   * The focus trap.
   *
   * A real cycle rather than CommandPalette's collapse-to-one-field, because a
   * drawer's whole content is a list of focusable links: pinning focus to one
   * of them would make the other seven unreachable from the keyboard, which is
   * the opposite of what this panel is for.
   */
  function onPanelKeyDown(e: React.KeyboardEvent<HTMLDivElement>) {
    if (e.key !== 'Tab') return;
    const stops = focusables(panelRef.current);
    if (stops.length === 0) return;
    const first = stops[0];
    const last = stops[stops.length - 1];
    const here = document.activeElement;
    // Only the edges are intercepted; in the middle of the list the browser's
    // own Tab order is already correct and reimplementing it would be a way to
    // get it subtly wrong.
    if (e.shiftKey && here === first) {
      e.preventDefault();
      last?.focus();
    } else if (!e.shiftKey && here === last) {
      e.preventDefault();
      first?.focus();
    }
  }

  return (
    <div
      className="navdrawer-scrim"
      // mousedown rather than click, matching the palette's backdrop: a drag
      // that begins inside the panel and ends outside it is not a dismissal.
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        className="navdrawer"
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        onKeyDown={onPanelKeyDown}
      >
        <div className="navdrawer-head">
          {/* The brand repeats here rather than being omitted: at phone width
              the scrim covers the bar entirely, so without it the panel is a
              list of words floating over a dimmed page with nothing saying
              which product it belongs to. */}
          <span className="brand" aria-hidden="true">
            <Logo size={20} className="brand-mark" />
            <span className="brand-word">KGF Nexus</span>
          </span>
          {/* The dialog's accessible name. Visually hidden because the brand
              beside it is already the visible heading, and printing "All
              destinations" as well would be a second title for one panel. */}
          <h2 className="sr-only" id={titleId}>
            All destinations
          </h2>
          <button
            type="button"
            className="navdrawer-close"
            onClick={() => {
              onClose();
              returnFocusTo?.current?.focus();
            }}
            title="Close navigation"
          >
            <IconClose size={16} />
            <span className="sr-only">Close navigation</span>
          </button>
        </div>

        {/* Labelled distinctly from the bar's own "Primary" nav: both are in
            the document while the drawer is open, and two landmarks with the
            same name is a worse outcome than the duplication itself. */}
        <nav className="navdrawer-nav" aria-label="All destinations">
          {groups.map((group) => (
            <div className="nav-group" key={group.label}>
              {/* aria-hidden for the same reason the rail's headings are: each
                  link's accessible name already carries its destination, and
                  announcing "Observe" as a stray line before three links adds
                  noise without adding navigation. The heading is here for the
                  eye, which is precisely the value the flat row cannot offer. */}
              <p className="nav-group-label" aria-hidden="true">
                {group.label}
              </p>
              <ul className="nav-list">
                {group.items.map((item) => {
                  const Icon = item.icon;
                  return (
                    <li key={item.to}>
                      <NavLink
                        to={item.to}
                        end={isEndMatch(item)}
                        className={({ isActive }) => `nav-item${isActive ? ' active' : ''}`}
                        // Closed on activation, and focus is NOT restored: the
                        // page that was just opened owns focus, and a drawer
                        // still covering it is the defect this exists to avoid.
                        onClick={onClose}
                      >
                        <span className="nav-icon" aria-hidden="true">
                          <Icon />
                        </span>
                        <span className="nav-label">{item.label}</span>
                      </NavLink>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))}
        </nav>
      </div>
    </div>
  );
}

/**
 * The panel's tab stops, in document order.
 *
 * Queried rather than registered so the trap covers whatever the panel happens
 * to contain; `:not([disabled])` and the tabindex guard keep it from parking
 * focus on something the browser would itself skip.
 */
function focusables(root: HTMLElement | null): HTMLElement[] {
  if (!root) return [];
  return Array.from(
    root.querySelectorAll<HTMLElement>(
      'a[href], button:not([disabled]), input:not([disabled]), [tabindex]:not([tabindex="-1"])',
    ),
  );
}
