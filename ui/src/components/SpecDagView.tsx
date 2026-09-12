// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The dependency graph of a spec DEFINITION.
 *
 * Deliberately a sibling of DagView rather than a mode of it. The two share
 * everything structural — layering, ordering, box geometry, edge curves, all of
 * it imported from lib/dag — and share nothing semantic, because DagView's node
 * body is four channels of STATE (fill, border colour, dash, glyph) and a spec
 * has no state at all. Folding this into DagView would mean every one of those
 * channels growing a "no run" branch, which is more code in the screen an
 * operator reads during an incident, to serve a screen they read while nothing
 * is wrong.
 *
 * What a task box shows instead is what a definition actually knows: the task
 * ref it dispatches, the queue it lands on, and its resource request — the
 * three things that decide whether it can be scheduled at all.
 */

import { useMemo } from 'react';
import { NODE_H, NODE_W, edgePath, layoutLayers } from '../lib/dag';
import type { SpecGraph, SpecGraphNode } from '../lib/specgraph';
import { fmtResources } from '../lib/format';
import { Empty } from './Status';

export function SpecDagView({
  graph,
  selectedTaskId,
  onSelect,
}: {
  graph: SpecGraph;
  selectedTaskId?: string | null;
  onSelect: (taskId: string) => void;
}) {
  const { places, width, height, byId } = useMemo(() => layoutLayers(graph.layers), [graph]);

  if (graph.nodes.length === 0) {
    return (
      <Empty
        title="This definition declares no tasks"
        detail="A spec with no tasks cannot produce a run. Check what the SDK or CLI submitted."
      />
    );
  }

  // Highlighting the selected task's immediate neighbours is the catalog's
  // equivalent of the run view's cascade path: the question here is "what does
  // this depend on and what depends on it", which is unreadable in a wide fan.
  const selected = selectedTaskId ? graph.byId[selectedTaskId] : undefined;
  const related = new Set<string>(
    selected ? [selected.id, ...selected.upstream, ...selected.downstream] : [],
  );

  return (
    <>
      {graph.hasCycle && (
        <div className="notice error" style={{ margin: 'var(--space-5)' }} role="alert">
          <span className="notice-glyph">!</span>
          <div>
            <div className="notice-title">This definition contains a cycle</div>
            <div className="notice-detail">
              Acyclicity is validated when a spec is submitted, so a stored spec that cycles means
              the validation was bypassed or the stored document is corrupt. It can never produce a
              run. Layout depths below the cycle are unreliable.
            </div>
          </div>
        </div>
      )}
      {graph.danglingEdges.length > 0 && (
        <div className="notice error" style={{ margin: 'var(--space-5)' }} role="alert">
          <span className="notice-glyph">!</span>
          <div>
            <div className="notice-title">
              {graph.danglingEdges.length} edge
              {graph.danglingEdges.length === 1 ? '' : 's'} name a task this spec does not declare
            </div>
            <div className="notice-detail mono">
              {graph.danglingEdges
                .map((e) => `${e.from_node_id} → ${e.to_node_id}`)
                .join(', ')}
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
          aria-label={`Workflow dependency graph, ${graph.nodes.length} tasks`}
        >
          <defs>
            <marker
              id="spec-arrow"
              viewBox="0 0 8 8"
              refX="7"
              refY="4"
              markerWidth="7"
              markerHeight="7"
              orient="auto"
            >
              <path d="M 0 0 L 8 4 L 0 8 z" fill="var(--border-strong)" />
            </marker>
            <marker
              id="spec-arrow-related"
              viewBox="0 0 8 8"
              refX="7"
              refY="4"
              markerWidth="7"
              markerHeight="7"
              orient="auto"
            >
              <path d="M 0 0 L 8 4 L 0 8 z" fill="var(--tone-active-bd)" />
            </marker>
          </defs>

          {graph.edges.map((e) => {
            const a = byId[e.from_node_id];
            const b = byId[e.to_node_id];
            if (!a || !b) return null;
            const onPath =
              selected !== undefined &&
              (e.from_node_id === selected.id || e.to_node_id === selected.id);
            return (
              <path
                key={`${e.from_node_id}->${e.to_node_id}`}
                className={`dag-edge${onPath ? ' spec-related' : ''}`}
                d={edgePath(a, b)}
                markerEnd={onPath ? 'url(#spec-arrow-related)' : 'url(#spec-arrow)'}
              >
                {/* The mapping is the contract between two tasks: which output
                    of the parent becomes which input of the child. It is the
                    one thing an edge carries that a line cannot show. */}
                <title>{describeEdge(e)}</title>
              </path>
            );
          })}

          {places.map((p) => {
            const node = graph.byId[p.id];
            if (!node) return null;
            return (
              <SpecTaskBox
                key={p.id}
                node={node}
                x={p.x}
                y={p.y}
                selected={p.id === selectedTaskId}
                dimmed={related.size > 0 && !related.has(p.id)}
                onSelect={onSelect}
              />
            );
          })}
        </svg>
      </div>
      <SpecLegend />
    </>
  );
}

function SpecTaskBox({
  node,
  x,
  y,
  selected,
  dimmed,
  onSelect,
}: {
  node: SpecGraphNode;
  x: number;
  y: number;
  selected: boolean;
  dimmed: boolean;
  onSelect: (taskId: string) => void;
}) {
  const resources = fmtResources(node.task.resources?.request);
  // Entry and exit points are the two structural facts worth seeing without
  // clicking, so they get the only non-selection treatment on the box.
  const role = node.isRoot ? 'entry point' : node.isLeaf ? 'exit point' : '';

  return (
    <g
      transform={`translate(${x},${y})`}
      className={`spec-node-g${dimmed ? ' dimmed' : ''}`}
      onClick={() => onSelect(node.id)}
      onKeyDown={(ev) => {
        if (ev.key === 'Enter' || ev.key === ' ') {
          ev.preventDefault();
          onSelect(node.id);
        }
      }}
      tabIndex={0}
      role="button"
      aria-label={`${node.id}, ${node.task.task_ref ?? 'no task ref'}${role ? `, ${role}` : ''}`}
    >
      <title>
        {`${node.id}\n${node.task.task_ref ?? 'no task_ref'}` +
          `\nqueue: ${node.task.queue ?? 'unset'}` +
          `\ntrigger: ${node.task.trigger_rule ?? 'unset'}` +
          `\nrequests: ${resources}` +
          `\ndepends on: ${node.upstream.length > 0 ? node.upstream.join(', ') : 'nothing'}`}
      </title>
      <rect
        className={`spec-node${selected ? ' selected' : ''}${node.isRoot ? ' root' : ''}`}
        width={NODE_W}
        height={NODE_H}
        rx={6}
      />
      <text className="dag-node-label" x={11} y={20}>
        {truncate(node.id, 22)}
      </text>
      <text className="dag-node-sub" x={11} y={35}>
        {truncate(node.task.task_ref ?? '—', 28)}
      </text>
      <text className="dag-node-sub" x={11} y={49}>
        {truncate(resources, 20)}
      </text>
      {node.task.queue && (
        <text className="dag-node-sub" x={NODE_W - 11} y={49} textAnchor="end" fill="var(--tone-queued-fg)">
          {truncate(node.task.queue, 10)}
        </text>
      )}
    </g>
  );
}

/** Human form of an edge's output→input mapping, for the hover title. */
export function describeEdge(e: { from_node_id: string; to_node_id: string; output_to_input?: Record<string, string> }): string {
  const pairs = Object.entries(e.output_to_input ?? {});
  const head = `${e.from_node_id} → ${e.to_node_id}`;
  if (pairs.length === 0) return `${head}\nOrdering only: no data is passed.`;
  return `${head}\n${pairs.map(([input, output]) => `${e.from_node_id}.${output} → ${input}`).join('\n')}`;
}

/**
 * States the one thing a reader could otherwise get wrong: this diagram is the
 * declared shape, not a run. Nothing on it has executed.
 */
function SpecLegend() {
  return (
    <div className="legend">
      <span className="legend-item">
        <span className="spec-swatch root" aria-hidden="true" /> entry point — no dependencies
      </span>
      <span className="legend-item">
        <span className="spec-swatch" aria-hidden="true" /> task — click to inspect, neighbours highlight
      </span>
      <span className="legend-item">
        This is the definition. No task here has run — states appear on a run&apos;s DAG.
      </span>
    </div>
  );
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n - 1)}…`;
}
