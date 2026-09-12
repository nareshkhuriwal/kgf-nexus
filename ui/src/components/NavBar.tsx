// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The alternative chrome: the same destinations as a horizontal top bar.
 *
 * It renders visibleItems() rather than a list of its own, so a destination
 * added to navItems.ts appears here without anyone remembering to add it. That
 * is the entire justification for this file being as thin as it is — the
 * difference between the two layouts is presentation, and presentation is all
 * that should be duplicated.
 *
 * ── the reference top-bar pattern, and what was taken from it ─────────────
 *
 * The bar now follows the shape of a cloud console's top bar: a hamburger, the
 * wordmark, a prominent search field in the middle, then a right-side cluster
 * of icon controls ending in the account block. Three of those were adopted and
 * one thing was refused, all on the same principle — a control in the chrome is
 * a promise, and the chrome is the worst place in the console to break one.
 *
 * ADOPTED, with a deviation: the hamburger. The reference hides EVERY
 * destination behind it. We keep the inline links AND add the hamburger, so
 * navigation stays one click while the drawer supplies the one thing this
 * layout genuinely loses — the group headings, which eight labels plus four
 * headings on one line cannot accommodate. See NavDrawer.tsx; the drawer is
 * worth building only because of the headings, and it is rendered here and not
 * by Sidebar because the rail already shows those groups permanently.
 *
 * ADOPTED: the centred search field. It is a button that opens the existing
 * CommandPalette rather than a second search implementation — see NavSearch.tsx
 * for why an input here would have been a downgrade.
 *
 * REFUSED: most of the icon cluster. The reference bar carries an assistant, a
 * cloud shell, notifications, settings, help and feedback. This console has a
 * real settings page, and it has an account control that must stay. It has no
 * assistant, no shell, no notification feed, no docs site and nowhere to send
 * feedback — so those buttons are ABSENT rather than present and inert. A dead
 * control in the chrome is worse than a missing one: it is on screen on every
 * page, it gets clicked during an incident, and the operator learns that the
 * chrome cannot be trusted. Fidelity to a screenshot is not a reason to ship a
 * button with nothing behind it.
 *
 * The namespace is NOT here. It rode next to the brand in both layouts and
 * read as noise beside the logotype — a mono-spaced string abutting a wordmark
 * looks like part of the product name. It moved to the account control at the
 * far end of the bar, which keeps the property that matters (its value is on
 * screen at all times, so an empty dashboard is legible as "wrong namespace"
 * rather than "nothing is running") while stopping it competing with the brand.
 *
 * The group labels are dropped from the ROW rather than rendered inline.
 * Horizontally they would double the bar's width to restate a classification
 * the operator can already infer from eight labels in a row, and the layout
 * exists precisely for people who want that vertical space back. The drawer is
 * where they come back for anyone who wants them.
 */

import { useRef, useState } from 'react';
import { NavLink } from 'react-router-dom';
import type { ReactNode } from 'react';
import { Logo } from './Logo';
import { NavDrawer } from './NavDrawer';
import { NavSearch } from './NavSearch';
import { isEndMatch, visibleItems } from './navItems';

export interface NavBarProps {
  canAdmin: boolean;
  /**
   * Accepted and deliberately UNUSED.
   *
   * The namespace is rendered by the account control now, not by the chrome,
   * but the prop stays in the signature so the shell that composes both
   * layouts keeps compiling while it is updated. It is the kind of thing a
   * later cleanup removes in one commit across three files rather than a
   * signature break landing in the middle of a restyle.
   */
  namespace?: string;
  account: ReactNode;
  /**
   * Opens the command palette the shell already mounts.
   *
   * A callback rather than an `open` boolean, because the bar's search control
   * can only ever ASK for the palette; the palette owns its own dismissal, its
   * query and its results. Optional so the search control simply does not
   * render for a caller that has no palette to open — a search field that
   * opens nothing is exactly the dead control this file refuses elsewhere.
   */
  onOpenSearch?: () => void;
  /**
   * Navigates to the settings page. Optional for the same reason: the gear is
   * omitted rather than inert when there is nowhere for it to go.
   */
  onOpenSettings?: () => void;
}

export function NavBar({ canAdmin, account, onOpenSearch, onOpenSettings }: NavBarProps) {
  const items = visibleItems(canAdmin);
  const [drawerOpen, setDrawerOpen] = useState(false);
  // The drawer hands focus back here on Escape and on its close button; the
  // ref is owned by the bar because the trigger is.
  const hamburgerRef = useRef<HTMLButtonElement>(null);

  return (
    <div className="navbar">
      {/* Far left, before the brand, which is where the reference puts it and
          also the only position where it does not separate the wordmark from
          the destinations it labels. */}
      <button
        type="button"
        ref={hamburgerRef}
        className="navbar-menu"
        aria-expanded={drawerOpen}
        onClick={() => setDrawerOpen((o) => !o)}
        title="All destinations, grouped"
      >
        <span aria-hidden="true">☰</span>
        <span className="sr-only">All destinations</span>
      </button>

      <span className="brand" aria-hidden="true">
        <Logo size={20} className="brand-mark" />
        <span className="brand-word">Nexus</span>
      </span>

      <nav className="navbar-nav" aria-label="Primary">
        <ul className="nav-list">
          {items.map((item) => (
            <li key={item.to}>
              <NavLink
                to={item.to}
                end={isEndMatch(item)}
                className={({ isActive }) => `nav-item${isActive ? ' active' : ''}`}
              >
                {/* The glyph is dropped at wide widths by CSS and returns when
                    the labels no longer fit, so the bar degrades to a rail
                    instead of wrapping onto a second line. */}
                <span className="nav-glyph" aria-hidden="true">
                  {item.glyph}
                </span>
                <span className="nav-label">{item.label}</span>
              </NavLink>
            </li>
          ))}
        </ul>
      </nav>

      {onOpenSearch && <NavSearch onOpen={onOpenSearch} />}

      {/* The right-side cluster: the gear and the account control, and nothing
          invented to pad it out. Both of these go somewhere that exists. */}
      <div className="navbar-tools">
        {onOpenSettings && (
          <button
            type="button"
            className="navbar-tool"
            onClick={onOpenSettings}
            // Icon-only, so the name lives in .sr-only text and the tooltip
            // repeats it for a sighted mouse user who cannot guess the glyph.
            title="Settings"
          >
            <span aria-hidden="true">⚙</span>
            <span className="sr-only">Settings</span>
          </button>
        )}
        <div className="navbar-account">{account}</div>
      </div>

      <NavDrawer
        open={drawerOpen}
        onClose={() => setDrawerOpen(false)}
        returnFocusTo={hamburgerRef}
        canAdmin={canAdmin}
      />
    </div>
  );
}
