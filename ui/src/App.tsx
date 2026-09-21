// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { RegisterPage } from './pages/RegisterPage';
import { CustomerPage } from './pages/CustomerPage';
import { PermissionContext, PermissionBoundary } from './components/Permissions';
import { RolesPage } from './pages/admin/RolesPage';
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
 * ── the shell's shape ─────────────────────────────────────────────────────
 *
 * ONE chrome, in two parts that are always both present: a navbar spanning the
 * full width, and below it a sidebar on the left with the page to its right.
 *
 * This replaced a choice between them — a NavLayout of 'sidebar' | 'navbar',
 * stamped on <html>, with this file rendering one or the other. That choice is
 * gone, along with the setting that made it, because it was a false one. The
 * two shapes do not compete for the same space: the bar wants horizontal room
 * for identity, search and tools, and the rail wants vertical room for a list
 * that only grows. An operator picking "navbar" gave up the group headings to
 * win vertical space; picking "sidebar" gave up the search field and the tool
 * cluster. Both halves are cheap, so both are present and nobody has to choose
 * — and the maintenance hazard of two renderers for one destination list goes
 * with it.
 *
 * What the operator chooses INSTEAD is narrower and real: whether the rail is
 * expanded or collapsed to its icon-and-label rail. That is one shell
 * remembering a width, not two shells.
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

