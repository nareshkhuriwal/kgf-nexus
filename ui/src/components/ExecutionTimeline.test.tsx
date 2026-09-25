// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The execution timeline as a component: it follows a live run, it agrees with
 * the page's selection in both directions, it stays a bounded DOM for huge
 * fan-outs, and everything it draws is reachable by keyboard and as a table.
 */

import { act, fireEvent, render, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Attempt, RunNode } from '../api/types';
import { ExecutionTimeline, type ExecutionTimelineProps } from './ExecutionTimeline';

const T0 = Date.parse('2026-09-25T10:00:00Z');
const iso = (ms: number) => new Date(T0 + ms).toISOString();

const node = (node_id: string, extra: Partial<RunNode> = {}): RunNode => ({
  run_id: 'r',
  node_id,
  task_ref: `t/${node_id}`,
  state: 'SUCCEEDED',
  ...extra,
});

function attempt(p: Partial<Attempt> & { node_id: string }): Attempt {
  return {
    id: `${p.node_id}#${p.attempt_number ?? 1}`,
    run_id: 'r',
    attempt_number: 1,
    state: 'SUCCEEDED',
    ...p,
  } as Attempt;
}

const nodes = [
  node('extract', { state: 'FAILED' }),
  node('fan'),
  node('fan[0]', { parent_node_id: 'fan', map_index: 0 }),
  node('fan[1]', { parent_node_id: 'fan', map_index: 1 }),
  node('load', { state: 'UPSTREAM_FAILED' }),
];
const attempts = [
  attempt({ node_id: 'extract', state: 'FAILED', scheduled_at: iso(0), dispatched_at: iso(1000), started_at: iso(1500), ended_at: iso(4000) }),
  attempt({ node_id: 'extract', attempt_number: 2, state: 'FAILED', scheduled_at: iso(5000), dispatched_at: iso(5200), started_at: iso(5300), ended_at: iso(6000) }),
  attempt({ node_id: 'fan[0]', scheduled_at: iso(100), dispatched_at: iso(200), started_at: iso(300), ended_at: iso(900) }),
  attempt({ node_id: 'fan[1]', scheduled_at: iso(100), dispatched_at: iso(250), started_at: iso(400), ended_at: iso(1200) }),
];

function setup(props: Partial<ExecutionTimelineProps> = {}) {
  const onSelectAttempt = vi.fn();
  const onSelectNode = vi.fn();
  const base: ExecutionTimelineProps = {
    run: { nodes, created_at: iso(0), started_at: iso(0), ended_at: iso(7000) },
    attempts,
    live: false,
    onSelectAttempt,
    onSelectNode,
    clock: () => T0 + 7000,
    ...props,
  };
  const utils = render(<ExecutionTimeline {...base} />);
  return {
    ...utils,
    onSelectAttempt,
    onSelectNode,
    rerenderWith: (next: Partial<ExecutionTimelineProps>) => utils.rerender(<ExecutionTimeline {...base} {...next} />),
  };
}

const list = () => screen.getByRole('list', { name: /execution timeline/i });
const rowButton = (name: RegExp) => within(list()).getByRole('button', { name });

afterEach(() => {
  vi.useRealTimers();
});

describe('what the chart shows', () => {
  it('draws one row per attempt with its phase durations, children under their template', () => {
    setup();
    const labels = within(list())
      .getAllByRole('button')
      .map((b) => b.getAttribute('aria-label'));
    expect(labels).toEqual([
      // Grouped by node: a retry sits directly under the try it repeats.
      expect.stringMatching(/^Attempt 1 of extract, FAILED; queued 1\.0s; starting 500ms; running 2\.5s/),
      expect.stringMatching(/^Attempt 2 of extract, FAILED/),
      expect.stringMatching(/^Map fan, 2 items/),
      expect.stringMatching(/^Attempt 1 of fan\[0\], SUCCEEDED/),
      expect.stringMatching(/^Attempt 1 of fan\[1\], SUCCEEDED/),
      expect.stringMatching(/^No attempt for load, UPSTREAM FAILED/),
    ]);
    // Three phases for a finished attempt, in the tone of its final state.
    const bars = rowButton(/^Attempt 1 of extract/).querySelectorAll('[data-phase]');
    expect([...bars].map((b) => b.getAttribute('data-phase'))).toEqual(['queued', 'starting', 'running']);
    expect(bars[2]!.className).toContain('tone-danger');
  });

  it('collapses and expands a fan-out from its header', () => {
    setup();
    const header = rowButton(/^Map fan, 2 items/);
    expect(header).toHaveAttribute('aria-expanded', 'true');
    fireEvent.click(header);
    expect(rowButton(/^Map fan, 2 items/)).toHaveAttribute('aria-expanded', 'false');
    expect(within(list()).queryByRole('button', { name: /^Attempt 1 of fan\[0\]/ })).toBeNull();
  });

  it('offers the same rows as a table', () => {
    const onModeChange = vi.fn();
    const { rerenderWith } = setup({ mode: 'chart', onModeChange });
    fireEvent.click(screen.getByRole('button', { name: 'Table' }));
    expect(onModeChange).toHaveBeenCalledWith('table');
    rerenderWith({ mode: 'table', onModeChange });
    const table = screen.getByRole('table');
    expect(within(table).getAllByRole('row')).toHaveLength(1 + 6);
    expect(within(table).getByRole('button', { name: 'Select extract attempt 2' })).toBeInTheDocument();
    expect(within(table).getAllByText('2.5s').length).toBeGreaterThan(0);
  });

  it('says so when nothing has been scheduled', () => {
    setup({ run: { nodes: [], created_at: iso(0) }, attempts: [] });
    expect(screen.getByText('Nothing has been scheduled yet')).toBeInTheDocument();
  });
});

