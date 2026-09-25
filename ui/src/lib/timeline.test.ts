// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Layout maths of the execution timeline. The properties that matter to an
 * operator: phases are split only where a timestamp proves the split, a live
 * attempt grows to now and a finished one never does, a fan-out groups under
 * its template in item order, zoom keeps the pointed-at instant still, and the
 * virtual window mounts a bounded number of rows however large the run.
 */

import { describe, expect, it } from 'vitest';
import type { Attempt, RunNode } from '../api/types';
import {
  MIN_SPAN_MS,
  attemptPhases,
  barExtent,
  buildTimelineRows,
  clampWindow,
  isFullWindow,
  panWindow,
  scrollToRow,
  tOf,
  tickStep,
  timeDomain,
  timeTicks,
  virtualRange,
  xOf,
  zoomWindow,
  type AttemptRow,
  type GroupRow,
} from './timeline';

const T0 = Date.parse('2026-09-25T10:00:00Z');
const iso = (ms: number) => new Date(T0 + ms).toISOString();

function attempt(p: Partial<Attempt> & { node_id: string }): Attempt {
  return {
    id: `${p.node_id}#${p.attempt_number ?? 1}`,
    run_id: 'r',
    attempt_number: 1,
    state: 'SUCCEEDED',
    ...p,
  } as Attempt;
}

const node = (node_id: string, extra: Partial<RunNode> = {}): RunNode => ({
  run_id: 'r',
  node_id,
  task_ref: `t/${node_id}`,
  state: 'SUCCEEDED',
  ...extra,
});

describe('attemptPhases', () => {
  it('splits a finished attempt into queued, starting and running', () => {
    const s = attemptPhases(
      attempt({ node_id: 'a', scheduled_at: iso(0), dispatched_at: iso(2000), started_at: iso(2500), ended_at: iso(10_000) }),
      T0 + 99_999,
    );
    expect(s.phases.map((p) => [p.kind, p.from - T0, p.to - T0, p.open])).toEqual([
      ['queued', 0, 2000, false],
      ['starting', 2000, 2500, false],
      ['running', 2500, 10_000, false],
    ]);
    expect([s.queuedMs, s.startingMs, s.runningMs, s.totalMs]).toEqual([2000, 500, 7500, 10_000]);
    expect(s.open).toBe(false);
  });

  it('grows the open phase of a live attempt to now', () => {
    const now = T0 + 7000;
    const running = attemptPhases(
      attempt({ node_id: 'a', state: 'RUNNING', scheduled_at: iso(0), dispatched_at: iso(1000), started_at: iso(1500) }),
      now,
    );
    expect(running.phases.at(-1)).toMatchObject({ kind: 'running', to: now, open: true });
    expect(running.runningMs).toBe(5500);

    const queued = attemptPhases(attempt({ node_id: 'a', state: 'PENDING', scheduled_at: iso(0) }), now);
    expect(queued.phases).toEqual([{ kind: 'queued', from: T0, to: now, open: true }]);

    const dispatched = attemptPhases(
      attempt({ node_id: 'a', state: 'DISPATCHED', scheduled_at: iso(0), dispatched_at: iso(4000) }),
      now,
    );
    expect(dispatched.phases.map((p) => [p.kind, p.open])).toEqual([
      ['queued', false],
      ['starting', true],
    ]);
  });

  it('never stretches a terminal attempt to now', () => {
    const cancelledQueued = attemptPhases(
      attempt({ node_id: 'a', state: 'CANCELLED', scheduled_at: iso(0), ended_at: iso(3000) }),
      T0 + 1e9,
    );
    expect(cancelledQueued.phases).toEqual([{ kind: 'queued', from: T0, to: T0 + 3000, open: false }]);
    expect(cancelledQueued.startingMs).toBeNull();
    expect(cancelledQueued.runningMs).toBeNull();
  });

  it('does not invent a dispatch time an older gateway did not report', () => {
    const s = attemptPhases(
      attempt({ node_id: 'a', scheduled_at: iso(0), started_at: iso(3000), ended_at: iso(4000) }),
      T0,
    );
    expect(s.phases.map((p) => p.kind)).toEqual(['queued', 'running']);
    expect(s.phases[0]).toMatchObject({ to: T0 + 3000, inferred: true });
  });

  it('clamps a skewed clock rather than drawing a bar backwards', () => {
    const s = attemptPhases(
      attempt({ node_id: 'a', scheduled_at: iso(5000), dispatched_at: iso(4000), started_at: iso(4500), ended_at: iso(6000) }),
      T0,
    );
    for (const p of s.phases) expect(p.to).toBeGreaterThanOrEqual(p.from);
  });

  it('yields no phases for an attempt with no timestamps', () => {
    const s = attemptPhases(attempt({ node_id: 'a', state: 'PENDING' }), T0);
    expect(s.phases).toEqual([]);
    expect(s.start).toBeNull();
  });
});

