// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The pure arithmetic behind DataTable: filtering, sorting, clamping a page
 * index, and — the part that actually needed extracting — deciding what a row
 * count is ENTITLED TO CLAIM.
 *
 * Kept free of React for the same reason lib/dag.ts is. The interesting logic
 * here is not "slice an array"; it is the honesty rules, and those are worth
 * asserting directly rather than only through a rendered DOM where an off-by-one
 * in a page clamp reads as a blank table and nothing more.
 *
 * ── WHY THE COUNTING RULES NEED A MODULE AT ALL ───────────────────────────
 *
 * Every list endpoint behind this console answers a DIFFERENT question, and the
 * differences are invisible in the response body:
 *
 *   /specs        takes `limit` (default 200). Answers "up to 200 rows".
 *   /schedules    takes `limit` (default 100). Same shape.
 *   /audit        takes `limit`. Same shape.
 *   /users        takes nothing. Answers "all of them" — but only because the
 *                 gateway has no pagination there yet, which is a fact about
 *                 today's gateway and not a promise.
 *   /runs         real keyset pagination, `next_before`. The only one.
 *   /workers      takes nothing at all.
 *
 * A component that received `rows.length` from any of those and rendered
 * "247 results" or "page 3 of 10" would be making a claim the response does not
 * support. When the server was asked for at most 200 and returned exactly 200,
 * the truthful statement is "200+, capped" — NOT "200". And "page 3 of 10" is
 * unstateable without a total, which none of these endpoints send.
 *
 * That is the same distinction the attention panel already draws with the
 * server's `truncated` flag (OverviewPage.tsx) and that CapacityGrid draws with
 * `derived`: a number the page computed itself and a number the server vouched
 * for are different claims, and the component refuses to let a caller present
 * one as the other. This module is that rule for row counts.
 */

/** How a column's cell contents sit in the box. */
export type ColumnAlign = 'start' | 'end';

/**
 * One column of a DataTable, over row type `Row`.
 *
 * Generic rather than `any`-typed on purpose: `cell` receives the caller's own
 * row type, so a column that reaches for a field the row does not have is a
 * compile error rather than a blank cell discovered in production.
 */
export interface Column<Row> {
  /**
   * Identifies the column. Used as the React key, as the sort target, and as
   * the value of aria-sort's owning header — so it must be unique within a
   * column set. It is deliberately NOT required to be a key of `Row`: the
   * schedules table has a column carrying a cron expression AND its timezone,
   * and the runs table has one carrying a progress bar derived from four
   * fields. Tying the id to a field name would have forced those to lie about
   * which field they render.
   */
  key: string;
  /**
   * The `<th>` text. Lower case by convention here because the stylesheet
   * upper-cases headers itself — passing "Name" would render "NAME" either way,
   * and the existing tables all pass lower case.
   */
  header: string;
  /** Renders the `<td>` contents for one row. */
  cell: (row: Row) => React.ReactNode;
  /**
   * Makes the header a sort button. Absent means the column is not sortable,
   * which is the right default: a comparator over a rendered React node is not
   * derivable, so a column can only be sorted if the caller says how.
   *
   * Returns the usual negative/zero/positive. Ascending order; DataTable
   * inverts it for descending rather than asking for two comparators.
   */
  sort?: (a: Row, b: Row) => number;
  /** Right-aligns numeric columns. Presentation only. */
  align?: ColumnAlign;
  /**
   * A width HINT, as a CSS length the caller owns (a token, a ch measure, a
   * percentage). Hint because `table-layout` here is auto: the browser will
   * overrule it for content that cannot fit, which is the behaviour a dense
   * operator table wants — an id that would be clipped widens its column
   * instead.
   */
  width?: string;
  /** Extra class on every `<td>` in the column, for `mono`/`nowrap`/`faint`. */
  cellClass?: string;
}

