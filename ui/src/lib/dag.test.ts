// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Tests for DAG state derivation.
 *
 * The case that matters most is root-cause attribution: given a cascade, the UI
 * must point at the node that actually broke and NOT at the ones that merely
 * inherited the failure. That is the operational payoff of separating FAILED
 * from UPSTREAM_FAILED, and it is pure logic, so it gets tested directly.
 */

import { describe, expect, it } from 'vitest';
import type { Attempt, NodeState, RunEdge, RunNode } from '../api/types';
import { buildDagGraph, groupAttempts, summarizeDag } from './dag';

function node(id: string, state: NodeState, extra: Partial<RunNode> = {}): RunNode {
  return {
    run_id: 'r1',
    node_id: id,
    task_ref: `pkg.${id}:v1`,
    state,
    ...extra,
  };
}

function edge(from: string, to: string): RunEdge {
  return { from_node_id: from, to_node_id: to };
}

function attempt(nodeId: string, n: number, state: Attempt['state'], extra: Partial<Attempt> = {}): Attempt {
  return {
    id: `${nodeId}-a${n}`,
    run_id: 'r1',
    node_id: nodeId,
    attempt_number: n,
    state,
    ...extra,
  };
}

describe('depth layering', () => {
  it('places roots at depth 0 and uses longest path so diamonds do not collapse', () => {
    // a → b → c → d  and  a → d : d must sit at depth 3, not depth 1.
    const g = buildDagGraph({
      nodes: ['a', 'b', 'c', 'd'].map((id) => node(id, 'PENDING')),
      edges: [edge('a', 'b'), edge('b', 'c'), edge('c', 'd'), edge('a', 'd')],
    });
    expect(g.byId.a!.depth).toBe(0);
    expect(g.byId.b!.depth).toBe(1);
    expect(g.byId.c!.depth).toBe(2);
    expect(g.byId.d!.depth).toBe(3);
    expect(g.hasCycle).toBe(false);
  });

  it('produces a stable, sorted order within each layer', () => {
    const g = buildDagGraph({
      nodes: [node('z', 'PENDING'), node('a', 'PENDING'), node('m', 'PENDING')],
      edges: [],
    });
    expect(g.layers[0]).toEqual(['a', 'm', 'z']);
    expect(g.byId.a!.order).toBe(0);
    expect(g.byId.z!.order).toBe(2);
  });

  it('reports a cycle rather than dropping the nodes in it', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'PENDING'), node('b', 'PENDING')],
      edges: [edge('a', 'b'), edge('b', 'a')],
    });
    expect(g.hasCycle).toBe(true);
    expect(g.nodes).toHaveLength(2);
  });

  it('ignores edges referencing unknown nodes instead of throwing', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'PENDING')],
      edges: [edge('a', 'ghost'), edge('ghost', 'a'), edge('a', 'a')],
    });
    expect(g.edges).toHaveLength(0);
    expect(g.nodes).toHaveLength(1);
  });

  it('handles an empty run without throwing', () => {
    const g = buildDagGraph({ nodes: [], edges: [] });
    expect(g.nodes).toHaveLength(0);
    expect(g.rootCauses).toHaveLength(0);
    expect(summarizeDag(g)).toBe('No nodes.');
  });
});

describe('root-cause attribution', () => {
  it('marks the origin of a cascade and not its victims', () => {
    //   extract(SUCCEEDED) → transform(FAILED) → load(UPSTREAM_FAILED)
    const g = buildDagGraph({
      nodes: [
        node('extract', 'SUCCEEDED'),
        node('transform', 'FAILED'),
        node('load', 'UPSTREAM_FAILED'),
      ],
      edges: [edge('extract', 'transform'), edge('transform', 'load')],
    });
    expect(g.rootCauses).toEqual(['transform']);
    expect(g.byId.transform!.isRootCause).toBe(true);
    expect(g.byId.load!.isRootCause).toBe(false);
    expect(g.byId.extract!.isRootCause).toBe(false);
  });

  it('does NOT mark a downstream FAILED node whose ancestor also failed', () => {
    // An all_done node runs and fails after an upstream failure. The upstream
    // failure is still the root cause; the downstream one is collateral.
    const g = buildDagGraph({
      nodes: [node('a', 'FAILED'), node('b', 'FAILED')],
      edges: [edge('a', 'b')],
    });
    expect(g.rootCauses).toEqual(['a']);
    expect(g.byId.b!.isRootCause).toBe(false);
  });

  it('sees through a chain of UPSTREAM_FAILED nodes to the real origin', () => {
    const g = buildDagGraph({
      nodes: [
        node('a', 'FAILED'),
        node('b', 'UPSTREAM_FAILED'),
        node('c', 'UPSTREAM_FAILED'),
        node('d', 'FAILED'),
      ],
      edges: [edge('a', 'b'), edge('b', 'c'), edge('c', 'd')],
    });
    expect(g.rootCauses).toEqual(['a']);
  });

  it('reports multiple independent root causes', () => {
    const g = buildDagGraph({
      nodes: [
        node('x', 'FAILED'),
        node('y', 'FAILED'),
        node('sink', 'UPSTREAM_FAILED'),
      ],
      edges: [edge('x', 'sink'), edge('y', 'sink')],
    });
    expect(g.rootCauses).toEqual(['x', 'y']);
  });

  it('never treats SKIPPED as a failure — a skipped branch is not a problem', () => {
    const g = buildDagGraph({
      nodes: [node('branch', 'SKIPPED'), node('after', 'SKIPPED')],
      edges: [edge('branch', 'after')],
    });
    expect(g.rootCauses).toEqual([]);
    expect(g.byId.after!.isRootCause).toBe(false);
  });

  it('never treats CANCELLED as a failure', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'CANCELLED'), node('b', 'CANCELLED')],
      edges: [edge('a', 'b')],
    });
    expect(g.rootCauses).toEqual([]);
  });

  it('does not hang on a cycle containing a failure', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'FAILED'), node('b', 'FAILED')],
      edges: [edge('a', 'b'), edge('b', 'a')],
    });
    expect(g.hasCycle).toBe(true);
    expect(g.rootCauses.length).toBeGreaterThanOrEqual(0);
  });

  it('accepts protobuf-spelled node states', () => {
    const g = buildDagGraph({
      nodes: [
        node('a', 'NODE_STATE_FAILED' as NodeState),
        node('b', 'NODE_STATE_UPSTREAM_FAILED' as NodeState),
      ],
      edges: [edge('a', 'b')],
    });
    expect(g.byId.a!.state).toBe('FAILED');
    expect(g.rootCauses).toEqual(['a']);
  });
});