describe('a live run', () => {
  it('draws a now-line and grows the running bar as the clock advances', () => {
    vi.useFakeTimers();
    vi.setSystemTime(T0 + 3000);
    const live = [
      attempt({ node_id: 'extract', state: 'RUNNING', scheduled_at: iso(0), dispatched_at: iso(500), started_at: iso(1000) }),
    ];
    setup({
      run: { nodes: [node('extract', { state: 'RUNNING' })], created_at: iso(0), started_at: iso(0) },
      attempts: live,
      live: true,
      clock: () => Date.now(),
      tickMs: 1000,
    });
    expect(screen.getByTestId('timeline-now')).toBeInTheDocument();
    expect(rowButton(/^Attempt 1 of extract/).getAttribute('aria-label')).toMatch(/running 2\.0s \(ongoing\)/);

    act(() => {
      vi.advanceTimersByTime(3000);
    });
    expect(rowButton(/^Attempt 1 of extract/).getAttribute('aria-label')).toMatch(/running 5\.0s \(ongoing\)/);
  });

  it('picks up attempts from the next poll and stops ticking once terminal', () => {
    vi.useFakeTimers();
    vi.setSystemTime(T0 + 3000);
    const first = [attempt({ node_id: 'extract', state: 'RUNNING', scheduled_at: iso(0), started_at: iso(1000) })];
    const { rerenderWith } = setup({
      run: { nodes: [node('extract'), node('load')], created_at: iso(0) },
      attempts: first,
      live: true,
      clock: () => Date.now(),
    });
    expect(within(list()).getAllByRole('button')).toHaveLength(2);

    const polled = [
      attempt({ node_id: 'extract', scheduled_at: iso(0), started_at: iso(1000), ended_at: iso(2500) }),
      attempt({ node_id: 'load', state: 'RUNNING', scheduled_at: iso(2600), started_at: iso(2700) }),
    ];
    rerenderWith({ run: { nodes: [node('extract'), node('load')], created_at: iso(0) }, attempts: polled, live: true, clock: () => Date.now() });
    expect(rowButton(/^Attempt 1 of extract/).getAttribute('aria-label')).toMatch(/running 1\.5s$/);
    expect(rowButton(/^Attempt 1 of load, RUNNING/)).toBeInTheDocument();

    const done = [polled[0]!, { ...polled[1]!, state: 'SUCCEEDED' as const, ended_at: iso(2900) }];
    rerenderWith({
      run: { nodes: [node('extract'), node('load')], created_at: iso(0), ended_at: iso(2900) },
      attempts: done,
      live: false,
      clock: () => Date.now(),
    });
    expect(screen.queryByTestId('timeline-now')).toBeNull();
    const label = rowButton(/^(No attempt for|Attempt \d of) load/).getAttribute('aria-label');
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    expect(rowButton(/^(No attempt for|Attempt \d of) load/).getAttribute('aria-label')).toBe(label);
  });

  it('keeps an operator zoom across polls, and Follow live returns to the whole run', () => {
    vi.useFakeTimers();
    vi.setSystemTime(T0 + 3000);
    const live = [attempt({ node_id: 'extract', state: 'RUNNING', scheduled_at: iso(0), started_at: iso(1000) })];
    const { rerenderWith } = setup({
      run: { nodes: [node('extract')], created_at: iso(0) },
      attempts: live,
      live: true,
      clock: () => Date.now(),
    });
    const follow = screen.getByRole('button', { name: 'Follow live' });
    expect(follow).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Zoom in' }));
    expect(follow).toBeEnabled();
    rerenderWith({ run: { nodes: [node('extract')], created_at: iso(0) }, attempts: [...live], live: true, clock: () => Date.now() });
    expect(screen.getByRole('button', { name: 'Follow live' })).toBeEnabled();
    fireEvent.click(screen.getByRole('button', { name: 'Follow live' }));
    expect(screen.getByRole('button', { name: 'Follow live' })).toBeDisabled();
  });
});