/**
 * What the row count is allowed to say.
 *
 * Three values rather than a boolean, because there are genuinely three
 * situations and collapsing any two of them produces a misleading count:
 *
 * - `exact`   — these rows are all the rows there are. The only endpoints that
 *               can honestly claim this are the ones with no limit parameter
 *               (users, tokens, workers): what came back IS the namespace's
 *               contents. "12 accounts" is true.
 * - `capped`  — the server was asked for at most N and may have more. Whether
 *               it DID cap is then a function of the row count, which is why
 *               this carries the cap rather than a bare flag: see `countClaim`.
 * - `unknown` — nobody can say. The server pages but does not total (runs), so
 *               a count of what is on screen is a count of this page only.
 *
 * Named `RowCountKind` rather than `TotalKind` because the honest cases are
 * precisely the ones where there is no total.
 */
export type RowCountKind =
  | { kind: 'exact' }
  | { kind: 'capped'; limit: number }
  | { kind: 'unknown' };

/** What a count chip may display, and what it may claim while displaying it. */
export interface CountClaim {
  /** The digits. Always the rows actually in hand — never an extrapolation. */
  n: number;
  /**
   * True when there may be rows beyond these. Renders as the "+" the attention
   * panel already uses for exactly this meaning, so the two agree.
   */
  more: boolean;
  /**
   * The accessible/tooltip sentence. It is the whole reason this returns an
   * object: "200+" in a chip is a glyph an operator has to already know, and
   * the sentence is what makes it legible the first time.
   */
  title: string;
}

/**
 * Turns a row count plus what the fetch promised into a claim.
 *
 * THE LOAD-BEARING CASE is `capped` where `n === limit`. Asked for 200, given
 * 200 — that is indistinguishable from a namespace holding exactly 200, so the
 * honest reading is "at least 200". Given 137 of a possible 200, the server
 * ran out of rows before the cap and the count IS exact, which is why a capped
 * fetch does not permanently forfeit an exact count. Getting that second half
 * right matters more than the first: every namespace this console has been
 * pointed at so far is under the cap, so a permanent "+" would be a permanent
 * lie in the common case.
 *
 * `n > limit` is treated as capped-and-more as well. It should be impossible,
 * but a gateway that ignores `limit` is a thing that happens, and inferring
 * "exact" from a response that overran the only constraint we set is the wrong
 * direction to guess in.
 */
export function countClaim(n: number, count: RowCountKind, noun: string): CountClaim {
  const plural = n === 1 ? noun.replace(/s$/, '') : noun;
  if (count.kind === 'exact') {
    return { n, more: false, title: `${n} ${plural}` };
  }
  if (count.kind === 'unknown') {
    return {
      n,
      more: true,
      title: `${n} ${plural} on this page. The server pages this list and does not report a total, so this is not a count of everything.`,
    };
  }
  const hit = n >= count.limit;
  return {
    n,
    more: hit,
    title: hit
      ? `At least ${n} ${plural}. The request was capped at ${count.limit} rows and that many came back, so there may be more that were not fetched.`
      : `${n} ${plural}. Fewer than the ${count.limit}-row cap, so this is all of them.`,
  };
}

/** Where search happens, and therefore what a filtered count means. */
export type SearchMode<Row> =
  /**
   * The caller's own query carries the term to the server, so every row this
   * component holds is already a match. It filters NOTHING — filtering an
   * already-filtered list client-side is how a second, narrower and undocumented
   * predicate sneaks in.
   */
  | {
      mode: 'server';
      value: string;
      onChange: (value: string) => void;
      /**
       * True while a debounce has not settled, i.e. the rows on screen answer
       * an OLDER term. WorkflowsPage already renders this distinction and it
       * has to survive the move into here: a count shown during that window
       * belongs to the previous question.
       */
      pending?: boolean;
    }
  /**
   * The component filters the rows it was given. Honest only about those rows —
   * and `countKind` is what stops it claiming more: filtering a capped 200 of
   * 400 rows and reporting "3 results" would be a statement about the namespace
   * that the component is in no position to make.
   */
  | {
      mode: 'client';
      /**
       * The haystack, per row. A function rather than a list of field names so
       * a row can contribute a derived string (a spec name plus its version, a
       * label map flattened) and so the match cannot silently reach a field the
       * table does not render.
       */
      fields: (row: Row) => (string | null | undefined)[];
    };

