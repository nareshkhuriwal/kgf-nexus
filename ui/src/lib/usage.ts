// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Usage units an attempt reported (ADR-0032 §2), parsed for display.
 *
 * The wire carries a flat map of `<kind>:<subject>[:<qualifier>]` keys to
 * non-negative integers:
 *
 *   tokens:<provider>/<model>:input|output|cached   token counts
 *   requests:<provider>/<model>                     provider calls made
 *   anything else                                   a generic unit
 *
 * Tokens and requests are folded into one row per model, because "which model
 * cost what" is the question an operator asks of them. Every other key — and
 * every key this code does not understand, including malformed ones — is kept
 * VERBATIM as an "other units" row. Dropping a key the console cannot parse
 * would under-report spend, which is worse than showing an unfamiliar name.
 *
 * Absence stays distinct from zero throughout: a model that reported input
 * tokens but no cached tokens has `cached: null`, rendered as an em dash, not
 * `0`. The provider may simply not report cached tokens.
 */

import type { Attempt } from '../api/types';
import { EMPTY, parseTs } from './format';
import { normalizeState } from './states';

/** Attempt end states in which the work did not succeed but may have spent. */
const UNSUCCESSFUL: ReadonlySet<string> = new Set(['FAILED', 'TIMED_OUT', 'LOST', 'CANCELLED']);

export type UsageUnits = Record<string, number>;

export type TokenQualifier = 'input' | 'output' | 'cached';
const TOKEN_QUALIFIERS: ReadonlySet<string> = new Set(['input', 'output', 'cached']);

/** One key, classified. `other` covers generic units and anything unparsable. */
export type ParsedUsageKey =
  | { kind: 'tokens'; model: string; qualifier: TokenQualifier }
  | { kind: 'requests'; model: string }
  | { kind: 'other'; key: string };

export function parseUsageKey(key: string): ParsedUsageKey {
  const parts = key.split(':');
  const [kind, subject, qualifier] = parts;
  if (kind === 'tokens' && parts.length === 3 && subject && TOKEN_QUALIFIERS.has(qualifier ?? '')) {
    return { kind: 'tokens', model: subject, qualifier: qualifier as TokenQualifier };
  }
  if (kind === 'requests' && parts.length === 2 && subject) {
    return { kind: 'requests', model: subject };
  }
  return { kind: 'other', key };
}

/** Token and request counts for one `<provider>/<model>`; null = not reported. */
export interface ModelUsage {
  model: string;
  input: number | null;
  output: number | null;
  cached: number | null;
  requests: number | null;
}

/** A generic or unrecognized key, shown verbatim. `value` null = not a valid count. */
export interface OtherUsage {
  key: string;
  value: number | null;
}

export interface UsageSummary {
  models: ModelUsage[];
  other: OtherUsage[];
  /** Column totals across models; null where no model reported that column. */
  totals: Omit<ModelUsage, 'model'>;
}

/** A count the ADR permits: a finite, non-negative number. */
function validCount(v: unknown): v is number {
  return typeof v === 'number' && Number.isFinite(v) && v >= 0;
}

function add(a: number | null, b: number): number {
  return (a ?? 0) + b;
}

/**
 * The unit map as a plain object, or null when there is none. Anything that is
 * not an object (an older gateway omits the field; a broken one might send a
 * string) is treated as "no usage map", never as an error.
 */
function asUnitMap(units: unknown): Record<string, unknown> | null {
  if (!units || typeof units !== 'object' || Array.isArray(units)) return null;
  return units as Record<string, unknown>;
}

/** Groups one unit map into per-model rows plus verbatim other rows. */
export function summarizeUsage(units: unknown): UsageSummary {
  const byModel = new Map<string, ModelUsage>();
  const other: OtherUsage[] = [];
  const map = asUnitMap(units);
  for (const [key, raw] of Object.entries(map ?? {})) {
    const parsed = parseUsageKey(key);
    // A value that is not a valid count cannot be summed, but the key is still
    // shown: the task claimed to report it, and hiding that would be silent.
    if (parsed.kind === 'other' || !validCount(raw)) {
      other.push({ key, value: validCount(raw) ? raw : null });
      continue;
    }
    let row = byModel.get(parsed.model);
    if (!row) {
      row = { model: parsed.model, input: null, output: null, cached: null, requests: null };
      byModel.set(parsed.model, row);
    }
    if (parsed.kind === 'requests') row.requests = add(row.requests, raw);
    else row[parsed.qualifier] = add(row[parsed.qualifier], raw);
  }

  const models = [...byModel.values()].sort(
    (a, b) => modelTokens(b) - modelTokens(a) || a.model.localeCompare(b.model),
  );
  other.sort((a, b) => a.key.localeCompare(b.key));

  const totals: Omit<ModelUsage, 'model'> = { input: null, output: null, cached: null, requests: null };
  for (const m of models) {
    for (const col of ['input', 'output', 'cached', 'requests'] as const) {
      const v = m[col];
      if (v !== null) totals[col] = add(totals[col], v);
    }
  }
  return { models, other, totals };
}

