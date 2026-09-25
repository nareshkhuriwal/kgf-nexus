// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Pure graph derivation for the DAG view: layout, root-cause attribution, and
 * the per-node summary the renderer draws.
 *
 * Kept free of React and of the network on purpose. The DAG view is the screen
 * an operator stares at during an incident, and the logic that decides "this
 * node is the thing that broke" is worth property-testing in isolation rather
 * than only through the DOM.
 */

import type { Attempt, NodeState, RunDetail, RunEdge, RunNode } from '../api/types';
import { normalizeState } from './states';
import { isExecutorWaiting, isInFlight } from './progress';

export interface DagNode extends RunNode {
  /** 0 for roots; longest-path depth, which is what makes layers read left→right. */
  depth: number;
  /** Rank within the depth layer, after ordering. */
  order: number;
  upstream: string[];
  downstream: string[];
  /**
   * True when this node is a FAILED node with no FAILED ancestor: the actual
   * origin of a failure cascade. UPSTREAM_FAILED nodes are, by construction,
   * never root causes — that separation is the entire point of having the two
   * states, so the UI must act on it rather than merely colour it.
   */
  isRootCause: boolean;
  attempts: Attempt[];
  /** Highest attempt_number, which is the one whose state the node reflects. */
  latestAttempt: Attempt | null;
  /** Lifted from the latest attempt so the graph can explain a stalled node.
   *  The SCHEDULER's reason for not placing it. */
  pendingReason: string | null;
  /** The latest attempt's EXECUTOR status (progress_message), only while that
   *  attempt is in flight: a finished attempt's last pod status is history,
   *  shown on the attempt card, not a live badge on the graph. */
  executorStatus: string | null;
  /** True when the in-flight attempt's executor still reports "pod Pending". */
  executorWaiting: boolean;
}

export interface DagGraph {
  nodes: DagNode[];
  edges: RunEdge[];
  byId: Record<string, DagNode>;
  /** Nodes per depth layer, in render order. */
  layers: string[][];
  /** FAILED nodes with no failed ancestor. Empty for a healthy run. */
  rootCauses: string[];
  /** True when the spec graph contains a cycle (should be impossible: the
   *  gateway validates acyclicity at submit time). Surfaced, not hidden. */
  hasCycle: boolean;
  counts: Partial<Record<NodeState, number>>;
}

/** States that mean "this node is itself broken", as opposed to blocked. */
const FAILURE_STATES = new Set<string>(['FAILED']);
/** States that mean "did not run because something upstream broke". */
const BLOCKED_STATES = new Set<string>(['UPSTREAM_FAILED']);

function normNode(state: string | null | undefined): NodeState {
  return normalizeState(state) as NodeState;
}

/**
 * Groups attempts by node and sorts each group by attempt_number ascending.
 *
 * Retries are separate rows and history is never mutated (invariant 9), so the
 * full list is the node's real history and the LAST element is the current
 * truth. Sorting rather than trusting server order means a paged or
 * out-of-order response still renders history correctly.
 */
export function groupAttempts(attempts: Attempt[]): Record<string, Attempt[]> {
  const out: Record<string, Attempt[]> = {};
  for (const a of attempts) {
    (out[a.node_id] ??= []).push(a);
  }
  for (const key of Object.keys(out)) {
    out[key]!.sort((x, y) => x.attempt_number - y.attempt_number);
  }
  return out;
}

/**
 * Longest-path depth via Kahn's algorithm.
 *
 * Longest path, not shortest: a node must be drawn to the right of EVERY
 * dependency, so a diamond does not collapse its long side onto its short one.
 * Nodes left unvisited when the queue drains are part of a cycle; they are
 * assigned a depth and reported rather than dropped, because silently hiding
 * nodes during an incident is worse than drawing a wrong-looking edge.
 */
