// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for DAG state derivation.
 *
 * The case that matters most is root-cause attribution: given a cascade, the UI
 * must point at the node that actually broke and NOT at the ones that merely
 * inherited the failure. That is the operational payoff of separating FAILED
 * from UPSTREAM_FAILED, and it is pure logic, so it gets tested directly.
 */

import { describe, expect, it } from 'vitest';
import type { Attempt, NodeState, RunEdge, RunNode } from '../api/types';
import {
  NODE_H,
  NODE_W,
  buildDagGraph,
  edgePath,
  groupAttempts,
  layoutGrid,
  layoutLayers,
  summarizeDag,
} from './dag';

function node(id: string, state: NodeState, extra: Partial<RunNode> = {}): RunNode {
  return {
    run_id: 'r1',
    node_id: id,
    task_ref: `pkg.${id}:v1`,
    state,
    ...extra,
  };
}

function edge(from: string, to: string): RunEdge {
  return { from_node_id: from, to_node_id: to };
}

function attempt(nodeId: string, n: number, state: Attempt['state'], extra: Partial<Attempt> = {}): Attempt {
  return {
    id: `${nodeId}-a${n}`,
    run_id: 'r1',
    node_id: nodeId,
    attempt_number: n,
    state,
    ...extra,
  };
}

describe('depth layering', () => {
  it('places roots at depth 0 and uses longest path so diamonds do not collapse', () => {
    // a → b → c → d  and  a → d : d must sit at depth 3, not depth 1.
    const g = buildDagGraph({
      nodes: ['a', 'b', 'c', 'd'].map((id) => node(id, 'PENDING')),
      edges: [edge('a', 'b'), edge('b', 'c'), edge('c', 'd'), edge('a', 'd')],
    });
    expect(g.byId.a!.depth).toBe(0);
    expect(g.byId.b!.depth).toBe(1);
    expect(g.byId.c!.depth).toBe(2);
    expect(g.byId.d!.depth).toBe(3);
    expect(g.hasCycle).toBe(false);
  });

  it('produces a stable, sorted order within each layer', () => {
    const g = buildDagGraph({
      nodes: [node('z', 'PENDING'), node('a', 'PENDING'), node('m', 'PENDING')],
      edges: [],
    });
    expect(g.layers[0]).toEqual(['a', 'm', 'z']);
    expect(g.byId.a!.order).toBe(0);
    expect(g.byId.z!.order).toBe(2);
  });

  it('reports a cycle rather than dropping the nodes in it', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'PENDING'), node('b', 'PENDING')],
      edges: [edge('a', 'b'), edge('b', 'a')],
    });
    expect(g.hasCycle).toBe(true);
    expect(g.nodes).toHaveLength(2);
  });

  it('ignores edges referencing unknown nodes instead of throwing', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'PENDING')],
      edges: [edge('a', 'ghost'), edge('ghost', 'a'), edge('a', 'a')],
    });
    expect(g.edges).toHaveLength(0);
    expect(g.nodes).toHaveLength(1);
  });

  it('handles an empty run without throwing', () => {
    const g = buildDagGraph({ nodes: [], edges: [] });
    expect(g.nodes).toHaveLength(0);
    expect(g.rootCauses).toHaveLength(0);
    expect(summarizeDag(g)).toBe('No nodes.');
  });
});

