// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { permissionForPath } from './Permissions';

/**
 * The console's destinations, declared ONCE.
 *
 * Three surfaces render this list — the sidebar's expanded rail, the same rail
 * collapsed to icons-with-labels, and the narrow-width drawer — and the whole
 * reason it lives in its own module is that a second copy would drift. The
 * first thing to rot in a multi-surface shell is a link added to one renderer
 * and forgotten in another, and an operator who cannot reach Schedules because
 * their window is narrow has been failed by a data-structure decision, not by a
 * styling one.
 *
 * The GROUPING is the editorial content here, not decoration. It answers "what
 * kind of thing is this" before the operator has read the label: Observe is
 * what happened, Automate is what will happen without anyone asking, Fleet is
 * what it runs on, Govern is who may touch any of it.
 */

import {
  IconAudit,
  IconCluster,
  IconShield,
  IconClock,
  IconQuota,
  IconDag,
  IconGauge,
  IconKey,
  IconBot,
  IconRuns,
  IconServer,
  IconUsers,
  type IconComponent,
} from './icons';

export interface NavItem {
  /** Router path. `end` matching is derived from this being the root. */
  to: string;
  label: string;
  /**
   * The destination's icon, as a COMPONENT rather than a text glyph.
   *
   * This used to be `glyph: string` — a single character like ◎ or ❋ — with the
   * stated reasoning that the icon only served a narrow-viewport fallback and
   * an icon set would cost more than that fallback was worth. Two things
   * changed. The fallback is now a first-class state: the rail collapses on
   * purpose, at any width, and in that state the icon is the PRIMARY way a
   * destination is recognised. And the glyphs were never doing the job even as
   * decoration — ⌥ is the Mac option key standing in for a DAG, ❋ a dingbat
   * standing in for an audit log. An icon that has to be decoded is a label
   * with extra steps.
   *
   * A component, not a string or a path, because it lets each icon be its own
   * documented drawing with its own reasoning (see icons.tsx) while all three
   * renderers stay dumb: they call it and hand it a size. Inline SVG inheriting
   * currentColor is what makes one drawing serve the muted rest state, the
   * accent active state and every theme without a second copy.
   */
  icon: IconComponent;
  /**
   * Whether the gateway must have said this credential may administer. Gating
   * lives on the ITEM so every surface filters identically; a renderer that
   * decided for itself would be a second authorization opinion.
   */
  adminOnly?: boolean;
}

export interface NavGroup {
  /** Uppercase heading in the expanded rail. Dropped when it is collapsed,
      where there is no width for a heading, and kept in the drawer. */
  label: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  {
    label: 'Observe',
    items: [
      { to: '/', label: 'Overview', icon: IconGauge },
      // Runs before Workflows here, unlike the old top bar: an operator opening
      // this console mid-incident is looking for a run, not for the catalog.
      { to: '/runs', label: 'Runs', icon: IconRuns },
      { to: '/workflows', label: 'Workflows', icon: IconDag },
    ],
  },
  {
    label: 'Automate',
    items: [{ to: '/schedules', label: 'Schedules', icon: IconClock }],
  },
  {
    label: 'Fleet',
    items: [{ to: '/workers', label: 'Workers', icon: IconServer }],
  },
  {
    label: 'Govern',
    items: [
      { to: '/admin/infrastructure', label: 'Infrastructure', icon: IconCluster, adminOnly: true },
      { to: '/admin/quotas', label: 'Quotas & usage', icon: IconQuota, adminOnly: true },
      { to: '/admin/roles', label: 'Roles & permissions', icon: IconShield, adminOnly: true },
      { to: '/admin/users', label: 'Users', icon: IconUsers, adminOnly: true },
      { to: '/admin/tokens', label: 'Tokens', icon: IconKey, adminOnly: true },
      { to: '/admin/service-accounts', label: 'Service accounts', icon: IconBot, adminOnly: true },
      { to: '/admin/audit', label: 'Audit', icon: IconAudit, adminOnly: true },
    ],
  },
];

/**
 * The groups this credential may see, with empty groups dropped.
 *
 * Returning groups rather than a flat list keeps every surface's rendering
 * derived from the same filter. A group whose every item was gated away
 * contributes no heading, so a non-admin never sees an empty "Govern" label
 * hinting at what they are missing.
 */
export function visibleGroups(canAdmin: boolean, can?: (permission: string) => boolean): NavGroup[] {
  return NAV_GROUPS.map((g) => ({
    ...g,
    items: g.items.filter((i) => can ? can(permissionForPath(i.to) ?? "") : (!i.adminOnly || canAdmin)),
  })).filter((g) => g.items.length > 0);
}

/**
 * The same destinations as one ordered flat list.
 *
 * Kept now that no surface renders a flat row — the navbar's link row is gone,
 * because the sidebar owns navigation — because it is what TESTS and callers
 * ask "what should be reachable" with, and deriving that from visibleGroups()
 * at each call site is how two answers to one question appear.
 */
export function visibleItems(canAdmin: boolean): NavItem[] {
  return visibleGroups(canAdmin).flatMap((g) => g.items);
}

/**
 * `end` matching belongs only to the root: without it "/" would light up on
 * every nested route, and with it applied everywhere "/runs/abc" would leave
 * the Runs item looking inactive while you are plainly inside Runs.
 */
export function isEndMatch(item: NavItem): boolean {
  return item.to === '/';
}