/** Input + output tokens for ordering; cached tokens are a subset of input. */
function modelTokens(m: ModelUsage): number {
  return (m.input ?? 0) + (m.output ?? 0);
}

export function hasUsage(s: UsageSummary): boolean {
  return s.models.length > 0 || s.other.length > 0;
}

/**
 * Sums unit maps across attempts, key by key. Each attempt's map is its own
 * cumulative total (the scheduler merges snapshots with `max`), so summing
 * across attempts is correct and summing snapshots would not be.
 *
 * Every attempt counts, whatever its state: tokens spent by an attempt that
 * later failed, was lost or was cancelled were still spent. A key whose value
 * is not a valid count in some attempt is kept (so it still shows), and only
 * the valid values are added.
 */
export function sumUsageUnits(attempts: ReadonlyArray<Pick<Attempt, 'usage_units'>>): Record<string, number | null> {
  const out: Record<string, number | null> = {};
  for (const a of attempts) {
    const map = asUnitMap(a.usage_units);
    if (!map) continue;
    for (const [key, raw] of Object.entries(map)) {
      if (validCount(raw)) out[key] = (out[key] ?? 0) + raw;
      else if (!(key in out)) out[key] = null;
    }
  }
  return out;
}

export interface RunUsage {
  summary: UsageSummary;
  /** Attempts whose map carries at least one key. */
  reporting: number;
  /** Attempts the gateway served a usage map for at all (possibly `{}`). */
  served: number;
  /**
   * Reporting attempts that ended without succeeding (failed, timed out, lost,
   * cancelled). Their units are in the totals: the tokens were still spent.
   */
  unsuccessful: number;
  /** Latest `usage_reported_at` across attempts; null when none. */
  lastReportedAt: string | null;
}

/** The run-level roll-up: every attempt's units summed and grouped. */
export function summarizeRunUsage(attempts: ReadonlyArray<Attempt>): RunUsage {
  let reporting = 0;
  let served = 0;
  let unsuccessful = 0;
  let last: { ts: string; t: number } | null = null;
  for (const a of attempts) {
    const map = asUnitMap(a.usage_units);
    if (map) {
      served++;
      if (Object.keys(map).length > 0) {
        reporting++;
        if (UNSUCCESSFUL.has(normalizeState(a.state))) unsuccessful++;
      }
    }
    const t = parseTs(a.usage_reported_at);
    if (t !== null && (last === null || t > last.t)) last = { ts: a.usage_reported_at as string, t };
  }
  return { summary: summarizeUsage(sumUsageUnits(attempts)), reporting, served, unsuccessful, lastReportedAt: last?.ts ?? null };
}

/**
 * A count, readable at a glance: exact below ten thousand ("1,234"), compact
 * above it ("12.3k", "4.56M", "1.2B"). The exact figure is `fmtCountExact`,
 * shown as a tooltip beside this. Null is the em dash, never "0".
 */
export function fmtCount(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return EMPTY;
  const abs = Math.abs(n);
  if (abs < 10_000) return fmtCountExact(n);
  const scales: Array<[number, string]> = [
    [1e15, 'P'],
    [1e12, 'T'],
    [1e9, 'B'],
    [1e6, 'M'],
    [1e3, 'k'],
  ];
  for (const [scale, suffix] of scales) {
    if (abs >= scale) {
      const v = n / scale;
      const digits = Math.abs(v) >= 100 ? 0 : Math.abs(v) >= 10 ? 1 : 2;
      // Trim trailing zeros so 2.00M reads as 2M.
      return `${Number(v.toFixed(digits))}${suffix}`;
    }
  }
  return fmtCountExact(n);
}

/** The exact count with thousands separators, locale-independent. */
export function fmtCountExact(n: number | null | undefined): string {
  if (n === null || n === undefined || !Number.isFinite(n)) return EMPTY;
  const [int = '', frac] = String(n).split('.');
  const sign = int.startsWith('-') ? '-' : '';
  const digits = sign ? int.slice(1) : int;
  const grouped = digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  return `${sign}${grouped}${frac ? `.${frac}` : ''}`;
}
