// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The counting and paging arithmetic, tested without a DOM.
 *
 * DataTable.test.tsx already drives these rules through rendered markup, and
 * these are not a duplicate of that. The component tests assert what an operator
 * SEES; these assert the boundaries where the rule flips, which are cheap to
 * enumerate here and expensive to reach through a rendered table — the row count
 * exactly at the cap, one below it and one above, a page index pushed past the
 * end by a shrinking list, a range label with no denominator to divide by.
 *
 * Those boundaries are where a count starts lying, and every one of them is an
 * off-by-one away from being wrong in the reassuring direction.
 */

import { describe, expect, it } from 'vitest';
import {
  clampPage,
  countClaim,
  filterRows,
  normalizeTerm,
  pageCount,
  pageSlice,
  rangeLabel,
  sortRows,
} from './table';
import type { RowCountKind } from './table';

describe('countClaim', () => {
  const capped: RowCountKind = { kind: 'capped', limit: 100 };

  it('claims a total only when the endpoint sends everything', () => {
    const claim = countClaim(12, { kind: 'exact' }, 'accounts');
    expect(claim).toEqual({ n: 12, more: false, title: '12 accounts' });
  });

  /**
   * The boundary, from both sides. Asked for 100 and given 100 is
   * indistinguishable from a namespace holding exactly 100, so the only honest
   * reading is "at least". Given 99, the server ran out before the cap and the
   * count IS exact — which matters because every namespace this has been
   * pointed at is under the cap, so a permanent "+" would be a permanent lie in
   * the common case.
   */
  it('flips to "at least" exactly at the cap and not before', () => {
    expect(countClaim(99, capped, 'rows').more).toBe(false);
    expect(countClaim(100, capped, 'rows').more).toBe(true);
  });

  it('treats an over-full response as capped rather than guessing it is exact', () => {
    // A gateway that ignores `limit` is a thing that happens. Inferring "exact"
    // from a response that overran the only constraint we set is the wrong
    // direction to guess in.
    expect(countClaim(140, capped, 'rows').more).toBe(true);
  });

  it('explains the cap in words, because "+" alone is a glyph nobody is born knowing', () => {
    expect(countClaim(100, capped, 'rows').title).toMatch(/capped at 100 rows/);
    expect(countClaim(4, capped, 'rows').title).toMatch(/Fewer than the 100-row cap/);
  });

  it('never claims a total when the server pages without totalling', () => {
    const claim = countClaim(25, { kind: 'unknown' }, 'runs');
    expect(claim.more).toBe(true);
    expect(claim.title).toMatch(/not a count of everything/);
  });

  it('singularises the noun so the sentence reads', () => {
    expect(countClaim(1, { kind: 'exact' }, 'accounts').title).toBe('1 account');
    expect(countClaim(2, { kind: 'exact' }, 'accounts').title).toBe('2 accounts');
  });
});

describe('rangeLabel', () => {
  it('states a denominator only for a count that has one', () => {
    expect(rangeLabel(0, 25, 137, { kind: 'exact' })).toBe('1–25 of 137');
  });

  it('marks the denominator as a floor when the cap bound', () => {
    expect(rangeLabel(0, 25, 200, { kind: 'capped', limit: 200 })).toBe('1–25 of 200+');
    // Under the cap there is nothing to qualify: the count is exact after all.
    expect(rangeLabel(0, 25, 88, { kind: 'capped', limit: 200 })).toBe('1–25 of 88');
  });

  /**
   * The case that must never grow an "of N". With keyset paging there is no
   * total, so the rows in hand are all that can be spoken about — "of these 30"
   * says that without inventing a namespace figure.
   */
  it('divides by nothing when there is no total to divide by', () => {
    expect(rangeLabel(0, 25, 30, { kind: 'unknown' })).toBe('1–25 of these 30');
  });

  it('renders the range 1-based and inclusive, as a human reads one', () => {
    // Half-open [25, 50) is the 26th through 50th rows.
    expect(rangeLabel(25, 50, 137, { kind: 'exact' })).toBe('26–50 of 137');
  });

  it('says "no rows" rather than printing an empty range', () => {
    expect(rangeLabel(0, 0, 0, { kind: 'exact' })).toBe('no rows');
  });
});