import { ConsoleContext } from './components/ConsoleContext';
import { NavLink, Outlet, Route, Routes, useNavigate } from 'react-router-dom';
import { useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { API_BASE, DEFAULT_NAMESPACE } from './api/client';
import { authApi, currentCredential, getSession, setSession } from './api/auth';
import { LoginPage } from './pages/LoginPage';
import { InfrastructurePage } from './pages/admin/InfrastructurePage';
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
import { NavDrawer } from './components/NavDrawer';
import { getRailCollapsed, setRailCollapsed } from './lib/appearance';
import { useAttention } from './api/hooks';

const NS_KEY = 'nexus.namespace';

/**
 * The width below which the rail leaves the layout and becomes an overlay.
 *
 * KEEP IN STEP WITH index.css, which hides the rail at the same 560px and is
 * commented pointing back here. Two declarations of one breakpoint is a real
 * cost, and the alternative — deriving the JS branch from a CSS custom property
 * — means reading a computed style on every resize to learn something that has
 * not changed. Both sides carry the cross-reference instead.
 */
const NARROW_QUERY = '(max-width: 560px)';

/**
 * Whether the viewport is currently narrow.
 *
 * Guarded because matchMedia is absent in some test environments, and returning
 * false there is the safe answer: the shell renders its desktop shape, which is
 * a rail beside the page rather than no navigation at all.
 */
function matchesNarrow(): boolean {
  try {
    return window.matchMedia(NARROW_QUERY).matches;
  } catch {
    return false;
  }
}

/**
 * The limit the bell's attention query asks for.
 *
 * IT MATCHES OverviewPage's ATTENTION_LIMIT EXACTLY, and that is the whole
 * design of this constant rather than a coincidence to be tidied up.
 *
 * Two reasons, and the first is correctness. AttentionList.count describes the
 * items RETURNED, not the namespace's total — types.ts is explicit about it,
 * which is why the page has a "view all" affordance instead of a total it could
 * subtract from. So a bell that asked for `limit: 1` to save bytes would
 * receive count: 1 and display a permanent, confident "1" no matter how many
 * runs were actually failing. That is the worst class of bug this console can
 * ship: a number in the chrome that is wrong in the reassuring direction.
 *
 * The second is cost. Matching the limit means this query and the overview's
 * are THE SAME cache entry — same key, one fetch, one 10s poll — so putting the
 * count in the chrome on every page costs nothing extra while the overview is
 * open, and one cheap read when it is not. A different limit would have
 * doubled the most expensive read in the console to show a smaller number.
 *
 * `truncated` then means the same thing in both places: there are more than
 * this many. The bell renders the count as "99+" only as a width concession;
 * see NavBar.tsx.
 */
const BELL_ATTENTION_LIMIT = 12;

export function App() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [namespace, setNamespace] = useState(
    () => getSession()?.namespace ?? window.localStorage.getItem(NS_KEY) ?? DEFAULT_NAMESPACE,
  );

  useEffect(() => {
    window.localStorage.setItem(NS_KEY, namespace);
  }, [namespace]);

  /**
   * Whether the sidebar is collapsed to its icon-and-label rail.
   *
   * Seeded from storage so the choice survives a reload, and written through
   * lib/appearance.ts on every toggle rather than in an effect: the write is
   * caused by the click, not by the state arriving, and an effect would also
   * re-persist the seeded value on mount for no reason.
   *
   * This is the one piece of chrome state App holds that is purely a display
   * preference. It lives here rather than in Sidebar because the control that
   * changes it is the hamburger in the NAVBAR, and this component is the only
   * one that can see both.
   */
  const [railCollapsed, setRailCollapsedState] = useState(() => getRailCollapsed());

  /**
   * Whether the viewport is too narrow for the rail to be in the layout at all.
   *
   * WHY A MEDIA QUERY IN JS AND NOT PURELY IN CSS. At this width the rail is
   * not merely styled differently, it becomes a MODAL panel: it needs a scrim,
   * a focus trap, an Escape binding and aria-modal, and none of that is
   * expressible in a stylesheet. So the breakpoint has to be a value the React
   * tree can branch on, and the hamburger has to know which of the two things
   * it is about to do so its label can say so.
   *
   * 560px matches the phone breakpoint the stylesheet already uses, and the two
   * are commented at both ends so a change to one is a visible inconsistency
   * rather than a silent one. Wrapped in try/catch because matchMedia is absent
   * in some test environments, defaulting to "not narrow" — the desktop shape,
   * which degrades to a rail that is merely wide rather than to no navigation.
   */
  const [narrow, setNarrow] = useState(() => matchesNarrow());
  useEffect(() => {
    let mq: MediaQueryList;
    try {
      mq = window.matchMedia(NARROW_QUERY);
    } catch {
      return;
    }
    const onChange = () => setNarrow(mq.matches);
    // Read once on subscribe as well: between the initial state and this effect
    // the viewport may already have changed (an orientation flip during boot).
    onChange();
    mq.addEventListener?.('change', onChange);
    return () => mq.removeEventListener?.('change', onChange);
  }, []);

  /**
   * Whether the narrow-width overlay panel is open.
   *
   * Separate state from railCollapsed, and deliberately NOT persisted. The
   * collapsed rail is a preference — how the operator likes their console — and
   * an open overlay is a transient act. Restoring a drawer over the page on
   * reload would be a console that boots with its navigation covering the
   * content.
   */
  const [drawerOpen, setDrawerOpen] = useState(false);

  // The drawer hands focus back to whatever opened it, which is always the
  // hamburger. Held here because the shell owns both ends of that handoff now.
  const hamburgerRef = useRef<HTMLButtonElement>(null);

  /**
   * The hamburger's one action, which means two different things by width.
   *
   * Narrow: open the overlay, because there is no room for a rail. Otherwise:
   * collapse or expand the rail in place. One control rather than two, because
   * from the operator's side it is one intent — "show me the destinations" /
   * "give me the room back" — and a bar that grew a second button at a
   * breakpoint would be a bar whose controls move while you are using it.
   */
  function toggleRail() {
    if (narrow) {
      setDrawerOpen((o) => !o);
      return;
    }
    const next = !railCollapsed;
    setRailCollapsedState(next);
    setRailCollapsed(next);
  }

  /**
   * Close the overlay if the viewport grows past the breakpoint.
   *
   * Without this, rotating a tablet to landscape leaves a modal scrim over a
   * page that now has a perfectly good rail beside it, with the only way out
   * being a control the scrim is covering.
   */
  useEffect(() => {
    if (!narrow) setDrawerOpen(false);
  }, [narrow]);

  // The key lives in the browser, never in the bundle: anything baked in at
  // build time is readable by everyone who loads the page. The state stays here
  // rather than moving to /settings because the value outlives that page — every
  // request the shell's children make is authenticated with it.

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
    staleTime: 10_000,
    refetchInterval: 10_000,
  });

  const grants = me.isError ? [] : me.data?.effective_permissions ?? [];
  const canAdmin = grants.includes("roles.manage");

  /**
   * How many items need attention, for the bar's bell.
   *
   * Read HERE rather than in NavBar so there is exactly one subscription to the
   * most expensive query in the console: the overview renders the same list, and
   * react-query dedupes the two consumers onto one poll only because both ask
   * with the same key and the same limit. That is why BELL_ATTENTION_LIMIT
   * matches the overview's — a different limit would be a different key, and a
   * second poll.
   *
   * Left UNDEFINED unless the query actually answered. A bell showing 0 because
   * the request failed, or because it has not returned yet, is a bell claiming
   * everything is fine at the exact moment it does not know — which is the one
   * lie this console must not tell.
   */
  const attention = useAttention(namespace, { limit: BELL_ATTENTION_LIMIT }, ["overview.view","runs.view","workers.view"].every(p => grants.includes(p)));
  const bellCount = attention.data?.items?.length;

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
    const nextSession=getSession();
    setSessionState(nextSession);
    if(nextSession)setNamespace(nextSession.namespace);
    queryClient.clear();
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
      onOpenApiKeys={grants.includes("tokens.view") ? () => navigate('/admin/tokens') : undefined}
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
    <div className={`app${railCollapsed ? ' rail-collapsed' : ''}`}>
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

      {/* ONE shell, not two. The bar and the rail are no longer alternatives an
          operator picks between — they are both always here, the bar spanning
          the top and the rail beneath it on the left. That is why there is no
          layout branch left: the setting that used to choose between them
          described a choice this shell no longer offers, so it was removed
          rather than left as a control that changes nothing.

          The search control and the gear are passed as callbacks rather than
          rendered by NavBar itself, so the bar owns no route and no palette
          state: App is the only thing here holding a router and a palette to
          hand it. The hamburger is the bar's, but what it TOGGLES is the rail
          below, so its state lives here — the one place that can see both. */}
      <header className="chrome chrome-navbar">
        <NavBar
          account={account}
          namespace={namespace}
          railExpanded={!railCollapsed}
          railIsOverlay={narrow}
          onToggleRail={toggleRail}
          hamburgerRef={hamburgerRef}
          onOpenSearch={() => setPaletteOpen(true)}
          onOpenSettings={() => navigate('/settings')}
          attentionCount={bellCount}
          onOpenAttention={() => navigate('/')}
        />
      </header>

      <div className="shell">
        {/* At narrow widths the rail has nowhere to sit, so the same
            destinations arrive as an off-canvas panel over the page instead of
            squeezing the content to nothing. One list, two presentations —
            NavDrawer renders visibleGroups() exactly as the rail does. */}
        {narrow ? (
          <NavDrawer
            open={drawerOpen}
            canAdmin={canAdmin}
            onClose={() => setDrawerOpen(false)}
            returnFocusTo={hamburgerRef}
          />
        ) : (
          // The collapsed class goes on the WRAPPER as well as the rail: the
          // wrapper owns the column's width and the rail owns what is inside
          // it, and the width is the half that must not depend on selector
          // support for :has().
          <aside className={`chrome chrome-sidebar${railCollapsed ? ' collapsed' : ''}`}>
            <Sidebar canAdmin={canAdmin} collapsed={railCollapsed} />
          </aside>
        )}

        <main className="main" id="main" tabIndex={-1}>
          <ConsoleContext />
          <ErrorBoundary>
            <PermissionBoundary pending={me.isPending && credential.kind !== "none"}><Outlet /></PermissionBoundary>
          </ErrorBoundary>
        </main>
      </div>
    </div>
  );

  return (
    <PermissionContext.Provider value={grants}><Routes><Route path="/register" element={<RegisterPage />} />
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
        <Route path="/runs" element={<RunListPage key={namespace} namespace={namespace} canAdmin={grants.includes("runs.delete")} />} />
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
              principal={me.isError ? undefined : me.data}
              namespace={namespace}
              onNamespaceChange={setNamespace}
            />
          }
        />
        <Route path="/customer" element={<CustomerPage />} />
        <Route path="/admin/roles" element={<RolesPage namespace={namespace} />} />
        <Route path="/admin/infrastructure" element={<InfrastructurePage key={namespace} namespace={namespace} canAdmin={grants.includes("infrastructure.view")} />} />
        <Route
          path="/admin/users"
          element={<UsersPage namespace={namespace} canAdmin={grants.includes("users.view")} />}
        />
        <Route
          path="/admin/tokens"
          element={<TokensPage namespace={namespace} canAdmin={grants.includes("tokens.view")} />}
        />
        <Route
          path="/admin/audit"
          element={<AuditPage namespace={namespace} canAdmin={grants.includes("audit.view")} />}
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
    </Routes></PermissionContext.Provider>
  );
}
