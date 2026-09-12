// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One table, for every list in the console.
 *
 * Seven screens hand-rolled a `<table>` before this existed — workflows, runs,
 * schedules, workers, users, tokens, audit — and the duplication was not the
 * expensive part. The expensive part was that each one had reinvented the
 * DECISIONS: which headers are sortable, whether the count beside the title
 * describes the namespace or the response, whether a `<th>` carries a scope.
 * Seven copies of a decision is seven chances for one of them to be wrong, and
 * the one that WAS wrong was the honesty of the count — see lib/table.ts, which
 * holds the rules this component renders.
 *
 * ── WHAT THIS DOES NOT DO, AND WHY ────────────────────────────────────────
 *
 * IT DOES NOT OWN LOADING, ERROR OR EMPTY. Status.tsx already does, every page
 * already routes through <AsyncPanel>, and each page's empty copy is written for
 * that page — "Nothing has been authored here yet" is not interchangeable with
 * "Integrations here still use static API keys". Swallowing those into a generic
 * `emptyMessage` prop would have replaced eight carefully-worded absences with
 * one bland one. So the division WorkersPage already uses is the division kept:
 * AsyncPanel decides WHETHER there are rows to draw, DataTable draws them. The
 * one empty state this component does render is the one AsyncPanel cannot see —
 * rows arrived, and the table's own search matched none of them. That is a
 * different sentence from "this namespace is empty" and it belongs here, because
 * only this component knows the term.
 *
 * IT DOES NOT FETCH. Paging and searching are either the caller's query's job
 * (server mode) or pure arithmetic over the rows it was handed (client mode).
 * A component that fetched would need to know about namespaces, poll intervals
 * and retry policy, all of which api/hooks.ts already owns.
 *
 * IT DOES NOT INVENT A TOTAL. There is no "page 3 of 10" anywhere below unless
 * the caller's `count` says a total is knowable, because for five of the six
 * endpoints behind these pages it is not. What replaces it is a range and a
 * clearly-marked cap — the same honesty the attention panel applies to
 * `truncated` and CapacityGrid applies to `derived`.
 *
 * ── THE SEARCH CONTRACT IS EXPLICIT, NOT INFERRED ─────────────────────────
 *
 * `search.mode` is a required discriminant. It would have been possible to
 * infer: "an onSearchChange was passed, so the server must be doing it".
 * Inference was rejected because the two modes make DIFFERENT CLAIMS and the
 * failure is silent. A client-side filter searches only the rows it was handed;
 * if those rows are a capped 100 of 400, then "3 results" is a statement about
 * the 100 dressed up as a statement about the namespace. A server-side search
 * has no such problem — the gateway matched the whole table — but it has the
 * opposite one: the rows on screen may answer a term the operator has since
 * edited. Each mode needs a different sentence, so each mode is named.
 */

