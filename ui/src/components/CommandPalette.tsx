// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The command palette: one search field that is both the console's navigation
 * and its lookup for "Run id, spec or node".
 *
 * WHY IT IS NOT A POWER-USER EASTER EGG. The reference design puts a search
 * field at the top of the console, so this is a primary entry point: the row an
 * operator uses during an incident when they have a run id in the clipboard and
 * no interest in finding out which page lists it. That is why the search half
 * talks to the REAL catalog endpoint rather than filtering a list the palette
 * happens to have, and why a failed search says so instead of rendering an
 * empty list -- "no matches" and "I could not ask" are opposite facts, and
 * confusing them during an incident sends an operator looking for a workflow
 * they were just told does not exist.
 *
 * Dismissal and focus handling follow ProfileMenu.tsx: Escape closes and
 * returns focus to whatever had it, because a keyboard user dropped at the top
 * of the document has lost their place. A palette goes further than the menu
 * does and TRAPS focus while open, since it covers the page -- Tab reaching a
 * control the operator cannot see is worse than no Tab at all.
 */

import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { api } from '../api/client';
import { useDebounced } from '../lib/useDebounced';
import {
  COMMANDS,
  asRunId,
  filterCommands,
  runPath,
  shouldSearchRemote,
  workflowPath,
  type Command,
  type CommandContext,
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
}

/** One row in the list, whatever produced it. */
interface Row {
  id: string;
  label: string;
  hint?: string;
  group: string;
  run: () => void;
}

type SearchState =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'ok'; rows: Row[] }
  | { status: 'error'; message: string };

export function CommandPalette({
  namespace,
  onNavigate,
  onSignOut,
  commands = COMMANDS,
  open: openProp,
  onOpenChange,
}: CommandPaletteProps) {
  // Uncontrolled by default: the shell should not have to own state for a thing
  // that opens on a global key. A controlled `open` is accepted so a header
  // button can drive the same instance.
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
    let live = true;
    setSearch({ status: 'loading' });
    api
      .listWorkflows(namespace, debounced, 8)
      .then((rows) => {
        if (!live) return;
        setSearch({
          status: 'ok',
          rows: rows.slice(0, 8).map((w) => ({
            id: `workflow.${w.id}`,
            label: w.name,
            hint: `Workflow · v${w.latest_version}`,
            group: 'Search',
            run: () => onNavigate(workflowPath(w.name)),
          })),
        });
      })
      .catch((err: unknown) => {
        if (!live) return;
        // The API client already produces operator-readable text, including the
        // "not implemented yet" case, so it is surfaced verbatim rather than
        // replaced with a generic apology that hides which thing broke.
        setSearch({
          status: 'error',
          message: err instanceof Error ? err.message : 'Workflow search failed.',
        });
      });
    return () => {
      live = false;
    };
  }, [open, debounced, namespace, onNavigate]);

  const ctx: CommandContext = useMemo(
    () => ({ navigate: onNavigate, signOut: onSignOut }),
    [onNavigate, onSignOut],
  );

  // ── the one list ──────────────────────────────────────────────────────────
  //
  // Search results lead when there are any: a query that produced a run id or a
  // workflow name is a query about that thing, and burying it under the static
  // commands would make the operator arrow past navigation they did not ask for.
  const rows: Row[] = useMemo(() => {
    const out: Row[] = [];

    const runId = asRunId(query);
    if (runId) {
      out.push({
        id: `run.${runId}`,
        label: `Open run ${runId}`,
        hint: runPath(runId),
        group: 'Search',
        run: () => onNavigate(runPath(runId)),
      });
    }

    if (search.status === 'ok') out.push(...search.rows);

    for (const { command } of filterCommands(query, commands)) {
      out.push({
        id: command.id,
        label: command.title,
        hint: command.hint,
        group: command.group,
        run: () => command.run(ctx),
      });
    }
    return out;
  }, [query, search, commands, ctx, onNavigate]);

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

  let groupSoFar = '';

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
          Search runs, workflows and commands
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
            // The reference screenshot's wording. It is also the honest list of
            // what the field accepts, which is why it is not "Search…".
            placeholder="Run id, spec or node"
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

        {search.status === 'error' && (
          // Stated as a failure, never as an empty result. The list below still
          // renders the static commands, which are still usable and still true.
          <p className="cmdk-error" role="alert">
            Workflow search failed — {search.message}
          </p>
        )}

        <ul className="cmdk-list" id={listId} role="listbox" aria-label="Results">
          {rows.map((row, i) => {
            const header = row.group !== groupSoFar ? row.group : null;
            groupSoFar = row.group;
            return (
              <li key={row.id} className="cmdk-row">
                {header && (
                  <p className="cmdk-group" aria-hidden="true">
                    {header}
                  </p>
                )}
                <div
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
              </li>
            );
          })}

          {rows.length === 0 && search.status !== 'error' && (
            <li className="cmdk-row">
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
