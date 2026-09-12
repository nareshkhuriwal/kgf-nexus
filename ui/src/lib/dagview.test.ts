// Copyright 2026 Khuriwal Google
// SPDX-License-Identifier: Apache-2.0

/**
 * The viewport arithmetic, tested where it is cheap to test.
 *
 * Everything in lib/dagview is a pure function precisely so the two bugs that
 * matter here can be pinned without a browser: "the graph does not fit and the
 * view does not notice", and "the exported file came out unstyled". Both are
 * arithmetic-and-strings failures, and both are invisible in a jsdom render —
 * jsdom has no layout, so it cannot tell you a fit was wrong, and it will
 * happily serialise an SVG with no colour in it.
 */

import { describe, expect, it } from 'vitest';
import {
  ZOOM_MAX,
  ZOOM_MIN,
  centerOffset,
  clampZoom,
  exportFileName,
  fitScale,
  inlineComputedStyles,
  overflows,
  svgDocument,
  zoomAbout,
  zoomIn,
  zoomLabel,
  zoomOut,
} from './dagview';

describe('zoom clamping', () => {
  it('clamps to sensible limits rather than letting a graph vanish or explode', () => {
    // Below the floor a node box is narrower than its own label; above the
    // ceiling one node fills the panel and the structure is gone.
    expect(clampZoom(0.0001)).toBe(ZOOM_MIN);
    expect(clampZoom(500)).toBe(ZOOM_MAX);
    expect(clampZoom(1)).toBe(1);
  });

  it('treats a non-finite scale as 100% rather than propagating NaN', () => {
    // A NaN scale reaches the transform attribute and the whole graph
    // disappears with no error anywhere -- the worst failure mode available.
    expect(clampZoom(NaN)).toBe(1);
    expect(clampZoom(Infinity)).toBe(ZOOM_MAX);
  });

  it('steps multiplicatively, so a click feels the same size at both ends', () => {
    // An additive step would be a 33% jump at 0.3 and a 3% nudge at 3.
    const a = zoomIn(1);
    const b = zoomIn(2);
    expect(a / 1).toBeCloseTo(b / 2, 6);
    expect(zoomOut(zoomIn(1))).toBeCloseTo(1, 6);
  });

  it('stops at the limits instead of stepping past them', () => {
    let z = 1;
    for (let i = 0; i < 40; i += 1) z = zoomIn(z);
    expect(z).toBe(ZOOM_MAX);
    for (let i = 0; i < 40; i += 1) z = zoomOut(z);
    expect(z).toBe(ZOOM_MIN);
  });

  it('reads out a whole percentage, which is what the operator is told', () => {
    // The readout and the transform must agree; a readout of "100%" over a
    // scaled view is worse than no readout.
    expect(zoomLabel(1)).toBe('100%');
    expect(zoomLabel(0.5)).toBe('50%');
    expect(zoomLabel(0.7326)).toBe('73%');
  });
});

