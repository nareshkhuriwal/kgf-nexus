// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Graph derivation for a spec DEFINITION, as opposed to a run.
 *
 * This is separate from buildDagGraph for one reason that is not stylistic: a
 * spec has no state. buildDagGraph's whole output is organised around node
 * states — root-cause attribution, per-state counts, attempt histories — and
 * the only way to feed it a spec would be to invent a state for every task.
 * Every candidate is a lie: PENDING means "the DAG engine created this node and
 * it is waiting on dependencies", which is a claim about a run that does not
 * exist. Colouring a catalog diagram with run states an operator could mistake
 * for live information is exactly the failure the console is supposed to avoid.
 *
 * So the STRUCTURE is shared and the semantics are not: layering, ordering,
 * cycle detection, placement and edge geometry all come from lib/dag, and this
 * module adds only what a definition actually knows — fan-in, fan-out, and the
 * declared shape of each task.
 */

import { assignLayers } from './dag';
import type { SpecDocument, SpecEdge, SpecTask } from '../api/types';

export interface SpecGraphNode {
  id: string;
  task: SpecTask;
  depth: number;
  order: number;
  upstream: string[];
  downstream: string[];
  /** No dependencies: where execution starts. */
  isRoot: boolean;
  /** Nothing depends on it: where execution ends. */
  isLeaf: boolean;
}

export interface SpecGraph {
  nodes: SpecGraphNode[];
  edges: SpecEdge[];
  byId: Record<string, SpecGraphNode>;
  layers: string[][];
  /**
   * Should be impossible — the gateway validates acyclicity at submit time —
   * but a stored spec that trips it is precisely the thing an inspection view
   * exists to reveal, so it is surfaced rather than swallowed.
   */
  hasCycle: boolean;
  /** Edges naming a task that is not in `tasks`. A structurally broken spec. */
  danglingEdges: SpecEdge[];
  roots: string[];
  leaves: string[];
  /** Longest dependency chain, in tasks. 0 for an empty spec. */
  criticalPathLength: number;
}

export function buildSpecGraph(spec: SpecDocument | undefined): SpecGraph {
  const tasks = spec?.tasks ?? [];
  const nodeIds = tasks.map((t) => t.id);
  const known = new Set(nodeIds);

  const upstream: Record<string, string[]> = {};
  const downstream: Record<string, string[]> = {};
  for (const id of nodeIds) {
    upstream[id] = [];
    downstream[id] = [];
  }

  // Split rather than filtered: an edge pointing at a task that does not exist
  // means the definition itself is wrong, and dropping it silently would make a
  // broken spec render as a healthy one.
  const all = spec?.edges ?? [];
  const edges: SpecEdge[] = [];
  const danglingEdges: SpecEdge[] = [];
  for (const e of all) {
    if (known.has(e.from_node_id) && known.has(e.to_node_id) && e.from_node_id !== e.to_node_id) {
      edges.push(e);
      upstream[e.to_node_id]!.push(e.from_node_id);
      downstream[e.from_node_id]!.push(e.to_node_id);
    } else {
      danglingEdges.push(e);
    }
  }

  const { depths, orderOf, layers, hasCycle } = assignLayers(nodeIds, upstream, downstream);

  const nodes: SpecGraphNode[] = tasks.map((task) => ({
    id: task.id,
    task,
    depth: depths[task.id] ?? 0,
    order: orderOf[task.id] ?? 0,
    upstream: [...(upstream[task.id] ?? [])].sort(),
    downstream: [...(downstream[task.id] ?? [])].sort(),
    isRoot: (upstream[task.id] ?? []).length === 0,
    isLeaf: (downstream[task.id] ?? []).length === 0,
  }));

  const byId: Record<string, SpecGraphNode> = {};
  for (const n of nodes) byId[n.id] = n;

  return {
    nodes,
    edges,
    byId,
    layers,
    hasCycle,
    danglingEdges,
    roots: nodes.filter((n) => n.isRoot).map((n) => n.id).sort(),
    leaves: nodes.filter((n) => n.isLeaf).map((n) => n.id).sort(),
    criticalPathLength: nodes.length === 0 ? 0 : Math.max(...nodes.map((n) => n.depth)) + 1,
  };
}

/**
 * A one-line structural description, shown above the diagram so the shape is
 * legible without reading it. Deliberately says nothing about execution: this
 * is a definition, and nothing here has ever run.
 */
export function summarizeSpecGraph(graph: SpecGraph): string {
  if (graph.nodes.length === 0) return 'No tasks.';
  const parts = [
    `${graph.nodes.length} task${graph.nodes.length === 1 ? '' : 's'}`,
    `${graph.edges.length} dependenc${graph.edges.length === 1 ? 'y' : 'ies'}`,
    `${graph.criticalPathLength} deep`,
  ];
  if (graph.roots.length > 1) parts.push(`${graph.roots.length} entry points`);
  return `${parts.join(' · ')}.`;
}

/** Distinct queues named by the spec, in the order a reader meets them. */
export function specQueues(graph: SpecGraph): string[] {
  const seen = new Set<string>();
  for (const n of graph.nodes) {
    const q = n.task.queue?.trim();
    if (q) seen.add(q);
  }
  return [...seen].sort();
}
