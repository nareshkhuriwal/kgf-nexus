// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The accent is user-switchable, which is only safe because the brand hue and
 * the STATE palette are different variables. The tests that matter here are the
 * ones that would catch that separation being broken, and the ones that keep a
 * storage failure from taking the console down.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  ACCENTS,
  applyAccent,
  applyStoredAppearance,
  getAccent,
  getRailCollapsed,
  isAccentName,
  setAccent,
  setRailCollapsed,
} from './appearance';

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.removeAttribute('data-accent');
  window.localStorage.removeItem('nexus.railCollapsed');
  document.documentElement.style.removeProperty('--accent');
  document.documentElement.style.removeProperty('--focus');
  document.documentElement.style.removeProperty('--accent-on');
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('defaults', () => {
  // Indigo and sidebar are the shipped defaults; a console with nothing stored
  // must render those rather than undefined.
  it('falls back to indigo and the sidebar when nothing is stored', () => {
    expect(getAccent()).toBe('indigo');
    expect(getRailCollapsed()).toBe(false);
  });

  it('ignores a stored value it does not recognise', () => {
    window.localStorage.setItem('nexus.accent', 'chartreuse');
    window.localStorage.setItem('nexus.nav', 'carousel');
    expect(getAccent()).toBe('indigo');
    expect(getRailCollapsed()).toBe(false);
  });
});

describe('applying an accent', () => {
  it('writes all three custom properties and records which accent is active', () => {
    applyAccent('plum');

    const root = document.documentElement;
    expect(root.style.getPropertyValue('--accent')).toBe(ACCENTS.plum.fg);
    expect(root.style.getPropertyValue('--focus')).toBe(ACCENTS.plum.focus);
    // --accent-on is not derivable from --accent, which is why it is carried
    // per accent rather than computed.
    expect(root.style.getPropertyValue('--accent-on')).toBe(ACCENTS.plum.on);
    expect(root.getAttribute('data-accent')).toBe('plum');
  });

  /**
   * THE LOAD-BEARING TEST. Swapping the brand hue must never touch a --tone-*
   * variable: those carry run state, and an accent that could repaint them
   * would make a FAILED run capable of looking SUCCEEDED. If this ever fails,
   * accent switching has stopped being safe to offer.
   */
  it('never writes a state-colour variable', () => {
    applyAccent('steel');

    const written = Array.from(document.documentElement.style);
    expect(written.some((prop) => prop.startsWith('--tone-'))).toBe(false);
    expect(written.sort()).toEqual(['--accent', '--accent-on', '--focus']);
  });

  it('round-trips every accent it offers', () => {
    for (const name of Object.keys(ACCENTS) as Array<keyof typeof ACCENTS>) {
      expect(setAccent(name)).toBe(true);
      expect(getAccent()).toBe(name);
      expect(document.documentElement.style.getPropertyValue('--accent')).toBe(ACCENTS[name].fg);
    }
  });
});

describe('the collapsed rail preference', () => {
  /**
   * Stored, unlike the layout choice it replaced, but NOT stamped on <html>.
   *
   * The rail's collapsed state is read by React and rendered as a class on the
   * shell, because the rail is a component that has to change what it renders --
   * labels under icons rather than beside them -- not merely how it is painted.
   * A data attribute would be a second copy of that fact for CSS to key on, and
   * the two would eventually disagree.
   */
  it('defaults to expanded, which is the shape an operator has not chosen', () => {
    expect(getRailCollapsed()).toBe(false);
  });

  it('round-trips both states', () => {
    expect(setRailCollapsed(true)).toBe(true);
    expect(getRailCollapsed()).toBe(true);
    expect(setRailCollapsed(false)).toBe(true);
    expect(getRailCollapsed()).toBe(false);
  });

  // Anything other than the exact stored 'true' is expanded, so a corrupted or
  // half-written value fails toward the shape that shows every destination.
  it('treats an unrecognised stored value as expanded', () => {
    window.localStorage.setItem('nexus.railCollapsed', 'yes');
    expect(getRailCollapsed()).toBe(false);
  });
});

describe('storage that is unavailable', () => {
  /**
   * Private browsing and blocked site-data both throw on localStorage. A
   * console that refused to render because it could not read a colour
   * preference would be a spectacular own goal, so every read swallows and
   * falls back.
   */
  it('still returns defaults when reading throws', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError');
    });
    expect(getAccent()).toBe('indigo');
    expect(getRailCollapsed()).toBe(false);
  });

  /**
   * A write that fails must still APPLY the choice — the operator asked for it
   * and this page can honour it — while reporting false so the UI can say the
   * preference will not survive a reload rather than silently forgetting.
   */
  it('applies the choice but reports that it was not persisted', () => {
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError');
    });

    expect(setAccent('plum')).toBe(false);
    expect(document.documentElement.style.getPropertyValue('--accent')).toBe(ACCENTS.plum.fg);

    // The rail preference has nowhere to "apply" to outside React, so the only
    // contract here is the honest false.
    expect(setRailCollapsed(true)).toBe(false);
  });
});

describe('applyStoredAppearance', () => {
  it('applies the stored accent', () => {
    window.localStorage.setItem('nexus.accent', 'azure');

    applyStoredAppearance();

    expect(document.documentElement.getAttribute('data-accent')).toBe('azure');
  });
});

describe('guards', () => {
  it('recognises exactly the values it offers', () => {
    expect(isAccentName('indigo')).toBe(true);
    expect(isAccentName('mauve')).toBe(false);
    expect(isAccentName(null)).toBe(false);
  });

  // Every accent needs all three values, or a swatch renders a broken palette.
  it('defines a complete triplet and a description for every accent', () => {
    for (const [name, a] of Object.entries(ACCENTS)) {
      expect(a.fg, `${name}.fg`).toMatch(/^#[0-9a-f]{6}$/i);
      expect(a.focus, `${name}.focus`).toMatch(/^#[0-9a-f]{6}$/i);
      expect(a.on, `${name}.on`).toMatch(/^#[0-9a-f]{6}$/i);
      expect(a.label, `${name}.label`).toBeTruthy();
      expect(a.hint, `${name}.hint`).toBeTruthy();
    }
  });
});
