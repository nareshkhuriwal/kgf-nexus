// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Bulk delete on the workflow catalog — and specifically, the REFUSAL.
 *
 * In its own file rather than added to WorkflowsPage.test.tsx, which covers the
 * list itself: this is one feature, and keeping it separate leaves the existing
 * catalog coverage untouched.
 *
 * The case that earns a file of its own is the 409. A workflow with run history
 * or a live schedule pointing at it CANNOT be deleted, and the gateway refuses
 * with `workflow_referenced` naming `referencing_runs` and
 * `referencing_schedules`. That refusal is not a platform failure — it is the
 * answer — and the operator's next move depends entirely on WHICH blocker it is:
 * a schedule can be removed first, run history cannot and means the workflow
 * stays. So what is pinned here is that the blocker REACHES THE OPERATOR:
 *
 *   - in a bulk delete of several workflows where some are referenced, the
 *     referenced ones report WHY, naming the runs and the schedules;
 *   - two rows blocked by different things report different reasons, not one
 *     generic "failed" repeated;
 *   - the rows that succeeded STAY DONE — a partial failure is not rolled back;
 *   - a refused row is still on screen afterwards, because the page refetches
 *     rather than optimistically removing what it asked to delete.
 */

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ReactNode } from 'react';
import { WorkflowsPage } from './WorkflowsPage';
import type { WorkflowSummary } from '../api/types';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

const workflow = (name: string): WorkflowSummary => ({
  id: `spec-${name}`,
  namespace: 'default',
  name,
  latest_version: 1,
  spec_hash: 'b12cd313da964e723a3236ca3bd60723',
  created_by: 'ops',
  updated_at: '2026-09-12T09:52:52Z',
  version_count: 1,
  first_created_at: '2026-09-12T08:44:15Z',
});

/** What the gateway answers for one DELETE, keyed by workflow name. */
type Refusal =
  | { kind: 'ok' }
  | { kind: 'referenced'; runs: number; schedules: string[] }
  | { kind: 'error'; status: number; message: string };

/**
 * Serves the catalog and records every DELETE, answering each per the map.
 *
 * The list it serves SHRINKS as deletes succeed, which is what makes "the page
 * refetched and the refused row is still there" a real assertion rather than a
 * restatement of the initial render.
 */
function mockCatalog(names: string[], outcomes: Record<string, Refusal> = {}) {
  const deleted: string[] = [];
  let live = [...names];

  vi.stubGlobal('fetch', (url: string, init?: RequestInit) => {
    const u = String(url);
    const m = /\/specs\/([^/?]+)$/.exec(u);
    if (m && init?.method === 'DELETE') {
      const name = decodeURIComponent(m[1]!);
      deleted.push(name);
      const outcome = outcomes[name] ?? { kind: 'ok' };
      if (outcome.kind === 'referenced') {
        return Promise.resolve(
          new Response(
            JSON.stringify({
              error: 'workflow_referenced',
              message: 'workflow is still referenced',
              referencing_runs: outcome.runs,
              referencing_schedules: outcome.schedules,
            }),
            { status: 409, headers: { 'Content-Type': 'application/json' } },
          ),
        );
      }
      if (outcome.kind === 'error') {
        return Promise.resolve(
          new Response(JSON.stringify({ error: 'failed', message: outcome.message }), {
            status: outcome.status,
            headers: { 'Content-Type': 'application/json' },
          }),
        );
      }
      live = live.filter((n) => n !== name);
      return Promise.resolve(
        new Response(
          JSON.stringify({ namespace: 'default', name, versions_deleted: 1, deleted: true }),
          { status: 200, headers: { 'Content-Type': 'application/json' } },
        ),
      );
    }
    return Promise.resolve(
      new Response(JSON.stringify({ workflows: live.map(workflow) }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    );
  });
  return { deleted, remaining: () => live };
}

/**
 * The failure line for one row, looked up inside the REPORT rather than the
 * page: a workflow's name appears both in its table row and in the report, so a
 * document-wide query would be ambiguous — and matching the table row instead of
 * the report would assert nothing about what the operator was told.
 */
function failureFor(name: string): HTMLElement {
  const list = document.querySelector('.bulk-bar-failures')!;
  const line = within(list as HTMLElement).getByText(name).closest('li');
  expect(line).not.toBeNull();
  return line as HTMLElement;
}

/** Selects every row, presses Delete, and confirms. */
async function deleteAll(n: number) {
  fireEvent.click(await screen.findByRole('checkbox', { name: `Select all ${n} loaded workflows` }));
  fireEvent.click(await screen.findByRole('button', { name: `Delete ${n} workflows` }));
  // Destructive: the confirmation comes first, and it states the exact count
  // and noun rather than "Are you sure?".
  expect(await screen.findByRole('alertdialog')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: new RegExp(`^Yes, delete ${n} workflows$`) }));
}

