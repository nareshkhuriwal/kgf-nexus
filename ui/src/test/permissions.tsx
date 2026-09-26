// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Permission fixtures for component tests.
 *
 * The console reads effective permissions from PermissionContext and renders
 * nothing it has not been granted. That is the right production behaviour, and
 * it means a test that renders a permission-aware component WITHOUT a provider
 * gets an empty shell: no links, no commands, no rows. The assertion then fails
 * with "unable to find role link", which reads like a markup bug and sends you
 * looking in the wrong file.
 *
 * So tests state the permissions they are testing under, and these are the two
 * sets almost every test wants.
 */

import type { ReactNode } from 'react';
import { PermissionContext } from '../components/Permissions';
import { NAV_GROUPS } from '../components/navItems';
import { permissionForPath } from '../components/Permissions';
import catalog from './permission-catalog.json';

/**
 * Every `<module>.view` the navigation can ask for, derived from the nav
 * declaration rather than typed out again here -- a hand-maintained list would
 * be a second copy of NAV_GROUPS and would drift the first time a destination
 * is added.
 */
export const ALL_VIEW_PERMISSIONS: readonly string[] = Object.freeze(
  Array.from(
    new Set(
      NAV_GROUPS.flatMap((g) => g.items)
        .map((i) => permissionForPath(i.to))
        .filter((p): p is string => Boolean(p)),
    ),
  ),
);

/**
 * The exact permission catalog -- ModuleActions in
 * services/api-gateway/authn/rbac.go, which is what Administrator holds.
 *
 * Read from permission-catalog.json, which the gateway's own test
 * (TestUIPermissionCatalogMatchesModuleActions) keeps byte-identical to
 * ModuleActions: add a permission on the server without regenerating it and
 * the Go suite fails. A hand-typed copy here drifted (F157): it lacked
 * workers.quarantine, audit.manage and workflows.cache.invalidate, so the
 * "Administrator" fixture could not render the quarantine controls at all.
 *
 * Not derived from the nav, because the nav only yields `<module>.view` and the
 * console gates ACTIONS too: a bulk cancel bar asks for runs.cancel, a delete
 * row action for runs.delete. A fixture with view permissions alone renders
 * every list and no control on it, which fails a bulk-action test with "unable
 * to find button" -- the same misleading shape as having no provider at all.
 */
export const MODULE_ACTIONS: Readonly<Record<string, readonly string[]>> = Object.freeze(catalog.modules);

/** Everything: what the Administrator role resolves to. The default for tests. */
export const ADMIN_PERMISSIONS: readonly string[] = Object.freeze(
  Object.entries(MODULE_ACTIONS)
    .flatMap(([module, actions]) => actions.map((a) => `${module}.${a}`))
    .sort(),
);

/**
 * Every view permission EXCEPT those behind an adminOnly destination.
 *
 * Derived from the `adminOnly` flag rather than a hand-typed list of module
 * names: a typed list silently stops excluding a destination the moment one is
 * added, and the test that asserts "an operator without admin sees no admin
 * links" would start passing for the wrong reason.
 */
export const NON_ADMIN_PERMISSIONS: readonly string[] = Object.freeze(
  Array.from(
    new Set(
      NAV_GROUPS.flatMap((g) => g.items)
        .filter((i) => !i.adminOnly)
        .map((i) => permissionForPath(i.to))
        .filter((p): p is string => Boolean(p)),
    ),
  ),
);

/** Granted nothing: what a test asserting a restriction renders under. */
export const NO_PERMISSIONS: readonly string[] = Object.freeze([]);

/**
 * Wraps `ui` in a permission provider.
 *
 * The value is passed straight through rather than defaulted inline, because a
 * fresh array literal per render is what made CommandPalette spin: `permissions`
 * is an effect dependency, so a new identity each render reran the effect, which
 * set state, which rendered again. Pass one of the frozen constants above, or
 * a value the caller holds stable itself.
 */
export function WithPermissions({
  children,
  permissions = ADMIN_PERMISSIONS,
}: {
  children: ReactNode;
  permissions?: readonly string[];
}) {
  return <PermissionContext.Provider value={permissions}>{children}</PermissionContext.Provider>;
}