describe('pageCount', () => {
  it('counts a partial last page', () => {
    expect(pageCount(30, 25)).toBe(2);
    expect(pageCount(50, 25)).toBe(2);
    expect(pageCount(51, 25)).toBe(3);
  });

  /**
   * Zero rows is ONE page, not zero. A zero would make every index out of
   * bounds, leaving nowhere to render an empty state — and "page 1 of 0" is not
   * a sentence.
   */
  it('calls an empty list one page', () => {
    expect(pageCount(0, 25)).toBe(1);
  });

  it('survives a nonsense page size rather than dividing by zero', () => {
    expect(pageCount(10, 0)).toBe(1);
    expect(pageCount(10, -5)).toBe(1);
  });
});

describe('clampPage', () => {
  it('holds an in-range index unchanged', () => {
    expect(clampPage(1, 60, 25)).toBe(1);
  });

  it('pulls an index back to the last real page', () => {
    // Three pages exist (0,1,2); page 9 does not.
    expect(clampPage(9, 60, 25)).toBe(2);
  });

  it('refuses to go below the first page', () => {
    expect(clampPage(-3, 60, 25)).toBe(0);
  });

  /**
   * The case that happens without anyone clicking anything: a filter narrows 60
   * rows to 3 while the operator sits on page 2. Clamping at the point of use is
   * what makes that show the nearest real page instead of a blank table.
   */
  it('lands on a real page when the list shrinks underneath the index', () => {
    expect(clampPage(2, 3, 25)).toBe(0);
  });

  it('does not propagate a non-finite index into a slice', () => {
    expect(clampPage(NaN, 60, 25)).toBe(0);
    expect(clampPage(Infinity, 60, 25)).toBe(2);
  });
});

describe('pageSlice', () => {
  it('returns a half-open range that never runs past the data', () => {
    expect(pageSlice(0, 60, 25)).toEqual([0, 25]);
    expect(pageSlice(2, 60, 25)).toEqual([50, 60]);
  });

  it('clamps before slicing, so an out-of-range page still yields real rows', () => {
    expect(pageSlice(99, 60, 25)).toEqual([50, 60]);
  });

  it('yields an empty range for no rows rather than a negative one', () => {
    expect(pageSlice(0, 0, 25)).toEqual([0, 0]);
  });
});

describe('filterRows', () => {
  const data = [
    { name: 'nightly-etl', spec: 'example-etl' },
    { name: 'hourly-sync', spec: 'sync-job' },
    { name: 'weekly-report', spec: null as string | null },
  ];
  const fields = (r: (typeof data)[number]) => [r.name, r.spec];

  it('matches a substring case-insensitively', () => {
    // Upper-case term, lower-case data, and "etl" sits in the MIDDLE of
    // "nightly-etl" — a prefix match would return nothing here.
    expect(filterRows(data, 'ETL', fields).map((r) => r.name)).toEqual(['nightly-etl']);
  });

  it('matches on any declared field, not only the first', () => {
    // "sync-job" is the spec of hourly-sync and appears in no name.
    expect(filterRows(data, 'job', fields).map((r) => r.name)).toEqual(['hourly-sync']);
  });

  it('returns everything for an empty or whitespace term', () => {
    expect(filterRows(data, '', fields)).toHaveLength(3);
    expect(filterRows(data, '   ', fields)).toHaveLength(3);
  });

  it('treats a null field as absent rather than throwing on it', () => {
    // weekly-report has a null spec; a filter that reached into it unguarded
    // would take the whole page down with a TypeError.
    expect(filterRows(data, 'weekly', fields).map((r) => r.name)).toEqual(['weekly-report']);
  });
});

describe('sortRows', () => {
  const data = [{ n: 3 }, { n: 1 }, { n: 2 }];
  const byN = (a: { n: number }, b: { n: number }) => a.n - b.n;

  it('sorts ascending and descending from one comparator', () => {
    expect(sortRows(data, byN, 'asc').map((r) => r.n)).toEqual([1, 2, 3]);
    expect(sortRows(data, byN, 'desc').map((r) => r.n)).toEqual([3, 2, 1]);
  });

  it('leaves the order alone when the column gave no comparator', () => {
    expect(sortRows(data, undefined, 'asc').map((r) => r.n)).toEqual([3, 1, 2]);
  });

  /**
   * In-place sorting would reorder the array React Query is caching, which
   * surfaces as a list that reshuffles itself on an unrelated refetch — a bug
   * that is very hard to attribute back to a sort click.
   */
  it('does not mutate the input', () => {
    const input = [...data];
    sortRows(input, byN, 'asc');
    expect(input.map((r) => r.n)).toEqual([3, 1, 2]);
  });
});

describe('normalizeTerm', () => {
  it('trims and lower-cases once for the whole scan', () => {
    expect(normalizeTerm('  Nightly ')).toBe('nightly');
  });
});
