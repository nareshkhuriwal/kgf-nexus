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
  effective_permissions?: string[];
  roles?: string[];
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
export async function authRequest<T>(path: string, init?: RequestInit): Promise<T> {
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

/** One page of the users listing. */
export interface UsersPage {
  users: User[];
  next_cursor: string | null;
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
  /** The service account this token belongs to (ADR-0024 §6), if any. */
  service_account_id?: string | null;
  /** The token's permission ceiling; null when it has none. */
  permissions?: string[] | null;
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
      body: JSON.stringify({ ...(namespace ? {namespace} : {}), email, password }),
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

  /**
   * The namespace's users.
   *
   * `limit` is the ONLY knob this endpoint has -- there is no search parameter
   * on the wire -- so a caller that wants to find a person by name or email has
   * to fetch a capped page and match in the browser. The parameter is optional
   * and unsent when absent, so the admin page keeps asking for exactly what it
   * asked for before (the server's own default) and only the global search
   * imposes a cap. Sending a cap matters there because the caller needs to KNOW
   * it was capped, and the only way to know is to have chosen the number.
   */
  async listUsers(namespace: string, limit?: number): Promise<User[]> {
    const q = limit !== undefined ? `?limit=${limit}` : '';
    return list<User>(await authRequest<unknown>(`${ns(namespace)}/users${q}`), 'users');
  },

  /**
   * One keyset page of users (ADR-0024 §6): newest first, at most 200, with
   * `q` matched against email and display name ON THE SERVER. `next_cursor`
   * is null on the last page; pass it back to get the next one.
   */
  async listUsersPage(
    namespace: string,
    opts: { limit?: number; cursor?: string | null; q?: string } = {},
  ): Promise<UsersPage> {
    const params = new URLSearchParams();
    if (opts.limit) params.set('limit', String(opts.limit));
    if (opts.cursor) params.set('cursor', opts.cursor);
    if (opts.q) params.set('q', opts.q);
    const qs = params.toString();
    const res = await authRequest<{ users?: User[]; next_cursor?: string | null }>(
      `${ns(namespace)}/users${qs ? `?${qs}` : ''}`,
    );
    return { users: Array.isArray(res?.users) ? res.users : [], next_cursor: res?.next_cursor ?? null };
  },

  createUser(
    namespace: string,
    input: { email: string; display_name: string; role: string; role_name?: string; password: string },
  ): Promise<User> {
    return authRequest<User>(`${ns(namespace)}/users`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    });
  },

  updateUser(namespace: string, user: User, input: {email: string; display_name: string}): Promise<User> {
    return authRequest<User>(`${ns(namespace)}/users/${encodeURIComponent(user.id)}`, {
      method: 'PUT', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({...input, previous_email: user.email, previous_display_name: user.display_name}),
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

  /**
   * One page of the trail, newest first, and the keyset cursor to the next
   * (older) page; `nextCursor` is null at the end of the trail.
   */
  async listAuditPage(namespace: string, filter: AuditFilter & { limit?: number; cursor?: string } = {}): Promise<AuditPage> {
    const q = auditQuery(filter);
    if (filter.limit) q.set('limit', String(filter.limit));
    if (filter.cursor) q.set('cursor', filter.cursor);
    const qs = q.toString();
    const payload = await authRequest<unknown>(`${ns(namespace)}/audit${qs ? `?${qs}` : ''}`);
    const next = payload && typeof payload === 'object' ? (payload as { next_cursor?: unknown }).next_cursor : null;
    return { entries: list<AuditEntry>(payload, 'entries'), nextCursor: typeof next === 'string' && next !== '' ? next : null };
  },

  /**
   * The server's export of every entry matching the filter (not one page):
   * NDJSON carries the hash-chain header and trailer `nexusctl audit verify`
   * checks, CSV is for spreadsheets and SIEM import, KGF is KGF Cloud's event
   * shape. Fetched rather than linked because the export needs the credential
   * header, which a plain link cannot send.
   */
  async exportAudit(namespace: string, format: AuditExportFormat, filter: AuditFilter = {}): Promise<AuditExport> {
    const q = auditQuery(filter);
    q.set('format', format);
    const url = apiUrl(`${ns(namespace)}/audit/export?${q.toString()}`);
    let res: Response;
    try {
      res = await fetch(url, { headers: { Accept: EXPORT_TYPES[format], ...authHeader() } });
    } catch (cause) {
      throw new ApiError(`Cannot reach the Nexus API at ${url || '/'}. Is the api-gateway running?`, { status: 0, url, body: String(cause) });
    }
    if (!res.ok) {
      const body = await res.text().catch(() => '');
      throw new ApiError(authMessage(res.status, body), { status: res.status, url, body });
    }
    const text = await res.text();
    const disposition = res.headers.get('Content-Disposition') ?? '';
    const named = /filename="?([^";]+)"?/.exec(disposition)?.[1];
    const stamp = new Date().toISOString().replace(/[:.]/g, '-');
    return {
      text,
      contentType: res.headers.get('Content-Type') ?? EXPORT_TYPES[format],
      filename: named ?? `audit-${namespace}-${stamp}${EXPORT_EXTENSIONS[format]}`,
    };
  },
};

export interface AuditFilter {
  action?: string;
  outcome?: string;
}

export interface AuditPage {
  entries: AuditEntry[];
  nextCursor: string | null;
}

export type AuditExportFormat = 'ndjson' | 'csv' | 'kgf';

export interface AuditExport {
  text: string;
  contentType: string;
  filename: string;
}

const EXPORT_TYPES: Record<AuditExportFormat, string> = {
  ndjson: 'application/x-ndjson',
  csv: 'text/csv',
  kgf: 'application/x-ndjson',
};

const EXPORT_EXTENSIONS: Record<AuditExportFormat, string> = {
  ndjson: '.ndjson',
  csv: '.csv',
  kgf: '.kgf.ndjson',
};

function auditQuery(filter: AuditFilter): URLSearchParams {
  const q = new URLSearchParams();
  if (filter.action) q.set('action', filter.action);
  if (filter.outcome) q.set('outcome', filter.outcome);
  return q;
}

/**
 * Whether an NDJSON export ends with its trailer. The gateway streams it, so a
 * failure after the first row cannot change the status code; the missing
 * trailer is the only sign the file stops early (the verifier reports it as
 * truncated).
 */
export function ndjsonExportComplete(text: string): boolean {
  const lines = text.trimEnd().split('\n');
  const last = lines[lines.length - 1];
  if (!last) return false;
  try {
    return (JSON.parse(last) as { type?: unknown }).type === 'trailer';
  } catch {
    return false;
  }
}
