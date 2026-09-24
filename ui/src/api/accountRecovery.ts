// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Email verification and password recovery.
 *
 * THE TOKEN NEVER TOUCHES A URL THE SERVER SEES. The gateway puts it in the
 * link's FRAGMENT (`/reset-password#token=…`). Browsers do not send fragments in
 * requests, so it is absent from nginx's access log and from any Referer header.
 * The page reads it once, removes it from the address bar and history, and sends
 * it to the gateway in a POST body (see useLinkToken).
 */

import { ApiError } from './client';
import { authRequest } from './auth';

export interface LinkAccepted {
  status: 'accepted';
  message: string;
}

const json = (body: unknown): RequestInit => ({
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
});

export const recoveryApi = {
  /** Ask for a new verification link. Same answer whether or not the address has an account. */
  requestVerification(email: string): Promise<LinkAccepted> {
    return authRequest<LinkAccepted>('/v1/auth/email/verification', json({ email }));
  },
  verifyEmail(token: string): Promise<{ verified: boolean; email: string }> {
    return authRequest('/v1/auth/email/verify', json({ token }));
  },
  /** Ask for a reset link. Same answer whether or not the address has an account. */
  forgotPassword(email: string): Promise<LinkAccepted> {
    return authRequest<LinkAccepted>('/v1/auth/password/forgot', json({ email }));
  },
  resetPassword(token: string, password: string): Promise<{ password_reset: boolean; sessions_revoked: number }> {
    return authRequest('/v1/auth/password/reset', json({ token, password }));
  },
};

/**
 * The machine-readable error code the gateway sent (`invalid_token`,
 * `email_not_verified`, `email_unavailable`, `rate_limited`…), or null.
 *
 * Pages branch on the CODE, never on message text, so rewording a message on
 * the server cannot silently change which state a page shows.
 */
export function errorCode(err: unknown): string | null {
  if (!(err instanceof ApiError)) return null;
  try {
    const parsed = JSON.parse(err.body) as { error?: unknown };
    return typeof parsed.error === 'string' ? parsed.error : null;
  } catch {
    return null;
  }
}

/** The link token in the URL fragment, or null. Pure: reads, never writes. */
export function readLinkToken(win: Window = window): string | null {
  const hash = win.location.hash.replace(/^#/, '');
  if (!hash) return null;
  const token = new URLSearchParams(hash).get('token')?.trim();
  return token ? token : null;
}

/**
 * Erase the fragment from the address bar.
 *
 * With replaceState, which rewrites the CURRENT history entry: Back does not
 * bring the token back, a screenshot of the tab does not show it, and a bookmark
 * or shared URL taken afterwards does not carry it. Kept separate from the read
 * so the read can run in a render (React may run it twice) and this in an effect.
 */
export function eraseLinkFragment(win: Window = window): void {
  if (!win.location.hash) return;
  try {
    win.history.replaceState(win.history.state, '', win.location.pathname + win.location.search);
  } catch {
    // A sandboxed frame can refuse; the token is still only in the fragment.
  }
}

/** Shortest password the gateway accepts (NIST 800-63B length rule, as elsewhere). */
export const MIN_PASSWORD_LENGTH = 12;