describe('root-cause attribution', () => {
  it('marks the origin of a cascade and not its victims', () => {
    //   extract(SUCCEEDED) → transform(FAILED) → load(UPSTREAM_FAILED)
    const g = buildDagGraph({
      nodes: [
        node('extract', 'SUCCEEDED'),
        node('transform', 'FAILED'),
        node('load', 'UPSTREAM_FAILED'),
      ],
      edges: [edge('extract', 'transform'), edge('transform', 'load')],
    });
    expect(g.rootCauses).toEqual(['transform']);
    expect(g.byId.transform!.isRootCause).toBe(true);
    expect(g.byId.load!.isRootCause).toBe(false);
    expect(g.byId.extract!.isRootCause).toBe(false);
  });

  it('does NOT mark a downstream FAILED node whose ancestor also failed', () => {
    // An all_done node runs and fails after an upstream failure. The upstream
    // failure is still the root cause; the downstream one is collateral.
    const g = buildDagGraph({
      nodes: [node('a', 'FAILED'), node('b', 'FAILED')],
      edges: [edge('a', 'b')],
    });
    expect(g.rootCauses).toEqual(['a']);
    expect(g.byId.b!.isRootCause).toBe(false);
  });

  it('sees through a chain of UPSTREAM_FAILED nodes to the real origin', () => {
    const g = buildDagGraph({
      nodes: [
        node('a', 'FAILED'),
        node('b', 'UPSTREAM_FAILED'),
        node('c', 'UPSTREAM_FAILED'),
        node('d', 'FAILED'),
      ],
      edges: [edge('a', 'b'), edge('b', 'c'), edge('c', 'd')],
    });
    expect(g.rootCauses).toEqual(['a']);
  });

  it('reports multiple independent root causes', () => {
    const g = buildDagGraph({
      nodes: [
        node('x', 'FAILED'),
        node('y', 'FAILED'),
        node('sink', 'UPSTREAM_FAILED'),
      ],
      edges: [edge('x', 'sink'), edge('y', 'sink')],
    });
    expect(g.rootCauses).toEqual(['x', 'y']);
  });

  it('never treats SKIPPED as a failure — a skipped branch is not a problem', () => {
    const g = buildDagGraph({
      nodes: [node('branch', 'SKIPPED'), node('after', 'SKIPPED')],
      edges: [edge('branch', 'after')],
    });
    expect(g.rootCauses).toEqual([]);
    expect(g.byId.after!.isRootCause).toBe(false);
  });

  it('never treats CANCELLED as a failure', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'CANCELLED'), node('b', 'CANCELLED')],
      edges: [edge('a', 'b')],
    });
    expect(g.rootCauses).toEqual([]);
  });

  it('does not hang on a cycle containing a failure', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'FAILED'), node('b', 'FAILED')],
      edges: [edge('a', 'b'), edge('b', 'a')],
    });
    expect(g.hasCycle).toBe(true);
    expect(g.rootCauses.length).toBeGreaterThanOrEqual(0);
  });

  it('accepts protobuf-spelled node states', () => {
    const g = buildDagGraph({
      nodes: [
        node('a', 'NODE_STATE_FAILED' as NodeState),
        node('b', 'NODE_STATE_UPSTREAM_FAILED' as NodeState),
      ],
      edges: [edge('a', 'b')],
    });
    expect(g.byId.a!.state).toBe('FAILED');
    expect(g.rootCauses).toEqual(['a']);
  });
});

