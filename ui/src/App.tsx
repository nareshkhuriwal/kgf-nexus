// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Shell and routing.
 *
 * The namespace is a top-level control rather than buried in each page because
 * every §8.2 endpoint is namespace-scoped, and an operator looking at the wrong
 * namespace sees a plausible empty dashboard — the most misleading possible
 * output. So its VALUE is always on screen in the chrome — the account control
 * renders it as a read-only fact — while the input that CHANGES it sits on the
 * settings page, two deliberate steps away from anything an operator reaches for
 * mid-incident. Value and control are split on purpose: the fact has to be
 * unmissable, and the control must not be nudgeable by accident.
 *
 * The chrome itself comes in two shapes — a grouped sidebar and a flat navbar —
 * chosen by the operator and stamped on <html> by lib/appearance.ts. Both read
 * the same destination list from navItems.ts; this file decides only which
 * component renders it, and composes the account menu once for whichever wins.
 *
 * Sign-in is the one route OUTSIDE that chrome, expressed as a layout route
 * rather than a conditional inside it. An unauthenticated visitor has no
 * namespace worth switching, no account to open a menu for and no catalog for
 * the command palette to search, so every one of those controls would be a
 * control that either lies or 401s. A layout route makes that structural — /login
 * is a sibling of the chrome, not a child of it — so a route added inside the
 * chrome later cannot accidentally inherit the exception, and the chrome's own
 * state cannot leak onto a screen that has no session yet.
 */

