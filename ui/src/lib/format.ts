/**
 * Formatting helpers for an operator view.
 *
 * The bias throughout is toward honesty over prettiness: an absent value
 * renders as an em dash, never as "0" or "just now", because in an incident a
 * fabricated zero is worse than a visible gap.
 */

import type { ResourceVector } from '../api/types';

export const EMPTY = '—';

export function fmtBytes(n: number | null | undefined): string {
  if (n === null || n === undefined || Number.isNaN(n)) return EMPTY;
  if (n === 0) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  const i = Math.min(units.length - 1, Math.floor(Math.log2(Math.abs(n)) / 10));
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

/** Millicores are the wire unit; cores are what humans reason in. */
export function fmtCpu(millis: number | null | undefined): string {
  if (millis === null || millis === undefined) return EMPTY;
  return millis % 1000 === 0 ? `${millis / 1000} cpu` : `${(millis / 1000).toFixed(2)} cpu`;
}

export function fmtDurationMs(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || Number.isNaN(ms)) return EMPTY;
  if (ms < 0) return EMPTY;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toFixed(s < 10 ? 1 : 0)}s`;
  const m = Math.floor(s / 60);
  const rem = Math.round(s % 60);
  if (m < 60) return `${m}m ${rem}s`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function parseTs(ts: string | null | undefined): number | null {
  if (!ts) return null;
  const t = Date.parse(ts);
  return Number.isNaN(t) ? null : t;
}

/** Duration between two timestamps; null if either endpoint is missing. */
export function durationBetween(
  from: string | null | undefined,
  to: string | null | undefined,
): number | null {
  const a = parseTs(from);
  const b = parseTs(to);
  if (a === null || b === null) return null;
  return b - a;
}

/**
 * Age relative to `now`, which is passed in rather than read from the clock so
 * that a table of ages re-renders consistently and is testable.
 */
export function fmtAge(ts: string | null | undefined, now: number = Date.now()): string {
  const t = parseTs(ts);
  if (t === null) return EMPTY;
  const d = now - t;
  if (d < 0) return 'in the future';
  return `${fmtDurationMs(d)} ago`;
}

export function fmtTs(ts: string | null | undefined): string {
  const t = parseTs(ts);
  if (t === null) return EMPTY;
  const d = new Date(t);
  // ISO-like, no timezone conversion surprises, second precision. Operators
  // correlate this against logs, so a locale-formatted string is a liability.
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(
    d.getMinutes(),
  )}:${pad(d.getSeconds())}`;
}

/** Compact single-line resource vector, e.g. "4 cpu · 16 GiB · 1×gpu". */
export function fmtResources(v: ResourceVector | null | undefined): string {
  if (!v) return EMPTY;
  const parts: string[] = [];
  if (v.cpu_millis) parts.push(fmtCpu(v.cpu_millis));
  if (v.memory_bytes) parts.push(fmtBytes(v.memory_bytes));
  if (v.gpu_count) parts.push(`${v.gpu_count}×${v.gpu_kind || 'gpu'}`);
  if (v.disk_bytes) parts.push(`${fmtBytes(v.disk_bytes)} disk`);
  for (const [k, n] of Object.entries(v.custom ?? {})) parts.push(`${k}=${n}`);
  return parts.length > 0 ? parts.join(' · ') : 'none';
}

/** Fraction committed/advertised, clamped to [0,1]; null when unmeasurable. */
export function utilization(
  committed: number | null | undefined,
  advertised: number | null | undefined,
): number | null {
  if (!advertised || advertised <= 0) return null;
  const c = committed ?? 0;
  return Math.max(0, Math.min(1, c / advertised));
}

export function fmtPercent(f: number | null | undefined): string {
  if (f === null || f === undefined || Number.isNaN(f)) return EMPTY;
  return `${Math.round(f * 100)}%`;
}

export function shortId(id: string | null | undefined, n = 8): string {
  if (!id) return EMPTY;
  return id.length <= n ? id : id.slice(0, n);
}
