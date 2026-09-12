// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for the shared list table.
 *
 * What is pinned here is the HONESTY, not the markup. Sorting an array and
 * slicing it into pages is arithmetic that would be hard to get wrong and
 * useless to over-test; what is genuinely easy to get wrong — and what the whole
 * component exists to prevent — is a count or a page label making a claim the
 * data does not support. So the bulk of this file asserts what the table says
 * about its rows rather than which rows it drew:
 *
 *   - a capped fetch that came back full says "at least N", never "N";
 *   - the same capped fetch that came back SHORT of the cap says "N" exactly,
 *     because the cap never bound and a permanent "+" would be its own lie;
 *   - a client-side filter over a capped set forfeits the count entirely,
 *     because 3 of the 100 rows we hold is not 3 in the namespace;
 *   - no "page N of M" is printed where M is unknown.
 *
 * The behavioural tests around those (search filters, page size changes the
 * visible count, pagination clamps at both ends, sort toggles aria-sort) are
 * here because each is a bound that fails silently: an off-by-one in a page
 * clamp renders as a blank table and nothing else.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';
import { DataTable } from './DataTable';
import type { Column } from './DataTable';

interface Row {
  id: string;
  name: string;
  size: number;
}

const columns: Column<Row>[] = [
  {
    key: 'name',
    header: 'name',
    sort: (a, b) => a.name.localeCompare(b.name),
    cell: (r) => r.name,
  },
  {
    key: 'size',
    header: 'size',
    align: 'end',
    sort: (a, b) => a.size - b.size,
    cell: (r) => r.size,
  },
  // Deliberately without a comparator: the component must render this header as
  // plain text and NOT as a button, because a sort control that cannot sort is
  // the dead control the rest of this console refuses.
  { key: 'note', header: 'note', cell: () => 'n/a' },
];

/** n rows named row-001, row-002 …, so ordering is legible in an assertion. */
function rows(n: number): Row[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `r${i}`,
    name: `row-${String(i + 1).padStart(3, '0')}`,
    size: n - i,
  }));
}

function bodyNames(): string[] {
  const body = document.querySelector('tbody')!;
  return Array.from(body.querySelectorAll('tr')).map(
    (tr) => tr.querySelector('td')!.textContent ?? '',
  );
}

/**
 * The count chip's text. Queried by class rather than by its text, because the
 * digits it shows also appear in the `size` column's cells — and an assertion
 * that could be satisfied by a row's data is not an assertion about the count.
 */
function chip(): HTMLElement {
  return document.querySelector('.dt-count')!;
}

function table(rowData: Row[], props: Partial<Parameters<typeof DataTable<Row>>[0]> = {}) {
  return render(
    <DataTable
      rows={rowData}
      columns={columns}
      rowKey={(r) => r.id}
      noun="things"
      {...props}
    />,
  );
}

