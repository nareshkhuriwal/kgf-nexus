// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The bar's search control: a BUTTON dressed as a field.
 *
 * WHY A BUTTON AND NOT AN INPUT. There is already one search in this console
 * and it is CommandPalette, which talks to the real catalog endpoint, ranks the
 * static commands against the same query, and — the part that matters most —
 * distinguishes "nothing matches" from "I could not ask". A second input in the
 * bar would either reimplement all of that badly or, far worse, accept
 * keystrokes and silently drop them until the operator noticed nothing was
 * happening. So this control's entire job is to LOOK like the reference
 * design's prominent search field and then hand the query to the palette that
 * can actually answer it. CommandPalette already accepts `open`/`onOpenChange`
 * for exactly this case, so nothing new had to be built to support it.
 *
 * WHY THE PLACEHOLDER SAYS WHAT IT SAYS. "Runs, workflows and commands" is the
 * honest list, checked against lib/commands.ts rather than against the
 * reference screenshot: a run id is matched locally by asRunId, workflow names
 * come from api.listWorkflows, and the rest of the registry is destinations and
 * preferences — commands. It deliberately does NOT promise nodes, logs, specs
 * or docs. The palette's own field says "Run id, spec or node" because that is
 * what a single query string may BE; this one names the three kinds of RESULT
 * you get back, which is the useful promise to make before the panel is open.
 * Over-promising here would be the same failure as rendering an empty list for
 * a failed search: the operator goes looking for something they were told was
 * searchable.
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
  const label = 'Search runs, workflows and commands';

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
        {label}
      </span>
      <kbd className="navsearch-kbd" aria-hidden="true">
        {mod}+K
      </kbd>
    </button>
  );
}
