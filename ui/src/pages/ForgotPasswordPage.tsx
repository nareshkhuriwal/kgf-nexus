// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Forgot password: ask for a reset link.
 *
 * The success state repeats the gateway's own sentence ("If an account with that
 * email address exists…") rather than "We sent you a link". The gateway answers
 * identically for every address so that this form cannot be used to discover who
 * has an account; a page that then said something more specific would undo that.
 *
 * The one failure that IS specific is email_unavailable: this deployment has no
 * mail server. That is true for every address, so saying it leaks nothing, and
 * hiding it would leave a locked-out person watching an inbox that will stay empty.
 */

import { useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { AuthShell, Notice, useFocusOnChange } from '../components/AuthShell';
import { errorCode, recoveryApi } from '../api/accountRecovery';
import { errorMessage } from '../api/hooks';

export function ForgotPasswordPage() {
  const [email, setEmail] = useState('');
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState<string | null>(null);
  const [error, setError] = useState<{ code: string | null; message: string } | null>(null);
  const resultRef = useFocusOnChange<HTMLHeadingElement>(sent);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const res = await recoveryApi.forgotPassword(email.trim());
      setSent(res.message);
    } catch (err) {
      setError({ code: errorCode(err), message: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  }

  return (
    <AuthShell title="Reset your password" subtitle="We will email you a link to choose a new one">
      {sent ? (
        <>
          <h2 className="auth-result" tabIndex={-1} ref={resultRef}>
            Check your email
          </h2>
          <Notice tone="ok" title="Request received">
            {sent}
          </Notice>
          <p className="auth-hint">
            The link works once and expires soon. It can take a minute to arrive; check spam too. Nothing arriving? You
            can ask again in a few minutes.
          </p>
          <p className="auth-links">
            <button type="button" className="login-submit" onClick={() => setSent(null)}>
              Use a different address
            </button>
          </p>
          <p className="auth-links">
            <Link to="/login">Back to sign in</Link>
          </p>
        </>
      ) : (
        <>
          <form className="login-form" onSubmit={submit} aria-busy={busy}>
            <label className="form-field">
              <span className="form-label">email</span>
              <input
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                autoComplete="email"
                required
                autoFocus
              />
            </label>
            <button type="submit" className="primary login-submit" disabled={busy}>
              {busy ? 'Sending…' : 'Email me a reset link'}
            </button>
          </form>
          {error && (
            <Notice
              tone="error"
              title={error.code === 'email_unavailable' ? 'Email is not set up here' : 'Could not send a reset link'}
            >
              {error.message}
            </Notice>
          )}
          <p className="auth-links">
            <Link to="/login">Back to sign in</Link>
            <Link to="/register">Create a workspace</Link>
          </p>
        </>
      )}
    </AuthShell>
  );
}
