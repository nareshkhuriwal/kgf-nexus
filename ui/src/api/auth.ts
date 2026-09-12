// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Identity and administration client.
 *
 * THE CREDENTIAL MODEL. The console holds exactly one credential at a time, in
 * browser storage, and presents it as a bearer token. That was already true for
 * the API key; a session token is the same shape and travels the same path,
 * which is why sessions needed no change to `request()` in client.ts. The
 * gateway decides what the credential IS by its prefix, so the console does not
 * have to — and an operator who pastes an API key keeps working exactly as
 * before (the CLI-parity escape hatch the brief requires).
 *
 * A session token is stored ALONGSIDE the key rather than replacing it, so
 * logging out cannot silently discard an API key the operator typed in.
 */

import { ApiError, apiUrl, getApiKey } from './client';

const SESSION_STORAGE = 'nexus.session';

/** How the console is currently authenticated. */
export type CredentialKind = 'session' | 'api_key' | 'none';

export interface StoredSession {
  token: string;
  expiresAt: string;
  email: string;
  /**
   * The person's name as the gateway knows it, when it sent one.
   *
   * Kept because the avatar wants real initials: "NK" from "Naresh Khuriwal"
   * identifies a person, while a letter derived from an email's local part is a
   * guess that happens to be right often enough to look deliberate. Optional
   * because it is the server's to supply and a session stored by an older build
   * will not have it -- getSession parses whatever is in localStorage, so every
   * field added here has to tolerate being absent.
   */
  displayName?: string;
  namespace: string;
  role: string;
}

export interface Principal {
  namespace: string;
  name: string;
  role: string;
  kind: string;
  user_id?: string;
  scopes?: string[];
  /**
   * What this credential may actually do, as computed BY THE GATEWAY.
   *
   * The console never re-derives the authorization matrix from the role name: a
   * second copy of it is a second opinion, and it will eventually disagree with
   * the enforcement point. Rendering a button the server will refuse is worse
   * than not rendering it.
   */
  permissions: Record<string, boolean>;
}

/** The stored session, or null. Expiry is checked so a stale token is not presented. */
export function getSession(): StoredSession | null {
  try {
    const raw = window.localStorage.getItem(SESSION_STORAGE);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as StoredSession;
    if (!parsed?.token) return null;
    // A locally-expired token is dropped rather than sent. The server would
    // reject it anyway, but presenting one guarantees a 401 on first load, which
    // reads to the operator as "my login broke" rather than "my session ended".
    if (parsed.expiresAt && Date.parse(parsed.expiresAt) < Date.now()) return null;
    return parsed;
  } catch {
    return null;
  }
}

export function setSession(s: StoredSession | null): boolean {
  try {
    if (s) window.localStorage.setItem(SESSION_STORAGE, JSON.stringify(s));
    else window.localStorage.removeItem(SESSION_STORAGE);
    return true;
  } catch {
    // Private browsing or blocked site data. The caller reports this rather than
    // pretending the login persisted.
    return false;
  }
}

/**
 * The credential to present, session first.
 *
 * Session takes precedence over a pasted API key because it is the more specific
 * and more recent act: someone who logs in means to act as themselves, even on a
 * browser that still has a key saved from before.
 */
export function currentCredential(): { token: string; kind: CredentialKind } {
  const session = getSession();
  if (session) return { token: session.token, kind: 'session' };
  const key = getApiKey();
  if (key) return { token: key, kind: 'api_key' };
  return { token: '', kind: 'none' };
}

function authHeader(): Record<string, string> {
  const { token } = currentCredential();
  return token ? { Authorization: `Bearer ${token}` } : {};
}

/**
 * A request that carries whichever credential is current.
 *
 * Deliberately a small duplicate of client.ts's `request` rather than an export
 * from it: client.ts reads the API key directly, and making it aware of sessions
 * would put the credential decision in two places. This is the one place that
 * decision lives.
 */
async function authRequest<T>(path: string, init?: RequestInit): Promise<T> {
  const url = apiUrl(path);
  let res: Response;
  try {
    res = await fetch(url, {
      ...init,
      headers: {
        Accept: 'application/json',
        ...authHeader(),
        ...(init?.headers ?? {}),
      },
    });
  } catch (cause) {
    throw new ApiError(`Cannot reach the Nexus API at ${url || '/'}. Is the api-gateway running?`, {
      status: 0,
      url,
      body: String(cause),
    });
  }

  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new ApiError(authMessage(res.status, body), { status: res.status, url, body });
  }
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (text.trim() === '') return undefined as T;
  try {
    return JSON.parse(text) as T;
  } catch {
    throw new ApiError(`Server returned non-JSON (${res.status}) from ${url}.`, {
      status: res.status,
      url,
      body: text.slice(0, 400),
    });
  }
}

/**
 * Error text for the identity surface.
 *
 * Prefers the server's own message, including the `problems` list the gateway
 * returns for a 422 — the whole reason it reports every problem at once is so a
 * form can show them all rather than one per round trip.
 */
function authMessage(status: number, body: string): string {
  try {
    const parsed = JSON.parse(body) as { message?: string; error?: string; problems?: string[] };
    if (Array.isArray(parsed.problems) && parsed.problems.length > 0) {
      return parsed.problems.join('; ');
    }
    const msg = parsed.message ?? parsed.error;
    if (msg) return msg;
  } catch {
    /* not JSON; fall through */
  }
  if (status === 401) return 'Invalid email or password.';
  if (status === 403) return 'Your credential is valid but not permitted to do this.';
  if (status === 503) return 'Authentication is temporarily unavailable. Try again shortly.';
  return `Request failed (HTTP ${status}).`;
}

