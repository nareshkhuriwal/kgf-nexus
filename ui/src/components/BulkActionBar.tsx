import { usePermission } from './Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The bar that appears when table rows are selected, and the helper that
 * actually performs the action across them.
 *
 * TWO DESIGN DECISIONS ARE BAKED IN HERE AND BOTH ARE DELIBERATE.
 *
 * 1. THE BUTTON SAYS WHAT IT DOES. There is no generic "delete" for every
 *    table. Deleting a run would destroy immutable execution history the audit
 *    trail references; deleting a worker row is meaningless because a live agent
 *    re-registers on its next heartbeat. So the right action differs per entity
 *    — schedules and workflow specs are DELETED, tokens are REVOKED, runs are
 *    CANCELLED, workers are DRAINED — and the caller passes the verb. A bar that
 *    said "Delete 5 runs" and then cancelled them would be lying about an
 *    irreversible-looking operation, which is the worst place to be imprecise.
 *
 * 2. IT CONTINUES ON FAILURE AND REPORTS PER ROW. Select ten, three fail: the
 *    seven that worked stay done and the operator is shown exactly which three
 *    failed and why. The alternatives are both worse. All-or-nothing would
 *    require a rollback the platform cannot perform (you cannot un-cancel a run,
 *    and there is no bulk transaction across N HTTP calls), so it would only
 *    ever be a lie about atomicity. Stop-at-first-error abandons the remaining
 *    rows for no reason and leaves the operator guessing where it got to.
 *
 * WHY NO /bulk ENDPOINT. The client issues N requests and aggregates. That needs
 * no new server surface, keeps each action's EXISTING authorization and audit
 * behaviour exactly as it is (a bulk endpoint would have to re-implement both,
 * and one that got it subtly wrong would be an authorization hole), and gives
 * per-row results for free — the response to each call already IS the per-row
 * result. The only thing a bulk endpoint would buy is fewer round trips, which
 * is not a problem an operator selecting rows in a table has.
 */

import { useEffect, useRef, useState } from 'react';

/**
 * The outcome of one row's operation.
 *
 * `error` is the operator-facing reason, not a code: this string is rendered
 * next to the row's label, and "403" tells them nothing they can act on while
 * "not permitted for this namespace" tells them to get a different credential.
 */
export interface BulkResult {
  id: string;
  ok: boolean;
  error?: string;
}

/** What `runBulk` reports when it finishes. Never rejects; see below. */
export interface BulkOutcome {
  results: BulkResult[];
  succeeded: number;
  failed: number;
}

/**
 * Default concurrency.
 *
 * Bounded rather than firing every request at once. Selecting 200 rows and
 * issuing 200 simultaneous requests would saturate the browser's per-origin
 * connection pool — starving the polling queries that keep the rest of the
 * console live — and would hit the gateway's per-credential rate limiter, which
 * would then fail rows for a reason that has nothing to do with the rows. Six is
 * at the browser's own per-host limit for HTTP/1.1, so it keeps the pipe full
 * without queueing behind itself.
 */
export const BULK_CONCURRENCY = 6;

/**
 * Runs `op` over every id with bounded concurrency, collecting a result per row.
 *
 * NEVER REJECTS. Every failure — a thrown error, a rejected promise, even a
 * non-Error thrown value — becomes a `{ ok: false, error }` entry. That is the
 * contract that makes decision 2 real: a caller cannot accidentally write
 * `await runBulk(...)` without a catch and lose the record of which seven rows
 * succeeded, because there is no rejection path to lose it down.
 *
 * Workers pull from a shared cursor rather than the ids being pre-partitioned
 * into chunks. Chunking would make the whole batch run at the speed of its
 * slowest chunk: one slow request would leave its chunk-mate idle while other
 * workers had finished entirely. A shared cursor keeps every worker busy until
 * the work is gone.
 *
 * Results come back in the INPUT order, not completion order, so the report
 * lines up with the table the operator is looking at.
 */
