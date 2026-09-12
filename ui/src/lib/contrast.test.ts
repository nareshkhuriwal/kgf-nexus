/**
 * The palette must stay legible in BOTH themes.
 *
 * These tests read the real index.css, so they assert what actually ships. The
 * failure they exist to prevent is silent: someone nudges a tone colour to look
 * better on their monitor, it drops under the legibility threshold in the other
 * theme, and nobody notices until an operator cannot read a state badge during
 * an incident. A ratio is checkable; "looks fine" is not.
 */

import { describe, expect, it } from 'vitest';
// The real stylesheet, loaded as text by Vite. Importing it rather than reading
// it through node:fs keeps this suite free of Node type dependencies the rest of
// the app deliberately does not carry -- and it still asserts the CSS that ships,
// which is the whole point.
import css from '../index.css?raw';
import {
  AA_BODY_TEXT,
  AA_UI,
  composite,
  contrastRatio,
  extractVars,
  hueDistance,
  parseColor,
  type RGB,
} from './contrast';

// The dark palette is the base on :root; the light palette is the explicit
// data-theme='light' block, which carries the same values the media query uses.
const dark = extractVars(css, ':root');
const light = extractVars(css, ":root[data-theme='light']");

const TONES = [
  'neutral',
  'queued',
  'active',
  'success',
  'danger',
  'blocked',
  'muted',
  'warning',
  'stopped',
] as const;

function mustParse(value: string | undefined, what: string): RGB {
  const parsed = value ? parseColor(value) : null;
  if (!parsed) throw new Error(`could not parse ${what}: ${String(value)}`);
  return parsed;
}

describe('contrast helpers', () => {
  it('computes the reference ratios', () => {
    // Black on white is the maximum, 21:1 -- the standard sanity check that the
    // luminance formula is right rather than merely plausible.
    const white = { r: 255, g: 255, b: 255 };
    const black = { r: 0, g: 0, b: 0 };
    expect(contrastRatio(white, black)).toBeCloseTo(21, 1);
    expect(contrastRatio(white, white)).toBeCloseTo(1, 5);
  });

  it('parses the colour forms the stylesheet actually uses', () => {
    expect(parseColor('#fff')).toEqual({ r: 255, g: 255, b: 255 });
    expect(parseColor('#0d1117')).toEqual({ r: 13, g: 17, b: 23 });
    expect(parseColor('rgba(145, 152, 161, 0.12)')).toEqual({ r: 145, g: 152, b: 161 });
    expect(parseColor('not-a-colour')).toBeNull();
  });

  it('composites a translucent fill over its background', () => {
    // A 50% white over black is mid-grey. Without compositing, a translucent
    // tone fill would be measured as a colour that never appears on screen.
    const out = composite('rgba(255, 255, 255, 0.5)', { r: 0, g: 0, b: 0 });
    expect(out?.r).toBeCloseTo(127.5, 1);
  });
});

describe.each([
  ['dark', dark],
  ['light', light],
])('%s palette', (themeName, vars) => {
  it('defines every semantic colour', () => {
    for (const name of ['--bg', '--bg-raised', '--fg', '--fg-muted', '--border', '--focus']) {
      expect(vars[name], `${themeName} is missing ${name}`).toBeTruthy();
    }
  });

  it('body text clears WCAG AA against both page grounds', () => {
    const fg = mustParse(vars['--fg'], `${themeName} --fg`);
    for (const ground of ['--bg', '--bg-raised', '--bg-inset']) {
      const bg = mustParse(vars[ground], `${themeName} ${ground}`);
      const ratio = contrastRatio(fg, bg);
      expect(
        ratio,
        `${themeName}: --fg on ${ground} is ${ratio.toFixed(2)}:1, below AA ${AA_BODY_TEXT}:1`,
      ).toBeGreaterThanOrEqual(AA_BODY_TEXT);
    }
  });

  it('muted text clears WCAG AA on the panel ground', () => {
    // --fg-muted carries real information (timestamps, counts, queue names), so
    // it is held to the body-text bar rather than the large-text one.
    const bg = mustParse(vars['--bg-raised'], `${themeName} --bg-raised`);
    const muted = mustParse(vars['--fg-muted'], `${themeName} --fg-muted`);
    const ratio = contrastRatio(muted, bg);
    expect(
      ratio,
      `${themeName}: --fg-muted on --bg-raised is ${ratio.toFixed(2)}:1, below AA ${AA_BODY_TEXT}:1`,
    ).toBeGreaterThanOrEqual(AA_BODY_TEXT);
  });

  it.each(TONES)('tone %s is readable on its own fill', (tone) => {
    // This is the one that matters most operationally. A badge is tone text on
    // the SAME tone's translucent fill over the panel ground, so the perceived
    // contrast is the composited result -- which is what an operator reads when
    // scanning a run for the one node that failed.
    const ground = mustParse(vars['--bg-raised'], `${themeName} --bg-raised`);
    const fillRaw = vars[`--tone-${tone}-bg`];
    const fill = composite(fillRaw ?? '', ground);
    const fg = mustParse(vars[`--tone-${tone}-fg`], `${themeName} --tone-${tone}-fg`);
    if (!fill) throw new Error(`could not composite --tone-${tone}-bg in ${themeName}`);

    const ratio = contrastRatio(fg, fill);
    expect(
      ratio,
      `${themeName}: tone "${tone}" text on its own fill is ${ratio.toFixed(2)}:1, ` +
        `below AA ${AA_BODY_TEXT}:1 — a state badge an operator cannot read`,
    ).toBeGreaterThanOrEqual(AA_BODY_TEXT);
  });

  it.each(TONES)('tone %s border clears the 3:1 non-text bar', (tone) => {
    // Borders and the state glyph strokes carry meaning (states.ts gives each
    // state a distinct border STYLE precisely so colour is not the only channel),
    // so they are held to the WCAG non-text contrast minimum.
    const ground = mustParse(vars['--bg-raised'], `${themeName} --bg-raised`);
    const bd = mustParse(vars[`--tone-${tone}-bd`], `${themeName} --tone-${tone}-bd`);
    const ratio = contrastRatio(bd, ground);
    expect(
      ratio,
      `${themeName}: tone "${tone}" border on --bg-raised is ${ratio.toFixed(2)}:1, below ${AA_UI}:1`,
    ).toBeGreaterThanOrEqual(AA_UI);
  });

  it('the focus ring is visible against every ground', () => {
    // A focus ring that fails contrast is the same as having no focus ring, and
    // keyboard navigation is the thing it makes possible.
    const focus = mustParse(vars['--focus'], `${themeName} --focus`);
    for (const ground of ['--bg', '--bg-raised', '--bg-inset']) {
      const bg = mustParse(vars[ground], `${themeName} ${ground}`);
      const ratio = contrastRatio(focus, bg);
      expect(
        ratio,
        `${themeName}: --focus on ${ground} is ${ratio.toFixed(2)}:1, below ${AA_UI}:1`,
      ).toBeGreaterThanOrEqual(AA_UI);
    }
  });
});