/** Which column is sorted, and in which direction. */
export interface SortState {
  key: string;
  dir: 'asc' | 'desc';
}

/** Lower-cases and trims once, so a filter over N rows does not do it N times. */
export function normalizeTerm(term: string): string {
  return term.trim().toLowerCase();
}

/**
 * Client-side filter: a row matches when ANY of its declared fields contains
 * the term, case-insensitively.
 *
 * Substring rather than prefix, and OR across fields rather than AND, because
 * this is the behaviour the server-side catalog search already has and two
 * search boxes in one console that behave differently is worse than either
 * behaviour. An empty term matches everything and short-circuits.
 */
export function filterRows<Row>(
  rows: Row[],
  term: string,
  fields: (row: Row) => (string | null | undefined)[],
): Row[] {
  const needle = normalizeTerm(term);
  if (needle === '') return rows;
  return rows.filter((row) =>
    fields(row).some((f) => (f ?? '').toLowerCase().includes(needle)),
  );
}

/**
 * Sorts a copy, never in place.
 *
 * In-place would mutate the array React Query is caching, which is the kind of
 * bug that shows up as a list that reorders itself on an unrelated refetch.
 * `toSorted` is not reachable at this TS target, so it is spread-then-sort.
 */
export function sortRows<Row>(
  rows: Row[],
  comparator: ((a: Row, b: Row) => number) | undefined,
  dir: 'asc' | 'desc',
): Row[] {
  if (!comparator) return rows;
  const sorted = [...rows].sort(comparator);
  return dir === 'asc' ? sorted : sorted.reverse();
}

/**
 * How many pages `total` rows occupy at `pageSize`.
 *
 * Zero rows is ONE page, not zero. A zero here would make every page index out
 * of bounds and there would be nowhere to render the empty state — and "page 1
 * of 0" is not a sentence.
 */
export function pageCount(total: number, pageSize: number): number {
  if (pageSize <= 0) return 1;
  return Math.max(1, Math.ceil(total / pageSize));
}

/**
 * Forces a page index into range.
 *
 * Applied on every render rather than only in the click handlers, because the
 * index goes out of range without anyone clicking anything: a filter that
 * narrows 200 rows to 3 while the operator is on page 4, or a poll that removes
 * the last row of the last page. Clamping at the point of USE means those cases
 * show the nearest real page instead of a blank table.
 */
export function clampPage(page: number, total: number, pageSize: number): number {
  const last = pageCount(total, pageSize) - 1;
  if (!Number.isFinite(page)) return 0;
  return Math.min(Math.max(Math.trunc(page), 0), last);
}

/** The half-open row range a page shows, already clamped. */
export function pageSlice(page: number, total: number, pageSize: number): [number, number] {
  const p = clampPage(page, total, pageSize);
  const from = p * pageSize;
  return [from, Math.min(from + pageSize, total)];
}

/**
 * "1–25 of 137" / "1–25 of 137+" / "1–25 of these 137".
 *
 * Built here rather than inline in JSX so it can be asserted directly, because
 * the wording is the honesty and the wording is easy to regress. The three
 * shapes correspond exactly to the three RowCountKinds:
 *
 * - exact:   "of 137" is a fact about the namespace.
 * - capped:  "of 137+" when the cap was reached — there are more we did not
 *            fetch; plain "of 137" when it was not, because then the cap never
 *            bound and the count is exact after all.
 * - unknown: no "of" at all. There is no denominator to state, so the range
 *            stands alone rather than being divided by a number we invented.
 */
export function rangeLabel(
  from: number,
  to: number,
  total: number,
  count: RowCountKind,
): string {
  // from/to arrive as 0-based half-open and are shown 1-based inclusive, which
  // is how every range an operator has ever read is written.
  if (total === 0) return 'no rows';
  const span = `${from + 1}–${to}`;
  if (count.kind === 'unknown') return `${span} of these ${total}`;
  const capped = count.kind === 'capped' && total >= count.limit;
  return `${span} of ${total}${capped ? '+' : ''}`;
}