function computeDepths(
  nodeIds: string[],
  upstream: Record<string, string[]>,
  downstream: Record<string, string[]>,
): { depths: Record<string, number>; hasCycle: boolean } {
  const indegree: Record<string, number> = {};
  const depths: Record<string, number> = {};
  for (const id of nodeIds) {
    indegree[id] = upstream[id]?.length ?? 0;
    depths[id] = 0;
  }

  const queue = nodeIds.filter((id) => indegree[id] === 0);
  let visited = 0;
  while (queue.length > 0) {
    const id = queue.shift()!;
    visited += 1;
    for (const next of downstream[id] ?? []) {
      depths[next] = Math.max(depths[next] ?? 0, (depths[id] ?? 0) + 1);
      indegree[next] = (indegree[next] ?? 0) - 1;
      if (indegree[next] === 0) queue.push(next);
    }
  }

  return { depths, hasCycle: visited !== nodeIds.length };
}

/**
 * Marks the origins of failure cascades.
 *
 * A FAILED node whose ancestors contain another FAILED node is collateral: the
 * interesting node is the earliest one. Walking ancestors rather than only
 * immediate parents matters because a chain of UPSTREAM_FAILED nodes can sit
 * between the real failure and a later FAILED node (a node with an all_done
 * trigger rule runs and fails downstream of a failure, for instance).
 */
function computeRootCauses(
  nodeIds: string[],
  states: Record<string, NodeState>,
  upstream: Record<string, string[]>,
): Set<string> {
  const memo = new Map<string, boolean>();

  const hasFailedAncestor = (id: string, seen: Set<string>): boolean => {
    const cached = memo.get(id);
    if (cached !== undefined) return cached;
    if (seen.has(id)) return false; // cycle guard; treated as no ancestor
    seen.add(id);

    let result = false;
    for (const parent of upstream[id] ?? []) {
      const pState = states[parent];
      if (pState && (FAILURE_STATES.has(pState) || BLOCKED_STATES.has(pState))) {
        // A blocked parent is itself downstream of a real failure, so a failure
        // exists above us either way.
        result = true;
        break;
      }
      if (hasFailedAncestor(parent, seen)) {
        result = true;
        break;
      }
    }
    seen.delete(id);
    memo.set(id, result);
    return result;
  };

  const roots = new Set<string>();
  for (const id of nodeIds) {
    const state = states[id];
    if (!state || !FAILURE_STATES.has(state)) continue;
    if (!hasFailedAncestor(id, new Set())) roots.add(id);
  }
  return roots;
}

/**
 * Builds the renderable graph from a run detail plus its attempts.
 *
 * Tolerant of a partial backend: edges naming unknown nodes are ignored rather
 * than throwing, and a missing attempts list simply yields empty histories.
 * A half-built gateway should degrade the view, not blank the page.
 */
export function buildDagGraph(run: Pick<RunDetail, 'nodes' | 'edges'>, attempts: Attempt[] = []): DagGraph {
  const nodes = run.nodes ?? [];
  const nodeIds = nodes.map((n) => n.node_id);
  const known = new Set(nodeIds);

  const upstream: Record<string, string[]> = {};
  const downstream: Record<string, string[]> = {};
  for (const id of nodeIds) {
    upstream[id] = [];
    downstream[id] = [];
  }

  const edges = (run.edges ?? []).filter(
    (e) => known.has(e.from_node_id) && known.has(e.to_node_id) && e.from_node_id !== e.to_node_id,
  );
  for (const e of edges) {
    upstream[e.to_node_id]!.push(e.from_node_id);
    downstream[e.from_node_id]!.push(e.to_node_id);
  }

  const states: Record<string, NodeState> = {};
  for (const n of nodes) states[n.node_id] = normNode(n.state);

  // Order within a layer by node id, so the layout is stable across refreshes.
  // A graph that reshuffles on every poll is unusable.
  const { depths, orderOf, layers, hasCycle } = assignLayers(nodeIds, upstream, downstream);
  const rootCauses = computeRootCauses(nodeIds, states, upstream);
  const grouped = groupAttempts(attempts);

  const dagNodes: DagNode[] = nodes.map((n) => {
    const list = grouped[n.node_id] ?? [];
    const latest = list.length > 0 ? list[list.length - 1]! : null;
    return {
      ...n,
      state: states[n.node_id] ?? normNode(n.state),
      depth: depths[n.node_id] ?? 0,
      order: orderOf[n.node_id] ?? 0,
      upstream: [...(upstream[n.node_id] ?? [])].sort(),
      downstream: [...(downstream[n.node_id] ?? [])].sort(),
      isRootCause: rootCauses.has(n.node_id),
      attempts: list,
      latestAttempt: latest,
      // The node-level column wins when present; otherwise lift it from the
      // attempt, which is where the scheduler actually records it.
      pendingReason: n.pending_reason ?? latest?.pending_reason ?? null,
      executorStatus: isInFlight(latest) ? (latest?.progress_message ?? null) : null,
      executorWaiting: isExecutorWaiting(latest),
    };
  });

  const byId: Record<string, DagNode> = {};
  for (const n of dagNodes) byId[n.node_id] = n;

  const counts: Partial<Record<NodeState, number>> = {};
  for (const n of dagNodes) counts[n.state] = (counts[n.state] ?? 0) + 1;

  return {
    nodes: dagNodes,
    edges,
    byId,
    layers,
    rootCauses: [...rootCauses].sort(),
    hasCycle,
    counts,
  };
}

