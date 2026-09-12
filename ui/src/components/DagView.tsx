// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * DAG rendering.
 *
 * Hand-rolled SVG rather than a graph library: the layout requirement is a
 * layered left-to-right DAG, which buildDagGraph already computes, and a
 * dependency that owns the node rendering would make the state-treatment
 * requirement (every state distinct, three of them emphatically so) somebody
 * else's styling decision.
 *
 * Each node carries FOUR independent state channels: fill colour, border
 * colour, border dash pattern, and a glyph. A root-cause FAILED node also gets
 * an outer halo. That redundancy is what makes the view readable in greyscale,
 * at a glance, and to a colour-blind operator.
 *
 * Selection is deliberately NOT a fifth use of the border colour: it thickens a
 * separate inset ring, so "which node did I click" and "what state is it in"
 * never have to share one visual property.
 *
 * Edges are orthogonal rounded elbows (lib/dag edgePath) rather than curves,
 * because a fan-out of three siblings drawn as three beziers overlaps into one
 * blur and the operator loses the parent→child mapping that is the only reason
 * to draw edges at all.
 */

import { useMemo } from 'react';
import {
  NODE_H,
  NODE_W,
  edgePath,
  layoutLayers,
  type DagGraph,
  type DagNode,
  type Placement,
} from '../lib/dag';
import { STATE_TABLES, stateVisual } from '../lib/states';
import { StateBadge } from './StateBadge';

/**
 * Box geometry and placement come from lib/dag: the catalog's spec graph draws
 * the same grid, and the layout being shared is what keeps a workflow's
 * diagram in the catalog identical to its diagram in a run.
 */
interface Placed extends Placement {
  node: DagNode;
}

function layout(graph: DagGraph): { placed: Placed[]; width: number; height: number } {
  const { places, width, height } = layoutLayers(graph.layers);
  const placed: Placed[] = [];
  for (const p of places) {
    const node = graph.byId[p.id];
    if (node) placed.push({ ...p, node });
  }
  return { placed, width, height };
}

