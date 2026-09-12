// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the bulk selection bar and its aggregation helper.
 *
 * What is pinned here is the two settled decisions, because both are the kind
 * that quietly regress: the button must say what it actually does, and a batch
 * must continue past a failure and report which rows failed and why. A bar that
 * stopped at the first error would still look correct in a screenshot.
 */

import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { BulkActionBar, runBulk } from './BulkActionBar';

interface Row {
  id: string;
  name: string;
}

const rows: Row[] = [
  { id: 'a', name: 'nightly' },
  { id: 'b', name: 'hourly' },
  { id: 'c', name: 'weekly' },
];

function bar(props: Partial<React.ComponentProps<typeof BulkActionBar<Row>>> = {}) {
  return render(
    <BulkActionBar<Row>
      selected={rows}
      rowId={(r) => r.id}
      rowLabel={(r) => r.name}
      noun={['schedule', 'schedules']}
      actions={[{ verb: 'Delete', destructive: true, run: async () => undefined }]}
      onClear={() => {}}
      {...props}
    />,
  );
}

describe('runBulk continues past failures and reports per row', () => {
  it('does not stop at the first error', async () => {
    const seen: string[] = [];
    const out = await runBulk(['a', 'b', 'c', 'd'], async (id) => {
      seen.push(id);
      if (id === 'b') throw new Error('conflict');
    });

    // Every row was attempted. Stop-at-first-error would have left c and d
    // untouched for no reason and the operator guessing where it got to.
    expect(seen.sort()).toEqual(['a', 'b', 'c', 'd']);
    expect(out.succeeded).toBe(3);
    expect(out.failed).toBe(1);
  });

  it('reports a result for every row, in input order, with the reason', async () => {
    const out = await runBulk(['a', 'b', 'c'], async (id) => {
      if (id === 'b') throw new Error('still referenced by 4 runs');
    });

    expect(out.results.map((r) => r.id)).toEqual(['a', 'b', 'c']);
    expect(out.results[1]).toEqual({
      id: 'b',
      ok: false,
      error: 'still referenced by 4 runs',
    });
    // The succeeded rows carry no error field to be rendered by accident.
    expect(out.results[0]?.error).toBeUndefined();
  });

  it('never rejects, even when the operation throws a non-Error', async () => {
    // A rejected fetch or a thrown string must not take the whole report with
    // it — the record of which rows DID succeed is the point.
    const out = await runBulk(['a', 'b'], async (id) => {
      if (id === 'a') throw 'plain string rejection';
      if (id === 'b') throw { weird: true };
    });

    expect(out.failed).toBe(2);
    expect(out.results[0]?.error).toBe('plain string rejection');
    // Not 'undefined' — a failure with no readable reason reads as "it failed
    // for no reason" and is the report that gets ignored.
    expect(out.results[1]?.error).toBeTruthy();
    expect(out.results[1]?.error).not.toBe('undefined');
  });

  it('bounds concurrency so a large selection does not fire at once', async () => {
    let inFlight = 0;
    let peak = 0;
    const ids = Array.from({ length: 40 }, (_, i) => String(i));

    await runBulk(
      ids,
      async () => {
        inFlight += 1;
        peak = Math.max(peak, inFlight);
        await new Promise((r) => setTimeout(r, 1));
        inFlight -= 1;
      },
      4,
    );

    // Firing 40 at once would saturate the browser's per-origin pool and trip
    // the gateway's rate limiter, failing rows for a reason unrelated to them.
    expect(peak).toBeLessThanOrEqual(4);
    // And it must actually use the lanes it was given, not run sequentially.
    expect(peak).toBeGreaterThan(1);
  });

  it('handles an empty selection without hanging', async () => {
    const out = await runBulk([], async () => {});
    expect(out).toEqual({ results: [], succeeded: 0, failed: 0 });
  });
});

describe('the bar states the exact action', () => {
  it('names the verb and the count, not a generic delete', () => {
    bar();
    // "Delete 3 schedules", never "Delete selected". The verb is the caller's,
    // because the right action differs per entity: runs are CANCELLED and
    // tokens are REVOKED, and a button that said "delete" for either would be
    // lying about an irreversible-looking operation.
    expect(screen.getByRole('button', { name: 'Delete 3 schedules' })).toBeInTheDocument();
  });

  it('uses the singular when one row is selected', () => {
    bar({ selected: [rows[0]!] });
    expect(screen.getByRole('button', { name: 'Delete 1 schedule' })).toBeInTheDocument();
  });

  it('renders a non-delete verb verbatim', () => {
    bar({
      noun: ['run', 'runs'],
      actions: [{ verb: 'Cancel', destructive: true, run: async () => undefined }],
    });
    expect(screen.getByRole('button', { name: 'Cancel 3 runs' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /delete/i })).not.toBeInTheDocument();
  });
});

