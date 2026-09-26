// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Declared asset lineage: which tasks produce an asset, and which consume it.
 *
 * Built from the same parts as SpecDagView — lib/dag's layering and layout,
 * DagCanvas's viewport, useTaskLayout's drag — so a lineage graph pans, zooms,
 * fits and exports exactly like a workflow graph. What differs is the node
 * vocabulary. The graph is bipartite (task → asset for "produces", asset → task
 * for "consumes"), and the two kinds must read apart without a legend: an asset
 * is a tinted pill with a cylinder glyph, a task a dashed card. The asset the
 * page is about carries the selection ring.
 *
 * This is DECLARED lineage: what a task's spec says it reads and writes, not
 * what it touched. The legend says so.
 */

import { useMemo, type SVGProps } from 'react';
import type { AssetGraph, AssetGraphNode } from '../api/types';
import { NODE_H, NODE_W, assignLayers, edgePath, layoutLayers } from '../lib/dag';
import { DagCanvas } from './DagCanvas';
import { Empty } from './Status';
import { useTaskLayout } from './useTaskLayout';

export interface LineageLayout {
  nodes: AssetGraphNode[];
  edges: { from: string; to: string }[];
  byId: Record<string, AssetGraphNode>;
  layers: string[][];
  hasCycle: boolean;
}

/**
 * Normalises the wire graph: drops edges that name an unknown node or repeat,
 * and layers the rest. Exported for tests.
 */
export function buildLineage(graph: AssetGraph): LineageLayout {
  const byId: Record<string, AssetGraphNode> = {};
  for (const n of graph.nodes) byId[n.id] = n;
  const seen = new Set<string>();
  const edges: { from: string; to: string }[] = [];
  const upstream: Record<string, string[]> = {};
  const downstream: Record<string, string[]> = {};
  for (const n of graph.nodes) {
    upstream[n.id] = [];
    downstream[n.id] = [];
  }
  for (const e of graph.edges) {
    const key = `${e.from}\u0000${e.to}`;
    if (!byId[e.from] || !byId[e.to] || e.from === e.to || seen.has(key)) continue;
    seen.add(key);
    edges.push(e);
    downstream[e.from]!.push(e.to);
    upstream[e.to]!.push(e.from);
  }
  const { layers, hasCycle } = assignLayers(
    graph.nodes.map((n) => n.id),
    upstream,
    downstream,
  );
  return { nodes: graph.nodes, edges, byId, layers, hasCycle };
}

export function AssetLineageGraph({
  graph,
  focusId,
  onSelectAsset,
  onSelectTask,
  exportName,
}: {
  graph: AssetGraph;
  /** The node id of the asset this view is about (`asset:<uri>`). */
  focusId?: string;
  onSelectAsset: (uri: string) => void;
  /** Absent when there is nowhere for a task to go. */
  onSelectTask?: (node: AssetGraphNode) => void;
  exportName?: string;
}) {
  const lineage = useMemo(() => buildLineage(graph), [graph]);
  const base = useMemo(() => layoutLayers(lineage.layers), [lineage]);
  const layout = useTaskLayout(base.places, `lineage:${exportName ?? 'assets'}`);
  const { places, width, height, byId } = layout;

  if (lineage.nodes.length === 0) {
    return <Empty title="No lineage recorded" detail="No task declares that it produces or consumes this asset yet." />;
  }

  const assets = lineage.nodes.filter((n) => n.kind === 'asset').length;
  const tasks = lineage.nodes.length - assets;

  return (
    <>
      {lineage.hasCycle && (
        <div className="notice" role="status" style={{ margin: 'var(--space-5)' }}>
          <span className="notice-glyph" aria-hidden="true">
            ↻
          </span>
          <div className="notice-detail">
            This lineage contains a loop (a task reads an asset that it, or something downstream of it, also writes). The
            left-to-right order below is approximate along the loop.
          </div>
        </div>
      )}
      {graph.truncated && (
        <div className="notice error" role="alert" style={{ margin: 'var(--space-5)' }}>
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div className="notice-detail">
            The server capped the lineage it read, so this graph may be missing edges. Narrow it with a smaller depth.
          </div>
        </div>
      )}
      <div className="dag-layout-note" role="status">
        {layout.message}
      </div>
      <DagCanvas
        places={places}
        viewportHeight={base.height}
        toolbarExtra={
          <button type="button" onClick={layout.reset}>
            Reset layout
          </button>
        }
        width={width}
        height={height}
        graphKey={`${exportName ?? 'lineage'}:${lineage.nodes.length}:${lineage.edges.length}:${layout.revision}`}
        label={`Asset lineage graph, ${assets} asset${assets === 1 ? '' : 's'} and ${tasks} task${tasks === 1 ? '' : 's'}`}
        exportName={exportName}
        stats={
          <span className="dag-stats">
            <span>
              {assets} asset{assets === 1 ? '' : 's'}
            </span>
            <span>
              {tasks} task{tasks === 1 ? '' : 's'}
            </span>
            <span>
              {lineage.edges.length} edge{lineage.edges.length === 1 ? '' : 's'}
            </span>
          </span>
        }
        legend={<LineageLegend canOpenTasks={!!onSelectTask} />}
        legendLabel="about this diagram"
        defs={
          <marker id="lineage-arrow" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="5" markerHeight="5" orient="auto">
            <path d="M 0 0.5 L 7.5 4 L 0 7.5 L 2 4 z" fill="var(--border-strong)" />
          </marker>
        }
      >
        {lineage.edges.map((e) => {
          const a = byId[e.from];
          const b = byId[e.to];
          if (!a || !b) return null;
          const fromNode = lineage.byId[e.from]!;
          return (
            <path key={`${e.from}->${e.to}`} className="dag-edge" d={edgePath(a, b)} markerEnd="url(#lineage-arrow)">
              <title>
                {fromNode.kind === 'task'
                  ? `${fromNode.label} produces ${lineage.byId[e.to]!.label}`
                  : `${lineage.byId[e.to]!.label} consumes ${fromNode.label}`}
              </title>
            </path>
          );
        })}
        {places.map((p) => {
          const node = lineage.byId[p.id];
          if (!node) return null;
          return (
            <LineageNode
              key={p.id}
              node={node}
              x={p.x}
              y={p.y}
              focus={p.id === focusId}
              dragBindings={layout.bindings(p.id)}
              onActivate={
                node.kind === 'asset'
                  ? () => node.uri && onSelectAsset(node.uri)
                  : onSelectTask
                    ? () => onSelectTask(node)
                    : undefined
              }
            />
          );
        })}
      </DagCanvas>
    </>
  );
}