import { useId, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import { Empty } from './Status';
import {
  clampPage,
  countClaim,
  filterRows,
  pageCount,
  pageSlice,
  rangeLabel,
  sortRows,
} from '../lib/table';
import type { Column, RowCountKind, SearchMode, SortState } from '../lib/table';

export type { Column, RowCountKind, SearchMode, SortState } from '../lib/table';

/**
 * The page-size choices.
 *
 * 25 / 50 / 100, and 100 is the ceiling on purpose: it is at or above the
 * default `limit` of every endpoint behind these tables except the catalog's
 * 200, so offering 250 would produce a selector whose top option could never
 * fill. A selector with an unreachable option teaches an operator that the
 * control is decorative.
 */
export const PAGE_SIZES = [25, 50, 100] as const;

/**
 * Paging arrangement.
 *
 * `client` — the component holds the page index and slices the rows it was
 * given. Correct for every list whose endpoint answers "everything, up to a
 * cap": catalog, schedules, users, tokens, audit. The rows are all in hand, so
 * moving between pages costs nothing and cannot fail.
 *
 * `server` — the caller owns the cursor. Only /runs can support this (keyset,
 * `next_before`), and the shape reflects what keyset paging can actually
 * promise: you may go forward while there IS a next cursor, and back through
 * cursors you have already seen. It emphatically does not offer "jump to page
 * 7", because a keyset cursor cannot address an arbitrary page — offering it
 * would mean fetching six pages to throw five away.
 *
 * `none` — no paging. The fleet view, where every worker must be on screen at
 * once because the question being asked is about the fleet as a whole.
 */
export type Paging =
  | { mode: 'client' }
  | {
      mode: 'server';
      /** Fetches the next page. Absent/undefined means there is no next. */
      onNext?: () => void;
      /** Returns to the previous page. Absent means this is the first. */
      onPrev?: () => void;
      /** 1-based, for display only. No total accompanies it; see rangeLabel. */
      pageNumber: number;
    }
  | { mode: 'none' };

export interface DataTableProps<Row> {
  /**
   * The rows to render. Already loaded — this component is mounted inside
   * AsyncPanel's children, so `undefined` and "failed" are not states it can
   * be in.
   */
  rows: Row[];
  columns: Column<Row>[];
  /**
   * Stable identity per row, for React's key. Required rather than defaulting
   * to the array index: these lists poll, and an index key makes React reuse
   * the DOM of row 3 for a different row 3 after a refetch — which in a table
   * with per-row buttons means a click can land on the wrong entity.
   */
  rowKey: (row: Row) => string;
  /** Per-row class, for the tone-coloured left rules the run/fleet tables use. */
  rowClass?: (row: Row) => string | undefined;
  /**
   * What the rows ARE, lower case and plural: "workflows", "accounts". Used in
   * the count chip, the range label and the search field's accessible name, so
   * it reads as a noun in a sentence rather than a heading.
   */
  noun: string;
  /**
   * What the row count is entitled to claim. Defaults to `unknown`, which is
   * the only safe default: it is the one value that never over-claims, so a
   * caller who has not thought about it yet gets a cautious table rather than a
   * confident falsehood.
   */
  count?: RowCountKind;
  /** Search wiring, or absent for a table with no search box. */
  search?: SearchMode<Row>;
  /** Defaults to client-side paging, the mode five of these six lists want. */
  paging?: Paging;
  /** Initial page size. Must be one of PAGE_SIZES to be selectable. */
  defaultPageSize?: (typeof PAGE_SIZES)[number];
  /** Initial sort. Absent leaves the rows in the order the server sent them. */
  defaultSort?: SortState;
  /**
   * Extra class on the `<table>`, for the per-table density classes that
   * already exist (`table-runs`, `table-fleet`).
   */
  tableClass?: string;
  /**
   * Rendered in the toolbar before the search box: the page's OWN filters,
   * which stay with the page. AuditPage's outcome/action selects are not
   * generic and pushing them in here would have meant a filter-definition
   * mini-language — the thing this component exists to avoid.
   */
  toolbar?: ReactNode;
  /**
   * Title for the "search matched nothing" state, so a page can phrase its own.
   * Defaults to a sentence naming the term, which is already better than the
   * generic "No results" because it shows what was actually searched for.
   */
  noMatchTitle?: string;
  noMatchDetail?: ReactNode;
}

export function DataTable<Row>({
  rows,
  columns,
  rowKey,
  rowClass,
  noun,
  count = { kind: 'unknown' },
  search,
  paging = { mode: 'client' },
  defaultPageSize = 25,
  defaultSort,
  tableClass,
  toolbar,
  noMatchTitle,
  noMatchDetail,
}: DataTableProps<Row>) {
  const [sort, setSort] = useState<SortState | undefined>(defaultSort);
  const [pageSize, setPageSize] = useState<number>(defaultPageSize);
  const [page, setPage] = useState(0);
  /**
   * The client-mode term. Held here rather than lifted, because in client mode
   * nobody outside needs it: there is no request to key, no URL to write. In
   * server mode the caller owns the value and this state is unused — the field
   * reads `search.value` instead, so the two can never disagree.
   */
  const [localTerm, setLocalTerm] = useState('');

  const searchId = useId();

  const term = search?.mode === 'client' ? localTerm : (search?.value ?? '');

  // Filter, then sort, then slice — in that order, because sorting before
  // filtering would sort rows that are about to be discarded, and slicing before
  // either would page through the wrong set.
  const filtered = useMemo(() => {
    if (search?.mode !== 'client') return rows;
    return filterRows(rows, localTerm, search.fields);
  }, [rows, search, localTerm]);

  const sorted = useMemo(() => {
    if (!sort) return filtered;
    const column = columns.find((c) => c.key === sort.key);
    return sortRows(filtered, column?.sort, sort.dir);
  }, [filtered, sort, columns]);

  const clientPaged = paging.mode === 'client';
  // Clamped at the point of USE, not only in the handlers: a poll that shortens
  // the list or a filter that narrows it can put the index out of range without
  // anyone clicking anything, and the operator should land on the nearest real
  // page rather than on a blank table. See clampPage's own note.
  const safePage = clientPaged ? clampPage(page, sorted.length, pageSize) : 0;
  const [from, to] = clientPaged
    ? pageSlice(safePage, sorted.length, pageSize)
    : [0, sorted.length];
  const visible = clientPaged ? sorted.slice(from, to) : sorted;
  const pages = clientPaged ? pageCount(sorted.length, pageSize) : 1;

  /**
   * The count chip's claim.
   *
   * In client mode with an active term the count describes the FILTERED rows,
   * so the claim is downgraded to `unknown` regardless of what the caller
   * passed: filtering a capped fetch produces a number nobody can vouch for.
   * "3 of the 100 rows we happen to hold" is not "3 in this namespace", and the
   * only honest denominator is none. This is the single most important line in
   * the file.
   */
  const claimKind: RowCountKind =
    search?.mode === 'client' && term.trim() !== '' ? { kind: 'unknown' } : count;
  const claim = countClaim(sorted.length, claimKind, noun);

  const searching = search?.mode === 'server' && search.pending === true;

  function toggleSort(key: string) {
    setSort((prev) => {
      if (prev?.key !== key) return { key, dir: 'asc' };
      // Third click clears rather than cycling asc→desc→asc forever: the
      // server's own order (name order for the catalog, newest-first for the
      // audit trail) is a considered default and there has to be a way back to
      // it without a reload.
      if (prev.dir === 'asc') return { key, dir: 'desc' };
      return undefined;
    });
    // A sort reorders the whole list, so the current page index means something
    // different afterwards. Returning to the first page is the only reading of
    // "sort by this" that matches what an operator asked for.
    setPage(0);
  }

  return (
    <>
      {/* The toolbar is UNCONDITIONAL, because the count chip inside it is.
          The count is a claim about the rows, not about the search — a table
          with no search box still owes its operator an honest statement of how
          many rows it is showing and whether that number is a total. Gating the
          whole band on `search` would have silently dropped that disclosure
          from exactly the tables that have no other way to make it. */}
      <div className="dt-toolbar">
        {toolbar}
        {/* The field and the sentence qualifying it, as ONE group.

            They were siblings before, separated by the spacer that pushes the
            count to the far edge — so the note explaining WHERE the search ran
            was laid out as a peer of the search box rather than as part of it,
            and on a band that wraps it could land on a different line from the
            field it describes. aria-describedby already tied the two together
            for a screen reader; this is that same relationship made visible,
            which is why it is a grouping and not a second statement. */}
        {search && (
          <div className="dt-search-group">
            <label className="dt-search filter-field" htmlFor={`${searchId}-q`}>
              <span className="filter-label">search</span>
              <input
                id={`${searchId}-q`}
                type="search"
                value={term}
                onChange={(e) => {
                  if (search.mode === 'client') {
                    setLocalTerm(e.target.value);
                    // A narrower result set makes the current page index stale,
                    // and clamping alone would leave the operator on the last
                    // page of three matches. First page is what they meant.
                    setPage(0);
                  } else {
                    search.onChange(e.target.value);
                  }
                }}
                placeholder={
                  search.mode === 'server' ? 'name contains…' : 'filter these rows…'
                }
                /**
                 * The accessible name says WHERE the search happens, because
                 * that is the difference an operator cannot see and it changes
                 * what the result means. Server: the whole table was matched.
                 * Client: only the rows already fetched were.
                 */
                aria-label={
                  search.mode === 'server'
                    ? `search ${noun} by name`
                    : `filter the loaded ${noun}`
                }
                aria-describedby={`${searchId}-scope`}
              />
            </label>
            {/* Stated rather than implied, and present in both modes so neither
                reads as the unqualified default. Visually quiet — it is a
                qualifier on the field above, not a warning. */}
            <span id={`${searchId}-scope`} className="dt-scope">
              {search.mode === 'server'
                ? 'searched on the server'
                : 'filters the loaded rows only'}
            </span>
          </div>
        )}
        <span className="spacer" />
        {/* The count. It renders what lib/table.ts says it may render and
            nothing more: the digits in hand, a "+" when there may be rows
            nobody fetched, and the full sentence on hover. */}
        <span className="chip dt-count" title={searching ? undefined : claim.title}>
          {searching ? 'searching…' : `${claim.n}${claim.more ? '+' : ''}`}
        </span>
      </div>

      {/* The one empty state that is this component's to own: rows arrived and
          the table's own filter matched none of them. AsyncPanel cannot phrase
          this one because it never sees the term. */}
      {visible.length === 0 && term.trim() !== '' && search?.mode === 'client' ? (
        <Empty
          title={noMatchTitle ?? `No ${noun} on this page match “${term.trim()}”`}
          detail={
            noMatchDetail ?? (
              <>
                This filter searches only the {rows.length} row
                {rows.length === 1 ? '' : 's'} already loaded, not the whole
                namespace.
              </>
            )
          }
        />
      ) : (
        <div className="table-scroll">
          <table className={tableClass}>
            <thead>
              <tr>
                {columns.map((c) => (
                  <HeaderCell
                    key={c.key}
                    column={c}
                    sort={sort}
                    onToggle={() => toggleSort(c.key)}
                  />
                ))}
              </tr>
            </thead>
            <tbody>
              {visible.map((row) => (
                <tr key={rowKey(row)} className={rowClass?.(row)}>
                  {columns.map((c) => (
                    <td
                      key={c.key}
                      className={
                        [c.cellClass, c.align === 'end' ? 'dt-end' : null]
                          .filter(Boolean)
                          .join(' ') || undefined
                      }
                    >
                      {c.cell(row)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {paging.mode !== 'none' && (
        <Pager
          paging={paging}
          noun={noun}
          page={safePage}
          pages={pages}
          from={from}
          to={to}
          total={sorted.length}
          count={claimKind}
          pageSize={pageSize}
          onPage={setPage}
          onPageSize={(n) => {
            setPageSize(n);
            // Row 30 is on page 2 at size 25 and page 1 at size 50, so the
            // index cannot be carried across the change. Page 1 is the only
            // position that is meaningful under both.
            setPage(0);
          }}
        />
      )}
    </>
  );
}

/**
 * One `<th>`, sortable or not.
 *
 * `scope="col"` unconditionally: without it a screen reader in table mode has
 * to guess which header belongs to a cell, and in a nine-column operator table
 * the guess is what makes the table unreadable rather than merely awkward.
 *
 * A sortable header is a real `<button>` INSIDE the `<th>`, not a click handler
 * on the `<th>` itself. A div with onClick is unreachable by keyboard and
 * announces nothing; a button is tab-reachable, activates on Enter and Space
 * for free, and takes the focus ring the rest of the console uses. aria-sort
 * goes on the `<th>` because that is the element the attribute is defined for —
 * on the button it is ignored.
 */
function HeaderCell<Row>({
  column,
  sort,
  onToggle,
}: {
  column: Column<Row>;
  sort: SortState | undefined;
  onToggle: () => void;
}) {
  const active = sort?.key === column.key;
  // "none" rather than omitting the attribute on the other sortable columns:
  // present-but-none is what tells a screen reader the column CAN be sorted,
  // which is the affordance a sighted user gets from the glyph below.
  const ariaSort = !column.sort
    ? undefined
    : active
      ? sort.dir === 'asc'
        ? 'ascending'
        : 'descending'
      : 'none';

  return (
    <th
      scope="col"
      aria-sort={ariaSort}
      className={column.align === 'end' ? 'dt-end' : undefined}
      style={column.width ? { width: column.width } : undefined}
    >
      {column.sort ? (
        <button type="button" className="dt-sort" onClick={onToggle}>
          <span>{column.header}</span>
          {/* Direction is carried by the glyph for a sighted user and by
              aria-sort for everyone else — never by colour alone, and never by
              the accent, which is brand and would make "sorted" look like a
              state. A dimmed double arrow on the unsorted columns says the
              control exists before it is used. */}
          <span className="dt-sort-glyph" aria-hidden="true">
            {active ? (sort.dir === 'asc' ? '↑' : '↓') : '⇅'}
          </span>
        </button>
      ) : (
        column.header
      )}
    </th>
  );
}

/**
 * Page controls.
 *
 * Every button has a real accessible name rather than a bare chevron, and the
 * names say what they move through ("next page of workflows") because in a
 * console with several tables on adjacent screens "Next" alone is not a
 * destination.
 *
 * The range label is the honest substitute for "page N of M": see rangeLabel.
 * In client mode M is genuinely known — we hold every row — so "page 2 of 6" IS
 * sayable there and is said. In server mode it is not, so only the current page
 * number appears, with no denominator invented to sit beside it.
 */
function Pager({
  paging,
  noun,
  page,
  pages,
  from,
  to,
  total,
  count,
  pageSize,
  onPage,
  onPageSize,
}: {
  paging: Paging;
  noun: string;
  page: number;
  pages: number;
  from: number;
  to: number;
  total: number;
  count: RowCountKind;
  pageSize: number;
  onPage: (n: number) => void;
  onPageSize: (n: number) => void;
}) {
  const sizeId = useId();
  const serverMode = paging.mode === 'server';

  // Bounds live here so the disabled attribute and the handler read the SAME
  // condition. Two expressions for one bound is how a button that looks
  // disabled still fires.
  const atFirst = serverMode ? paging.onPrev === undefined : page <= 0;
  const atLast = serverMode ? paging.onNext === undefined : page >= pages - 1;

  return (
    <div className="dt-pager">
      <label className="dt-pagesize" htmlFor={`${sizeId}-size`}>
        <span className="filter-label">rows</span>
        <select
          id={`${sizeId}-size`}
          value={pageSize}
          onChange={(e) => onPageSize(Number(e.target.value))}
          aria-label={`${noun} per page`}
        >
          {PAGE_SIZES.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </label>

      <span className="spacer" />

      {/* What is on screen, phrased so it never implies a total the endpoint
          cannot supply. */}
      <span className="dt-range">{rangeLabel(from, to, total, count)}</span>

      <div className="dt-pager-buttons">
        <button
          type="button"
          disabled={atFirst}
          onClick={() => {
            if (serverMode) paging.onPrev?.();
            else onPage(page - 1);
          }}
          aria-label={`previous page of ${noun}`}
        >
          Previous
        </button>
        {/* The position. In client mode the denominator is a fact about rows we
            hold; in server mode there is no denominator to state, so none is
            printed rather than one being guessed. */}
        <span className="dt-page-of" aria-live="polite">
          {serverMode ? `page ${paging.pageNumber}` : `page ${page + 1} of ${pages}`}
        </span>
        <button
          type="button"
          disabled={atLast}
          onClick={() => {
            if (serverMode) paging.onNext?.();
            else onPage(page + 1);
          }}
          aria-label={`next page of ${noun}`}
        >
          Next
        </button>
      </div>
    </div>
  );
}
