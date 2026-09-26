// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The permission fixtures against the server's catalog (F157).
 *
 * permission-catalog.json is generated from the gateway's ModuleActions and a Go
 * test fails when the two differ. These tests close the other side: the
 * Administrator fixture is exactly that catalog, and every permission the
 * console's own source gates a control on exists in it -- so a typo, or a
 * permission the server renamed, fails here instead of rendering a control no
 * role can ever see.
 */

import { describe, expect, it } from 'vitest';
import catalog from './permission-catalog.json';
import { ADMIN_PERMISSIONS, ALL_VIEW_PERMISSIONS, MODULE_ACTIONS, NON_ADMIN_PERMISSIONS } from './permissions';

const CATALOG = new Set(Object.entries(catalog.modules).flatMap(([m, actions]) => actions.map((a) => `${m}.${a}`)));

// Every non-test source file, as text.
const SOURCES = import.meta.glob(['../**/*.{ts,tsx}', '!../**/*.test.{ts,tsx}', '!../test/**'], {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>;

/** Permission literals the console gates on: `permission="x.y"`, `permission: 'x.y'`, `can('x.y')`. */
function gatedPermissions(): Map<string, string[]> {
  const found = new Map<string, string[]>();
  const patterns = [/\bpermission=["']([a-z_]+(?:\.[a-z_]+)+)["']/g, /\bpermission:\s*["']([a-z_]+(?:\.[a-z_]+)+)["']/g, /\bcan\(\s*["']([a-z_]+(?:\.[a-z_]+)+)["']\s*\)/g];
  for (const [file, text] of Object.entries(SOURCES)) {
    for (const re of patterns) {
      for (const m of text.matchAll(re)) {
        const p = m[1]!;
        found.set(p, [...(found.get(p) ?? []), file]);
      }
    }
  }
  return found;
}

describe('the permission fixtures', () => {
  it('give Administrator exactly the server catalog', () => {
    expect([...ADMIN_PERMISSIONS].sort()).toEqual([...CATALOG].sort());
    expect(MODULE_ACTIONS.workers).toContain('quarantine');
    expect(ADMIN_PERMISSIONS).toEqual(expect.arrayContaining(['workers.quarantine', 'audit.manage', 'workflows.cache.invalidate']));
  });

  it('only ever grant catalog permissions', () => {
    for (const p of [...ALL_VIEW_PERMISSIONS, ...NON_ADMIN_PERMISSIONS]) expect(CATALOG).toContain(p);
  });

  it('cover every permission a control in the console is gated on', () => {
    const gated = gatedPermissions();
    // The scan must actually see the source, or this passes vacuously.
    expect(Object.keys(SOURCES).length).toBeGreaterThan(50);
    expect(gated.has('workers.quarantine')).toBe(true);
    const unknown = [...gated].filter(([p]) => !CATALOG.has(p)).map(([p, files]) => `${p} (${files.join(', ')})`);
    expect(unknown).toEqual([]);
  });
});