// ── wire shapes ───────────────────────────────────────────────────────────

export interface User {
  id: string;
  namespace: string;
  email: string;
  display_name: string;
  role: string;
  created_at: string;
  disabled_at: string | null;
  disabled: boolean;
}

export interface ScopedToken {
  id: string;
  namespace: string;
  name: string;
  scopes: string[];
  role: string;
  expires_at: string | null;
  last_used_at: string | null;
  created_by: string | null;
  created_at: string;
  revoked_at: string | null;
  live: boolean;
  /** Present ONLY in the create response, and never again. */
  token?: string;
}

export interface AuditEntry {
  id: number;
  namespace: string;
  actor_id: string | null;
  actor_kind: string;
  actor_label: string;
  action: string;
  target_kind: string;
  target_id: string;
  outcome: string;
  detail: unknown;
  client_ip: string;
  occurred_at: string;
}

export interface UserSession {
  id: string;
  created_at: string;
  expires_at: string;
  revoked_at: string | null;
  last_used_at: string | null;
  user_agent: string;
  client_ip: string;
  live: boolean;
}

const ns = (namespace: string) => `/v1/namespaces/${encodeURIComponent(namespace)}`;

function list<T>(payload: unknown, key: string): T[] {
  if (Array.isArray(payload)) return payload as T[];
  if (payload && typeof payload === 'object') {
    const v = (payload as Record<string, unknown>)[key];
    if (Array.isArray(v)) return v as T[];
  }
  return [];
}

export const authApi = {
  /**
   * Exchange an email and password for a session token.
   *
   * Does NOT store it — the caller decides, because storing a credential is a
   * side effect the component that owns the login flow should perform
   * explicitly rather than have happen inside a fetch helper.
   */
  async login(namespace: string, email: string, password: string): Promise<StoredSession> {
    const res = await authRequest<{
      token: string;
      expires_at: string;
      user: { email: string; namespace: string; role: string; display_name?: string };
    }>('/v1/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ namespace, email, password }),
    });
    return {
      token: res.token,
      expiresAt: res.expires_at,
      email: res.user.email,
      // Dropped on the floor until now, which is why the avatar had only an
      // email to work from.
      displayName: res.user.display_name,
      namespace: res.user.namespace,
      role: res.user.role,
    };
  },

  /**
   * End the current session server-side, then locally.
   *
   * The local token is cleared even if the server call fails: the operator asked
   * to be logged out of this browser, and leaving a usable credential behind
   * because the network blipped is the opposite of what they asked for.
   */
  async logout(): Promise<void> {
    try {
      await authRequest<void>('/v1/auth/logout', { method: 'POST' });
    } finally {
      setSession(null);
    }
  },

  whoami(): Promise<Principal> {
    return authRequest<Principal>('/v1/auth/whoami');
  },

  async listUsers(namespace: string): Promise<User[]> {
    return list<User>(await authRequest<unknown>(`${ns(namespace)}/users`), 'users');
  },

  createUser(
    namespace: string,
    input: { email: string; display_name: string; role: string; password: string },
  ): Promise<User> {
    return authRequest<User>(`${ns(namespace)}/users`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    });
  },

  setUserDisabled(namespace: string, userId: string, disabled: boolean): Promise<User> {
    const verb = disabled ? 'disable' : 'enable';
    return authRequest<User>(`${ns(namespace)}/users/${encodeURIComponent(userId)}/${verb}`, {
      method: 'POST',
    });
  },

  resetPassword(
    namespace: string,
    userId: string,
    password: string,
  ): Promise<{ user_id: string; sessions_revoked: number }> {
    return authRequest(`${ns(namespace)}/users/${encodeURIComponent(userId)}/password`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    });
  },

  revokeUserSessions(
    namespace: string,
    userId: string,
  ): Promise<{ user_id: string; sessions_revoked: number }> {
    return authRequest(`${ns(namespace)}/users/${encodeURIComponent(userId)}/sessions/revoke`, {
      method: 'POST',
    });
  },

  async listUserSessions(namespace: string, userId: string): Promise<UserSession[]> {
    const payload = await authRequest<unknown>(
      `${ns(namespace)}/users/${encodeURIComponent(userId)}/sessions`,
    );
    return list<UserSession>(payload, 'sessions');
  },

  async listTokens(namespace: string): Promise<ScopedToken[]> {
    return list<ScopedToken>(await authRequest<unknown>(`${ns(namespace)}/tokens`), 'tokens');
  },

  createToken(
    namespace: string,
    input: { name: string; role: string; scopes: string[]; expires_in_days: number },
  ): Promise<ScopedToken> {
    return authRequest<ScopedToken>(`${ns(namespace)}/tokens`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    });
  },

  revokeToken(namespace: string, tokenId: string): Promise<{ token_id: string }> {
    return authRequest(`${ns(namespace)}/tokens/${encodeURIComponent(tokenId)}/revoke`, {
      method: 'POST',
    });
  },

  async listAudit(
    namespace: string,
    filter: { action?: string; outcome?: string; limit?: number } = {},
  ): Promise<AuditEntry[]> {
    const q = new URLSearchParams();
    if (filter.action) q.set('action', filter.action);
    if (filter.outcome) q.set('outcome', filter.outcome);
    if (filter.limit) q.set('limit', String(filter.limit));
    const qs = q.toString();
    const payload = await authRequest<unknown>(`${ns(namespace)}/audit${qs ? `?${qs}` : ''}`);
    return list<AuditEntry>(payload, 'entries');
  },
};