import { NavLink, Outlet, Route, Routes, useLocation, useNavigate } from 'react-router-dom';
import { useEffect, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { API_BASE, DEFAULT_NAMESPACE, getApiKey, setApiKey } from './api/client';
import { authApi, currentCredential, getSession, setSession } from './api/auth';
import { LoginPage } from './pages/LoginPage';
import { UsersPage } from './pages/admin/UsersPage';
import { TokensPage } from './pages/admin/TokensPage';
import { AuditPage } from './pages/admin/AuditPage';
import { OverviewPage } from './pages/OverviewPage';
import { SettingsPage } from './pages/SettingsPage';
import { RunListPage } from './pages/RunListPage';
import { WorkflowsPage } from './pages/WorkflowsPage';
import { WorkflowDetailPage } from './pages/WorkflowDetailPage';
import { RunDetailPage } from './pages/RunDetailPage';
import { WorkersPage } from './pages/WorkersPage';
import { WorkerDetailPage } from './pages/WorkerDetailPage';
import { SchedulesPage } from './pages/SchedulesPage';
import { ScheduleDetailPage } from './pages/ScheduleDetailPage';
import { BackfillDetailPage } from './pages/BackfillDetailPage';
import { ErrorBoundary } from './components/ErrorBoundary';
import { CommandPalette } from './components/CommandPalette';
import { ProfileMenu } from './components/ProfileMenu';
import { Sidebar } from './components/Sidebar';
import { NavBar } from './components/NavBar';
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
   * Seeded from storage and re-read on every navigation. The appearance panel
   * writes through lib/appearance.ts, which stamps <html> directly so CSS reacts
   * immediately — but the React tree has to be told, because which COMPONENT
   * renders the nav is a structural choice, not a styling one.
   *
   * It used to re-read when the account menu closed, which was the right hook
   * while the appearance panel lived inside that menu. The panel is on /settings
   * now, so the only place the layout can change is that page, and leaving it is
   * the natural moment to pick the change up. A route change is a cheaper and
   * more honest trigger than a context or an event bus for a value an operator
   * changes roughly once.
   */
  const location = useLocation();
  const [navLayout, setNavLayoutState] = useState<NavLayout>(() => getNavLayout());
  useEffect(() => {
    setNavLayoutState(getNavLayout());
  }, [location.pathname]);

  // The key lives in the browser, never in the bundle: anything baked in at
  // build time is readable by everyone who loads the page. The state stays here
  // rather than moving to /settings because the value outlives that page — every
  // request the shell's children make is authenticated with it.
  const [apiKey, setApiKeyState] = useState(() => getApiKey());
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

  /**
   * The stored session, held in STATE rather than read during render.
   *
   * This was `getSession()` inline, which looks like state and has none of its
   * guarantees: a plain localStorage read is only as fresh as the last render,
   * with no dependency tracking to cause one. Any path that changes the
   * credential without re-rendering — a token expiring mid-session, signOut's
   * catch branch, a key saved in another tab — left the account menu showing the
   * wrong verb with nothing to correct it, which is why the symptom was
   * intermittent rather than absolute. Refreshed wherever the credential
   * changes, which is the one moment it can become wrong.
   */
  const [session, setSessionState] = useState(() => getSession());

  function credentialChanged() {
    setSessionState(getSession());
    setCredRevision((n) => n + 1);
    queryClient.invalidateQueries();
  }

  /**
   * Sign out, THEN go to the login screen — in that order.
   *
   * The order is the whole point. The credential has to be gone and the cached
   * queries invalidated BEFORE the route changes: navigate first and react-query
   * spends the transition refetching every mounted panel with a token the
   * gateway has just revoked, so a rack of 401s renders behind the login screen
   * for no reason. Completing the logout first means the only thing left to do
   * is leave.
   *
   * The navigation belongs HERE and not at either call site. Sign-out is
   * reachable from two places — the account menu's row and the palette's
   * sign-out command — and a shell that navigated from one of them would have a
   * second path that clears the credential and then strands the operator on a
   * page that has become entirely 401s. It replaces rather than pushes, so Back
   * from the login screen does not return an unauthenticated browser to a
   * signed-in page; LoginPage already replaces on the way in, so a sign-out and
   * a sign-in leave one entry between them rather than a growing pair.
   *
   * The local clear is stated unconditionally here rather than left to the
   * `finally` in authApi.logout(). It was previously only in the catch, which
   * read as though the success path did NOT clear — the opposite of the truth —
   * and made this function's correctness depend on an implementation detail of
   * another module: move that clear out of auth.ts's `finally` and the success
   * path would silently keep a usable token while the failure path went on
   * working. The guarantee the operator asked for is "this browser no longer
   * holds a credential", and it is cheap to assert it at the level that owns the
   * credential state. A network blip must not leave a working token behind.
   */
  async function signOut() {
    try {
      await authApi.logout();
    } catch {
      // Swallowed on purpose: the server's opinion of the logout does not
      // change what happens locally. The operator asked to be signed out of
      // this browser, and refusing because the network blipped would be the
      // opposite of what they asked for.
    }
    setSession(null);
    credentialChanged();
    navigate('/login', { replace: true });
  }

  function saveKey(next: string) {
    const stored = setApiKey(next);
    // Reflect what is actually READABLE, not what was typed: if persistence
    // failed, the header must not claim a key is set.
    setApiKeyState(getApiKey());
    setKeyPersistFailed(!stored && next.trim() !== '');
    // Every page's data was fetched with the old credential, so re-fetch rather
    // than leave stale 401 panels on screen next to a now-valid key.
    credentialChanged();
  }

  /**
   * Whether the command palette is open.
   *
   * Lifted out of the palette ONLY because the navbar now carries a search
   * control that has to open the same instance. The palette still owns every
   * other part of its behaviour — the Cmd+K binding, the query, dismissal,
   * focus restoration — and it is written to accept a controlled `open` for
   * exactly this case, so the shell holds one boolean and nothing else. Mounting
   * a second palette for the header button would have been two searches with two
   * pieces of state that disagree the moment both are reachable.
   */
  const [paletteOpen, setPaletteOpen] = useState(false);

  /**
   * The account menu, built once and handed to whichever chrome is active.
   *
   * Composed here rather than inside Sidebar/NavBar because every fact in it is
   * state this component owns — namespace, credential, session — and pushing
   * that down would mean threading it through two layouts to render the same
   * menu twice.
   *
   * The menu is now rows that NAVIGATE, not widgets that act. The namespace
   * input, the API-key form and the appearance panel all moved to /settings;
   * what stays is the identity, the two facts an operator has to be able to read
   * at any moment (which namespace, which deployment), and the way out. The
   * namespace in particular is load-bearing rather than decorative: every
   * endpoint is namespace-scoped, so an operator scoped to the wrong one reads a
   * plausible empty dashboard as "nothing is running".
   */
  const account = (
    <ProfileMenu
      session={session}
      principal={me.data}
      organization="KhuriwalGroup IT Solutions"
      namespace={namespace}
      connectedTo={API_BASE || 'same origin'}
      canAdmin={canAdmin}
      onSignOut={() => void signOut()}
      onSignIn={() => navigate('/login')}
      onOpenSettings={() => navigate('/settings')}
      onOpenApiKeys={() => navigate('/admin/tokens')}
      onOpenUsers={() => navigate('/admin/users')}
    />
  );

  /**
   * The signed-in shell: chrome, command palette, skip link, and an <Outlet/>
   * for whichever destination is routed into it.
   *
   * Declared here rather than in its own file because every value it renders —
   * namespace, credential, layout, the composed account menu — is state this
   * component owns, and lifting it out would mean threading all of it back down
   * through props to build the same header twice.
   */
  const chrome = (
    <div className={`app app-${navLayout}`}>
      {/* First focusable element on the page: a keyboard user must be able to
          reach the content without tabbing the whole nav on every navigation. */}
      <a className="skip-link" href="#main">
        Skip to content
      </a>

      {/* Mounted at the shell so Cmd+K works from every page. It owns its own
          open state and renders null while closed, so it costs nothing here.
          Deliberately inside the chrome layout and not above the router: on
          /login there is no catalog to search and no credential to search it
          with, so the palette would open onto a list of 401s. */}
      <CommandPalette
        namespace={namespace}
        onNavigate={navigate}
        onSignOut={() => void signOut()}
        open={paletteOpen}
        onOpenChange={setPaletteOpen}
      />

      {navLayout === 'navbar' ? (
        <header className="chrome chrome-navbar">
          {/* The search control and the gear are passed as callbacks rather
              than rendered by NavBar itself, so the bar owns no route and no
              palette state: App is the only thing here that has a router and a
              palette to hand it. Sidebar gets neither — the rail already shows
              the grouped destinations a drawer would duplicate, and the gear's
              destination is already a row in the account menu it pins to its
              own bottom edge. */}
          <NavBar
            canAdmin={canAdmin}
            account={account}
            onOpenSearch={() => setPaletteOpen(true)}
            onOpenSettings={() => navigate('/settings')}
          />
        </header>
      ) : (
        <header className="chrome chrome-sidebar">
          <Sidebar canAdmin={canAdmin} account={account} />
        </header>
      )}

      <main className="main" id="main" tabIndex={-1}>
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  );

  return (
    <Routes>
      {/* Outside the chrome: no nav, no namespace control, no account menu. */}
      <Route
        path="/login"
        element={
          <ErrorBoundary>
            <LoginPage namespace={namespace} onAuthenticated={credentialChanged} />
          </ErrorBoundary>
        }
      />

      <Route element={chrome}>
        <Route path="/" element={<OverviewPage namespace={namespace} />} />
        <Route path="/workflows" element={<WorkflowsPage namespace={namespace} />} />
        <Route path="/workflows/:name" element={<WorkflowDetailPage namespace={namespace} />} />
        <Route path="/runs" element={<RunListPage namespace={namespace} />} />
        <Route path="/runs/:runId" element={<RunDetailPage namespace={namespace} />} />
        <Route path="/schedules" element={<SchedulesPage namespace={namespace} />} />
        <Route path="/schedules/:name" element={<ScheduleDetailPage namespace={namespace} />} />
        <Route
          path="/backfills/:backfillId"
          element={<BackfillDetailPage namespace={namespace} />}
        />
        <Route path="/workers" element={<WorkersPage namespace={namespace} />} />
        <Route path="/workers/:workerId" element={<WorkerDetailPage namespace={namespace} />} />
        {/* Inside the chrome, and deliberately not in navItems.ts: settings is
            somewhere you go from your own account control, not a destination
            competing with the runs and workflows an operator came for. */}
        <Route
          path="/settings"
          element={
            <SettingsPage
              namespace={namespace}
              onNamespaceChange={setNamespace}
              apiKey={apiKey}
              onApiKeyChange={saveKey}
              apiKeyPersistFailed={keyPersistFailed}
            />
          }
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
        {/* Inside the chrome on purpose: a mistyped URL from a signed-in
            operator is a navigation error, and stripping the nav from the one
            screen that exists to send them somewhere else would be perverse. */}
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
      </Route>
    </Routes>
  );
}
