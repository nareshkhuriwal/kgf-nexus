// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Pure viewport maths for the DAG canvas: zoom clamping, fit-to-view, and SVG
 * serialisation for export.
 *
 * A SEPARATE module from lib/dag on purpose. lib/dag answers "where does this
 * node go", which is a property of the graph and is property-tested as such;
 * this module answers "what part of that layout is the operator currently
 * looking at", which is a property of a rendered viewport. Folding the second
 * into the first would put a container width — a DOM measurement — into the
 * module whose whole value is that it has neither React nor the DOM in it.
 *
 * Kept pure for the same reason lib/dag is: "the view snapped back to fit while
 * I was inspecting a node" and "the exported file came out black and white" are
 * both bugs about arithmetic and string building, and both are far cheaper to
 * pin here than through a jsdom render.
 */

/**
 * Zoom limits.
 *
 * The floor is not 0: below roughly a third of scale a 196x64 node box is
 * narrower than its own truncated label, so the graph stops being a graph and
 * becomes a smudge that still costs a render. The ceiling is 3x because past
 * that a single node fills the panel and the operator has lost the structure
 * that is the only reason to draw a DAG rather than list the nodes.
 *
 * Fit-to-view is allowed to go BELOW the floor (see fitScale): a 20-layer DAG
 * legitimately needs 0.2 to fit, and refusing to fit it would reinstate exactly
 * the overflow bug this exists to solve. The floor constrains the buttons, which
 * are a human deciding to zoom out, not the arithmetic answering "what fits".
 */
export const ZOOM_MIN = 0.3;
export const ZOOM_MAX = 3;
/** Multiplicative, not additive: a fixed +0.1 step is a 33% jump at 0.3 scale
 *  and a 3% nudge at 3, so the button would feel like two different controls at
 *  the two ends of its own range. */
export const ZOOM_STEP = 1.25;

/**
 * NaN and the infinities are different inputs and get different answers.
 *
 * NaN carries no direction — it is the result of 0/0 or a parse that failed — so
 * the only safe reading is "no meaningful value", and 100% is the neutral view.
 * The infinities DO carry a direction: they arrive from a division that
 * underflowed or a gesture that multiplied too far, and the operator's intent
 * was unmistakably "more" or "less". Sending those to 1 would snap the view back
 * to 100% at the exact moment someone zoomed hard, which reads as the control
 * being broken; Math.min/max already resolve them to the right end of the range,
 * so they simply must not be intercepted first.
 */
export function clampZoom(z: number): number {
  if (Number.isNaN(z)) return 1;
  return Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, z));
}

export function zoomIn(z: number): number {
  return clampZoom(z * ZOOM_STEP);
}

export function zoomOut(z: number): number {
  return clampZoom(z / ZOOM_STEP);
}

/** The readout an operator reads to know whether they are looking at a scaled
 *  view. Rounded to a whole percent: 87.4999% is noise, not information. */
export function zoomLabel(z: number): string {
  return `${Math.round(z * 100)}%`;
}

/**
 * The scale at which a graph of `graph` dimensions fits inside `view`.
 *
 * Capped at 1 so fitting never MAGNIFIES: a two-node graph blown up to fill a
 * 1500px panel looks like a mistake, and worse, it means the default view of a
 * small run and of a large one are at different scales with nothing on screen
 * explaining why. Fit means "make it all visible", not "fill the box".
 *
 * Not clamped at ZOOM_MIN: see the note there. A graph that needs 0.18 to fit
 * gets 0.18, because the alternative is the original bug — the first and last
 * node off-screen with no indication they exist.
 */
export function fitScale(
  graph: { width: number; height: number },
  view: { width: number; height: number },
): number {
  if (graph.width <= 0 || graph.height <= 0) return 1;
  // A container that has not been measured yet (both zero, pre-layout, or
  // jsdom) must not produce a 0 scale and a blank canvas. 1 is the honest
  // "I do not know yet" answer and it is also the answer that shows content.
  if (view.width <= 0 || view.height <= 0) return 1;
  return Math.min(1, view.width / graph.width, view.height / graph.height);
}

/**
 * True when the graph genuinely does not fit, with a tolerance.
 *
 * The tolerance matters: a graph 3px wider than its container does not need the
 * default view scaled down, and without a slack term sub-pixel container
 * measurements would make "does it fit" flap between renders and re-trigger the
 * auto-fit. 4px is under a node's stroke width, so nothing readable is lost.
 */
export function overflows(
  graph: { width: number; height: number },
  view: { width: number; height: number },
  tolerance = 4,
): boolean {
  if (view.width <= 0 || view.height <= 0) return false;
  return graph.width > view.width + tolerance || graph.height > view.height + tolerance;
}

/**
 * The pan offset that centres a graph drawn at `scale` inside `view`.
 *
 * Centring rather than pinning top-left because a fitted graph usually has
 * slack on one axis (the aspect ratios rarely match), and a graph hugging the
 * top-left corner of a wide panel reads as "the layout is broken" rather than
 * as "this is the whole graph". Negative offsets are allowed: a graph larger
 * than the view on one axis is centred on that axis too, which keeps the
 * middle of a wide DAG in the middle of the panel.
 */
export function centerOffset(
  graph: { width: number; height: number },
  view: { width: number; height: number },
  scale: number,
): { x: number; y: number } {
  return {
    x: (view.width - graph.width * scale) / 2,
    y: (view.height - graph.height * scale) / 2,
  };
}

