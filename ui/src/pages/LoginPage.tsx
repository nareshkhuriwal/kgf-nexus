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
    <div className="login-wrap">
      <div className="panel login-card">
        <div className="panel-body col" style={{ gap: 'var(--space-6)' }}>
          <div>
            <h1 style={{ margin: 0, fontSize: 'var(--text-xl)' }}>
              <span className="brand-mark">◇</span> nexus
            </h1>
            <div className="sub" style={{ marginTop: 'var(--space-2)' }}>
              operator console · namespace <code>{namespace}</code>
            </div>
          </div>

          <div className="seg" role="tablist" aria-label="Sign-in method">
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
            <form className="col" style={{ gap: 'var(--space-5)' }} onSubmit={signIn}>
              <label className="col" style={{ gap: 'var(--space-2)' }}>
                <span className="sub">email</span>
                <input
                  type="email"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  autoComplete="username"
                  required
                  autoFocus
                />
              </label>
              <label className="col" style={{ gap: 'var(--space-2)' }}>
                <span className="sub">password</span>
                <input
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoComplete="current-password"
                  required
                />
              </label>
              <button type="submit" className="primary" disabled={busy}>
                {busy ? 'Signing in…' : 'Sign in'}
              </button>
            </form>
          ) : (
            <form className="col" style={{ gap: 'var(--space-5)' }} onSubmit={useKey}>
              <label className="col" style={{ gap: 'var(--space-2)' }}>
                <span className="sub">API key</span>
                <input
                  type="password"
                  value={key}
                  onChange={(e) => setKey(e.target.value)}
                  placeholder="nxs_admin_…"
                  aria-label="API key"
                  autoFocus
                />
              </label>
              <div className="sub">
                The same credential the CLI and workers use. Stored in this browser only, never
                sent anywhere but this gateway. Run <code>make seed</code> to mint one.
              </div>
              <button type="submit" className="primary">
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
          {warning && (
            <div className="notice error" role="status">
              <span className="notice-glyph" aria-hidden="true">
                !
              </span>
              <div>
                <div className="notice-detail">{warning}</div>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