export function DagView({
  graph,
  selectedNodeId,
  onSelect,
}: {
  graph: DagGraph;
  selectedNodeId?: string | null;
  onSelect: (nodeId: string) => void;
}) {
  const { placed, width, height } = useMemo(() => layout(graph), [graph]);
  const byId = useMemo(() => {
    const m: Record<string, Placed> = {};
    for (const p of placed) m[p.node.node_id] = p;
    return m;
  }, [placed]);

  if (graph.nodes.length === 0) {
    return (
      <div className="notice empty" style={{ margin: 'var(--space-5)' }}>
        <span className="notice-glyph">∅</span>
        <div>
          <div className="notice-title">No nodes materialised for this run</div>
          <div className="notice-detail">
            The DAG engine expands a spec into task_nodes when the run is created. An empty graph on a
            PENDING run means that has not happened yet.
          </div>
        </div>
      </div>
    );
  }

  return (
    <>
      {graph.hasCycle && (
        <div className="notice error" style={{ margin: 'var(--space-5)' }} role="alert">
          <span className="notice-glyph">!</span>
          <div>
            <div className="notice-title">This graph contains a cycle</div>
            <div className="notice-detail">
              Acyclicity is validated at submit time, so this should be impossible. Layout depths below
              the cycle are unreliable. Report it — the spec or the node projection is wrong.
            </div>
          </div>
        </div>
      )}
      <div className="dag-viewport">
        <svg
          width={width}
          height={height}
          viewBox={`0 0 ${width} ${height}`}
          role="img"
          aria-label={`Run DAG, ${graph.nodes.length} nodes`}
        >
          {/*
            Arrowheads are drawn on their own rather than inherited from the
            path's stroke so the head keeps full opacity while the line itself
            can sit back at a lower one: the direction of flow stays legible
            without the edges competing with the nodes for attention.

            refX places the tip at the marker's leading edge so the head lands
            exactly on the target's border instead of overlapping into it.
          */}
          <defs>
            <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto">
              <path d="M 0 1 L 9 5 L 0 9 z" fill="var(--border-strong)" />
            </marker>
            <marker
              id="arrow-failed"
              viewBox="0 0 10 10"
              refX="9"
              refY="5"
              markerWidth="6"
              markerHeight="6"
              orient="auto"
            >
              <path d="M 0 1 L 9 5 L 0 9 z" fill="var(--tone-danger-bd)" />
            </marker>
          </defs>

          {graph.edges.map((e) => {
            const a = byId[e.from_node_id];
            const b = byId[e.to_node_id];
            if (!a || !b) return null;
            // An edge is highlighted when it is the path along which a failure
            // propagated, which is how a cascade reads as a cascade.
            const propagates =
              (a.node.state === 'FAILED' || a.node.state === 'UPSTREAM_FAILED') &&
              b.node.state === 'UPSTREAM_FAILED';
            return (
              <path
                key={`${e.from_node_id}->${e.to_node_id}`}
                className={`dag-edge${propagates ? ' failed-path' : ''}`}
                d={edgePath(a, b)}
                markerEnd={propagates ? 'url(#arrow-failed)' : 'url(#arrow)'}
              />
            );
          })}

          {placed.map(({ node, x, y }) => {
            const v = stateVisual('node', node.state);
            const selected = node.node_id === selectedNodeId;
            const attemptCount = node.attempts.length;
            return (
              <g
                key={node.node_id}
                transform={`translate(${x},${y})`}
                className={`dag-node-g tone-${v.tone}${selected ? ' selected' : ''}`}
                onClick={() => onSelect(node.node_id)}
                onKeyDown={(ev) => {
                  if (ev.key === 'Enter' || ev.key === ' ') {
                    ev.preventDefault();
                    onSelect(node.node_id);
                  }
                }}
                tabIndex={0}
                role="button"
                aria-label={`${node.node_id}, ${v.label}${
                  node.isRootCause ? ', root cause of the failure' : ''
                }`}
              >
                <title>
                  {`${node.node_id} — ${v.label}\n${v.hint}` +
                    (node.isRootCause ? '\nROOT CAUSE: the earliest failure on this path.' : '') +
                    (node.pendingReason ? `\n${node.pendingReason}` : '')}
                </title>
                {node.isRootCause && (
                  <rect
                    className="dag-rootcause-halo"
                    x={-6}
                    y={-6}
                    width={NODE_W + 12}
                    height={NODE_H + 12}
                    rx={16}
                  />
                )}
                {/*
                  The vertex is built from three stacked rects rather than one.
                  The shadow rect sits a pixel low to read as elevation, the ring
                  is a separate inset stroke so the selected state can thicken it
                  WITHOUT touching the state border (channel 2), and the body
                  carries fill (channel 1), border colour (channel 2) and border
                  dash (channel 3). Collapsing these into one rect is what would
                  force selection and state to share a stroke and lose a channel.
                */}
                <rect className="dag-node-shadow" y={2} width={NODE_W} height={NODE_H} rx={12} />
                <rect
                  className={`dag-node${selected ? ' selected' : ''}`}
                  width={NODE_W}
                  height={NODE_H}
                  rx={12}
                  strokeDasharray={dashFor(v.borderStyle)}
                />
                {/*
                  Channel 4, the glyph, now sits in its own tinted chip. A bare
                  glyph at 13px was the weakest of the four channels; a filled
                  square around it survives both a thumbnail and a greyscale
                  print, which is the whole point of the redundancy.
                */}
                <rect className="dag-glyph-chip" x={10} y={11} width={22} height={22} rx={7} />
                <text className="dag-glyph" x={21} y={26} textAnchor="middle" fill="var(--tone-bd)">
                  {v.glyph}
                </text>
                {/* The node id is the thing an operator searches for, so it is
                    the only element at full foreground weight and colour. */}
                <text className="dag-node-label" x={40} y={26}>
                  {truncate(node.node_id, 18)}
                </text>
                <text className="dag-node-sub" x={40} y={39}>
                  {truncate(node.task_ref, 24)}
                </text>
                {/* The state name is repeated as text under the glyph because a
                    colour plus a symbol still leaves an operator guessing which
                    of two symbols they are looking at. */}
                <text className="dag-node-state" x={11} y={55} fill="var(--tone-bd)">
                  {v.label}
                </text>
                {attemptCount > 1 && (
                  <text className="dag-node-sub" x={NODE_W - 11} y={55} textAnchor="end">
                    {attemptCount} attempts
                  </text>
                )}
                {node.pendingReason && attemptCount <= 1 && (
                  <text className="dag-node-sub" x={NODE_W - 11} y={55} textAnchor="end" fill="var(--tone-warning-fg)">
                    stalled
                  </text>
                )}
              </g>
            );
          })}
        </svg>
      </div>
      <DagLegend />
    </>
  );
}

/**
 * Double borders cannot be expressed on an SVG rect, so the SVG channel for
 * "double" is a wide dash — visually distinct from dashed and dotted, which is
 * all the requirement actually needs.
 */
function dashFor(style: string): string | undefined {
  switch (style) {
    case 'dashed':
      return '7 4';
    case 'dotted':
      return '2 3';
    case 'double':
      return '12 3 2 3';
    default:
      return undefined;
  }
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n - 1)}…`;
}

/**
 * The legend is not decoration: it is the mapping from the visual treatment to
 * the state names, which is what makes the distinctions usable by someone who
 * has not read the state machine.
 */
export function DagLegend() {
  const states = Object.keys(STATE_TABLES.node);
  return (
    <div className="legend">
      {states.map((s) => (
        <span className="legend-item" key={s}>
          <StateBadge kind="node" state={s} small />
          <span>{STATE_TABLES.node[s]?.hint.split('.')[0]}</span>
        </span>
      ))}
    </div>
  );
}