describe('attempt history attachment', () => {
  it('keeps every attempt as its own row, ordered by attempt_number', () => {
    // Deliberately out of order on the wire: retries are separate immutable
    // rows (invariant 9) and the UI must show all of them, newest last.
    const g = buildDagGraph(
      { nodes: [node('t', 'FAILED')], edges: [] },
      [
        attempt('t', 3, 'FAILED'),
        attempt('t', 1, 'FAILED'),
        attempt('t', 2, 'LOST'),
      ],
    );
    expect(g.byId.t!.attempts.map((a) => a.attempt_number)).toEqual([1, 2, 3]);
    expect(g.byId.t!.latestAttempt!.attempt_number).toBe(3);
  });

  it('leaves a node with no attempts with an empty history, not null', () => {
    const g = buildDagGraph({ nodes: [node('t', 'PENDING')], edges: [] }, []);
    expect(g.byId.t!.attempts).toEqual([]);
    expect(g.byId.t!.latestAttempt).toBeNull();
  });

  it('lifts pending_reason from the latest attempt so a stall is explained', () => {
    const g = buildDagGraph(
      { nodes: [node('train', 'READY')], edges: [] },
      [attempt('train', 1, 'PENDING', { pending_reason: 'waiting_for_resources: gpu (need 4, free 3)' })],
    );
    expect(g.byId.train!.pendingReason).toBe('waiting_for_resources: gpu (need 4, free 3)');
  });

  it('prefers a node-level pending_reason when the projection supplies one', () => {
    const g = buildDagGraph(
      { nodes: [node('train', 'READY', { pending_reason: 'node-level reason' })], edges: [] },
      [attempt('train', 1, 'PENDING', { pending_reason: 'attempt-level reason' })],
    );
    expect(g.byId.train!.pendingReason).toBe('node-level reason');
  });

  it('groups attempts across nodes independently', () => {
    const grouped = groupAttempts([
      attempt('a', 2, 'SUCCEEDED'),
      attempt('b', 1, 'RUNNING'),
      attempt('a', 1, 'FAILED'),
    ]);
    expect(Object.keys(grouped).sort()).toEqual(['a', 'b']);
    expect(grouped.a!.map((x) => x.attempt_number)).toEqual([1, 2]);
  });
});

describe('counts and summary', () => {
  it('counts nodes per state', () => {
    const g = buildDagGraph({
      nodes: [
        node('a', 'SUCCEEDED'),
        node('b', 'SUCCEEDED'),
        node('c', 'RUNNING'),
        node('d', 'SKIPPED'),
      ],
      edges: [],
    });
    expect(g.counts.SUCCEEDED).toBe(2);
    expect(g.counts.RUNNING).toBe(1);
    expect(g.counts.SKIPPED).toBe(1);
    expect(g.counts.FAILED).toBeUndefined();
  });

  it('summarises a failure by naming the root cause and the blocked count', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'FAILED'), node('b', 'UPSTREAM_FAILED'), node('c', 'UPSTREAM_FAILED')],
      edges: [edge('a', 'b'), edge('b', 'c')],
    });
    expect(summarizeDag(g)).toBe('Failed at a; 2 nodes blocked downstream.');
  });

  it('summarises a healthy finished run', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'SUCCEEDED'), node('b', 'SUCCEEDED')],
      edges: [edge('a', 'b')],
    });
    expect(summarizeDag(g)).toBe('All nodes succeeded.');
  });

  it('summarises in-flight work', () => {
    const g = buildDagGraph({
      nodes: [node('a', 'RUNNING'), node('b', 'PENDING')],
      edges: [edge('a', 'b')],
    });
    expect(summarizeDag(g)).toBe('1 node running.');
  });
});
