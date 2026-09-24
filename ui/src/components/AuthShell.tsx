// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The frame for the account pages that sit outside the console chrome: verify
 * email, forgot password, reset password.
 *
 * It reuses the sign-in page's own classes -- the same column width, brand block,
 * panel, field and notice styles -- so these screens read as the same product as
 * the one the person just left, in both themes. Only the brand panel is dropped:
 * these are single-task screens reached from an email, and the task is the page.
 */

import { useEffect, useRef, useState, type ReactNode } from 'react';
import { Logo } from './Logo';
import { errorMessage } from '../api/hooks';
import { eraseLinkFragment, readLinkToken, recoveryApi } from '../api/accountRecovery';
import '../pages/accountRecovery.css';

export function AuthShell({ title, subtitle, children }: { title: string; subtitle: string; children: ReactNode }) {
  return (
    <div className="login-page auth-single">
      <main className="login-main">
        <div className="login-col">
          <div className="login-brand">
            <Logo size={36} className="login-mark" />
            <h1 className="login-title">{title}</h1>
            <div className="login-sub">{subtitle}</div>
          </div>
          <div className="panel">
            <div className="panel-body col" style={{ gap: 'var(--space-6)' }}>
              {children}
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}

/**
 * Suppress the Referer header while a page holding a link token is open.
 *
 * Belt and braces: the token is in the fragment, which browsers never put in a
 * Referer, and it is erased from the URL on first read. This covers anything the
 * page loads before that erase, and any future change that moves the token.
 */
export function useNoReferrer() {
  useEffect(() => {
    const meta = document.createElement('meta');
    meta.name = 'referrer';
    meta.content = 'no-referrer';
    document.head.appendChild(meta);
    return () => {
      meta.remove();
    };
  }, []);
}

/**
 * The token from an emailed link, captured once and then erased from the URL.
 *
 * Read in a lazy state initializer (pure, so StrictMode's double call is
 * harmless) and erased in an effect, so the token survives in component state
 * but not in the address bar, history, or anything that copies the URL.
 */
export function useLinkToken(): string | null {
  const [token] = useState(() => readLinkToken());
  useEffect(() => {
    eraseLinkFragment();
  }, []);
  return token;
}

/**
 * Move focus to an element when a page changes state, so a keyboard or
 * screen-reader user lands on the outcome rather than on a button that has just
 * disappeared.
 */
export function useFocusOnChange<T extends HTMLElement>(key: unknown) {
  const ref = useRef<T>(null);
  useEffect(() => {
    ref.current?.focus();
  }, [key]);
  return ref;
}

export function Notice({ tone, title, children }: { tone: 'error' | 'ok' | 'warning'; title: string; children?: ReactNode }) {
  const cls = tone === 'error' ? 'notice error' : tone === 'warning' ? 'notice login-warning' : 'notice auth-ok';
  return (
    <div className={cls} role={tone === 'error' ? 'alert' : 'status'}>
      <span className="notice-glyph" aria-hidden="true">
        {tone === 'ok' ? '✓' : '!'}
      </span>
      <div>
        <div className="notice-title">{title}</div>
        {children && <div className="notice-detail">{children}</div>}
      </div>
    </div>
  );
}

/**
 * "Send me a new verification link".
 *
 * The confirmation is the gateway's own sentence, which is deliberately the same
 * for every address -- this form cannot tell anyone whether an account exists,
 * so neither may its success message.
 */
export function ResendVerification({ initialEmail = '', compact = false }: { initialEmail?: string; compact?: boolean }) {
  const [email, setEmail] = useState(initialEmail);
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setSent(null);
    try {
      const res = await recoveryApi.requestVerification(email.trim());
      setSent(res.message);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="login-form" onSubmit={submit} aria-busy={busy} aria-label="Request a new verification link">
      {!(compact && initialEmail) && (
        <label className="form-field">
          <span className="form-label">email</span>
          <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="email" required />
        </label>
      )}
      <button type="submit" className={compact ? 'login-submit' : 'primary login-submit'} disabled={busy || !email.trim()}>
        {busy ? 'Sending…' : 'Send a new verification link'}
      </button>
      {sent && (
        <Notice tone="ok" title="Check your inbox">
          {sent}
        </Notice>
      )}
      {error && (
        <Notice tone="error" title="Could not send a link">
          {error}
        </Notice>
      )}
    </form>
  );
}
