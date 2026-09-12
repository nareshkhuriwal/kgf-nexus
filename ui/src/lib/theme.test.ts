// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The theme control must behave correctly in all three states, including the
 * case index.css could not previously express: an explicit choice that
 * CONTRADICTS the OS preference.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  applyTheme,
  getThemeChoice,
  isThemeChoice,
  nextTheme,
  resolveTheme,
  setThemeChoice,
  themeLabel,
} from './theme';

function mockMatchMedia(prefersLight: boolean) {
  vi.stubGlobal('matchMedia', (query: string) => ({
    matches: query.includes('light') ? prefersLight : !prefersLight,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
  }));
}

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.removeAttribute('data-theme');
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('theme choice storage', () => {
  it('defaults to system when nothing is stored', () => {
    // "System" means "I have not chosen", which is different from having chosen
    // whichever theme happens to match right now.
    expect(getThemeChoice()).toBe('system');
  });

  it('round-trips an explicit choice', () => {
    expect(setThemeChoice('light')).toBe(true);
    expect(getThemeChoice()).toBe('light');
    expect(setThemeChoice('dark')).toBe(true);
    expect(getThemeChoice()).toBe('dark');
  });

  it('stores system as an absence, so the media query takes over again', () => {
    setThemeChoice('dark');
    setThemeChoice('system');
    expect(getThemeChoice()).toBe('system');
    expect(window.localStorage.getItem('nexus.theme')).toBeNull();
  });

  it('ignores a corrupt stored value rather than applying it', () => {
    // A value the CSS does not know would match neither palette rule, leaving
    // the page unstyled; falling back to system is the safe reading.
    window.localStorage.setItem('nexus.theme', 'chartreuse');
    expect(getThemeChoice()).toBe('system');
  });

  it('survives localStorage throwing', () => {
    // Private browsing and blocked site-data both throw here. A console that
    // refused to render because it could not read a colour preference would be
    // a spectacular own goal.
    const spy = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('site data blocked');
    });
    expect(getThemeChoice()).toBe('system');
    spy.mockRestore();
  });

  it('reports when a choice could not be persisted', () => {
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded');
    });
    // It still APPLIES (the page changes) but reports that it will not survive a
    // reload, so the UI can be honest rather than silently forgetting.
    expect(setThemeChoice('light')).toBe(false);
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    spy.mockRestore();
  });
});

describe('applying a theme to the document', () => {
  it('stamps data-theme for an explicit choice', () => {
    applyTheme('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
    applyTheme('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('REMOVES data-theme for system', () => {
    // Removing rather than setting data-theme="system" is load-bearing: a value
    // the stylesheet does not know matches neither palette rule.
    applyTheme('dark');
    applyTheme('system');
    expect(document.documentElement.hasAttribute('data-theme')).toBe(false);
  });
});

describe('resolving what is actually on screen', () => {
  it('follows the OS when the choice is system', () => {
    mockMatchMedia(true);
    expect(resolveTheme('system')).toBe('light');
    mockMatchMedia(false);
    expect(resolveTheme('system')).toBe('dark');
  });

  it('an explicit choice overrides the OS in BOTH directions', () => {
    // This is the case the original stylesheet could not express: its only light
    // rule was gated on the OS preferring light, so a user on a dark OS who
    // picked "light" got nothing at all.
    mockMatchMedia(false); // OS prefers dark
    expect(resolveTheme('light')).toBe('light');
    mockMatchMedia(true); // OS prefers light
    expect(resolveTheme('dark')).toBe('dark');
  });

  it('falls back to dark when matchMedia is unavailable', () => {
    vi.stubGlobal('matchMedia', undefined);
    // Dark is the design's base palette, so it is the safer assumption.
    expect(resolveTheme('system')).toBe('dark');
  });
});

describe('the toggle control', () => {
  it('cycles system -> light -> dark -> system', () => {
    expect(nextTheme('system')).toBe('light');
    expect(nextTheme('light')).toBe('dark');
    expect(nextTheme('dark')).toBe('system');
  });

  it('labels every state', () => {
    expect(themeLabel('system')).toBe('System');
    expect(themeLabel('light')).toBe('Light');
    expect(themeLabel('dark')).toBe('Dark');
  });

  it('validates choices', () => {
    expect(isThemeChoice('light')).toBe(true);
    expect(isThemeChoice('system')).toBe(true);
    expect(isThemeChoice('sepia')).toBe(false);
    expect(isThemeChoice(null)).toBe(false);
  });
});
