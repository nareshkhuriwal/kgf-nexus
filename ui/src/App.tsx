// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Shell and routing.
 *
 * The namespace is a top-level control rather than buried in each page because
 * every §8.2 endpoint is namespace-scoped, and an operator looking at the wrong
 * namespace sees a plausible empty dashboard — the most misleading possible
 * output. So its VALUE is always on screen in the chrome, while the input that
 * changes it sits in the account menu where it cannot be nudged by accident.
 *
 * The chrome itself comes in two shapes — a grouped sidebar and a flat navbar —
 * chosen by the operator and stamped on <html> by lib/appearance.ts. Both read
 * the same destination list from navItems.ts; this file decides only which
 * component renders it, and composes the account menu once for whichever wins.
 */

import { NavLink, Route, Routes, useNavigate } from 'react-router-dom';
import { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { API_BASE, DEFAULT_NAMESPACE, getApiKey, setApiKey } from './api/client';
import { authApi, currentCredential, getSession, setSession } from './api/auth';
import { LoginPage } from './pages/LoginPage';
import { UsersPage } from './pages/admin/UsersPage';
import { TokensPage } from './pages/admin/TokensPage';
import { AuditPage } from './pages/admin/AuditPage';
import { OverviewPage } from './pages/OverviewPage';
import { RunListPage } from './pages/RunListPage';
import { WorkflowsPage } from './pages/WorkflowsPage';
import { WorkflowDetailPage } from './pages/WorkflowDetailPage';
import { RunDetailPage } from './pages/RunDetailPage';
import { WorkersPage } from './pages/WorkersPage';
import { SchedulesPage } from './pages/SchedulesPage';
import { ScheduleDetailPage } from './pages/ScheduleDetailPage';
import { BackfillDetailPage } from './pages/BackfillDetailPage';
import { ErrorBoundary } from './components/ErrorBoundary';
import { CommandPalette } from './components/CommandPalette';
import { ProfileMenu, ProfileSection } from './components/ProfileMenu';
import { Sidebar } from './components/Sidebar';
import { NavBar } from './components/NavBar';
import { AppearancePanel } from './components/AppearancePanel';
import { getNavLayout, type NavLayout } from './lib/appearance';

const NS_KEY = 'nexus.namespace';

export function App() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [namespace, setNamespace] = useState(
    () => window.localStorage.getItem(NS_KEY) ?? DEFAULT_NAMESPACE,
  );

  useEffect(() => {
    window.localStorage.setItem(NS_KEY, namespace);
  }, [namespace]);

  /**
   * Which chrome to render.
   *
   * Seeded from storage, but re-read whenever the account menu opens or closes:
   * the appearance panel writes through lib/appearance.ts (which stamps <html>
   * directly, so CSS reacts immediately), and this keeps the React tree in step
   * without a context or an event bus for a value that changes once a year.
   */
  const [navLayout, setNavLayoutState] = useState<NavLayout>(() => getNavLayout());

  // The key lives in the browser, never in the bundle: anything baked in at
  // build time is readable by everyone who loads the page.
  const [apiKey, setApiKeyState] = useState(() => getApiKey());
  const [keyDraft, setKeyDraft] = useState('');
  const [keyOpen, setKeyOpen] = useState(false);
  const [keyPersistFailed, setKeyPersistFailed] = useState(false);

  // Bumped whenever the credential changes, so whoami re-runs rather than
  // serving a cached answer for a credential that is no longer presented.
  const [credRevision, setCredRevision] = useState(0);
  const credential = currentCredential();

  /**
   * Who the gateway says we are.
   *
   * The SERVER decides what this credential may do; the console never re-derives
   * the authorization matrix from a role name, because a second copy of it is a
   * second opinion that will eventually disagree with the enforcement point.
   * Rendering an admin link that 403s is worse than not rendering it.
   */
  const me = useQuery({
    queryKey: ['whoami', credRevision, credential.token],
    queryFn: () => authApi.whoami(),
    enabled: credential.kind !== 'none',
    retry: false,
    staleTime: 60_000,
  });

  const canAdmin = me.data?.permissions?.admin_write === true;
  const session = getSession();

  function credentialChanged() {
    setCredRevision((n) => n + 1);
    queryClient.invalidateQueries();
  }

  async function signOut() {
    try {
      await authApi.logout();
    } catch {
      // The local credential is cleared regardless: the operator asked to be
      // signed out of this browser, and leaving a usable token behind because
      // the network blipped is the opposite of what they asked for.
      setSession(null);
    }
    credentialChanged();
  }

  function saveKey(next: string) {
    const stored = setApiKey(next);
    // Reflect what is actually READABLE, not what was typed: if persistence
    // failed, the header must not claim a key is set.
    setApiKeyState(getApiKey());
    setKeyPersistFailed(!stored && next.trim() !== '');
    setKeyOpen(false);
    // Every page's data was fetched with the old credential, so re-fetch rather
    // than leave stale 401 panels on screen next to a now-valid key.
    credentialChanged();
  }

  /**
   * The account menu, built once and handed to whichever chrome is active.
   *
   * Composed here rather than inside Sidebar/NavBar because every control in it
   * is state this component owns — namespace, API key, session — and pushing
   * that down would mean threading six props through two layouts to render the
   * same menu twice.
   */
  const account = (
    <ProfileMenu
      session={session}
      principal={me.data}
      organization="KhuriwalGroup IT Solutions"
      onSignOut={() => void signOut()}
      onSignIn={() => navigate('/login')}
      onOpenChange={() => setNavLayoutState(getNavLayout())}
    >
      <ProfileSection label="Namespace">
        <input
          type="text"
          value={namespace}
          onChange={(e) => setNamespace(e.target.value)}
          aria-label="namespace"
        />
      </ProfileSection>

      <ProfileSection label="API key">
        {keyOpen ? (
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault();
              saveKey(keyDraft.trim());
            }}
          >
            <input
              type="password"
              value={keyDraft}
              onChange={(e) => setKeyDraft(e.target.value)}
              placeholder="API key"
              aria-label="API key"
              autoFocus
            />
            <button type="submit">Save</button>
            <button type="button" onClick={() => setKeyOpen(false)}>
              Cancel
            </button>
          </form>
        ) : (
          <button
            type="button"
            onClick={() => {
              setKeyDraft(apiKey);
              setKeyOpen(true);
            }}
            title={
              apiKey
                ? 'An API key is set for this browser. Click to change it.'
                : 'No API key set — every request will be refused with 401.'
            }
          >
            {keyPersistFailed ? 'API key ✗ not saved' : apiKey ? 'API key ✓' : 'Set API key'}
          </button>
        )}
      </ProfileSection>

      {/* The API base stays in the menu but OUTSIDE any collapsible control:
          it says which deployment this tab is pointed at, and mistaking
          staging for production is exactly the error a hidden value invites. */}
      <ProfileSection label="Connected to">
        <span className="chip" title={`API base: ${API_BASE || 'same origin'}`}>
          {API_BASE || 'same origin'}
        </span>
      </ProfileSection>

      <ProfileSection label="Appearance">
        <AppearancePanel />
      </ProfileSection>
    </ProfileMenu>
  );

  return (
    <div className={`app app-${navLayout}`}>
      {/* First focusable element on the page: a keyboard user must be able to
          reach the content without tabbing the whole nav on every navigation. */}
      <a className="skip-link" href="#main">
        Skip to content
      </a>

      {/* Mounted at the shell so Cmd+K works from every page. It owns its own
          open state and renders null while closed, so it costs nothing here. */}
      <CommandPalette namespace={namespace} onNavigate={navigate} onSignOut={() => void signOut()} />

      {navLayout === 'navbar' ? (
        <header className="chrome chrome-navbar">
          <NavBar canAdmin={canAdmin} namespace={namespace} account={account} />
        </header>
      ) : (
        <header className="chrome chrome-sidebar">
          <Sidebar canAdmin={canAdmin} namespace={namespace} account={account} />
        </header>
      )}

      <main className="main" id="main" tabIndex={-1}>
        <ErrorBoundary>
          <Routes>
            <Route path="/" element={<OverviewPage namespace={namespace} />} />
            <Route path="/workflows" element={<WorkflowsPage namespace={namespace} />} />
            <Route
              path="/workflows/:name"
              element={<WorkflowDetailPage namespace={namespace} />}
            />
            <Route path="/runs" element={<RunListPage namespace={namespace} />} />
            <Route path="/runs/:runId" element={<RunDetailPage namespace={namespace} />} />
            <Route path="/schedules" element={<SchedulesPage namespace={namespace} />} />
            <Route
              path="/schedules/:name"
              element={<ScheduleDetailPage namespace={namespace} />}
            />
            <Route
              path="/backfills/:backfillId"
              element={<BackfillDetailPage namespace={namespace} />}
            />
            <Route path="/workers" element={<WorkersPage namespace={namespace} />} />
            <Route
              path="/login"
              element={<LoginPage namespace={namespace} onAuthenticated={credentialChanged} />}
            />
            <Route
              path="/admin/users"
              element={<UsersPage namespace={namespace} canAdmin={canAdmin} />}
            />
            <Route
              path="/admin/tokens"
              element={<TokensPage namespace={namespace} canAdmin={canAdmin} />}
            />
            <Route
              path="/admin/audit"
              element={<AuditPage namespace={namespace} canAdmin={canAdmin} />}
            />
            <Route
              path="*"
              element={
                <div className="notice empty">
                  <span className="notice-glyph">?</span>
                  <div>
                    <div className="notice-title">No such page</div>
                    <div className="notice-detail">
                      <NavLink to="/">Go to the overview</NavLink>
                    </div>
                  </div>
                </div>
              }
            />
          </Routes>
        </ErrorBoundary>
      </main>
    </div>
  );
}
