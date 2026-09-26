// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Render tests for the attempt history.
 *
 * The component's stated reason to exist: a retry is a NEW attempt row and a
 * terminal attempt is never mutated (invariant 9), so collapsing retries into
 * "the node failed 3 times" throws away what makes a flaky task diagnosable.
 * What is pinned here is therefore that every attempt survives as its own
 * entry, newest first, and that the two facts an operator cannot reconstruct
 * from anywhere else — why a PENDING attempt has not dispatched, and that LOST
 * is not FAILED — are stated on the card.
 */

import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { Attempt } from '../api/types';
import { AttemptList } from './AttemptDetail';

const attempt = (over: Partial<Attempt> = {}): Attempt => ({
  id: 'att-1',
  run_id: 'r1',
  node_id: 'extract',
  attempt_number: 1,
  state: 'SUCCEEDED',
  ...over,
});

describe('every attempt survives as its own entry', () => {
  /**
   * Three tries against three different workers is a placement problem; three
   * tries against one worker is a code problem. Collapsing them loses that.
   */
  it('renders one entry per attempt, newest first', () => {
    render(
      <AttemptList
        attempts={[
          attempt({ id: 'a1', attempt_number: 1, state: 'FAILED', worker_hostname: 'agent-1' }),
          attempt({ id: 'a2', attempt_number: 2, state: 'FAILED', worker_hostname: 'agent-2' }),
          attempt({ id: 'a3', attempt_number: 3, state: 'RUNNING', worker_hostname: 'agent-3' }),
        ]}
      />,
    );

    const headings = screen.getAllByText(/^attempt #\d+$/);
    expect(headings.map((h) => h.textContent)).toEqual(['attempt #3', 'attempt #2', 'attempt #1']);
    // Each attempt keeps its own worker, which is the placement evidence.
    expect(screen.getByText('agent-1')).toBeInTheDocument();
    expect(screen.getByText('agent-3')).toBeInTheDocument();
  });

  it('marks which attempt is the current one and which are retries', () => {
    render(
      <AttemptList
        attempts={[attempt({ id: 'a1', attempt_number: 1 }), attempt({ id: 'a2', attempt_number: 2 })]}
      />,
    );

    const latest = screen.getByText('attempt #2').closest('section')!;
    const first = screen.getByText('attempt #1').closest('section')!;
    expect(within(latest).getByText('latest')).toBeInTheDocument();
    expect(within(latest).getByText('retry')).toBeInTheDocument();
    // The first attempt is not a retry — calling it one would misreport history.
    expect(within(first).queryByText('retry')).not.toBeInTheDocument();
  });

  it('teaches rather than blanking when a node has no attempts yet', () => {
    render(<AttemptList attempts={[]} />);
    expect(screen.getByRole('status')).toHaveTextContent(/trigger rule/i);
  });
});

describe('LOST is not FAILED', () => {
  /**
   * CONTRACT.md invariant 2. A LOST attempt's lease expired: we do not know
   * whether it ran, is still running, or had side effects. Rendering that as a
   * failure would tell an operator it is safe to re-run something that may be
   * executing right now.
   */
  it('says the outcome is unknown, and that it is not a failure', () => {
    render(<AttemptList attempts={[attempt({ state: 'LOST' })]} />);

    const card = screen.getByText('attempt #1').closest('section')!;
    expect(within(card).getByText(/do not know whether this attempt ran/i)).toBeInTheDocument();
    expect(within(card).getByText(/not FAILED/)).toBeInTheDocument();
  });

  it('gives LOST a different badge treatment from FAILED', () => {
    const { rerender } = render(<AttemptList attempts={[attempt({ state: 'LOST' })]} />);
    const lost = screen.getByText('LOST', { selector: '[data-state]' }).getAttribute('data-tone');

    rerender(<AttemptList attempts={[attempt({ state: 'FAILED' })]} />);
    const failed = screen.getByText('FAILED', { selector: '[data-state]' }).getAttribute('data-tone');

    expect(lost).not.toBe(failed);
  });

  it('does not claim the outcome is unknown once a real error was reported', () => {
    render(
      <AttemptList
        attempts={[attempt({ state: 'FAILED', error: { class: 'APPLICATION', message: 'boom' } })]}
      />,
    );
    expect(screen.queryByText(/do not know whether this attempt ran/i)).not.toBeInTheDocument();
    expect(screen.getByText('boom')).toBeInTheDocument();
  });
});

describe('why an attempt has not dispatched', () => {
  /**
   * pending_reason is the difference between an unexplained stall and
   * "waiting_for_resources: gpu (need 4, free 3)". It is the single most
   * useful field on this card when nothing is happening.
   */
  it('leads with the pending reason while the attempt is still PENDING', () => {
    render(
      <AttemptList
        attempts={[
          attempt({ state: 'PENDING', pending_reason: 'waiting_for_resources: gpu (need 4, free 3)' }),
        ]}
      />,
    );

    expect(screen.getByText(/Why this has not dispatched/i)).toBeInTheDocument();
    expect(screen.getByText('waiting_for_resources: gpu (need 4, free 3)')).toBeInTheDocument();
  });

  /**
   * The same string on a finished attempt is history, not a live diagnosis.
   * Presenting it as current would send an operator chasing a resolved stall.
   */
  it('demotes the same reason to history once the attempt has moved on', () => {
    render(
      <AttemptList
        attempts={[
          attempt({ state: 'RUNNING', pending_reason: 'waiting_for_resources: gpu (need 4, free 3)' }),
        ]}
      />,
    );

    expect(screen.getByText(/historical/i)).toBeInTheDocument();
    expect(screen.queryByText(/Why this has not dispatched/i)).not.toBeInTheDocument();
  });
});

describe('error reporting', () => {
  it("shows the error class, type and the worker's retryability verdict", () => {
    render(
      <AttemptList
        attempts={[
          attempt({
            state: 'FAILED',
            error: {
              class: 'TRANSIENT',
              type: 'ConnectionReset',
              message: 'peer reset',
              retryable: true,
            },
          }),
        ]}
      />,
    );

    expect(screen.getByText('TRANSIENT')).toBeInTheDocument();
    expect(screen.getByText('ConnectionReset')).toBeInTheDocument();
    // Whether the WORKER thought this was retryable explains why it was or was
    // not retried, which is otherwise invisible.
    expect(screen.getByText(/worker says retryable/i)).toBeInTheDocument();
  });

  it('renders an error with no message rather than an empty box', () => {
    render(<AttemptList attempts={[attempt({ state: 'FAILED', error: { class: 'APPLICATION' } })]} />);
    expect(screen.getByText('(no message reported)')).toBeInTheDocument();
  });
});

describe('selection', () => {
  it('offers a logs control per attempt and reports which one was chosen', () => {
    const onSelect = vi.fn();
    render(
      <AttemptList
        attempts={[attempt({ id: 'a1', attempt_number: 1 }), attempt({ id: 'a2', attempt_number: 2 })]}
        onSelectAttempt={onSelect}
      />,
    );

    const first = screen.getByText('attempt #1').closest('section')!;
    fireEvent.click(within(first).getByRole('button', { name: /logs/i }));
    // The OLDER attempt's logs must be reachable: a fixed bug's logs are how
    // you confirm it was the same failure.
    expect(onSelect).toHaveBeenCalledWith('a1');
  });
});

describe('usage units (ADR-0032 §2)', () => {
  it('renders tokens by model and other units verbatim', () => {
    render(
      <AttemptList
        attempts={[
          attempt({
            usage_units: {
              'tokens:openai/gpt-4o-mini:input': 1200,
              'tokens:openai/gpt-4o-mini:output': 340,
              'requests:openai/gpt-4o-mini': 3,
              'gpu_seconds:a100': 42,
            },
            usage_reported_at: '2026-09-12T10:01:00Z',
          }),
        ]}
      />,
    );
    const usage = screen.getByTestId('attempt-usage');
    const models = within(usage).getByRole('table', { name: /tokens by model/i });
    expect(within(models).getAllByRole('columnheader').map((h) => h.textContent)).toEqual([
      'Model',
      'Input',
      'Output',
      'Cached',
      'Requests',
    ]);
    const row = within(models).getByRole('row', { name: /openai\/gpt-4o-mini/ });
    // Cached was not reported, so it is a dash, not a zero.
    expect(within(row).getAllByRole('cell').map((c) => c.textContent)).toEqual(['1,200', '340', '—', '3']);
    const other = within(usage).getByRole('table', { name: /other units/i });
    expect(within(other).getByText('gpu_seconds:a100')).toBeInTheDocument();
    expect(within(usage).getByText(/last reported/)).toBeInTheDocument();
    // The scroll regions are reachable by keyboard.
    for (const region of within(usage).getAllByRole('region')) expect(region).toHaveAttribute('tabindex', '0');
  });

  it('says no usage was reported for an empty map, never a table of zeros', () => {
    render(<AttemptList attempts={[attempt({ usage_units: {}, usage_reported_at: null })]} />);
    const usage = screen.getByTestId('attempt-usage');
    expect(usage).toHaveTextContent(/No usage reported/);
    expect(within(usage).queryByRole('table')).not.toBeInTheDocument();
    expect(within(usage).queryByText(/last reported/)).not.toBeInTheDocument();
  });

  it('leaves the section out when the gateway does not serve usage', () => {
    render(<AttemptList attempts={[attempt()]} />);
    expect(screen.queryByTestId('attempt-usage')).not.toBeInTheDocument();
  });
});