function LineageNode({
  node,
  x,
  y,
  focus,
  dragBindings,
  onActivate,
}: {
  node: AssetGraphNode;
  x: number;
  y: number;
  focus: boolean;
  dragBindings: SVGProps<SVGGElement>;
  onActivate?: () => void;
}) {
  const isAsset = node.kind === 'asset';
  const title = isAsset
    ? `Asset ${node.label}${node.uri && node.uri !== node.label ? `\n${node.uri}` : ''}`
    : `Task ${node.node_id ?? node.label} in workflow ${node.workflow ?? '?'}`;
  const aria = isAsset
    ? `Asset ${node.label}${focus ? ', the asset shown on this page' : ', open this asset'}`
    : `Task ${node.node_id ?? node.label} in workflow ${node.workflow ?? '?'}${onActivate ? ', open the workflow' : ''}`;
  const interactive = !!onActivate;
  return (
    <g
      {...dragBindings}
      transform={`translate(${x},${y})`}
      className="lineage-node-g spec-node-g"
      data-kind={node.kind}
      onClick={onActivate}
      onKeyDown={(ev) => {
        if (onActivate && (ev.key === 'Enter' || ev.key === ' ')) {
          ev.preventDefault();
          onActivate();
        }
      }}
      tabIndex={0}
      role={interactive ? 'button' : 'img'}
      aria-label={aria}
      aria-current={focus ? 'true' : undefined}
    >
      <title>{title}</title>
      <rect className="dag-node-shadow" y={3} width={NODE_W} height={NODE_H} rx={isAsset ? NODE_H / 2 : 14} />
      {focus && <rect className="dag-select-ring" x={-5} y={-5} width={NODE_W + 10} height={NODE_H + 10} rx={isAsset ? NODE_H / 2 + 5 : 18} />}
      {isAsset ? (
        <>
          <rect className={`lineage-asset${focus ? ' focus' : ''}`} width={NODE_W} height={NODE_H} rx={NODE_H / 2} />
          <g transform="translate(22,28)" className="lineage-glyph">
            <ellipse cx={11} cy={5} rx={11} ry={4} />
            <path d="M0 5v22c0 2.2 4.9 4 11 4s11-1.8 11-4V5" />
            <path d="M0 16c0 2.2 4.9 4 11 4s11-1.8 11-4" />
          </g>
          <text className="dag-node-label" x={58} y={38}>
            {truncate(node.label, 22)}
          </text>
          <text className="dag-node-sub" x={58} y={56}>
            {truncate(node.uri ?? 'asset', 26)}
          </text>
        </>
      ) : (
        <>
          <rect className="lineage-task" width={NODE_W} height={NODE_H} rx={14} />
          <text className="dag-node-sub" x={16} y={25}>
            task
          </text>
          <text className="dag-node-label" x={16} y={46}>
            {truncate(node.node_id ?? node.label, 26)}
          </text>
          <line className="dag-card-divider" x1={12} x2={NODE_W - 12} y1={57} y2={57} />
          <text className="dag-node-sub" x={16} y={75}>
            {truncate(node.workflow ?? '', 30)}
          </text>
        </>
      )}
      <circle className="dag-port" cx={0} cy={NODE_H / 2} r={4} />
      <circle className="dag-port" cx={NODE_W} cy={NODE_H / 2} r={4} />
    </g>
  );
}

function LineageLegend({ canOpenTasks }: { canOpenTasks: boolean }) {
  return (
    <div className="legend">
      <span className="legend-item">
        <span className="lineage-swatch-asset" aria-hidden="true" /> asset — click to open it
      </span>
      <span className="legend-item">
        <span className="lineage-swatch-task" aria-hidden="true" /> task{canOpenTasks ? ' — click to open its workflow' : ''}
      </span>
      <span className="legend-item">Arrows run task → asset it produces → task that consumes it. Declared in specs, not observed.</span>
    </div>
  );
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n - 1)}…`;
}
