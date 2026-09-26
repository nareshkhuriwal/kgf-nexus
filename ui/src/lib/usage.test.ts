// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Usage-unit parsing and roll-up (ADR-0032 §2).
 *
 * Pinned: tokens and requests fold into one row per model; every key the
 * console does not understand is kept verbatim rather than dropped (dropping
 * it would under-report spend); absence stays distinct from zero; and the run
 * roll-up sums every attempt, whatever state it ended in.
 */

import { describe, expect, it } from 'vitest';
import type { Attempt } from '../api/types';
import {
  fmtCount,
  fmtCountExact,
  hasUsage,
  parseUsageKey,
  sumUsageUnits,
  summarizeRunUsage,
  summarizeUsage,
} from './usage';

const attempt = (over: Partial<Attempt> = {}): Attempt => ({
  id: 'a',
  run_id: 'r',
  node_id: 'n',
  attempt_number: 1,
  state: 'SUCCEEDED',
  ...over,
});

describe('parseUsageKey', () => {
  it('classifies token, request and generic keys', () => {
    expect(parseUsageKey('tokens:openai/gpt-4o-mini:input')).toEqual({
      kind: 'tokens',
      model: 'openai/gpt-4o-mini',
      qualifier: 'input',
    });
    expect(parseUsageKey('tokens:anthropic/claude:cached')).toMatchObject({ kind: 'tokens', qualifier: 'cached' });
    expect(parseUsageKey('requests:openai/gpt-4o-mini')).toEqual({ kind: 'requests', model: 'openai/gpt-4o-mini' });
    expect(parseUsageKey('images:openai/dall-e-3')).toEqual({ kind: 'other', key: 'images:openai/dall-e-3' });
  });

  it('treats malformed keys as other, verbatim', () => {
    for (const key of ['tokens', 'tokens:', 'tokens:m', 'tokens:m:bogus', 'tokens::input', 'requests:', 'requests:m:x', '', ':::']) {
      expect(parseUsageKey(key)).toEqual({ kind: 'other', key });
    }
  });
});

describe('summarizeUsage', () => {
  it('groups tokens and requests by model and keeps other units verbatim', () => {
    const s = summarizeUsage({
      'tokens:openai/gpt-4o-mini:input': 1200,
      'tokens:openai/gpt-4o-mini:output': 340,
      'requests:openai/gpt-4o-mini': 3,
      'tokens:anthropic/claude-x:input': 50_000,
      'tokens:anthropic/claude-x:cached': 20_000,
      'gpu_seconds:a100': 42,
      'tokens:broken': 7,
    });
    // Ordered by tokens spent, largest first.
    expect(s.models.map((m) => m.model)).toEqual(['anthropic/claude-x', 'openai/gpt-4o-mini']);
    expect(s.models[1]).toEqual({ model: 'openai/gpt-4o-mini', input: 1200, output: 340, cached: null, requests: 3 });
    // Not reported stays null, never zero.
    expect(s.models[0]).toMatchObject({ output: null, requests: null, cached: 20_000 });
    expect(s.other).toEqual([
      { key: 'gpu_seconds:a100', value: 42 },
      { key: 'tokens:broken', value: 7 },
    ]);
    expect(s.totals).toEqual({ input: 51_200, output: 340, cached: 20_000, requests: 3 });
  });

  it('shows a key whose value is not a valid count as other with no value', () => {
    const s = summarizeUsage({ 'tokens:m/x:input': -5, 'requests:m/x': 'many' as unknown as number });
    expect(s.models).toEqual([]);
    expect(s.other).toEqual([
      { key: 'requests:m/x', value: null },
      { key: 'tokens:m/x:input', value: null },
    ]);
  });

  it('tolerates a missing or non-object map', () => {
    for (const units of [undefined, null, 'x', 3, [], {}]) {
      const s = summarizeUsage(units);
      expect(hasUsage(s)).toBe(false);
      expect(s.totals).toEqual({ input: null, output: null, cached: null, requests: null });
    }
  });
});

describe('run roll-up', () => {
  it('sums every attempt, including failed and lost ones', () => {
    const attempts = [
      attempt({ id: 'a1', state: 'FAILED', usage_units: { 'tokens:openai/gpt-4o-mini:input': 1000, 'requests:openai/gpt-4o-mini': 1 }, usage_reported_at: '2026-09-12T10:01:00Z' }),
      attempt({ id: 'a2', state: 'LOST', usage_units: { 'tokens:openai/gpt-4o-mini:input': 500 }, usage_reported_at: '2026-09-12T10:03:00Z' }),
      attempt({ id: 'a3', state: 'SUCCEEDED', usage_units: { 'tokens:openai/gpt-4o-mini:input': 250, 'tokens:openai/gpt-4o-mini:output': 90, 'requests:openai/gpt-4o-mini': 2 }, usage_reported_at: '2026-09-12T10:02:00Z' }),
      attempt({ id: 'a4', state: 'SUCCEEDED', usage_units: {} }),
      attempt({ id: 'a5', state: 'PENDING' }),
    ];
    expect(sumUsageUnits(attempts)).toEqual({
      'tokens:openai/gpt-4o-mini:input': 1750,
      'tokens:openai/gpt-4o-mini:output': 90,
      'requests:openai/gpt-4o-mini': 3,
    });
    const run = summarizeRunUsage(attempts);
    expect(run.summary.models).toEqual([
      { model: 'openai/gpt-4o-mini', input: 1750, output: 90, cached: null, requests: 3 },
    ]);
    expect(run.reporting).toBe(3);
    expect(run.served).toBe(4);
    expect(run.unsuccessful).toBe(2);
    expect(run.lastReportedAt).toBe('2026-09-12T10:03:00Z');
  });

  it('reports nothing served when the gateway omits the field', () => {
    const run = summarizeRunUsage([attempt()]);
    expect(run.served).toBe(0);
    expect(hasUsage(run.summary)).toBe(false);
    expect(run.lastReportedAt).toBeNull();
  });
});

describe('count formatting', () => {
  it('is exact below ten thousand and compact above', () => {
    expect(fmtCount(0)).toBe('0');
    expect(fmtCount(1234)).toBe('1,234');
    expect(fmtCount(12_345)).toBe('12.3k');
    expect(fmtCount(2_000_000)).toBe('2M');
    expect(fmtCount(4_560_000)).toBe('4.56M');
    expect(fmtCount(1_200_000_000)).toBe('1.2B');
  });

  it('renders absence as an em dash, never zero', () => {
    expect(fmtCount(null)).toBe('—');
    expect(fmtCount(undefined)).toBe('—');
    expect(fmtCountExact(null)).toBe('—');
  });

  it('groups thousands exactly', () => {
    expect(fmtCountExact(1_234_567)).toBe('1,234,567');
    expect(fmtCountExact(999)).toBe('999');
  });
});