beforeEach(() => {
  window.localStorage.clear();
  window.localStorage.setItem('nexus.apiKey', 'nxs_admin_test');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('deleting workflows in bulk', () => {
  it('offers DELETE, the action that is true for a workflow spec', async () => {
    mockCatalog(['alpha', 'beta']);
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select alpha' }));
    expect(await screen.findByRole('button', { name: 'Delete 1 workflow' })).toBeInTheDocument();
  });

  it('confirms with the exact count and noun before deleting anything', async () => {
    const calls = mockCatalog(['alpha', 'beta']);
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    fireEvent.click(await screen.findByRole('checkbox', { name: 'Select all 2 loaded workflows' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Delete 2 workflows' }));

    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Delete 2 workflows?')).toBeInTheDocument();
    // And nothing has been issued yet: a destructive bulk action that fired on
    // the first click would give no chance to notice a mis-click in the table.
    expect(calls.deleted).toEqual([]);
  });

  it('names what else goes with it, because a version count is not recoverable', async () => {
    mockCatalog(['alpha']);
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select alpha' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Delete 1 workflow' }));

    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText(/every version/i)).toBeInTheDocument();
  });

  /**
   * THE CASE THIS FILE EXISTS FOR.
   *
   * Three workflows, one referenced by runs and a schedule. The refusal must
   * name the blocker — "29 runs", "nightly-etl" — because that is the
   * difference between an operator who knows their next step and one who goes
   * back to the table to guess.
   */
  it('reports WHY a referenced workflow was refused, naming the blockers', async () => {
    mockCatalog(['alpha', 'beta', 'gamma'], {
      beta: { kind: 'referenced', runs: 29, schedules: ['nightly-etl'] },
    });
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    await deleteAll(3);

    await screen.findByText('2 succeeded');
    expect(screen.getByText('1 failed')).toBeInTheDocument();

    // The failure line names the ROW and the REASON, and the reason names both
    // blockers the gateway reported.
    const failure = failureFor('beta');
    expect(failure).toHaveTextContent(/29 runs/);
    expect(failure).toHaveTextContent(/nightly-etl/);
    // Not a bare status code: "409" tells an operator nothing they can act on.
    expect(failure).not.toHaveTextContent(/^409$/);
  });

  it('gives two blocked rows their own reasons rather than one generic failure', async () => {
    mockCatalog(['alpha', 'beta', 'gamma'], {
      alpha: { kind: 'referenced', runs: 4, schedules: [] },
      beta: { kind: 'referenced', runs: 0, schedules: ['nightly-etl', 'hourly-roll'] },
    });
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    await deleteAll(3);
    await screen.findByText('1 succeeded');

    // alpha is blocked by run history — which is final, the runs are immutable.
    const alpha = failureFor('alpha');
    expect(alpha).toHaveTextContent(/4 runs/);
    expect(alpha).not.toHaveTextContent(/schedule/);

    // beta is blocked by schedules, which the operator CAN remove first — so
    // they are named, not counted.
    const beta = failureFor('beta');
    expect(beta).toHaveTextContent(/nightly-etl/);
    expect(beta).toHaveTextContent(/hourly-roll/);
    expect(beta).not.toHaveTextContent(/0 runs/);
  });

  /**
   * PARTIAL FAILURE CONTINUES. The rows that worked stay done — there is no
   * rollback, and pretending otherwise would be a lie about atomicity across N
   * HTTP calls.
   */
  it('completes the workflows that worked when one is refused', async () => {
    const calls = mockCatalog(['alpha', 'beta', 'gamma'], {
      beta: { kind: 'referenced', runs: 29, schedules: ['nightly-etl'] },
    });
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    await deleteAll(3);
    await screen.findByText('2 succeeded');

    // Every row was attempted: it did not stop at the first refusal.
    expect(calls.deleted.sort()).toEqual(['alpha', 'beta', 'gamma']);
    // And the two that worked are gone for good.
    expect(calls.remaining()).toEqual(['beta']);
  });

  /**
   * A REFUSED ROW IS STILL ON SCREEN. The page refetches rather than removing
   * rows optimistically: a delete the gateway refused must not vanish from a
   * table that then disagrees with the failure report directly above it.
   */
  it('leaves the refused workflow in the table after the batch', async () => {
    mockCatalog(['alpha', 'beta'], {
      beta: { kind: 'referenced', runs: 29, schedules: ['nightly-etl'] },
    });
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    await deleteAll(2);
    await screen.findByText('1 succeeded');

    await waitFor(() => {
      // alpha really went; beta is still a row, because it still exists.
      expect(document.querySelector('tbody')).not.toHaveTextContent('alpha');
    });
    expect(document.querySelector('tbody')).toHaveTextContent('beta');
  });

  /** A non-409 failure still reports its own reason, per row. */
  it('reports an ordinary failure against the row it belongs to', async () => {
    mockCatalog(['alpha', 'beta'], {
      beta: { kind: 'error', status: 403, message: 'not permitted for this namespace' },
    });
    wrap(<WorkflowsPage namespace="default" />);
    await screen.findByText('alpha');

    await deleteAll(2);
    await screen.findByText('1 succeeded');

    expect(failureFor('beta')).toHaveTextContent(/not permitted/);
  });
});
