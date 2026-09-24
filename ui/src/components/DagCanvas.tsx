// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The interactive viewport both DAG views are drawn inside: zoom, pan,
 * fit-to-view, export and share.
 *
 * WHY THIS EXISTS AT ALL. Both views previously rendered their SVG at its
 * INTRINSIC size inside a box that only scrolled. A seven-layer run at
 * NODE_W 196 + GAP_X 104 is ~2048px wide against a ~1500px panel, so the
 * default view of a normal DAG had its first and last node off-screen — the
 * header said "10 nodes" while showing eight, and the only way to learn the
 * others existed was to scroll sideways on a hunch. A deep DAG is the common
 * case, not the edge case, so that was the default experience of the screen an
 * operator opens during an incident.
 *
 * WHY ONE COMPONENT FOR BOTH VIEWS rather than a hook each. A catalog spec
 * graph overflows for exactly the same reason as a run graph — it is the same
 * layout function — so the two need the same controls. Sharing the container
 * rather than the logic alone is what keeps the two toolbars from drifting into
 * different keyboard shortcuts and different zoom limits for the same gesture.
 * The views keep their own node bodies, which is where they legitimately
 * differ: four channels of STATE versus a definition that has no state.
 *
 * WHY A TRANSFORM ON A GROUP RATHER THAN MANIPULATING THE viewBox. Both work
 * arithmetically. The group transform wins on three counts that matter here:
 *
 *  1. The viewBox stays the graph's intrinsic coordinate space, so everything
 *     lib/dag computes — node placements, edge elbow routes, the halo inset —
 *     keeps meaning exactly what it meant before, and nothing downstream of
 *     layoutLayers has to know that a zoom exists.
 *  2. Hit-testing and focus stay sane. Nodes are focusable role="button"
 *     elements and that is the accessible path through the graph; a transform is
 *     applied by the same engine that does hit-testing, so a click at 40% zoom
 *     lands on the node the operator can see. A hand-computed viewBox that
 *     drifts from the pan state by a rounding error produces the worst possible
 *     bug: the graph looks right and clicks land on the wrong node.
 *  3. Fractional zoom cannot corrupt the coordinate space. A viewBox carries
 *     four numbers that must stay mutually consistent across every gesture;
 *     a transform carries the same information but the browser composes it.
 */

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import {
  ZOOM_MAX,
  ZOOM_MIN,
  centerOffset,
  centerOn,
  clampZoom,
  exportFileName,
  fitScale,
  initialView,
  inlineComputedStyles,
  minimapScale,
  offscreenCount,
  overflowEdges,
  svgDocument,
  zoomAbout,
  zoomIn,
  zoomLabel,
  zoomOut,
} from '../lib/dagview';
import { NODE_H, NODE_W } from '../lib/dag';
// The one component-scoped stylesheet in the app.
//
// WHY NOT index.css, which is where every other rule lives. The DAG canvas is
// the only place in the console whose type sizes are NOT part of the type
// scale: its labels sit at fixed x/y offsets inside a fixed 196x64 SVG box, so
// they cannot reflow, and a change to --text-* — a base size raised from 13 to
// 14px, say — would push the sub row into the state row rather than making the
// text bigger. Those sizes therefore have to be literal, and a block of
// deliberately token-free declarations sitting in the middle of the token-based
// stylesheet reads as an oversight. Isolating it makes the exception explicit
// and local, which is also why the canvas is the right place to draw the line:
// nothing outside these two views has a fixed-geometry text problem.
import './dag.css';

/** View state: one scale and one translation, in screen pixels. */
interface View {
  scale: number;
  x: number;
  y: number;
}

const IDENTITY: View = { scale: 1, x: 0, y: 0 };

