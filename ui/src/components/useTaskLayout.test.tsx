// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { useTaskLayout } from './useTaskLayout';
import { edgePath } from '../lib/dag';

const select = vi.fn();
const base = [{ id: 'a', x: 32, y: 32 }, { id: 'b', x: 400, y: 32 }];
function Harness() {
  const layout = useTaskLayout(base, 'test-run');
  return <><button onClick={layout.reset}>Reset layout</button><span>{layout.message}</span>
    <svg><g className="dag-zoom"><path data-testid="edge" d={edgePath(layout.places[0]!, layout.places[1]!)} />
      {layout.places.map(p => <g key={p.id} {...layout.bindings(p.id)} role="button" aria-label={p.id} onClick={select} transform={`translate(${p.x},${p.y})`}><g role="button" aria-label={`toggle ${p.id}`} /></g>)}
    </g></svg></>;
}
beforeEach(() => {
  localStorage.clear(); select.mockClear();
  class TestPointerEvent extends MouseEvent {
    pointerId: number;
    constructor(type: string, init: PointerEventInit = {}) { super(type, init); this.pointerId = init.pointerId ?? 1; }
  }
  vi.stubGlobal('PointerEvent', TestPointerEvent);
  Object.defineProperty(SVGElement.prototype, 'getScreenCTM', { configurable: true, value: () => ({ a: 2, b: 0, c: 0, d: 2 }) });
});
afterEach(() => { vi.unstubAllGlobals(); delete (SVGElement.prototype as unknown as { getScreenCTM?: unknown }).getScreenCTM; });

it('moves at the correct zoom, updates edges, suppresses selection, and restores after remount', () => {
  const view = render(<Harness />);
  const a = screen.getByRole('button', { name: 'a' });
  const oldEdge = screen.getByTestId('edge').getAttribute('d');
  fireEvent.pointerDown(a, { button: 0, clientX: 100, clientY: 100 });
  fireEvent.pointerMove(a, { clientX: 180, clientY: 160 });
  fireEvent.pointerUp(a);
  fireEvent.click(a);
  expect(a).toHaveAttribute('transform', 'translate(72,62)');
  expect(screen.getByTestId('edge').getAttribute('d')).not.toBe(oldEdge);
  expect(select).not.toHaveBeenCalled();
  view.unmount(); render(<Harness />);
  expect(screen.getByRole('button', { name: 'a' })).toHaveAttribute('transform', 'translate(72,62)');
});
it('preserves clicks below the drag threshold and cancels interrupted drags', () => {
  render(<Harness />);
  const a = screen.getByRole('button', { name: 'a' });
  fireEvent.pointerDown(a, { button: 0, clientX: 100, clientY: 100 });
  fireEvent.pointerMove(a, { clientX: 102, clientY: 100 });
  fireEvent.pointerUp(a); fireEvent.click(a);
  expect(select).toHaveBeenCalledOnce();
  fireEvent.pointerDown(a, { button: 0, clientX: 100, clientY: 100 });
  fireEvent.pointerMove(a, { clientX: 200, clientY: 200 });
  fireEvent.pointerCancel(a);
  expect(a).toHaveAttribute('transform', 'translate(32,32)');
});
it('supports keyboard repositioning and resetting saved positions', () => {
  render(<Harness />);
  const a = screen.getByRole('button', { name: 'a' });
  fireEvent.keyDown(a, { key: 'ArrowRight', shiftKey: true });
  expect(a).toHaveAttribute('transform', 'translate(48,32)');
  fireEvent.click(screen.getByRole('button', { name: 'Reset layout' }));
  expect(a).toHaveAttribute('transform', 'translate(32,32)');
  expect(localStorage.getItem('nexus.task-layout.v1:test-run')).toBe('{}');
});
it('never captures the pointer for a control inside the card, so its click stays its own', () => {
  const capture = vi.fn();
  Object.defineProperty(SVGElement.prototype, 'setPointerCapture', { configurable: true, value: capture });
  try {
    render(<Harness />);
    fireEvent.pointerDown(screen.getByRole('button', { name: 'toggle a' }), { button: 0, clientX: 100, clientY: 100 });
    expect(capture).not.toHaveBeenCalled();
    fireEvent.pointerDown(screen.getByRole('button', { name: 'a' }), { button: 0, clientX: 100, clientY: 100 });
    expect(capture).toHaveBeenCalledOnce();
  } finally {
    delete (SVGElement.prototype as unknown as { setPointerCapture?: unknown }).setPointerCapture;
  }
});