/**
 * A one-line human answer to "what is happening with this run", derived from
 * node states alone. Shown above the graph so the answer does not require
 * reading the graph.
 */
export function summarizeDag(graph: DagGraph): string {
  const c = graph.counts;
  if (graph.rootCauses.length > 0) {
    const blocked = c.UPSTREAM_FAILED ?? 0;
    const cause = graph.rootCauses.join(', ');
    return blocked > 0
      ? `Failed at ${cause}; ${blocked} node${blocked === 1 ? '' : 's'} blocked downstream.`
      : `Failed at ${cause}.`;
  }
  if ((c.RUNNING ?? 0) > 0) return `${c.RUNNING} node${c.RUNNING === 1 ? '' : 's'} running.`;
  if ((c.READY ?? 0) > 0) return `${c.READY} node${c.READY === 1 ? '' : 's'} ready, waiting to dispatch.`;
  if ((c.PENDING ?? 0) > 0) return `${c.PENDING} node${c.PENDING === 1 ? '' : 's'} waiting on dependencies.`;
  const total = graph.nodes.length;
  if (total > 0 && (c.SUCCEEDED ?? 0) === total) return 'All nodes succeeded.';
  return total === 0 ? 'No nodes.' : 'No nodes in flight.';
}


/**
 * Every node reachable from `id` by walking upstream, plus everything reachable
 * by walking downstream, plus `id` itself.
 *
 * This is the set that answers "what does this node depend on, and what depends
 * on it" — the question the graph view dims the rest of the graph to answer.
 * TRANSITIVE rather than just the immediate neighbours, which is the whole
 * point: in a cascade the node that actually broke is usually several hops from
 * the node an operator clicked, and a one-hop highlight stops exactly short of
 * showing them the thing they are looking for.
 *
 * Lives here rather than in the component because it is pure graph reachability
 * over the same adjacency lists buildDagGraph already produced, and because a
 * cycle must not hang the render: the visited set doubles as the cycle guard, so
 * a graph the gateway should have rejected still draws.
 *
 * Returns a Set so the caller's per-node lookup while rendering is O(1); a
 * linear scan per node would be quadratic over the graph.
 */
export function relatedSet(
  id: string,
  adjacency: { upstream: Record<string, string[]>; downstream: Record<string, string[]> },
): Set<string> {
  const seen = new Set<string>([id]);
  const walk = (from: string, edges: Record<string, string[]>) => {
    const stack = [from];
    while (stack.length > 0) {
      const cur = stack.pop()!;
      for (const next of edges[cur] ?? []) {
        if (seen.has(next)) continue;
        seen.add(next);
        stack.push(next);
      }
    }
  };
  walk(id, adjacency.upstream);
  walk(id, adjacency.downstream);
  return seen;
}

/**
 * Adjacency lists lifted back out of a built graph, in the shape relatedSet
 * wants.
 *
 * DagNode already carries its own upstream/downstream arrays, so this is a
 * reshape rather than a recomputation — which matters, because recomputing
 * adjacency from `edges` at render time would be a second implementation of
 * something buildDagGraph already did, free to disagree with it.
 */
