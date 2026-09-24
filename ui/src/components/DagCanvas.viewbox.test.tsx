// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The canvas's coordinate space.
 *
 * The reported bug: a single-node run rendered as one enormous box filling the
 * whole panel, with the zoom readout still saying 100%. The readout was honest
 * — the zoom group really was at scale 1 — but the SVG underneath mapped a
 * viewBox of the GRAPH's extent (196x64) onto a 100%-sized element with
 * preserveAspectRatio="none", which is a non-uniform fit: about 9x across and
 * 11x down. Two scales were being composed and only one of them was reported.
 *
 * So what is pinned here is that the viewBox describes the CONTAINER, making
 * one user unit one CSS pixel, which is what makes "100%" mean intrinsic size.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { beforeAll, describe, expect, it, vi } from 'vitest';
import { DagCanvas } from './DagCanvas';

/**
 * jsdom reports every element as 0x0 and has no ResizeObserver, so the
 * component would never leave its pre-measurement branch. Both are stubbed to
 * give the container a real size — which is the whole subject of these tests.
 */
const BOX = { width: 1700, height: 700 };

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
  vi.stubGlobal(
    'ResizeObserver',
    class {
      observe() {}
      disconnect() {}
    },
  );
});


function renderCanvas(width: number, height: number) {
  render(
    <DagCanvas width={width} height={height} label="1 node" graphKey={`g-${width}x${height}`}>
      <rect width={width} height={height} />
    </DagCanvas>,
  );
  return document.querySelector('svg')!;
}

describe('the DAG canvas coordinate space', () => {
  it('sizes the viewBox to the container, so one unit is one pixel', () => {
    // A single 196x64 node — the case that rendered ~9x too wide.
    const svg = renderCanvas(196, 64);
    expect(svg.getAttribute('viewBox')).toBe(`0 0 ${BOX.width} ${BOX.height}`);
  });

  it('does not stretch content non-uniformly', () => {
    const svg = renderCanvas(196, 64);
    // "none" is what made a small graph fill the panel in both axes at once.
    expect(svg.getAttribute('preserveAspectRatio')).not.toBe('none');
  });

  it('leaves a small graph at scale 1 rather than magnifying it to fit', () => {
    const svg = renderCanvas(196, 64);
    const g = svg.querySelector('g.dag-zoom')!;
    // Center the small graph without magnifying it.
    expect(g.getAttribute('transform')).toBe('translate(752,318) scale(1)');
  });
});


it('expands the workspace and returns to its content-aware height', () => {
  renderCanvas(1200, 152);
  const viewport = document.querySelector('.dag-viewport') as HTMLElement;
  expect(viewport.style.height).toBe('280px');
  fireEvent.click(screen.getByRole('button', { name: 'Expand view' }));
  expect(viewport).toHaveClass('expanded');
  fireEvent.click(screen.getByRole('button', { name: 'Compact view' }));
  expect(viewport).not.toHaveClass('expanded');
  expect(viewport.style.height).toBe('280px');
});

it('keeps the operator zoom across a status-only rerender', () => {
  const result = render(<DagCanvas width={196} height={64} graphKey="stable" label="graph"><rect /></DagCanvas>);
  fireEvent.click(screen.getByRole('button', { name: 'Zoom in' }));
  const transform = document.querySelector('.dag-zoom')?.getAttribute('transform');
  result.rerender(<DagCanvas width={196} height={64} graphKey="stable" label="graph updated"><rect /></DagCanvas>);
  expect(document.querySelector('.dag-zoom')?.getAttribute('transform')).toBe(transform);
});