describe('fit to view', () => {
  /**
   * The reported bug, as arithmetic. A seven-layer run at NODE_W 196 +
   * GAP_X 104 is ~2048px wide; the panel is ~1500px. Before this, the SVG was
   * rendered at 2048 and the first and last node were off-screen.
   */
  it('scales a graph that overflows its panel down until it fits', () => {
    const graph = { width: 2048, height: 400 };
    const view = { width: 1500, height: 520 };
    expect(overflows(graph, view)).toBe(true);
    const s = fitScale(graph, view);
    expect(s).toBeLessThan(1);
    expect(graph.width * s).toBeLessThanOrEqual(view.width + 0.01);
    expect(graph.height * s).toBeLessThanOrEqual(view.height + 0.01);
  });

  it('fits on the tighter axis, so neither dimension is left clipped', () => {
    // A graph short and very wide must be limited by width; tall and narrow by
    // height. Taking one axis only is how a "fit" leaves content off-screen.
    expect(fitScale({ width: 2000, height: 100 }, { width: 1000, height: 1000 })).toBeCloseTo(0.5, 6);
    expect(fitScale({ width: 100, height: 2000 }, { width: 1000, height: 1000 })).toBeCloseTo(0.5, 6);
  });

  it('never magnifies a small graph to fill the panel', () => {
    // Capped at 1. A two-node graph blown up to 4x looks like a bug, and it
    // would mean a small run and a large one defaulting to different scales
    // with nothing on screen explaining why.
    expect(fitScale({ width: 200, height: 100 }, { width: 1500, height: 600 })).toBe(1);
  });

  it('goes below the button floor when a graph genuinely needs it', () => {
    // A 20-layer DAG needs ~0.2 to fit. Clamping the ARITHMETIC to ZOOM_MIN
    // would reinstate the original bug for the graphs that suffer it worst.
    const s = fitScale({ width: 9000, height: 400 }, { width: 1200, height: 600 });
    expect(s).toBeLessThan(ZOOM_MIN);
    expect(9000 * s).toBeCloseTo(1200, 6);
  });

  it('reports "fits" rather than 0 when the container has not been measured', () => {
    // Pre-layout and in jsdom the container is 0x0. A 0 scale would blank the
    // canvas; "1, and do not auto-fit" shows content, which is the old
    // behaviour rather than a new failure.
    expect(fitScale({ width: 2048, height: 400 }, { width: 0, height: 0 })).toBe(1);
    expect(overflows({ width: 2048, height: 400 }, { width: 0, height: 0 })).toBe(false);
  });

  it('tolerates a few pixels of overflow instead of flapping', () => {
    // Without slack, sub-pixel container measurements make "does it fit" toggle
    // between renders and re-trigger the auto-fit.
    expect(overflows({ width: 1502, height: 300 }, { width: 1500, height: 600 })).toBe(false);
    expect(overflows({ width: 1600, height: 300 }, { width: 1500, height: 600 })).toBe(true);
  });

  it('centres the fitted graph rather than pinning it to a corner', () => {
    const off = centerOffset({ width: 1000, height: 200 }, { width: 1500, height: 600 }, 1);
    expect(off.x).toBeCloseTo(250, 6);
    expect(off.y).toBeCloseTo(200, 6);
  });
});

describe('zoom about a point', () => {
  /**
   * The gesture that separates zoom-that-aims from zoom-that-guesses: the graph
   * coordinate under the cursor must not move while zooming toward it.
   */
  it('keeps the graph point under the cursor fixed', () => {
    const before = { scale: 1, x: 0, y: 0 };
    const pointer = { x: 400, y: 150 };
    const graphPoint = { x: (pointer.x - before.x) / before.scale, y: (pointer.y - before.y) / before.scale };

    const after = zoomAbout(before, 2, pointer);
    // Where does that same graph point land now?
    expect(after.x + graphPoint.x * after.scale).toBeCloseTo(pointer.x, 6);
    expect(after.y + graphPoint.y * after.scale).toBeCloseTo(pointer.y, 6);
  });

  it('holds the anchor across a chain of zooms, not just one', () => {
    let v = { scale: 0.6, x: -120, y: 40 };
    const pointer = { x: 300, y: 200 };
    const gx = (pointer.x - v.x) / v.scale;
    for (const s of [0.9, 1.4, 2.2, 1.1]) v = zoomAbout(v, s, pointer);
    expect(v.x + gx * v.scale).toBeCloseTo(pointer.x, 5);
  });

  it('clamps through the anchored path too', () => {
    const v = zoomAbout({ scale: 1, x: 0, y: 0 }, 99, { x: 10, y: 10 });
    expect(v.scale).toBe(ZOOM_MAX);
  });
});

describe('export filenames', () => {
  it('identifies the run, so a download is not an anonymous file', () => {
    expect(exportFileName('run-abc123', 'svg')).toBe('nexus-run-abc123-dag.svg');
    expect(exportFileName('run-abc123', 'png')).toBe('nexus-run-abc123-dag.png');
  });

  it('strips characters that break a download or silently rename the file', () => {
    // Spec names are user-supplied and can carry slashes, colons and spaces.
    expect(exportFileName('etl/daily: v2', 'svg')).toBe('nexus-etl-daily-v2-dag.svg');
  });

  it('still names the file when there is nothing to name it after', () => {
    expect(exportFileName(undefined, 'svg')).toBe('nexus-graph-dag.svg');
    expect(exportFileName('///', 'svg')).toBe('nexus-graph-dag.svg');
  });
});

/**
 * Export fidelity.
 *
 * THE FAILURE THIS PREVENTS is specific and was easy to ship: the live graph is
 * coloured entirely by dag.css and by --tone-* custom properties resolved
 * against the theme on <html>. A naive outerHTML serialise carries the class
 * names but neither the stylesheet nor the variables, so the operator opens a
 * file in which FAILED and SUCCEEDED are the same default-black rectangle — an
 * export that looks like it worked and destroyed the only information it
 * existed to carry.
 *
 * "A download was triggered" would pass over exactly that bug, so these tests
 * assert on the SERIALISED BYTES instead: the fills and strokes must be in
 * them.
 */
