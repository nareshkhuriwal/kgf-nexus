// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Sign-in.
 *
 * The API-key escape hatch is kept and given equal standing rather than hidden
 * behind an "advanced" disclosure: the CLI and every worker authenticate with a
 * key, and an operator debugging at 3am with a key already in their clipboard
 * should not have to hunt for where to put it. The brief requires CLI parity,
 * and parity means the path is a real one, not a grudging fallback.
 *
 * It is also the only screen with no navigation, no namespace switcher and
 * nothing else competing for the eye, so it is laid out as a centred column
 * rather than as a panel dropped into the page grid: here the page IS the card.
 * The accent appears once, on the mark — the same rule that governs the rest of
 * the console, where colour that means something comes from --tone-* instead.
 */

import { useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { authApi, setSession } from '../api/auth';
import { setApiKey } from '../api/client';
import { errorMessage } from '../api/hooks';

export function LoginPage({
  namespace,
  onAuthenticated,
}: {
  namespace: string;
  onAuthenticated: () => void;
}) {
  const navigate = useNavigate();
  const [mode, setMode] = useState<'password' | 'key'>('password');

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');
  const [key, setKey] = useState('');

  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  async function signIn(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setWarning(null);
    try {
      const session = await authApi.login(namespace, email.trim(), password);
      if (!setSession(session)) {
        // Honest rather than silent: the login worked, but it will not survive a
        // reload, and an operator should know that before they close the tab.
        setWarning(
          'Signed in, but this browser refused to store the session — you will be signed out on reload.',
        );
      }
      onAuthenticated();
      navigate('/runs', { replace: true });
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  }

  function useKey(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setWarning(null);
    const stored = setApiKey(key.trim());
    if (!stored && key.trim() !== '') {
      setError('This browser refused to store the API key (site data may be blocked).');
      return;
    }
    onAuthenticated();
    navigate('/runs', { replace: true });
  }

  return (
    <div className="login-page">
      <div className="login-col">
        <div className="login-brand">
          <span className="login-mark" aria-hidden="true">
            ◇
          </span>
          <h1 className="login-title">nexus</h1>
          <div className="login-sub">
            operator console · namespace <code>{namespace}</code>
          </div>
        </div>

        <div className="panel">
          <div className="panel-body col" style={{ gap: 'var(--space-6)' }}>
            {/* Two methods of equal standing, so the control gives them equal
                width rather than making the key a secondary tab. */}
            <div className="seg login-seg" role="tablist" aria-label="Sign-in method">
              <button
                type="button"
                role="tab"
                aria-selected={mode === 'password'}
                className={`seg-btn${mode === 'password' ? ' active' : ''}`}
                onClick={() => setMode('password')}
              >
                Email &amp; password
              </button>
              <button
                type="button"
                role="tab"
                aria-selected={mode === 'key'}
                className={`seg-btn${mode === 'key' ? ' active' : ''}`}
                onClick={() => setMode('key')}
              >
                API key
              </button>
            </div>

            {mode === 'password' ? (
              <form className="login-form" onSubmit={signIn}>
                <label className="form-field">
                  <span className="form-label">email</span>
                  <input
                    type="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    autoComplete="username"
                    required
                    autoFocus
                  />
                </label>
                <label className="form-field">
                  <span className="form-label">password</span>
                  <input
                    type="password"
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                    autoComplete="current-password"
                    required
                  />
                </label>
                <button type="submit" className="primary login-submit" disabled={busy}>
                  {busy ? 'Signing in…' : 'Sign in'}
                </button>
              </form>
            ) : (
              <form className="login-form" onSubmit={useKey}>
                <label className="form-field">
                  <span className="form-label">API key</span>
                  <input
                    type="password"
                    value={key}
                    onChange={(e) => setKey(e.target.value)}
                    placeholder="nxs_admin_…"
                    aria-label="API key"
                    autoFocus
                  />
                  <span className="form-hint">
                    The same credential the CLI and workers use. Stored in this browser only, never
                    sent anywhere but this gateway. Run <code>make seed</code> to mint one.
                  </span>
                </label>
                <button type="submit" className="primary login-submit">
                  Use this key
                </button>
              </form>
            )}

            {error && (
              <div className="notice error" role="alert">
                <span className="notice-glyph" aria-hidden="true">
                  !
                </span>
                <div>
                  <div className="notice-title">Could not sign in</div>
                  <div className="notice-detail">{error}</div>
                </div>
              </div>
            )}
            {/* A warning, not a failure: the sign-in worked. It keeps role=status
                so it is announced without interrupting, and takes the warning
                tone rather than the danger tone the error above uses. */}
            {warning && (
              <div className="notice login-warning" role="status">
                <span className="notice-glyph" aria-hidden="true">
                  !
                </span>
                <div className="notice-detail">{warning}</div>
              </div>
            )}
          </div>
        </div>

        <div className="login-foot">
          This console talks only to the gateway configured for it. Nothing you type here is sent
          anywhere else.
        </div>
      </div>
    </div>
  );
}
