import { usePermission } from './Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The console's navigation: a vertical rail below the navbar, carrying the
 * grouped destinations and the account control.
 *
 * It is no longer one of two alternative chromes. The shell renders the navbar
 * AND this rail together — the bar spans the full width with identity, search
 * and tools; the rail sits under it on the left with the destinations; the page
 * is to the right of the rail. The either/or is gone because it was a false
 * choice: an operator picking "navbar" gave up the group headings to win
 * vertical space, and picking "sidebar" gave up the search field and the tool
 * cluster to keep them. Both halves are cheap and neither competes for the
 * other's axis, so both are present and nobody chooses.
 *
 * WHAT THAT MEANS FOR THIS FILE. The rail lost the brand — the navbar above it
 * owns the logotype now, and two wordmarks stacked in one corner is the kind of
 * duplication that reads as a rendering fault. It keeps the destinations, which
 * are now its entire subject, and the account control at the far end.
 *
 * ── the two widths ────────────────────────────────────────────────────────
 *
 * EXPANDED (the default): group headings, icon and label per row.
 *
 * COLLAPSED: an icon rail that KEEPS A SMALL LABEL BENEATH EACH ICON. This is
 * the decision worth stating, because the obvious collapsed rail is
 * icons-only-with-tooltips and that is what was rejected. A tooltip is not a
 * label: it costs a hover, it never appears on touch, and it means an operator
 * navigating this console has to remember which of eight abstract shapes is
 * Schedules. Keeping two words under each icon costs about 30px of width and
 * removes the memory requirement entirely — the rail stays READABLE at both
 * widths, and the collapse buys page width rather than buying it by making the
 * navigation worse. The group headings are what genuinely cannot survive: there
 * is no room for a heading beside an 80px column, so they are dropped and the
 * groups remain as spacing.
 *
 * Neither state hides a destination. That is the property that makes the
 * collapse safe to offer at all: if collapsing could take a link away, it would
 * be a navigation feature masquerading as a display preference.
 *
 * The account control is NOT here. It used to be pinned to the rail's bottom
 * edge, away from the links, because signing out is not a destination and a
 * mis-click there ends someone's session mid-incident. That reasoning still
 * holds — it is simply satisfied better by the bar, which is where the control
 * now lives: the far end of the top row is further from these links than the
 * bottom of this rail ever was, and it stays in one place while the rail
 * collapses. Rendering it in both would put two account menus on screen at
 * once, which is what happened the first time this shell was wired.
 */

import { NavLink } from 'react-router-dom';
import { isEndMatch, visibleGroups } from './navItems';

export interface SidebarProps {
  /** Gate for the Govern group. The gateway's answer, never re-derived here. */
  canAdmin: boolean;
  /**
   * Whether to render the narrow icon-and-label rail rather than the full one.
   *
   * A prop rather than state owned here, because the control that toggles it is
   * the hamburger in the NAVBAR — a different component entirely — and the
   * shell is the only thing that can see both. Passing it down keeps one
   * boolean in one place; state here would need an event channel to reach it
   * and could disagree with the hamburger's aria-expanded.
   */
  collapsed?: boolean;
}

export function Sidebar({ canAdmin, collapsed = false }: SidebarProps) {
  const can = usePermission();
  const groups = visibleGroups(canAdmin, can);

  return (
    <div className={`sidebar${collapsed ? ' collapsed' : ''}`}>
      {/* Labelled so a screen-reader user landing here knows which of the
          page's <nav>s they are in. The name does not change with the collapsed
          state: it is the same navigation either way, and a landmark that
          renames itself when a display preference changes would make the
          document outline depend on the rail's width. */}
      <nav className="sidebar-nav" aria-label="Primary">
        {groups.map((group) => (
          <div className="nav-group" key={group.label}>
            {/* Rendered in both states and hidden by CSS when collapsed, rather
                than omitted from the tree. The heading is aria-hidden either
                way — see below — so it contributes nothing to the accessibility
                tree that could change; keeping it in the DOM means collapsing
                and expanding does not add and remove nodes around the links,
                which is what would otherwise cost focus mid-navigation. */}
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
                    >
                      {/* aria-hidden: the icon illustrates the label beside it,
                          and icons.tsx already hides each SVG from the
                          accessibility tree. The wrapper is what CSS sizes, so
                          the icon's own box never has to know which state the
                          rail is in. */}
                      <span className="nav-icon" aria-hidden="true">
                        <Icon />
                      </span>
                      {/* The label is present in BOTH states — that is the
                          whole point of this collapsed rail — and CSS only
                          changes where it sits and how large it is. A collapsed
                          rail that dropped it would be the icons-only rail this
                          file's header explains was rejected. */}
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
  );
}
