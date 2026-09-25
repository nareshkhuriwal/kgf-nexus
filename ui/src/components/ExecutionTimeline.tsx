// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Execution timeline: a Gantt chart of every task attempt in a run.
 *
 * The DAG answers "what broke"; this answers "where did the time go". An
 * attempt's bar is split into queued (waiting for the scheduler), starting
 * (placed, worker not yet running it) and running, because "the task is slow"
 * and "the task waited four minutes for a GPU" are different incidents with
 * different owners, and one undivided bar cannot tell them apart.
 *
 * Phase is carried by bar HEIGHT (queued thin, starting medium, running full),
 * final state by the state table's tone plus the state glyph in the label, so
 * the chart reads in greyscale and to a colour-blind operator, exactly like the
 * DAG (lib/states).
 *
 * Interaction mirrors DagCanvas deliberately, so the two canvases on one page
 * never disagree about what a gesture means:
 *   - plain wheel scrolls the rows, as it scrolls any list;
 *   - Ctrl/Cmd + wheel zooms about the pointer;
 *   - Shift + wheel or a sideways swipe pans time; so does dragging the chart;
 *   - keyboard: arrows up/down move between rows, left/right pan, + and -
 *     zoom, 0 fits the whole run, Enter selects.
 *
 * Only the rows in view are mounted (lib/timeline virtualRange): a fan-out of
 * several thousand attempts costs a few dozen DOM rows. The table view is the
 * accessible equivalent and a sortable-by-eye record: same rows, same order,
 * every duration as text.
 */

import { useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { Attempt, RunDetail } from '../api/types';
import { fmtDurationMs, fmtTs, parseTs } from '../lib/format';
import { normalizeState, stateVisual } from '../lib/states';
import {
  barExtent,
  buildTimelineRows,
  isFullWindow,
  panWindow,
  scrollToRow,
  tOf,
  timeDomain,
  timeTicks,
  virtualRange,
  xOf,
  zoomWindow,
  type AttemptRow,
  type AttemptSpan,
  type GroupRow,
  type Phase,
  type TimeWindow,
  type TimelineRow,
} from '../lib/timeline';
import './timeline.css';

export const ROW_H = 28;
const MAX_VIEWPORT_H = 440;
/** Track width before the first measurement (and in jsdom, which has no layout). */
const FALLBACK_TRACK_W = 720;
/** Label column: wide enough for `process_shard[123]  #2`, narrower on a phone. */
const LABEL_W = 240;
const LABEL_W_NARROW = 136;
const ZOOM_STEP = 1.5;
const PAN_FRACTION = 0.1;

export type TimelineMode = 'chart' | 'table';

export interface ExecutionTimelineProps {
  run: Pick<RunDetail, 'nodes' | 'created_at' | 'started_at' | 'ended_at'>;
  attempts: Attempt[];
  /** The run is not terminal: open phases grow and the now-line is drawn. */
  live: boolean;
  selectedNodeId?: string | null;
  selectedAttemptId?: string | null;
  onSelectAttempt: (nodeId: string, attemptId: string) => void;
  onSelectNode: (nodeId: string) => void;
  mode?: TimelineMode;
  onModeChange?: (mode: TimelineMode) => void;
  /** How often the now-line advances while live. */
  tickMs?: number;
  /** Clock source, injectable for tests. */
  clock?: () => number;
}

function phaseText(ms: number | null, span: AttemptSpan, kind: Phase['kind']): string {
  if (ms === null) return 'did not happen';
  const p = span.phases.find((x) => x.kind === kind);
  const suffix = p?.open ? ' (ongoing)' : p?.inferred ? ' (dispatch time not reported)' : '';
  return `${fmtDurationMs(ms)}${suffix}`;
}

/** The screen-reader sentence for an attempt row: identity, state, durations. */
export function describeAttemptRow(r: AttemptRow): string {
  const v = stateVisual('attempt', r.attempt.state);
  const s = r.span;
  const parts = [
    `Attempt ${r.attempt.attempt_number} of ${r.nodeId}, ${v.label}`,
    `queued ${phaseText(s.queuedMs, s, 'queued')}`,
    `starting ${phaseText(s.startingMs, s, 'starting')}`,
    `running ${phaseText(s.runningMs, s, 'running')}`,
  ];
  if (r.attempt.cache_hit_of) parts.push('served from cache');
  return parts.join('; ');
}

function describeGroupRow(g: GroupRow): string {
  const states = Object.entries(g.counts)
    .map(([s, n]) => `${n} ${stateVisual('node', s).label.toLowerCase()}`)
    .join(', ');
  return `Map ${g.templateId}, ${g.childCount} item${g.childCount === 1 ? '' : 's'}${states ? `: ${states}` : ''}; ${
    g.collapsed ? 'collapsed' : 'expanded'
  }`;
}

function rowLabel(r: TimelineRow): string {
  switch (r.kind) {
    case 'attempt':
      return describeAttemptRow(r);
    case 'group':
      return describeGroupRow(r);
    case 'idle':
      return `No attempt for ${r.nodeId}, ${stateVisual('node', r.node.state).label}`;
  }
}

/** Which row the page's selection points at. */
export function selectedRowKey(
  rows: TimelineRow[],
  selectedNodeId: string | null | undefined,
  selectedAttemptId: string | null | undefined,
): string | null {
  if (selectedAttemptId) {
    const hit = rows.find((r) => r.kind === 'attempt' && r.attempt.id === selectedAttemptId);
    if (hit) return hit.key;
  }
  if (!selectedNodeId) return null;
  let latest: AttemptRow | null = null;
  for (const r of rows) {
    if (r.kind === 'attempt' && r.nodeId === selectedNodeId) {
      if (!latest || r.attempt.attempt_number > latest.attempt.attempt_number) latest = r;
    }
    if (r.kind === 'idle' && r.nodeId === selectedNodeId) return r.key;
  }
  if (latest) return latest.key;
  const group = rows.find((r) => r.kind === 'group' && r.templateId === selectedNodeId);
  return group ? group.key : null;
}

export function ExecutionTimeline({
  run,
  attempts,
  live,
  selectedNodeId,
  selectedAttemptId,
  onSelectAttempt,
  onSelectNode,
  mode: modeProp,
  onModeChange,
  tickMs = 1000,
  clock = Date.now,
}: ExecutionTimelineProps) {
  const [localMode, setLocalMode] = useState<TimelineMode>('chart');
  const mode = modeProp ?? localMode;
  const setMode = (m: TimelineMode) => (onModeChange ? onModeChange(m) : setLocalMode(m));

  // The clock only runs while the run is live: a finished run's chart is a
  // fixed record and must not re-render every second for nothing.
  const [now, setNow] = useState(() => clock());
  useEffect(() => {
    setNow(clock());
    if (!live) return;
    const id = window.setInterval(() => setNow(clock()), tickMs);
    return () => window.clearInterval(id);
  }, [live, tickMs, clock]);

  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const toggleGroup = useCallback((templateId: string) => {
    setCollapsed((cur) => {
      const next = new Set(cur);
      if (next.has(templateId)) next.delete(templateId);
      else next.add(templateId);
      return next;
    });
  }, []);

  const rows = useMemo(
    () => buildTimelineRows(run.nodes ?? [], attempts, collapsed, now),
    [run.nodes, attempts, collapsed, now],
  );
  const domain = useMemo(
    () =>
      timeDomain(rows, {
        now,
        live,
        runStart: parseTs(run.started_at) ?? parseTs(run.created_at),
        runEnd: live ? null : parseTs(run.ended_at),
      }),
    [rows, now, live, run.started_at, run.created_at, run.ended_at],
  );

  // null = follow the whole run (and keep following it as a live run grows);
  // a window = the operator zoomed or panned, and polling must not undo that.
  const [view, setView] = useState<TimeWindow | null>(null);
  const window_ = view ?? domain;

  const selectedKey = selectedRowKey(rows, selectedNodeId, selectedAttemptId);

  // When the selection changes from elsewhere (the DAG, a deep link), expand
  // the template holding it. Keyed on the selection, not on the rows, so a
  // poll never re-expands a group the operator just collapsed.
  const lastSelection = useRef<string | null>(null);
  const selectionId = `${selectedNodeId ?? ''}|${selectedAttemptId ?? ''}`;
  useEffect(() => {
    if (lastSelection.current === selectionId) return;
    lastSelection.current = selectionId;
    const target = attempts.find((a) => a.id === selectedAttemptId)?.node_id ?? selectedNodeId;
    const parent = (run.nodes ?? []).find((n) => n.node_id === target)?.parent_node_id;
    if (parent && collapsed.has(parent)) toggleGroup(parent);
  }, [selectionId, attempts, selectedAttemptId, selectedNodeId, run.nodes, collapsed, toggleGroup]);

  const titleId = useId();
  const noteId = useId();

  const attemptCount = attempts.length;
  const headline =
    attemptCount === 0
      ? 'No attempts yet'
      : `${attemptCount} attempt${attemptCount === 1 ? '' : 's'} across ${new Set(attempts.map((a) => a.node_id)).size} nodes`;

  return (
    <section className="tl" aria-labelledby={titleId}>
      <div className="tl-toolbar">
        <span id={titleId} className="tl-title">
          Execution timeline
        </span>
        <span className="tl-sub">{headline}</span>
        <span className="spacer" />
        <div className="tl-seg" role="group" aria-label="Timeline view">
          <button type="button" aria-pressed={mode === 'chart'} onClick={() => setMode('chart')}>
            Chart
          </button>
          <button type="button" aria-pressed={mode === 'table'} onClick={() => setMode('table')}>
            Table
          </button>
        </div>
        {mode === 'chart' && (
          <div className="tl-seg" role="group" aria-label="Zoom">
            <button
              type="button"
              aria-label="Zoom out"
              title="Zoom out (−)"
              disabled={isFullWindow(window_, domain)}
              onClick={() => setView(nextView(window_, ZOOM_STEP, (window_.start + window_.end) / 2, domain))}
            >
              −
            </button>
            <button
              type="button"
              aria-label="Zoom in"
              title="Zoom in (+)"
              onClick={() => setView(nextView(window_, 1 / ZOOM_STEP, (window_.start + window_.end) / 2, domain))}
            >
              +
            </button>
            <button
              type="button"
              title="Show the whole run (0)"
              disabled={view === null}
              onClick={() => setView(null)}
            >
              {live ? 'Follow live' : 'Fit'}
            </button>
          </div>
        )}
      </div>

      {attemptCount === 0 && rows.length === 0 ? (
        <div className="notice empty tl-empty">
          <span className="notice-glyph" aria-hidden="true">
            ∅
          </span>
          <div>
            <div className="notice-title">Nothing has been scheduled yet</div>
            <div className="notice-detail">
              A bar appears when the DAG engine creates an attempt for a node.
            </div>
          </div>
        </div>
      ) : mode === 'chart' ? (
        <TimelineChart
          rows={rows}
          domain={domain}
          window_={window_}
          onWindow={setView}
          live={live}
          now={now}
          selectedKey={selectedKey}
          onToggleGroup={toggleGroup}
          onSelectAttempt={onSelectAttempt}
          onSelectNode={onSelectNode}
          labelledBy={titleId}
          describedBy={noteId}
        />
      ) : (
        <TimelineTable
          rows={rows}
          selectedKey={selectedKey}
          onToggleGroup={toggleGroup}
          onSelectAttempt={onSelectAttempt}
          onSelectNode={onSelectNode}
        />
      )}

      <div className="tl-foot" id={noteId}>
        <span className="tl-legend" aria-hidden="true">
          <span className="tl-key">
            <i className="tl-swatch tl-phase-queued tone-neutral" /> queued
          </span>
          <span className="tl-key">
            <i className="tl-swatch tl-phase-starting tone-neutral" /> starting
          </span>
          <span className="tl-key">
            <i className="tl-swatch tl-phase-running tone-neutral" /> running
          </span>
        </span>
        <span>
          Colour is the attempt's final state. {mode === 'chart' && 'Ctrl/⌘ + wheel zooms, Shift + wheel or drag pans; arrow keys move and pan, + / − zoom, 0 fits.'}
        </span>
      </div>
    </section>
  );
}

/** Zoom, returning null when the result is the whole domain so the chart resumes following. */
function nextView(w: TimeWindow, factor: number, anchor: number, domain: TimeWindow): TimeWindow | null {
  const z = zoomWindow(w, factor, anchor, domain);
  return isFullWindow(z, domain) ? null : z;
}

interface ChartProps {
  rows: TimelineRow[];
  domain: TimeWindow;
  window_: TimeWindow;
  onWindow: (w: TimeWindow | null) => void;
  live: boolean;
  now: number;
  selectedKey: string | null;
  onToggleGroup: (templateId: string) => void;
  onSelectAttempt: (nodeId: string, attemptId: string) => void;
  onSelectNode: (nodeId: string) => void;
  labelledBy: string;
  describedBy: string;
}

function TimelineChart({
  rows,
  domain,
  window_,
  onWindow,
  live,
  now,
  selectedKey,
  onToggleGroup,
  onSelectAttempt,
  onSelectNode,
  labelledBy,
  describedBy,
}: ChartProps) {
  const scrollRef = useRef<HTMLDivElement>(null);
  const [trackW, setTrackW] = useState(FALLBACK_TRACK_W);
  const [labelW, setLabelW] = useState(LABEL_W);
  // The rows' scrollbar narrows their track; the axis is padded by the same
  // amount so a tick and the bar edge it labels stay on the same pixel.
  const [gutter, setGutter] = useState(0);
  const [scrollTop, setScrollTop] = useState(0);
  const viewportH = Math.min(MAX_VIEWPORT_H, Math.max(ROW_H, rows.length * ROW_H));
  const tooltipId = useId();

  // Measure the track from the scroller: its client width excludes the
  // scrollbar, which is exactly the width the rows' tracks get.
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const measure = () => {
      const cw = el.clientWidth;
      if (cw <= 0) return;
      const lw = cw < 640 ? LABEL_W_NARROW : LABEL_W;
      setLabelW(lw);
      setTrackW(Math.max(40, cw - lw));
      setGutter(Math.max(0, el.offsetWidth - cw));
    };
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const { first, last } = virtualRange(scrollTop, viewportH, ROW_H, rows.length);
  const ticks = useMemo(() => timeTicks(window_, Math.max(2, Math.floor(trackW / 90))), [window_, trackW]);

  // Roving focus: one row is tabbable. Kept as a KEY, so a poll that inserts a
  // row above it does not move focus to a different attempt.
  const [activeKey, setActiveKey] = useState<string | null>(null);
  const activeIndex = Math.max(
    0,
    rows.findIndex((r) => r.key === (activeKey ?? selectedKey)),
  );
  const [focusRequest, setFocusRequest] = useState(0);
  const [focused, setFocused] = useState<string | null>(null);
  const [hover, setHover] = useState<{ key: string; x: number } | null>(null);

  // Bring the selected row into view when the selection changes.
  const lastSelected = useRef<string | null>(null);
  useEffect(() => {
    if (!selectedKey || selectedKey === lastSelected.current) return;
    const idx = rows.findIndex((r) => r.key === selectedKey);
    if (idx < 0) return;
    lastSelected.current = selectedKey;
    const el = scrollRef.current;
    if (!el) return;
    const next = scrollToRow(idx, el.scrollTop, viewportH, ROW_H);
    if (next !== null) {
      el.scrollTop = next;
      setScrollTop(next);
    }
  }, [selectedKey, rows, viewportH]);

  // Move DOM focus after the target row has been mounted by the virtual window.
  useEffect(() => {
    if (focusRequest === 0) return;
    const el = scrollRef.current?.querySelector<HTMLElement>(`[data-row-index="${activeIndex}"]`);
    el?.focus({ preventScroll: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusRequest]);

  const moveTo = (idx: number) => {
    const clamped = Math.max(0, Math.min(rows.length - 1, idx));
    const row = rows[clamped];
    if (!row) return;
    setActiveKey(row.key);
    const el = scrollRef.current;
    if (el) {
      const next = scrollToRow(clamped, el.scrollTop, viewportH, ROW_H);
      if (next !== null) {
        el.scrollTop = next;
        setScrollTop(next);
      }
    }
    setFocusRequest((n) => n + 1);
  };

  const span = window_.end - window_.start;
  const zoomAbout = (factor: number, anchor: number) => onWindow(nextView(window_, factor, anchor, domain));
  const pan = (deltaMs: number) => {
    const p = panWindow(window_, deltaMs, domain);
    onWindow(isFullWindow(p, domain) ? null : p);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    const center = (window_.start + window_.end) / 2;
    const pageRows = Math.max(1, Math.floor(viewportH / ROW_H) - 1);
    switch (e.key) {
      case 'ArrowDown':
        moveTo(activeIndex + 1);
        break;
      case 'ArrowUp':
        moveTo(activeIndex - 1);
        break;
      case 'PageDown':
        moveTo(activeIndex + pageRows);
        break;
      case 'PageUp':
        moveTo(activeIndex - pageRows);
        break;
      case 'Home':
        moveTo(0);
        break;
      case 'End':
        moveTo(rows.length - 1);
        break;
      case 'ArrowLeft':
        pan(-span * PAN_FRACTION);
        break;
      case 'ArrowRight':
        pan(span * PAN_FRACTION);
        break;
      case '+':
      case '=':
        zoomAbout(1 / ZOOM_STEP, center);
        break;
      case '-':
      case '_':
        zoomAbout(ZOOM_STEP, center);
        break;
      case '0':
        onWindow(null);
        break;
      default:
        return;
    }
    e.preventDefault();
  };

  // Wheel: registered natively and non-passive, for the same reason as
  // DagCanvas -- React's onWheel is passive, so preventDefault on Ctrl+wheel
  // would be ignored and the browser would page-zoom as well.
  const latest = useRef({ window_, domain, trackW });
  latest.current = { window_, domain, trackW };
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      const { window_: w, domain: d, trackW: tw } = latest.current;
      const track = el.querySelector<HTMLElement>('.tl-track-layer');
      const left = track ? track.getBoundingClientRect().left : el.getBoundingClientRect().left;
      const x = e.clientX - left;
      if (e.ctrlKey || e.metaKey) {
        e.preventDefault();
        const factor = e.deltaY < 0 ? 1 / ZOOM_STEP : ZOOM_STEP;
        onWindow(nextView(w, factor, tOf(Math.max(0, Math.min(tw, x)), w, tw), d));
        return;
      }
      const sideways = e.shiftKey ? e.deltaY || e.deltaX : Math.abs(e.deltaX) > Math.abs(e.deltaY) ? e.deltaX : 0;
      if (sideways !== 0) {
        e.preventDefault();
        const p = panWindow(w, (sideways / tw) * (w.end - w.start), d);
        onWindow(isFullWindow(p, d) ? null : p);
      }
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, [onWindow]);

  // Drag to pan. A drag that moved suppresses the click it ends in, so panning
  // across a bar does not also select it.
  const drag = useRef<{ id: number; x: number; moved: boolean; start: TimeWindow } | null>(null);
  const suppressClick = useRef(false);
  const onPointerDown = (e: React.PointerEvent) => {
    if (e.button !== 0) return;
    drag.current = { id: e.pointerId, x: e.clientX, moved: false, start: window_ };
  };
  const onPointerMove = (e: React.PointerEvent) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    const dx = e.clientX - d.x;
    if (!d.moved && Math.abs(dx) < 4) return;
    if (!d.moved) {
      d.moved = true;
      e.currentTarget.setPointerCapture?.(e.pointerId);
    }
    const p = panWindow(d.start, (-dx / trackW) * (d.start.end - d.start.start), domain);
    onWindow(isFullWindow(p, domain) ? null : p);
  };
  const onPointerUp = (e: React.PointerEvent) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    suppressClick.current = d.moved;
    drag.current = null;
    if (e.currentTarget.hasPointerCapture?.(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
  };

  const activate = (r: TimelineRow) => {
    if (suppressClick.current) {
      suppressClick.current = false;
      return;
    }
    setActiveKey(r.key);
    if (r.kind === 'group') onToggleGroup(r.templateId);
    else if (r.kind === 'attempt') onSelectAttempt(r.nodeId, r.attempt.id);
    else onSelectNode(r.nodeId);
  };

  const tipKey = focused ?? hover?.key ?? null;
  const tipIndex = tipKey ? rows.findIndex((r) => r.key === tipKey) : -1;
  const tipRow = tipIndex >= 0 ? rows[tipIndex] : undefined;

  const nowX = live ? xOf(now, window_, trackW) : null;
  const innerH = rows.length * ROW_H;

  return (
    <div
      className="tl-chart"
      onKeyDown={onKeyDown}
      style={{ ['--tl-label-w' as string]: `${labelW}px` }}
    >
      <div className="tl-axis" aria-hidden="true" style={{ paddingRight: gutter }}>
        <div className="tl-axis-label">node · attempt</div>
        <div className="tl-axis-track">
          {ticks.map((t) => (
            <span
              key={t.t}
              className={`tl-tick${t.major ? ' major' : ''}`}
              style={{ left: xOf(t.t, window_, trackW) }}
            >
              {t.label}
            </span>
          ))}
          {nowX !== null && nowX >= 0 && nowX <= trackW && (
            <span className="tl-now-flag" style={{ left: nowX }}>
              now
            </span>
          )}
        </div>
      </div>

      <div
        className="tl-scroll"
        ref={scrollRef}
        style={{ height: viewportH }}
        onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onPointerLeave={() => setHover(null)}
      >
        <div className="tl-inner" style={{ height: innerH }}>
          <div className="tl-track-layer" aria-hidden="true" style={{ height: innerH }}>
            {ticks.map((t) => (
              <span key={t.t} className="tl-grid" style={{ left: xOf(t.t, window_, trackW) }} />
            ))}
            {nowX !== null && nowX >= 0 && nowX <= trackW && (
              <span className="tl-now" data-testid="timeline-now" style={{ left: nowX }} />
            )}
          </div>
          <div role="list" aria-labelledby={labelledBy} aria-describedby={describedBy} className="tl-list">
            {rows.slice(first, last + 1).map((r, i) => {
              const index = first + i;
              const selected = r.key === selectedKey;
              return (
                <div
                  key={r.key}
                  role="listitem"
                  aria-setsize={rows.length}
                  aria-posinset={index + 1}
                  className={`tl-row tl-row-${r.kind}${selected ? ' selected' : ''}${
                    'depth' in r && r.depth === 1 ? ' child' : ''
                  }${r.kind === 'attempt' && !r.firstOfNode ? ' retry' : ''}`}
                  style={{ top: index * ROW_H, height: ROW_H }}
                >
                  <button
                    type="button"
                    className="tl-row-btn"
                    data-row-index={index}
                    data-row-key={r.key}
                    tabIndex={index === activeIndex ? 0 : -1}
                    aria-label={rowLabel(r)}
                    {...(r.kind === 'group' ? { 'aria-expanded': !r.collapsed } : { 'aria-pressed': selected })}
                    aria-describedby={tipKey === r.key ? tooltipId : undefined}
                    onClick={() => activate(r)}
                    onFocus={() => {
                      setFocused(r.key);
                      setActiveKey(r.key);
                    }}
                    onBlur={() => setFocused((f) => (f === r.key ? null : f))}
                    onPointerEnter={(e) => setHover({ key: r.key, x: e.clientX })}
                    onPointerMove={(e) => setHover({ key: r.key, x: e.clientX })}
                  >
                    <RowLabel row={r} />
                    <span className="tl-track">
                      <RowBars row={r} window_={window_} width={trackW} />
                    </span>
                  </button>
                </div>
              );
            })}
          </div>
        </div>
      </div>

      {tipRow && tipIndex >= first && tipIndex <= last && (
        <Tooltip
          id={tooltipId}
          row={tipRow}
          top={28 + (tipIndex + 1) * ROW_H - scrollTop}
          left={tooltipLeft(tipRow, hover, scrollRef.current, window_, trackW, labelW)}
        />
      )}
    </div>
  );
}

function tooltipLeft(
  row: TimelineRow,
  hover: { key: string; x: number } | null,
  scroller: HTMLElement | null,
  w: TimeWindow,
  trackW: number,
  labelW: number,
): number {
  if (hover && hover.key === row.key && scroller) {
    return Math.max(0, hover.x - scroller.getBoundingClientRect().left + 12);
  }
  const start = row.kind === 'attempt' ? row.span.start : row.kind === 'group' ? row.start : null;
  const x = start !== null ? Math.max(0, Math.min(trackW, xOf(start, w, trackW))) : 0;
  return labelW + x;
}

function RowLabel({ row }: { row: TimelineRow }) {
  if (row.kind === 'group') {
    return (
      <span className="tl-label">
        <span className="tl-caret" aria-hidden="true">
          {row.collapsed ? '▸' : '▾'}
        </span>
        <span className="tl-name">{row.templateId}</span>
        <span className="tl-meta">
          map · {row.childCount} item{row.childCount === 1 ? '' : 's'}
        </span>
      </span>
    );
  }
  if (row.kind === 'idle') {
    const v = stateVisual('node', row.node.state);
    return (
      <span className="tl-label">
        <span className={`tl-glyph tone-${v.tone}`} aria-hidden="true">
          {v.glyph}
        </span>
        <span className="tl-name">{row.nodeId}</span>
        <span className="tl-meta">{v.label.toLowerCase()}</span>
      </span>
    );
  }
  const v = stateVisual('attempt', row.attempt.state);
  return (
    <span className="tl-label">
      <span className={`tl-glyph tone-${v.tone}`} aria-hidden="true">
        {v.glyph}
      </span>
      <span className="tl-name">{row.firstOfNode ? row.nodeId : '↳ retry'}</span>
      <span className="tl-meta">#{row.attempt.attempt_number}</span>
    </span>
  );
}

function RowBars({ row, window_, width }: { row: TimelineRow; window_: TimeWindow; width: number }) {
  if (row.kind === 'idle') return null;
  if (row.kind === 'group') {
    if (row.start === null || row.end === null) return null;
    const ext = barExtent(row.start, row.end, window_, width);
    if (!ext) return null;
    return (
      <span
        className={`tl-bar tl-phase-envelope tone-${row.tone}${row.open ? ' open' : ''}`}
        style={{ left: ext.left, width: ext.width }}
      />
    );
  }
  return (
    <>
      {row.span.phases.map((p) => {
        const ext = barExtent(p.from, p.to, window_, width);
        if (!ext) return null;
        return (
          <span
            key={p.kind}
            data-phase={p.kind}
            className={`tl-bar tl-phase-${p.kind} tone-${row.tone}${p.open ? ' open' : ''}${
              row.attempt.cache_hit_of ? ' cached' : ''
            }`}
            style={{ left: ext.left, width: ext.width }}
          />
        );
      })}
    </>
  );
}

function Tooltip({ id, row, top, left }: { id: string; row: TimelineRow; top: number; left: number }) {
  let body: React.ReactNode;
  if (row.kind === 'attempt') {
    const s = row.span;
    const a = row.attempt;
    body = (
      <>
        <div className="tl-tip-head">
          <strong>{row.nodeId}</strong> attempt #{a.attempt_number} · {stateVisual('attempt', a.state).label}
        </div>
        <dl>
          <dt>queued</dt>
          <dd>{phaseText(s.queuedMs, s, 'queued')}</dd>
          <dt>starting</dt>
          <dd>{phaseText(s.startingMs, s, 'starting')}</dd>
          <dt>running</dt>
          <dd>{phaseText(s.runningMs, s, 'running')}</dd>
          <dt>total</dt>
          <dd>{fmtDurationMs(s.totalMs)}{s.open ? ' so far' : ''}</dd>
          <dt>worker</dt>
          <dd>{a.worker_hostname ?? a.worker_id ?? '—'}</dd>
          <dt>scheduled</dt>
          <dd>{fmtTs(a.scheduled_at)}</dd>
        </dl>
        {a.cache_hit_of && <div className="tl-tip-note">Served from cache; nothing executed.</div>}
      </>
    );
  } else if (row.kind === 'group') {
    body = (
      <>
        <div className="tl-tip-head">
          <strong>{row.templateId}</strong> · map of {row.childCount}
        </div>
        <dl>
          {Object.entries(row.counts).map(([s, n]) => (
            <FragmentPair key={s} term={stateVisual('node', s).label.toLowerCase()} value={String(n)} />
          ))}
          <dt>attempts</dt>
          <dd>{row.attemptCount}</dd>
          <dt>span</dt>
          <dd>
            {row.start !== null && row.end !== null ? fmtDurationMs(row.end - row.start) : '—'}
            {row.open ? ' so far' : ''}
          </dd>
        </dl>
      </>
    );
  } else {
    body = (
      <div className="tl-tip-head">
        <strong>{row.nodeId}</strong> · {stateVisual('node', normalizeState(row.node.state)).hint}
      </div>
    );
  }
  return (
    <div id={id} role="tooltip" className="tl-tip" style={{ top, left }}>
      {body}
    </div>
  );
}

function FragmentPair({ term, value }: { term: string; value: string }) {
  return (
    <>
      <dt>{term}</dt>
      <dd>{value}</dd>
    </>
  );
}

function TimelineTable({
  rows,
  selectedKey,
  onToggleGroup,
  onSelectAttempt,
  onSelectNode,
}: {
  rows: TimelineRow[];
  selectedKey: string | null;
  onToggleGroup: (templateId: string) => void;
  onSelectAttempt: (nodeId: string, attemptId: string) => void;
  onSelectNode: (nodeId: string) => void;
}) {
  return (
    <div className="table-scroll tl-table-wrap">
      <table className="tl-table">
        <caption className="sr-only">Task attempts in execution order, with their phase durations</caption>
        <thead>
          <tr>
            <th scope="col">Node</th>
            <th scope="col">Attempt</th>
            <th scope="col">State</th>
            <th scope="col">Worker</th>
            <th scope="col">Scheduled</th>
            <th scope="col">Queued</th>
            <th scope="col">Starting</th>
            <th scope="col">Running</th>
            <th scope="col">Total</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const selected = r.key === selectedKey;
            if (r.kind === 'group') {
              return (
                <tr key={r.key} className="tl-table-group">
                  <th scope="rowgroup" colSpan={9}>
                    <button type="button" className="link" aria-expanded={!r.collapsed} onClick={() => onToggleGroup(r.templateId)}>
                      {r.collapsed ? '▸' : '▾'} {r.templateId}
                    </button>{' '}
                    <span className="faint">
                      map · {r.childCount} item{r.childCount === 1 ? '' : 's'} · {r.attemptCount} attempt
                      {r.attemptCount === 1 ? '' : 's'}
                    </span>
                  </th>
                </tr>
              );
            }
            if (r.kind === 'idle') {
              return (
                <tr key={r.key} aria-current={selected || undefined} className={selected ? 'selected' : undefined}>
                  <th scope="row">
                    <button type="button" className="link" onClick={() => onSelectNode(r.nodeId)}>
                      {r.nodeId}
                    </button>
                  </th>
                  <td>—</td>
                  <td>{stateVisual('node', r.node.state).label}</td>
                  <td colSpan={6} className="faint">
                    no attempt
                  </td>
                </tr>
              );
            }
            const s = r.span;
            const a = r.attempt;
            return (
              <tr key={r.key} aria-current={selected || undefined} className={selected ? 'selected' : undefined}>
                <th scope="row">
                  <button
                    type="button"
                    className="link"
                    aria-label={`Select ${r.nodeId} attempt ${a.attempt_number}`}
                    onClick={() => onSelectAttempt(r.nodeId, a.id)}
                  >
                    {r.nodeId}
                  </button>
                </th>
                <td>#{a.attempt_number}</td>
                <td>{stateVisual('attempt', a.state).label}</td>
                <td>{a.worker_hostname ?? '—'}</td>
                <td className="nowrap">{fmtTs(a.scheduled_at)}</td>
                <td>{phaseText(s.queuedMs, s, 'queued')}</td>
                <td>{phaseText(s.startingMs, s, 'starting')}</td>
                <td>{phaseText(s.runningMs, s, 'running')}</td>
                <td>
                  {fmtDurationMs(s.totalMs)}
                  {s.open ? ' so far' : ''}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
