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

  it('renders the navbar when that layout is stored', () => {
    window.localStorage.setItem('nexus.nav', 'navbar');
    renderApp();

    expect(screen.getByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
    // Flattened: same destinations, no headings.
    expect(screen.queryByText('Observe')).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
  });

  /**
   * The namespace is displayed in the chrome and edited in the menu. Its value
   * has to be visible at all times: an operator reading an empty dashboard must
   * be able to tell "nothing is running" from "wrong namespace".
   */
  it('shows the namespace in the chrome and stores what is typed', async () => {
    window.localStorage.setItem('nexus.namespace', 'payments');
    renderApp();

    // Scoped to the chrome: pages may legitimately print the namespace too, and
    // the claim here is specifically that the SHELL always shows it.
    const chrome = document.querySelector('.chrome') as HTMLElement;
    expect(within(chrome).getByText('payments')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));
    const input = screen.getByLabelText('namespace');
    expect(input).toHaveValue('payments');

    fireEvent.change(input, { target: { value: 'billing' } });

    await waitFor(() =>
      expect(window.localStorage.getItem('nexus.namespace')).toBe('billing'),
    );
  });

  it('keeps the API-key control in the account menu', () => {
    renderApp();
    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));

    expect(screen.getByRole('button', { name: /set api key/i })).toBeInTheDocument();
  });

  it('reports the key as set once one is stored, and saves what is typed', async () => {
    renderApp();
    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));
    fireEvent.click(screen.getByRole('button', { name: /set api key/i }));

    fireEvent.change(screen.getByLabelText('API key'), { target: { value: 'k-123' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(screen.getByRole('button', { name: /api key ✓/i })).toBeInTheDocument());
  });

  /** The appearance panel had to reach the menu; this is its home. */
  it('puts the appearance panel inside the account menu', () => {
    renderApp();

    expect(screen.queryByRole('radiogroup', { name: 'Accent' })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));

    expect(screen.getByRole('radiogroup', { name: 'Accent' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: 'Layout' })).toBeInTheDocument();
    expect(screen.getByRole('radiogroup', { name: /colour theme/i })).toBeInTheDocument();
  });

  /** Choosing a layout in the menu must reshape the shell, not just a token. */
  it('switches the chrome when the layout is changed in the menu', async () => {
    renderApp();
    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));
    expect(screen.getByText('Observe')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('radio', { name: 'Navbar' }));
    // The menu is closed, which is what tells App to re-read the layout.
    fireEvent.keyDown(document, { key: 'Escape' });

    await waitFor(() => expect(screen.queryByText('Observe')).not.toBeInTheDocument());
    expect(screen.getByRole('link', { name: 'Runs' })).toBeInTheDocument();
  });

  it('still shows which deployment the tab is pointed at', () => {
    renderApp();
    fireEvent.click(screen.getByRole('button', { name: /account menu/i }));

    expect(screen.getByText(/same origin/i)).toBeInTheDocument();
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
    // The shell still frames it: chrome is present on every route.
    expect(screen.getByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
  });
});
