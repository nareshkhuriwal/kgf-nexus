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
  /** Lifted from the latest attempt so the graph can explain a stalled node. */
  pendingReason: string | null;
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
export const NODE_W = 176;
export const NODE_H = 56;
const GAP_X = 76;
const GAP_Y = 20;
const PAD = 26;

export interface Placement {
  id: string;
  x: number;
  y: number;
}

/** Places one node per id in `layers`, left→right by depth, top→bottom by rank. */
export function layoutLayers(layers: string[][]): {
  places: Placement[];
  width: number;
  height: number;
  byId: Record<string, Placement>;
} {
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

  const byId: Record<string, Placement> = {};
  for (const p of places) byId[p.id] = p;

  return {
    places,
    byId,
    width: PAD * 2 + Math.max(1, layers.length) * (NODE_W + GAP_X) - GAP_X,
    height: PAD * 2 + Math.max(1, maxRows) * (NODE_H + GAP_Y) - GAP_Y,
  };
}

/** Cubic bezier between the right edge of `a` and the left edge of `b`. */
export function edgePath(a: Placement, b: Placement): string {
  const x1 = a.x + NODE_W;
  const y1 = a.y + NODE_H / 2;
  const x2 = b.x;
  const y2 = b.y + NODE_H / 2;
  const dx = Math.max(28, (x2 - x1) / 2);
  return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`;
}