export function adjacencyOf(nodes: Pick<DagNode, 'node_id' | 'upstream' | 'downstream'>[]): {
  upstream: Record<string, string[]>;
  downstream: Record<string, string[]>;
} {
  const upstream: Record<string, string[]> = {};
  const downstream: Record<string, string[]> = {};
  for (const n of nodes) {
    upstream[n.node_id] = n.upstream;
    downstream[n.node_id] = n.downstream;
  }
  return { upstream, downstream };
}

/**
 * Layer assignment shared by the run graph and the spec graph.
 *
 * Extracted from buildDagGraph rather than copied into the catalog: "which
 * column does this node sit in, and in what order within the column" is the
 * same question for a spec's tasks as for a run's materialised nodes, and two
 * implementations of it would eventually disagree about the same workflow --
 * a catalog diagram that does not match the run diagram for the same DAG is
 * worse than no catalog diagram.
 */
export function assignLayers(
  nodeIds: string[],
  upstream: Record<string, string[]>,
  downstream: Record<string, string[]>,
): { layers: string[][]; depths: Record<string, number>; orderOf: Record<string, number>; hasCycle: boolean } {
  const { depths, hasCycle } = computeDepths(nodeIds, upstream, downstream);

  const layers: string[][] = [];
  for (const id of nodeIds) {
    const d = depths[id] ?? 0;
    (layers[d] ??= []).push(id);
  }
  for (const layer of layers) {
    if (layer) layer.sort((a, b) => a.localeCompare(b));
  }

  const orderOf: Record<string, number> = {};
  layers.forEach((layer) => {
    (layer ?? []).forEach((id, i) => {
      orderOf[id] = i;
    });
  });

  return { layers: layers.map((l) => l ?? []), depths, orderOf, hasCycle };
}

/**
 * Node box geometry, and the placement maths that turns layers into coordinates.
 *
 * These live here rather than in DagView so the catalog's spec graph draws
 * boxes of the same size in the same grid without importing a component. The
 * numbers are literal pixels on purpose: they are SVG user-space units inside a
 * viewBox, not CSS lengths, so the spacing tokens do not apply to them and a
 * token indirection would only hide that.
 */
export const NODE_W = 248;
export const NODE_H = 88;
/**
 * The horizontal gap is deliberately wide relative to the node: it is the only
 * space an edge has to make its turn, and a route that has to elbow inside a
 * narrow channel reads as a smudge between two boxes rather than as a
 * connection. The vertical gap is likewise larger than the old 20 so sibling
 * fan-outs do not stack their inbound segments on top of each other.
 */
const GAP_X = 104;
const GAP_Y = 34;
const PAD = 32;
/** Corner radius of an orthogonal edge's turn. Tuned against GAP_X/2. */
const ELBOW_R = 14;
/**
 * Columns a graph with no edges wraps at. An edgeless graph has no meaningful
 * left→right reading order, so the only job of the layout is to let the eye
 * sweep a block of vertices; 4 keeps the block close to square for the 8-16
 * node runs where this case actually shows up.
 */
const GRID_COLS = 4;

export interface Placement {
  id: string;
  x: number;
  y: number;
}

export interface Layout {
  places: Placement[];
  width: number;
  height: number;
  byId: Record<string, Placement>;
}

function finish(places: Placement[], cols: number, rows: number): Layout {
  const byId: Record<string, Placement> = {};
  for (const p of places) byId[p.id] = p;
  return {
    places,
    byId,
    width: PAD * 2 + Math.max(1, cols) * (NODE_W + GAP_X) - GAP_X,
    height: PAD * 2 + Math.max(1, rows) * (NODE_H + GAP_Y) - GAP_Y,
  };
}

/**
 * Wrap-around grid for a graph with no edges at all.
 *
 * Every node in an edgeless graph has indegree 0, so layering legitimately puts
 * all of them at depth 0 -- and the layered layout then draws that single layer
 * as one tall column, which for ten nodes is a 1000px ribbon the operator has
 * to scroll past to learn nothing. A depth axis only means something when edges
 * define it; with no edges there is no order to preserve, so wrapping into a
 * grid is not a compromise, it is the honest rendering of "these vertices are
 * unrelated". Kept as a named export so the catalog's spec graph gets the same
 * treatment for a single-task workflow.
 */
