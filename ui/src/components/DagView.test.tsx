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
