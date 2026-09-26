// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The destination list is the one thing both layouts share, so what is pinned
 * here is the contract they both rely on: that admin gating happens once, that
 * an emptied group leaves no orphan heading, and that flattening preserves
 * order. If these hold, neither layout can invent or lose a destination.
 */

import { describe, expect, it } from 'vitest';
import { NAV_GROUPS, isEndMatch, visibleGroups, visibleItems } from './navItems';

describe('the shared destination list', () => {
  it('hides every admin destination from a credential without admin_write', () => {
    const paths = visibleItems(false).map((i) => i.to);

    expect(paths).not.toContain('/admin/users');
    expect(paths).not.toContain('/admin/tokens');
    expect(paths).not.toContain('/admin/audit');
    // The non-admin destinations are untouched by the gate.
    expect(paths).toEqual([
      '/',
      '/runs',
      '/workflows',
      '/assets',
      '/schedules',
      '/triggers',
      '/sensors',
      '/notifications',
      '/workers',
    ]);
  });

  it('shows them to an admin', () => {
    const paths = visibleItems(true).map((i) => i.to);
    expect(paths).toContain('/admin/users');
    expect(paths).toContain('/admin/tokens');
    expect(paths).toContain('/admin/audit');
  });

  /**
   * A heading with nothing under it would advertise to a reader exactly which
   * capability they lack, and look like a rendering fault besides.
   */
  it('drops a group whose every item was gated away', () => {
    expect(visibleGroups(false).map((g) => g.label)).not.toContain('Govern');
    expect(visibleGroups(true).map((g) => g.label)).toContain('Govern');
  });

  it('flattens to exactly the grouped items, in the same order', () => {
    for (const canAdmin of [false, true]) {
      const flat = visibleItems(canAdmin);
      const fromGroups = visibleGroups(canAdmin).flatMap((g) => g.items);
      expect(flat).toEqual(fromGroups);
    }
  });

  /**
   * `end` on the root only. Applied to everything, "/runs/abc" would leave Runs
   * looking inactive while you are inside it; applied to nothing, Overview
   * would stay lit on every page.
   */
  it('uses exact matching for the root and prefix matching elsewhere', () => {
    for (const item of visibleItems(true)) {
      expect(isEndMatch(item)).toBe(item.to === '/');
    }
  });

  it('gives every destination a distinct path, a label and an icon', () => {
    const items = NAV_GROUPS.flatMap((g) => g.items);
    expect(new Set(items.map((i) => i.to)).size).toBe(items.length);
    for (const i of items) {
      expect(i.label).not.toBe('');
      // An icon COMPONENT, not a text glyph. The collapsed rail shows the icon
      // as the primary way a destination is recognised, so a missing one is not
      // a cosmetic gap there — it is an unidentifiable row.
      expect(typeof i.icon).toBe('function');
    }
  });

  /**
   * Distinct icons, not merely present ones.
   *
   * The glyphs this replaced were abstract symbols reused loosely, and two
   * destinations sharing an icon in a collapsed rail is worse than either
   * having none: it actively asserts they are the same kind of thing.
   */
  it('gives no two destinations the same icon', () => {
    const icons = NAV_GROUPS.flatMap((g) => g.items).map((i) => i.icon);
    expect(new Set(icons).size).toBe(icons.length);
  });
});