export interface DagCanvasProps {
  /** Intrinsic graph dimensions from layoutLayers: the viewBox extent. */
  width: number;
  height: number;
  /**
   * Identity of the graph being drawn — a run id plus a node count, typically.
   * Fit-to-view runs ONCE per distinct value of this.
   *
   * This prop is the whole anti-fighting-the-operator mechanism. The run detail
   * page polls, so `graph` is a fresh object on every refetch; keying the
   * auto-fit on the object would re-fit every few seconds and snap the view
   * back while someone was mid-inspection of a node. Keying it on a declared
   * identity string means a refetch of the same run is a no-op and navigating
   * to a different run fits afresh. Same lesson as the attention panel's
   * expansion deliberately surviving its refetch.
   */
  graphKey: string;
  /** Accessible name for the SVG. */
  label: string;
  /** Node/edge content, drawn in intrinsic graph coordinates. */
  children: ReactNode;
  /** <defs> content: markers, which must sit outside the zoomed group. */
  defs?: ReactNode;
  /** Identifies the export file. Absent ⇒ export buttons are hidden, because a
   *  download called nexus-graph-dag.svg tells its recipient nothing. */
  exportName?: string;
  /** Extra toolbar controls, rendered before the zoom group. */
  toolbarExtra?: ReactNode;
  /** Readout shown in the toolbar: node and layer counts. */
  stats?: ReactNode;
  /** Collapsible legend content, rendered under the canvas. */
  legend?: ReactNode;
  legendLabel?: string;
  viewportHeight?: number;
  focusTarget?: { x: number; y: number };
  /**
   * Where the boxes are, for the minimap and the off-screen count. Optional:
   * a caller that draws something other than NODE_W x NODE_H boxes gets the
   * canvas without either. `tone` colours the minimap's dot for that box.
   */
  places?: { id: string; x: number; y: number; tone?: string }[];
}

/** Thumbnail bounds. Wide rather than square because the graphs are. */
const MINIMAP_MAX = { width: 200, height: 110 };
/** Keyboard pan step, in screen pixels. */
const PAN_STEP = 80;

