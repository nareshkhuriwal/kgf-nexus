// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The mark has to survive being tinted, scaled and read by a screen reader. The
 * tests here are the properties that would silently break those, not the exact
 * path data.
 */

import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Logo, logoDataUri } from './Logo';

describe('the Nexus mark', () => {
  /**
   * currentColor is the whole reason this is inline SVG rather than a file: the
   * mark sits on the accent in the sidebar, on an inverted button foreground,
   * and on plain text in a loading row. A baked-in fill would need three copies
   * and would drift the first time the accent changed.
   */
  it('inherits the surrounding colour rather than baking one in', () => {
    const { container } = render(<Logo />);
    const svg = container.querySelector('svg');

    expect(svg).not.toBeNull();
    expect(svg!.innerHTML).toContain('currentColor');
    expect(svg!.innerHTML).not.toMatch(/#[0-9a-f]{3,6}/i);
  });

  it('scales from both axes off one size', () => {
    const { container } = render(<Logo size={48} />);
    const svg = container.querySelector('svg')!;

    expect(svg.getAttribute('width')).toBe('48');
    expect(svg.getAttribute('height')).toBe('48');
    // A fixed square viewBox is what lets the geometry stay identical at 16px
    // and 512px.
    expect(svg.getAttribute('viewBox')).toBe('0 0 24 24');
  });

  /**
   * The mark almost always sits beside the word "Nexus", so announcing it too
   * would read the brand twice. It becomes an image only when given a label,
   * which is the case where it stands alone.
   */
  it('is decorative by default and an image when labelled', () => {
    const { container: bare } = render(<Logo />);
    const decorative = bare.querySelector('svg')!;
    expect(decorative.getAttribute('aria-hidden')).toBe('true');
    expect(decorative.getAttribute('role')).toBeNull();

    const { container: labelled } = render(<Logo title="Nexus" />);
    const meaningful = labelled.querySelector('svg')!;
    expect(meaningful.getAttribute('role')).toBe('img');
    expect(meaningful.getAttribute('aria-label')).toBe('Nexus');
    expect(meaningful.getAttribute('aria-hidden')).toBeNull();
  });
});

describe('the serialised mark', () => {
  // currentColor cannot resolve outside a document, so the data-URI form has to
  // pin a colour explicitly or it renders invisible.
  it('pins a colour, because currentColor cannot resolve outside a document', () => {
    const uri = logoDataUri('#818cf8');

    expect(uri.startsWith('data:image/svg+xml,')).toBe(true);
    const decoded = decodeURIComponent(uri.slice('data:image/svg+xml,'.length));
    expect(decoded).toContain('#818cf8');
    expect(decoded).not.toContain('currentColor');
  });

  /**
   * The serialised form is cut for SMALL sizes, and these are the two properties
   * that make it legible there.
   *
   * The component's own numbers — a 1.6 stroke on a 24 viewBox, edges held back
   * to 0.55 opacity — work in a rail at 20px and up, and fail at 16px: the
   * strokes land on ~1.07 device pixels and the edges keep barely 0.6px of ink,
   * which rasterises to a grey smudge rather than a mark. So this form uses a 32
   * viewBox with heavier strokes and no opacity knockback.
   *
   * Asserted as ratios rather than literals so the geometry can be retuned; what
   * must not come back is sub-pixel line-work at a 16px render.
   */
  it('stays above one device pixel when rendered at 16px', () => {
    const decoded = decodeURIComponent(logoDataUri().slice('data:image/svg+xml,'.length));

    const viewBox = /viewBox="0 0 (\d+) \1"/.exec(decoded);
    expect(viewBox).not.toBeNull();
    const units = Number(viewBox![1]);

    const widths = [...decoded.matchAll(/stroke-width="([\d.]+)"/g)].map((m) => Number(m[1]));
    expect(widths.length).toBeGreaterThan(0);
    for (const w of widths) {
      expect((w * 16) / units).toBeGreaterThan(1);
    }
  });

  it('spends no opacity on de-emphasis, since there is none to spare at 16px', () => {
    const decoded = decodeURIComponent(logoDataUri().slice('data:image/svg+xml,'.length));

    expect(decoded).not.toContain('opacity');
  });
});
