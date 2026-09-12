// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Run list: filter by state and spec, newest first, live, and — finally —
 * PAGED.
 *
 * Filters live in the URL query string so an operator can paste "the failed runs
 * of daily-etl" into a chat window and have the recipient see the same view.
 *
 * The filters sit in a bar of their own, ABOVE the table rather than inside
 * DataTable's `toolbar` slot, and the reason is a hard one: the table renders
 * only on the branch where rows arrived. AsyncPanel owns loading, error and
 * empty, and a filter that matched nothing lands on the empty branch — which is
 * the one moment an operator most needs the control that clears it. A clear
 * button that vanishes precisely when it is needed leaves them hand-editing a
 * URL. So the bar is the panel's. .panel-head was never a candidate either: it
 * is uppercase and letter-spaced, the wrong container for controls people read
 * and click, and fighting it needed an inline override on every label.
 *
 * ── WHY THIS PAGE IS THE ONLY ONE IN SERVER PAGING MODE ───────────────────
 *
 * /runs is the one endpoint in this gateway with real pagination: it answers a
 * `before` timestamp and returns `next_before` for the following page. Every
 * other list here answers "everything, up to a cap", which is what client-mode
 * paging is for. So this is the only screen that can page honestly, and until
 * now it did not page at all — it fetched a capped 100 and stopped, with the
 * gateway's cursor being discarded inside the API client.
 *
 * ── THE CURSOR STACK, WHICH IS THE NON-OBVIOUS PART ───────────────────────
 *
 * Keyset pagination is not symmetric. Going FORWARD is free: the server hands
 * back `next_before` and the next request carries it. Going BACKWARD is not
 * addressable at all — there is no "after" cursor, and a keyset cursor cannot
 * be decremented, because the rows between two timestamps are whatever exists
 * at read time and not a fixed count.
 *
 * So "previous" can only mean "a cursor we have ALREADY SEEN", and the
 * structure that holds those is a STACK, not a single value:
 *
 *   page 1  stack []                     request has no `before`
 *   next →  stack ['t1']                 request has before=t1
 *   next →  stack ['t1','t2']            request has before=t2
 *   prev →  stack ['t1']                 request has before=t1 again
 *   prev →  stack []                     back to the unparameterised first page
 *
 * The top of the stack IS the current request's cursor, and its depth IS the
 * 1-based page number minus one. A single "previous cursor" variable cannot
 * express this: it would get an operator back one page and then have nothing
 * left to pop, so page 3 → 2 → 1 would be unreachable past the first step.
 *
 * The stack is state, not URL: unlike the filters, a cursor is not worth
 * pasting. It is a timestamp whose meaning depends on rows that may have moved
 * by the time a colleague opens the link, so "page 3 of the failed runs" would
 * reproduce a different page 3 for them. The FILTERS reproduce exactly; the
 * position within them deliberately does not.
 *
 * ── AND WHY THE COUNT CLAIMS NOTHING ──────────────────────────────────────
 *
 * `count: {kind:'unknown'}`. The runs endpoint pages but never totals — there
 * is no count in any response — so there is no denominator and the UI must not
 * imply one. No "page 3 of 10", no "of 240 runs": what is stated is the page
 * number and the rows on it. See lib/table.ts, which holds that rule.
 */

import { useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { RUN_STATES, type NodeState, type RunSummary } from '../api/types';
import { useRuns } from '../api/hooks';
import { api } from '../api/client';
import { PageHeader } from '../components/PageHeader';
import { StateBadge } from '../components/StateBadge';
import { AsyncPanel, StaleWarning } from '../components/Status';
import { BulkActionBar } from '../components/BulkActionBar';
import { DataTable } from '../components/DataTable';
import type { Column, RowCountKind } from '../components/DataTable';
import { EMPTY, durationBetween, fmtAge, fmtDurationMs, fmtTs, shortId } from '../lib/format';
import { isTerminal, stateVisual } from '../lib/states';

/**
 * The runs endpoint pages and never totals, so no count drawn from it may claim
 * to describe more than the page it came from. This is the one list in the
 * console that gets `unknown`, and it is the honest answer rather than a
 * cautious default.
 */
const RUNS_COUNT: RowCountKind = { kind: 'unknown' };

/**
 * Rows per request.
 *
 * MUST MATCH the gateway's `intParam(r, "limit", 100)` default only in the
 * sense that it must be a value the gateway accepts; it is sent explicitly so
 * the page size the UI pages at and the page size the server cuts at are the
 * same number. They have to agree, because `next_before` is emitted only when a
 * FULL page comes back: if the UI asked for 50 while the server cut at 100, a
 * 50-row page would look like the end of the list.
 *
 * DataTable's own page-size selector is not wired to this. Client-mode paging
 * slices rows already in hand and can resize for free; a keyset page size is a
 * property of the REQUEST, and changing it mid-listing invalidates every cursor
 * already on the stack — page 2 at size 25 does not begin where page 2 at size
 * 50 does. Rather than silently reset the stack under the operator, the size is
 * fixed and the selector's absence is the honest form of that.
 */
const RUNS_PAGE_SIZE = 50;

export function RunListPage({ namespace }: { namespace: string }) {
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const state = params.get('state') ?? '';
  const spec = params.get('spec') ?? '';

  /**
   * The cursors already visited, oldest first. See the header: the top is the
   * current page's `before` and the depth is the page index.
   *
   * Empty means page 1, which is the request with no `before` at all — not a
   * cursor of "now". Sending a synthesised "now" would exclude any run created
   * in the same instant and is a boundary nobody needs to defend.
   *
   * ── WHY THE FILTERS ARE STORED ALONGSIDE THE STACK ────────────────────────
   *
   * A cursor addresses a position within ONE result set, so changing the
   * filters must reset it. The obvious way to do that is an effect keyed on the
   * filters — and it is wrong, in a way that only shows up on the wire: an
   * effect runs AFTER render, so the render that first sees `state=FAILED`
   * still holds the old stack, and React Query fires a request for
   * `state=FAILED&before=<a cursor from the unfiltered list>` before the reset
   * lands. That request is not merely wasted; it is a real query against a
   * meaningless boundary, and if it resolves last the operator reads its empty
   * page as "there are no failed runs".
   *
   * So the reset is DERIVED DURING RENDER instead, by storing the filters the
   * stack belongs to next to the stack itself. When they disagree, the stack
   * is empty as far as this render is concerned — no request with a stale
   * cursor is ever constructed, because the state it would come from never
   * exists. This is React's documented "adjusting state when props change"
   * pattern, and the setState during render it uses is the cheap kind: it
   * re-renders this component before anything is committed.
   */
  const [cursors, setCursors] = useState<{ state: string; spec: string; stack: string[] }>({
    state,
    spec,
    stack: [],
  });
  if (cursors.state !== state || cursors.spec !== spec) {
    setCursors({ state, spec, stack: [] });
  }
  const stack = cursors.state === state && cursors.spec === spec ? cursors.stack : [];
  const before = stack[stack.length - 1];

  /** Pushes the server's next cursor; the stack's depth is the page index. */
  const pushCursor = (cursor: string) =>
    setCursors((c) => ({ ...c, stack: [...c.stack, cursor] }));
  /** Previous is a POP — the only positions reachable are those already seen. */
  const popCursor = () => setCursors((c) => ({ ...c, stack: c.stack.slice(0, -1) }));

  const query = useRuns(namespace, {
    state: state || undefined,
    spec: spec || undefined,
    limit: RUNS_PAGE_SIZE,
    before,
  });
  const now = Date.now();
  const filtered = Boolean(state || spec);

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };

  const nextBefore = query.data?.nextBefore;
  const rows = query.data?.runs;

  /**
   * The selected run IDS.
   *
   * This list polls every 4s and its rows transition constantly, so ids are the
   * only identity that survives — and unlike the other tables, a row here can
   * become INELIGIBLE while selected: a RUNNING run the operator ticked may be
   * SUCCEEDED by the time they press the button. `selectedRuns` below re-applies
   * the eligibility test against the latest rows for exactly that reason, so the
   * bar's count is a statement about what is still cancellable NOW rather than
   * about what was cancellable when the box was ticked.
   */
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());

  /**
   * A run can only be cancelled while it is not terminal.
   *
   * The same predicate feeds `isSelectable` on the table and this list, so the
   * checkbox an operator sees and the rows the action runs over can never
   * disagree — which is what keeps "Cancel 8 runs" from including a SUCCEEDED
   * run that would refuse. isTerminal is lib/states.ts's own table, not a
   * hand-written state list here: a console with two opinions about which states
   * are final is a console where one of them is wrong.
   */
  const cancellable = (r: RunSummary) => !isTerminal('run', r.state);
  const selectedRuns = useMemo(
    () => (rows ?? []).filter((r) => selected.has(r.id) && cancellable(r)),
    [rows, selected],
  );

  const columns: Column<RunSummary>[] = [
    {
      key: 'state',
      header: 'state',
      cell: (r) => <StateBadge kind="run" state={r.state} />,
    },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      cell: (r) => (
        <Link to={`/runs/${encodeURIComponent(r.id)}`} title={r.id}>
          {shortId(r.id, 12)}
        </Link>
      ),
    },
    {
      key: 'spec',
      header: 'spec',
      cell: (r) => (
        <div className="cell-stack">
          <span>{r.spec_name ?? <span className="faint">{shortId(r.spec_id, 10)}</span>}</span>
          {r.spec_version !== undefined && <span className="cell-sub">v{r.spec_version}</span>}
        </div>
      ),
    },
    {
      key: 'progress',
      header: 'progress',
      cell: (r) => <NodeCounts counts={r.node_counts} />,
    },
    {
      key: 'trigger',
      header: 'trigger',
      cellClass: 'mono faint',
      cell: (r) => <>{r.trigger_kind ?? EMPTY}</>,
    },
    {
      key: 'created',
      header: 'created',
      cellClass: 'nowrap',
      cell: (r) => <span title={fmtTs(r.created_at)}>{fmtAge(r.created_at, now)}</span>,
    },
    {
      key: 'duration',
      header: 'duration',
      cellClass: 'num',
      cell: (r) => <>{fmtDurationMs(runDuration(r, now))}</>,
    },
  ];

  /**
   * No `sort` on any column, and that is deliberate rather than an omission.
   *
   * A comparator here would sort THIS PAGE, and a page of a keyset listing is a
   * window into a server-ordered sequence. Sorting 50 of an unknown number of
   * runs by duration and presenting the result as "the longest runs" is the
   * exact class of claim lib/table.ts exists to prevent — the honest version of
   * that question is a server-side order the gateway does not offer. Newest
   * first is the server's order and the only order this list asserts.
   */

  return (
    <div className="col">
      <PageHeader
        title="Runs"
        subtitle={
          <>
            namespace <code>{namespace}</code> · newest first · refreshes every 4s
          </>
        }
      />

      <div className="panel">
        {/* THE FILTERS RENDER OUTSIDE AsyncPanel, AND THAT IS THE WHOLE POINT.
            DataTable has a `toolbar` slot and these belong in it conceptually,
            but the table only exists on the branch where rows arrived — and the
            single most important moment for a filter control is the branch
            where they did NOT. An operator who filters to state=FAILED and gets
            an empty list must be able to clear it; a clear button that
            disappears exactly when it is needed strands them on a URL they have
            to hand-edit. Loading and error have the same problem. So the bar is
            the panel's, not the table's, and DataTable's toolbar slot carries
            only the count beside it. */}
        <div className="filterbar">
          <RunFilters
            state={state}
            spec={spec}
            filtered={filtered}
            onChange={setParam}
            onClear={() => setParams(new URLSearchParams(), { replace: true })}
          />
        </div>

        {/* Stale data plus a warning beats an error box that hides the data. */}
        {query.error != null && query.data !== undefined ? (
          <div className="panel-notice">
            <StaleWarning error={query.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={{ ...query, data: rows }}
          what="runs"
          isEmpty={(runs) => runs.length === 0}
          emptyTitle={filtered ? 'No runs match these filters' : 'No runs yet'}
          emptyDetail={
            filtered ? (
              'Loosen the filters, or check that the run you expect is in this namespace.'
            ) : (
              <>
                Submit a spec and create a run: <code>nexusctl run create --spec example-etl</code>
              </>
            )
          }
          loadingRows={6}
        >
          {(runs) => (
            <>
              {/*
                CANCEL, and never delete. A run is immutable execution history
                that the audit trail references — there is no DELETE for one and
                there must not be. Cancel is the lifecycle action that actually
                exists, and it is what an operator selecting a page of stuck runs
                wants: stop them, keep the record of what happened.

                Destructive, because it cannot be undone: a cancelled run does
                not resume. The confirmation says so rather than letting "cancel"
                read as the reversible word it is in most other interfaces.
              */}
              <BulkActionBar<RunSummary>
                selected={selectedRuns}
                rowId={(r) => r.id}
                rowLabel={(r) => shortId(r.id, 12)}
                noun={['run', 'runs']}
                actions={[
                  {
                    verb: 'Cancel',
                    destructive: true,
                    consequence:
                      'In-flight attempts are stopped and the runs end as CANCELLED. This cannot be undone — a cancelled run does not resume, and the work already done is not repeated on a new one. The runs themselves stay as history.',
                    run: (r) => api.cancelRun(namespace, r.id),
                  },
                ]}
                onClear={() => setSelected(new Set())}
                onDone={() => {
                  /* Refetch rather than mutate the rows in place: a run that
                     reached a terminal state between the tick and the request
                     refuses the cancel, and it must still be on screen showing
                     what it actually is. Prefix-invalidated so every cursor and
                     filter combination in the cache is refreshed — the operator
                     may page back to one of them. */
                  void qc.invalidateQueries({ queryKey: ['runs', namespace] });
                  void qc.invalidateQueries({ queryKey: ['attention', namespace] });
                }}
              />
              <DataTable
              rows={runs}
              columns={columns}
              rowKey={(r) => r.id}
              rowClass={rowTone}
              noun="runs"
              tableClass="table-runs"
              count={RUNS_COUNT}
              /* Selection, with the eligibility rule applied at the CHECKBOX
                 rather than at the action. A SUCCEEDED or FAILED run gets no
                 checkbox at all, so it cannot be selected for a cancel that
                 would certainly be refused — reporting that refusal afterwards
                 would read as the platform breaking rather than as a row that
                 was never a candidate.

                 The count kind is `unknown` — /runs pages and never totals — so
                 select-all says "loaded runs", which here means the rows of this
                 page. There is deliberately no control that reaches the pages
                 the client has not fetched. */
              selection={{
                selected,
                onChange: setSelected,
                rowId: (r) => r.id,
                isSelectable: cancellable,
                label: (r) => `run ${shortId(r.id, 12)}`,
              }}
              /* No `search`. The gateway matches runs by SPEC NAME, which the
                 spec filter in the toolbar already is; a second box beside it
                 that filtered only the 50 rows on this page would answer a
                 narrower question while looking like the broader one. */
              paging={{
                mode: 'server',
                pageNumber: stack.length + 1,
                /* Forward only while the server says there is more. Absent
                   `next_before` means the page was not full, which is the
                   gateway's own statement that the list is exhausted — so the
                   button is not merely disabled, the capability is absent. */
                onNext: nextBefore ? () => pushCursor(nextBefore) : undefined,
                /* Backward is a POP, never a computed cursor: the only
                   positions reachable are the ones already visited. On page one
                   the stack is empty and there is nothing to pop, so no handler
                   is passed and DataTable disables the control. */
                onPrev: stack.length > 0 ? popCursor : undefined,
              }}
              />
            </>
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}

/**
 * The page's own filters.
 *
 * They stay here rather than becoming table props for the reason DataTable's
 * own comment gives: a generic filter prop would need a filter-definition
 * mini-language, and these two are specific to runs — one is the run state
 * enum, the other a spec name the gateway matches server-side. Extracted into a
 * component of their own only so the bar above AsyncPanel reads as one thing.
 */
function RunFilters({
  state,
  spec,
  filtered,
  onChange,
  onClear,
}: {
  state: string;
  spec: string;
  filtered: boolean;
  onChange: (key: string, value: string) => void;
  onClear: () => void;
}) {
  return (
    <>
      {/* Both controls carry an explicit aria-label rather than relying on the
          wrapping <label>'s "state"/"spec" text. The table below contributes a
          rows-per-page <select> and a search-shaped field of its own, so on a
          page with rows there are two comboboxes and two searchboxes — and a
          screen reader user hearing "combobox, state" against "combobox, runs
          per page" is the difference between a filter and a page size. The
          visible text stays as it was; this only names it. */}
      <label className="filter-field">
        <span className="filter-label">state</span>
        <select
          value={state}
          onChange={(e) => onChange('state', e.target.value)}
          aria-label="filter runs by state"
        >
          <option value="">any</option>
          {RUN_STATES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
      </label>
      <label className="filter-field">
        <span className="filter-label">spec</span>
        <input
          type="search"
          placeholder="workflow name"
          value={spec}
          onChange={(e) => onChange('spec', e.target.value)}
          aria-label="filter runs by workflow name"
        />
      </label>
      {filtered && (
        <button type="button" onClick={onClear}>
          clear filters
        </button>
      )}
    </>
  );
}

/**
 * How long a run has been going, or how long it took.
 *
 * A running run is measured against `now`, so the column ticks. Extracted from
 * the row component it used to live in because the cell renderer above is a
 * plain function now and this is the one piece of it worth naming.
 */
function runDuration(run: RunSummary, now: number): number | null {
  if (isTerminal('run', run.state)) {
    return durationBetween(run.started_at ?? run.created_at, run.ended_at);
  }
  return run.started_at ? now - Date.parse(run.started_at) : null;
}

/**
 * Which rows carry a tone-coloured left rule.
 *
 * The rule is redundant with the badge in the same row — it exists so a
 * screenful can be triaged by edge colour without reading a label. It is derived
 * from the run's own tone rather than from a second list of state names here,
 * which is what stops the edge and the badge from ever disagreeing: adding a run
 * state to lib/states.ts gives it the right edge for free.
 *
 * Only the two tones that mean "something is wrong" get a rule. An in-flight or
 * succeeded run gets none, because a rule on every row is a rule on no row. And
 * neither tone is the accent: the accent is operator-switchable, so spending it
 * here would let a theme change make a FAILED run read as a finished one.
 */
function rowTone(run: RunSummary): string {
  const tone = stateVisual('run', run.state).tone;
  if (tone === 'danger') return 'row-failed';
  // A run that succeeded overall while a node inside it failed: the row's own
  // badge says SUCCEEDED, so this edge is the only thing that flags it.
  if ((run.node_counts?.FAILED ?? 0) > 0) return 'row-failed';
  // Stopped and ambiguous outcomes did not do what was asked, but they are not
  // the failure to look at first, so they take the blocked tone rather than
  // competing with it.
  if (tone === 'stopped' || tone === 'warning') return 'row-blocked';
  return '';
}

/**
 * Compact per-state node tally. Only non-zero states are shown, so a healthy
 * run reads as "8 SUCCEEDED" rather than a row of zeroes to scan past.
 */
function NodeCounts({ counts }: { counts: Partial<Record<NodeState, number>> | undefined }) {
  if (!counts) return <span className="faint">{EMPTY}</span>;
  const entries = (Object.entries(counts) as [NodeState, number][]).filter(([, n]) => n > 0);
  if (entries.length === 0) return <span className="faint">{EMPTY}</span>;
  // Failure-bearing states first: they are why anyone is looking at this row.
  const rank: Record<string, number> = {
    FAILED: 0,
    UPSTREAM_FAILED: 1,
    RUNNING: 2,
    READY: 3,
    PENDING: 4,
    CANCELLED: 5,
    SKIPPED: 6,
    SUCCEEDED: 7,
  };
  entries.sort((a, b) => (rank[a[0]] ?? 9) - (rank[b[0]] ?? 9));
  return (
    <span className="node-tally">
      {entries.map(([s, n]) => (
        <span key={s} className="node-tally-item">
          <StateBadge kind="node" state={s} small showGlyph={false} />
          <span className="node-tally-n">{n}</span>
        </span>
      ))}
    </span>
  );
}