export function DagCanvas({
  width,
  height,
  graphKey,
  label,
  children,
  defs,
  exportName,
  toolbarExtra,
  stats,
  legend,
  legendLabel = 'legend',
  focusTarget,
  viewportHeight,
  places,
}: DagCanvasProps) {
  const viewportRef = useRef<HTMLDivElement | null>(null);
  const svgRef = useRef<SVGSVGElement | null>(null);
  const [view, setView] = useState<View>(IDENTITY);
  const [box, setBox] = useState({ width: 0, height: 0 });
  const [notice, setNotice] = useState<string | null>(null);
  const [shareUrl, setShareUrl] = useState<string | null>(null);
  const [legendOpen, setLegendOpen] = useState(false);
  const [expanded, setExpanded] = useState(false);

  /**
   * Container measurement. ResizeObserver rather than a window resize
   * listener: the panel also changes width when the sidebar collapses or a
   * sibling panel appears, neither of which fires a window resize, and a
   * fit computed against a stale width is a fit that does not fit.
   *
   * Guarded because jsdom has no ResizeObserver — the tests still need the
   * component to render, and an unmeasured container falls back to "1, show
   * everything", which is the pre-existing behaviour rather than a blank box.
   */
  useLayoutEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const measure = () =>
      setBox({ width: el.clientWidth, height: el.clientHeight });
    measure();
    if (typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const fit = useCallback(() => {
    const el = viewportRef.current;
    const v = { width: el?.clientWidth ?? box.width, height: el?.clientHeight ?? box.height };
    const scale = fitScale({ width, height }, v);
    setView({ scale, ...centerOffset({ width, height }, v, scale) });
  }, [width, height, box.width, box.height]);

  // The opening view, on graph or viewport size changes, never on ordinary
  // status polling. Not `fit`: a fit of a deep graph is unreadable, so the
  // canvas opens readable and anchored to the first layer (lib/dagview
  // initialView), and the Fit button remains the way to see everything.
  const fittedFor = useRef<string | null>(null);
  useEffect(() => {
    if (box.width <= 0 || box.height <= 0) return;
    const identity = `${graphKey}:${box.width}:${box.height}`;
    if (fittedFor.current === identity) return;
    fittedFor.current = identity;
    setView(initialView({ width, height }, box));
  }, [graphKey, box.width, box.height, width, height]);

  useEffect(() => {
    if (!focusTarget) return;
    const el = viewportRef.current;
    if (!el) return;
    setView({ scale: 1, ...centerOn(focusTarget, 1, { width: el.clientWidth, height: el.clientHeight }) });
  }, [focusTarget]);

  const reset = () => setView({ scale: 1, ...centerOffset({ width, height }, box, 1) });

  const setScale = useCallback(
    (next: number) => {
      // Zoom from a button is about the centre of the viewport, which is where
      // the operator is looking. Zooming about the origin instead would walk
      // the content off the top-left corner over a few clicks.
      const el = viewportRef.current;
      const w = el?.clientWidth ?? box.width;
      const h = el?.clientHeight ?? box.height;
      setView((v) => zoomAbout(v, next, { x: w / 2, y: h / 2 }));
    },
    [box.width, box.height],
  );

  /** Pan. setPointerCapture so a drag that leaves the element keeps tracking:
   *  without it, dragging a wide graph past the panel edge drops the gesture
   *  and the graph stops following the cursor mid-pan. */
  const drag = useRef<{ id: number; x: number; y: number } | null>(null);
  const [dragging, setDragging] = useState(false);

  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    // Primary button only, and never a drag that started on a node: the nodes
    // are buttons and stealing their click would remove the primary way of
    // selecting one.
    if (e.button !== 0) return;
    if ((e.target as Element).closest('[role="button"], .dag-minimap')) return;
    drag.current = { id: e.pointerId, x: e.clientX, y: e.clientY };
    setDragging(true);
    e.currentTarget.setPointerCapture(e.pointerId);
  };

  const onPointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    const d = drag.current;
    if (!d || d.id !== e.pointerId) return;
    const dx = e.clientX - d.x;
    const dy = e.clientY - d.y;
    drag.current = { id: e.pointerId, x: e.clientX, y: e.clientY };
    setView((v) => ({ ...v, x: v.x + dx, y: v.y + dy }));
  };

  const endDrag = (e: React.PointerEvent<HTMLDivElement>) => {
    if (drag.current?.id !== e.pointerId) return;
    drag.current = null;
    setDragging(false);
    if (e.currentTarget.hasPointerCapture(e.pointerId)) {
      e.currentTarget.releasePointerCapture(e.pointerId);
    }
  };

  /**
   * Ctrl/Cmd + wheel zooms; a plain wheel is left entirely alone.
   *
   * Hijacking a plain wheel is the single most hostile thing an embedded canvas
   * can do: the operator scrolls the page, the page stops moving, and the graph
   * silently zooms instead. So the plain wheel is not handled at all and the
   * page scrolls past the canvas the way it does past a table.
   *
   * Registered as a non-passive native listener rather than via onWheel,
   * because React's wheel handler is passive and preventDefault() inside it is
   * ignored — so Ctrl+wheel would zoom the graph AND page-zoom the browser.
   */
  /**
   * ...with one addition: a SIDEWAYS wheel pans the graph. A horizontal
   * trackpad swipe or Shift+wheel has no meaning to the page, which does not
   * scroll sideways, so taking it costs the operator nothing — and it is the
   * gesture everyone tries first on a wide diagram. Vertical wheel is still
   * the page's. Taken only while the graph actually extends past the sides,
   * so a graph that fits never swallows a stray swipe.
   */
  const latest = useRef({ view, box, width, height });
  latest.current = { view, box, width, height };
  useEffect(() => {
    const el = viewportRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (!e.ctrlKey && !e.metaKey) {
        const sideways = e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY);
        if (!sideways) return;
        const { view: v, box: b, width: w, height: h } = latest.current;
        const edges = overflowEdges({ width: w, height: h }, v, b);
        if (!edges.left && !edges.right) return;
        e.preventDefault();
        const delta = e.shiftKey && Math.abs(e.deltaX) < Math.abs(e.deltaY) ? e.deltaY : e.deltaX;
        setView((cur) => ({ ...cur, x: cur.x - delta }));
        return;
      }
      e.preventDefault();
      const rect = el.getBoundingClientRect();
      const pointer = { x: e.clientX - rect.left, y: e.clientY - rect.top };
      // deltaY is in lines or pixels depending on the device, so only its sign
      // is trusted; the magnitude comes from the same ratio the buttons use, so
      // a wheel notch and a button click are the same size of step.
      setView((v) => zoomAbout(v, e.deltaY < 0 ? zoomIn(v.scale) : zoomOut(v.scale), pointer));
    };
    el.addEventListener('wheel', onWheel, { passive: false });
    return () => el.removeEventListener('wheel', onWheel);
  }, []);

  /**
   * Keyboard zoom, scoped to the canvas.
   *
   * Handled on the viewport rather than on the document so +/-/0 cannot steal
   * keys from the page — the command palette and the page's own shortcuts stay
   * reachable when the graph merely happens to be on screen. The node buttons'
   * Enter/Space handling is untouched: those keys are not in this switch, and
   * a keypress on a node bubbles up here only for the zoom keys.
   */
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    switch (e.key) {
      case '+':
      case '=':
        e.preventDefault();
        setScale(zoomIn(view.scale));
        break;
      case '-':
      case '_':
        e.preventDefault();
        setScale(zoomOut(view.scale));
        break;
      case '0':
        e.preventDefault();
        reset();
        break;
      case 'f':
        e.preventDefault();
        fit();
        break;
      // Plain arrows pan the canvas. Shift+arrows are the cards' own (they
      // move a card, in useTaskLayout) and are stopped before they get here.
      case 'ArrowLeft':
      case 'ArrowRight':
      case 'ArrowUp':
      case 'ArrowDown': {
        if (e.shiftKey) return;
        e.preventDefault();
        const dx = e.key === 'ArrowLeft' ? PAN_STEP : e.key === 'ArrowRight' ? -PAN_STEP : 0;
        const dy = e.key === 'ArrowUp' ? PAN_STEP : e.key === 'ArrowDown' ? -PAN_STEP : 0;
        setView((v) => ({ ...v, x: v.x + dx, y: v.y + dy }));
        break;
      }
      default:
        break;
    }
  };

  /**
   * Minimap: the whole graph as a thumbnail with the viewport drawn on it.
   * Click or drag anywhere on it to put that point under the middle of the
   * view. This is what every workflow designer offers for a diagram wider
   * than its panel, and it answers "where am I" and "how do I get there" in
   * one gesture where the old toolbar hint answered neither.
   */
  const mapScale = minimapScale({ width, height }, MINIMAP_MAX);
  const mapDrag = useRef<number | null>(null);
  const jumpTo = (e: React.PointerEvent<SVGSVGElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const point = { x: (e.clientX - rect.left) / mapScale, y: (e.clientY - rect.top) / mapScale };
    setView((v) => ({ ...v, ...centerOn(point, v.scale, box) }));
  };

  /** Transient toolbar message ("Link copied", "Export failed"). Cleared on a
   *  timer so it does not become permanent furniture. */
  const flash = useCallback((message: string) => {
    setNotice(message);
    window.setTimeout(() => setNotice((n) => (n === message ? null : n)), 2600);
  }, []);

  /**
   * Serialises the live SVG with its appearance inlined.
   *
   * Exported at the graph's INTRINSIC size with the zoom transform stripped:
   * the file is the graph, not a photo of the operator's current viewport. A
   * recipient who wanted the zoomed crop can zoom their own viewer; a recipient
   * who got a 40%-scaled render of half a graph cannot recover the rest.
   */
  const serialise = useCallback((): string | null => {
    const live = svgRef.current;
    if (!live) return null;
    const clone = live.cloneNode(true) as SVGSVGElement;
    // Drop the pan/zoom transform so the clone sits in intrinsic coordinates.
    const zoomed = clone.querySelector('g.dag-zoom');
    zoomed?.removeAttribute('transform');
    inlineComputedStyles(live, clone, (el) => window.getComputedStyle(el));
    // The canvas colour has to be carried explicitly: an SVG has no background
    // of its own, so a dark-theme graph would arrive as pale text on the
    // viewer's white default.
    const ground = window.getComputedStyle(live).getPropertyValue('background-color');
    const inner = new XMLSerializer().serializeToString(clone)
      // Unwrap: the clone's own <svg> attributes are replaced by svgDocument's,
      // which are the ones that make the file standalone.
      .replace(/^<svg[^>]*>/, '')
      .replace(/<\/svg>$/, '');
    return svgDocument(inner, {
      width,
      height,
      viewBox: `0 0 ${width} ${height}`,
      background: ground && ground !== 'rgba(0, 0, 0, 0)' ? ground : '#ffffff',
    });
  }, [width, height]);

  const download = (blob: Blob, filename: string) => {
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = filename;
    a.rel = 'noopener';
    document.body.appendChild(a);
    a.click();
    a.remove();
    // Revoked on a turn of the event loop rather than immediately: some
    // browsers have not started reading the blob when click() returns, and
    // revoking synchronously produces a zero-byte file.
    window.setTimeout(() => URL.revokeObjectURL(url), 0);
  };

  const exportSvg = () => {
    const markup = serialise();
    if (!markup) {
      flash('Export failed');
      return;
    }
    download(new Blob([markup], { type: 'image/svg+xml;charset=utf-8' }), exportFileName(exportName, 'svg'));
    flash('SVG downloaded');
  };

  /**
   * PNG via a canvas raster of the SAME serialised SVG the SVG export produces.
   *
   * Rasterising the already-style-inlined markup is what makes the PNG faithful
   * rather than a second, differently-broken export: there is exactly one
   * serialiser, so a colour that appears in the SVG appears in the PNG by
   * construction. A data: URL is used for the image source instead of a blob
   * URL because a blob-sourced SVG taints the canvas in some browsers and
   * toBlob then throws a SecurityError — the export appears to work and
   * produces nothing.
   *
   * 2x scale: an operator pastes this into an incident ticket, and a 1x raster
   * of 10px node labels is unreadable at the size a ticket renders it.
   */
  const exportPng = () => {
    const markup = serialise();
    if (!markup) {
      flash('Export failed');
      return;
    }
    const scale = 2;
    const canvas = document.createElement('canvas');
    canvas.width = Math.max(1, Math.round(width * scale));
    canvas.height = Math.max(1, Math.round(height * scale));
    const ctx = canvas.getContext('2d');
    if (!ctx || typeof canvas.toBlob !== 'function') {
      flash('PNG unsupported — use SVG');
      return;
    }
    const img = new Image();
    img.onload = () => {
      ctx.setTransform(scale, 0, 0, scale, 0, 0);
      ctx.drawImage(img, 0, 0, width, height);
      canvas.toBlob((blob) => {
        if (!blob) {
          flash('PNG failed — use SVG');
          return;
        }
        download(blob, exportFileName(exportName, 'png'));
        flash('PNG downloaded');
      }, 'image/png');
    };
    img.onerror = () => flash('PNG failed — use SVG');
    // encodeURIComponent, not btoa: the markup can contain non-Latin-1
    // characters (a node id with an accent, the ✓ and ✕ state glyphs) and btoa
    // throws on those, which would break the export for exactly the graphs
    // whose glyphs make it worth exporting.
    img.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(markup)}`;
  };

  /**
   * Share: the current URL, which already identifies the selected node.
   *
   * Node selection lives in the query string (`?node=...`), so the current
   * location is already a link that reopens this run with this node selected.
   * That is why share copies the URL as-is rather than constructing one.
   *
   * ZOOM AND PAN ARE DELIBERATELY NOT ENCODED. They were considered and
   * rejected: a pan gesture is a continuous stream of view states, so putting
   * them in the URL means either churning history on every frame or debouncing
   * a write that is still wrong for the frame it lands on, and the receiving
   * end then opens at a scale someone else chose for a window that is not
   * theirs — which re-creates the original bug (a graph that does not fit the
   * viewport) for a recipient on a smaller screen. Fit-to-view on arrival is
   * both cheaper and more likely to be what the recipient wanted. What a shared
   * link must reproduce is WHICH NODE, and that is already in the URL.
   *
   * navigator.clipboard throws in an insecure context and rejects when
   * permission is denied, and it is absent entirely in some embedded webviews,
   * so every one of those paths falls back to a readonly input with the link
   * pre-selected. A share button that silently does nothing is worse than no
   * share button, because the operator believes they have copied a link.
   */
  const share = async () => {
    const url = window.location.href;
    try {
      if (!navigator.clipboard?.writeText) throw new Error('clipboard unavailable');
      await navigator.clipboard.writeText(url);
      flash('Link copied');
    } catch {
      setShareUrl(url);
    }
  };

  const atMin = view.scale <= ZOOM_MIN + 1e-6;
  const atMax = view.scale >= ZOOM_MAX - 1e-6;

  /**
   * Does content extend past the viewport at the CURRENT scale? Drives the
   * "scroll/pan for more" hint, which is the minimum honest answer to the
   * original bug: if a node is off-screen the operator is told so rather than
   * having to guess from a count that does not match what they can see.
   */
  const edges = useMemo(() => overflowEdges({ width, height }, view, box), [box, width, height, view]);
  const clipped = edges.left || edges.right || edges.top || edges.bottom;
  const offscreen = useMemo(
    () => (places ? offscreenCount(places, { width: NODE_W, height: NODE_H }, view, box) : 0),
    [places, view, box],
  );

  return (
    <>
      <div className="dag-toolbar">
        {stats}
        {toolbarExtra}
        <span className="dag-toolbar-spacer" />
        {clipped && (
          <span className="dag-clip-hint" role="status">
            {places && offscreen > 0
              ? `${offscreen} of ${places.length} off-screen · scroll sideways, drag, or use the map`
              : 'part of the graph is outside the view · drag to pan'}
          </span>
        )}
        {notice && (
          <span className="dag-flash" role="status">
            {notice}
          </span>
        )}
        <div className="dag-zoom-group" role="group" aria-label="zoom">
          <button type="button" onClick={() => setScale(zoomOut(view.scale))} disabled={atMin} title="Zoom out (−)" aria-label="Zoom out">
            −
          </button>
          {/* The readout is a button as well as a display: "reset to 100%" is
              the action an operator wants the instant they notice the number is
              not 100%, so it lives on the number itself. */}
          <button
            type="button"
            className="dag-zoom-readout"
            onClick={reset}
            title="Reset to 100%"
            aria-label={`Zoom ${zoomLabel(view.scale)}, reset to 100%`}
          >
            {zoomLabel(view.scale)}
          </button>
          <button type="button" onClick={() => setScale(zoomIn(view.scale))} disabled={atMax} title="Zoom in (+)" aria-label="Zoom in">
            +
          </button>
          <button type="button" onClick={fit} title="Fit the whole graph in the view (F)">
            Fit
          </button>
        </div>
        <button type="button" className="dag-expand" aria-pressed={expanded} onClick={() => setExpanded(value => !value)}>
          {expanded ? 'Compact view' : 'Expand view'}
        </button>
        <div className="dag-action-group" role="group" aria-label="graph actions">
          {exportName && (
            <>
              <button type="button" onClick={exportSvg} title="Download as SVG (vector, exact)">
                SVG
              </button>
              <button type="button" onClick={exportPng} title="Download as PNG (2x raster)">
                PNG
              </button>
            </>
          )}
          <button type="button" onClick={share} title="Copy a link to this view, with the selected node">
            Share
          </button>
        </div>
      </div>

      {/*
        Clipboard fallback. A readonly input with its text selected is the one
        path that works with no permissions at all: the operator presses their
        own copy shortcut, which no API can deny them.
      */}
      {shareUrl && (
        <div className="dag-share-fallback">
          <label htmlFor="dag-share-url">
            Clipboard access was denied. Copy this link:
          </label>
          <input
            id="dag-share-url"
            readOnly
            value={shareUrl}
            ref={(el) => el?.select()}
            onFocus={(e) => e.currentTarget.select()}
          />
          <button type="button" onClick={() => setShareUrl(null)} aria-label="Dismiss link">
            Done
          </button>
        </div>
      )}

      <div
        ref={viewportRef}
        className={
          `dag-viewport${dragging ? ' dragging' : ''}${expanded ? ' expanded' : ''}` +
          `${edges.left ? ' overflow-left' : ''}${edges.right ? ' overflow-right' : ''}` +
          `${edges.top ? ' overflow-top' : ''}${edges.bottom ? ' overflow-bottom' : ''}`
        }
        style={{ height: expanded ? 'min(72vh, 850px)' : `${Math.min(540, Math.max(280, (viewportHeight ?? height) + 80))}px` }}
        tabIndex={0}
        aria-label="Interactive graph canvas. Drag to pan, plus or minus to zoom, F to fit."
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onKeyDown={onKeyDown}
      >
        {/*
          width/height 100%, and the viewBox is the CONTAINER's pixel box — not
          the graph's extent.

          It used to be the graph's extent with preserveAspectRatio="none", on
          the reasoning that the zoom group owns scale so the SVG must not also
          fit content. The first half was right and the second half is what the
          attribute actually did: mapping a `0 0 width height` viewBox onto a
          100%-sized element IS a fit, and disabling preserveAspectRatio made it
          a non-uniform one. A single 196x64 node in a 1700x700 panel was
          therefore stretched about 9x across and 11x down — rendering as a
          full-width box with enormous text while the readout still said 100%,
          because the zoom group genuinely was at 1. The readout was not lying;
          the coordinate space underneath it was.

          With the viewBox set to the container, one user unit is one CSS pixel
          at every size, so scale belongs to the zoom group alone and "100%"
          means a node is drawn at its intrinsic 196x64. Nothing about panning,
          fitting or hit-testing changes: those are all computed in the same
          pixel space the viewBox now declares.

          The fallback to the graph extent covers the pre-measurement render
          (and jsdom, which has no ResizeObserver): a zero-area viewBox would
          blank the canvas, so an unmeasured container keeps the old behaviour
          for the one frame before the measurement lands.
        */}
        <svg
          ref={svgRef}
          width="100%"
          height="100%"
          viewBox={
            box.width > 0 && box.height > 0
              ? `0 0 ${box.width} ${box.height}`
              : `0 0 ${width} ${height}`
          }
          role="img"
          aria-label={label}
          // Uniform now that the viewBox matches the element's own aspect ratio:
          // there is nothing left for it to distort, and leaving it at "none"
          // would re-introduce the stretch during the pre-measurement frame.
          preserveAspectRatio="xMidYMid meet"
        >
          {/* Markers live outside the zoomed group: a marker inherits the
              referencing path's user space, so putting the defs inside would
              scale arrowheads twice. */}
          <defs>{defs}</defs>
          {/*
            One group carries the whole view transform. translate before scale,
            so the translation is in screen pixels and a drag of 10px moves the
            content 10px at every zoom level — with the operations reversed the
            same drag would move it 10*scale px and panning would feel
            progressively heavier as you zoomed in.
          */}
          <g className="dag-zoom" transform={`translate(${view.x},${view.y}) scale(${view.scale})`}>
            {children}
          </g>
        </svg>
        {places && places.length > 1 && clipped && box.width > 0 && (
          <svg
            className="dag-minimap"
            width={Math.max(1, Math.round(width * mapScale))}
            height={Math.max(1, Math.round(height * mapScale))}
            viewBox={`0 0 ${width} ${height}`}
            role="img"
            aria-label={`Map of the graph. ${offscreen} of ${places.length} nodes off-screen. Click to move the view.`}
            onPointerDown={(e) => {
              if (e.button !== 0) return;
              e.stopPropagation();
              mapDrag.current = e.pointerId;
              e.currentTarget.setPointerCapture?.(e.pointerId);
              jumpTo(e);
            }}
            onPointerMove={(e) => {
              if (mapDrag.current !== e.pointerId) return;
              jumpTo(e);
            }}
            onPointerUp={(e) => {
              if (mapDrag.current !== e.pointerId) return;
              mapDrag.current = null;
              if (e.currentTarget.hasPointerCapture?.(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
            }}
            onPointerCancel={() => {
              mapDrag.current = null;
            }}
          >
            {places.map((p) => (
              <rect
                key={p.id}
                className={`dag-minimap-node${p.tone ? ` tone-${p.tone}` : ''}`}
                x={p.x}
                y={p.y}
                width={NODE_W}
                height={NODE_H}
                rx={NODE_H / 4}
              />
            ))}
            <rect
              className="dag-minimap-view"
              x={-view.x / view.scale}
              y={-view.y / view.scale}
              width={box.width / view.scale}
              height={box.height / view.scale}
              rx={6}
            />
          </svg>
        )}
      </div>

      <div className="dag-guidance"><span>Select a task to inspect its details</span><span>Drag or scroll sideways to pan · Ctrl/⌘ + scroll to zoom · F to fit</span></div>
      {legend && (
        /*
          The legend was two permanently-open rows under every graph — a
          reference table an operator reads once and then scrolls past forever,
          occupying space the graph itself wanted. <details> rather than a
          custom disclosure: it is open-able without JavaScript, is announced as
          expandable, and keeps its state when the run refetches because the
          browser owns it, not React.
        */
        <details className="dag-legend" open={legendOpen} onToggle={(e) => setLegendOpen(e.currentTarget.open)}>
          <summary>{legendLabel}</summary>
          {legend}
        </details>
      )}
    </>
  );
}

export { clampZoom };
