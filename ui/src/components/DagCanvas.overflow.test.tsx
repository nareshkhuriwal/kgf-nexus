// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * A graph wider than its panel.
 *
 * Before: fit-to-view opened an eleven-layer run at 37%, unreadable, with a
 * yellow "part of the graph is outside the view" hint if the operator then
 * zoomed. Now the canvas opens at a readable scale on the first layer and
 * offers the ways out: a count of what is off-screen, a minimap that moves
 * the view, and edge fades on the sides that have more.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { beforeAll, describe, expect, it, vi } from 'vitest';
import { DagCanvas } from './DagCanvas';
import { NODE_H, NODE_W } from '../lib/dag';
import { READABLE_MIN } from '../lib/dagview';

const BOX = { width: 1500, height: 600 };

beforeAll(() => {
  Object.defineProperty(HTMLElement.prototype, 'clientWidth', {
    configurable: true,
    get() {
      return this.classList?.contains('dag-viewport') ? BOX.width : 0;
    },
  });
  Object.defineProperty(HTMLElement.prototype, 'clientHeight', {
    configurable: true,
    get() {
      return this.classList?.contains('dag-viewport') ? BOX.height : 0;
    },
  });
  // jsdom has no PointerEvent, so fireEvent.pointerDown would dispatch a bare
  // Event with no button and no coordinates. A MouseEvent carries both.
  vi.stubGlobal('PointerEvent', MouseEvent);
  vi.stubGlobal(
    'ResizeObserver',
    class {
      observe() {}
      disconnect() {}
    },
  );
});

/** Twelve layers in a row: 12 * (248 + 104) wide, far past a 1500px panel. */
const WIDE = Array.from({ length: 12 }, (_, i) => ({ id: `n${i}`, x: 32 + i * (NODE_W + 104), y: 32, tone: 'success' }));
const WIDTH = 32 * 2 + 12 * (NODE_W + 104) - 104;

function renderWide() {
  render(
    <DagCanvas width={WIDTH} height={NODE_H + 64} label="wide" graphKey="wide" places={WIDE}>
      <rect width={WIDTH} height={NODE_H + 64} />
    </DagCanvas>,
  );
}

describe('a graph wider than the panel', () => {
  it('opens at the readable floor, on the first layer, not at a fit', () => {
    renderWide();
    const transform = document.querySelector('.dag-zoom')!.getAttribute('transform')!;
    expect(transform).toContain(`scale(${READABLE_MIN})`);
    // Left-anchored: the translation is a small positive margin, not the
    // large negative one a centred oversize graph would have.
    const x = Number(/translate\(([-\d.]+),/.exec(transform)![1]);
    expect(x).toBeGreaterThan(0);
    expect(x).toBeLessThan(64);
    expect(screen.getByRole('button', { name: /^Zoom \d+%/ })).toHaveTextContent('75%');
  });

  it('says how many nodes are off-screen, in plain words', () => {
    renderWide();
    expect(screen.getByRole('status')).toHaveTextContent(/\d+ of 12 off-screen/);
  });

  it('fades the side that has more and not the side that does not', () => {
    renderWide();
    const viewport = document.querySelector('.dag-viewport')!;
    expect(viewport.className).toContain('overflow-right');
    expect(viewport.className).not.toContain('overflow-left');
  });

  it('offers a minimap, and clicking it moves the view there', () => {
    renderWide();
    const map = screen.getByRole('img', { name: /map of the graph/i });
    expect(map.querySelectorAll('.dag-minimap-node')).toHaveLength(12);
    const before = document.querySelector('.dag-zoom')!.getAttribute('transform');
    map.getBoundingClientRect = () => ({ left: 0, top: 0, width: 200, height: 10, right: 200, bottom: 10, x: 0, y: 0, toJSON() {} });
    // Click near the right end of the thumbnail: the far layers.
    fireEvent.pointerDown(map, { button: 0, clientX: 190, clientY: 5, pointerId: 1 });
    const after = document.querySelector('.dag-zoom')!.getAttribute('transform');
    expect(after).not.toBe(before);
    const x = Number(/translate\(([-\d.]+),/.exec(after!)![1]);
    expect(x).toBeLessThan(-1000);
    // Now the LEFT side has more, and the right may not.
    expect(document.querySelector('.dag-viewport')!.className).toContain('overflow-left');
  });

  it('still fits the whole graph on Fit', () => {
    renderWide();
    fireEvent.click(screen.getByRole('button', { name: 'Fit' }));
    const transform = document.querySelector('.dag-zoom')!.getAttribute('transform')!;
    const scale = Number(/scale\(([\d.]+)\)/.exec(transform)![1]);
    expect(scale).toBeLessThan(READABLE_MIN);
    expect(screen.queryByRole('img', { name: /map of the graph/i })).toBeNull();
  });
});

describe('a graph that fits', () => {
  it('shows no map and no off-screen count', () => {
    render(
      <DagCanvas width={600} height={200} label="small" graphKey="small" places={[{ id: 'a', x: 32, y: 32 }, { id: 'b', x: 384, y: 32 }]}>
        <rect width={600} height={200} />
      </DagCanvas>,
    );
    expect(screen.queryByRole('img', { name: /map of the graph/i })).toBeNull();
    expect(screen.queryByRole('status')).toBeNull();
  });
});
