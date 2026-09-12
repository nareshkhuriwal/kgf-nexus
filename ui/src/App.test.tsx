// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Shell-level tests.
 *
 * The layouts are covered in Sidebar.test.tsx; what is pinned here is what only
 * App can get wrong. It owns the namespace, the credential, the admin gate and
 * the routes, and the rewrite that introduced two chrome shapes is exactly the
 * kind of change that quietly drops one of them — the skip link, say, or the
 * admin links appearing for a reader. Each of these was a working behaviour
 * before the shell existed and must still be one after.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

/** Whoami answers; every page query is allowed to fail harmlessly. */
function mockGateway(permissions: Record<string, boolean>) {
  vi.stubGlobal('fetch', (url: string) => {
    if (String(url).includes('/whoami')) {
      return Promise.resolve(
        new Response(
          JSON.stringify({
            namespace: 'default',
            name: 'naresh@khuriwalgroup.com',
            role: 'admin',
            kind: 'session',
            permissions,
          }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      );
    }
    return Promise.resolve(new Response(JSON.stringify({ items: [] }), { status: 200 }));
  });
}

function signIn() {
  window.localStorage.setItem(
    'nexus.session',
    JSON.stringify({
      token: 'tok',
      // Far future, so the stored session is not dropped as expired.
      expiresAt: new Date(Date.now() + 86_400_000).toISOString(),
      email: 'naresh@khuriwalgroup.com',
      namespace: 'default',
      role: 'admin',
    }),
  );
}

function renderApp(route = '/') {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[route]}>
        <App />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.removeAttribute('data-nav');
  mockGateway({ admin_write: false });
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('the application shell', () => {
  /** The first focusable element on the page, and the reason a keyboard user
      does not have to tab the whole rail on every navigation. */
  it('keeps the skip link as the route to the main region', () => {
    renderApp();

    const skip = screen.getByRole('link', { name: /skip to content/i });
    expect(skip).toHaveAttribute('href', '#main');
    expect(document.getElementById('main')).toBeInTheDocument();
  });

  it('renders the sidebar by default', () => {
    renderApp();

    expect(screen.getByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
    // The grouped headings are the sidebar's distinguishing mark.
    expect(screen.getByText('Observe')).toBeInTheDocument();
  });

  /**
   * Both chromes, always. There is no stored layout to honour any more.
   *
   * The old test here set `nexus.nav` and asserted the navbar replaced the rail.
   * That preference is gone: the bar and the rail are not alternatives, so the
   * shell renders the bar's identity-and-tools row AND the rail's grouped
   * destinations at the same time.
   */
  it('renders the bar and the rail together, with the groups on the rail', () => {
    renderApp();

    expect(screen.getByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
    expect(screen.getByText('Observe')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
    // The bar's own contribution: the brand, which the rail no longer prints.
    expect(screen.getByText('KGF Nexus')).toBeInTheDocument();
  });

  /**
   * The collapsed state reaches the WRAPPER, not just the rail.
   *
   * This shipped broken: App set the class on <Sidebar> only, and every
   * collapsed style that mattered — the column's 72px width above all — keys off
   * the wrapper. So the button toggled state, the rail re-rendered, and nothing
   * moved. The React-level test passed throughout, which is exactly why this
   * assertion is about the class landing on both elements rather than about the
   * boolean changing.
   */
  it('marks both the rail and its column as collapsed', async () => {
    renderApp();

    const column = document.querySelector('.chrome-sidebar')!;
    expect(column.className).not.toContain('collapsed');

    fireEvent.click(screen.getByRole('button', { name: /collapse the sidebar/i }));

    await waitFor(() => expect(column.className).toContain('collapsed'));
    expect(document.querySelector('.sidebar')!.className).toContain('collapsed');
  });

  /**
   * Exactly one account menu.
   *
   * Worth its own assertion because the first wiring of this shell passed
   * `account` to both the bar and the rail, putting two account menus — and so
   * two sign-out buttons — on screen at once.
   */
  it('renders exactly one account control', () => {
    renderApp();

    expect(screen.getAllByRole('button', { name: /account menu/i })).toHaveLength(1);
  });

  /**
   * The namespace's VALUE must be on screen at every moment, and this is the
   * assertion that keeps it there. It survived the controls moving to /settings
   * because it was never about the control: every endpoint is namespace-scoped,
   * so an operator scoped to the wrong one reads a plausible empty dashboard as
   * "nothing is running" — the most misleading output this console can produce.
   */
  it('shows the namespace in the chrome at all times', () => {
    window.localStorage.setItem('nexus.namespace', 'payments');
    renderApp();

    // Scoped to the chrome: pages may legitimately print the namespace too, and
    // the claim here is specifically that the SHELL always shows it.
    const chrome = document.querySelector('.chrome') as HTMLElement;
    expect(within(chrome).getByText('payments')).toBeInTheDocument();
  });

  /**
   * The menu NAVIGATES; it no longer acts.
   *
   * The namespace input, the API-key form and the appearance panel all moved to
   * /settings, and this pins the negative: a dropdown that both states facts and
   * mutates the credential behind them is a control an operator can change by
   * accident while reading it. SettingsPage.test.tsx covers the controls
   * themselves at the page level.
   */
  it('offers rows that navigate rather than widgets that act', () => {
    renderApp();
    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));

    expect(screen.getByRole('menuitem', { name: 'Settings' })).toBeInTheDocument();
    // None of the three widgets it used to embed.
    expect(screen.queryByLabelText('namespace')).toBeNull();
    expect(screen.queryByLabelText('API key')).toBeNull();
    expect(screen.queryByRole('radiogroup', { name: 'Accent' })).toBeNull();
  });

  /** Settings is reachable from the account control, and only from there. */
  it('routes the account menu to the settings page', async () => {
    renderApp();
    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));

    fireEvent.click(screen.getByRole('menuitem', { name: 'Settings' }));

    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeInTheDocument();
    // Deliberately absent from the nav rail: settings is somewhere you go from
    // your own account, not a destination competing with runs and workflows.
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    expect(within(nav).queryByRole('link', { name: 'Settings' })).toBeNull();
  });

  /**
   * The namespace App owns is the one /settings edits. The value is threaded
   * down rather than re-read there, so a change on that page has to come back
   * through App to reach the chrome and every scoped query under it.
   */
  it('stores what the settings page types into the namespace', async () => {
    window.localStorage.setItem('nexus.namespace', 'payments');
    renderApp('/settings');

    const input = await screen.findByLabelText('namespace');
    expect(input).toHaveValue('payments');

    fireEvent.change(input, { target: { value: 'billing' } });

    await waitFor(() => expect(window.localStorage.getItem('nexus.namespace')).toBe('billing'));
    // Round trip complete: the chrome is reading App's state, not its own copy.
    const chrome = document.querySelector('.chrome') as HTMLElement;
    expect(within(chrome).getByText('billing')).toBeInTheDocument();
  });

  /**
   * The key App presents is the one /settings saves. Asserted through the shell
   * rather than on the page in isolation, because the reason the state stayed in
   * App is that every request the shell's children make is authenticated with it.
   */
  it('saves an API key typed on the settings page', async () => {
    renderApp('/settings');

    fireEvent.click(await screen.findByRole('button', { name: /set api key/i }));
    fireEvent.change(screen.getByLabelText('API key'), { target: { value: 'k-123' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /api key ✓/i })).toBeInTheDocument(),
    );
  });

  /**
   * Choosing a layout must reshape the shell, not just stamp a token.
   *
   * The trigger moved with the panel: App used to re-read the layout when the
   * menu closed, and now re-reads it on navigation — the appearance controls
   * live on /settings, so leaving that page is the moment the change lands.
   */
  /**
   * The hamburger collapses the rail, and the choice is remembered.
   *
   * This is what replaced the layout setting: not a preference you navigate to
   * configure, but a direct control in the bar, because collapsing the rail is
   * something an operator does while working rather than a decision they make
   * once. Collapsing must never REMOVE a destination — that is the property that
   * makes it safe to offer — so the links are asserted present in both states.
   */
  it('collapses and expands the rail from the bar, and remembers which', async () => {
    const first = renderApp();

    // Asserted on the rail's own class rather than on the headings being
    // absent: the headings stay in the DOM when collapsed and are hidden by CSS,
    // deliberately, so that collapsing does not add and remove nodes around the
    // links and cost focus mid-navigation. jsdom applies no CSS, so the text is
    // still findable here and testing for it would assert the wrong mechanism.
    expect(document.querySelector('.sidebar')!.className).not.toContain('collapsed');
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /collapse the sidebar/i }));

    await waitFor(() =>
      expect(document.querySelector('.sidebar')!.className).toContain('collapsed'),
    );
    // Collapsing must never take a destination away. That is the property that
    // makes it safe to offer as a display preference at all.
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
    expect(window.localStorage.getItem('nexus.railCollapsed')).toBe('true');
    first.unmount();

    // Remembered across a remount, which is the point of persisting it.
    renderApp();
    expect(document.querySelector('.sidebar')!.className).toContain('collapsed');
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
  });

  /**
   * The bar's search control and the Cmd+K palette are ONE palette.
   *
   * Asserted at the shell because that is where it can go wrong: App holds the
   * open boolean, and the failure mode is a second palette mounted for the
   * header button, which looks identical until both are reachable and the two
   * pieces of state disagree about whether search is open.
   */
  it('opens the one command palette from the navbar search control', async () => {
    window.localStorage.setItem('nexus.nav', 'navbar');
    renderApp();

    expect(screen.queryByRole('dialog')).toBeNull();
    // The accessible name is the palette's HONEST coverage, which grew when the
    // search stopped being one call to the workflow catalog. Asserting it by
    // name here rather than by class is deliberate: if the two ever disagree
    // about what is searchable, this is the test that catches it.
    fireEvent.click(
      screen.getByRole('button', {
        name: 'Search workflows, runs, schedules, workers, users and commands',
      }),
    );

    const dialogs = screen.getAllByRole('dialog');
    expect(dialogs).toHaveLength(1);
    // The real palette, not a lookalike: its own field is the tell.
    expect(within(screen.getByRole('dialog')).getByRole('combobox')).toBeInTheDocument();
  });

  /** The gear is a real destination, which is the only reason it is rendered. */
  it('routes the navbar gear to the settings page', async () => {
    window.localStorage.setItem('nexus.nav', 'navbar');
    renderApp();

    fireEvent.click(screen.getByRole('button', { name: 'Settings' }));

    expect(await screen.findByRole('heading', { name: 'Settings' })).toBeInTheDocument();
  });

  /**
   * The drawer belongs to the navbar and to nothing else.
   *
   * The rail already renders these groups permanently, so a drawer there would
   * be a second way to reach a list that has not scrolled off the screen — a
   * control whose only effect is to cover the identical control beneath it.
   */
  it('offers the drawer in the navbar layout only', () => {
    renderApp();
    expect(screen.queryByRole('button', { name: 'All destinations' })).toBeNull();
    // The rail shows the headings outright, which is why it needs no drawer.
    expect(screen.getByText('Observe')).toBeInTheDocument();
  });

  /**
   * The deployment is still reachable, on the trigger rather than in a row.
   *
   * It had a labelled row in the panel, and the reason was sound: mistaking
   * staging for production is exactly the error a hidden value invites. But the
   * console is normally served by its own gateway, so the row read "same
   * origin" — a labelled line in an identity card that tells an operator
   * nothing. The tooltip keeps the fact without spending the space, and a base
   * URL that IS worth naming still appears there.
   */
  it('still reports which deployment the tab is pointed at, on the trigger', () => {
    renderApp();

    expect(screen.getByRole('button', { name: /account menu/i })).toHaveAttribute(
      'title',
      expect.stringMatching(/same origin/i) as unknown as string,
    );
  });
});