/**
 * Zoom about a fixed point in CLIENT space, returning the pan that keeps that
 * point under the cursor.
 *
 * Without this, Ctrl+wheel zoom scales about the group's origin, so the content
 * under the pointer slides away as the operator zooms toward it — the specific
 * interaction that makes an embedded canvas feel broken. Solving for "the graph
 * coordinate under the pointer is unchanged" is three lines of algebra and is
 * the difference between zoom that aims and zoom that guesses.
 */
export function zoomAbout(
  current: { scale: number; x: number; y: number },
  nextScale: number,
  pointer: { x: number; y: number },
): { scale: number; x: number; y: number } {
  const scale = clampZoom(nextScale);
  // Graph-space coordinate currently under the pointer.
  const gx = (pointer.x - current.x) / current.scale;
  const gy = (pointer.y - current.y) / current.scale;
  return { scale, x: pointer.x - gx * scale, y: pointer.y - gy * scale };
}

/**
 * A filesystem-safe basename for an export.
 *
 * Run ids are UUIDs and spec names are user-supplied, so they can carry
 * slashes, colons and spaces — all of which either break a download on some
 * platform or silently turn into a differently-named file. Normalising here
 * rather than at each call site means the SVG and the PNG of the same graph can
 * never disagree about their own name.
 */
export function exportFileName(name: string | undefined, ext: 'svg' | 'png'): string {
  const slug = (name ?? 'graph')
    .trim()
    .replace(/[^A-Za-z0-9._-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 80);
  return `nexus-${slug || 'graph'}-dag.${ext}`;
}

/**
 * CSS properties that actually carry the appearance of an SVG node or edge.
 *
 * Enumerated rather than copying the whole computed style: getComputedStyle
 * returns several hundred declarations per element, and serialising all of them
 * for a 200-node graph produces a multi-megabyte file that some viewers refuse
 * to open. These are the properties the DAG's own stylesheet actually sets.
 */
const EXPORT_PROPS = [
  'fill',
  'fill-opacity',
  'stroke',
  'stroke-width',
  'stroke-dasharray',
  'stroke-linecap',
  'stroke-linejoin',
  'opacity',
  'font',
  'font-family',
  'font-size',
  'font-weight',
  'letter-spacing',
  'text-anchor',
] as const;

/** Minimal shape of what inlineComputedStyles needs, so the function can be
 *  tested against a stub and does not require a real SVGElement. */
interface StyleReader {
  (el: Element): { getPropertyValue(prop: string): string };
}

/**
 * Rewrites every element's styling as inline attributes on a DETACHED clone.
 *
 * THIS IS THE CORRECTNESS CRUX OF THE EXPORT. The live graph gets all of its
 * colour from dag.css and from the --tone-* custom properties resolved against
 * the theme on <html>. A naive outerHTML serialise carries neither: the class
 * names survive, the stylesheet does not, every var() resolves to nothing, and
 * the operator opens a file in which a FAILED node and a SUCCEEDED node are the
 * same default-black rectangle. That is worse than no export, because it looks
 * like an export that worked.
 *
 * Inlining COMPUTED values rather than embedding the stylesheet text is the
 * choice that survives the theme and the custom properties: getComputedStyle
 * has already resolved `fill: var(--tone-bg)` to an actual rgb() in whichever
 * theme is active, so the exported file looks like what was on screen instead
 * of like whatever the CSS cascade would produce in a viewer that has no
 * <html data-theme> to resolve against.
 *
 * Walks the live tree and the clone in lockstep — the clone has no layout and
 * no stylesheet, so it cannot be asked for its own computed style.
 */
export function inlineComputedStyles(live: Element, clone: Element, read: StyleReader): void {
  const liveKids = live.children;
  const cloneKids = clone.children;
  const style = read(live);
  const declarations: string[] = [];
  for (const prop of EXPORT_PROPS) {
    const value = style.getPropertyValue(prop);
    // `none` is meaningful on fill and stroke (an edge has fill:none), so it is
    // kept; an empty string is the absent case and would serialise as garbage.
    if (value && value !== 'normal') declarations.push(`${prop}:${value}`);
  }
  if (declarations.length > 0) clone.setAttribute('style', declarations.join(';'));

  for (let i = 0; i < liveKids.length && i < cloneKids.length; i += 1) {
    inlineComputedStyles(liveKids[i]!, cloneKids[i]!, read);
  }
}

/**
 * Wraps a serialised SVG fragment into a standalone document.
 *
 * The xmlns is not optional: a file without it is XML a browser will render as
 * a tree of unknown elements rather than as a picture, and the failure appears
 * only when the file is opened outside the app that produced it — i.e. always,
 * since the point of an export is to send it to someone. The explicit
 * background rect is here because an SVG has no canvas colour of its own, so a
 * dark-theme graph exported without one arrives as light-grey text on the
 * viewer's white default and is unreadable.
 */
export function svgDocument(
  inner: string,
  opts: { width: number; height: number; background: string; viewBox: string },
): string {
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" ` +
    `width="${opts.width}" height="${opts.height}" viewBox="${opts.viewBox}">` +
    `<rect x="0" y="0" width="100%" height="100%" fill="${opts.background}"/>` +
    inner +
    `</svg>`
  );
}