describe('buildTimelineRows', () => {
  const nodes = [
    node('summarize'),
    node('list'),
    node('map'),
    node('map[1]', { parent_node_id: 'map', map_index: 1 }),
    node('map[0]', { parent_node_id: 'map', map_index: 0 }),
    node('never', { state: 'UPSTREAM_FAILED' }),
  ];
  const attempts = [
    attempt({ node_id: 'summarize', scheduled_at: iso(9000), ended_at: iso(9500) }),
    attempt({ node_id: 'list', scheduled_at: iso(0), ended_at: iso(1000) }),
    attempt({ node_id: 'map[1]', scheduled_at: iso(2000), ended_at: iso(5000), state: 'FAILED' }),
    attempt({ node_id: 'map[1]', attempt_number: 2, scheduled_at: iso(6000), ended_at: iso(8000) }),
    attempt({ node_id: 'map[0]', scheduled_at: iso(2000), ended_at: iso(3000) }),
  ];

  it('orders nodes by when they first ran, children by map index, retries in order', () => {
    const rows = buildTimelineRows(nodes, attempts, new Set(), T0);
    expect(rows.map((r) => r.key)).toEqual([
      'list#1',
      'group:map',
      'map[0]#1',
      'map[1]#1',
      'map[1]#2',
      'summarize#1',
      'idle:never',
    ]);
    const retry = rows.find((r) => r.key === 'map[1]#2') as AttemptRow;
    expect(retry).toMatchObject({ depth: 1, firstOfNode: false, template: 'map' });
  });

  it('summarises a group and hides its children when collapsed', () => {
    const rows = buildTimelineRows(nodes, attempts, new Set(['map']), T0);
    expect(rows.map((r) => r.key)).toEqual(['list#1', 'group:map', 'summarize#1', 'idle:never']);
    const g = rows[1] as GroupRow;
    expect(g).toMatchObject({ childCount: 2, attemptCount: 3, collapsed: true, start: T0 + 2000, end: T0 + 8000 });
    // Worst outcome among the children wins the envelope's colour.
    expect(g.tone).toBe('danger');
  });

  it('scales to a fan-out of thousands', () => {
    const kids = Array.from({ length: 3000 }, (_, i) => node(`m[${i}]`, { parent_node_id: 'm', map_index: i }));
    const many = kids.map((k, i) => attempt({ node_id: k.node_id, scheduled_at: iso(i), ended_at: iso(i + 10) }));
    const t = performance.now();
    const rows = buildTimelineRows([node('m'), ...kids], many, new Set(), T0);
    expect(rows).toHaveLength(3001);
    expect(performance.now() - t).toBeLessThan(1000);
  });
});