export async function runBulk(
  ids: string[],
  op: (id: string) => Promise<unknown>,
  concurrency: number = BULK_CONCURRENCY,
): Promise<BulkOutcome> {
  const results: BulkResult[] = new Array(ids.length);
  let cursor = 0;

  async function worker(): Promise<void> {
    for (;;) {
      const i = cursor++;
      const id = ids[i];
      // Checked on the VALUE rather than on `i >= ids.length`, because under
      // noUncheckedIndexedAccess that is the check the compiler can see; the two
      // are equivalent here since `ids` is never sparse.
      if (id === undefined) return;
      try {
        await op(id);
        results[i] = { id, ok: true };
      } catch (e) {
        results[i] = { id, ok: false, error: describeError(e) };
      }
    }
  }

  // Math.max(1, ...) so a caller passing 0 or a negative gets a sequential run
  // rather than zero workers and a promise that never settles.
  const lanes = Math.min(Math.max(1, concurrency), ids.length || 1);
  await Promise.all(Array.from({ length: lanes }, () => worker()));

  const succeeded = results.filter((r) => r.ok).length;
  return { results, succeeded, failed: results.length - succeeded };
}

/**
 * Turns anything throwable into a sentence.
 *
 * A non-Error CAN reach here: a rejected fetch promise, a thrown string, or a
 * library rejecting with a plain object. Falling through to String(e) rather
 * than assuming `.message` means the worst case is an ugly reason rather than
 * "undefined" next to a failed row — which would read as "it failed for no
 * reason" and is exactly the report that gets ignored.
 */
function describeError(e: unknown): string {
  if (e instanceof Error && e.message) return e.message;
  if (typeof e === 'string' && e) return e;
  return String(e);
}

/**
 * How the bar describes and performs its action.
 */
export interface BulkAction<Row> {
  permission?: string;
  /**
   * The verb, imperative and exact: "Delete", "Revoke", "Cancel", "Drain". It is
   * rendered as `${verb} ${n} ${noun}` on the button, so it must be the word for
   * what actually happens — never a euphemism and never a generic "delete".
   */
  verb: string;
  /**
   * True when the action destroys something or cannot be undone. Gates the
   * confirmation step and the danger tone. Cancelling a run and revoking a token
   * are both destructive in the sense that matters: they cannot be reversed.
   * Draining a worker is not — it is resumed by the worker's own next heartbeat
   * cycle — so it takes no confirmation.
   */
  destructive?: boolean;
  /** Performs the action for one row. Rejects to report that row as failed. */
  run: (row: Row) => Promise<unknown>;
  /**
   * Extra sentence shown in the confirmation, naming what is lost. Optional but
   * strongly encouraged for anything irreversible: "Delete 3 schedules" does not
   * tell an operator that the tick history goes with them.
   */
  consequence?: string;
}

export interface BulkActionBarProps<Row> {
  /** The selected rows, in the order the table shows them. */
  selected: Row[];
  /** Stable id per row — the key the results are reported against. */
  rowId: (row: Row) => string;
  /**
   * Human label per row, for the results list. Falls back to the id, but an id
   * is a UUID on most of these tables and "3f2a…-failed" is not a report anyone
   * can act on.
   */
  rowLabel?: (row: Row) => string;
  /** What the rows ARE, singular and plural: `['schedule', 'schedules']`. */
  noun: [string, string];
  /** The actions offered. Usually one; runs could offer cancel and retry. */
  actions: BulkAction<Row>[];
  /** Clears the table's selection. Called on dismiss and after a clean run. */
  onClear: () => void;
  /**
   * Called after every completed batch so the caller can refetch. Given the
   * outcome, because a caller may want to keep the failed rows selected.
   */
  onDone?: (outcome: BulkOutcome) => void;
}

