// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Picks the workflow a schedule will create runs of.
 *
 * WHY THIS EXISTS AT ALL. The field it replaces was a bare text input, so
 * creating a schedule meant typing a spec name exactly, from memory, with the
 * catalog one navigation away. When this control was written that was worse
 * than an annoyance: the gateway accepted ANY spec_name, so a typo returned 201
 * and armed a row that fired forever and resolved nothing — a schedule that
 * looked perfectly healthy on the list page and produced only failures.
 *
 * THE GATEWAY NOW CHECKS THE NAME TOO, and this control is no longer the only
 * thing standing between a typo and that row: `POST /schedules` rejects an
 * unknown spec_name with a 400 naming the field and the value, exactly as it
 * already did for a bad cron. That does NOT make the picker redundant, for the
 * ordinary reason a client-side control is not redundant — it turns a round
 * trip and an error message into a choice from a list, and the operator learns
 * what exists rather than what does not. It does change what the free-text
 * fallback below costs: a name typed there that matches nothing is now refused
 * by the server with a message worth reading, instead of silently accepted.
 *
 * WHY A <select> AND NOT A COMBOBOX. A namespace can hold more workflows than a
 * dropdown wants, and a typeahead is the usual answer. It is not the answer
 * here for two reasons. A correct combobox needs a popup listbox with its own
 * styles, and the one control in this console that does it properly —
 * CommandPalette — is an overlay with a stylesheet block of its own; a second,
 * half-styled implementation inlined into a form is precisely the "broken
 * autocomplete" that loses to a working select. And a native select is already
 * a listbox: it gets type-to-jump, keyboard selection, the platform's own
 * scrolling and mobile pickers for free, and no aria-activedescendant to get
 * wrong. The catalog is fetched at the same 200-row cap the catalog PAGE uses,
 * which every namespace this has been pointed at fits inside several times over.
 *
 * WHY FREE TEXT SURVIVES ANYWAY. The fetch is capped and can fail, so the list
 * is not authoritative about what exists — and making a valid spec name
 * untypeable because the console could not enumerate it would turn a degraded
 * read into a blocked write. The escape hatch is therefore explicit ("a name
 * not in the list"), and it is the ONLY mode offered when the catalog could not
 * be read at all, where a select would be an empty list asserting something the
 * console never established.
 *
 * THE THREE EMPTY-ISH STATES ARE THREE DIFFERENT FACTS, and the console is
 * careful about this everywhere else (see CommandPalette's per-group notices):
 *
 *   - the catalog was READ and holds nothing → nobody has authored a workflow
 *     yet, so say how one is authored;
 *   - the catalog COULD NOT BE READ → say that, and do not render a list whose
 *     emptiness would read as "this namespace has no workflows";
 *   - the catalog is still loading → say nothing yet, rather than flashing
 *     either of the above at an operator who has waited 200ms.
 */

import { useEffect, useId, useState } from 'react';
import { errorMessage, useWorkflows } from '../api/hooks';

/**
 * MUST MATCH api/client.ts listWorkflows' default `limit`, for the same reason
 * WorkflowsPage restates it: 200 rows back from a 200-row request means "at
 * least 200", and the hint below only promises the list is complete when it
 * demonstrably is.
 */
const CATALOG_LIMIT = 200;

export interface WorkflowPickerProps {
  namespace: string;
  /** The chosen spec name, owned by the form so submit reads one value. */
  value: string;
  onChange: (specName: string) => void;
  /** Wired to the same id the label points at, so the field stays labelled. */
  id?: string;
}

export function WorkflowPicker({ namespace, value, onChange, id }: WorkflowPickerProps) {
  // The empty search fetches the catalog once and caches it for 30s. No
  // debounce is needed because nothing here is typed into the query: unlike
  // WorkflowsPage, the term never changes.
  const workflows = useWorkflows(namespace, '');
  const rows = workflows.data;

  /**
   * Whether the operator has switched to typing a name.
   *
   * Held as state rather than derived from "is the value in the list", because
   * those disagree at exactly the wrong moment: a pre-filled spec name arrives
   * before the catalog does, and deriving the mode would start the field in
   * free-text, then yank it into a select underneath the operator when the
   * fetch lands. Once chosen, the mode is the operator's.
   */
  const [manual, setManual] = useState(false);

  const failed = workflows.isError;
  // Loading is only interesting on the FIRST fetch; a background refetch that
  // still has rows should not blank the control.
  const loading = workflows.isLoading && rows === undefined;
  const catalogEmpty = rows !== undefined && rows.length === 0;

  /**
   * The catalog cannot offer a choice, so the field must be typeable.
   *
   * Note this is not "manual || failed": a failed or empty catalog forces the
   * text input regardless of what the operator picked earlier, because the
   * alternative is a select with no options — a control that cannot be used to
   * complete the form it is required by.
   */
  const typing = manual || failed || catalogEmpty;

  const hintId = useId();

  /**
   * A pre-filled spec that is not in the catalog must still be reachable.
   *
   * This is the deep-link case: /schedules?new=1&spec=<name> pre-selects a
   * workflow, and the name can legitimately be absent from the fetched page —
   * past the 200-row cap, or authored between this fetch and the link being
   * followed. Silently leaving the select on its placeholder would then discard
   * a value the operator explicitly asked for, so the control switches itself
   * to the text mode that CAN hold it. Only ever switches on, never back:
   * flipping an operator out of a mode they are typing in would be worse than
   * the stale mode.
   */
  useEffect(() => {
    if (value === '' || rows === undefined) return;
    if (!rows.some((w) => w.name === value)) setManual(true);
  }, [value, rows]);

  return (
    <div className="form-field">
      <span className="form-label" id={`${hintId}-label`}>
        workflow
      </span>

      {typing ? (
        <input
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder="example-etl"
          className="mono"
          aria-label="workflow"
          aria-describedby={hintId}
          required
        />
      ) : (
        <select
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          aria-label="workflow"
          aria-describedby={hintId}
          required
        >
          {/* An explicit unselected option rather than defaulting to the first
              workflow in the catalog. A select that arrives pre-pointed at a
              workflow nobody chose is how a schedule gets created for the
              alphabetically-first spec in the namespace. */}
          <option value="">Select a workflow…</option>
          {(rows ?? []).map((w) => (
            <option key={w.name} value={w.name}>
              {w.name} (v{w.latest_version})
            </option>
          ))}
        </select>
      )}

      <span className={`form-hint${failed ? ' invalid' : ''}`} id={hintId}>
        <WorkflowPickerHint
          loading={loading}
          failed={failed}
          error={workflows.error}
          catalogEmpty={catalogEmpty}
          count={rows?.length ?? 0}
          typing={typing}
          manual={manual}
          onUseCatalog={() => setManual(false)}
          onTypeName={() => setManual(true)}
        />
      </span>
    </div>
  );
}

/**
 * The line under the field, which is doing more work than a hint usually does:
 * it is where the three states above are told apart, and it carries the toggle
 * between the two modes.
 *
 * Split out so each branch is a readable sentence rather than a nested ternary
 * inside the JSX, and so the "could not read the catalog" case is impossible to
 * merge back into the "no workflows" one by accident.
 */
function WorkflowPickerHint({
  loading,
  failed,
  error,
  catalogEmpty,
  count,
  typing,
  manual,
  onUseCatalog,
  onTypeName,
}: {
  loading: boolean;
  failed: boolean;
  error: unknown;
  catalogEmpty: boolean;
  count: number;
  typing: boolean;
  manual: boolean;
  onUseCatalog: () => void;
  onTypeName: () => void;
}) {
  if (loading) {
    // Deliberately says nothing about the catalog's contents — it does not know
    // any of it yet, and both of the other branches would be a guess.
    return <>Loading the workflows in this namespace…</>;
  }

  if (failed) {
    // NOT "no workflows". The request failed, so the console has established
    // nothing about what this namespace holds, and the field falls back to free
    // text so the failure costs a listing rather than the whole form. role=alert
    // because a control that silently changed shape needs to say why.
    return (
      <span role="alert">
        Could not load the workflow catalog — {errorMessage(error)} Type the spec name instead;
        it is checked when you save.
      </span>
    );
  }

  if (catalogEmpty) {
    return (
      <>
        No workflows have been authored in this namespace yet. They are submitted through the
        SDK or <code>nexusctl spec apply -f workflow.yaml</code> — a schedule can still name one
        that does not exist yet, but it will fail every run until it does.
      </>
    );
  }

  if (typing && manual) {
    return (
      <>
        Typing a name the catalog did not list. The gateway does NOT check that it exists, so a
        typo creates a schedule that fires and fails.{' '}
        <button type="button" className="link" onClick={onUseCatalog}>
          Choose from the list instead
        </button>
      </>
    );
  }

  return (
    <>
      The workflow this schedule creates runs of.{' '}
      {count >= CATALOG_LIMIT
        ? `Showing the first ${CATALOG_LIMIT} in the catalog.`
        : `All ${count} in this namespace.`}{' '}
      <button type="button" className="link" onClick={onTypeName}>
        Enter a name not in the list
      </button>
    </>
  );
}