describe('search', () => {
  it('filters the rows in client mode', () => {
    table(rows(5), {
      search: { mode: 'client', fields: (r) => [r.name] },
    });
    expect(bodyNames()).toHaveLength(5);

    fireEvent.change(screen.getByLabelText('filter the loaded things'), {
      target: { value: 'row-003' },
    });

    expect(bodyNames()).toEqual(['row-003']);
  });

  it('matches case-insensitively on any declared field, not just the first', () => {
    const data: Row[] = [
      { id: 'a', name: 'alpha', size: 1 },
      { id: 'b', name: 'beta', size: 2 },
    ];
    render(
      <DataTable
        rows={data}
        columns={columns}
        rowKey={(r) => r.id}
        noun="things"
        // Two fields, and the term matches only the SECOND one — so a filter
        // that only ever looked at fields()[0] would pass a single-field test
        // and fail here.
        search={{ mode: 'client', fields: (r) => [r.name, r.id === 'b' ? 'TAGGED' : ''] }}
      />,
    );

    fireEvent.change(screen.getByLabelText('filter the loaded things'), {
      target: { value: 'tagged' },
    });
    expect(bodyNames()).toEqual(['beta']);
  });

  /**
   * The mode distinction, at the level it is actually visible to an operator.
   * A server-mode table must not quietly filter what it was handed: the caller's
   * query already did the matching, and a second predicate here would narrow the
   * result by rules nobody documented.
   */
  it('does not filter locally in server mode — it only reports the term upward', () => {
    const seen: string[] = [];
    render(
      <DataTable
        rows={rows(4)}
        columns={columns}
        rowKey={(r) => r.id}
        noun="things"
        search={{ mode: 'server', value: 'row-002', onChange: (v) => seen.push(v) }}
      />,
    );

    // All four rows are still drawn even though the term matches exactly one.
    expect(bodyNames()).toHaveLength(4);

    fireEvent.change(screen.getByLabelText('search things by name'), {
      target: { value: 'zzz' },
    });
    expect(seen).toEqual(['zzz']);
  });

  /**
   * The two modes must be TELLABLE APART on screen. An operator cannot see
   * where a search ran, and the reach of the result depends entirely on that.
   */
  it('says where the search happened, differently for each mode', () => {
    const { unmount } = table(rows(2), {
      search: { mode: 'client', fields: (r) => [r.name] },
    });
    expect(screen.getByText(/filters the loaded rows only/i)).toBeInTheDocument();
    unmount();

    table(rows(2), {
      search: { mode: 'server', value: '', onChange: () => {} },
    });
    expect(screen.getByText(/searched on the server/i)).toBeInTheDocument();
  });

  it('says it is searching rather than showing a count for the settled-out term', () => {
    table(rows(9), {
      search: { mode: 'server', value: 'ro', onChange: () => {}, pending: true },
    });
    // The chip says what it is DOING, not a number belonging to the old term.
    expect(chip()).toHaveTextContent('searching…');
    expect(chip()).not.toHaveTextContent('9');
  });

  /**
   * A local filter matching nothing is a DIFFERENT situation from an empty
   * namespace, and AsyncPanel — which owns the page's empty copy — never sees
   * the term, so it cannot phrase this one. The message has to say the filter
   * was local, or an operator reads "no matches" as "no such rows exist".
   */
  it('explains that a no-match filter searched only the loaded rows', () => {
    table(rows(3), {
      search: { mode: 'client', fields: (r) => [r.name] },
    });

    fireEvent.change(screen.getByLabelText('filter the loaded things'), {
      target: { value: 'nothing-matches-this' },
    });

    expect(screen.getByRole('status')).toHaveTextContent(/already loaded, not the whole namespace/i);
    expect(document.querySelector('tbody')).toBeNull();
  });
});

describe('page size', () => {
  it('changes how many rows are visible', () => {
    table(rows(120));
    expect(bodyNames()).toHaveLength(25);

    fireEvent.change(screen.getByLabelText('things per page'), { target: { value: '50' } });
    expect(bodyNames()).toHaveLength(50);

    fireEvent.change(screen.getByLabelText('things per page'), { target: { value: '100' } });
    expect(bodyNames()).toHaveLength(100);
  });

  /**
   * Row 30 is on page 2 at size 25 and page 1 at size 50, so the index cannot be
   * carried across the change — the operator would land somewhere they did not
   * ask for. Page 1 is the only position meaningful under both.
   */
  it('returns to the first page when the size changes', () => {
    table(rows(120));
    fireEvent.click(screen.getByRole('button', { name: 'next page of things' }));
    expect(bodyNames()[0]).toBe('row-026');

    fireEvent.change(screen.getByLabelText('things per page'), { target: { value: '50' } });
    expect(bodyNames()[0]).toBe('row-001');
  });
});

