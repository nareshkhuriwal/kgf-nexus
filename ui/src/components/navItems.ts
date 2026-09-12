// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The console's destinations, declared ONCE.
 *
 * Two chrome layouts render this list — the sidebar groups it under headings,
 * the navbar flattens it into a row — and the whole reason it lives in its own
 * module is that a second copy would drift. The first thing to rot in a
 * two-layout shell is a link added to one layout and forgotten in the other,
 * and an operator who cannot reach Schedules because they chose the navbar has
 * been failed by a data-structure decision, not by a styling one.
 *
 * The GROUPING is the editorial content here, not decoration. It answers "what
 * kind of thing is this" before the operator has read the label: Observe is
 * what happened, Automate is what will happen without anyone asking, Fleet is
 * what it runs on, Govern is who may touch any of it.
 */

export interface NavItem {
  /** Router path. `end` matching is derived from this being the root. */
  to: string;
  label: string;
  /**
   * A short glyph for the collapsed rail, where there is no room for the label.
   * Text rather than icon components: the rail is a fallback for a narrow
   * viewport, and shipping an icon set to serve it would cost more than the
   * layout it rescues is worth.
   */
  glyph: string;
  /**
   * Whether the gateway must have said this credential may administer. Gating
   * lives on the ITEM so both layouts filter identically; a layout that decided
   * for itself would be a second authorization opinion.
   */
  adminOnly?: boolean;
}

export interface NavGroup {
  /** Uppercase heading in the sidebar. Dropped entirely in the navbar. */
  label: string;
  items: NavItem[];
}

export const NAV_GROUPS: NavGroup[] = [
  {
    label: 'Observe',
    items: [
      { to: '/', label: 'Overview', glyph: '◎' },
      // Runs before Workflows here, unlike the old top bar: an operator opening
      // this console mid-incident is looking for a run, not for the catalog.
      { to: '/runs', label: 'Runs', glyph: '▤' },
      { to: '/workflows', label: 'Workflows', glyph: '⌥' },
    ],
  },
  {
    label: 'Automate',
    items: [{ to: '/schedules', label: 'Schedules', glyph: '◷' }],
  },
  {
    label: 'Fleet',
    items: [{ to: '/workers', label: 'Workers', glyph: '▣' }],
  },
  {
    label: 'Govern',
    items: [
      { to: '/admin/users', label: 'Users', glyph: '◍', adminOnly: true },
      { to: '/admin/tokens', label: 'Tokens', glyph: '⚿', adminOnly: true },
      { to: '/admin/audit', label: 'Audit', glyph: '❋', adminOnly: true },
    ],
  },
];

/**
 * The groups this credential may see, with empty groups dropped.
 *
 * Returning groups rather than a flat list keeps the sidebar's headings and the
 * navbar's row derived from the same filter: the navbar simply flattens what
 * comes back. A group whose every item was gated away contributes no heading,
 * so a non-admin never sees an empty "Govern" label hinting at what they are
 * missing.
 */
export function visibleGroups(canAdmin: boolean): NavGroup[] {
  return NAV_GROUPS.map((g) => ({
    ...g,
    items: g.items.filter((i) => !i.adminOnly || canAdmin),
  })).filter((g) => g.items.length > 0);
}

/** The same destinations as one ordered list, for the flattened navbar. */
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
