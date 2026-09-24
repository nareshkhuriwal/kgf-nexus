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
 *
 * VIEWPORT. The nodes and edges below are drawn in the graph's intrinsic
 * coordinate space and handed to DagCanvas, which owns zoom, pan, fit-to-view,
 * export and share. That split is deliberate: this file decides what a node
 * LOOKS LIKE, which is where the run view and the catalog's spec view
 * legitimately differ, and DagCanvas decides what part of the layout is on
 * screen, which is identical for both and was previously broken in both — the
 * SVG was rendered at its intrinsic size, so a normal seven-layer run was
 * ~2048px wide in a ~1500px panel and its first and last nodes were off-screen
 * by default.
 */

import { useTaskLayout } from './useTaskLayout';
import { useMemo, useState } from 'react';
import {
  NODE_H,
  NODE_W,
  adjacencyOf,
  edgePath,
  layoutLayers,
  relatedSet,
  type DagGraph,
  type DagNode,
  type Placement,
} from '../lib/dag';
import { STATE_TABLES, stateVisual } from '../lib/states';
import { StateBadge } from './StateBadge';
import { DagCanvas } from './DagCanvas';
import { describeGroup, type ExpansionGroup } from '../lib/expansion';

/** The expansion summary groupExpansions attached, if this node is a template. */
function groupOf(node: DagNode): ExpansionGroup | undefined {
  return (node as DagNode & { group?: ExpansionGroup }).group;
}

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
  exportName,
  onToggleGroup,
}: {
  graph: DagGraph;
  /**
   * Fold/unfold a map template's children (§5.4). Absent means the view offers
   * no toggle -- the catalog and tests draw the graph as given.
   */
  onToggleGroup?: (templateId: string) => void;
  selectedNodeId?: string | null;
  onSelect: (nodeId: string) => void;
  /**
   * Identifies the run in an exported filename and keys the one-shot
   * fit-to-view.
   *
   * Threaded as a prop rather than read from the router inside this component
   * on purpose: the same view is drawn from the run page and is exercised
   * directly by tests, and reaching for useParams here would couple a pure
   * renderer to a route shape and force every test to mount a router to draw a
   * graph.
   */
  exportName?: string;
}) {
  const [focusTarget, setFocusTarget] = useState<{ x: number; y: number }>();
  const base = useMemo(() => layout(graph), [graph]);
  const taskLayout = useTaskLayout(base.placed, `run:${window.location.pathname}:${exportName ?? 'run'}`);
  const { places: placed, width, height, byId } = taskLayout;

  /**
   * The selected node's full ancestry and descendancy. Everything outside it is
   * dimmed, which turns "what does this node depend on" from a trace-the-lines
   * exercise into a glance.
   *
   * Empty when nothing is selected: dimming with no selection would mean the
   * default view of a graph is a dimmed one.
   */
  const related = useMemo(() => {
    if (!selectedNodeId || !graph.byId[selectedNodeId]) return null;
    return relatedSet(selectedNodeId, adjacencyOf(graph.nodes));
  }, [selectedNodeId, graph]);

  /**
   * Fit-to-view runs once per this string. Node count is in it because a run
   * whose DAG is still expanding legitimately changes SHAPE between polls, and
   * a graph that grew from 3 nodes to 30 is a new layout that deserves a fresh
   * fit; a poll that returns the same 30 nodes is not and must leave the
   * operator's zoom alone.
   */
  const graphKey = `${exportName ?? 'run'}:${graph.nodes.length}:${graph.edges.length}:${taskLayout.revision}`;

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
      <div className="dag-state-summary" aria-label="Task state summary">
        <span className="dag-flow-label">Execution flow <span aria-hidden="true">→</span></span>
        {Object.entries(graph.counts).filter(([, count]) => count > 0).map(([state, count]) =>
          <span className="dag-state-count" key={state}><StateBadge kind="node" state={state} small /><strong>{count}</strong></span>)}
      </div>
      <div className="dag-layout-note" role="status">{taskLayout.message}</div>
      <DagCanvas
        viewportHeight={base.height}
        focusTarget={focusTarget}
        toolbarExtra={<><button type="button" onClick={taskLayout.reset}>Reset layout</button><label className="dag-jump">Jump to task
          <select aria-label="Jump to task" value={selectedNodeId ?? ''} onChange={event => {
            const target = byId[event.target.value];
            if (!target) return;
            onSelect(event.target.value);
            setFocusTarget({ x: target.x + NODE_W / 2, y: target.y + NODE_H / 2 });
          }}>
            <option value="" disabled>Select a task…</option>
            {placed.map(({ node }) => <option key={node.node_id} value={node.node_id}>{node.node_id} · {stateVisual('node', node.state).label}</option>)}
          </select>
        </label></>}
        width={width}
        height={height}
        graphKey={graphKey}
        label={`Run DAG, ${graph.nodes.length} nodes`}
        exportName={exportName}
        stats={
          <span className="dag-stats">
            {/* The counts are here, next to the graph, because the panel header
                claiming "10 nodes" over a view showing 8 was the symptom that
                made the overflow bug invisible. With fit-to-view on by default
                these now agree, and stating the depth as well tells an operator
                how wide the thing they are scaled down to actually is. */}
            <span>
              {graph.nodes.length} node{graph.nodes.length === 1 ? '' : 's'}
            </span>
            <span>
              {layerCount(graph)} layer{layerCount(graph) === 1 ? '' : 's'} deep
            </span>
            {related && <span>{related.size} on this path</span>}
          </span>
        }
        legend={<DagLegend />}
        legendLabel="what the states look like"
        defs={
          <>
            {/*
              Arrowheads are drawn on their own rather than inherited from the
              path's stroke so the head keeps full opacity while the line itself
              can sit back at a lower one: the direction of flow stays legible
              without the edges competing with the nodes for attention.

              refX places the tip at the marker's leading edge so the head lands
              exactly on the target's border instead of overlapping into it.
            */}
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
          </>
        }
      >
        {graph.edges.map((e) => {
          const a = byId[e.from_node_id];
          const b = byId[e.to_node_id];
          if (!a || !b) return null;
          // An edge is highlighted when it is the path along which a failure
          // propagated, which is how a cascade reads as a cascade.
          const propagates =
            (a.node.state === 'FAILED' || a.node.state === 'UPSTREAM_FAILED') &&
            b.node.state === 'UPSTREAM_FAILED';
          // Relatedness is a separate, additive class from the failed-path
          // treatment: a cascade edge that is also on the selected ancestry
          // must keep reading as a cascade, so dimming never replaces it.
          const onPath = related ? related.has(e.from_node_id) && related.has(e.to_node_id) : null;
          return (
            <path
              key={`${e.from_node_id}->${e.to_node_id}`}
              className={
                `dag-edge${propagates ? ' failed-path' : ''}` +
                (onPath === null ? '' : onPath ? ' related' : ' unrelated')
              }
              d={edgePath(a, b)}
              markerEnd={propagates ? 'url(#arrow-failed)' : 'url(#arrow)'}
            />
          );
        })}

        {placed.map(({ node, x, y }) => {
          const v = stateVisual('node', node.state);
          const selected = node.node_id === selectedNodeId;
          const attemptCount = node.attempts.length;
          const unrelated = related !== null && !related.has(node.node_id);
          const group = groupOf(node);
          const cached = !!node.latestAttempt?.cache_hit_of;
          return (
            <g
              {...taskLayout.bindings(node.node_id)}
              key={node.node_id}
              transform={`translate(${x},${y})`}
              className={`dag-node-g tone-${v.tone}${selected ? ' selected' : ''}${
                unrelated ? ' unrelated' : ''
              }`}
              onClick={() => onSelect(node.node_id)}
              onKeyDown={(ev) => {
                if (ev.key === 'Enter' || ev.key === ' ') {
                  ev.preventDefault();
                  onSelect(node.node_id);
                }
              }}
              aria-pressed={selected}
              tabIndex={0}
              role="button"
              aria-label={`${node.node_id}, ${v.label}${
                node.isRootCause ? ', root cause of the failure' : ''
              }`}
            >
              <title>
                {`${node.node_id} — ${v.label}\n${v.hint}` +
                  (node.isRootCause ? '\nROOT CAUSE: the earliest failure on this path.' : '') +
                  (node.pendingReason ? `\n${node.pendingReason}` : '') +
                  (node.failure_reason ? `\n${node.failure_reason}` : '') +
                  (group ? `\nMAP TEMPLATE: ${describeGroup(group)}` : '') +
                  (cached ? `\nCACHED: reused the result of attempt ${node.latestAttempt?.cache_hit_of}` : '')}
              </title>
              {group && (
                // A stacked second outline reads as "several of these" before any
                // text is read, which is the point of a folded fan-out.
                <rect className="dag-node-shadow dag-group-stack" x={6} y={-6} width={NODE_W} height={NODE_H} rx={12} />
              )}
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
              <line className="dag-card-divider" x1={14} x2={NODE_W - 14} y1={57} y2={57} />
              {/*
                Channel 4, the glyph, now sits in its own tinted chip. A bare
                glyph at 13px was the weakest of the four channels; a filled
                square around it survives both a thumbnail and a greyscale
                print, which is the whole point of the redundancy.
              */}
              <rect className="dag-glyph-chip" x={14} y={15} width={26} height={26} rx={7} />
              <text className="dag-glyph" x={27} y={33} textAnchor="middle" fill="var(--tone-bd)">
                {v.glyph}
              </text>
              {/* The node id is the thing an operator searches for, so it is
                  the only element at full foreground weight and colour. */}
              <text className="dag-node-label" x={50} y={30}>
                {truncate(node.node_id, 23)}
              </text>
              <text className="dag-node-sub" x={50} y={47}>
                {truncate(node.task_ref, 27)}
              </text>
              {/* The state name is repeated as text under the glyph because a
                  colour plus a symbol still leaves an operator guessing which
                  of two symbols they are looking at. */}
              <text className="dag-node-state" x={14} y={73} fill="var(--tone-bd)">
                {v.label}
              </text>
              {group && (
                <text className="dag-node-sub dag-group-summary" x={NODE_W - 14} y={73} textAnchor="end">
                  {truncate(describeGroup(group), 26)}
                </text>
              )}
              {/* Which worker has (or had) this task. The same slot a group
                  uses for its summary, because a folded group has no single
                  worker to name. Hostname when the gateway joined it, else the
                  worker id shortened -- a machine name is what an operator
                  matches against the fleet page. */}
              {!group && workerLabel(node) && (
                <text className="dag-node-sub dag-node-worker" x={NODE_W - 14} y={73} textAnchor="end">
                  <title>{workerTitle(node)}</title>
                  {truncate(workerLabel(node) ?? '', 22)}
                </text>
              )}
              {group && onToggleGroup && group.unfoldable && (
                <text
                  className="dag-node-sub dag-group-toggle"
                  x={NODE_W - 14}
                  y={30}
                  textAnchor="end"
                  role="button"
                  tabIndex={0}
                  aria-label={`${group.unfolded ? 'Fold' : 'Unfold'} the ${group.total} items of ${node.node_id}`}
                  onClick={(ev) => {
                    ev.stopPropagation();
                    onToggleGroup(node.node_id);
                  }}
                  onKeyDown={(ev) => {
                    if (ev.key === 'Enter' || ev.key === ' ') {
                      ev.preventDefault();
                      ev.stopPropagation();
                      onToggleGroup(node.node_id);
                    }
                  }}
                >
                  {group.unfolded ? '− fold' : '+ unfold'}
                </text>
              )}
              {!group && node.failure_reason && (
                <text className="dag-node-sub" x={NODE_W - 14} y={73} textAnchor="end" fill="var(--tone-danger-fg)">
                  {node.failure_reason.startsWith('fan_in_too_large') ? 'fan-in too large' : 'not run'}
                </text>
              )}
              {!group && cached && (
                <text className="dag-node-sub dag-cached" x={NODE_W - 14} y={73} textAnchor="end">
                  cached
                </text>
              )}
              {!group && !cached && attemptCount > 1 && (
                <text className="dag-node-sub" x={NODE_W - 14} y={73} textAnchor="end">
                  {attemptCount} attempts
                </text>
              )}
              {!group && node.pendingReason && attemptCount <= 1 && (
                <text className="dag-node-sub" x={NODE_W - 14} y={73} textAnchor="end" fill="var(--tone-warning-fg)">
                  stalled
                </text>
              )}
            </g>
          );
        })}
      </DagCanvas>
    </>
  );
}

/** Layers actually occupied. An edgeless graph is laid out as a grid rather
 *  than as one column (lib/dag layoutGrid), so its honest depth is 1. */
function layerCount(graph: DagGraph): number {
  return graph.layers.filter((l) => l.length > 0).length;
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

/**
 * The worker the node's latest attempt is placed on, as a name for the card.
 * Null for a node that has not been dispatched, so the card stays quiet
 * rather than showing a dash where a hostname would go.
 */
function workerLabel(node: DagNode): string | null {
  const a = node.latestAttempt;
  if (!a || !a.worker_id) return null;
  return a.worker_hostname ?? a.worker_id.slice(0, 12);
}

function workerTitle(node: DagNode): string {
  const a = node.latestAttempt;
  if (!a || !a.worker_id) return '';
  return `${a.worker_hostname ? a.worker_hostname + ' · ' : ''}worker ${a.worker_id}`;
}