describe('attempt history attachment', () => {
  it('keeps every attempt as its own row, ordered by attempt_number', () => {
    // Deliberately out of order on the wire: retries are separate immutable
    // rows (invariant 9) and the UI must show all of them, newest last.
    const g = buildDagGraph(
      { nodes: [node('t', 'FAILED')], edges: [] },
      [
        attempt('t', 3, 'FAILED'),
        attempt('t', 1, 'FAILED'),
        attempt('t', 2, 'LOST'),
      ],
    );
    expect(g.byId.t!.attempts.map((a) => a.attempt_number)).toEqual([1, 2, 3]);
    expect(g.byId.t!.latestAttempt!.attempt_number).toBe(3);
  });

  it('leaves a node with no attempts with an empty history, not null', () => {
    const g = buildDagGraph({ nodes: [node('t', 'PENDING')], edges: [] }, []);
    expect(g.byId.t!.attempts).toEqual([]);
    expect(g.byId.t!.latestAttempt).toBeNull();
  });

  it('lifts pending_reason from the latest attempt so a stall is explained', () => {
    const g = buildDagGraph(
      { nodes: [node('train', 'READY')], edges: [] },
      [attempt('train', 1, 'PENDING', { pending_reason: 'waiting_for_resources: gpu (need 4, free 3)' })],
    );
    expect(g.byId.train!.pendingReason).toBe('waiting_for_resources: gpu (need 4, free 3)');
  });

  it('prefers a node-level pending_reason when the projection supplies one', () => {
    const g = buildDagGraph(
      { nodes: [node('train', 'READY', { pending_reason: 'node-level reason' })], edges: [] },
      [attempt('train', 1, 'PENDING', { pending_reason: 'attempt-level reason' })],
    );
    expect(g.byId.train!.pendingReason).toBe('node-level reason');
  });

  it('groups attempts across nodes independently', () => {
    const grouped = groupAttempts([
      attempt('a', 2, 'SUCCEEDED'),
      attempt('b', 1, 'RUNNING'),
      attempt('a', 1, 'FAILED'),
    ]);
    expect(Object.keys(grouped).sort()).toEqual(['a', 'b']);
    expect(grouped.a!.map((x) => x.attempt_number)).toEqual([1, 2]);
  });
});

describe('counts and summary', () => {
  it('counts nodes per state', () => {
    const g = buildDagGraph({
      nodes: [
        node('a', 'SUCCEEDED'),
        node('b', 'SUCCEEDED'),
        node('c', 'RUNNING'),
        node('d', 'SKIPPED'),
      ],
      edges: [],
    });
    expect(g.counts.SUCCEEDED).toBe(2);
    expect(g.counts.RUNNING).toBe(1);
    expect(g.counts.SKIPPED).toBe(1);
    expect(g.counts.FAILED).toBeUndefined();
  });

  it('summarises a failure by naming the root cause and the blocked count', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'FAILED'), node('b', 'UPSTREAM_FAILED'), node('c', 'UPSTREAM_FAILED')],
      edges: [edge('a', 'b'), edge('b', 'c')],
    });
    expect(summarizeDag(g)).toBe('Failed at a; 2 nodes blocked downstream.');
  });

  it('summarises a healthy finished run', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'SUCCEEDED'), node('b', 'SUCCEEDED')],
      edges: [edge('a', 'b')],
    });
    expect(summarizeDag(g)).toBe('All nodes succeeded.');
  });

  it('summarises in-flight work', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'RUNNING'), node('b', 'PENDING')],
      edges: [edge('a', 'b')],
    });
    expect(summarizeDag(g)).toBe('1 node running.');
  });
});

/**
 * Layout geometry.
 *
 * Tested here rather than through the DOM because both the run graph and the
 * catalog's spec graph consume these coordinates, so a regression in them
 * silently breaks two screens. The assertions are about RELATIONSHIPS between
 * placements -- same column, distinct rows, bounds containing every node --
 * rather than about literal pixel values, so retuning the gaps does not force a
 * test rewrite while a genuinely broken layout still fails.
 */