describe('exported markup carries the appearance, not just the shape', () => {
  /** A fake computed-style source, keyed by class, standing in for the real
   *  cascade — which jsdom does not compute for var() anyway. */
  const fakeStyles: Record<string, Record<string, string>> = {
    'dag-node': { fill: 'rgb(20, 30, 40)', stroke: 'rgb(248, 81, 73)', 'stroke-width': '1.5px' },
    'dag-edge': { fill: 'none', stroke: 'rgb(74, 85, 99)', opacity: '0.75' },
    'dag-node-label': { fill: 'rgb(230, 237, 243)', 'font-size': '13px', 'font-weight': '600' },
  };
  const reader = (el: Element) => {
    const cls = el.getAttribute('class') ?? '';
    const found = Object.keys(fakeStyles).find((k) => cls.split(/\s+/).includes(k));
    const props = found ? fakeStyles[found]! : {};
    return { getPropertyValue: (p: string) => props[p] ?? '' };
  };

  function tree() {
    const wrap = document.createElement('div');
    wrap.innerHTML =
      '<svg><g class="dag-zoom" transform="translate(10,20) scale(0.5)">' +
      '<path class="dag-edge" d="M 0 0 L 10 10"/>' +
      '<g class="dag-node-g tone-danger"><rect class="dag-node"/>' +
      '<text class="dag-node-label">extract</text></g>' +
      '</g></svg>';
    return wrap.querySelector('svg')!;
  }

  it('inlines the fills and strokes the stylesheet would otherwise have owned', () => {
    const live = tree();
    const clone = live.cloneNode(true) as SVGSVGElement;
    inlineComputedStyles(live, clone, reader);
    const out = new XMLSerializer().serializeToString(clone);

    // The node's state colours, which are the whole content of the export.
    expect(out).toContain('fill:rgb(20, 30, 40)');
    expect(out).toContain('stroke:rgb(248, 81, 73)');
    // The edge, including fill:none -- which is meaningful and must survive.
    expect(out).toContain('fill:none');
    expect(out).toContain('stroke:rgb(74, 85, 99)');
    // And the label's type, so the text is not rendered at a viewer default.
    expect(out).toContain('font-size:13px');
  });

  it('leaves no element styled only by a class name', () => {
    // The regression guard: if this ever passes vacuously the export is back to
    // being an unstyled black-and-white graph.
    const live = tree();
    const clone = live.cloneNode(true) as SVGSVGElement;
    inlineComputedStyles(live, clone, reader);
    for (const cls of ['dag-node', 'dag-edge', 'dag-node-label']) {
      const el = clone.querySelector(`.${cls}`)!;
      expect(el.getAttribute('style'), `${cls} lost its appearance`).toMatch(/fill|stroke/);
    }
  });

  it('walks the whole tree, not only the top level', () => {
    // The nodes are nested two groups deep; a shallow walk would style the
    // edges and leave every node box bare.
    const live = tree();
    const clone = live.cloneNode(true) as SVGSVGElement;
    inlineComputedStyles(live, clone, reader);
    expect(clone.querySelector('.dag-node-label')?.getAttribute('style')).toContain('fill:');
  });

  it('wraps the fragment into a file a viewer outside the app can open', () => {
    const doc = svgDocument('<rect class="dag-node"/>', {
      width: 2048,
      height: 400,
      viewBox: '0 0 2048 400',
      background: 'rgb(1, 4, 9)',
    });
    // Without the namespace a browser renders the file as an XML tree rather
    // than a picture -- and only outside the app that produced it.
    expect(doc).toContain('xmlns="http://www.w3.org/2000/svg"');
    // Intrinsic size, not the operator's current zoom: the file is the graph,
    // not a photo of their viewport.
    expect(doc).toContain('width="2048"');
    expect(doc).toContain('viewBox="0 0 2048 400"');
    // An SVG has no canvas colour of its own, so a dark-theme graph without an
    // explicit ground arrives as pale text on the viewer's white default.
    expect(doc).toContain('fill="rgb(1, 4, 9)"');
    expect(doc.startsWith('<svg')).toBe(true);
    expect(doc.endsWith('</svg>')).toBe(true);
  });
});
