// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the admin surface.
 *
 * These cover the states the brief calls out as non-negotiable: permission
 * denied, empty, error, and the one-time token reveal. They drive the REAL
 * components against a mocked fetch, so a control that renders but is wired to
 * nothing fails here rather than in front of an operator.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WithPermissions } from '../../test/permissions';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { UsersPage } from './UsersPage';
import { TokensPage } from './TokensPage';
import { AuditPage } from './AuditPage';

function wrap(ui: ReactNode) {
  // retry:false so an error state renders immediately rather than after backoff.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <WithPermissions><MemoryRouter>{ui}</MemoryRouter></WithPermissions>
    </QueryClientProvider>,
  );
}

function mockFetch(handler: (url: string, init?: RequestInit) => [number, unknown]) {
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    const [status, body] = handler(String(url), init);
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('permission-denied states', () => {
  // A 403 is an ANSWER, not a retryable failure. Each admin screen must say what
  // is missing rather than render a red panel with a Retry button that cannot help.
  it.each([
    ['users', <UsersPage namespace="default" canAdmin={false} />],
    ['tokens', <TokensPage namespace="default" canAdmin={false} />],
    ['audit', <AuditPage namespace="default" canAdmin={false} />],
  ])('%s explains the missing permission and issues no request', async (_name, ui) => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);

    wrap(ui);

    expect(await screen.findByText(/Not permitted/i)).toBeInTheDocument();
    // No request is attempted: firing one we know will 403 just to render an
    // error is noise in the gateway log and in the audit trail.
    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe('UsersPage', () => {
  it('renders the account list from the API', async () => {
    mockFetch(() => [
      200,
      {
        users: [
          {
            id: 'u1',
            namespace: 'default',
            email: 'ops@example.com',
            display_name: 'Ops',
            role: 'admin',
            created_at: '2026-09-12T10:00:00Z',
            disabled_at: null,
            disabled: false,
          },
          {
            id: 'u2',
            namespace: 'default',
            email: 'gone@example.com',
            display_name: '',
            role: 'reader',
            created_at: '2026-09-11T10:00:00Z',
            disabled_at: '2026-09-12T09:00:00Z',
            disabled: true,
          },
        ],
      },
    ]);

    wrap(<UsersPage namespace="default" canAdmin />);

    expect(await screen.findByText('ops@example.com')).toBeInTheDocument();
    expect(screen.getByText('gone@example.com')).toBeInTheDocument();
    // Status is rendered from the server-derived `disabled` flag, not re-derived.
    expect(screen.getByText('disabled')).toBeInTheDocument();
    expect(screen.getByText('active')).toBeInTheDocument();
  });

  it('pages accounts with the server cursor instead of claiming the first page is everyone', async () => {
    const user = (id: string, email: string) => ({
      id, namespace: 'default', email, display_name: '', role: 'reader',
      created_at: '2026-09-12T10:00:00Z', disabled_at: null, disabled: false,
    });
    const seen: string[] = [];
    mockFetch((url) => {
      seen.push(url);
      return url.includes('cursor=c2')
        ? [200, { users: [user('u2', 'second@example.com')], next_cursor: null }]
        : [200, { users: [user('u1', 'first@example.com')], next_cursor: 'c2' }];
    });

    wrap(<UsersPage namespace="default" canAdmin />);

    expect(await screen.findByText('first@example.com')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Load more accounts' }));
    expect(await screen.findByText('second@example.com')).toBeInTheDocument();
    expect(seen.some((u) => u.includes('limit=100'))).toBe(true);
    expect(screen.queryByRole('button', { name: 'Load more accounts' })).toBeNull();
  });

  it('teaches rather than blanking when there are no users', async () => {
    mockFetch(() => [200, { users: [] }]);
    wrap(<UsersPage namespace="default" canAdmin />);
    expect(await screen.findByText(/No user accounts yet/i)).toBeInTheDocument();
    // The empty state explains what the system is doing instead, matching the
    // quality of RunListPage's empty state.
    expect(screen.getByText(/Only API keys can authenticate/i)).toBeInTheDocument();
  });

  it('surfaces a server failure instead of rendering an empty table', async () => {
    mockFetch(() => [500, { error: 'internal', message: 'request failed' }]);
    wrap(<UsersPage namespace="default" canAdmin />);

    // The page sets its own `retry: shouldRetry`, which retries a 5xx twice
    // (api/hooks.ts: a service restarting under Compose is the common case), so
    // the error state is reached only after those attempts. Waiting for it here
    // asserts the REAL retry policy rather than a test-only one.
    expect(
      await screen.findByText(/Could not load users/i, {}, { timeout: 5000 }),
    ).toBeInTheDocument();
  });

  it('does not retry a 403, so a permission error surfaces immediately', async () => {
    // shouldRetry treats 4xx as final: retrying a refusal four times only delays
    // an honest message by several seconds.
    let calls = 0;
    mockFetch(() => {
      calls++;
      return [403, { error: 'forbidden', message: 'not permitted for this namespace' }];
    });
    wrap(<UsersPage namespace="default" canAdmin />);

    expect(await screen.findByText(/Could not load users/i)).toBeInTheDocument();
    expect(calls).toBe(1);
  });

  /**
   * A successful administrative action is reported in the success tone, and it
   * reports the CONSEQUENCE rather than the fact that a request returned. An
   * offboarding needs "3 sessions ended" as evidence; "done" is not evidence.
   */
  it('reports the session count as evidence after signing an account out', async () => {
    const { fireEvent } = await import('@testing-library/react');
    mockFetch((url, init) => {
      if (init?.method === 'POST' && url.includes('/sessions')) {
        return [200, { sessions_revoked: 3 }];
      }
      return [
        200,
        {
          users: [
            {
              id: 'u1',
              namespace: 'default',
              email: 'ops@example.com',
              display_name: 'Ops',
              role: 'admin',
              created_at: '2026-09-12T10:00:00Z',
              disabled_at: null,
              disabled: false,
            },
          ],
        },
      ];
    });
    wrap(<UsersPage namespace="default" canAdmin />);

    fireEvent.click(await screen.findByRole('button', { name: /sign out everywhere/i }));

    const notice = await screen.findByRole('status');
    expect(notice).toHaveTextContent(/Ended 3 sessions/i);
    // Success-toned from the state palette, not from the operator-chosen accent.
    expect(notice).toHaveClass('admin-notice');
  });

  it('warns that disabling also ends live sessions, before the click', async () => {
    mockFetch(() => [
      200,
      {
        users: [
          {
            id: 'u1',
            namespace: 'default',
            email: 'ops@example.com',
            display_name: 'Ops',
            role: 'admin',
            created_at: '2026-09-12T10:00:00Z',
            disabled_at: null,
            disabled: false,
          },
        ],
      },
    ]);
    wrap(<UsersPage namespace="default" canAdmin />);

    // The control is an icon action now, so its accessible name is the
    // aria-label rather than visible text. The title it carries -- the point of
    // this test -- is unchanged.
    const disable = await screen.findByRole('button', { name: 'Disable user' });
    // The consequence is the reason an operator is on this screen during an
    // offboarding, so it is stated up front rather than reported afterwards.
    expect(disable).toHaveAttribute('title', expect.stringContaining('end every session'));
  });
});

describe('TokensPage', () => {
  it('shows last-used, the column api_keys could never answer', async () => {
    mockFetch(() => [
      200,
      {
        tokens: [
          {
            id: 't1',
            namespace: 'default',
            name: 'ci-pipeline',
            scopes: ['read', 'write'],
            role: 'admin',
            expires_at: '2026-12-12T00:00:00Z',
            last_used_at: null,
            created_by: null,
            created_at: '2026-09-12T10:00:00Z',
            revoked_at: null,
            live: true,
          },
        ],
      },
    ]);

    wrap(<TokensPage namespace="default" canAdmin />);

    expect(await screen.findByText('ci-pipeline')).toBeInTheDocument();
    expect(screen.getByText('read')).toBeInTheDocument();
    expect(screen.getByText('write')).toBeInTheDocument();
    expect(screen.getByText('live')).toBeInTheDocument();
    // A token that has never authenticated says so, rather than showing a blank.
    expect(screen.getByTitle(/never authenticated/i)).toBeInTheDocument();
  });

  it('marks a revoked token and disables its revoke button', async () => {
    mockFetch(() => [
      200,
      {
        tokens: [
          {
            id: 't1',
            namespace: 'default',
            name: 'old',
            scopes: ['read'],
            role: 'reader',
            expires_at: null,
            last_used_at: null,
            created_by: null,
            created_at: '2026-09-01T10:00:00Z',
            revoked_at: '2026-09-10T10:00:00Z',
            live: false,
          },
        ],
      },
    ]);

    wrap(<TokensPage namespace="default" canAdmin />);

    expect(await screen.findByText('revoked')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Revoke token' })).toBeDisabled();
  });

  it('explains that an empty token list is not an error', async () => {
    mockFetch(() => [200, { tokens: [] }]);
    wrap(<TokensPage namespace="default" canAdmin />);
    expect(await screen.findByText(/No scoped tokens/i)).toBeInTheDocument();
  });

  /**
   * The one-time reveal, and the reason it is the most dangerous panel in the
   * console to restyle: only a hash is stored, so an operator who scrolls past
   * this has permanently lost the credential.
   *
   * What is pinned is that the plaintext appears EXACTLY once, in the reveal
   * panel, and that the listing renders nothing of it. A restyle that helpfully
   * added the value to a table cell or a tooltip would pass every other test in
   * this file.
   */
  it('reveals a new token exactly once and never in the listing', async () => {
    const { fireEvent } = await import('@testing-library/react');
    const SECRET = 'nxs_scoped_PLAINTEXT_ONLY_ONCE';
    mockFetch((_url, init) => {
      if (init?.method === 'POST') {
        return [
          201,
          {
            id: 't9',
            namespace: 'default',
            name: 'ci-pipeline',
            scopes: ['read'],
            role: 'reader',
            expires_at: '2026-12-12T00:00:00Z',
            last_used_at: null,
            created_by: null,
            created_at: '2026-09-12T10:00:00Z',
            revoked_at: null,
            live: true,
            // The gateway returns the plaintext on CREATE and never again.
            token: SECRET,
          },
        ];
      }
      // The listing is what a reload shows: no `token` field at all.
      return [
        200,
        {
          tokens: [
            {
              id: 't9',
              namespace: 'default',
              name: 'ci-pipeline',
              scopes: ['read'],
              role: 'reader',
              expires_at: '2026-12-12T00:00:00Z',
              last_used_at: null,
              created_by: null,
              created_at: '2026-09-12T10:00:00Z',
              revoked_at: null,
              live: true,
            },
          ],
        },
      ];
    });

    wrap(<TokensPage namespace="default" canAdmin />);

    fireEvent.click(await screen.findByRole('button', { name: /new token/i }));
    fireEvent.change(screen.getByPlaceholderText('ci-pipeline'), {
      target: { value: 'ci-pipeline' },
    });
    fireEvent.click(screen.getByRole('button', { name: /create token/i }));

    // Shown once, and the panel says so as an ALERT rather than a passive
    // status: missing this message costs the credential permanently.
    const reveal = await screen.findByText(SECRET);
    expect(reveal).toBeInTheDocument();
    expect(await screen.findByRole('alert')).toHaveTextContent(/Shown once and never again/i);

    // Dismissed, and then it is gone from the document entirely — the listing
    // beneath it never carried the value.
    fireEvent.click(screen.getByRole('button', { name: /I have saved it/i }));
    await waitFor(() => {
      expect(screen.queryByText(SECRET)).not.toBeInTheDocument();
    });
    // Nor in any title/tooltip, which is the quiet way a secret leaks.
    expect(document.body.innerHTML).not.toContain(SECRET);
  });
});

describe('AuditPage', () => {
  const entries = [
    {
      id: 2,
      namespace: 'default',
      actor_id: null,
      actor_kind: 'anonymous',
      actor_label: 'ghost@example.com',
      action: 'session.login',
      target_kind: '',
      target_id: '',
      outcome: 'denied',
      detail: { reason: 'invalid credentials' },
      client_ip: '10.0.0.9',
      occurred_at: '2026-09-12T11:00:00Z',
    },
    {
      id: 1,
      namespace: 'default',
      actor_id: 'u1',
      actor_kind: 'session',
      actor_label: 'ops@example.com',
      action: 'user.create',
      target_kind: 'user',
      target_id: 'u2',
      outcome: 'success',
      detail: { role: 'reader' },
      client_ip: '10.0.0.1',
      occurred_at: '2026-09-12T10:00:00Z',
    },
  ];

  it('renders the trail with actor kind, not just a name', async () => {
    mockFetch(() => [200, { entries }]);
    wrap(<AuditPage namespace="default" canAdmin />);

    expect(await screen.findByText('session.login')).toBeInTheDocument();
    expect(screen.getByText('user.create')).toBeInTheDocument();
    // An action from a browser session and the same action from a long-lived CI
    // key are different facts during a review, so the kind is shown.
    expect(screen.getByText(/session · 10\.0\.0\.1/)).toBeInTheDocument();
    expect(screen.getByText(/anonymous · 10\.0\.0\.9/)).toBeInTheDocument();
  });

  it('shows a denied outcome as prominently as a success', async () => {
    mockFetch(() => [200, { entries }]);
    wrap(<AuditPage namespace="default" canAdmin />);
    // A burst of denials is what an attack in progress looks like, so it must be
    // visible rather than filtered out by default.
    expect(await screen.findByText('denied')).toBeInTheDocument();
    expect(screen.getByText('success')).toBeInTheDocument();
  });

  it('sends the outcome filter to the server', async () => {
    const seen: string[] = [];
    mockFetch((url) => {
      seen.push(url);
      return [200, { entries: [] }];
    });

    wrap(<AuditPage namespace="default" canAdmin />);
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));

    // Filtering happens in SQL, not in the browser: the trail can be long, and
    // filtering client-side would mean fetching everything to show a little.
    const { fireEvent } = await import('@testing-library/react');
    fireEvent.click(screen.getByRole('button', { name: /Show denials/i }));

    await waitFor(() => {
      expect(seen.some((u) => u.includes('outcome=denied'))).toBe(true);
    });
  });

  it('distinguishes "no events" from "no matches"', async () => {
    mockFetch(() => [200, { entries: [] }]);
    wrap(<AuditPage namespace="default" canAdmin />);
    // With no filter applied the message must not imply the filter hid something.
    expect(await screen.findByText(/No administrative events yet/i)).toBeInTheDocument();
  });
});
