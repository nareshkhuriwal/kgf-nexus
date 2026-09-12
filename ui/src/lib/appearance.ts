// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Appearance preferences beyond light/dark: the ACCENT hue and the NAV layout.
 *
 * Separate from theme.ts because they answer different questions and change on
 * different clocks. theme.ts resolves "light or dark", which the OS also has an
 * opinion about; these are purely the operator's taste and have no system
 * equivalent to defer to.
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
export type NavLayout = 'sidebar' | 'navbar';

const ACCENT_KEY = 'nexus.accent';
const NAV_KEY = 'nexus.nav';

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

export function isNavLayout(v: unknown): v is NavLayout {
  return v === 'sidebar' || v === 'navbar';
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
 * The stored nav layout, defaulting to the sidebar.
 *
 * Sidebar by default because the destination count only grows -- the brief
 * names eight areas -- and a top bar that wraps is worse than a rail that
 * scrolls. The navbar remains available for operators who want the vertical
 * space back.
 */
export function getNavLayout(): NavLayout {
  try {
    const raw = window.localStorage.getItem(NAV_KEY);
    if (isNavLayout(raw)) return raw;
  } catch {
    /* storage unavailable; fall through to the default */
  }
  return 'sidebar';
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

/** Apply a nav layout by stamping the root, which the stylesheet keys on. */
export function applyNavLayout(layout: NavLayout, root?: HTMLElement): void {
  (root ?? document.documentElement).setAttribute('data-nav', layout);
}

/** Persist and apply. Returns whether it survived. */
export function setNavLayout(layout: NavLayout, root?: HTMLElement): boolean {
  let stored = false;
  try {
    window.localStorage.setItem(NAV_KEY, layout);
    stored = true;
  } catch {
    /* storage unavailable; the choice still applies for this page */
  }
  applyNavLayout(layout, root);
  return stored;
}

/**
 * Apply both stored preferences. Called once at startup, before first paint,
 * so the console does not flash the default accent and then correct itself.
 */
export function applyStoredAppearance(root?: HTMLElement): void {
  applyAccent(getAccent(), root);
  applyNavLayout(getNavLayout(), root);
}