describe('pagination', () => {
  it('moves through pages', () => {
    table(rows(60));
    expect(bodyNames()[0]).toBe('row-001');

    fireEvent.click(screen.getByRole('button', { name: 'next page of things' }));
    expect(bodyNames()[0]).toBe('row-026');

    fireEvent.click(screen.getByRole('button', { name: 'previous page of things' }));
    expect(bodyNames()[0]).toBe('row-001');
  });

  it('cannot go below the first page', () => {
    table(rows(60));
    const prev = screen.getByRole('button', { name: 'previous page of things' });
    expect(prev).toBeDisabled();
    // Clicking a disabled button is a no-op in the DOM, but the handler must
    // agree with the attribute — a bound expressed twice is a bound that drifts.
    fireEvent.click(prev);
    expect(bodyNames()[0]).toBe('row-001');
  });

  it('cannot go past the last page', () => {
    // 30 rows at 25 per page is two pages, the second holding five rows.
    table(rows(30));
    const next = screen.getByRole('button', { name: 'next page of things' });
    fireEvent.click(next);

    expect(bodyNames()).toHaveLength(5);
    expect(next).toBeDisabled();
    fireEvent.click(next);
    expect(bodyNames()).toHaveLength(5);
  });

  /**
   * The index goes out of range without anyone clicking anything: a filter that
   * narrows 60 rows to 3 while the operator is on page 2. Clamping at the point
   * of USE is what turns that into "the nearest real page" instead of a blank
   * table with working controls.
   */
  it('clamps to a real page when a filter shortens the list underneath it', () => {
    table(rows(60), { search: { mode: 'client', fields: (r) => [r.name] } });
    fireEvent.click(screen.getByRole('button', { name: 'next page of things' }));
    expect(bodyNames()[0]).toBe('row-026');

    fireEvent.change(screen.getByLabelText('filter the loaded things'), {
      target: { value: 'row-007' },
    });

    expect(bodyNames()).toEqual(['row-007']);
    expect(screen.getByText('page 1 of 1')).toBeInTheDocument();
  });

  it('renders no pager at all when paging is off', () => {
    table(rows(60), { paging: { mode: 'none' } });
    expect(screen.queryByRole('button', { name: /next page/i })).not.toBeInTheDocument();
    // And every row is drawn, which is the point of turning it off.
    expect(bodyNames()).toHaveLength(60);
  });

  /**
   * Server paging is keyset: a cursor addresses "the next page", never "page 7".
   * So the bounds come from whether the CALLER supplied a handler, and the page
   * label carries no denominator — there is no total to divide by.
   */
  it('takes its bounds from the callbacks in server mode and states no total', () => {
    let nexts = 0;
    render(
      <DataTable
        rows={rows(3)}
        columns={columns}
        rowKey={(r) => r.id}
        noun="things"
        paging={{ mode: 'server', pageNumber: 2, onNext: () => nexts++ }}
      />,
    );

    // No onPrev was given, so there is no previous page to offer.
    expect(screen.getByRole('button', { name: 'previous page of things' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'next page of things' }));
    expect(nexts).toBe(1);

    expect(screen.getByText('page 2')).toBeInTheDocument();
    // The claim that must never appear without a total behind it.
    expect(screen.queryByText(/page 2 of/)).not.toBeInTheDocument();
  });
});

