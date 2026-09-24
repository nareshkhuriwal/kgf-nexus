// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Verify email: the page a verification link opens.
 *
 * CONFIRMATION IS A CLICK, not a page load. Corporate mail scanners open links
 * to inspect them; a page that verified on load would be verified by the scanner
 * and the person would arrive at "link already used". The token sits in the
 * fragment, which scanners that only fetch never see, and the POST happens only
 * when a person presses the button.
 *
 * When the link is dead (or absent), the page offers the resend form in place,
 * because requesting a new link is the only thing the person can do next.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { AuthShell, Notice, ResendVerification, useFocusOnChange, useLinkToken, useNoReferrer } from '../components/AuthShell';
import { errorCode, recoveryApi } from '../api/accountRecovery';
import { errorMessage } from '../api/hooks';

type Phase = 'ready' | 'verified' | 'expired';

export function VerifyEmailPage() {
  useNoReferrer();
  const token = useLinkToken();
  const [phase, setPhase] = useState<Phase>('ready');
  const [busy, setBusy] = useState(false);
  const [email, setEmail] = useState('');
  const [error, setError] = useState<string | null>(null);
  const resultRef = useFocusOnChange<HTMLHeadingElement>(phase);

  async function confirm() {
    if (!token) return;
    setBusy(true);
    setError(null);
    try {
      const res = await recoveryApi.verifyEmail(token);
      setEmail(res.email);
      setPhase('verified');
    } catch (err) {
      if (errorCode(err) === 'invalid_token') setPhase('expired');
      else setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  if (phase === 'verified') {
    return (
      <AuthShell title="Email confirmed" subtitle="Your workspace is ready">
        <h2 className="auth-result" tabIndex={-1} ref={resultRef}>
          Thanks — your address is confirmed
        </h2>
        <Notice tone="ok" title="Verified">
          {email ? `${email} is now confirmed.` : 'Your email address is now confirmed.'} You can sign in.
        </Notice>
        <Link className="btn primary login-submit" to="/login">
          Sign in
        </Link>
      </AuthShell>
    );
  }

  if (!token || phase === 'expired') {
    return (
      <AuthShell title="Confirm your email" subtitle={token ? 'This link can no longer be used' : 'This link is incomplete'}>
        <h2 className="auth-result" tabIndex={-1} ref={resultRef}>
          {token ? 'Link expired or already used' : 'No confirmation code in this link'}
        </h2>
        <p className="auth-hint">
          {token
            ? 'Confirmation links work once and expire. Sending a new link cancels older ones. If you already confirmed, just sign in.'
            : 'The link may have been cut short by your mail client. Request a new one below.'}
        </p>
        <ResendVerification />
        <p className="auth-links">
          <Link to="/login">Back to sign in</Link>
        </p>
      </AuthShell>
    );
  }

  return (
    <AuthShell title="Confirm your email" subtitle="One step before you can sign in">
      <p className="auth-hint">Confirm that this email address belongs to you to finish setting up your account.</p>
      <button type="button" className="primary login-submit" onClick={() => void confirm()} disabled={busy} autoFocus>
        {busy ? 'Confirming…' : 'Confirm my email address'}
      </button>
      {error && (
        <Notice tone="error" title="Could not confirm your email">
          {error}
        </Notice>
      )}
      <p className="auth-links">
        <Link to="/login">Back to sign in</Link>
      </p>
    </AuthShell>
  );
}