describe('state legibility invariants', () => {
  /**
   * CONTRACT.md invariant 2: LOST is NOT FAILED. LOST means "we do not know if
   * it ran"; FAILED means "we know it ran and did not succeed". They drive
   * different operator actions, so they must not look alike -- states.ts already
   * gives them different tones, glyphs and border styles, and this asserts the
   * COLOURS stay distinguishable too.
   */
  it.each([
    ['dark', dark],
    ['light', light],
  ])('%s: LOST (warning) and FAILED (danger) are visually distinct', (themeName, vars) => {
    const lost = mustParse(vars['--tone-warning-fg'], `${themeName} warning`);
    const failed = mustParse(vars['--tone-danger-fg'], `${themeName} danger`);

    // Hue again: both are alarm colours of similar brightness by design.
    const separation = hueDistance(lost, failed);
    expect(separation, `${themeName}: one of warning/danger is achromatic`).not.toBeNull();
    expect(
      separation!,
      `${themeName}: LOST and FAILED are only ${separation?.toFixed(1)}° apart in hue; ` +
        `CONTRACT invariant 2 requires an operator to tell them apart at a glance`,
    ).toBeGreaterThan(15);
  });

  /**
   * UPSTREAM_FAILED must not read as the failure itself: index.css states the
   * blocked tone is "deliberately amber-brown and NOT red" for this reason.
   */
  it.each([
    ['dark', dark],
    ['light', light],
  ])('%s: UPSTREAM_FAILED (blocked) does not read as FAILED (danger)', (themeName, vars) => {
    const blocked = mustParse(vars['--tone-blocked-fg'], `${themeName} blocked`);
    const danger = mustParse(vars['--tone-danger-fg'], `${themeName} danger`);

    // HUE, not contrast ratio. The two are deliberately similar in brightness --
    // both are alarm colours — so a luminance measure reports ~1.1:1 and says
    // nothing. The distinction index.css makes ("amber-brown and NOT red") is a
    // hue distinction, so that is what must be asserted.
    const separation = hueDistance(blocked, danger);
    expect(separation, `${themeName}: one of blocked/danger is achromatic`).not.toBeNull();
    expect(
      separation!,
      `${themeName}: blocked and danger are only ${separation?.toFixed(1)}° apart in hue; ` +
        `"blocked by an upstream failure" must not look like "this is the failure"`,
    ).toBeGreaterThan(15);
  });
});

describe('design scales', () => {
  // The scales are the whole point of deliverable 3.5: without them every
  // component invents its own spacing and density cannot be adjusted globally.
  it('defines the spacing, type, radius, shadow and z-index scales', () => {
    for (const token of [
      '--space-1', '--space-4', '--space-9',
      '--text-xs', '--text-base', '--text-xl',
      '--radius-sm', '--radius-pill',
      '--shadow-sm', '--shadow-lg',
      '--z-sticky', '--z-modal', '--z-toast',
      '--focus-ring',
    ]) {
      expect(dark[token], `:root is missing ${token}`).toBeTruthy();
    }
  });

  it('keeps the z-index scale strictly ordered', () => {
    // An overlay that lands below a sticky header is a bug that only shows up on
    // one screen size; ordering them once here prevents arbitrary new values.
    const z = (name: string) => parseInt(dark[name] ?? '', 10);
    expect(z('--z-base')).toBeLessThan(z('--z-sticky'));
    expect(z('--z-sticky')).toBeLessThan(z('--z-overlay'));
    expect(z('--z-overlay')).toBeLessThan(z('--z-modal'));
    expect(z('--z-modal')).toBeLessThan(z('--z-toast'));
  });
});
