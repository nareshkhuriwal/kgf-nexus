// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Pure layout maths for the run's execution timeline (Gantt).
 *
 * One row per task ATTEMPT, never per node: a retry is a new attempt and
 * history is never mutated (invariant 9), so the timeline is the one view that
 * shows a flaky task's tries side by side on the same clock. Rows are grouped
 * by node, and a map template's children sit under a collapsible header row,
 * because a fan-out of a thousand children drawn flat buries every other node.
 *
 * Each attempt is split into the three phases the timestamps can prove:
 *
 *   queued    scheduled_at  -> dispatched_at   waiting for the scheduler
 *   starting  dispatched_at -> started_at      placed, worker not yet running it
 *   running   started_at    -> ended_at        the task itself
 *
 * A phase whose closing timestamp is absent is still OPEN and ends at `now`
 * for a live attempt; a terminal attempt that never reached a phase (cancelled
 * while queued, a cache hit that never ran) simply does not have it. Nothing
 * here invents a timestamp: a gateway that predates dispatched_at yields a
 * queued phase that runs straight to started_at, flagged `inferred`, rather
 * than a fabricated split.
 *
 * Kept free of React and the DOM so the maths (phases, domain, ticks, zoom,
 * pan, virtual window) is testable directly, as lib/dag is.
 */

import type { Attempt, RunNode } from '../api/types';
import { parseTs } from './format';
import { normalizeState, stateVisual, type Tone } from './states';

export type PhaseKind = 'queued' | 'starting' | 'running';

export interface Phase {
  kind: PhaseKind;
  from: number;
  to: number;
  /** Still in progress: the right edge is `now`, not a recorded timestamp. */
  open: boolean;
  /**
   * The phase boundary was not reported (a gateway without dispatched_at), so
   * queued runs to the next known timestamp rather than being split.
   */
  inferred?: boolean;
}

export interface AttemptSpan {
  phases: Phase[];
  /** Earliest phase start, null when the attempt has no usable timestamp. */
  start: number | null;
  /** Latest phase end (now for an open phase). */
  end: number | null;
  queuedMs: number | null;
  startingMs: number | null;
  runningMs: number | null;
  totalMs: number | null;
  open: boolean;
}

/** Terminal attempt states: their open phases are closed at ended_at, never at now. */
function isTerminalAttempt(state: string): boolean {
  return stateVisual('attempt', state).terminal;
}

/**
 * The phases of one attempt, clamped so a skewed clock can never draw a bar
 * that runs backwards.
 */
export function attemptPhases(a: Attempt, now: number): AttemptSpan {
  const scheduled = parseTs(a.scheduled_at);
  const dispatched = parseTs(a.dispatched_at);
  const started = parseTs(a.started_at);
  const ended = parseTs(a.ended_at);
  const terminal = isTerminalAttempt(normalizeState(a.state));
  // Where an open phase stops: the recorded end for a finished attempt, the
  // present for a live one. A terminal attempt with no ended_at (should not
  // happen) is closed at its last known timestamp rather than stretched to now.
  const close = ended ?? (terminal ? null : now);

  const phases: Phase[] = [];
  const push = (kind: PhaseKind, from: number | null, to: number | null, open: boolean, inferred?: boolean) => {
    if (from === null || to === null) return;
    phases.push({ kind, from, to: Math.max(from, to), open, ...(inferred ? { inferred } : {}) });
  };

  if (scheduled !== null) {
    const next = dispatched ?? started;
    if (next !== null) push('queued', scheduled, next, false, dispatched === null && started !== null);
    else push('queued', scheduled, close, ended === null && !terminal);
  }
  if (dispatched !== null) {
    if (started !== null) push('starting', dispatched, started, false);
    else push('starting', dispatched, close, ended === null && !terminal);
  }
  if (started !== null) {
    push('running', started, close ?? started, ended === null && !terminal);
  }

  const start = phases.length > 0 ? Math.min(...phases.map((p) => p.from)) : null;
  const end = phases.length > 0 ? Math.max(...phases.map((p) => p.to)) : null;
  const len = (k: PhaseKind) => {
    const p = phases.find((x) => x.kind === k);
    return p ? p.to - p.from : null;
  };
  return {
    phases,
    start,
    end,
    queuedMs: len('queued'),
    startingMs: len('starting'),
    runningMs: len('running'),
    totalMs: start !== null && end !== null ? end - start : null,
    open: phases.some((p) => p.open),
  };
}

// ── rows ────────────────────────────────────────────────────────────────────