describe('sorting', () => {
  it('toggles ascending, descending, then back to the given order', () => {
    table(rows(3));
    const header = screen.getByRole('button', { name: /name/ });

    // Unsorted: the order the caller handed over, which for a real page is the
    // server's considered ordering and must be returnable to.
    expect(bodyNames()).toEqual(['row-001', 'row-002', 'row-003']);

    fireEvent.click(header);
    expect(bodyNames()).toEqual(['row-001', 'row-002', 'row-003']);

    fireEvent.click(header);
    expect(bodyNames()).toEqual(['row-003', 'row-002', 'row-001']);

    fireEvent.click(header);
    expect(bodyNames()).toEqual(['row-001', 'row-002', 'row-003']);
  });

  it('sets aria-sort on the header cell, and only on the sorted one', () => {
    table(rows(3));
    const headers = screen.getAllByRole('columnheader');
    const [name, size, note] = headers;

    // Sortable but unsorted columns say "none" rather than omitting the
    // attribute: present-but-none is what tells a screen reader the column CAN
    // be sorted, which is the affordance the glyph gives a sighted user.
    expect(name).toHaveAttribute('aria-sort', 'none');
    expect(size).toHaveAttribute('aria-sort', 'none');
    // The column with no comparator is not sortable and must claim nothing.
    expect(note).not.toHaveAttribute('aria-sort');

    fireEvent.click(within(name!).getByRole('button'));
    expect(name).toHaveAttribute('aria-sort', 'ascending');
    expect(size).toHaveAttribute('aria-sort', 'none');

    fireEvent.click(within(name!).getByRole('button'));
    expect(name).toHaveAttribute('aria-sort', 'descending');
  });

  it('moves the sort to another column rather than combining them', () => {
    table(rows(3));
    fireEvent.click(screen.getByRole('button', { name: /name/ }));
    fireEvent.click(screen.getByRole('button', { name: /size/ }));

    const [name, size] = screen.getAllByRole('columnheader');
    expect(name).toHaveAttribute('aria-sort', 'none');
    expect(size).toHaveAttribute('aria-sort', 'ascending');
    // size counts down as name counts up, so an ascending size sort reverses.
    expect(bodyNames()).toEqual(['row-003', 'row-002', 'row-001']);
  });

  it('offers no sort control on a column that gave no comparator', () => {
    table(rows(2));
    const note = screen.getAllByRole('columnheader')[2]!;
    expect(within(note).queryByRole('button')).toBeNull();
    expect(note).toHaveTextContent('note');
  });

  it('returns to the first page when the sort changes', () => {
    table(rows(60));
    fireEvent.click(screen.getByRole('button', { name: 'next page of things' }));
    expect(bodyNames()[0]).toBe('row-026');

    fireEvent.click(screen.getByRole('button', { name: /name/ }));
    expect(bodyNames()[0]).toBe('row-001');
  });

  it('does not mutate the caller’s array', () => {
    const data = rows(3);
    const original = [...data];
    table(data);
    fireEvent.click(screen.getByRole('button', { name: /name/ }));
    fireEvent.click(screen.getByRole('button', { name: /name/ }));
    // Sorting in place would reorder the array React Query is caching, which
    // shows up as a list that reshuffles itself on an unrelated refetch.
    expect(data).toEqual(original);
  });
});

/**
 * The heart of the component. Each of these is a sentence the table is or is not
 * entitled to say about the rows it holds.
 */
