// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The three appearance choices, together in one place: theme, accent, layout.
 *
 * Together because they are one decision made three ways — "how should this
 * console look to me" — and splitting them across the menu would make the
 * operator hunt. None of them is scoped to what is on screen, which is what
 * qualifies them for the account menu rather than a page.
 *
 * NOTHING HERE HAS A SAVE BUTTON. Every control applies on click, because the
 * result is visible on the same screen the moment it is chosen: a preview IS
 * the confirmation, and a save step would only let someone leave a change
 * half-applied. Persistence is then a separate fact the panel reports honestly
 * rather than assumes — see below.
 */

import { useState } from 'react';
import { ThemeToggle } from './ThemeToggle';
import {
  ACCENTS,
  getAccent,
  getNavLayout,
  setAccent,
  setNavLayout,
  type AccentName,
  type NavLayout,
} from '../lib/appearance';

const ACCENT_NAMES = Object.keys(ACCENTS) as AccentName[];

const LAYOUTS: { value: NavLayout; label: string; hint: string }[] = [
  { value: 'sidebar', label: 'Sidebar', hint: 'Grouped rail on the left. More room for destinations.' },
  { value: 'navbar', label: 'Navbar', hint: 'One top row. More vertical room for the page.' },
];

export function AppearancePanel() {
  const [accent, setAccentState] = useState<AccentName>(() => getAccent());
  const [layout, setLayoutState] = useState<NavLayout>(() => getNavLayout());
  /**
   * Whether the last choice survived being written down.
   *
   * The setters return false when site data is blocked, and the panel says so
   * in the same words the API-key control uses. Silently forgetting a
   * preference and then reverting on reload is the behaviour that makes people
   * think a setting is broken rather than unavailable.
   */
  const [persisted, setPersisted] = useState(true);

  function chooseAccent(name: AccentName) {
    const ok = setAccent(name);
    setAccentState(name);
    setPersisted(ok);
  }

  function chooseLayout(next: NavLayout) {
    const ok = setNavLayout(next);
    setLayoutState(next);
    setPersisted(ok);
  }

  return (
    <div className="appearance">
      <section className="appearance-group">
        <h3 className="appearance-label">Theme</h3>
        {/* Reused rather than reimplemented: it already owns the three-state
            semantics and the OS-change subscription, and a second control
            writing the same key would be a second source of truth. */}
        <ThemeToggle />
      </section>

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

      <section className="appearance-group">
        <h3 className="appearance-label" id="layout-label">
          Layout
        </h3>
        <div className="layout-choices" role="radiogroup" aria-labelledby="layout-label">
          {LAYOUTS.map((l) => {
            const active = l.value === layout;
            return (
              <button
                key={l.value}
                type="button"
                role="radio"
                aria-checked={active}
                className={`layout-option${active ? ' active' : ''}`}
                onClick={() => chooseLayout(l.value)}
                title={l.hint}
              >
                {/* A miniature of the layout itself. Two rectangles say which
                    shape you are choosing faster than either word does. */}
                <span className={`layout-glyph ${l.value}`} aria-hidden="true">
                  <span className="layout-glyph-chrome" />
                  <span className="layout-glyph-body" />
                </span>
                <span className="layout-name">{l.label}</span>
              </button>
            );
          })}
        </div>
      </section>

      {!persisted && (
        // role=status so the warning is announced when it appears, rather than
        // only being available to someone who happens to re-read the panel.
        <p className="appearance-warning" role="status">
          Applied, but it won't survive a reload — this browser is blocking site
          data.
        </p>
      )}
    </div>
  );
}