export interface AttemptRow {
  kind: 'attempt';
  /** Stable row key: the attempt id. */
  key: string;
  nodeId: string;
  attempt: Attempt;
  span: AttemptSpan;
  /** 0 for an ordinary node, 1 for a map child under its template header. */
  depth: 0 | 1;
  /** First row of its node: carries the node label; later rows are retries. */
  firstOfNode: boolean;
  tone: Tone;
  template?: string;
}

/** A node that has no attempt (not yet ready, upstream failed, skipped). */
export interface IdleNodeRow {
  kind: 'idle';
  key: string;
  nodeId: string;
  node: RunNode;
  depth: 0 | 1;
  template?: string;
}

/** Header of a map template's children; toggles them. */
export interface GroupRow {
  kind: 'group';
  key: string;
  templateId: string;
  childCount: number;
  attemptCount: number;
  collapsed: boolean;
  /** Envelope of every child attempt, drawn as one bar while collapsed. */
  start: number | null;
  end: number | null;
  open: boolean;
  /** Children per final node state, for the header summary. */
  counts: Record<string, number>;
  tone: Tone;
}

export type TimelineRow = AttemptRow | IdleNodeRow | GroupRow;

/** Worst-first tone ranking for a group's envelope bar. */
const TONE_RANK: Tone[] = ['danger', 'warning', 'blocked', 'stopped', 'active', 'queued', 'success', 'muted', 'neutral'];

function worstTone(tones: Tone[]): Tone {
  let best = TONE_RANK.length;
  for (const t of tones) best = Math.min(best, TONE_RANK.indexOf(t) === -1 ? best : TONE_RANK.indexOf(t));
  return TONE_RANK[best] ?? 'neutral';
}

/**
 * Orders nodes the way the run happened: by their first attempt's scheduled
 * time, then by node order for ties and for nodes that never ran. Children of
 * a template keep map_index order, because "item 17" should sit between 16
 * and 18 however the scheduler interleaved them.
 */
export function buildTimelineRows(
  nodes: RunNode[],
  attempts: Attempt[],
  collapsed: ReadonlySet<string>,
  now: number,
): TimelineRow[] {
  const byNode = new Map<string, Attempt[]>();
  for (const a of attempts) {
    const list = byNode.get(a.node_id);
    if (list) list.push(a);
    else byNode.set(a.node_id, [a]);
  }
  for (const list of byNode.values()) list.sort((x, y) => x.attempt_number - y.attempt_number);

  const children = new Map<string, RunNode[]>();
  for (const n of nodes) {
    if (n.parent_node_id) {
      const list = children.get(n.parent_node_id);
      if (list) list.push(n);
      else children.set(n.parent_node_id, [n]);
    }
  }
  for (const list of children.values()) {
    list.sort((x, y) => (x.map_index ?? 0) - (y.map_index ?? 0) || x.node_id.localeCompare(y.node_id));
  }

  const firstScheduled = (nodeId: string): number => {
    const own = byNode.get(nodeId)?.[0];
    let t = own ? parseTs(own.scheduled_at) : null;
    for (const c of children.get(nodeId) ?? []) {
      const ct = parseTs(byNode.get(c.node_id)?.[0]?.scheduled_at);
      if (ct !== null && (t === null || ct < t)) t = ct;
    }
    return t ?? Number.POSITIVE_INFINITY;
  };

  const top = nodes
    .map((n, i) => ({ n, i }))
    .filter(({ n }) => !n.parent_node_id)
    .sort((a, b) => firstScheduled(a.n.node_id) - firstScheduled(b.n.node_id) || a.i - b.i)
    .map(({ n }) => n);

  const rows: TimelineRow[] = [];
  const nodeRows = (n: RunNode, depth: 0 | 1, template?: string): TimelineRow[] => {
    const list = byNode.get(n.node_id) ?? [];
    if (list.length === 0) {
      return [{ kind: 'idle', key: `idle:${n.node_id}`, nodeId: n.node_id, node: n, depth, template }];
    }
    return list.map((a, i) => ({
      kind: 'attempt' as const,
      key: a.id,
      nodeId: n.node_id,
      attempt: a,
      span: attemptPhases(a, now),
      depth,
      firstOfNode: i === 0,
      tone: stateVisual('attempt', a.state).tone,
      template,
    }));
  };

  for (const n of top) {
    const kids = children.get(n.node_id);
    if (!kids || kids.length === 0) {
      rows.push(...nodeRows(n, 0));
      continue;
    }
    const inner: TimelineRow[] = [];
    // A template that also carries attempts of its own keeps them, above its
    // children, so nothing the server reported is dropped.
    if ((byNode.get(n.node_id) ?? []).length > 0) inner.push(...nodeRows(n, 1, n.node_id));
    for (const k of kids) inner.push(...nodeRows(k, 1, n.node_id));

    const spans = inner.filter((r): r is AttemptRow => r.kind === 'attempt');
    const starts = spans.map((r) => r.span.start).filter((t): t is number => t !== null);
    const ends = spans.map((r) => r.span.end).filter((t): t is number => t !== null);
    const counts: Record<string, number> = {};
    for (const k of kids) {
      const s = normalizeState(k.state);
      counts[s] = (counts[s] ?? 0) + 1;
    }
    const isCollapsed = collapsed.has(n.node_id);
    rows.push({
      kind: 'group',
      key: `group:${n.node_id}`,
      templateId: n.node_id,
      childCount: kids.length,
      attemptCount: spans.length,
      collapsed: isCollapsed,
      start: starts.length ? Math.min(...starts) : null,
      end: ends.length ? Math.max(...ends) : null,
      open: spans.some((r) => r.span.open),
      counts,
      // The children's CURRENT outcome, not their history: a fan-out whose
      // failed child was retried to success is a success, and painting it
      // red forever would contradict the node states beside it.
      tone: worstTone(kids.map((k) => stateVisual('node', k.state).tone)),
    });
    if (!isCollapsed) rows.push(...inner);
  }
  return rows;
}

