// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Service accounts (ADR-0024 §6).
 *
 * A service account is a named, non-human principal that holds roles. It has
 * no credential of its own: tokens are minted FOR it through /tokens, each with
 * a permission ceiling, and a token's effective grants are the account's
 * CURRENT roles intersected with that ceiling. Narrowing the roles or disabling
 * the account therefore takes effect on the next request of every token it
 * has, which is the reason to use one instead of a free-standing token.
 */

import { authRequest, type ScopedToken } from './auth';

export interface ServiceAccount {
  id: string;
  namespace: string;
  name: string;
  description: string;
  disabled: boolean;
  disabled_at: string | null;
  created_at: string;
  revision: number;
  roles: string[];
  /** Tokens for this account that are neither revoked nor expired. */
  live_tokens: number;
}

const base = (namespace: string) => `/v1/namespaces/${encodeURIComponent(namespace)}/service-accounts`;
const json = (method: string, body: unknown): RequestInit => ({
  method,
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
});

export const serviceAccountsApi = {
  async list(namespace: string): Promise<ServiceAccount[]> {
    const res = await authRequest<{ service_accounts?: ServiceAccount[] }>(base(namespace));
    return Array.isArray(res?.service_accounts) ? res.service_accounts : [];
  },
  create(namespace: string, input: { name: string; description: string; roles: string[] }): Promise<ServiceAccount> {
    return authRequest(base(namespace), json('POST', input));
  },
  update(namespace: string, id: string, description: string, revision: number): Promise<ServiceAccount> {
    return authRequest(`${base(namespace)}/${encodeURIComponent(id)}`, json('PUT', { description, revision }));
  },
  setDisabled(namespace: string, id: string, disabled: boolean): Promise<ServiceAccount> {
    return authRequest(`${base(namespace)}/${encodeURIComponent(id)}/${disabled ? 'disable' : 'enable'}`, { method: 'POST' });
  },
  getRoles(namespace: string, id: string): Promise<{ roles: string[]; revision: number }> {
    return authRequest(`${base(namespace)}/${encodeURIComponent(id)}/roles`);
  },
  putRoles(namespace: string, id: string, roles: string[], revision: number): Promise<{ roles: string[]; revision: number }> {
    return authRequest(`${base(namespace)}/${encodeURIComponent(id)}/roles`, json('PUT', { roles, revision }));
  },
  /**
   * Mint a token for an account, with a permission ceiling. The secret is in
   * the response ONCE (`token`) and never again.
   */
  mintToken(
    namespace: string,
    input: { name: string; service_account_id: string; permissions: string[]; expires_in_days: number },
  ): Promise<ScopedToken> {
    return authRequest(`/v1/namespaces/${encodeURIComponent(namespace)}/tokens`, json('POST', input));
  },
};

/** A role as GET /roles describes it, which is all the grant arithmetic needs. */
export interface RoleGrants {
  name: string;
  permissions: string[];
  builtin?: boolean;
  assignable?: boolean;
  revision?: number;
}

/**
 * The union of the named roles' grants, sorted.
 *
 * Used only to OFFER a sensible default (every grant the account holds) in the
 * mint form. The gateway recomputes it and is the one that refuses a ceiling
 * outside it; this is a convenience, not a second authorization opinion.
 */
export function grantsOf(roleNames: readonly string[], roles: readonly RoleGrants[]): string[] {
  const wanted = new Set(roleNames);
  return [...new Set(roles.filter((r) => wanted.has(r.name)).flatMap((r) => r.permissions))].sort();
}
