// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The default chrome: a vertical rail carrying the brand, the grouped
 * destinations, and the account control.
 *
 * Vertical rather than horizontal by default because the destination list only
 * grows — eight today across four concerns — and a top bar that wraps to a
 * second line is worse than a rail with room to spare. The rail also gives the
 * namespace somewhere honest to live: directly under the brand, where it reads
 * as "which slice of the world am I looking at" rather than as one more control
 * lost in a row of them.
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
  /** Shown under the brand: the namespace every page's data is scoped to. */
  namespace: string;
  /** The account control, composed by App so this file owns no session state. */
  account: ReactNode;
}

export function Sidebar({ canAdmin, namespace, account }: SidebarProps) {
  const groups = visibleGroups(canAdmin);

  return (
    <div className="sidebar">
      <div className="sidebar-brand">
        <span className="brand" aria-hidden="true">
          <Logo size={20} className="brand-mark" />
          <span className="brand-word">Nexus</span>
        </span>
        {/* The namespace is displayed, not edited, here: editing it is a rare
            act with wide consequences, so it lives behind the account menu
            while its VALUE stays permanently visible. */}
        <span className="sidebar-namespace" title={`Namespace: ${namespace}`}>
          {namespace}
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