type Phase =
  | { kind: 'idle' }
  | { kind: 'confirming'; action: BulkAction<unknown> }
  | { kind: 'running'; action: BulkAction<unknown>; done: number; total: number }
  | { kind: 'done'; action: BulkAction<unknown>; outcome: BulkOutcome };

/**
 * The selection bar.
 *
 * Renders nothing when nothing is selected AND no report is outstanding. The
 * second half matters: the report must survive the selection being cleared, or
 * the operator would lose the list of which rows failed at the moment the action
 * finished — which is precisely when they need it.
 */
export function BulkActionBar<Row>({
  selected,
  rowId,
  rowLabel,
  noun,
  actions,
  onClear,
  onDone,
}: BulkActionBarProps<Row>) {
  const can=usePermission();
  actions = actions.filter(a => !a.permission || can(a.permission));
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });
  const confirmRef = useRef<HTMLButtonElement>(null);

  /**
   * Focus moves to the confirm button when the confirmation appears.
   *
   * Without this a keyboard operator who pressed "Delete 3 schedules" is left
   * with focus on a button that has just been replaced, and has to hunt for the
   * dialog they summoned. Focus lands on CONFIRM rather than cancel because the
   * operator asked for this action; Escape (wired below) is the one-key way out,
   * so the destructive default is still one deliberate keystroke from reversed.
   */
  useEffect(() => {
    if (phase.kind === 'confirming') confirmRef.current?.focus();
  }, [phase.kind]);

  const count = selected.length;
  const nounFor = (n: number) => (n === 1 ? noun[0] : noun[1]);

  // The bar disappears only when there is nothing selected AND nothing to report.
  if (count === 0 && phase.kind !== 'done') return null;

  function labelOf(id: string): string {
    const row = selected.find((r) => rowId(r) === id);
    return row && rowLabel ? rowLabel(row) : id;
  }

  async function perform(action: BulkAction<Row>) {
    const rows = selected.slice();
    const total = rows.length;
    setPhase({ kind: 'running', action: action as BulkAction<unknown>, done: 0, total });

    const byId = new Map(rows.map((r) => [rowId(r), r]));
    let done = 0;
    const outcome = await runBulk(
      rows.map(rowId),
      async (id) => {
        const row = byId.get(id);
        try {
          // The row is always present; the non-null path is the only real one.
          // Guarded anyway so a caller whose rowId is not injective produces a
          // reported failure rather than an unhandled TypeError that would take
          // the whole batch's report with it.
          if (!row) throw new Error('row disappeared from the selection');
          return await action.run(row);
        } finally {
          done += 1;
          setPhase((p) =>
            p.kind === 'running' ? { ...p, done } : p,
          );
        }
      },
    );

    setPhase({ kind: 'done', action: action as BulkAction<unknown>, outcome });
    onDone?.(outcome);
    // Selection is cleared ONLY when everything worked. On a partial failure the
    // rows stay selected so the operator can retry exactly the ones that failed
    // without re-finding them in the table.
    if (outcome.failed === 0) onClear();
  }

  function dismiss() {
    setPhase({ kind: 'idle' });
    onClear();
  }

  /**
   * Escape backs out of a confirmation and dismisses a finished report, from
   * anywhere inside the bar. It deliberately does NOT interrupt a running batch:
   * the requests are already in flight and there is nothing to cancel, so an
   * Escape that appeared to stop it would be claiming an abort it cannot perform.
   */
  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key !== 'Escape') return;
    if (phase.kind === 'confirming') {
      e.stopPropagation();
      setPhase({ kind: 'idle' });
    } else if (phase.kind === 'done') {
      e.stopPropagation();
      dismiss();
    }
  }

  return (
    <div
      className="panel bulk-bar"
      onKeyDown={onKeyDown}
      /* A region rather than a dialog: it does not trap focus, and the table
         behind it stays usable while it is open. aria-label names it so a
         screen-reader user landing here knows what it belongs to. */
      role="region"
      aria-label={`Actions for selected ${noun[1]}`}
    >
      {phase.kind === 'idle' && (
        <div className="bulk-bar-row">
          {/* aria-live so the count is announced as rows are ticked, rather than
              a screen-reader user having to go looking for it. polite, not
              assertive: it must not interrupt them mid-row. */}
          <span className="bulk-count" aria-live="polite">
            <strong>{count}</strong> {nounFor(count)} selected
          </span>
          <div className="cell-actions">
            {actions.map((a) => (
              <button
                key={a.verb}
                type="button"
                className={a.destructive ? 'danger' : undefined}
                onClick={() => {
                  if (a.destructive) {
                    setPhase({ kind: 'confirming', action: a as BulkAction<unknown> });
                  } else {
                    void perform(a);
                  }
                }}
              >
                {a.verb} {count} {nounFor(count)}
              </button>
            ))}
            <button type="button" onClick={onClear}>
              Clear selection
            </button>
          </div>
        </div>
      )}

      {phase.kind === 'confirming' && (
        <div className="bulk-bar-confirm">
          {/* alertdialog, so the confirmation is announced when it appears
              rather than waiting to be discovered. The destructive tone comes
              from --tone-danger-*, never from --accent: --accent is brand, and
              using it here would make "delete" look like the primary happy path. */}
          <div className="notice error" role="alertdialog" aria-label="Confirm this action">
            <span className="notice-glyph" aria-hidden="true">
              !
            </span>
            <div>
              <div className="notice-title">
                {phase.action.verb} {count} {nounFor(count)}?
              </div>
              <div className="notice-detail">
                {phase.action.consequence ??
                  'This cannot be undone.'}{' '}
                {count > 1 && (
                  <>
                    Each {noun[0]} is handled separately: if some fail, the rest still
                    complete and you will be told which failed.
                  </>
                )}
              </div>
            </div>
          </div>
          <div className="cell-actions">
            <button
              ref={confirmRef}
              type="button"
              className="danger"
              onClick={() => void perform(phase.action as BulkAction<Row>)}
            >
              Yes, {phase.action.verb.toLowerCase()} {count} {nounFor(count)}
            </button>
            <button type="button" onClick={() => setPhase({ kind: 'idle' })}>
              Cancel
            </button>
          </div>
        </div>
      )}

      {phase.kind === 'running' && (
        <div className="bulk-bar-row">
          {/* The running count is the honest progress indicator: N of M, not a
              spinner. An operator who walked away needs to know whether it got
              stuck at 3 or finished. */}
          <span aria-live="polite">
            {phase.action.verb}… {phase.done} of {phase.total} {nounFor(phase.total)} done
          </span>
        </div>
      )}

      {phase.kind === 'done' && (
        <div className="bulk-bar-results">
          <div className="bulk-bar-row">
            {/* role=status so the summary is announced on completion. The counts
                are stated separately rather than as "7/10", because "3 failed"
                is the half that needs acting on and it must not be arithmetic
                the reader performs. */}
            <span role="status">
              <span className="badge tone-success">{phase.outcome.succeeded} succeeded</span>{' '}
              {phase.outcome.failed > 0 && (
                <span className="badge tone-danger">{phase.outcome.failed} failed</span>
              )}
            </span>
            <div className="cell-actions">
              <button type="button" onClick={dismiss}>
                Dismiss
              </button>
            </div>
          </div>
          {phase.outcome.failed > 0 && (
            <ul className="bulk-bar-failures">
              {phase.outcome.results
                .filter((r) => !r.ok)
                .map((r) => (
                  /* Each failure names the ROW and the REASON. A report that
                     said only "3 failed" would send the operator back to the
                     table to work out which three, which is the report that
                     gets ignored. */
                  <li key={r.id}>
                    <code className="mono">{labelOf(r.id)}</code>{' '}
                    <span className="tone-text-danger">{r.error ?? 'failed'}</span>
                  </li>
                ))}
            </ul>
          )}
        </div>
      )}
    </div>
  );
}
