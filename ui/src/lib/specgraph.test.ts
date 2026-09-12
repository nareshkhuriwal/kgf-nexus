// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Property tests for spec-graph derivation.
 *
 * The claim worth proving here is the one the whole catalog rests on: a spec's
 * tasks and edges lay out into exactly the same shape as the same DAG's
 * materialised run nodes. If those two ever diverge, a workflow's diagram in
 * the catalog stops matching its diagram in a run, and an operator comparing
 * the two would be reading a difference that does not exist.
 */

import { describe, expect, it } from 'vitest';
import { buildSpecGraph, specQueues, summarizeSpecGraph } from './specgraph';
import { buildDagGraph } from './dag';
import type { RunDetail, SpecDocument } from '../api/types';

/** Diamond: one root, two parallel middles, one fan-in leaf. */
const diamond: SpecDocument = {
  tasks: [
    { id: 'a', task_ref: 'x.a', queue: 'default' },
    { id: 'b', task_ref: 'x.b', queue: 'gpu' },
    { id: 'c', task_ref: 'x.c', queue: 'default' },
    { id: 'd', task_ref: 'x.d', queue: 'gpu' },
  ],
  edges: [
    { from_node_id: 'a', to_node_id: 'b' },
    { from_node_id: 'a', to_node_id: 'c' },
    { from_node_id: 'b', to_node_id: 'd' },
    { from_node_id: 'c', to_node_id: 'd' },
  ],
};

describe('buildSpecGraph', () => {
  it('layers by longest path, so a diamond does not collapse its long side', () => {
    const g = buildSpecGraph(diamond);
    expect(g.byId['a']!.depth).toBe(0);
    expect(g.byId['b']!.depth).toBe(1);
    expect(g.byId['c']!.depth).toBe(1);
    expect(g.byId['d']!.depth).toBe(2);
    expect(g.criticalPathLength).toBe(3);
  });

  it('identifies entry and exit points', () => {
    const g = buildSpecGraph(diamond);
    expect(g.roots).toEqual(['a']);
    expect(g.leaves).toEqual(['d']);
  });

  it('records both directions of every dependency', () => {
    const g = buildSpecGraph(diamond);
    expect(g.byId['d']!.upstream).toEqual(['b', 'c']);
    expect(g.byId['a']!.downstream).toEqual(['b', 'c']);
  });

  /**
   * An edge naming a task that is not declared means the definition itself is
   * broken. Dropping it silently would render a broken spec as a healthy one,
   * so it is separated out and reported rather than filtered away.
   */
  it('separates edges naming a task the spec does not declare', () => {
    const g = buildSpecGraph({
      tasks: [{ id: 'a' }],
      edges: [
        { from_node_id: 'a', to_node_id: 'ghost' },
        { from_node_id: 'a', to_node_id: 'a' },
      ],
    });
    expect(g.edges).toHaveLength(0);
    expect(g.danglingEdges).toHaveLength(2);
  });

  it('reports a cycle rather than dropping the nodes inside it', () => {
    const g = buildSpecGraph({
      tasks: [{ id: 'a' }, { id: 'b' }],
      edges: [
        { from_node_id: 'a', to_node_id: 'b' },
        { from_node_id: 'b', to_node_id: 'a' },
      ],
    });
    expect(g.hasCycle).toBe(true);
    expect(g.nodes).toHaveLength(2);
  });

  it('tolerates an absent, empty or task-less document', () => {
    for (const doc of [undefined, {} as SpecDocument, { tasks: [] } as SpecDocument]) {
      const g = buildSpecGraph(doc);
      expect(g.nodes).toHaveLength(0);
      expect(g.criticalPathLength).toBe(0);
      expect(summarizeSpecGraph(g)).toBe('No tasks.');
    }
  });

  it('collects the distinct queues the definition names', () => {
    expect(specQueues(buildSpecGraph(diamond))).toEqual(['default', 'gpu']);
  });

  it('summarises structure without claiming anything ran', () => {
    expect(summarizeSpecGraph(buildSpecGraph(diamond))).toBe(
      '4 tasks · 4 dependencies · 3 deep.',
    );
  });
});

describe('the spec graph and the run graph agree', () => {
  /**
   * The same DAG, expressed both ways. This is the regression that would
   * otherwise be invisible: two layout implementations drifting apart so that
   * the catalog draws a workflow differently from the run that executed it.
   */
  it('produces identical layers for the same DAG expressed as a run', () => {
    const run: Pick<RunDetail, 'nodes' | 'edges'> = {
      nodes: diamond.tasks!.map((t) => ({
        run_id: 'r1',
        node_id: t.id,
        task_ref: t.task_ref ?? '',
        state: 'PENDING' as const,
      })),
      edges: diamond.edges!,
    };

    const specGraph = buildSpecGraph(diamond);
    const runGraph = buildDagGraph(run, []);

    expect(specGraph.layers).toEqual(runGraph.layers);
    for (const node of specGraph.nodes) {
      expect(node.depth).toBe(runGraph.byId[node.id]!.depth);
      expect(node.order).toBe(runGraph.byId[node.id]!.order);
    }
  });
});
