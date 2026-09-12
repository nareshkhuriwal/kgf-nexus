/**
 * Render tests for the run event timeline.
 *
 * run_events is the authoritative record — the current-state tables are a
 * projection of it, and on disagreement the log wins. Two properties follow
 * from that and are pinned here: ordering is by `seq` (the run-scoped ordering
 * the schema guarantees unique, unlike occurred_at which can tie or skew), and
 * every row is attributable to an actor, because "who caused this transition"
 * is the question an incident review actually asks.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { RunEvent } from '../api/types';
import { EventTimeline, actorKind } from './EventTimeline';

const event = (over: Partial<RunEvent> = {}): RunEvent => ({
  id: 1,
  run_id: 'r1',
  seq: 1,
  kind: 'run.created',
  actor: 'scheduler',
  occurred_at: '2026-09-12T10:00:00Z',
  ...over,
});

describe('ordering', () => {
  /**
   * seq, not occurred_at: two events written in the same millisecond tie on
   * the timestamp, and a skewed clock can invert them outright. Reading the
   * history in the wrong order during a review invents causality that did not
   * happen.
   */
  it('orders newest first by seq, even when the timestamps disagree', () => {
    render(
      <EventTimeline
        events={[
          event({ id: 1, seq: 1, kind: 'run.created', occurred_at: '2026-09-12T10:00:05Z' }),
          event({ id: 2, seq: 2, kind: 'node.started', occurred_at: '2026-09-12T10:00:00Z' }),
          event({ id: 3, seq: 3, kind: 'node.failed', occurred_at: '2026-09-12T10:00:00Z' }),
        ]}
      />,
    );

    // Scoped to the table because the kind filter's <option> list repeats every
    // kind; the ORDER of the kinds down the rows is the actual claim.
    const table = within(screen.getByRole('table'));
    const kinds = table.getAllByText(/^(run\.created|node\.started|node\.failed)$/);
    expect(kinds.map((k) => k.textContent)).toEqual(['node.failed', 'node.started', 'run.created']);
  });
});

describe('attribution', () => {
  it('shows who caused each transition', () => {
    render(
      <EventTimeline
        events={[
          event({ id: 1, seq: 1, actor: 'scheduler' }),
          event({ id: 2, seq: 2, actor: 'worker:wk-1111111122222', kind: 'attempt.started' }),
          event({ id: 3, seq: 3, actor: 'user:ops@example.com', kind: 'run.cancelled' }),
        ]}
      />,
    );

    // Scoped to the rows: the actor filter's <option> list repeats each kind.
    const table = within(screen.getByRole('table'));
    expect(table.getByText('scheduler')).toBeInTheDocument();
    // A user action and a scheduler action are different facts in a review, so
    // the user is named in full rather than reduced to a kind.
    expect(table.getByText('user:ops@example.com')).toBeInTheDocument();
    expect(table.getByText(/^worker:/)).toBeInTheDocument();
  });

  it('classifies actors by their schema-guaranteed prefix', () => {
    // scheduler | worker:<id> | user:<sub> is a contract, not a convention.
    expect(actorKind('scheduler')).toBe('scheduler');
    expect(actorKind('worker:wk-1')).toBe('worker');
    expect(actorKind('user:ops@example.com')).toBe('user');
    // Anything unrecognised still classifies rather than throwing.
    expect(actorKind('cron')).toBe('system');
  });
});

describe('filtering', () => {
  const events = [
    event({ id: 1, seq: 1, kind: 'run.created', actor: 'scheduler' }),
    event({ id: 2, seq: 2, kind: 'attempt.started', actor: 'worker:wk-1' }),
    event({ id: 3, seq: 3, kind: 'attempt.failed', actor: 'worker:wk-1' }),
  ];

  it('narrows the log to one event kind', () => {
    render(<EventTimeline events={events} />);

    const selects = screen.getAllByRole('combobox');
    fireEvent.change(selects[0]!, { target: { value: 'attempt.failed' } });

    const table = within(screen.getByRole('table'));
    expect(table.getByText('attempt.failed')).toBeInTheDocument();
    expect(table.queryByText('run.created')).not.toBeInTheDocument();
  });

  it('narrows the log to one actor kind', () => {
    render(<EventTimeline events={events} />);

    const selects = screen.getAllByRole('combobox');
    fireEvent.change(selects[1]!, { target: { value: 'scheduler' } });

    const table = within(screen.getByRole('table'));
    expect(table.getByText('run.created')).toBeInTheDocument();
    expect(table.queryByText('attempt.started')).not.toBeInTheDocument();
  });

  /**
   * Filtering must not be mistakable for an empty history — the count of what
   * is shown stays visible so a filtered view cannot read as "nothing
   * happened".
   */
  it('keeps the unfiltered total visible while a filter is applied', () => {
    render(<EventTimeline events={events} />);
    expect(screen.getByText(/all \(3\)/)).toBeInTheDocument();
  });
});

describe('navigation into the graph', () => {
  it('turns a node reference into a control that selects that node', () => {
    const onSelectNode = vi.fn();
    render(
      <EventTimeline
        events={[event({ id: 1, seq: 1, kind: 'node.failed', node_id: 'extract' })]}
        onSelectNode={onSelectNode}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'extract' }));
    // The timeline answers "when", the graph answers "where"; this is the jump
    // between them that keeps an incident loop tight.
    expect(onSelectNode).toHaveBeenCalledWith('extract');
  });

  it('still shows the node when there is nowhere to navigate', () => {
    render(<EventTimeline events={[event({ id: 1, seq: 1, node_id: 'extract' })]} />);
    expect(screen.getByText('extract')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'extract' })).not.toBeInTheDocument();
  });
});

describe('payloads', () => {
  it('renders flat payloads inline, where they can be scanned', () => {
    render(
      <EventTimeline
        events={[event({ id: 1, seq: 1, payload: { exit_code: 1, signal: 'SIGKILL' } })]}
      />,
    );
    // Inline key=value beats pretty-printed JSON in a dense table.
    expect(screen.getByText(/exit_code=1/)).toBeInTheDocument();
    expect(screen.getByText(/signal=SIGKILL/)).toBeInTheDocument();
  });

  it('keeps a nested payload collapsed until asked for', () => {
    render(
      <EventTimeline
        events={[event({ id: 1, seq: 1, payload: { error: { class: 'OOM', limit: 4096 } } })]}
      />,
    );

    expect(screen.queryByText(/"class"/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /1 field/ }));
    expect(screen.getByText(/"class": "OOM"/)).toBeInTheDocument();
  });
});

describe('empty history', () => {
  it('explains that an empty log may mean the writer is down, not that nothing happened', () => {
    render(<EventTimeline events={[]} />);
    const empty = screen.getByRole('status');
    // "Nothing has happened" and "the event-service is not writing" look
    // identical from here, so the empty state must name both.
    expect(empty).toHaveTextContent(/event-service is not writing/i);
  });
});
