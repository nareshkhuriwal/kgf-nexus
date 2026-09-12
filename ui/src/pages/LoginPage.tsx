// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Sign-in.
 *
 * Interactive sign-in is email and password ONLY. The console previously gave an
 * API key equal standing here on a CLI-parity argument, but parity was the wrong
 * frame: a key is a machine credential. It is what the CLI and every worker
 * present, it is long-lived, it is not scoped to a browser session, and pasting
 * one into the first field a person sees on the first screen they see trains
 * operators to treat a service credential as a personal one. The two paths also
 * mean different things on success — a session can be revoked and expires, a key
 * sits in this browser until someone clears it — so offering them as
 * interchangeable tabs misrepresented both.
 *
 * Keys have not gone anywhere; they moved to where a machine credential belongs.
 * Settings configures the key this browser presents, which is the right place for
 * a value you set once for a deployment rather than one you type to identify
 * yourself. This screen answers only "who are you", and the answer is a session.
 *
 * LAYOUT. This is the only screen with no navigation, no namespace switcher and
 * nothing else competing for the eye, so it takes the whole viewport as a split:
 * a dark brand panel on the left, the form on the right. The split is not
 * decoration — it does a job the chrome normally does. Everywhere else in the
 * console the shell tells you WHAT you are looking at; here there is no shell, so
 * an operator landing on a bare form has no way to tell a Nexus gateway from any
 * other login page on the intranet. The left panel is that answer, and it is
 * restricted to things that are actually true of this deployment: what the
 * product is, and which surfaces the console covers. No metrics, no customer
 * marks, no version claims — nothing this page cannot verify.
 *
 * The texture behind it is a DAG, drawn from the same shape vocabulary as the
 * mark: nodes joined by edges. It is on-subject rather than generic geometry,
 * and it is aria-hidden and very low contrast, because its job is to keep a
 * large dark rectangle from reading as a rendering failure.
 *
 * The accent still appears exactly ONCE, on the mark. The submit button keeps the
 * console's existing .primary treatment; the left panel's own surface is a fixed
 * brand dark rather than an accent wash, so switching accents cannot turn this
 * screen into a monochrome field of one hue.
 */

import { useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import { authApi, setSession } from '../api/auth';
import { errorMessage } from '../api/hooks';
import { Logo } from '../components/Logo';

/**
 * What this console actually puts in front of an operator.
 *
 * Each line names a real surface with a real page behind it, so the panel is a
 * description of the tool rather than a pitch. Kept to five: this is orientation
 * for someone who has not signed in yet, not a feature matrix.
 */
const COVERS: { title: string; detail: string }[] = [
  { title: 'Runs', detail: 'Live and historical executions, attempt by attempt' },
  { title: 'Workflows', detail: 'The DAG catalog and every published version' },
  { title: 'Schedules', detail: 'Cron triggers, backfills and their windows' },
  { title: 'Worker fleet', detail: 'Registration, capacity and liveness' },
  { title: 'Audit', detail: 'Who changed what, and when' },
];

export function LoginPage({
  namespace,
  onAuthenticated,
}: {
  namespace: string;
  onAuthenticated: () => void;
}) {
  const navigate = useNavigate();

  const [email, setEmail] = useState('');
  const [password, setPassword] = useState('');

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

  return (
    <div className="login-page">
      {/* Expository, not functional: nothing in here is an input or a link, which
          is what makes it safe to drop entirely at phone widths. */}
      <aside className="login-aside" aria-hidden="true">
        <LoginTexture />
        <div className="login-aside-inner">
          <div className="login-aside-head">
            <Logo size={40} className="login-aside-mark" />
            <div className="login-aside-word">Nexus</div>
          </div>
          <p className="login-aside-lede">
            A control plane for DAG workflows — submit them, watch them run, and see exactly which
            node stopped.
          </p>
          <ul className="login-covers">
            {COVERS.map((c) => (
              <li key={c.title} className="login-cover">
                <span className="login-cover-dot" />
                <span className="login-cover-text">
                  <span className="login-cover-title">{c.title}</span>
                  <span className="login-cover-detail">{c.detail}</span>
                </span>
              </li>
            ))}
          </ul>
        </div>
      </aside>

      {/* Follows the operator's theme, unlike the panel beside it: this half is
          console surface, and a hardcoded white card would be the one thing on a
          dark-theme screen that burns. */}
      <div className="login-main">
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
              {/* A warning, not a failure: the sign-in worked. It keeps
                  role=status so it is announced without interrupting, and takes
                  the warning tone rather than the danger tone the error uses. */}
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

          {/* Says where the machine credential lives, without offering a field
              for it: an operator who arrived with a key in their clipboard needs
              to know it still works, and needs to not paste it here. */}
          <div className="login-foot">
            This console talks only to the gateway configured for it. Nothing you type here is sent
            anywhere else. API keys are the CLI and worker credential — set one in Settings, not
            here.
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * The brand panel's background.
 *
 * A DAG, because that is the subject: three ranks of nodes joined left to right,
 * the same hub-and-edge vocabulary as the mark. Deliberately NOT a real run —
 * drawing a plausible pipeline with plausible node names on the sign-in screen
 * would be inventing data about a namespace nobody has authenticated to yet.
 *
 * preserveAspectRatio="none" on a cover-sized SVG would shear the circles, so it
 * keeps its ratio and is scaled to cover with the CSS instead.
 */
function LoginTexture() {
  return (
    <svg
      className="login-texture"
      viewBox="0 0 400 300"
      fill="none"
      aria-hidden="true"
      focusable="false"
    >
      {/* Edges under nodes, so no stroke end pokes out of a circle. */}
      <g className="login-texture-edges">
        <path d="M60 150 L160 70" />
        <path d="M60 150 L160 150" />
        <path d="M60 150 L160 230" />
        <path d="M160 70 L270 110" />
        <path d="M160 150 L270 110" />
        <path d="M160 150 L270 200" />
        <path d="M160 230 L270 200" />
        <path d="M270 110 L360 150" />
        <path d="M270 200 L360 150" />
      </g>
      <g className="login-texture-nodes">
        <circle cx="60" cy="150" r="13" />
        <circle cx="160" cy="70" r="9" />
        <circle cx="160" cy="150" r="9" />
        <circle cx="160" cy="230" r="9" />
        <circle cx="270" cy="110" r="9" />
        <circle cx="270" cy="200" r="9" />
        <circle cx="360" cy="150" r="13" />
      </g>
    </svg>
  );
}