describe('destructive actions confirm first', () => {
  it('does not run until the operator confirms', async () => {
    const run = vi.fn().mockResolvedValue(undefined);
    bar({ actions: [{ verb: 'Delete', destructive: true, run }] });

    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    // Nothing has happened yet. A destructive bulk action that fired on the
    // first click would give no chance to notice a mis-click in the table.
    expect(run).not.toHaveBeenCalled();
    expect(screen.getByRole('alertdialog')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: /^Yes, delete 3 schedules$/ }));
    await waitFor(() => expect(run).toHaveBeenCalledTimes(3));
  });

  it('names the consequence, not just the count', () => {
    bar({
      actions: [
        {
          verb: 'Delete',
          destructive: true,
          consequence: 'Their tick history goes with them.',
          run: async () => undefined,
        },
      ],
    });
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    expect(screen.getByRole('alertdialog')).toHaveTextContent('Their tick history goes with them.');
  });

  it('backs out on Cancel without running anything', () => {
    const run = vi.fn();
    bar({ actions: [{ verb: 'Delete', destructive: true, run }] });
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(run).not.toHaveBeenCalled();
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
  });

  it('backs out on Escape, so a keyboard operator is never trapped', () => {
    const run = vi.fn();
    bar({ actions: [{ verb: 'Delete', destructive: true, run }] });
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    fireEvent.keyDown(screen.getByRole('alertdialog'), { key: 'Escape' });
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
    expect(run).not.toHaveBeenCalled();
  });

  it('runs a non-destructive action immediately, with no confirmation', async () => {
    const run = vi.fn().mockResolvedValue(undefined);
    // Draining a worker is reversed by the worker's own next heartbeat cycle,
    // so demanding confirmation would be ceremony that teaches operators to
    // click through the confirmations that DO matter.
    bar({ noun: ['worker', 'workers'], actions: [{ verb: 'Drain', run }] });
    fireEvent.click(screen.getByRole('button', { name: 'Drain 3 workers' }));
    await waitFor(() => expect(run).toHaveBeenCalledTimes(3));
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument();
  });
});

describe('per-row outcomes are reported', () => {
  async function runWithOneFailure() {
    const run = vi.fn(async (r: Row) => {
      if (r.id === 'b') throw new Error('still referenced by 4 runs');
    });
    const onClear = vi.fn();
    bar({ actions: [{ verb: 'Delete', destructive: true, run }], onClear });
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    fireEvent.click(screen.getByRole('button', { name: /^Yes, delete 3 schedules$/ }));
    await screen.findByRole('status');
    return { onClear };
  }

  it('distinguishes succeeded from failed and names both counts', async () => {
    await runWithOneFailure();
    const summary = screen.getByRole('status');
    // Stated separately rather than as "2/3": "1 failed" is the half that needs
    // acting on and must not be arithmetic the reader performs.
    expect(summary).toHaveTextContent('2 succeeded');
    expect(summary).toHaveTextContent('1 failed');
  });

  it('names the failed row and the reason', async () => {
    await runWithOneFailure();
    // The LABEL, not the opaque id: a report saying "b failed" sends the
    // operator back to the table to work out which row that was.
    expect(screen.getByText('hourly')).toBeInTheDocument();
    expect(screen.getByText('still referenced by 4 runs')).toBeInTheDocument();
    // And the rows that worked are not listed as problems.
    expect(screen.queryByText('nightly')).not.toBeInTheDocument();
  });

  it('keeps the failed rows selected so they can be retried', async () => {
    const { onClear } = await runWithOneFailure();
    // Not cleared on a partial failure: clearing would make the operator
    // re-find exactly the rows that still need attention.
    expect(onClear).not.toHaveBeenCalled();
  });

  it('clears the selection when everything succeeded', async () => {
    const onClear = vi.fn();
    bar({
      actions: [{ verb: 'Delete', destructive: true, run: async () => undefined }],
      onClear,
    });
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    fireEvent.click(screen.getByRole('button', { name: /^Yes, delete 3 schedules$/ }));
    await waitFor(() => expect(onClear).toHaveBeenCalled());
  });

  it('lets the operator dismiss the report', async () => {
    await runWithOneFailure();
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    await waitFor(() => expect(screen.queryByRole('status')).not.toBeInTheDocument());
  });

  it('reports the outcome to the caller so it can refetch', async () => {
    const onDone = vi.fn();
    bar({
      actions: [{ verb: 'Delete', destructive: true, run: async () => undefined }],
      onDone,
    });
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    fireEvent.click(screen.getByRole('button', { name: /^Yes, delete 3 schedules$/ }));
    await waitFor(() =>
      expect(onDone).toHaveBeenCalledWith(
        expect.objectContaining({ succeeded: 3, failed: 0 }),
      ),
    );
  });
});

describe('accessibility and keyboard operation', () => {
  it('renders nothing when nothing is selected', () => {
    const { container } = bar({ selected: [] });
    expect(container).toBeEmptyDOMElement();
  });

  it('gives the bar an accessible name', () => {
    bar();
    expect(
      screen.getByRole('region', { name: 'Actions for selected schedules' }),
    ).toBeInTheDocument();
  });

  it('gives every control an accessible name', () => {
    bar();
    for (const b of screen.getAllByRole('button')) {
      expect(b).toHaveAccessibleName();
    }
  });

  it('moves focus to the confirm button so the dialog is reachable by keyboard', async () => {
    bar();
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 schedules' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /^Yes, delete 3 schedules$/ })).toHaveFocus(),
    );
  });
});
