// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { Attempt, RunDetail, RunExpansion } from '../api/types';
import { DagView } from '../components/DagView';
import { buildDagGraph } from './dag';
import { MAX_UNFOLDED, describeGroup, groupExpansions } from './expansion';

const node = (id: string, state: string, over: Record<string, unknown> = {}) => ({
  run_id: 'r1',
  node_id: id,
  task_ref: `t/${id}`,
  state,
  ...over,
});

/** list -> process (map, n children) -> summarize. */
function fanout(n: number, childState = 'SUCCEEDED') {
  const nodes = [node('list', 'SUCCEEDED'), node('process', 'RUNNING'), node('summarize', 'PENDING')];
  const edges = [
    { from_node_id: 'list', to_node_id: 'process' },
    { from_node_id: 'process', to_node_id: 'summarize' },
  ];
  for (let i = 0; i < n; i++) {
    const id = `process[${i}]`;
    nodes.push(node(id, i === 0 && childState !== 'SUCCEEDED' ? childState : 'SUCCEEDED', {
      parent_node_id: 'process',
      map_index: i,
    }));
    edges.push({ from_node_id: 'list', to_node_id: id });
  }
  const expansions: RunExpansion[] = [{
    template_node_id: 'process', source_node_id: 'list', source_output: 'files',
    item_count: n, max_fanout: 5000, outcome: 'EXPANDED',
  }];
  return { nodes, edges, expansions } as Pick<RunDetail, 'nodes' | 'edges' | 'expansions'>;
}

describe('groupExpansions', () => {
  it('folds children into their template by default and drops their edges', () => {
    const g = groupExpansions(fanout(3));
    expect(g.nodes.map((n) => n.node_id)).toEqual(['list', 'process', 'summarize']);
    expect(g.edges).toHaveLength(2);
    expect(g.groups.process).toMatchObject({ total: 3, unfolded: false, unfoldable: true });
    expect(g.parentOf['process[2]']).toBe('process');
  });

  it('unfolds a small template on request', () => {
    const g = groupExpansions(fanout(3), [], new Set(['process']));
    expect(g.nodes).toHaveLength(6);
    expect(g.edges).toHaveLength(5);
    expect(g.groups.process!.unfolded).toBe(true);
  });

  it('never unfolds a fan-out too large to read', () => {
    const g = groupExpansions(fanout(MAX_UNFOLDED + 1), [], new Set(['process']));
    expect(g.nodes).toHaveLength(3);
    expect(g.groups.process).toMatchObject({ unfolded: false, unfoldable: false, total: MAX_UNFOLDED + 1 });
  });

  it('counts states and cached children', () => {
    const attempts = [
      { id: 'a0', run_id: 'r1', node_id: 'process[1]', attempt_number: 1, state: 'SUCCEEDED', cache_hit_of: 'x' },
    ] as Attempt[];
    const g = groupExpansions(fanout(4, 'FAILED'), attempts);
    expect(g.groups.process!.counts).toMatchObject({ FAILED: 1, SUCCEEDED: 3 });
    expect(g.groups.process!.cached).toBe(1);
    expect(describeGroup(g.groups.process!)).toBe('4 items · 3 done · 1 failed · 1 cached');
  });

  it('explains a refused expansion', () => {
    const run = fanout(0);
    run.expansions = [{ ...run.expansions![0]!, outcome: 'FAILED', reason: 'max_fanout_exceeded' }];
    const g = groupExpansions(run);
    expect(describeGroup(g.groups.process!)).toBe('expansion refused: max_fanout_exceeded');
  });

  it('lays out a 5000-item fan-out as three nodes', () => {
    const g = groupExpansions(fanout(5000));
    const graph = buildDagGraph({ nodes: g.nodes, edges: g.edges }, []);
    expect(graph.nodes).toHaveLength(3);
  });
});

describe('the DAG view draws a folded template', () => {
  it('shows the summary and offers unfold', () => {
    const g = groupExpansions(fanout(3));
    const graph = buildDagGraph({ nodes: g.nodes, edges: g.edges }, []);
    const toggle = vi.fn();
    render(<DagView graph={graph} onSelect={() => {}} onToggleGroup={toggle} />);
    expect(screen.getAllByText(/3 items/).length).toBeGreaterThan(0);
    fireEvent.click(screen.getByRole('button', { name: /Unfold the 3 items of process/ }));
    expect(toggle).toHaveBeenCalledWith('process');
  });

  it('marks a memoized node as cached', () => {
    const graph = buildDagGraph(
      { nodes: [node('a', 'SUCCEEDED')], edges: [] } as Pick<RunDetail, 'nodes' | 'edges'>,
      [{ id: 'x', run_id: 'r1', node_id: 'a', attempt_number: 1, state: 'SUCCEEDED', cache_hit_of: 'src' } as Attempt],
    );
    render(<DagView graph={graph} onSelect={() => {}} />);
    expect(screen.getByText('cached')).toBeInTheDocument();
  });
});