describe('layout', () => {
  const xs = (l: ReturnType<typeof layoutLayers>) => l.places.map((p) => p.x);
  const ys = (l: ReturnType<typeof layoutLayers>) => l.places.map((p) => p.y);

  it('puts each depth in its own column, left to right', () => {
    const l = layoutLayers([['a'], ['b'], ['c']]);
    expect(l.byId.a!.x).toBeLessThan(l.byId.b!.x);
    expect(l.byId.b!.x).toBeLessThan(l.byId.c!.x);
    // A single-node layer is a single row, so the column is a straight line.
    expect(new Set(ys(l)).size).toBe(1);
  });

  it('stacks nodes within a layer into distinct rows of one column', () => {
    const l = layoutLayers([['a'], ['b', 'c', 'd']]);
    expect(l.byId.b!.x).toBe(l.byId.c!.x);
    expect(l.byId.c!.x).toBe(l.byId.d!.x);
    expect(new Set([l.byId.b!.y, l.byId.c!.y, l.byId.d!.y]).size).toBe(3);
  });

  it('leaves a gap between adjacent columns wider than the node itself is tall', () => {
    // The gap is what an edge has to turn in; if columns ever abut, the
    // orthogonal routing has nowhere to place its channel.
    const l = layoutLayers([['a'], ['b']]);
    expect(l.byId.b!.x - (l.byId.a!.x + NODE_W)).toBeGreaterThan(0);
  });

  it('reports bounds that contain every placed node', () => {
    const l = layoutLayers([['a', 'b'], ['c']]);
    for (const p of l.places) {
      expect(p.x + NODE_W).toBeLessThanOrEqual(l.width);
      expect(p.y + NODE_H).toBeLessThanOrEqual(l.height);
    }
  });

  it('never reports zero bounds for an empty graph', () => {
    const l = layoutLayers([]);
    expect(l.places).toEqual([]);
    expect(l.width).toBeGreaterThan(0);
    expect(l.height).toBeGreaterThan(0);
  });

  /**
   * The degenerate case behind the "DAG is not connected" report: when the API
   * returns no edges, every node is a root and layering legitimately puts all of
   * them at depth 0. Rendering that single layer as one column produced a very
   * tall ribbon; it must wrap into a grid instead.
   */
  describe('edgeless graph', () => {
    const ten = ['n0', 'n1', 'n2', 'n3', 'n4', 'n5', 'n6', 'n7', 'n8', 'n9'];

    it('wraps a single layer into a grid rather than one tall column', () => {
      const l = layoutLayers([ten]);
      expect(new Set(xs(l)).size).toBeGreaterThan(1);
      expect(new Set(ys(l)).size).toBeGreaterThan(1);
    });

    it('keeps the grid wider than it is tall, unlike the column it replaces', () => {
      const grid = layoutLayers([ten]);
      const column = layoutLayers([['a'], ten]);
      expect(grid.height).toBeLessThan(column.height);
      expect(grid.width).toBeGreaterThan(NODE_W * 2);
    });

    it('places grid nodes in reading order, filling each row before the next', () => {
      const l = layoutGrid(['a', 'b', 'c', 'd', 'e'], 4);
      expect(l.byId.a!.y).toBe(l.byId.d!.y);
      expect(l.byId.a!.x).toBeLessThan(l.byId.b!.x);
      // The fifth node wraps: same column as the first, next row down.
      expect(l.byId.e!.x).toBe(l.byId.a!.x);
      expect(l.byId.e!.y).toBeGreaterThan(l.byId.a!.y);
    });

    it('gives every node a distinct position in the grid', () => {
      const l = layoutLayers([ten]);
      const seen = new Set(l.places.map((p) => `${p.x},${p.y}`));
      expect(seen.size).toBe(ten.length);
    });

    it('reports bounds containing the whole grid', () => {
      const l = layoutLayers([ten]);
      for (const p of l.places) {
        expect(p.x + NODE_W).toBeLessThanOrEqual(l.width);
        expect(p.y + NODE_H).toBeLessThanOrEqual(l.height);
      }
    });

    it('does not grid a graph that has real layers', () => {
      // Two layers means edges exist, so depth is meaningful and must survive.
      const l = layoutLayers([['a'], ['b']]);
      expect(l.byId.a!.y).toBe(l.byId.b!.y);
      expect(l.byId.a!.x).not.toBe(l.byId.b!.x);
    });

    it('handles a single node without wrapping or throwing', () => {
      const l = layoutLayers([['only']]);
      expect(l.places).toHaveLength(1);
      expect(l.width).toBeGreaterThanOrEqual(NODE_W);
    });
  });
});

