// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The executor's status (F96) on the attempt card, the placement panel and
 * the graph.
 *
 * Pinned here: the executor's status reaches every surface an operator reads;
 * it is never presented as the SCHEDULER's placement reason (they come from
 * two different schedulers); a finished attempt's status reads as history;
 * and the empty states are honest: "no status reported" is said only when the
 * gateway serves the field and the attempt is actually on a worker.
 */

import { render, screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { Attempt, RunDetail } from '../api/types';
import { buildDagGraph } from '../lib/dag';
import { executorStatus, isExecutorWaiting, progressFraction, progressPhase } from '../lib/progress';
import { AttemptList } from './AttemptDetail';
import { DagView } from './DagView';
import { StatusCell } from './PlacementPanel';

const POD = 'pod Pending: Unschedulable: 0/3 nodes are available: 3 Insufficient cpu';

const attempt = (over: Partial<Attempt> = {}): Attempt => ({
  id: 'att-1',
  run_id: 'r1',
  node_id: 'train',
  attempt_number: 1,
  state: 'RUNNING',
  worker_id: 'w-1',
  worker_hostname: 'k8s-agent-1',
  ...over,
});

describe('progress helpers', () => {
  it('takes the phase before the first ": "', () => {
    expect(progressPhase(POD)).toBe('pod Pending');
    expect(progressPhase('pod Running')).toBe('pod Running');
    expect(progressPhase('')).toBeNull();
    expect(progressPhase(null)).toBeNull();
  });

  it('flags a placed attempt whose pod is still pending, and nothing else', () => {
    expect(isExecutorWaiting(attempt({ progress_message: POD }))).toBe(true);
    expect(isExecutorWaiting(attempt({ progress_message: 'pod Running' }))).toBe(false);
    // Finished: the pending pod is history, not a current stall.
    expect(isExecutorWaiting(attempt({ state: 'FAILED', progress_message: POD }))).toBe(false);
  });

  it('marks a terminal attempt status as stale', () => {
    expect(executorStatus(attempt({ progress_message: POD }))?.stale).toBe(false);
    expect(executorStatus(attempt({ state: 'SUCCEEDED', progress_message: 'pod Succeeded' }))?.stale).toBe(true);
    expect(executorStatus(attempt({ progress_message: null }))).toBeNull();
  });

  it('refuses a fraction that is not a number in [0,1]', () => {
    expect(progressFraction(attempt({ progress_fraction: 0.4 }))).toBe(0.4);
    expect(progressFraction(attempt({ progress_fraction: 1.4 }))).toBeNull();
    expect(progressFraction(attempt({ progress_fraction: null }))).toBeNull();
  });
});

describe('the attempt card', () => {
  it('shows the executor status as the worker’s, not as the scheduler’s reason', () => {
    render(
      <AttemptList
        attempts={[attempt({ progress_message: POD, progress_fraction: 0.4, progress_at: '2026-09-24T12:00:00Z' })]}
      />,
    );
    const box = screen.getByTestId('executor-status');
    expect(within(box).getByText(/Executor status \(reported by the worker\)/)).toBeInTheDocument();
    expect(within(box).getByText(POD)).toBeInTheDocument();
    expect(within(box).getByText('40%')).toBeInTheDocument();
    expect(within(box).getByText(/cluster's scheduler, not from Nexus/)).toBeInTheDocument();
    // Never under the scheduler's heading.
    expect(screen.queryByText(/Why this has not dispatched/)).not.toBeInTheDocument();
  });

  it('keeps the two sources apart when both exist', () => {
    render(
      <AttemptList
        attempts={[
          attempt({
            pending_reason: 'waiting_for_resources: cpu (need 8000, free 2000)',
            progress_message: POD,
          }),
        ]}
      />,
    );
    expect(screen.getByText(/Last pending reason \(scheduler placement, historical\)/)).toBeInTheDocument();
    expect(within(screen.getByTestId('executor-status')).getByText(POD)).toBeInTheDocument();
  });

  it('labels a finished attempt’s status as historical', () => {
    render(<AttemptList attempts={[attempt({ state: 'FAILED', progress_message: 'pod Failed: OOMKilled' })]} />);
    expect(screen.getByText(/Last executor status \(historical/)).toBeInTheDocument();
  });

  it('says "no status reported" only for an in-flight attempt on a gateway that serves the field', () => {
    const { unmount } = render(<AttemptList attempts={[attempt({ progress_message: null })]} />);
    expect(screen.getByText(/No status reported/)).toBeInTheDocument();
    unmount();

    // Field absent: an older gateway. Claiming "nothing reported" would be false.
    const old = render(<AttemptList attempts={[attempt()]} />);
    expect(screen.queryByText(/No status reported/)).not.toBeInTheDocument();
    old.unmount();

    // PENDING: no executor exists yet, so no executor section at all.
    render(<AttemptList attempts={[attempt({ state: 'PENDING', worker_id: null, progress_message: null })]} />);
    expect(screen.queryByText(/Executor status/)).not.toBeInTheDocument();
  });

  it('renders a hostile message as inert text', () => {
    render(<AttemptList attempts={[attempt({ progress_message: '<img src=x onerror=alert(1)>' })]} />);
    expect(screen.getByText('<img src=x onerror=alert(1)>')).toBeInTheDocument();
    expect(document.querySelector('img')).toBeNull();
  });
});

describe('the placement panel status column', () => {
  const cell = (a: Attempt | null) => render(<StatusCell attempt={a} />);

  it('attributes a pending reason to the scheduler', () => {
    cell(attempt({ state: 'PENDING', worker_id: null, pending_reason: 'waiting_for_resources: gpu' }));
    expect(screen.getByText('scheduler')).toBeInTheDocument();
    expect(screen.getByText('waiting_for_resources: gpu')).toBeInTheDocument();
  });

  it('attributes a pod status to the executor', () => {
    cell(attempt({ progress_message: POD }));
    expect(screen.getByText('executor')).toBeInTheDocument();
    expect(screen.getByText(POD)).toBeInTheDocument();
  });

  it('is honest when there is nothing to say', () => {
    const { unmount } = cell(attempt({ progress_message: null }));
    expect(screen.getByText('no executor status reported')).toBeInTheDocument();
    unmount();
    cell(attempt());
    expect(screen.queryByText('no executor status reported')).not.toBeInTheDocument();
  });
});

describe('the graph', () => {
  const graphWith = (a: Attempt) =>
    buildDagGraph(
      {
        nodes: [{ run_id: 'r1', node_id: 'train', task_ref: 'ml/train', state: 'RUNNING' }],
        edges: [],
      } as Pick<RunDetail, 'nodes' | 'edges'>,
      [a],
    );

  it('badges a RUNNING node whose pod is still pending, and labels the tooltip by source', () => {
    render(<DagView graph={graphWith(attempt({ progress_message: POD }))} onSelect={() => {}} />);
    expect(screen.getByText('pod pending')).toBeInTheDocument();
    const node = screen.getByRole('button', { name: /^train, .*executor reports pod pending/ });
    expect(node).toHaveTextContent(`executor: ${POD}`);
    expect(node).not.toHaveTextContent('scheduler:');
  });

  it('shows no badge once the pod runs, or for a finished attempt', () => {
    const g = graphWith(attempt({ progress_message: 'pod Running' }));
    expect(g.byId.train?.executorWaiting).toBe(false);
    expect(g.byId.train?.executorStatus).toBe('pod Running');
    const done = graphWith(attempt({ state: 'SUCCEEDED', progress_message: POD }));
    expect(done.byId.train?.executorWaiting).toBe(false);
    expect(done.byId.train?.executorStatus).toBeNull();
    render(<DagView graph={g} onSelect={() => {}} />);
    expect(screen.queryByText('pod pending')).not.toBeInTheDocument();
  });
});
