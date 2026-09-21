import { useContext } from 'react';
import { PermissionContext, permissionForPath } from './Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The command palette: one search field that is both the console's navigation
 * and its GLOBAL lookup across every entity the console knows about.
 *
 * WHY IT IS NOT A POWER-USER EASTER EGG. The reference design puts a search
 * field at the top of the console, so this is a primary entry point: the row an
 * operator uses during an incident when they have a run id in the clipboard and
 * no interest in finding out which page lists it. That is why the search half
 * talks to the REAL endpoints rather than filtering a list the palette happens
 * to have, and why a failed search says so instead of rendering an empty list
 * -- "no matches" and "I could not ask" are opposite facts, and confusing them
 * during an incident sends an operator looking for a workflow they were just
 * told does not exist.
 *
 * WHY IT SEARCHES FIVE THINGS AND USED TO SEARCH ONE. The previous version
 * asked the workflow catalog and nothing else, while the field promised to
 * "search runs, workflows and commands". An operator who typed a colleague's
 * email or a schedule name got an empty panel and drew the only available
 * conclusion: the console cannot find them. A search that silently covers a
 * fifth of the console is worse than an obviously narrow one, because the empty
 * result is indistinguishable from an authoritative "no such thing".
 *
 * WHAT THE FAN-OUT GUARANTEES, and why each guarantee is here rather than in
 * lib/commands.ts:
 *
 *  - CONCURRENCY is in lib/commands.ts (`searchEverything`), because who is
 *    asked and what a per-group failure MEANS is backend policy, testable with
 *    no DOM.
 *  - THE OUT-OF-ORDER GUARD is here, because it is a property of this
 *    component's state: typing is faster than the network, and a fan-out makes
 *    that worse rather than better -- five in-flight requests per keystroke,
 *    any of which can land after the operator has moved on. One `live` flag per
 *    effect run covers all five at once, since they are awaited together.
 *  - THE DEBOUNCE and the minimum-length gate are here too, and matter five
 *    times as much as they did: one keystroke is now five requests, so an
 *    ungated field would put five requests per character on the gateway.
 *
 * Dismissal and focus handling follow ProfileMenu.tsx: Escape closes and
 * returns focus to whatever had it, because a keyboard user dropped at the top
 * of the document has lost their place. A palette goes further than the menu
 * does and TRAPS focus while open, since it covers the page -- Tab reaching a
 * control the operator cannot see is worse than no Tab at all.
 */

import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { useDebounced } from '../lib/useDebounced';
import { searchSources } from '../lib/searchSources';
import {
  CLIENT_FILTER_CAP,
  COMMANDS,
  SEARCH_GROUPS,
  asRunId,
  filterCommands,
  runPath,
  searchEverything,
  shouldSearchRemote,
  type Command,
  type CommandContext,
  type SearchResults,
  type SearchSources,
} from '../lib/commands';

export interface CommandPaletteProps {
  /** Namespace to search in. Every catalog endpoint is namespace-scoped. */
  namespace: string;
  /** Router navigation, owned by the shell. */
  onNavigate: (to: string) => void;
  /** Clears the credential, owned by the shell's session state. */
  onSignOut: () => void;
  /**
   * Commands, injectable only so tests can exercise the list mechanics against
   * a small fixture. Production always gets the registry.
   */
  commands?: Command[];
  /** Open state, when the shell wants a button in the header to open it too. */
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  /**
   * The five endpoints the search fans out to, injectable only so tests can
   * make one source hang or refuse without a fetch mock that has to guess which
   * URL belongs to which group. Production always gets the real wiring.
   */
  sources?: SearchSources;
}

/** One row in the list, whatever produced it. */
interface Row {
  id: string;
  label: string;
  hint?: string;
  group: string;
  run: () => void;
}

/**
 * One heading and the rows under it -- plus, if there is something to say about
 * the group that is not a row, a notice.
 *
 * A notice is NOT a row: it is not selectable, Enter never runs it, and it does
 * not enter the option index. That is the point. "I could not search users" is
 * information about the search, not a destination, and making it an option
 * would put a thing the operator cannot act on under a cursor whose whole
 * contract is that Enter does something.
 */
interface Section {
  heading: string;
  rows: Row[];
  notice?: { tone: 'muted' | 'danger'; text: string };
}

/**
 * The state of the whole fan-out, not of one request.
 *
 * `ok` carries per-group results rather than a flat row list, because the
 * groups that FAILED have to survive as far as the render -- a flat list would
 * have already thrown that information away, and a group that failed would be
 * indistinguishable from a group with no matches. That is the one behaviour
 * this control cannot get wrong.
 */
