// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the spec dependency graph.
 *
 * The distinction this component exists to hold is the one under test: a
 * definition has no state, so the diagram must show the declared shape and must
 * not carry any treatment an operator could read as live run information.
 */

import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { SpecDagView } from './SpecDagView';
import { buildSpecGraph } from '../lib/specgraph';
import type { SpecDocument } from '../api/types';

const chain: SpecDocument = {
  tasks: [
    { id: 'ingest', task_ref: 'ml.ingest', queue: 'default', resources: { request: { cpu_millis: 2000 } } },
    { id: 'train', task_ref: 'ml.train', queue: 'gpu', resources: { request: { gpu_count: 1 } } },
    { id: 'publish', task_ref: 'ml.publish', queue: 'default' },
  ],
  edges: [
    { from_node_id: 'ingest', to_node_id: 'train', output_to_input: { raw: 'result' } },
    { from_node_id: 'train', to_node_id: 'publish' },
  ],
};

describe('the spec dependency graph', () => {
  it('draws one selectable box per declared task', () => {
    render(<SpecDagView graph={buildSpecGraph(chain)} onSelect={() => {}} />);

    expect(screen.getByRole('img', { name: /3 tasks/i })).toBeInTheDocument();
    expect(screen.getAllByRole('button')).toHaveLength(3);
  });

  it('names entry and exit points in the accessible label', () => {
    render(<SpecDagView graph={buildSpecGraph(chain)} onSelect={() => {}} />);

    // Structure is the only thing a definition can assert about a task's
    // position, so it is the only thing the label claims.
    expect(screen.getByRole('button', { name: /ingest, ml.ingest, entry point/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /publish, ml.publish, exit point/ })).toBeInTheDocument();
  });

  it('shows the queue and resource request on the box, which decide schedulability', () => {
    render(<SpecDagView graph={buildSpecGraph(chain)} onSelect={() => {}} />);

    expect(screen.getByText('gpu')).toBeInTheDocument();
    expect(screen.getByText('1×gpu')).toBeInTheDocument();
  });

  it('states that nothing on the diagram has run', () => {
    render(<SpecDagView graph={buildSpecGraph(chain)} onSelect={() => {}} />);

    expect(screen.getByText(/No task here has run/)).toBeInTheDocument();
  });

  it('selects a task by click and by keyboard', () => {
    const onSelect = vi.fn();
    render(<SpecDagView graph={buildSpecGraph(chain)} onSelect={onSelect} />);

    fireEvent.click(screen.getByRole('button', { name: /^train, ml.train$/ }));
    expect(onSelect).toHaveBeenCalledWith('train');

    fireEvent.keyDown(screen.getByRole('button', { name: /^train, ml.train$/ }), { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledTimes(2);
  });

  it('says a definition declares no tasks rather than drawing an empty canvas', () => {
    render(<SpecDagView graph={buildSpecGraph({ tasks: [] })} onSelect={() => {}} />);

    expect(screen.getByText('This definition declares no tasks')).toBeInTheDocument();
  });

  /**
   * Both of these should be impossible — the gateway validates a spec at submit
   * time — which is exactly why a stored spec that exhibits them is worth
   * shouting about rather than rendering as a normal graph.
   */
  it('calls out a cycle in a stored definition', () => {
    const graph = buildSpecGraph({
      tasks: [{ id: 'a' }, { id: 'b' }],
      edges: [
        { from_node_id: 'a', to_node_id: 'b' },
        { from_node_id: 'b', to_node_id: 'a' },
      ],
    });
    render(<SpecDagView graph={graph} onSelect={() => {}} />);

    expect(screen.getByRole('alert')).toHaveTextContent('This definition contains a cycle');
  });

  it('names edges pointing at a task the spec does not declare', () => {
    const graph = buildSpecGraph({
      tasks: [{ id: 'a' }],
      edges: [{ from_node_id: 'a', to_node_id: 'ghost' }],
    });
    render(<SpecDagView graph={graph} onSelect={() => {}} />);

    expect(screen.getByRole('alert')).toHaveTextContent('a → ghost');
  });
});
