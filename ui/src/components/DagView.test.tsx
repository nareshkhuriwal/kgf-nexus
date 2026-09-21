// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the DAG view.
 *
 * The component's stated job is to make "what actually broke" answerable at a
 * glance, and to stay answerable in greyscale and to a colour-blind operator.
 * That is carried by redundant channels — each node exposes its state as TEXT
 * and in its accessible name, not by colour alone — and by marking the root
 * cause as distinct from the nodes that merely inherited the failure.
 *
 * These tests deliberately do not assert on fill colours, stroke dash values
 * or coordinates: those are the redesign's to change. They assert that the
 * state is READABLE and that the root cause is IDENTIFIED.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { buildDagGraph } from '../lib/dag';
import type { Attempt, RunDetail } from '../api/types';
import { DagView } from './DagView';

const node = (id: string, state: string, over: Record<string, unknown> = {}) => ({
  run_id: 'r1',
  node_id: id,
  task_ref: `tasks/${id}`,
  state,
  ...over,
});

/** extract → transform → load, where extract broke and the rest never ran. */
function cascade() {
  const run = {
    nodes: [
      node('extract', 'FAILED'),
      node('transform', 'UPSTREAM_FAILED'),
      node('load', 'UPSTREAM_FAILED'),
    ],
    edges: [
      { from_node_id: 'extract', to_node_id: 'transform' },
      { from_node_id: 'transform', to_node_id: 'load' },
    ],
  } as Pick<RunDetail, 'nodes' | 'edges'>;
  return buildDagGraph(run, []);
}

describe('every node is readable without colour', () => {
  it('exposes each node as a control whose accessible name carries its state', () => {
    render(<DagView graph={cascade()} onSelect={() => {}} />);

    // The accessible name is the channel that survives greyscale, a screen
    // reader, and a redesign of the fill palette.
    expect(screen.getByRole('button', { name: /^extract, FAILED/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^transform, UPSTREAM FAILED/ })).toBeInTheDocument();
  });

  /**
   * CONTRACT.md invariant 2 at the point of render: "this broke" and "this
   * never ran because something upstream broke" must not read alike. Here the
   * two states differ in their rendered label as well as their tone.
   */
  it('never lets UPSTREAM_FAILED read as FAILED', () => {
    render(<DagView graph={cascade()} onSelect={() => {}} />);

    const failed = screen.getByRole('button', { name: /^extract,/ });
    const blocked = screen.getByRole('button', { name: /^transform,/ });
    expect(failed.getAttribute('aria-label')).not.toBe(blocked.getAttribute('aria-label'));
    // And the distinction is stated in the node body, not only in its colour.
    expect(within(failed).getByText('FAILED')).toBeInTheDocument();
    expect(within(blocked).getByText('UPSTREAM FAILED')).toBeInTheDocument();
  });
});

describe('root cause attribution', () => {
  /**
   * The origin of a cascade is the one node worth opening. UPSTREAM_FAILED
   * nodes are collateral by construction, and calling them out too would
   * destroy the signal.
   */
  it('marks only the origin of the cascade as the root cause', () => {
    render(<DagView graph={cascade()} onSelect={() => {}} />);

    expect(screen.getByRole('button', { name: /extract.*root cause/i })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /transform.*root cause/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /load.*root cause/i })).not.toBeInTheDocument();
  });

  it('marks nothing as a root cause in a healthy run', () => {
    const graph = buildDagGraph(
      {
        nodes: [node('a', 'SUCCEEDED'), node('b', 'RUNNING')],
        edges: [{ from_node_id: 'a', to_node_id: 'b' }],
      } as Pick<RunDetail, 'nodes' | 'edges'>,
      [],
    );
    render(<DagView graph={graph} onSelect={() => {}} />);
    expect(screen.queryByRole('button', { name: /root cause/i })).not.toBeInTheDocument();
  });
});