// ── time domain, ticks, zoom and pan ────────────────────────────────────────

export interface TimeWindow {
  start: number;
  end: number;
}

/** Narrowest window a zoom may reach: below this a tick label means nothing. */
export const MIN_SPAN_MS = 20;

/**
 * The run's whole extent, padded so a bar never touches the edge. A live run
 * extends to `now`, which is what makes the chart grow as it runs. An empty
 * run gets a one-minute window ending now, so the axis is still meaningful.
 */
export function timeDomain(
  rows: TimelineRow[],
  opts: { now: number; live: boolean; runStart?: number | null; runEnd?: number | null },
): TimeWindow {
  let lo = opts.runStart ?? Number.POSITIVE_INFINITY;
  let hi = opts.runEnd ?? Number.NEGATIVE_INFINITY;
  for (const r of rows) {
    const s = r.kind === 'attempt' ? r.span.start : r.kind === 'group' ? r.start : null;
    const e = r.kind === 'attempt' ? r.span.end : r.kind === 'group' ? r.end : null;
    if (s !== null) lo = Math.min(lo, s);
    if (e !== null) hi = Math.max(hi, e);
  }
  if (opts.live) hi = Math.max(hi, opts.now);
  if (!Number.isFinite(lo) && !Number.isFinite(hi)) return { start: opts.now - 60_000, end: opts.now };
  if (!Number.isFinite(lo)) lo = hi - 60_000;
  if (!Number.isFinite(hi) || hi <= lo) hi = lo + 1000;
  const pad = Math.max((hi - lo) * 0.02, 50);
  return { start: lo - pad, end: hi + pad };
}

/** Candidate tick steps, smallest first: human-round durations only. */
const STEPS_MS = [
  1, 2, 5, 10, 20, 50, 100, 200, 500,
  1_000, 2_000, 5_000, 10_000, 15_000, 30_000,
  60_000, 120_000, 300_000, 600_000, 900_000, 1_800_000,
  3_600_000, 7_200_000, 10_800_000, 21_600_000, 43_200_000, 86_400_000,
];

export interface Tick {
  t: number;
  label: string;
  /** A day boundary: the label then carries the date. */
  major: boolean;
}

/** Smallest round step giving at most `maxTicks` ticks across the window. */
export function tickStep(window: TimeWindow, maxTicks: number): number {
  const span = window.end - window.start;
  const target = span / Math.max(1, maxTicks);
  for (const s of STEPS_MS) if (s >= target) return s;
  return Math.ceil(target / 86_400_000) * 86_400_000;
}

const pad2 = (n: number) => String(n).padStart(2, '0');

/**
 * Local wall-clock ticks at round multiples of the step. Wall clock, not
 * "+1m30s": operators correlate this chart with log lines and alerts, which
 * carry clock times. Sub-second steps add milliseconds; day boundaries add the
 * date so a run crossing midnight is not ambiguous.
 */