describe('honesty about what the row count means', () => {
  it('marks a capped fetch that came back full as capped, not as a total', () => {
    table(rows(100), { count: { kind: 'capped', limit: 100 } });

    // The "+" is the same glyph the attention panel uses for `truncated`, so
    // the two places in this console that mean "there may be more" agree.
    expect(chip()).toHaveTextContent('100+');
    expect(chip()).toHaveAttribute('title', expect.stringMatching(/At least 100 things/));
    expect(screen.getByText(/1–25 of 100\+/)).toBeInTheDocument();
  });

  /**
   * The other half, and the one that is easier to get wrong in the cautious
   * direction: a capped request that came back SHORT of its cap has an exact
   * count. Every namespace this console has been pointed at is under the cap, so
   * a permanent "+" would be a permanent lie in the common case.
   */
  it('does not mark a capped fetch as capped when the cap never bound', () => {
    table(rows(7), { count: { kind: 'capped', limit: 100 } });

    expect(chip()).toHaveTextContent('7');
    expect(chip()).not.toHaveTextContent('7+');
    expect(chip()).toHaveAttribute(
      'title',
      expect.stringMatching(/Fewer than the 100-row cap, so this is all of them/),
    );
    expect(screen.getByText('1–7 of 7')).toBeInTheDocument();
  });

  it('states an exact count plainly when the endpoint sends everything', () => {
    table(rows(12), { count: { kind: 'exact' } });
    expect(chip()).toHaveTextContent('12');
    expect(chip()).toHaveAttribute('title', '12 things');
    expect(screen.getByText('1–12 of 12')).toBeInTheDocument();
  });

  /**
   * The single most important assertion in the file. A client-side filter over a
   * CAPPED set knows only about the rows it holds, so a count of the matches is
   * a statement about those rows dressed up as a statement about the namespace.
   * "3 results" out of a capped 100 of a possible 400 is a lie, and the only
   * honest denominator is none.
   */
  it('forfeits the count when a local filter narrows a capped set', () => {
    table(rows(100), {
      count: { kind: 'capped', limit: 100 },
      search: { mode: 'client', fields: (r) => [r.name] },
    });
    expect(chip()).toHaveTextContent('100+');

    fireEvent.change(screen.getByLabelText('filter the loaded things'), {
      target: { value: 'row-01' },
    });

    // Ten matches, and the table refuses to present them as ten of anything.
    expect(chip()).toHaveTextContent('10+');
    expect(chip()).toHaveAttribute(
      'title',
      expect.stringMatching(/does not report a total, so this is not a count of everything/),
    );
    expect(screen.getByText('1–10 of these 10')).toBeInTheDocument();
  });

  it('defaults to the claim that over-states nothing', () => {
    // No `count` passed at all. The default must be the cautious one, because a
    // caller who has not thought about it yet gets a table that under-claims
    // rather than one that confidently misleads.
    table(rows(4));
    expect(screen.getByText('1–4 of these 4')).toBeInTheDocument();
    expect(screen.queryByText('1–4 of 4')).not.toBeInTheDocument();
  });

  it('never prints a page denominator it cannot support', () => {
    table(rows(30), { count: { kind: 'unknown' } });
    // Client paging genuinely knows how many pages the rows in hand make, so
    // "of 2" here is a fact about those rows and is allowed. What is NOT allowed
    // is a range that divides by a namespace total nobody sent.
    expect(screen.getByText('page 1 of 2')).toBeInTheDocument();
    expect(screen.getByText('1–25 of these 30')).toBeInTheDocument();
  });

  it('says "no rows" rather than an empty range', () => {
    table([], { count: { kind: 'exact' } });
    expect(screen.getByText('no rows')).toBeInTheDocument();
  });
});

describe('table semantics', () => {
  it('gives every header a column scope', () => {
    table(rows(2));
    for (const th of screen.getAllByRole('columnheader')) {
      expect(th).toHaveAttribute('scope', 'col');
    }
  });

  it('labels the search input and names every pagination control', () => {
    table(rows(60), { search: { mode: 'client', fields: (r) => [r.name] } });

    // Each of these would be an unlabelled control without the wiring, and an
    // unlabelled control is unusable by anyone not looking at the screen.
    expect(screen.getByLabelText('filter the loaded things')).toBeInTheDocument();
    expect(screen.getByLabelText('things per page')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'next page of things' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'previous page of things' })).toBeInTheDocument();
  });

  it('makes a sortable header a real button, reachable by keyboard', () => {
    table(rows(3));
    const button = screen.getByRole('button', { name: /name/ });
    expect(button.tagName).toBe('BUTTON');
    // A <button> activates on Enter and Space for free; a div with onClick — the
    // thing this replaces — does neither and announces nothing.
    expect(button).toHaveAttribute('type', 'button');
  });

  it('applies the per-row class a caller supplies', () => {
    table(rows(2), { rowClass: (r) => (r.name === 'row-001' ? 'row-failed' : undefined) });
    const [first, second] = Array.from(document.querySelectorAll('tbody tr'));
    expect(first).toHaveClass('row-failed');
    expect(second!.className).toBe('');
  });

  /**
   * The scope note explains WHERE the search ran, which is the one thing about
   * a result an operator cannot see. It was previously a sibling of the field,
   * separated from it by the spacer that pushes the count chip to the far edge —
   * so on a band narrow enough to wrap it could be laid out under an unrelated
   * control. Grouping them is a presentational fix, but the thing worth pinning
   * is the RELATIONSHIP it makes visible, which must not be re-separated:
   * aria-describedby names the note from the input, and the note must be inside
   * the same group as the input it describes.
   */
  it('keeps the search field and the note describing its scope in one group', () => {
    table(rows(2), { search: { mode: 'client', fields: (r) => [r.name] } });

    const input = screen.getByLabelText('filter the loaded things');
    const described = input.getAttribute('aria-describedby');
    expect(described).toBeTruthy();

    const note = document.getElementById(described!);
    expect(note).toHaveTextContent(/filters the loaded rows only/i);

    // The wiring and the layout agree: the element aria-describedby points at
    // is a descendant of the same group as the field, not a loose sibling of it.
    const group = input.closest('.dt-search-group');
    expect(group).not.toBeNull();
    expect(group!.contains(note!)).toBe(true);
  });

  /**
   * A table with no search has no group to render, and must not render an empty
   * one: a stray flex child in the toolbar collapses to a zero-width box that
   * still eats a gap, which is how a control strip acquires a mystery hole.
   */
  it('renders no search group at all when the table has no search', () => {
    table(rows(2));
    expect(document.querySelector('.dt-search-group')).toBeNull();
    // The count is still disclosed, which is why the toolbar is unconditional.
    expect(chip()).toHaveTextContent('2');
  });
});

