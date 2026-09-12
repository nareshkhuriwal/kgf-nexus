// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Three-state theme control: light, dark, or follow the system.
 *
 * WHY THREE AND NOT TWO. A two-state toggle has to pick an initial value, and
 * whatever it picks is wrong for half of users on first load — and worse, it
 * permanently overrides the OS preference they already expressed. "System" is
 * the honest default: it means "I have not chosen", which is different from
 * having chosen the theme that happens to match right now. Only an explicit
 * choice writes anything down.
 *
 * The mechanism is the one index.css already anticipated: an explicit choice
 * stamps data-theme on <html>, and "system" REMOVES the attribute so the
 * prefers-color-scheme media query takes over again. Removing rather than
 * setting data-theme="system" matters — a value the CSS does not know would
 * match neither palette rule.
 */

export type ThemeChoice = 'light' | 'dark' | 'system';

/** What the page is actually rendering, once "system" has been resolved. */
export type ResolvedTheme = 'light' | 'dark';

const STORAGE_KEY = 'nexus.theme';

export function isThemeChoice(v: unknown): v is ThemeChoice {
  return v === 'light' || v === 'dark' || v === 'system';
}

/**
 * The stored choice, defaulting to 'system'.
 *
 * Every storage access is wrapped: private browsing and blocked site-data both
 * throw on localStorage, and a console that refuses to render because it could
 * not read a colour preference would be a spectacular own goal.
 */
export function getThemeChoice(): ThemeChoice {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (isThemeChoice(raw)) return raw;
  } catch {
    /* storage unavailable; fall through to the system default */
  }
  return 'system';
}

/**
 * Persist a choice and apply it. Returns whether it was actually persisted, so
 * a caller can tell the user their preference will not survive a reload rather
 * than silently forgetting it — the same honesty the API-key control already
 * applies in client.ts.
 */
export function setThemeChoice(choice: ThemeChoice): boolean {
  let stored = false;
  try {
    if (choice === 'system') {
      // 'system' is the absence of a preference, so it is stored as an absence.
      // Writing the literal string would work too, but removing keeps the stored
      // state and the DOM state describing the same idea.
      window.localStorage.removeItem(STORAGE_KEY);
    } else {
      window.localStorage.setItem(STORAGE_KEY, choice);
    }
    stored = getThemeChoice() === choice;
  } catch {
    stored = false;
  }
  applyTheme(choice);
  return stored;
}

/**
 * Stamp the choice onto the document element.
 *
 * Exported so the app can apply the stored choice on boot before first paint.
 */
export function applyTheme(choice: ThemeChoice, root?: HTMLElement): void {
  const el = root ?? document.documentElement;
  if (choice === 'system') {
    el.removeAttribute('data-theme');
  } else {
    el.setAttribute('data-theme', choice);
  }
}

/** Whether the OS currently prefers light. */
export function systemPrefersLight(): boolean {
  try {
    return window.matchMedia('(prefers-color-scheme: light)').matches;
  } catch {
    // matchMedia is absent in some test environments. Dark is the design's base
    // palette, so it is the safer assumption.
    return false;
  }
}

/** What is actually on screen, with 'system' resolved against the OS. */
export function resolveTheme(choice: ThemeChoice): ResolvedTheme {
  if (choice !== 'system') return choice;
  return systemPrefersLight() ? 'light' : 'dark';
}

/** The next choice in the cycle, for a single-button toggle. */
export function nextTheme(choice: ThemeChoice): ThemeChoice {
  return choice === 'system' ? 'light' : choice === 'light' ? 'dark' : 'system';
}

/** Human label for the control. */
export function themeLabel(choice: ThemeChoice): string {
  return choice === 'system' ? 'System' : choice === 'light' ? 'Light' : 'Dark';
}