describe('selection sync', () => {
  it('reports a clicked attempt with its node', () => {
    const { onSelectAttempt, onSelectNode } = setup();
    fireEvent.click(rowButton(/^Attempt 2 of extract/));
    expect(onSelectAttempt).toHaveBeenCalledWith('extract', 'extract#2');
    fireEvent.click(rowButton(/^(No attempt for|Attempt \d of) load/));
    expect(onSelectNode).toHaveBeenCalledWith('load');
  });

  it('marks the attempt the page selected, and the latest attempt for a node-only selection', () => {
    const { rerenderWith } = setup({ selectedNodeId: 'extract', selectedAttemptId: 'extract#1' });
    expect(rowButton(/^Attempt 1 of extract/)).toHaveAttribute('aria-pressed', 'true');
    expect(rowButton(/^Attempt 2 of extract/)).toHaveAttribute('aria-pressed', 'false');

    // A DAG click sets only the node: the latest attempt is the one shown.
    rerenderWith({ selectedNodeId: 'extract', selectedAttemptId: null });
    expect(rowButton(/^Attempt 2 of extract/)).toHaveAttribute('aria-pressed', 'true');
  });

  it('expands a collapsed fan-out when the selection moves into it', () => {
    const { rerenderWith } = setup();
    fireEvent.click(rowButton(/^Map fan, 2 items/));
    expect(within(list()).queryByRole('button', { name: /^Attempt 1 of fan\[1\]/ })).toBeNull();
    rerenderWith({ selectedNodeId: 'fan[1]', selectedAttemptId: null });
    expect(rowButton(/^Attempt 1 of fan\[1\]/)).toHaveAttribute('aria-pressed', 'true');
  });
});

describe('scale and keyboard', () => {
  it('mounts a bounded number of rows for a fan-out of 2000 attempts', () => {
    const kids = Array.from({ length: 2000 }, (_, i) => node(`m[${i}]`, { parent_node_id: 'm', map_index: i }));
    const many = kids.map((k, i) =>
      attempt({ node_id: k.node_id, scheduled_at: iso(i * 10), started_at: iso(i * 10 + 5), ended_at: iso(i * 10 + 50) }),
    );
    setup({ run: { nodes: [node('m'), ...kids], created_at: iso(0), ended_at: iso(21_000) }, attempts: many });
    const items = within(list()).getAllByRole('listitem');
    expect(items.length).toBeLessThan(60);
    expect(items[0]).toHaveAttribute('aria-setsize', '2001');
    expect(screen.getByText('2000 attempts across 2000 nodes')).toBeInTheDocument();
  });

  it('moves between rows with the arrow keys and shows durations in a tooltip on focus', () => {
    setup();
    const first = rowButton(/^Attempt 1 of extract/);
    expect(first).toHaveAttribute('tabindex', '0');
    act(() => first.focus());
    const tip = screen.getByRole('tooltip');
    expect(within(tip).getByText('queued')).toBeInTheDocument();
    expect(within(tip).getByText('2.5s')).toBeInTheDocument();
    expect(first).toHaveAttribute('aria-describedby', tip.id);

    fireEvent.keyDown(first, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(rowButton(/^Attempt 2 of extract/));
    fireEvent.keyDown(document.activeElement!, { key: 'ArrowDown' });
    expect(document.activeElement).toBe(rowButton(/^Map fan, 2 items/));
    fireEvent.keyDown(document.activeElement!, { key: 'End' });
    expect(document.activeElement).toBe(rowButton(/^(No attempt for|Attempt \d of) load/));
  });

  it('zooms and fits from the keyboard', () => {
    setup();
    const fit = screen.getByRole('button', { name: 'Fit' });
    expect(fit).toBeDisabled();
    const first = rowButton(/^Attempt 1 of extract/);
    fireEvent.keyDown(first, { key: '+' });
    expect(fit).toBeEnabled();
    fireEvent.keyDown(first, { key: 'ArrowRight' });
    fireEvent.keyDown(first, { key: '0' });
    expect(fit).toBeDisabled();
  });
});
