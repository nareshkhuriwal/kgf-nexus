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
 * The namespace is NOT here. It rode next to the brand in both layouts and
 * read as noise beside the logotype — a mono-spaced string abutting a wordmark
 * looks like part of the product name. It moved to the account control at the
 * far end of the bar, which keeps the property that matters (its value is on
 * screen at all times, so an empty dashboard is legible as "wrong namespace"
 * rather than "nothing is running") while stopping it competing with the brand.
 *
 * The group labels are dropped rather than rendered inline. Horizontally they
 * would double the bar's width to restate a classification the operator can
 * already infer from eight labels in a row, and the layout exists precisely for
 * people who want that vertical space back.
 */

import { NavLink } from 'react-router-dom';
import type { ReactNode } from 'react';
import { Logo } from './Logo';
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
}

export function NavBar({ canAdmin, account }: NavBarProps) {
  const items = visibleItems(canAdmin);

  return (
    <div className="navbar">
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

      <div className="navbar-account">{account}</div>
    </div>
  );
}
