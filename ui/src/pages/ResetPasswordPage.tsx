// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Reset password: the page a reset link opens.
 *
 * The token arrives in the URL fragment and is taken exactly once, on mount,
 * then erased from the address bar (useLinkToken). It lives only in component
 * state after that and is sent in a POST body.
 *
 * STATES, each honest about what happened:
 *   - no token      the link was truncated (mail clients do this); offer a new one
 *   - form          new password twice, validated locally for length and match
 *   - expired       the gateway said invalid_token: used, expired or superseded.
 *                   One state for all three, because the gateway deliberately
 *                   does not say which, and the remedy is the same
 *   - done          the password changed and EVERY session was signed out,
 *                   including this browser's -- so the stored session is cleared
 *                   here too rather than left to 401 on the next click
 */

import { useState, type FormEvent } from 'react';
import { Link } from 'react-router-dom';
import { AuthShell, Notice, useFocusOnChange, useLinkToken, useNoReferrer } from '../components/AuthShell';
import { errorCode, MIN_PASSWORD_LENGTH, recoveryApi } from '../api/accountRecovery';
import { setSession } from '../api/auth';
import { errorMessage } from '../api/hooks';

type Phase = 'form' | 'expired' | 'done';

export function ResetPasswordPage() {
  useNoReferrer();
  const token = useLinkToken();
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [busy, setBusy] = useState(false);
  const [phase, setPhase] = useState<Phase>('form');
  const [error, setError] = useState<string | null>(null);
  const [revoked, setRevoked] = useState(0);
  const resultRef = useFocusOnChange<HTMLHeadingElement>(phase);

  const tooShort = password.length > 0 && password.length < MIN_PASSWORD_LENGTH;
  const mismatch = confirm.length > 0 && confirm !== password;

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!token || password.length < MIN_PASSWORD_LENGTH || password !== confirm) return;
    setBusy(true);
    setError(null);
    try {
      const res = await recoveryApi.resetPassword(token, password);
      setRevoked(res.sessions_revoked);
      // The gateway revoked every session, this browser's included.
      setSession(null);
      setPassword('');
      setConfirm('');
      setPhase('done');
    } catch (err) {
      if (errorCode(err) === 'invalid_token') setPhase('expired');
      else setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  if (!token) {
    return (
      <AuthShell title="Reset your password" subtitle="This link is incomplete">
        <Notice tone="error" title="No reset code in this link">
          The link may have been cut short by your mail client. Request a new one and open it directly from the email.
        </Notice>
        <p className="auth-links">
          <Link to="/forgot-password">Request a new reset link</Link>
          <Link to="/login">Back to sign in</Link>
        </p>
      </AuthShell>
    );
  }

  if (phase === 'expired') {
    return (
      <AuthShell title="Reset your password" subtitle="This link can no longer be used">
        <h2 className="auth-result" tabIndex={-1} ref={resultRef}>
          Link expired or already used
        </h2>
        <p className="auth-hint">
          Reset links work once and expire quickly. Requesting a new link also cancels older ones. Your password has not
          been changed.
        </p>
        <p className="auth-links">
          <Link to="/forgot-password">Request a new reset link</Link>
          <Link to="/login">Back to sign in</Link>
        </p>
      </AuthShell>
    );
  }

  if (phase === 'done') {
    return (
      <AuthShell title="Password changed" subtitle="Sign in with your new password">
        <h2 className="auth-result" tabIndex={-1} ref={resultRef}>
          Your password has been reset
        </h2>
        <Notice tone="ok" title="Signed out everywhere">
          {revoked === 1
            ? '1 existing session was signed out.'
            : `${revoked} existing sessions were signed out.`}{' '}
          Anyone using your old password is no longer signed in.
        </Notice>
        <Link className="btn primary login-submit" to="/login">
          Sign in
        </Link>
      </AuthShell>
    );
  }

  return (
    <AuthShell title="Choose a new password" subtitle="Resetting signs you out on every device">
      <form className="login-form" onSubmit={submit} aria-busy={busy} noValidate>
        <label className="form-field">
          <span className="form-label">new password</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            minLength={MIN_PASSWORD_LENGTH}
            maxLength={1024}
            required
            autoFocus
            aria-invalid={tooShort}
            aria-describedby="reset-password-hint"
          />
        </label>
        {/* Outside the label so it describes the field rather than renaming it. */}
        <small id="reset-password-hint" className="auth-hint">
          {tooShort
            ? `${MIN_PASSWORD_LENGTH - password.length} more characters needed.`
            : `At least ${MIN_PASSWORD_LENGTH} characters. Length matters more than symbols.`}
        </small>
        <label className="form-field">
          <span className="form-label">confirm new password</span>
          <input
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            autoComplete="new-password"
            required
            aria-invalid={mismatch}
            aria-describedby={mismatch ? 'reset-confirm-hint' : undefined}
          />
        </label>
        {mismatch && (
          <small id="reset-confirm-hint" className="auth-hint" role="alert">
            The passwords do not match.
          </small>
        )}
        <button
          type="submit"
          className="primary login-submit"
          disabled={busy || password.length < MIN_PASSWORD_LENGTH || password !== confirm}
        >
          {busy ? 'Saving…' : 'Set new password'}
        </button>
      </form>
      {error && (
        <Notice tone="error" title="Could not reset your password">
          {error}
        </Notice>
      )}
    </AuthShell>
  );
}
