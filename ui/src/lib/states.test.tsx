/**
 * Tests for the state→visual mapping.
 *
 * The load-bearing assertion is the distinctness one: FAILED, UPSTREAM_FAILED
 * and SKIPPED must differ on tone AND glyph AND border style. That is the whole
 * operational justification for having three separate states, so it is worth a
 * test that fails loudly if someone "tidies up" the palette.
 */

import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import {
  ATTEMPT_STATES,
  NODE_STATES,
  RUN_STATES,
  WORKER_STATES,
} from '../api/types';
import { STATE_TABLES, UNKNOWN_VISUAL, isTerminal, normalizeState, stateVisual } from './states';
import { StateBadge } from '../components/StateBadge';

describe('normalizeState', () => {
  it('passes canonical Postgres enum labels through unchanged', () => {
    expect(normalizeState('UPSTREAM_FAILED')).toBe('UPSTREAM_FAILED');
    expect(normalizeState('TIMED_OUT')).toBe('TIMED_OUT');
  });

  it('strips protobuf enum prefixes so either wire spelling renders', () => {
    expect(normalizeState('NODE_STATE_UPSTREAM_FAILED')).toBe('UPSTREAM_FAILED');
    expect(normalizeState('ATTEMPT_STATE_LOST')).toBe('LOST');
    expect(normalizeState('RUN_STATE_CANCELLING')).toBe('CANCELLING');
    expect(normalizeState('WORKER_STATE_DRAINING')).toBe('DRAINING');
    expect(normalizeState('ERROR_CLASS_OOM')).toBe('OOM');
  });

  it('uppercases and normalises separators', () => {
    expect(normalizeState('upstream-failed')).toBe('UPSTREAM_FAILED');
  });

  it('returns empty string for absent values rather than throwing', () => {
    expect(normalizeState(null)).toBe('');
    expect(normalizeState(undefined)).toBe('');
  });
});

describe('stateVisual coverage', () => {
  it.each([
    ['run', RUN_STATES],
    ['node', NODE_STATES],
    ['attempt', ATTEMPT_STATES],
    ['worker', WORKER_STATES],
  ] as const)('maps every %s state from the frozen contract', (kind, states) => {
    for (const s of states) {
      const v = stateVisual(kind, s);
      expect(v, `${kind}/${s} must have a visual`).not.toBe(UNKNOWN_VISUAL);
      expect(v.label.length).toBeGreaterThan(0);
      expect(v.glyph.length).toBeGreaterThan(0);
      expect(v.hint.length).toBeGreaterThan(0);
    }
  });

  it('degrades unknown states to UNKNOWN instead of crashing', () => {
    expect(stateVisual('node', 'SOMETHING_NEW')).toBe(UNKNOWN_VISUAL);
    expect(stateVisual('run', undefined)).toBe(UNKNOWN_VISUAL);
    expect(stateVisual('attempt', 'ATTEMPT_STATE_UNSPECIFIED')).toBe(UNKNOWN_VISUAL);
  });

  it('has no extra states beyond the contract (catches a stale table)', () => {
    expect(Object.keys(STATE_TABLES.node).sort()).toEqual([...NODE_STATES].sort());
    expect(Object.keys(STATE_TABLES.run).sort()).toEqual([...RUN_STATES].sort());
    expect(Object.keys(STATE_TABLES.attempt).sort()).toEqual([...ATTEMPT_STATES].sort());
    expect(Object.keys(STATE_TABLES.worker).sort()).toEqual([...WORKER_STATES].sort());
  });
});

describe('the three states that must never look alike', () => {
  const failed = stateVisual('node', 'FAILED');
  const upstream = stateVisual('node', 'UPSTREAM_FAILED');
  const skipped = stateVisual('node', 'SKIPPED');

  it('uses three different tones', () => {
    expect(new Set([failed.tone, upstream.tone, skipped.tone]).size).toBe(3);
  });

  it('uses three different glyphs', () => {
    expect(new Set([failed.glyph, upstream.glyph, skipped.glyph]).size).toBe(3);
  });

  it('uses three different border styles, so the difference survives greyscale', () => {
    expect(new Set([failed.borderStyle, upstream.borderStyle, skipped.borderStyle]).size).toBe(3);
  });

  it('reserves the danger tone for the node that actually broke', () => {
    expect(failed.tone).toBe('danger');
    expect(upstream.tone).not.toBe('danger');
    expect(skipped.tone).not.toBe('danger');
  });

  it('labels UPSTREAM_FAILED so it cannot be misread as a local failure', () => {
    expect(upstream.label).toMatch(/UPSTREAM/);
    expect(upstream.hint.toLowerCase()).toContain('ancestor');
  });
});

describe('LOST is not FAILED (invariant 2)', () => {
  const lost = stateVisual('attempt', 'LOST');
  const failed = stateVisual('attempt', 'FAILED');

  it('renders with a different tone and glyph', () => {
    expect(lost.tone).not.toBe(failed.tone);
    expect(lost.glyph).not.toBe(failed.glyph);
  });

  it('explains the ambiguity rather than implying a known outcome', () => {
    expect(lost.hint.toLowerCase()).toMatch(/do not know|unknown/);
  });
});

describe('terminality matches the state machines', () => {
  it('marks §5.1 terminal attempt states terminal', () => {
    for (const s of ['SUCCEEDED', 'FAILED', 'TIMED_OUT', 'LOST', 'CANCELLED']) {
      expect(isTerminal('attempt', s), s).toBe(true);
    }
  });

  it('marks in-flight attempt states non-terminal', () => {
    for (const s of ['PENDING', 'DISPATCHED', 'RUNNING']) {
      expect(isTerminal('attempt', s), s).toBe(false);
    }
  });

  it('treats PAUSED and CANCELLING as non-terminal run states (§5.2)', () => {
    expect(isTerminal('run', 'PAUSED')).toBe(false);
    expect(isTerminal('run', 'PAUSING')).toBe(false);
    expect(isTerminal('run', 'CANCELLING')).toBe(false);
    expect(isTerminal('run', 'CANCELLED')).toBe(true);
    expect(isTerminal('run', 'TIMED_OUT')).toBe(true);
  });
});

describe('StateBadge rendering', () => {
  it('renders the label, glyph and tone class', () => {
    render(<StateBadge kind="node" state="UPSTREAM_FAILED" />);
    const badge = screen.getByText(/UPSTREAM FAILED/);
    expect(badge).toBeInTheDocument();
    expect(badge.className).toContain('tone-blocked');
    expect(badge.getAttribute('title')).toMatch(/ancestor/i);
  });

  it('renders protobuf-spelled states identically to enum-label ones', () => {
    const { container: a } = render(<StateBadge kind="node" state="NODE_STATE_SKIPPED" />);
    const { container: b } = render(<StateBadge kind="node" state="SKIPPED" />);
    expect(a.innerHTML).toBe(b.innerHTML);
  });

  it('renders an unknown state as UNKNOWN rather than blank', () => {
    render(<StateBadge kind="run" state="WAT" />);
    expect(screen.getByText('UNKNOWN')).toBeInTheDocument();
  });
});