export function timeTicks(window: TimeWindow, maxTicks: number): Tick[] {
  const step = tickStep(window, maxTicks);
  // Align to the local clock, not the epoch, so an hour tick lands on :00 in
  // a timezone with a non-hour offset.
  const offset = new Date(window.start).getTimezoneOffset() * 60_000;
  const first = Math.ceil((window.start - offset) / step) * step + offset;
  const ticks: Tick[] = [];
  for (let t = first; t <= window.end && ticks.length < 1000; t += step) {
    const d = new Date(t);
    const midnight = d.getHours() === 0 && d.getMinutes() === 0 && d.getSeconds() === 0 && d.getMilliseconds() === 0;
    let label = `${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
    if (step < 60_000) label += `:${pad2(d.getSeconds())}`;
    if (step < 1000) label += `.${String(d.getMilliseconds()).padStart(3, '0')}`;
    if (midnight && step >= 3_600_000) label = `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
    ticks.push({ t, label, major: midnight });
  }
  return ticks;
}

/** Clamps a window into the domain, keeping its span where possible. */
export function clampWindow(w: TimeWindow, domain: TimeWindow): TimeWindow {
  const domSpan = domain.end - domain.start;
  let span = Math.max(MIN_SPAN_MS, Math.min(w.end - w.start, domSpan));
  if (!Number.isFinite(span) || span <= 0) span = domSpan;
  let start = Math.max(domain.start, Math.min(w.start, domain.end - span));
  if (!Number.isFinite(start)) start = domain.start;
  return { start, end: start + span };
}

/**
 * Zooms by `factor` (<1 in, >1 out) keeping the instant under `anchor` fixed,
 * which is what makes a wheel-zoom feel like it zooms "where I point".
 */
export function zoomWindow(w: TimeWindow, factor: number, anchor: number, domain: TimeWindow): TimeWindow {
  const span = w.end - w.start;
  const next = Math.max(MIN_SPAN_MS, span * factor);
  const ratio = span > 0 ? (anchor - w.start) / span : 0.5;
  const start = anchor - ratio * next;
  return clampWindow({ start, end: start + next }, domain);
}

/** Pans by `deltaMs` (positive = later), clamped to the domain. */
export function panWindow(w: TimeWindow, deltaMs: number, domain: TimeWindow): TimeWindow {
  return clampWindow({ start: w.start + deltaMs, end: w.end + deltaMs }, domain);
}

/** True when the window shows the whole domain (the "fit" state that follows a live run). */
export function isFullWindow(w: TimeWindow, domain: TimeWindow): boolean {
  return w.start <= domain.start + 1 && w.end >= domain.end - 1;
}

/** Pixel x of instant `t` for a track `width` pixels wide. */
export function xOf(t: number, w: TimeWindow, width: number): number {
  const span = w.end - w.start;
  return span > 0 ? ((t - w.start) / span) * width : 0;
}

/** Instant at pixel x; the inverse of xOf. */
export function tOf(x: number, w: TimeWindow, width: number): number {
  return width > 0 ? w.start + (x / width) * (w.end - w.start) : w.start;
}

/**
 * A bar's pixel extent, clipped to the track, or null when it is entirely out
 * of view. Very short phases get a minimum visible width so a 3 ms attempt on
 * an hour-long axis is still a clickable mark rather than nothing.
 */
export function barExtent(
  from: number,
  to: number,
  w: TimeWindow,
  width: number,
  minPx = 2,
): { left: number; width: number } | null {
  const x0 = xOf(from, w, width);
  const x1 = xOf(to, w, width);
  if (x1 < 0 || x0 > width) return null;
  const left = Math.max(0, x0);
  const right = Math.min(width, Math.max(x1, x0 + minPx));
  return { left, width: Math.max(minPx, right - left) };
}

// ── virtual window ──────────────────────────────────────────────────────────

/**
 * Which rows to mount for a scroll position. Only these are in the DOM, which
 * is what keeps a 5000-attempt fan-out at a few dozen elements; `overscan`
 * rows either side hide the mount during fast scrolling.
 */
export function virtualRange(
  scrollTop: number,
  viewportHeight: number,
  rowHeight: number,
  count: number,
  overscan = 8,
): { first: number; last: number } {
  if (count === 0) return { first: 0, last: -1 };
  const first = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const visible = Math.ceil(Math.max(viewportHeight, rowHeight) / rowHeight);
  const last = Math.min(count - 1, Math.floor(scrollTop / rowHeight) + visible + overscan);
  return { first, last };
}

/**
 * The scrollTop that brings row `index` into view, or null when it already
 * is. Minimal movement: a row just below the fold is scrolled to the bottom
 * edge, not yanked to the top.
 */
export function scrollToRow(
  index: number,
  scrollTop: number,
  viewportHeight: number,
  rowHeight: number,
): number | null {
  const top = index * rowHeight;
  const bottom = top + rowHeight;
  if (top < scrollTop) return top;
  if (bottom > scrollTop + viewportHeight) return Math.max(0, bottom - viewportHeight);
  return null;
}