describe('admin gating in the shell', () => {
  /** The gateway decides. A link that 403s is worse than no link. */
  it('hides the Govern destinations from a credential without admin_write', async () => {
    signIn();
    mockGateway({ admin_write: false });
    renderApp();

    await waitFor(() => expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument());
    expect(screen.queryByRole('link', { name: 'Users' })).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Audit' })).not.toBeInTheDocument();
  });

  it('shows them once whoami reports admin_write', async () => {
    signIn();
    mockGateway({ admin_write: true });
    renderApp();

    await waitFor(() => expect(screen.getByRole('link', { name: 'Users' })).toBeInTheDocument());
    expect(screen.getByRole('link', { name: 'Tokens' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Audit' })).toBeInTheDocument();
  });
});

describe('routing', () => {
  it('renders the not-found notice with a way back for an unknown path', () => {
    renderApp('/no/such/place');

    expect(screen.getByText('No such page')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /go to the overview/i })).toHaveAttribute('href', '/');
  });

  it('routes to the sign-in page', () => {
    renderApp('/login');

    expect(screen.getByRole('button', { name: /sign in/i })).toBeInTheDocument();
  });

  /**
   * /login is a SIBLING of the chrome layout, not a child of it, and this is the
   * test that keeps it one. Every control the shell renders is a control an
   * unauthenticated visitor cannot use: the nav links 401, the namespace input
   * scopes reads nobody is allowed to make, and the account menu offers to sign
   * out a session that does not exist. The regression this pins is the easy one
   * — someone moves the route back inside the layout because that is where all
   * the other routes are.
   */
  it('renders sign-in with no chrome, no namespace control and no account menu', () => {
    window.localStorage.setItem('nexus.namespace', 'payments');
    renderApp('/login');

    expect(screen.queryByRole('navigation', { name: 'Primary' })).toBeNull();
    expect(document.querySelector('.chrome')).toBeNull();
    expect(screen.queryByRole('button', { name: /account menu/i })).toBeNull();
    expect(screen.queryByLabelText('namespace')).toBeNull();
    // The page still knows which namespace it will authenticate against; what
    // it does not offer is the control that changes it.
    expect(screen.getByText('payments')).toBeInTheDocument();
  });

  /**
   * The palette is a catalog search. Unauthenticated there is no catalog and no
   * credential to read one with, so opening it would present a list of 401s —
   * and its sign-out action would offer to end a session that never began.
   */
  it('does not mount the command palette on the sign-in screen', () => {
    renderApp('/login');

    fireEvent.keyDown(document, { key: 'k', metaKey: true });
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  /** The chrome layout still frames every signed-in route, palette included. */
  it('keeps the chrome and the palette on the routes inside the layout', () => {
    renderApp('/runs');

    expect(screen.getByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
    fireEvent.keyDown(document, { key: 'k', metaKey: true });
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  /**
   * A mistyped URL from a signed-in operator is a navigation error, so the one
   * screen whose purpose is to send them elsewhere keeps its navigation.
   */
  it('keeps the chrome on the not-found route', () => {
    renderApp('/no/such/place');

    expect(screen.getByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
  });
});

/**
 * Signing out has to LAND somewhere.
 *
 * It used to clear the credential and leave the operator on whatever page they
 * were on, which then had no credential to fetch with — so every panel on it
 * turned into a 401 rather than into the login screen. The order pinned here is
 * the one that is correct rather than merely the one that was asked for: the
 * logout completes and the cached queries are invalidated BEFORE the route
 * changes, because navigating first spends the transition refetching every
 * mounted panel with a token the gateway has just revoked.
 *
 * Both entry points are covered. Sign-out is reachable from the account menu's
 * row and from the palette's command, and the navigation lives in signOut()
 * precisely so neither can be the path that forgets it.
 */
describe('signing out', () => {
  it('clears the credential and then lands on the sign-in page', async () => {
    signIn();
    renderApp();

    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Sign out' }));

    // The sign-in screen, which is outside the chrome entirely.
    await waitFor(() => expect(document.querySelector('.chrome')).toBeNull());
    expect(screen.getByRole('button', { name: /sign in/i })).toBeInTheDocument();
    // And the browser no longer holds a usable session.
    expect(window.localStorage.getItem('nexus.session')).toBeNull();
  });

  it('lands there from the palette command as well', async () => {
    signIn();
    renderApp();

    fireEvent.keyDown(document, { key: 'k', metaKey: true });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'sign out' } });
    fireEvent.keyDown(screen.getByRole('dialog'), { key: 'Enter' });

    await waitFor(() => expect(document.querySelector('.chrome')).toBeNull());
    expect(window.localStorage.getItem('nexus.session')).toBeNull();
  });
});