type SearchState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ok'; results: SearchResults };

/** Frozen and shared: see the note where this is read. */
const NO_PERMISSIONS: readonly string[] = Object.freeze([]);

export function CommandPalette({
  namespace,
  onNavigate,
  onSignOut,
  commands = COMMANDS,
  open: openProp,
  onOpenChange,
  sources = searchSources,
}: CommandPaletteProps) {
  // Uncontrolled by default: the shell should not have to own state for a thing
  // that opens on a global key. A controlled `open` is accepted so a header
  // button can drive the same instance.
  // NO_PERMISSIONS is a module-level constant, not a `?? []` fallback written
  // inline. An inline literal allocates a NEW array on every render, and
  // `permissions` is a dependency of the search effect below -- so with no
  // provider above this component, every render produced a new identity, the
  // effect reran, it called setSearch, and that rendered again: a synchronous
  // loop that pins a core and never yields. It cannot be interrupted by a test
  // timeout, because the event loop never gets a turn.
  const permissions = useContext(PermissionContext) ?? NO_PERMISSIONS;
  const allowed = (permission: string) => permissions.includes(permission);
  const [openState, setOpenState] = useState(false);
  const open = openProp ?? openState;

  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);
  const [search, setSearch] = useState<SearchState>({ status: 'idle' });

  const dialogRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  // Whatever had focus before we opened, so Escape can hand it back.
  const restoreRef = useRef<HTMLElement | null>(null);
  const listId = useId();
  const labelId = useId();

  const setOpen = useCallback(
    (next: boolean) => {
      setOpenState(next);
      onOpenChange?.(next);
    },
    [onOpenChange],
  );

  const close = useCallback(() => {
    setOpen(false);
    // Restore before React unmounts anything focusable inside, or the browser
    // moves focus to <body> and the restore target is never reached.
    const target = restoreRef.current;
    restoreRef.current = null;
    if (target && document.contains(target)) target.focus();
  }, [setOpen]);

  // ── the global shortcut ───────────────────────────────────────────────────
  //
  // Cmd+K / Ctrl+K only. It is deliberately the only key this component claims
  // globally: a bare-letter shortcut would fight with every text field in the
  // console, and Cmd+F must keep opening browser find -- a palette that steals
  // it has broken a browser feature to reimplement it worse. The modifier makes
  // Cmd+K unambiguous even while the operator is typing in an input, so it is
  // honoured there too rather than being suppressed.
  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      const isK = e.key === 'k' || e.key === 'K';
      if (!isK || !(e.metaKey || e.ctrlKey) || e.altKey || e.shiftKey) return;
      e.preventDefault();
      if (open) {
        close();
        return;
      }
      const activeEl = document.activeElement;
      restoreRef.current = activeEl instanceof HTMLElement ? activeEl : null;
      setQuery('');
      setActive(0);
      setSearch({ status: 'idle' });
      setOpen(true);
    }
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, [open, close, setOpen]);

  // Focus the field once it exists. Without this the dialog opens with focus
  // still on the page behind it, so the first keystroke goes somewhere else.
  useEffect(() => {
    if (open) inputRef.current?.focus();
  }, [open]);

  // ── the remote half ───────────────────────────────────────────────────────
  const debounced = useDebounced(query, 250);

  useEffect(() => {
    if (!open) return;
    if (!shouldSearchRemote(debounced)) {
      setSearch({ status: 'idle' });
      return;
    }
    // Guards a slow response for an older query from overwriting a newer one:
    // typing is faster than the network, and out-of-order results would show
    // matches for a prefix the operator has already moved past.
    //
    // ONE flag for all five requests, checked once after they have all settled.
    // That is sound because searchEverything awaits them together, so there is
    // exactly one state update per query and no window in which a stale group
    // could be merged into a fresh result. Per-group flags would have bought
    // nothing and opened that window.
    let live = true;
    setSearch({ status: 'loading' });
    searchEverything(namespace, debounced, {
      workflows: (...args) => allowed('workflows.view') ? sources.workflows(...args) : Promise.resolve([]),
      runs: (...args) => allowed('runs.view') ? sources.runs(...args) : Promise.resolve([]),
      schedules: (...args) => allowed('schedules.view') ? sources.schedules(...args) : Promise.resolve({hits:[],truncated:false}),
      workers: (...args) => allowed('workers.view') ? sources.workers(...args) : Promise.resolve({hits:[],truncated:false}),
      users: (...args) => allowed('users.view') ? sources.users(...args) : Promise.resolve({hits:[],truncated:false}),
    })
      .then((results) => {
        if (!live) return;
        setSearch({ status: 'ok', results });
      })
      .catch(() => {
        if (!live) return;
        // Reaching here means the fan-out ITSELF threw, which it is written not
        // to do -- every group failure is reported inside the results. It is
        // still handled rather than left to become an unhandled rejection: a
        // programming error in the search layer must not leave the panel
        // spinning "Searching…" forever, which looks like a slow network and
        // sends the operator to the wrong problem.
        setSearch({
          status: 'ok',
          results: Object.fromEntries(
            SEARCH_GROUPS.map((g) => [g, { status: 'failed', message: 'The search failed.' }]),
          ) as SearchResults,
        });
      });
    return () => {
      live = false;
    };
  }, [open, debounced, namespace, sources, permissions]);

  const ctx: CommandContext = useMemo(
    () => ({ navigate: onNavigate, signOut: onSignOut }),
    [onNavigate, onSignOut],
  );

  // ── the one list, in sections ─────────────────────────────────────────────
  //
  // Search results lead when there are any: a query that produced a run id or a
  // workflow name is a query about that thing, and burying it under the static
  // commands would make the operator arrow past navigation they did not ask for.
  //
  // WHY SECTIONS AND NOT A FLAT LIST WITH A CHANGING HEADING. The old list
  // printed a heading whenever the group name changed from the previous row,
  // which works for one search group and breaks for five: a group that FAILED
  // has no rows, so a change-detecting heading can never print it, and the
  // group that could not be searched would be exactly as invisible as a group
  // with no matches. Sections are built explicitly so an empty-but-broken group
  // still gets its heading and its notice.
  //
  // The flat `rows` array below is derived FROM the sections rather than built
  // alongside them, so the keyboard index and the rendered order cannot drift.
  const sections: Section[] = useMemo(() => {
    const out: Section[] = [];

    // A pasted run id, answered with no request at all. It keeps its own
    // section above the fetched groups because a paste is an unambiguous
    // instruction -- the operator has the id, they want the run -- and it must
    // never wait behind a network round trip to appear.
    const runId = asRunId(query);
    if (runId && allowed('runs.view')) {
      out.push({
        heading: 'Run',
        rows: [
          {
            id: `run.${runId}`,
            label: `Open run ${runId}`,
            hint: runPath(runId),
            group: 'Run',
            run: () => onNavigate(runPath(runId)),
          },
        ],
      });
    }

    if (search.status === 'ok') {
      for (const group of SEARCH_GROUPS) {
        if (!allowed(group.toLowerCase()+'.view')) continue;
        const result = search.results[group];
        if (!result) continue;

        if (result.status === 'forbidden') {
          // Not an error. The credential works and is being refused, which is a
          // fact about permission rather than about the console -- so it is
          // stated calmly, in its own group, and only where it applies.
          out.push({
            heading: group,
            rows: [],
            notice: { tone: 'muted', text: `Not searched — this credential may not read ${group.toLowerCase()}.` },
          });
          continue;
        }
        if (result.status === 'failed') {
          out.push({
            heading: group,
            rows: [],
            notice: { tone: 'danger', text: `Could not search ${group.toLowerCase()} — ${result.message}` },
          });
          continue;
        }
        // A group with no matches gets no heading and no note. That is not an
        // oversight: four silent groups is what makes the fifth one's rows
        // readable, and a "checked the first 200" line under every group an
        // operator did NOT ask about would drown the panel in caveats. The
        // truncation that matters -- a capped group finding nothing, where
        // "nothing matches" might be wrong -- is instead handled by withholding
        // that claim below, which is the only place it could mislead anyone.
        if (result.hits.length === 0) continue;

        out.push({
          heading: group,
          rows: result.hits.map((hit) => ({
            id: hit.id,
            label: hit.label,
            hint: hit.hint,
            group,
            run: () => onNavigate(hit.path),
          })),
          // Said only when it is true and only for the groups that can be
          // capped, because the alternative is implying a completeness this
          // search does not have: the users, schedules and workers endpoints
          // have no search parameter, so those groups matched a capped page in
          // the browser and a match past the cap was never visible.
          notice: result.truncated
            ? { tone: 'muted', text: `Matched within the first ${CLIENT_FILTER_CAP} ${group.toLowerCase()}.` }
            : undefined,
        });
      }
    }

    // Commands last, grouped as the registry groups them, which is what the
    // list has always done when nothing is typed.
    const matched = filterCommands(query, commands.filter(command => { const required = command.hint?.startsWith('/') ? permissionForPath(command.hint) : undefined; return !required || allowed(required); }));
    let currentGroup = '';
    for (const { command } of matched) {
      const row: Row = {
        id: command.id,
        label: command.title,
        hint: command.hint,
        group: command.group,
        run: () => command.run(ctx),
      };
      if (command.group !== currentGroup) {
        currentGroup = command.group;
        out.push({ heading: command.group, rows: [row] });
      } else {
        out[out.length - 1]!.rows.push(row);
      }
    }
    return out;
  }, [query, search, commands, ctx, onNavigate, permissions]);

  const rows: Row[] = useMemo(() => sections.flatMap((s) => s.rows), [sections]);

  /**
   * Whether the search is entitled to say "nothing matches".
   *
   * It is not, in two cases, and both are the same mistake in different
   * clothes: claiming a fact the search never established.
   *
   *  - A GROUP WAS REFUSED OR BROKE. A panel with no rows because the users
   *    endpoint 403'd has not established that no user matches; it has
   *    established that it could not look.
   *  - A CAPPED GROUP FOUND NOTHING. The users, schedules and workers endpoints
   *    have no search parameter, so a miss over a capped page is only a miss
   *    within that page. The group prints no note of its own (see above -- a
   *    caveat under every silent group is noise), so the honesty lands here
   *    instead: the panel stays quiet rather than asserting the thing does not
   *    exist.
   */
  const canClaimNoMatches = useMemo(() => {
    if (search.status !== 'ok') return true;
    return SEARCH_GROUPS.every((group) => {
      if (!allowed(group.toLowerCase()+'.view')) return true;
        const result = search.results[group];
      if (!result) return true;
      if (result.status !== 'ok') return false;
      return !(result.truncated && result.hits.length === 0);
    });
  }, [search]);

  // Clamp rather than reset: the list changes shape as results arrive, and
  // resetting to 0 on every arrival would yank the cursor out from under an
  // operator who has already arrowed down.
  useEffect(() => {
    setActive((i) => (rows.length === 0 ? 0 : Math.min(i, rows.length - 1)));
  }, [rows.length]);

  if (!open) return null;

  const activeRow = rows[active];
  const activeId = activeRow ? `${listId}-${activeRow.id}` : undefined;

  function runRow(row: Row) {
    // Closed BEFORE the effect runs, so a command that navigates does not leave
    // the palette floating over the page it just opened. Focus restoration is
    // skipped for an activation: the destination owns focus now, and yanking it
    // back to the old trigger would undo the navigation the operator asked for.
    setOpen(false);
    restoreRef.current = null;
    row.run();
  }

  function onDialogKeyDown(e: React.KeyboardEvent) {
    if (e.key === 'Escape') {
      e.preventDefault();
      close();
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      // Wraps, because a list this short is faster to reach from either end and
      // a cursor stuck at the bottom reads as a broken key.
      setActive((i) => (rows.length === 0 ? 0 : (i + 1) % rows.length));
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((i) => (rows.length === 0 ? 0 : (i - 1 + rows.length) % rows.length));
      return;
    }
    if (e.key === 'Home') {
      e.preventDefault();
      setActive(0);
      return;
    }
    if (e.key === 'End') {
      e.preventDefault();
      setActive(Math.max(0, rows.length - 1));
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      const row = rows[active];
      if (row) runRow(row);
      return;
    }
    if (e.key === 'Tab') {
      // Focus trap. Only the input and the option buttons are focusable inside,
      // and the list is driven by aria-activedescendant rather than real focus,
      // so the trap collapses to "keep focus in the field" -- which is also
      // where every keystroke should go while the palette is open.
      e.preventDefault();
      inputRef.current?.focus();
    }
  }

  // A running index across the sections, so the option a key moves to is the
  // option the eye is on. Incremented inside the render rather than precomputed
  // because the sections already carry the order and a parallel index array
  // would be a second source of truth for the same thing.
  let flatIndex = -1;

  return (
    <div
      className="cmdk-backdrop"
      // Dismiss on backdrop click, matching every other overlay in the console.
      // mousedown rather than click so a drag that starts inside the dialog and
      // ends outside it does not read as a dismissal.
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) close();
      }}
    >
      <div
        className="cmdk"
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={labelId}
        onKeyDown={onDialogKeyDown}
      >
        <label className="sr-only" id={labelId} htmlFor={`${listId}-input`}>
          Search workflows, runs, schedules, workers, users and commands
        </label>
        <div className="cmdk-field">
          <span className="cmdk-icon" aria-hidden="true">
            ⌕
          </span>
          <input
            id={`${listId}-input`}
            ref={inputRef}
            type="text"
            className="cmdk-input"
            // The honest list of what a single query string may BE, which is
            // why it is not "Search…" and why it is not "search anything". A
            // run id is resolved locally; a name is matched against workflows,
            // schedules and spec-backed runs; a host or queue reaches the
            // fleet; an email reaches the user list when the credential may
            // read it. Nothing here promises node ids, logs or docs, because
            // none of those are searched -- and over-promising in a placeholder
            // is the same failure as an empty list for a failed request: the
            // operator goes looking for something they were told existed.
            placeholder="Run id, name, host or email"
            role="combobox"
            aria-expanded="true"
            aria-controls={listId}
            aria-autocomplete="list"
            aria-activedescendant={activeId}
            autoComplete="off"
            spellCheck={false}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActive(0);
            }}
          />
          <kbd className="cmdk-kbd" aria-hidden="true">
            esc
          </kbd>
        </div>

        {/* There is no panel-wide error banner any more, and that is the point
            of this whole change. A fan-out has no single failure: one banner
            reading "search failed" over a panel that is showing perfectly good
            workflow matches would be false, and the failure that actually
            happened -- which group, and whether it was refused or broken -- is
            exactly the fact the operator needs. So every failure is stated
            inside the group it belongs to, next to the heading whose absence of
            rows it explains. `role="alert"` stays on those notices so a screen
            reader is told the search could not complete rather than left with a
            list that quietly lost a group. */}

        <ul className="cmdk-list" id={listId} role="listbox" aria-label="Results">
          {sections.map((section) => (
            <li key={`section.${section.heading}`} className="cmdk-row" role="presentation">
              {/* aria-hidden because the heading is decoration for the eye: the
                  option's own label already names the thing, and a listbox
                  child that is not an option confuses the count assistive
                  technology reports. */}
              <p className="cmdk-group" aria-hidden="true">
                {section.heading}
              </p>

              {section.notice && (
                <p
                  className={`cmdk-note${section.notice.tone === 'danger' ? ' is-danger' : ''}`}
                  // Announced for the two cases that are news -- a group that
                  // could not be searched at all. A truncation notice is not an
                  // alert: the search worked, it was simply capped, and
                  // interrupting a screen-reader user mid-typing for that would
                  // make the honest note more annoying than the dishonest
                  // silence it replaces.
                  role={section.rows.length === 0 ? 'alert' : undefined}
                >
                  {section.notice.text}
                </p>
              )}

              {section.rows.map((row) => {
                flatIndex += 1;
                const i = flatIndex;
                return (
                  <div
                    key={row.id}
                    id={`${listId}-${row.id}`}
                    role="option"
                    aria-selected={i === active}
                    className={`cmdk-option${i === active ? ' is-active' : ''}`}
                    // Pointer selection follows the mouse, so clicking never
                    // activates a different row than the one under the cursor.
                    onMouseMove={() => setActive(i)}
                    onMouseDown={(e) => e.preventDefault()}
                    onClick={() => runRow(row)}
                  >
                    <span className="cmdk-label">{row.label}</span>
                    {row.hint && <span className="cmdk-hint">{row.hint}</span>}
                  </div>
                );
              })}
            </li>
          ))}

          {/* "Nothing matches" is an ASSERTION, so it is withheld unless every
              group was in a position to make it -- see canClaimNoMatches.
              Claiming nothing matches when three of five sources never replied,
              or when a capped page was all anyone looked at, is the failure
              this whole control is built to avoid. */}
          {rows.length === 0 && canClaimNoMatches && (
            <li className="cmdk-row" role="presentation">
              <p className="cmdk-empty">
                {search.status === 'loading' ? 'Searching…' : `Nothing matches “${query}”.`}
              </p>
            </li>
          )}
        </ul>

        <p className="cmdk-foot">
          <span>
            <kbd className="cmdk-kbd">↑</kbd>
            <kbd className="cmdk-kbd">↓</kbd> to move
          </span>
          <span>
            <kbd className="cmdk-kbd">↵</kbd> to run
          </span>
          <span>
            <kbd className="cmdk-kbd">esc</kbd> to close
          </span>
        </p>
      </div>
    </div>
  );
}