/**
 * Row selection.
 *
 * What is pinned here is, again, the HONESTY plus the two traps that fail
 * SILENTLY — the ones that produce a wrong UI rather than a broken one:
 *
 *   - select-all covers the rows the client actually HAS, and the control's
 *     accessible name says which rows those are. A capped or paged fetch says
 *     "loaded"; only an uncapped one may say the bare noun.
 *   - `indeterminate` is a DOM property with no HTML attribute behind it, so a
 *     JSX prop would be dropped in silence and the header would render
 *     plain-unchecked while rows were selected — claiming "nothing is selected"
 *     next to a bulk bar saying otherwise.
 *   - an ineligible row gets no checkbox, so a selection can never over-promise
 *     what the action will achieve.
 *   - a checkbox inside a row that is also a link must not navigate.
 */
describe('selection', () => {
  /** Renders a selectable table whose selection is held the way a page holds it. */
  function Selectable({
    rowData,
    isSelectable,
    count,
    search,
    onSelect,
  }: {
    rowData: Row[];
    isSelectable?: (r: Row) => boolean;
    count?: Parameters<typeof DataTable<Row>>[0]['count'];
    search?: Parameters<typeof DataTable<Row>>[0]['search'];
    onSelect?: (next: ReadonlySet<string>) => void;
  }) {
    const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
    return (
      <DataTable
        rows={rowData}
        columns={columns}
        rowKey={(r) => r.id}
        noun="things"
        count={count}
        search={search}
        selection={{
          selected,
          onChange: (next) => {
            onSelect?.(next);
            setSelected(next);
          },
          rowId: (r) => r.id,
          isSelectable,
          label: (r) => r.name,
        }}
      />
    );
  }

  function selectAll(): HTMLInputElement {
    return document.querySelector('.dt-select-all')!;
  }

  it('renders no selection column at all when no selection prop is given', () => {
    table(rows(3));
    expect(document.querySelectorAll('input[type=checkbox]')).toHaveLength(0);
    // Every existing caller keeps its own column count, untouched.
    expect(document.querySelectorAll('thead th')).toHaveLength(columns.length);
  });

  it('prepends one checkbox column rather than making the caller declare it', () => {
    render(<Selectable rowData={rows(3)} />);
    expect(document.querySelectorAll('thead th')).toHaveLength(columns.length + 1);
    // One per row, plus the select-all in the header.
    expect(screen.getAllByRole('checkbox')).toHaveLength(4);
  });

  it('names every row checkbox after the row, not "select row"', () => {
    render(<Selectable rowData={rows(2)} />);
    expect(screen.getByRole('checkbox', { name: 'Select row-001' })).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Select row-002' })).toBeInTheDocument();
  });

  /**
   * THE HONESTY RULE, in the place an operator actually meets it.
   *
   * A capped fetch means the client holds a page, so the control must say
   * "loaded" — it cannot select rows it never received, and a bare "Select all"
   * would be claiming authority over them.
   */
  it('says "loaded" in select-all when the fetch was capped', () => {
    render(<Selectable rowData={rows(5)} count={{ kind: 'capped', limit: 5 }} />);
    expect(
      screen.getByRole('checkbox', { name: 'Select all 5 loaded things' }),
    ).toBeInTheDocument();
  });

  it('says "loaded" in select-all when no total is knowable', () => {
    render(<Selectable rowData={rows(4)} count={{ kind: 'unknown' }} />);
    expect(
      screen.getByRole('checkbox', { name: 'Select all 4 loaded things' }),
    ).toBeInTheDocument();
  });

  /** Only an uncapped endpoint may drop the qualifier, because only it is telling the truth. */
  it('drops the qualifier only when the endpoint returned everything', () => {
    render(<Selectable rowData={rows(3)} count={{ kind: 'exact' }} />);
    expect(screen.getByRole('checkbox', { name: 'Select all 3 things' })).toBeInTheDocument();
  });

  it('selects only the rows the client holds, never a page beyond them', () => {
    const seen: ReadonlySet<string>[] = [];
    render(
      <Selectable
        rowData={rows(60)}
        count={{ kind: 'capped', limit: 60 }}
        onSelect={(n) => seen.push(n)}
      />,
    );

    // 60 rows loaded, 25 on the page. Select-all takes all 60 — the filtered
    // set in hand, not the visible slice — and emphatically not more.
    fireEvent.click(selectAll());
    expect(seen[0]!.size).toBe(60);
  });

  it('offers no control that reaches beyond the loaded rows', () => {
    render(<Selectable rowData={rows(60)} count={{ kind: 'capped', limit: 60 }} />);
    // There is deliberately no "select all N across all pages" affordance: the
    // client cannot select what it never received.
    expect(screen.queryByText(/across all pages/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/all \d+ matching in the namespace/i)).not.toBeInTheDocument();
  });

  /**
   * THE INDETERMINATE TRAP. Setting it as a JSX prop does nothing; this asserts
   * the property on the live node, which is the only place the state exists.
   */
  it('renders the header checkbox indeterminate when only some rows are selected', () => {
    render(<Selectable rowData={rows(3)} />);

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select row-002' }));

    const all = selectAll();
    expect(all.indeterminate).toBe(true);
    // Unchecked AND indeterminate: "some", which is a different claim from
    // "none" and must not be collapsed into it.
    expect(all.checked).toBe(false);
  });

  it('is checked and not indeterminate once every selectable row is selected', () => {
    render(<Selectable rowData={rows(2)} />);
    fireEvent.click(selectAll());

    const all = selectAll();
    expect(all.checked).toBe(true);
    expect(all.indeterminate).toBe(false);
  });

  it('is neither checked nor indeterminate when nothing is selected', () => {
    render(<Selectable rowData={rows(2)} />);
    const all = selectAll();
    expect(all.checked).toBe(false);
    expect(all.indeterminate).toBe(false);
  });

  /**
   * An ineligible row gets NO checkbox — not a disabled one. Selecting it would
   * let the count over-promise what the action can achieve, and then report a
   * failure for a row that was never a candidate.
   */
  it('excludes ineligible rows from selection entirely', () => {
    render(<Selectable rowData={rows(3)} isSelectable={(r) => r.name !== 'row-002'} />);

    expect(screen.queryByRole('checkbox', { name: 'Select row-002' })).not.toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Select row-001' })).toBeInTheDocument();
    expect(screen.getByRole('checkbox', { name: 'Select row-003' })).toBeInTheDocument();
  });

  it('counts only the eligible rows in the select-all claim', () => {
    const seen: ReadonlySet<string>[] = [];
    render(
      <Selectable
        rowData={rows(4)}
        count={{ kind: 'exact' }}
        isSelectable={(r) => r.name !== 'row-002'}
        onSelect={(n) => seen.push(n)}
      />,
    );

    // Three of four rows are eligible, and the label says three.
    const all = screen.getByRole('checkbox', { name: 'Select all 3 things' });
    fireEvent.click(all);
    expect(seen[0]!.size).toBe(3);
    expect(seen[0]!.has('r1')).toBe(false);
  });

  it('disables select-all when no row can be selected', () => {
    render(<Selectable rowData={rows(2)} isSelectable={() => false} />);
    expect(selectAll()).toBeDisabled();
  });

  /**
   * SELECTION DOES NOT SURVIVE A FILTER CHANGE.
   *
   * Carrying ids across a filter means the bar can say "3 selected" over a table
   * that does not contain those three — and the operator then presses a
   * destructive button against rows they cannot see. Re-selecting costs clicks;
   * deleting unseen rows costs a restore.
   */
  it('clears the selection when the filter changes', () => {
    render(
      <Selectable
        rowData={rows(5)}
        search={{ mode: 'client', fields: (r) => [r.name] }}
      />,
    );

    fireEvent.click(screen.getByRole('checkbox', { name: 'Select row-001' }));
    expect((screen.getByRole('checkbox', { name: 'Select row-001' }) as HTMLInputElement).checked)
      .toBe(true);

    fireEvent.change(screen.getByLabelText('filter the loaded things'), {
      target: { value: 'row-00' },
    });

    // Every box is clear again: the selection belonged to the previous view.
    const boxes = screen
      .getAllByRole('checkbox')
      .filter((b) => b !== selectAll()) as HTMLInputElement[];
    expect(boxes.every((b) => !b.checked)).toBe(true);
  });

  /**
   * A SORT is not a filter. It shows the same rows in a different order, so
   * every selected row is still on screen and still means what it meant.
   */
  it('keeps the selection across a sort, which reorders rather than replaces', () => {
    render(<Selectable rowData={rows(3)} />);
    fireEvent.click(screen.getByRole('checkbox', { name: 'Select row-001' }));

    fireEvent.click(screen.getByRole('button', { name: /name/ }));

    expect(
      (screen.getByRole('checkbox', { name: 'Select row-001' }) as HTMLInputElement).checked,
    ).toBe(true);
  });

  /**
   * A checkbox inside a row that is ALSO a link is a known trap: the click
   * bubbles to the row and the operator is navigated away from the table they
   * were selecting in.
   */
  it('does not trigger the row’s link when the checkbox is clicked', () => {
    let navigations = 0;
    const linked: Column<Row>[] = [
      {
        key: 'name',
        header: 'name',
        cell: (r) => (
          <a href={`/things/${r.id}`} onClick={() => (navigations += 1)}>
            {r.name}
          </a>
        ),
      },
    ];

    function Linked() {
      const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
      return (
        <DataTable
          rows={rows(2)}
          columns={linked}
          rowKey={(r) => r.id}
          noun="things"
          selection={{
            selected,
            onChange: setSelected,
            rowId: (r) => r.id,
            label: (r) => r.name,
          }}
        />
      );
    }
    render(<Linked />);

    const box = screen.getByRole('checkbox', { name: 'Select row-001' }) as HTMLInputElement;
    fireEvent.click(box);

    // The row was selected and nothing navigated.
    expect(box.checked).toBe(true);
    expect(navigations).toBe(0);
  });

  /** Native checkboxes, so Tab and Space work with no extra code. */
  it('uses real checkbox inputs rather than styled divs', () => {
    render(<Selectable rowData={rows(2)} />);
    for (const box of screen.getAllByRole('checkbox')) {
      expect(box.tagName).toBe('INPUT');
      expect(box).toHaveAttribute('type', 'checkbox');
    }
  });
});
