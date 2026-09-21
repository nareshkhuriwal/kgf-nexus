// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Bulk revocation on the tokens screen — the worked example for the selection
 * pattern.
 *
 * In its own file rather than added to admin.test.tsx, which is shared: this
 * covers one feature across one screen, and keeping it separate means the
 * existing admin coverage is untouched.
 *
 * What is pinned is the honesty of the whole flow: that the action is called
 * REVOKE and not delete, that a revoked credential cannot be selected (so the
 * count never over-promises), and that a partial failure reports which token
 * failed and why rather than a bare "some failed".
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { WithPermissions } from '../../test/permissions';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { TokensPage } from './TokensPage';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <WithPermissions><MemoryRouter>{ui}</MemoryRouter></WithPermissions>
    </QueryClientProvider>,
  );
}

function token(id: string, name: string, revoked = false) {
  return {
    id,
    namespace: 'default',
    name,
    scopes: ['read'],
    role: 'reader',
    expires_at: '2026-12-12T00:00:00Z',
    last_used_at: null,
    created_by: null,
    created_at: '2026-09-12T10:00:00Z',
    revoked_at: revoked ? '2026-09-10T10:00:00Z' : null,
    live: !revoked,
  };
}

/** Records every revoke the page issued, and lets a test fail chosen ones. */
function mockTokens(
  tokens: ReturnType<typeof token>[],
  failFor: string[] = [],
): { revoked: string[] } {
  const revoked: string[] = [];
  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    const u = String(url);
    const m = /\/tokens\/([^/]+)\/revoke$/.exec(u);
    if (m && init?.method === 'POST') {
      const id = m[1]!;
      revoked.push(id);
      if (failFor.includes(id)) {
        return Promise.resolve(
          new Response(
            JSON.stringify({ error: 'forbidden', message: 'not permitted for this namespace' }),
            { status: 403, headers: { 'Content-Type': 'application/json' } },
          ),
        );
      }
      return Promise.resolve(
        new Response(JSON.stringify({ token_id: id, revoked: true }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
      );
    }
    return Promise.resolve(
      new Response(JSON.stringify({ tokens }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return { revoked };
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('selecting tokens', () => {
  it('shows no bulk bar until something is selected', async () => {
    mockTokens([token('t1', 'ci-pipeline')]);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('ci-pipeline');
    expect(screen.queryByRole('region', { name: /Actions for selected/ })).not.toBeInTheDocument();
  });

  it('offers REVOKE, never delete', async () => {
    mockTokens([token('t1', 'ci-pipeline'), token('t2', 'backup-job')]);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('ci-pipeline');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select ci-pipeline' }));

    // The store has no delete for a token and must not: the row is the audit
    // evidence that the credential existed and when it was last used.
    expect(await screen.findByRole('button', { name: 'Revoke 1 token' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
  });

  it('does not offer a revoked token for selection', async () => {
    mockTokens([token('t1', 'ci-pipeline'), token('t2', 'dead-key', true)]);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('dead-key');

    // A revoked credential cannot be revoked again, so including it would let
    // the count over-promise what the action will achieve.
    expect(screen.queryByRole('checkbox', { name: 'Select dead-key' })).not.toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Select ci-pipeline' })).toBeInTheDocument();
  });

  it('select-all claims only the live tokens, and says how many', async () => {
    mockTokens([token('t1', 'a'), token('t2', 'b'), token('t3', 'dead', true)]);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('dead');

    /*
      Two live of three rows, and the accessible name carries BOTH the number
      and its scope. "2" rather than "3" is the eligibility rule: a revoked
      credential is not selectable, so select-all must not count it.

      "tokens" with no "loaded" qualifier is the honesty rule, and it is true
      here for a specific reason — /tokens takes no `limit`, so the page passes
      count `exact` and the client genuinely holds every credential in the
      namespace. A capped table's control says "loaded" instead; see the
      schedules and runs tests.
    */
    const all = await screen.findByRole('checkbox', { name: 'Select all 2 tokens' });

    fireEvent.click(all);
    expect(await screen.findByRole('button', { name: 'Revoke 2 tokens' })).toBeInTheDocument();
  });

  it('marks select-all indeterminate when only some rows are selected', async () => {
    mockTokens([token('t1', 'a'), token('t2', 'b')]);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('a');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select a' }));

    // Unchecked would claim "nothing is selected" while the bar below says
    // otherwise, leaving the operator to choose which control to believe.
    const all = screen.getByRole('checkbox', {
      name: 'Select all 2 tokens',
    }) as HTMLInputElement;
    await waitFor(() => expect(all.indeterminate).toBe(true));
    expect(all.checked).toBe(false);
  });
});

describe('bulk revoking', () => {
  async function selectBothAndConfirm(failFor: string[] = []) {
    const calls = mockTokens([token('t1', 'ci-pipeline'), token('t2', 'backup-job')], failFor);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('ci-pipeline');

    fireEvent.click(await screen.findByRole('checkbox', { name: 'Select all 2 tokens' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Revoke 2 tokens' }));
    // Confirmation first: a destructive bulk action that fired on the first
    // click would give no chance to notice a mis-click in the table.
    expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Yes, revoke 2 tokens$/ }));
    return calls;
  }

  it('confirms, then issues one request per token', async () => {
    const calls = await selectBothAndConfirm();
    await waitFor(() => expect(calls.revoked.sort()).toEqual(['t1', 't2']));
  });

  it('names what is lost in the confirmation', async () => {
    mockTokens([token('t1', 'ci-pipeline')]);
    wrap(<TokensPage namespace="default" canAdmin />);
    await screen.findByText('ci-pipeline');
    fireEvent.click(screen.getByRole('checkbox', { name: 'Select ci-pipeline' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Revoke 1 token' }));

    // "Revoke 1 token" alone does not tell an operator that whatever is using
    // the credential starts failing to authenticate the moment they confirm.
    expect(await screen.findByRole('alertdialog')).toHaveTextContent(
      /cannot be restored|failing to authenticate/i,
    );
  });

  it('completes the rows that worked when one fails, and names the one that did not', async () => {
    const calls = await selectBothAndConfirm(['t2']);

    const summary = await screen.findByRole('status');
    expect(summary).toHaveTextContent('1 succeeded');
    expect(summary).toHaveTextContent('1 failed');

    // The token that worked stayed done -- not rolled back, which could not be
    // done anyway, and not abandoned.
    await waitFor(() => expect(calls.revoked.sort()).toEqual(['t1', 't2']));

    // And the failure names the row AND the reason the operator can act on.
    // Scoped to the bar, because the token's name is legitimately also in the
    // table row behind it -- the report is what is being asserted here.
    const bar = screen.getByRole('region', { name: /Actions for selected/ });
    expect(within(bar).getByText('backup-job')).toBeInTheDocument();
    expect(within(bar).getByText(/not permitted for this namespace/i)).toBeInTheDocument();
    // The token that succeeded is NOT listed as a problem.
    expect(within(bar).queryByText('ci-pipeline')).not.toBeInTheDocument();
  });
});
