// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * WCAG contrast computation, used by the palette tests.
 *
 * This exists as source rather than living inside a test because the palette is
 * a design decision that must stay checkable: a colour edit that drops a state
 * below the legibility threshold is invisible in review and obvious only to the
 * person who cannot read it. Making the ratio computable turns that into a
 * failing test.
 *
 * Colours are parsed from the same CSS text the browser loads, so the test
 * asserts what actually ships rather than a second copy of the palette that
 * could drift from it.
 */

export interface RGB {
  r: number;
  g: number;
  b: number;
}

/** Parses #rgb, #rrggbb, rgb() and rgba(). Returns null for anything else. */
export function parseColor(input: string): RGB | null {
  const s = input.trim();

  const hex = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(s);
  if (hex) {
    const h = hex[1] ?? '';
    if (h.length === 3) {
      const [r = '0', g = '0', b = '0'] = h.split('');
      return {
        r: parseInt(r + r, 16),
        g: parseInt(g + g, 16),
        b: parseInt(b + b, 16),
      };
    }
    return {
      r: parseInt(h.slice(0, 2), 16),
      g: parseInt(h.slice(2, 4), 16),
      b: parseInt(h.slice(4, 6), 16),
    };
  }

  const rgb = /^rgba?\(([^)]+)\)$/i.exec(s);
  if (rgb) {
    const parts = (rgb[1] ?? '').split(',').map((p) => parseFloat(p.trim()));
    const [r, g, b] = parts;
    if (r === undefined || g === undefined || b === undefined) return null;
    if ([r, g, b].some((n) => Number.isNaN(n))) return null;
    return { r, g, b };
  }
  return null;
}

/**
 * Composites a possibly-translucent colour over a background.
 *
 * Necessary because the tone fills are deliberately translucent (index.css says
 * why: opaque fills turn a dense table into a paint chart). The contrast a user
 * actually perceives is against the COMPOSITED result, so measuring the raw
 * rgba would be measuring a colour that is never on screen.
 */
export function composite(fg: string, background: RGB): RGB | null {
  const s = fg.trim();
  const rgba = /^rgba\(([^)]+)\)$/i.exec(s);
  const base = parseColor(s);
  if (!base) return null;
  if (!rgba) return base;

  const parts = (rgba[1] ?? '').split(',').map((p) => parseFloat(p.trim()));
  const alpha = parts[3] ?? 1;
  return {
    r: base.r * alpha + background.r * (1 - alpha),
    g: base.g * alpha + background.g * (1 - alpha),
    b: base.b * alpha + background.b * (1 - alpha),
  };
}

/** Relative luminance per WCAG 2.1. */
export function luminance({ r, g, b }: RGB): number {
  const channel = (v: number) => {
    const c = v / 255;
    return c <= 0.04045 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4);
  };
  return 0.2126 * channel(r) + 0.7152 * channel(g) + 0.0722 * channel(b);
}

/** Contrast ratio between two colours, 1 (identical) to 21 (black on white). */
export function contrastRatio(a: RGB, b: RGB): number {
  const la = luminance(a);
  const lb = luminance(b);
  const light = Math.max(la, lb);
  const dark = Math.min(la, lb);
  return (light + 0.05) / (dark + 0.05);
}

/**
 * Extracts CSS custom properties from one selector block of a stylesheet.
 *
 * A deliberately small parser rather than a CSS library: it reads `--name: value;`
 * pairs out of a block located by its selector, which is all the palette test
 * needs, and a dependency in the build for that would not earn its place.
 */
export function extractVars(css: string, selector: string): Record<string, string> {
  const at = css.indexOf(selector);
  if (at < 0) return {};

  const open = css.indexOf('{', at);
  if (open < 0) return {};

  let depth = 1;
  let i = open + 1;
  while (i < css.length && depth > 0) {
    if (css[i] === '{') depth++;
    else if (css[i] === '}') depth--;
    i++;
  }
  const body = css.slice(open + 1, i - 1);

  const out: Record<string, string> = {};
  for (const m of body.matchAll(/(--[\w-]+)\s*:\s*([^;]+);/g)) {
    const name = m[1];
    const value = m[2];
    if (name && value) out[name] = value.trim();
  }
  return out;
}

/** WCAG AA thresholds. */
export const AA_BODY_TEXT = 4.5;
export const AA_LARGE_TEXT = 3.0;
/** Non-text UI (borders, meters, glyph strokes) share the 3:1 bar. */
export const AA_UI = 3.0;

/**
 * Hue angle in degrees (0-360), or null for an achromatic colour.
 *
 * Needed because contrast ratio is a LUMINANCE measure and therefore blind to
 * hue: amber-brown and red can sit at the same brightness and score ~1.1:1
 * against each other while being obviously different colours to a viewer. The
 * blocked-vs-danger distinction index.css deliberately makes is a hue
 * distinction, so asserting it requires measuring hue rather than luminance.
 */
export function hue({ r, g, b }: RGB): number | null {
  const rn = r / 255;
  const gn = g / 255;
  const bn = b / 255;
  const max = Math.max(rn, gn, bn);
  const min = Math.min(rn, gn, bn);
  const delta = max - min;
  // A near-grey has no meaningful hue; reporting one would be noise.
  if (delta < 0.02) return null;

  let h: number;
  if (max === rn) h = ((gn - bn) / delta) % 6;
  else if (max === gn) h = (bn - rn) / delta + 2;
  else h = (rn - gn) / delta + 4;

  h *= 60;
  return h < 0 ? h + 360 : h;
}

/** Smallest angular distance between two hues, 0-180 degrees. */
export function hueDistance(a: RGB, b: RGB): number | null {
  const ha = hue(a);
  const hb = hue(b);
  if (ha === null || hb === null) return null;
  const diff = Math.abs(ha - hb) % 360;
  return diff > 180 ? 360 - diff : diff;
}