describe('selection', () => {
  it('reports the clicked node to its caller', () => {
    const onSelect = vi.fn();
    render(<DagView graph={cascade()} onSelect={onSelect} />);

    fireEvent.click(screen.getByRole('button', { name: /^extract,/ }));
    expect(onSelect).toHaveBeenCalledWith('extract');
  });

  it('is operable from the keyboard, not only by mouse', () => {
    const onSelect = vi.fn();
    render(<DagView graph={cascade()} onSelect={onSelect} />);

    const target = screen.getByRole('button', { name: /^transform,/ });
    fireEvent.keyDown(target, { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledWith('transform');

    fireEvent.keyDown(target, { key: ' ' });
    expect(onSelect).toHaveBeenCalledTimes(2);
  });
});

describe('degenerate graphs are surfaced, not hidden', () => {
  /**
   * A run whose spec has not been expanded into nodes yet is a legitimate
   * state; an empty box with no explanation is not.
   */
  it('explains an empty graph instead of rendering nothing', () => {
    const graph = buildDagGraph({ nodes: [], edges: [] } as Pick<RunDetail, 'nodes' | 'edges'>, []);
    render(<DagView graph={graph} onSelect={() => {}} />);

    expect(screen.getByText(/No nodes materialised/i)).toBeInTheDocument();
    expect(screen.queryByRole('img')).not.toBeInTheDocument();
  });

  /**
   * Acyclicity is validated at submit time, so a cycle here means the spec or
   * the node projection is wrong. Drawing it silently with unreliable depths
   * would let a real data bug pass as a layout quirk.
   */
  it('warns loudly when the graph contains a cycle', () => {
    const graph = buildDagGraph(
      {
        nodes: [node('a', 'RUNNING'), node('b', 'RUNNING')],
        edges: [
          { from_node_id: 'a', to_node_id: 'b' },
          { from_node_id: 'b', to_node_id: 'a' },
        ],
      } as Pick<RunDetail, 'nodes' | 'edges'>,
      [],
    );
    render(<DagView graph={graph} onSelect={() => {}} />);

    expect(screen.getByRole('alert')).toHaveTextContent(/cycle/i);
    // The nodes are still drawn: hiding them during an incident is worse.
    expect(screen.getByRole('button', { name: /^a,/ })).toBeInTheDocument();
  });
});

describe('the node body carries diagnostic detail', () => {
  it('shows the retry count when a node has been attempted more than once', () => {
    const attempts: Attempt[] = [1, 2, 3].map((n) => ({
      id: `att-${n}`,
      run_id: 'r1',
      node_id: 'extract',
      attempt_number: n,
      state: n === 3 ? 'FAILED' : 'FAILED',
    }));
    const graph = buildDagGraph(
      { nodes: [node('extract', 'FAILED')], edges: [] } as Pick<RunDetail, 'nodes' | 'edges'>,
      attempts,
    );
    render(<DagView graph={graph} onSelect={() => {}} />);

    // "It failed once" and "it failed three times" are different bugs.
    expect(screen.getByText('3 attempts')).toBeInTheDocument();
  });

  /**
   * A stalled node looks identical to a merely-waiting one unless the reason is
   * surfaced. This is the difference between an unexplained stall and
   * "waiting_for_resources: gpu (need 4, free 3)".
   */
  it('marks a node stalled when the scheduler recorded a pending reason', () => {
    const graph = buildDagGraph(
      {
        nodes: [node('train', 'PENDING', { pending_reason: 'waiting_for_resources: gpu (need 4, free 3)' })],
        edges: [],
      } as Pick<RunDetail, 'nodes' | 'edges'>,
      [],
    );
    render(<DagView graph={graph} onSelect={() => {}} />);

    expect(screen.getByText('stalled')).toBeInTheDocument();
    // The reason itself is reachable on the node, not buried in another panel.
    expect(screen.getByRole('button', { name: /^train,/ })).toHaveTextContent(
      /waiting_for_resources/,
    );
  });
});

/**
 * Connectedness.
 *
 * The reported symptom was a graph whose nodes rendered as a bare stack with no
 * visible connections, so "an edge exists for every dependency and it points at
 * its target" is now asserted rather than assumed. These tests would have
 * caught the rendering half of that bug; the other half was the gateway
 * omitting `edges` from the run payload entirely, which no UI test can see.
 */
describe('edges read as connections', () => {
  const svg = () => document.querySelector('svg')!;

  it('draws one edge path per dependency', () => {
    render(<DagView graph={cascade()} onSelect={() => {}} />);
    expect(svg().querySelectorAll('path.dag-edge')).toHaveLength(2);
  });

  it('points every edge at its target with an arrowhead', () => {
    render(<DagView graph={cascade()} onSelect={() => {}} />);
    for (const p of svg().querySelectorAll('path.dag-edge')) {
      // Direction is the whole point: an undirected line cannot express a DAG.
      expect(p.getAttribute('marker-end')).toMatch(/^url\(#arrow/);
      expect(p.getAttribute('d')).toBeTruthy();
      expect(p.getAttribute('d')).not.toContain('NaN');
    }
  });

  it('distinguishes the path a failure propagated along', () => {
    render(<DagView graph={cascade()} onSelect={() => {}} />);
    // A cascade must read as a cascade, so its edges are marked and get their
    // own arrowhead rather than sharing the neutral one.
    const failed = svg().querySelectorAll('path.dag-edge.failed-path');
    expect(failed.length).toBeGreaterThan(0);
    for (const p of failed) expect(p.getAttribute('marker-end')).toBe('url(#arrow-failed)');
  });

  it('draws no edges for a graph that has none, without failing', () => {
    const graph = buildDagGraph(
      { nodes: [node('a', 'SUCCEEDED'), node('b', 'SUCCEEDED')], edges: [] } as Pick<
        RunDetail,
        'nodes' | 'edges'
      >,
      [],
    );
    render(<DagView graph={graph} onSelect={() => {}} />);
    expect(svg().querySelectorAll('path.dag-edge')).toHaveLength(0);
    expect(screen.getByRole('button', { name: /^a,/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^b,/ })).toBeInTheDocument();
  });

  /**
   * An edgeless graph used to stack every node into one very tall column,
   * because with no edges every node is a root and lands at depth 0. It must
   * wrap into a grid instead, which is observable as more than one distinct x
   * among the node transforms.
   */
  it('lays an edgeless graph out as a grid rather than one tall column', () => {
    const ids = ['n0', 'n1', 'n2', 'n3', 'n4', 'n5', 'n6', 'n7', 'n8', 'n9'];
    const graph = buildDagGraph(
      { nodes: ids.map((i) => node(i, 'SUCCEEDED')), edges: [] } as Pick<
        RunDetail,
        'nodes' | 'edges'
      >,
      [],
    );
    render(<DagView graph={graph} onSelect={() => {}} />);

    const cols = new Set<string>();
    const rows = new Set<string>();
    for (const g of svg().querySelectorAll('g.dag-node-g')) {
      const m = /translate\((-?[\d.]+),(-?[\d.]+)\)/.exec(g.getAttribute('transform') ?? '');
      if (m) {
        cols.add(m[1]!);
        rows.add(m[2]!);
      }
    }
    expect(cols.size).toBeGreaterThan(1);
    expect(rows.size).toBeGreaterThan(1);
    // Wider than tall, which is what distinguishes a grid from the old ribbon.
    //
    // Read off the viewBox rather than width/height. Those attributes are now
    // 100%: the SVG sizes to its CONTAINER and the zoom transform decides how
    // much of the graph is visible, which is the fix for the graph overflowing
    // its panel with its first and last node off-screen. The viewBox is where
    // the intrinsic layout extent lives, so it is what a layout assertion has
    // to look at -- and asserting the rendered width would now be asserting
    // the bug back into existence.
    const [, , vbW, vbH] = (svg().getAttribute('viewBox') ?? '').split(/\s+/).map(Number);
    expect(vbW).toBeGreaterThan(vbH!);
  });
});

/**
 * The four redundant state channels are a hard accessibility requirement, not a
 * styling preference: fill, border colour, border dash and a glyph, so the view
 * survives greyscale and colour blindness. A redesign is free to retune them but
 * not to drop one, so their PRESENCE is pinned here.
 */
describe('the four redundant state channels survive the restyle', () => {
  const graph = () =>
    buildDagGraph(
      {
        nodes: [node('waiting', 'PENDING'), node('done', 'SUCCEEDED')],
        edges: [],
      } as Pick<RunDetail, 'nodes' | 'edges'>,
      [],
    );

  it('keeps a tone class on every node, carrying fill and border colour', () => {
    render(<DagView graph={graph()} onSelect={() => {}} />);
    for (const g of document.querySelectorAll('g.dag-node-g')) {
      // getAttribute, not .className: on an SVG element that property is an
      // SVGAnimatedString rather than a string.
      expect(g.getAttribute('class')).toMatch(/tone-/);
      expect(g.querySelector('rect.dag-node')).toBeTruthy();
    }
  });

  it('keeps the border dash channel, which distinguishes states in greyscale', () => {
    render(<DagView graph={graph()} onSelect={() => {}} />);
    const dashes = [...document.querySelectorAll('rect.dag-node')].map((r) =>
      r.getAttribute('stroke-dasharray'),
    );
    // PENDING is dashed and SUCCEEDED is solid, so the two must not agree.
    expect(new Set(dashes).size).toBeGreaterThan(1);
  });

  it('keeps a glyph on every node, which is the channel that needs no colour', () => {
    render(<DagView graph={graph()} onSelect={() => {}} />);
    for (const g of document.querySelectorAll('g.dag-node-g')) {
      expect(g.querySelector('text.dag-glyph')?.textContent).toBeTruthy();
    }
  });

  it('keeps selection separable from state, so neither masks the other', () => {
    render(<DagView graph={graph()} selectedNodeId="done" onSelect={() => {}} />);
    const selected = document.querySelectorAll('rect.dag-node.selected');
    expect(selected).toHaveLength(1);
    // Selection must not have consumed the state's dash channel.
    expect(
      document.querySelector('g.dag-node-g.selected rect.dag-node')?.getAttribute('class'),
    ).toContain('dag-node');
  });
});

it('jumps to a task from the graph toolbar', () => {
  const onSelect = vi.fn();
  render(<DagView graph={cascade()} onSelect={onSelect} />);
  fireEvent.change(screen.getByRole('combobox', { name: 'Jump to task' }), { target: { value: 'load' } });
  expect(onSelect).toHaveBeenCalledWith('load');
  expect(document.querySelector('.dag-zoom')?.getAttribute('transform')).toContain('scale(1)');
});