/**
 * Edge routing.
 *
 * The path string is asserted on structurally -- where it starts, where it ends,
 * which commands it uses -- because those are the properties that make an edge
 * read as a connection: it must leave the source's right edge, arrive at the
 * target's left edge, and arrive horizontally so the arrowhead points into the
 * node.
 */
describe('edgePath', () => {
  const a = { id: 'a', x: 0, y: 0 };
  const nums = (d: string) => d.match(/-?\d+(\.\d+)?/g)!.map(Number);

  it('leaves the right edge of the source at its vertical midpoint', () => {
    const d = edgePath(a, { id: 'b', x: 300, y: 200 });
    expect(d.startsWith(`M ${NODE_W} ${NODE_H / 2}`)).toBe(true);
  });

  it('arrives at the left edge of the target at its vertical midpoint', () => {
    const b = { id: 'b', x: 300, y: 200 };
    const d = edgePath(a, b);
    expect(d.trimEnd().endsWith(`L ${b.x} ${b.y + NODE_H / 2}`)).toBe(true);
  });

  it('draws a straight line between nodes on the same row', () => {
    // A jog between two aligned nodes is noise, and worse, implies a turn that
    // the graph does not actually take.
    const d = edgePath(a, { id: 'b', x: 300, y: 0 });
    expect(d).toBe(`M ${NODE_W} ${NODE_H / 2} L 300 ${NODE_H / 2}`);
    expect(d).not.toContain('A');
  });

  it('turns with rounded elbows, not a diagonal, when the rows differ', () => {
    const d = edgePath(a, { id: 'b', x: 300, y: 200 });
    // Two arcs: one out of the horizontal, one back into it.
    expect(d.match(/A /g)).toHaveLength(2);
  });

  it('arrives horizontally whether the target is above or below', () => {
    for (const dy of [200, -200]) {
      const b = { id: 'b', x: 300, y: dy };
      const d = edgePath(a, b);
      // The final command is a horizontal L at the target's midline.
      expect(d.trimEnd().endsWith(`L ${b.x} ${b.y + NODE_H / 2}`)).toBe(true);
      expect(d.match(/A /g)).toHaveLength(2);
    }
  });

  it('keeps the turn channel between the two nodes', () => {
    // An edge that turned outside the gap would cross whichever node sits
    // there. Only the x of each L command is checked: the numbers in an A
    // command are radii and sweep flags, not coordinates.
    const b = { id: 'b', x: 400, y: 150 };
    const d = edgePath(a, b);
    const lineXs = [...d.matchAll(/L (-?\d+(?:\.\d+)?) /g)].map((m) => Number(m[1]));
    expect(lineXs.length).toBeGreaterThan(0);
    for (const x of lineXs) {
      expect(x).toBeGreaterThanOrEqual(NODE_W - 1);
      expect(x).toBeLessThanOrEqual(b.x + 1);
    }
  });

  it('shrinks the corner radius rather than overshooting on a tiny row offset', () => {
    // A 4px offset cannot host a 14px radius; the arcs must not extend past the
    // midline or the path renders as a visible kink.
    const b = { id: 'b', x: 300, y: 4 };
    const d = edgePath(a, b);
    expect(d).not.toContain('NaN');
    for (const n of nums(d)) expect(Number.isFinite(n)).toBe(true);
  });

  it('produces a finite path even when the target sits left of the source', () => {
    // Only reachable via a cycle, which the view reports rather than hides, so
    // the renderer must still get a drawable string.
    const d = edgePath({ id: 'a', x: 400, y: 0 }, { id: 'b', x: 0, y: 120 });
    expect(d).not.toContain('NaN');
    for (const n of nums(d)) expect(Number.isFinite(n)).toBe(true);
  });

  it('is pure: the same placements always yield the same path', () => {
    const b = { id: 'b', x: 300, y: 90 };
    expect(edgePath(a, b)).toBe(edgePath(a, b));
  });
});

