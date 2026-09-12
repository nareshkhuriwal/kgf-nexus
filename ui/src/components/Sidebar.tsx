// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The default chrome: a vertical rail carrying the brand, the grouped
 * destinations, and the account control.
 *
 * Vertical rather than horizontal by default because the destination list only
 * grows — eight today across four concerns — and a top bar that wraps to a
 * second line is worse than a rail with room to spare.
 *
 * The namespace used to sit directly under the brand and no longer does: a
 * mono-spaced string tucked under a wordmark reads as part of the product name
 * rather than as context, which is noise in the one place the eye lands first.
 * It moved to the account control at the FAR END of the rail — still permanently
 * on screen, because an operator scoped to the wrong namespace otherwise reads
 * a plausible empty dashboard as "nothing is running", but now adjacent to the
 * identity it qualifies rather than to the logo.
 *
 * The account control is pinned to the BOTTOM, away from navigation. Signing
 * out is not a destination, and putting it adjacent to the links invites the
 * mis-click that ends someone's session during an incident.
 */

import { NavLink } from 'react-router-dom';
import type { ReactNode } from 'react';
import { Logo } from './Logo';
import { isEndMatch, visibleGroups } from './navItems';

export interface SidebarProps {
  /** Gate for the Govern group. The gateway's answer, never re-derived here. */
  canAdmin: boolean;
  /**
   * Accepted and deliberately UNUSED.
   *
   * The namespace is rendered by the account control now, not by the rail, but
   * the prop stays in the signature so the shell that composes both layouts
   * keeps compiling while it is updated. A later cleanup drops it from both
   * layouts and the caller in one commit.
   */
  namespace?: string;
  /** The account control, composed by App so this file owns no session state. */
  account: ReactNode;
}

export function Sidebar({ canAdmin, account }: SidebarProps) {
  const groups = visibleGroups(canAdmin);

  return (
    <div className="sidebar">
      <div className="sidebar-brand">
        <span className="brand" aria-hidden="true">
          <Logo size={20} className="brand-mark" />
          <span className="brand-word">Nexus</span>
        </span>
      </div>

      {/* Labelled so a screen-reader user landing here knows which of the
          page's two <nav>s (this and any in-page one) they are in. */}
      <nav className="sidebar-nav" aria-label="Primary">
        {groups.map((group) => (
          <div className="nav-group" key={group.label}>
            {/* aria-hidden: the heading is a visual grouping cue, and the
                accessible name of each link already carries its destination.
                Announcing "Observe" as a stray line of text before three links
                adds noise without adding navigation. */}
            <p className="nav-group-label" aria-hidden="true">
              {group.label}
            </p>
            <ul className="nav-list">
              {group.items.map((item) => (
                <li key={item.to}>
                  <NavLink
                    to={item.to}
                    end={isEndMatch(item)}
                    className={({ isActive }) => `nav-item${isActive ? ' active' : ''}`}
                  >
                    <span className="nav-glyph" aria-hidden="true">
                      {item.glyph}
                    </span>
                    <span className="nav-label">{item.label}</span>
                  </NavLink>
                </li>
              ))}
            </ul>
          </div>
        ))}
      </nav>

      <div className="sidebar-account">{account}</div>
    </div>
  );
}
