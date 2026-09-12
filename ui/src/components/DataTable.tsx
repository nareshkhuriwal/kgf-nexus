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

import { useEffect, useId, useMemo, useRef, useState } from 'react';
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

/**
 * Row selection, for the tables that offer a bulk action.
 *
 * THE STATE LIVES IN THE CALLER, not here, and that is the whole shape of this
 * prop. The page is what performs the action, what refetches afterwards, and
 * what has to decide whether a row is eligible at all — so the page is what
 * must be able to read the selection. A DataTable that owned it internally
 * would have to hand it back out through a callback anyway, which is the same
 * data flowing the same direction with one more place for the two copies to
 * disagree.
 */
export interface TableSelection<Row> {
  /**
   * The selected row IDS, not rows.
   *
   * Every list behind these tables polls, and a poll replaces the row OBJECTS
   * with structurally-identical new ones. A Set of rows would therefore empty
   * itself every few seconds — the selection would appear to survive right up
   * until the operator looked away for one poll interval.
   */
  selected: ReadonlySet<string>;
  /**
   * Called with the COMPLETE next selection, not with one toggled id.
   *
   * The header checkbox changes many rows at once, and expressing that as N
   * onToggle calls would make the page perform N setState reductions to carry
   * out one intent — every one of which re-renders, and any one of which could
   * be batched against a stale closure. One call, one intent, one new Set.
   */
  onChange: (next: ReadonlySet<string>) => void;
  /**
   * Identity for SELECTION, deliberately separate from `rowKey`.
   *
   * They are usually the same value and they answer different questions:
   * `rowKey` is React's reconciliation key, this is the id the bulk action is
   * issued against. Coupling them would mean a table needing a composite React
   * key (a spec name plus its version, say) could not be selected by id at all.
   */
  rowId: (row: Row) => string;
  /**
   * Which rows can be actioned. Rows that cannot get NO checkbox rather than a
   * disabled one.
   *
   * A revoked token cannot be revoked again; a SUCCEEDED run cannot be
   * cancelled. Excluding them at the point of selection is what keeps
   * "Cancel 8 runs" from promising work on rows where it would certainly fail —
   * the alternative is letting the operator build the selection and then
   * reporting failures for rows that were never candidates, which reads as the
   * platform breaking rather than as the operator's mistake.
   *
   * Absent means every row is selectable.
   */
  isSelectable?: (row: Row) => boolean;
  /**
   * Accessible-name fragment for each row's checkbox, rendered as
   * `Select ${label(row)}`. Without it every checkbox in the column is named
   * "Select row" and a screen-reader user moving down the column cannot tell
   * which row they are about to select. Falls back to the row's id.
   */
  label?: (row: Row) => string;
}

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
  /**
   * Row selection, for the tables that offer a bulk action.
   *
   * ABSENT MEANS NO SELECTION COLUMN AND NO BEHAVIOUR CHANGE — every existing
   * caller keeps working untouched, which is what makes this additive rather
   * than a migration across six screens.
   *
   * When present the checkbox column is PREPENDED here rather than declared by
   * the caller. A column the caller has to remember to add is a column somebody
   * forgets, and a selection UI that renders on four tables out of five is
   * worse than none: the operator learns the control exists and then meets a
   * table where it silently does not.
   */
  selection?: TableSelection<Row>;
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
  selection,
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
    search?.mode === 'client' && term.trim() !== ''
      ? { kind: 'unknown' }
      : count;
  const claim = countClaim(sorted.length, claimKind, noun);

  const searching = search?.mode === 'server' && search.pending === true;

  /**
   * WHAT SELECT-ALL IS ENTITLED TO SELECT — the honesty rule, applied to rows
   * rather than to a count.
   *
   * It is the FILTERED, SORTED set (`sorted`), not the current page's slice and
   * not the unfiltered `rows`. Both alternatives are wrong in a way the
   * operator cannot see:
   *
   * - the page slice would mean an operator who searched "nightly-" and pressed
   *   select-all got the first 25 matches rather than the matches, and the
   *   count in the bar would quietly disagree with the sentence they read;
   * - `rows` would select rows the filter has taken off screen, so a "Delete 3"
   *   could hit a row they cannot see and did not choose.
   *
   * `sorted` is exactly "what this table currently claims to be showing", which
   * is the set the label below describes and the only set the operator can
   * verify by scrolling.
   *
   * AND IT NEVER REACHES PAST THE CLIENT. There is no "select all N across all
   * pages" affordance anywhere below, because the client does not HAVE those
   * rows: on a capped fetch they were never sent, and a control claiming
   * authority over them would be promising work on a set whose size it is
   * guessing at. The label says "loaded" wherever that is the case — see
   * `selectAllLabel`.
   */
  /**
   * ── SELECTION SURVIVES A POLL. IT DOES NOT SURVIVE A FILTER OR A PAGE TURN.
   *
   * These are two different questions and they get two different answers,
   * because the operator's intent is preserved by one and violated by the other.
   *
   * A POLL: the selection survives, and it is what the `ReadonlySet<string>` of
   * IDS above exists for. Every list here refetches on a timer, and each refetch
   * replaces the row OBJECTS with structurally-identical new ones. Selection by
   * row identity would therefore empty itself every few seconds — silently, and
   * only when the operator looked away long enough. Nothing about the operator's
   * intent changed when the same rows arrived again, so nothing about the
   * selection should. (A selected row that has genuinely GONE from the refetch
   * is a different matter: it is no longer in `sorted`, so it is not in the rows
   * the page hands the bulk bar, so it is neither counted nor actioned. The bar
   * never claims to act on a row the table cannot produce.)
   *
   * A FILTER CHANGE OR A PAGE TURN: the selection is CLEARED, and this is the
   * decision worth arguing. The handoff proposed keeping ids across a filter, on
   * the reasoning that dropping them discards an operator's work when they type
   * in the search box. That reasoning is right about the cost and wrong about
   * the risk. Carrying ids across a filter change means the bulk bar can be
   * showing "Delete 3 schedules" while the table shows rows that are not those
   * three — the operator reads a count, looks at a screen that does not contain
   * what the count describes, and presses a destructive button. The selection is
   * made by POINTING AT ROWS, so it is only meaningful against the rows that
   * were on screen when it was made; once the view is a different set of rows,
   * the selection is a claim about rows the operator can no longer see.
   *
   * Re-selecting after a filter costs a few clicks. Deleting three rows an
   * operator could not see costs a restore from backup. The asymmetry decides
   * it, and it is the same asymmetry the count claims resolve the same way.
   *
   * ── HOW THE RESET IS SEQUENCED, WHICH IS THE FIDDLY PART ──────────────────
   *
   * The selection state belongs to the PAGE, so clearing it means calling the
   * page's `onChange` — and a child may not set a parent's state during its own
   * render (React refuses it, loudly and correctly). So the clear is committed
   * in an effect.
   *
   * An effect ALONE would leave a window: the render that first sees the new
   * term would still be holding the previous view's ids, and a select-all
   * counting them would flash a number describing rows that are already gone.
   * So this render does not wait for the effect — it derives `staleView` and
   * treats the selection as already empty. The effect then makes the page agree.
   * No render ever shows a selection belonging to a view that has left the
   * screen, and no state is set during render.
   *
   * A SORT is deliberately NOT in this list. Reordering shows the same rows in a
   * different sequence: every selected row is still on screen and still means
   * what it meant, so clearing would be a loss with nothing bought.
   */
  const viewKey = `${term}\u0000${clientPaged ? safePage : 0}`;
  const selectionViewRef = useRef(viewKey);
  /**
   * True on the one render that first sees a new view. Everything below reads
   * the selection through this, so the stale ids are never counted, never
   * rendered as ticked, and never handed to a select-all label.
   */
  const staleView = selectionViewRef.current !== viewKey;
  useEffect(() => {
    if (selectionViewRef.current === viewKey) return;
    selectionViewRef.current = viewKey;
    // Guarded on non-empty so a table with nothing selected does not call back
    // into the page on every page turn, re-rendering it for no change.
    if (selectionRef.current && selectionRef.current.selected.size > 0) {
      selectionRef.current.onChange(new Set());
    }
  }, [viewKey]);

  /**
   * A live handle on the current `selection` prop.
   *
   * The reset effect below needs to read the selection at the moment it fires,
   * but it must be keyed on the VIEW, not on the selection: callers pass a fresh
   * object literal on every render, so depending on it directly would re-run the
   * effect continuously. A ref updated each render gives the effect current
   * values without putting the object in its dependency list.
   */
  const selectionRef = useRef(selection);
  selectionRef.current = selection;

  const selectable = useMemo(
    () =>
      selection
        ? sorted.filter((r) => selection.isSelectable?.(r) ?? true)
        : [],
    [sorted, selection],
  );
  /**
   * Read through `isSelected` everywhere, never through `selection.selected`
   * directly: on the render that first sees a new view the ids still in the
   * page's state belong to the PREVIOUS view, and counting them would flash a
   * select-all claiming rows that have already left the screen.
   */
  const isSelected = (row: Row) =>
    !!selection && !staleView && selection.selected.has(selection.rowId(row));
  const selectedCount = selection ? selectable.filter(isSelected).length : 0;
  const allSelected =
    selectable.length > 0 && selectedCount === selectable.length;
  /**
   * The third state, and the one that gets dropped.
   *
   * `indeterminate` is a DOM PROPERTY, not an attribute — setting it in JSX
   * does nothing at all, silently. A header checkbox that rendered plain
   * unchecked while three rows were selected would be claiming "nothing is
   * selected" at the same moment the bulk bar below it says otherwise, leaving
   * the operator to pick which control to believe. It is applied through a ref
   * effect below, which is the only channel that reaches the property.
   */
  const someSelected = selectedCount > 0 && !allSelected;

  /**
   * The set every toggle below builds on.
   *
   * Empty on a stale-view render, for the same reason `isSelected` is false
   * there: the ids still in the page's state belong to the view that has just
   * left the screen, and a toggle that started from them would resurrect a
   * selection the filter change was supposed to end.
   */
  const baseSelection = () =>
    selection && !staleView ? new Set(selection.selected) : new Set<string>();

  function toggleAll() {
    if (!selection) return;
    const next = baseSelection();
    if (allSelected) {
      // Clearing removes only the ids this table is CURRENTLY showing, rather
      // than replacing the whole set with an empty one. On a filtered view the
      // operator asked to clear what they can see; silently dropping selections
      // the filter is hiding would be a second, unrequested action.
      for (const r of selectable) next.delete(selection.rowId(r));
      selection.onChange(next);
      return;
    }
    for (const r of selectable) next.add(selection.rowId(r));
    selection.onChange(next);
  }

  function toggleOne(row: Row) {
    if (!selection) return;
    const id = selection.rowId(row);
    const next = baseSelection();
    if (next.has(id)) next.delete(id);
    else next.add(id);
    selection.onChange(next);
  }

  /**
   * What the select-all control CLAIMS, in words, keyed to the same
   * RowCountKind the count chip reads.
   *
   * `exact` is the only kind entitled to say "all N": the endpoint takes no
   * limit, so the rows in hand ARE the namespace's rows and the claim is true.
   * Every other kind gets "loaded" — the client holds a page or a capped slice
   * and cannot speak for what it never received. A filter narrows it further
   * still, and "matching" says so, because select-all then covers the matches
   * rather than the table.
   *
   * This is the row-selection twin of `countClaim` in lib/table.ts, and it says
   * the same thing for the same reason: a control that over-claims its scope is
   * worse than one that is merely cautious, because the operator acts on it.
   */
  const filteringLocally = search?.mode === 'client' && term.trim() !== '';
  const selectAllScope = filteringLocally
    ? `matching ${noun}`
    : count.kind === 'exact'
      ? noun
      : `loaded ${noun}`;
  const selectAllLabel = allSelected
    ? `Clear the selected ${noun}`
    : `Select all ${selectable.length} ${selectAllScope}`;

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

  /**
   * The rendered column set: the selection column, then the caller's.
   *
   * Built here rather than in a memo because it closes over the selection
   * state, which changes on every tick of a checkbox — a memo would need every
   * one of those in its dependency list and would then never hit. The array is
   * a handful of objects; the cost of building it is far below the cost of a
   * stale one.
   */
  const allColumns: Column<Row>[] = selection
    ? [
        {
          key: '__select',
          /* The select-all lives in the HEADER CELL, above the column of
             checkboxes it governs, which is only expressible because
             Column.header is a ReactNode. It was in the toolbar before, beside
             the search box, where the operator had to infer what it acted on. */
          header: (
            <SelectAll
              checked={allSelected}
              indeterminate={someSelected}
              /* Nothing selectable means nothing to select. Disabled rather
                 than hidden: the column still exists and a control that
                 vanishes and reappears as rows poll in is harder to trust than
                 one that is visibly inert. */
              disabled={selectable.length === 0}
              label={selectAllLabel}
              onChange={toggleAll}
            />
          ),
          /* A hint only — table-layout is auto — sized for a checkbox plus the
             cell padding either side, so the column does not claim a share of
             the width the data columns need. */
          width: '2.5rem',
          cellClass: 'dt-select-cell',
          cell: (row) => {
            const eligible = selection.isSelectable?.(row) ?? true;
            /* An ineligible row renders NO checkbox rather than a disabled one.
               A disabled checkbox invites the operator to work out why it is
               disabled; the row's own status cell (revoked, SUCCEEDED, DEAD)
               already says, and saying it twice in two vocabularies is worse
               than saying it once. */
            if (!eligible) return null;
            const id = selection.rowId(row);
            return (
              <input
                type="checkbox"
                checked={isSelected(row)}
                onChange={() => toggleOne(row)}
                /* THE ROW IS OFTEN ALSO A LINK. Runs and workers both put a
                   <Link> in a neighbouring cell, and the row lights on
                   :focus-within — so a click that bubbled out of this checkbox
                   and reached an ancestor handler would select the row AND
                   navigate away from the table it was selected in. Stopping
                   propagation here keeps the two interactions separate without
                   either control knowing about the other. It does not
                   preventDefault: the checkbox's own toggle must still happen,
                   and Space on a focused checkbox is a keyboard path that never
                   produces a click on the link at all. */
                onClick={(e) => e.stopPropagation()}
                aria-label={`Select ${selection.label?.(row) ?? id}`}
              />
            );
          },
        },
        ...columns,
      ]
    : columns;

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
                  search.mode === 'server'
                    ? 'name contains…'
                    : 'filter these rows…'
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
        <span
          className="chip dt-count"
          title={searching ? undefined : claim.title}
        >
          {searching ? 'searching…' : `${claim.n}${claim.more ? "+" : ""}`}
        </span>
      </div>

      {/* The one empty state that is this component's to own: rows arrived and
          the table's own filter matched none of them. AsyncPanel cannot phrase
          this one because it never sees the term. */}
      {visible.length === 0 &&
      term.trim() !== '' &&
      search?.mode === 'client' ? (
        <Empty
          title={
            noMatchTitle ?? `No ${noun} on this page match “${term.trim()}”`
          }
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
                {allColumns.map((c) => (
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
                  {allColumns.map((c) => (
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
 * The header checkbox, and the ONE reason it is a component rather than inline
 * JSX: `indeterminate`.
 *
 * It is a DOM property with no HTML attribute behind it, so React has nothing
 * to write when it appears as a JSX prop — the attribute is simply dropped, in
 * silence, and the checkbox renders plain-unchecked. That failure is
 * particularly bad here because it is not a missing affordance but a WRONG
 * STATEMENT: an unchecked select-all says "nothing is selected" at the same
 * moment the bulk bar underneath says three rows are, and an operator with two
 * controls disagreeing has to guess which one the delete button believes.
 *
 * So it is written to the node directly, in an effect keyed on the value. A ref
 * CALLBACK would also work and is what TokensPage used as a stopgap, but it
 * runs only when the node identity changes, so it is a trap of its own: a
 * re-render that keeps the same node (the common case — every tick of a row
 * checkbox) would not re-run it under React 19's ref semantics. The effect runs
 * on every change of the value it writes, which is the property this needs.
 */
function SelectAll({
  checked,
  indeterminate,
  disabled,
  label,
  onChange,
}: {
  checked: boolean;
  indeterminate: boolean;
  disabled: boolean;
  label: string;
  onChange: () => void;
}) {
  const ref = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = indeterminate;
  }, [indeterminate]);

  return (
    <input
      ref={ref}
      type="checkbox"
      className="dt-select-all"
      checked={checked}
      disabled={disabled}
      onChange={onChange}
      /* The accessible name carries the SCOPE, not just the verb: "Select all
         12 loaded runs" tells a screen-reader user that the control does not
         reach the rows nobody fetched, which is the one thing about it that is
         not obvious from the control itself. The caller's count kind decides
         the wording; see selectAllLabel. */
      aria-label={label}
    />
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
          {serverMode
            ? `page ${paging.pageNumber}`
            : `page ${page + 1} of ${pages}`}
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