export function layoutGrid(ids: string[], cols = GRID_COLS): Layout {
  const places: Placement[] = ids.map((id, i) => ({
    id,
    x: PAD + (i % cols) * (NODE_W + GAP_X),
    y: PAD + Math.floor(i / cols) * (NODE_H + GAP_Y),
  }));
  return finish(places, Math.min(cols, Math.max(1, ids.length)), Math.ceil(ids.length / cols));
}

/**
 * Places one node per id in `layers`, left→right by depth, top→bottom by rank.
 *
 * A graph whose every node sits at depth 0 is, by construction, a graph with no
 * edges (any edge would push its target to depth >= 1), so that case is routed
 * to the grid instead of producing a single tall column.
 */
export function layoutLayers(layers: string[][]): Layout {
  const filled = layers.filter((l) => l.length > 0);
  if (filled.length === 1) return layoutGrid(filled[0]!);

  const places: Placement[] = [];
  let maxRows = 0;
  layers.forEach((layer, depth) => {
    maxRows = Math.max(maxRows, layer.length);
    layer.forEach((id, row) => {
      places.push({
        id,
        x: PAD + depth * (NODE_W + GAP_X),
        y: PAD + row * (NODE_H + GAP_Y),
      });
    });
  });

  return finish(places, layers.length, maxRows);
}

/**
 * Orthogonal rounded-elbow route from the right-edge midpoint of `a` to the
 * left-edge midpoint of `b`.
 *
 * Replaces the old cubic bezier because a bezier between two rows sags through
 * the space between columns, and when several siblings fan out of one node their
 * curves overlap into a single blur -- the operator cannot tell which parent
 * feeds which child, which is the one question the edges exist to answer. An
 * orthogonal route leaves each node horizontally, turns once at a shared mid-x
 * channel, runs vertically, and arrives horizontally, so every arrival is
 * perpendicular to the target's left edge and reads unambiguously as "into this
 * node". The corners are rounded rather than mitred so the result still looks
 * like a modern diagram rather than a circuit schematic.
 *
 * Pure and coordinate-only: the catalog's spec graph draws its edges with the
 * same function, which is what keeps a workflow's diagram identical in the
 * catalog and in a run.
 */
export function edgePath(a: Placement, b: Placement): string {
  const x1 = a.x + NODE_W;
  const y1 = a.y + NODE_H / 2;
  const x2 = b.x;
  const y2 = b.y + NODE_H / 2;

  // Custom layouts may put a child behind its parent. Route above the cards
  // instead of drawing a backwards arrow through their bodies.
  if (x2 < x1 + 16) {
    const start = a.x + NODE_W / 2, end = b.x + NODE_W / 2;
    const top = Math.max(4, Math.min(a.y, b.y) - 28);
    return `M ${start} ${a.y} C ${start} ${top} ${end} ${top} ${end} ${b.y}`;
  }

  // Same row: a straight line is both the clearest route and the only one
  // without a spurious jog.
  if (Math.abs(y2 - y1) < 1) return `M ${x1} ${y1} L ${x2} ${y2}`;

  // The turn channel sits midway between the two nodes so that edges entering
  // the same column share a vertical line-up rather than each cutting its own
  // diagonal. Clamped to keep the channel inside the gap even when a node is
  // placed to the left of its parent (a cycle, which is reported, not hidden).
  const mid = x2 > x1 ? x1 + (x2 - x1) / 2 : x1 + GAP_X / 2;

  // Radius must not exceed half of either leg, or the two arcs would overlap
  // and SVG would render a kink.
  const r = Math.min(ELBOW_R, Math.abs(mid - x1), Math.abs(x2 - mid), Math.abs(y2 - y1) / 2);
  const down = y2 > y1;
  // Sweep flags: the first corner turns from +x into ±y, the second from ±y
  // back into +x, so the two arcs curve in opposite senses.
  const s1 = down ? 1 : 0;
  const s2 = down ? 0 : 1;
  const vy = down ? y2 - r : y2 + r;

  return (
    `M ${x1} ${y1} L ${mid - r} ${y1}` +
    ` A ${r} ${r} 0 0 ${s1} ${mid} ${down ? y1 + r : y1 - r}` +
    ` L ${mid} ${vy}` +
    ` A ${r} ${r} 0 0 ${s2} ${mid + r} ${y2}` +
    ` L ${x2} ${y2}`
  );
}
