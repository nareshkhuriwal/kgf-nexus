// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The bar's search control: a BUTTON dressed as a field.
 *
 * WHY A BUTTON AND NOT AN INPUT. There is already one search in this console
 * and it is CommandPalette, which fans out across the real endpoints, ranks the
 * static commands against the same query, and — the part that matters most —
 * distinguishes "nothing matches" from "I could not ask", per source. A second input in the
 * bar would either reimplement all of that badly or, far worse, accept
 * keystrokes and silently drop them until the operator noticed nothing was
 * happening. So this control's entire job is to LOOK like the reference
 * design's prominent search field and then hand the query to the palette that
 * can actually answer it. CommandPalette already accepts `open`/`onOpenChange`
 * for exactly this case, so nothing new had to be built to support it.
 *
 * WHY THE LABEL SAYS WHAT IT SAYS. It is the honest list of what the palette
 * ACTUALLY searches, checked against lib/searchSources.ts rather than against
 * the reference screenshot — which is why it changed. It used to read "runs,
 * workflows and commands" while the palette asked exactly one endpoint, the
 * workflow catalog; an operator who typed a colleague's email or a schedule
 * name got an empty panel and concluded the console could not find them. The
 * label now names all six kinds of RESULT because all six are genuinely
 * searched: workflows and spec-backed runs server-side, schedules, workers and
 * (for a credential permitted to read them) users matched over a capped fetch,
 * plus the command registry. It still does NOT promise node ids, logs or docs,
 * because those are still not searched. The palette's own field says "Run id,
 * name, host or email" because that is what a single query string may BE; this
 * one names what comes back, which is the useful promise to make before the
 * panel is open. Over-promising here would be the same failure as rendering an
 * empty list for a failed search: the operator goes looking for something they
 * were told was searchable.
 *
 * The Cmd+K hint is inside the control, the way the reference puts its shortcut
 * inside the field, so the keyboard route is discoverable from the thing it
 * replaces rather than from documentation nobody opens mid-incident.
 */

import { useMemo } from 'react';

export interface NavSearchProps {
  /** Opens the palette. The bar owns no search state of its own. */
  onOpen: () => void;
}

/**
 * Which modifier to print in the hint.
 *
 * CommandPalette accepts metaKey OR ctrlKey without caring which, so there is
 * no existing platform check to reuse and none is added to the shortcut itself
 * — both keys must keep working regardless of what this label says, because a
 * Mac with an external PC keyboard is a real configuration. The detection is
 * therefore PRESENTATION ONLY, and it is kept to a userAgent sniff on purpose:
 * navigator.userAgentData is not available everywhere this console runs, and a
 * wrong glyph in a hint costs an operator one guess while a feature-detection
 * dance for a cosmetic label costs every reader of this file a paragraph.
 */
export function shortcutModifier(platform = navigator.userAgent): string {
  return /Mac|iPhone|iPad|iPod/i.test(platform) ? '⌘' : 'Ctrl';
}

export function NavSearch({ onOpen }: NavSearchProps) {
  // Computed once: the platform cannot change while the tab is open, and
  // recomputing it on every render would suggest it might.
  const mod = useMemo(() => shortcutModifier(), []);

  /**
   * TWO strings, not one, and this is a change from the single label this
   * control shipped with.
   *
   * The accessible name is the full list, because a screen-reader user gets it
   * read once and in full — there is nothing to truncate and no reason to be
   * vague. The visible text is the short form, because .navsearch-text
   * ellipsises rather than wraps (the bar is nowrap and a two-line control is
   * the one thing that would make the bar taller), so the long list rendered
   * visibly would become "Search workflows, runs, sched…" — which promises
   * LESS than the old wording did while the palette now does more. A short
   * accurate phrase beats a long one the layout eats.
   *
   * The short form is "Search this namespace" and NOT "Search everything".
   * Everything is not searched: node ids, log lines and spec bodies are not,
   * and a control that claims them sends an operator hunting for a feature
   * that does not exist. What IS true of all six groups is that they are the
   * things this namespace contains, which is both accurate and, on a console
   * that switches namespaces, the more useful thing to say.
   */
  const label = 'Search workflows, runs, schedules, workers, users and commands';
  const shortLabel = 'Search this namespace';

  return (
    <button
      type="button"
      className="navsearch"
      onClick={onOpen}
      // The accessible name is the full sentence even though the visible text
      // collapses to an icon at narrow widths, so the control never becomes an
      // unlabelled glyph for a screen-reader user.
      aria-label={label}
      title={`${label} (${mod}+K)`}
    >
      <span className="navsearch-icon" aria-hidden="true">
        ⌕
      </span>
      {/* aria-hidden because aria-label above already names the control; left
          visible it would be announced twice. */}
      <span className="navsearch-text" aria-hidden="true">
        {shortLabel}
      </span>
      <kbd className="navsearch-kbd" aria-hidden="true">
        {mod}+K
      </kbd>
    </button>
  );
}
