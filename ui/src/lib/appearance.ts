// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Appearance preferences beyond light/dark: the ACCENT hue and whether the
 * sidebar is collapsed.
 *
 * Separate from theme.ts because they answer different questions and change on
 * different clocks. theme.ts resolves "light or dark", which the OS also has an
 * opinion about; these are purely the operator's taste and have no system
 * equivalent to defer to.
 *
 * WHAT LEFT THIS FILE. There used to be a NavLayout — 'sidebar' | 'navbar' —
 * stamped on <html> as data-nav, with App rendering one chrome or the other and
 * a radiogroup on /settings to choose. The shell no longer offers that choice
 * because it no longer has to: the navbar and the sidebar are now both present
 * at once, the bar carrying identity and tools across the top and the rail
 * carrying navigation down the left. With one shell the setting described
 * nothing, and a control that changes nothing is worse than an absent one — it
 * invites an operator to spend a click discovering that. The type, its storage
 * key, its getter, its setter, its applier and the stylesheet rules that keyed
 * on data-nav are all gone rather than left dormant.
 *
 * What replaced it is a narrower and honest preference: the rail's COLLAPSED
 * state. That is not a layout choice between two shells, it is one shell
 * remembering how wide the operator left its rail.
 *
 * WHY THE ACCENT IS SAFE TO CHANGE AT ALL. index.css already separates the
 * brand hue (--accent, --focus) from the STATE palette (--tone-*), and only
 * nine rules reference the accent: links, the active nav item, the primary
 * button, the focus ring and the brand mark. Nothing that carries meaning is
 * accent-coloured, so swapping it cannot make a FAILED run look like a
 * SUCCEEDED one. That separation is the precondition for offering this at all;
 * a console where the brand hue and the danger hue were the same variable could
 * not.
 */

export type AccentName = 'indigo' | 'plum' | 'steel' | 'azure';

const ACCENT_KEY = 'nexus.accent';
const RAIL_KEY = 'nexus.railCollapsed';

/**
 * The accents, as the values the CSS custom properties take.
 *
 * `fg` is the hue itself, `focus` the lighter ring, and `on` the text colour
 * that sits ON a filled accent surface. The third is not derivable: a mid-tone
 * indigo needs white text while a pale steel needs near-black, and computing
 * that at runtime would be guesswork where a measured value is cheap.
 *
 * Each pair was checked against both palettes for the 4.5:1 text contrast the
 * rest of index.css already holds itself to.
 */
export const ACCENTS: Record<AccentName, { label: string; hint: string; fg: string; focus: string; on: string }> = {
  indigo: {
    label: 'Indigo',
    hint: 'Cool and technical. Furthest from every state colour.',
    fg: '#818cf8',
    focus: '#a5b4fc',
    on: '#0b0f1a',
  },
  plum: {
    label: 'Plum',
    hint: 'Warmer and more branded. Sits nearer the FAILED hue.',
    fg: '#e07a9c',
    focus: '#f0a3bd',
    on: '#1a0b12',
  },
  steel: {
    label: 'Steel',
    hint: 'No brand hue: every coloured pixel on screen is a run state.',
    fg: '#9aa4b2',
    focus: '#cbd5e1',
    on: '#0d1117',
  },
  azure: {
    label: 'Azure',
    hint: 'The original blue. Closest to the ACTIVE state colour.',
    fg: '#4493f8',
    focus: '#58a6ff',
    on: '#06101f',
  },
};

export function isAccentName(v: unknown): v is AccentName {
  return typeof v === 'string' && Object.prototype.hasOwnProperty.call(ACCENTS, v);
}

/**
 * The stored accent, defaulting to indigo.
 *
 * Wrapped like every other storage read here: private browsing and blocked
 * site-data both throw, and a console that will not render because it could not
 * read a colour preference would be a spectacular own goal.
 */
export function getAccent(): AccentName {
  try {
    const raw = window.localStorage.getItem(ACCENT_KEY);
    if (isAccentName(raw)) return raw;
  } catch {
    /* storage unavailable; fall through to the default */
  }
  return 'indigo';
}

/**
 * Whether the sidebar was left collapsed to its icon rail. Defaults to FALSE —
 * expanded.
 *
 * Expanded by default because the labels are what make the rail readable
 * without prior knowledge of this console: a new operator, or one on their
 * first shift with it, should not have to learn eight icons before they can
 * navigate. The collapse is for the operator who already knows them and wants
 * the horizontal space back, and that is a preference they express rather than
 * one imposed on them.
 *
 * Stored as the string 'true' and compared exactly, rather than coerced. The
 * whole set of values that mean collapsed is one string, so anything else —
 * '1', 'yes', a stale value from a previous encoding, JSON — falls through to
 * expanded, which is the safe direction: an unreadable preference leaves the
 * labels on rather than hiding them.
 */
export function getRailCollapsed(): boolean {
  try {
    return window.localStorage.getItem(RAIL_KEY) === 'true';
  } catch {
    /* storage unavailable; fall through to the default */
    return false;
  }
}

/**
 * Persist the rail state. Returns whether it survived, like setAccent.
 *
 * There is no `apply` half to this pair, which is the difference from the
 * accent and from the NavLayout this replaced. Those stamped the document —
 * custom properties, a data attribute — because CSS had to react before React
 * did. The rail's state is a class on the shell element that React owns and
 * re-renders anyway, so stamping <html> as well would be a second copy of one
 * boolean, and the two would disagree the first time one of them was set
 * without the other.
 */
export function setRailCollapsed(collapsed: boolean): boolean {
  try {
    window.localStorage.setItem(RAIL_KEY, collapsed ? 'true' : 'false');
    return true;
  } catch {
    /* storage unavailable; the choice still applies for this page */
    return false;
  }
}

/**
 * Apply an accent by writing the three custom properties onto the root.
 *
 * Written as inline custom properties rather than a data-attribute plus a CSS
 * block per accent, so adding an accent is one entry in ACCENTS rather than an
 * entry plus a stylesheet rule that can silently disagree with it.
 */
export function applyAccent(name: AccentName, root?: HTMLElement): void {
  const el = root ?? document.documentElement;
  const a = ACCENTS[name] ?? ACCENTS.indigo;
  el.style.setProperty('--accent', a.fg);
  el.style.setProperty('--focus', a.focus);
  el.style.setProperty('--accent-on', a.on);
  // Recorded so CSS can special-case an accent if it ever must, and so a
  // screenshot or a bug report shows which one was active.
  el.setAttribute('data-accent', name);
}

/** Persist and apply. Returns whether it survived, like setThemeChoice. */
export function setAccent(name: AccentName, root?: HTMLElement): boolean {
  let stored = false;
  try {
    window.localStorage.setItem(ACCENT_KEY, name);
    stored = true;
  } catch {
    /* storage unavailable; the choice still applies for this page */
  }
  applyAccent(name, root);
  return stored;
}

/**
 * Apply the stored appearance to the document. Called once at startup, before
 * first paint, so the console does not flash the default accent and then
 * correct itself.
 *
 * The accent is now the only thing it applies, which is why it is still a
 * function rather than a call to applyAccent at the one call site: this is the
 * "everything that must be stamped before paint" hook, and the next preference
 * that needs stamping belongs here rather than in main.tsx.
 *
 * The rail's collapsed state is deliberately NOT stamped here. It is React
 * state read from storage when the shell mounts, and a class on the shell
 * element rather than an attribute on <html> — so there is nothing to apply
 * before paint and no second copy to keep in step. See setRailCollapsed.
 */
export function applyStoredAppearance(root?: HTMLElement): void {
  applyAccent(getAccent(), root);
}
