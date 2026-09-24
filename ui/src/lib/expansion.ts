// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Dynamic-expansion grouping for the run DAG (architecture §5.4).
 *
 * A map template can materialise thousands of children. Drawing each as a node
 * box would turn the graph into an unreadable wall and cost a render per poll,
 * so by default a template's children are FOLDED into the template node, which
 * carries a summary (how many, in which states, how many were cached). An
 * operator can unfold a template with a modest number of children to see them
 * individually; above MAX_UNFOLDED children the fold is not offered, because the
 * result would not be a graph anyone can read.
 *
 * Pure, like lib/dag: it rewrites the run's node and edge lists before layout,
 * so layout, root-cause attribution and selection all work on the folded graph
 * unchanged.
 */

import type { Attempt, NodeState, RunDetail, RunEdge, RunExpansion, RunNode } from '../api/types';
import { normalizeState } from './states';

/** Above this a template is always folded: unfolded it would not be readable. */
export const MAX_UNFOLDED = 50;

export interface ExpansionGroup {
  templateId: string;
  /** Children materialised. */
  total: number;
  /** Children per node state. */
  counts: Partial<Record<NodeState, number>>;
  /** Children whose latest attempt reused a memoized result. */
  cached: number;
  /** Whether the children are currently drawn individually. */
  unfolded: boolean;
  /** Whether unfolding is offered at all (total <= MAX_UNFOLDED). */
  unfoldable: boolean;
  /** The expansion record, when the gateway reported one. */
  record?: RunExpansion;
}

/** A node as the folded graph carries it. */
export interface GroupedNode extends RunNode {
  group?: ExpansionGroup;
}

export interface GroupedRun {
  nodes: GroupedNode[];
  edges: RunEdge[];
  groups: Record<string, ExpansionGroup>;
  /** Child node id -> its template, for mapping a selection back. */
  parentOf: Record<string, string>;
}

/**
 * Folds every expansion's children into its template unless the template is in
 * `unfold` and small enough to draw.
 *
 * Children are recognised by parent_node_id (the fact the engine records),
 * never by the `template[i]` spelling of their id.
 */
export function groupExpansions(
  run: Pick<RunDetail, 'nodes' | 'edges'> & { expansions?: RunExpansion[] },
  attempts: Attempt[] = [],
  unfold: ReadonlySet<string> = new Set(),
): GroupedRun {
  const nodes = run.nodes ?? [];
  const edges = run.edges ?? [];
  const records: Record<string, RunExpansion> = {};
  for (const e of run.expansions ?? []) records[e.template_node_id] = e;

  const latestCached: Record<string, boolean> = {};
  const latestNum: Record<string, number> = {};
  for (const a of attempts) {
    if ((latestNum[a.node_id] ?? -1) < a.attempt_number) {
      latestNum[a.node_id] = a.attempt_number;
      latestCached[a.node_id] = !!a.cache_hit_of;
    }
  }

  const childrenOf: Record<string, RunNode[]> = {};
  const parentOf: Record<string, string> = {};
  for (const n of nodes) {
    if (n.parent_node_id) {
      (childrenOf[n.parent_node_id] ??= []).push(n);
      parentOf[n.node_id] = n.parent_node_id;
    }
  }

  const groups: Record<string, ExpansionGroup> = {};
  const hidden = new Set<string>();
  const templateIds = new Set([...Object.keys(childrenOf), ...Object.keys(records)]);
  for (const t of templateIds) {
    const kids = childrenOf[t] ?? [];
    const counts: Partial<Record<NodeState, number>> = {};
    let cached = 0;
    for (const k of kids) {
      const s = normalizeState(k.state) as NodeState;
      counts[s] = (counts[s] ?? 0) + 1;
      if (latestCached[k.node_id]) cached += 1;
    }
    const unfoldable = kids.length > 0 && kids.length <= MAX_UNFOLDED;
    const unfolded = unfoldable && unfold.has(t);
    groups[t] = { templateId: t, total: kids.length, counts, cached, unfolded, unfoldable, record: records[t] };
    if (!unfolded) for (const k of kids) hidden.add(k.node_id);
  }

  return {
    nodes: nodes
      .filter((n) => !hidden.has(n.node_id))
      .map((n) => (groups[n.node_id] ? { ...n, group: groups[n.node_id] } : n)),
    // A folded child's inherited edges would point at nothing; the template's
    // own edges already carry the dependency.
    edges: edges.filter((e) => !hidden.has(e.from_node_id) && !hidden.has(e.to_node_id)),
    groups,
    parentOf,
  };
}

/** One line describing a group, for the node footer and its tooltip. */
export function describeGroup(g: ExpansionGroup): string {
  if (g.record?.outcome === 'FAILED') {
    return `expansion refused: ${g.record.reason ?? 'unknown reason'}`;
  }
  const parts = [`${g.total} item${g.total === 1 ? '' : 's'}`];
  const done = g.counts.SUCCEEDED ?? 0;
  if (g.total > 0) parts.push(`${done} done`);
  const failed = (g.counts.FAILED ?? 0) + (g.counts.UPSTREAM_FAILED ?? 0);
  if (failed > 0) parts.push(`${failed} failed`);
  const running = (g.counts.RUNNING ?? 0) + (g.counts.READY ?? 0);
  if (running > 0) parts.push(`${running} active`);
  if (g.cached > 0) parts.push(`${g.cached} cached`);
  return parts.join(' · ');
}
