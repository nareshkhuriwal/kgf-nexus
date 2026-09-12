// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The credential model.
 *
 * The property these protect is the one the brief calls out: adding sessions
 * must not break API-key clients. The console holds one credential at a time and
 * a session takes precedence, but removing the session must fall back to the key
 * rather than leaving the console unauthenticated.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { currentCredential, getSession, setSession, type StoredSession } from './auth';
import { setApiKey } from './client';

const liveSession: StoredSession = {
  token: 'nxsess_abc',
  expiresAt: new Date(Date.now() + 3_600_000).toISOString(),
  email: 'ops@example.com',
  namespace: 'default',
  role: 'admin',
};

beforeEach(() => {
  window.localStorage.clear();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('session storage', () => {
  it('round-trips a session', () => {
    expect(setSession(liveSession)).toBe(true);
    expect(getSession()?.token).toBe('nxsess_abc');
    expect(getSession()?.email).toBe('ops@example.com');
  });

  it('drops a locally-expired session rather than presenting it', () => {
    // The server would refuse it anyway, but presenting one guarantees a 401 on
    // first load, which reads as "my login is broken" rather than "it ended".
    setSession({ ...liveSession, expiresAt: new Date(Date.now() - 1000).toISOString() });
    expect(getSession()).toBeNull();
  });

  it('survives malformed stored JSON', () => {
    window.localStorage.setItem('nexus.session', '{not json');
    expect(getSession()).toBeNull();
  });

  it('ignores a stored object with no token', () => {
    window.localStorage.setItem('nexus.session', JSON.stringify({ email: 'x@y.com' }));
    expect(getSession()).toBeNull();
  });

  it('reports when a session could not be persisted', () => {
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('site data blocked');
    });
    expect(setSession(liveSession)).toBe(false);
    spy.mockRestore();
  });
});

describe('which credential is presented', () => {
  it('uses the API key when there is no session', () => {
    // This is the pre-existing behaviour and the CLI-parity escape hatch. It must
    // keep working exactly as it did before sessions existed.
    setApiKey('nxs_admin_legacy');
    const cred = currentCredential();
    expect(cred.kind).toBe('api_key');
    expect(cred.token).toBe('nxs_admin_legacy');
  });

  it('prefers a session over a stored API key', () => {
    // Signing in is the more specific and more recent act: someone who logs in
    // means to act as themselves, even on a browser with a key saved from before.
    setApiKey('nxs_admin_legacy');
    setSession(liveSession);
    const cred = currentCredential();
    expect(cred.kind).toBe('session');
    expect(cred.token).toBe('nxsess_abc');
  });

  it('falls BACK to the API key when the session is cleared', () => {
    // The key is not discarded on logout: clearing a session must not silently
    // throw away a credential the operator typed in separately.
    setApiKey('nxs_admin_legacy');
    setSession(liveSession);
    setSession(null);
    const cred = currentCredential();
    expect(cred.kind).toBe('api_key');
    expect(cred.token).toBe('nxs_admin_legacy');
  });

  it('reports none when there is no credential at all', () => {
    expect(currentCredential().kind).toBe('none');
    expect(currentCredential().token).toBe('');
  });

  it('falls back to the key when the session has expired', () => {
    setApiKey('nxs_admin_legacy');
    setSession({ ...liveSession, expiresAt: new Date(Date.now() - 1000).toISOString() });
    expect(currentCredential().kind).toBe('api_key');
  });
});
