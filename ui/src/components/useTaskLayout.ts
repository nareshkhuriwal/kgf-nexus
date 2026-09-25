// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { useEffect, useMemo, useRef, useState, type PointerEvent, type SVGProps } from 'react';
import { NODE_H, NODE_W, type Placement } from '../lib/dag';

type Positions = Record<string, { x: number; y: number }>;
function read(key: string): Positions {
  try {
    const data = JSON.parse(localStorage.getItem(key) ?? '{}');
    return Object.fromEntries(Object.entries(data).filter(([, value]) => {
      const p = value as { x: number; y: number };
      return p && Number.isFinite(p.x) && Number.isFinite(p.y) && p.x >= 16 && p.y >= 16;
    })) as Positions;
  } catch { return {}; }
}

/** Visual positions only. Workflow definitions and execution order are unchanged. */
export function useTaskLayout<T extends Placement>(base: T[], identity: string) {
  const key = `nexus.task-layout.v1:${identity}`;
  const initial = useMemo(() => read(key), [key]);
  const [stored, setStored] = useState({ key, positions: initial });
  const positions = stored.key === key ? stored.positions : initial;
  const current = useRef(positions);
  current.current = positions;
  const [message, setMessage] = useState('Drag cards to arrange · Shift + arrow keys to move');
  const drag = useRef<{ id: string; pointer: number; x: number; y: number; start: { x: number; y: number }; scaleX: number; scaleY: number; original: Positions; moved: boolean } | null>(null);
  const [revision, setRevision] = useState(0);
  useEffect(() => { drag.current = null; }, [key]);
  const suppressClick = useRef<string | null>(null);
  const places = base.map(p => ({ ...p, ...positions[p.id] }));
  const byId = Object.fromEntries(places.map(p => [p.id, p]));
  const update = (next: Positions) => { current.current = next; setStored({ key, positions: next }); };
  const save = () => {
    try {
      // Only retain nodes present in this graph.
      localStorage.setItem(key, JSON.stringify(Object.fromEntries(base.filter(p => current.current[p.id]).map(p => [p.id, current.current[p.id]]))));
      setMessage('Layout saved in this browser');
    } catch { setMessage('Layout kept for this session — browser storage unavailable'); }
  };
  const end = (event: PointerEvent<SVGGElement>, cancel = false) => {
    const d = drag.current;
    if (!d || d.pointer !== event.pointerId) return;
    event.stopPropagation();
    drag.current = null;
    if (d.moved) {
      suppressClick.current = d.id;
      if (cancel) update(d.original); else save();
    }
    if (event.currentTarget.hasPointerCapture?.(event.pointerId)) event.currentTarget.releasePointerCapture(event.pointerId);
  };
  const bindings = (id: string): SVGProps<SVGGElement> => ({
    'aria-description': 'Drag to reposition. Shift plus arrow keys moves this card.',
    onPointerDown: event => {
      const point = byId[id];
      if (event.button !== 0 || !point) return;
      // A control INSIDE the card (the map's fold/unfold toggle) is not a drag
      // handle. Capturing the pointer here would retarget its click to the
      // card, so in a real browser the toggle selected the node instead of
      // unfolding it; jsdom's synthetic clicks never showed that.
      const control = (event.target as Element).closest?.('[role="button"]');
      if (control && control !== event.currentTarget) return;
      event.stopPropagation();
      suppressClick.current = null;
      const matrix = event.currentTarget.closest('svg')?.querySelector<SVGGElement>('.dag-zoom')?.getScreenCTM?.();
      drag.current = { id, pointer: event.pointerId, x: event.clientX, y: event.clientY,
        start: point, original: current.current, moved: false,
        scaleX: matrix ? Math.hypot(matrix.a, matrix.b) || 1 : 1,
        scaleY: matrix ? Math.hypot(matrix.c, matrix.d) || 1 : 1 };
      event.currentTarget.setPointerCapture?.(event.pointerId);
    },
    onPointerMove: event => {
      const d = drag.current;
      if (!d || d.pointer !== event.pointerId) return;
      event.stopPropagation();
      const dx = event.clientX - d.x, dy = event.clientY - d.y;
      if (!d.moved && Math.hypot(dx, dy) < 4) return;
      d.moved = true;
      update({ ...current.current, [id]: { x: Math.max(16, d.start.x + dx / d.scaleX), y: Math.max(16, d.start.y + dy / d.scaleY) } });
    },
    onPointerUp: event => end(event),
    onPointerCancel: event => end(event, true),
    onLostPointerCapture: event => end(event),
    onClickCapture: event => {
      if (suppressClick.current === id) { event.stopPropagation(); event.preventDefault(); suppressClick.current = null; }
    },
    onKeyDownCapture: event => {
      if (!event.shiftKey || !['ArrowLeft', 'ArrowRight', 'ArrowUp', 'ArrowDown'].includes(event.key)) return;
      event.preventDefault(); event.stopPropagation();
      const point = byId[id];
      if (!point) return;
      update({ ...current.current, [id]: {
        x: Math.max(16, point.x + (event.key === 'ArrowRight' ? 16 : event.key === 'ArrowLeft' ? -16 : 0)),
        y: Math.max(16, point.y + (event.key === 'ArrowDown' ? 16 : event.key === 'ArrowUp' ? -16 : 0)),
      } });
      save();
    },
  });
  return { places, byId, bindings, message, revision,
    width: Math.max(64, ...places.map(p => p.x + NODE_W + 32)),
    height: Math.max(64, ...places.map(p => p.y + NODE_H + 32)),
    reset: () => { update({}); save(); setRevision(value => value + 1); setMessage('Automatic layout restored'); },
  };
}
