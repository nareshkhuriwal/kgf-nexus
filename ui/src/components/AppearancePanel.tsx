// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The appearance choices: theme and accent.
 *
 * Together because they are one decision made two ways — "how should this
 * console look to me" — and splitting them across surfaces would make the
 * operator hunt.
 *
 * There was a third, LAYOUT, choosing between a sidebar and a navbar. It is
 * gone because the shell no longer offers that choice: the bar and the rail
 * render together now, so the control described nothing an operator could
 * change. A radiogroup whose options both produce the same screen is worse than
 * an absent one — it invites someone to toggle it and conclude the console is
 * broken when nothing moves. They now live on the SETTINGS PAGE rather than in the account
 * menu: a swatch grid is a widget, and a
 * floating dropdown is the worst place to operate one, because a stray click
 * outside it takes the surface away mid-choice. A page cannot be dismissed.
 *
 * What the menu keeps is the theme alone, as `ThemeSection`. Light-versus-dark
 * is the one appearance choice that is made reactively — the sun comes up, the
 * room lights change — so it has to be reachable without a navigation, while
 * the accent is decided once and then left alone for months.
 *
 * NOTHING HERE HAS A SAVE BUTTON. Every control applies on click, because the
 * result is visible on the same screen the moment it is chosen: a preview IS
 * the confirmation, and a save step would only let someone leave a change
 * half-applied. Persistence is then a separate fact the panel reports honestly
 * rather than assumes — see `usePersisted` below.
 */

import { useState } from 'react';
import { ThemeToggle } from './ThemeToggle';
import {
  ACCENTS,
  getAccent,
  setAccent,
  type AccentName,
} from '../lib/appearance';

const ACCENT_NAMES = Object.keys(ACCENTS) as AccentName[];


/**
 * The accent state, and the honesty flag that goes with it.
 *
 * A hook rather than duplicated `useState` in two components: the panel and any
 * future partial view of it must agree on what "applied but not persisted"
 * means, and the setters are the only place that fact is produced. Splitting
 * the UI must not fork the state handling.
 *
 * `persisted` is whether the last choice survived being written down. The
 * setters return false when site data is blocked, and the panel says so in the
 * same words the API-key control uses. Silently forgetting a preference and
 * then reverting on reload is the behaviour that makes people think a setting
 * is broken rather than unavailable.
 */
function useAppearanceState() {
  const [accent, setAccentState] = useState<AccentName>(() => getAccent());
  const [persisted, setPersisted] = useState(true);

  function chooseAccent(name: AccentName) {
    const ok = setAccent(name);
    setAccentState(name);
    setPersisted(ok);
  }

  return { accent, persisted, chooseAccent };
}

/**
 * Reported, never silent. Extracted because both views can produce it and the
 * wording is the contract, not an implementation detail.
 *
 * role=status so the warning is announced when it appears, rather than only
 * being available to someone who happens to re-read the panel.
 */
function PersistenceWarning() {
  return (
    <p className="appearance-warning" role="status">
      Applied, but it won't survive a reload — this browser is blocking site data.
    </p>
  );
}

/**
 * The theme control on its own, for the account menu.
 *
 * Reuses ThemeToggle rather than reimplementing it: that component already owns
 * the three-state semantics and the OS-change subscription, and a second
 * control writing the same key would be a second source of truth. It also
 * reports its own persistence failure through its own status text, so this
 * wrapper adds no second warning.
 */
export function ThemeSection() {
  return (
    <section className="appearance-group">
      <h3 className="appearance-label">Theme</h3>
      <ThemeToggle />
    </section>
  );
}

/** The full set, for the settings page. */
export function AppearancePanel() {
  const { accent, persisted, chooseAccent } = useAppearanceState();

  return (
    <div className="appearance">
      <ThemeSection />

      <section className="appearance-group">
        <h3 className="appearance-label" id="accent-label">
          Accent
        </h3>
        {/* A radiogroup, not a row of buttons: exactly one is in effect, and
            arrow-key semantics come free from the role only if the role is
            real. */}
        <div className="accent-grid" role="radiogroup" aria-labelledby="accent-label">
          {ACCENT_NAMES.map((name) => {
            const a = ACCENTS[name];
            const active = name === accent;
            return (
              <button
                key={name}
                type="button"
                role="radio"
                aria-checked={active}
                className={`accent-option${active ? ' active' : ''}`}
                onClick={() => chooseAccent(name)}
                title={a.hint}
              >
                {/* The swatch paints the accent's own value, so the choice is
                    shown in the colour it produces rather than described. */}
                <span
                  className="accent-swatch"
                  style={{ background: a.fg }}
                  aria-hidden="true"
                />
                <span className="accent-text">
                  <span className="accent-name">{a.label}</span>
                  {/* The hint is the honest part: it names how close each hue
                      sits to a STATE colour, which is the only way this choice
                      can degrade legibility. */}
                  <span className="accent-hint">{a.hint}</span>
                </span>
                {/* A glyph as well as a fill: colour must never be the only
                    signal for which option is selected, least of all in a
                    control whose whole subject is colour. */}
                <span className="accent-check" aria-hidden="true">
                  {active ? '✓' : ''}
                </span>
              </button>
            );
          })}
        </div>
      </section>

      {!persisted && <PersistenceWarning />}
    </div>
  );
}