describe('time domain and ticks', () => {
  it('spans every bar with padding, and reaches now for a live run', () => {
    const rows = buildTimelineRows(
      [node('a')],
      [attempt({ node_id: 'a', scheduled_at: iso(0), ended_at: iso(10_000) })],
      new Set(),
      T0,
    );
    const done = timeDomain(rows, { now: T0 + 60_000, live: false });
    expect(done.start).toBeLessThan(T0);
    expect(done.end).toBeGreaterThan(T0 + 10_000);
    expect(done.end).toBeLessThan(T0 + 20_000);
    const live = timeDomain(rows, { now: T0 + 60_000, live: true });
    expect(live.end).toBeGreaterThanOrEqual(T0 + 60_000);
  });

  it('gives an empty run a meaningful window', () => {
    const d = timeDomain([], { now: T0, live: true });
    expect(d.end - d.start).toBeGreaterThan(0);
  });

  it('picks round steps', () => {
    expect(tickStep({ start: 0, end: 60_000 }, 6)).toBe(10_000);
    expect(tickStep({ start: 0, end: 3_600_000 }, 8)).toBe(600_000);
    expect(tickStep({ start: 0, end: 900 }, 9)).toBe(100);
  });

  it('labels ticks with the local wall clock at round instants', () => {
    const w = { start: T0 + 1234, end: T0 + 61_000 };
    const ticks = timeTicks(w, 6);
    expect(ticks.length).toBeGreaterThan(2);
    expect(ticks.length).toBeLessThanOrEqual(7);
    for (const t of ticks) {
      expect(t.t).toBeGreaterThanOrEqual(w.start);
      expect(t.t % 10_000).toBe(0);
      expect(t.label).toMatch(/^\d\d:\d\d:\d\d$/);
    }
    const sub = timeTicks({ start: T0, end: T0 + 500 }, 5);
    expect(sub[0]!.label).toMatch(/\.\d{3}$/);
  });
});

describe('zoom and pan', () => {
  const domain = { start: 0, end: 100_000 };

  it('keeps the anchored instant under the pointer while zooming', () => {
    const w = { start: 0, end: 100_000 };
    const z = zoomWindow(w, 0.5, 25_000, domain);
    expect(z.end - z.start).toBe(50_000);
    // 25% across before, 25% across after.
    expect((25_000 - z.start) / (z.end - z.start)).toBeCloseTo(0.25);
  });

  it('never zooms past the domain or below the minimum span', () => {
    expect(zoomWindow({ start: 0, end: 100_000 }, 4, 50_000, domain)).toEqual(domain);
    const tiny = zoomWindow({ start: 0, end: 30 }, 0.01, 10, domain);
    expect(tiny.end - tiny.start).toBe(MIN_SPAN_MS);
  });

  it('pans within the domain', () => {
    const w = { start: 10_000, end: 20_000 };
    expect(panWindow(w, 5000, domain)).toEqual({ start: 15_000, end: 25_000 });
    expect(panWindow(w, -50_000, domain)).toEqual({ start: 0, end: 10_000 });
    expect(panWindow(w, 500_000, domain)).toEqual({ start: 90_000, end: 100_000 });
  });

  it('knows when the window is the whole run', () => {
    expect(isFullWindow(domain, domain)).toBe(true);
    expect(isFullWindow({ start: 1000, end: 100_000 }, domain)).toBe(false);
    expect(clampWindow({ start: -5, end: 1e9 }, domain)).toEqual(domain);
  });

  it('maps time to pixels and back', () => {
    const w = { start: 1000, end: 3000 };
    expect(xOf(2000, w, 400)).toBe(200);
    expect(tOf(200, w, 400)).toBe(2000);
  });

  it('clips bars to the track and keeps tiny ones visible', () => {
    const w = { start: 0, end: 1000 };
    expect(barExtent(-500, 250, w, 100)).toEqual({ left: 0, width: 25 });
    expect(barExtent(2000, 3000, w, 100)).toBeNull();
    expect(barExtent(500, 500.001, w, 100)!.width).toBeGreaterThanOrEqual(2);
  });
});

describe('virtual window', () => {
  it('mounts only the rows in view plus overscan', () => {
    const r = virtualRange(28 * 1000, 440, 28, 5000, 8);
    expect(r.first).toBe(992);
    expect(r.last - r.first + 1).toBeLessThan(40);
    expect(virtualRange(0, 440, 28, 3)).toEqual({ first: 0, last: 2 });
    expect(virtualRange(0, 440, 28, 0)).toEqual({ first: 0, last: -1 });
  });

  it('scrolls minimally to reveal a row', () => {
    expect(scrollToRow(0, 0, 280, 28)).toBeNull();
    expect(scrollToRow(20, 0, 280, 28)).toBe(21 * 28 - 280);
    expect(scrollToRow(2, 280, 280, 28)).toBe(56);
  });
});
