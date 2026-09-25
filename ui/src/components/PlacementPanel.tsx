// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Which worker is running which task, for one run.
 *
 * The graph answers "what broke" and the attempt cards answer "why", but both
 * are per node: to learn WHERE a task is running an operator had to click each
 * node in turn and read the placement block on its latest attempt. During an
 * incident the question is asked the other way round — "is everything on
 * gpu-node-3 stuck?" — and that needs every node's placement on one screen.
 * This panel is that join, read from the attempts the run page already holds,
 * so it costs no extra request and cannot disagree with the cards.
 *
 * Rows are ordered by what is happening now: RUNNING and DISPATCHED first,
 * then PENDING (with the reason it has not dispatched), then everything
 * finished, in graph order. Each worker links to its fleet page, which lists
 * what ELSE that machine is doing — the other half of the question.
 */

import { Link } from 'react-router-dom';
import type { Attempt } from '../api/types';
import type { DagGraph, DagNode } from '../lib/dag';
import { EMPTY, fmtDurationMs, fmtTs, parseTs, shortId } from '../lib/format';
import { executorStatus, isExecutorWaiting, isInFlight } from '../lib/progress';
import { normalizeState } from '../lib/states';
import { StateBadge } from './StateBadge';
import { DataTable } from './DataTable';
import type { Column, RowCountKind } from './DataTable';
import { Empty } from './Status';

interface PlacementRow {
  node: DagNode;
  attempt: Attempt | null;
}

/** Every node of the graph is a row, so the count is exact by construction. */
const PLACEMENT_COUNT: RowCountKind = { kind: 'exact' };

const ACTIVE = new Set(['DISPATCHED', 'RUNNING']);

function rank(row: PlacementRow): number {
  const s = row.attempt ? normalizeState(row.attempt.state) : null;
  if (s && ACTIVE.has(s)) return 0;
  if (s === 'PENDING') return 1;
  return 2;
}

/** The worker a row is on, as a label an operator can match to the fleet page. */
export function workerName(a: Attempt | null): string | null {
  if (!a || !a.worker_id) return null;
  return a.worker_hostname ?? shortId(a.worker_id, 12);
}

export function PlacementPanel({
  graph,
  selectedNodeId,
  onSelectNode,
  live,
}: {
  graph: DagGraph;
  selectedNodeId?: string | null;
  onSelectNode: (nodeId: string) => void;
  /** True while the run is not terminal: durations then read as elapsed-so-far. */
  live: boolean;
}) {
  const now = Date.now();
  const rows: PlacementRow[] = graph.nodes
    .map((node) => ({ node, attempt: node.latestAttempt }))
    .sort((a, b) => rank(a) - rank(b) || a.node.depth - b.node.depth || a.node.order - b.node.order);

  const active = rows.filter((r) => r.attempt && ACTIVE.has(normalizeState(r.attempt.state) ?? ''));
  const workers = new Set(rows.map((r) => r.attempt?.worker_id).filter((id): id is string => !!id));
  const activeWorkers = new Set(active.map((r) => r.attempt?.worker_id).filter((id): id is string => !!id));
  const placed = rows.some((r) => r.attempt?.worker_id);

  const columns: Column<PlacementRow>[] = [
    {
      key: 'state',
      header: 'state',
      cell: (r) =>
        r.attempt ? (
          <StateBadge kind="attempt" state={r.attempt.state} />
        ) : (
          <StateBadge kind="node" state={r.node.state} />
        ),
    },
    {
      key: 'task',
      header: 'task',
      cell: (r) => (
        <div className="cell-stack">
          {/* The node id is the link into the graph and the attempt cards: the
              same selection the graph makes, so the two never disagree. */}
          <button
            type="button"
            className="link mono"
            aria-current={r.node.node_id === selectedNodeId ? 'true' : undefined}
            onClick={() => onSelectNode(r.node.node_id)}
          >
            {r.node.node_id}
          </button>
          <span className="cell-sub mono">
            {r.node.task_ref}
            {r.attempt && r.attempt.attempt_number > 1 && ` · attempt ${r.attempt.attempt_number}`}
          </span>
        </div>
      ),
    },
    {
      key: 'worker',
      header: 'worker',
      cell: (r) => <WorkerCell attempt={r.attempt} />,
    },
    {
      key: 'status',
      header: 'why / status',
      cell: (r) => <StatusCell attempt={r.attempt} />,
    },
    {
      key: 'queue',
      header: 'queue',
      cellClass: 'mono',
      cell: (r) => <>{r.attempt?.queue || EMPTY}</>,
    },
    {
      key: 'duration',
      header: live ? 'running for' : 'took',
      cell: (r) => <Duration attempt={r.attempt} now={now} />,
    },
  ];

  return (
    <div className="panel">
      <div className="panel-head">
        <span>task placement</span>
        {active.length > 0 && (
          <span className="chip">
            {active.length} running on {activeWorkers.size} {activeWorkers.size === 1 ? 'worker' : 'workers'}
          </span>
        )}
        {active.length === 0 && workers.size > 0 && (
          <span className="chip">
            {workers.size} {workers.size === 1 ? 'worker' : 'workers'} used
          </span>
        )}
        <span className="spacer" />
        <span className="panel-note">which worker holds each task · a worker links to everything else on it</span>
      </div>
      {!placed ? (
        <div className="panel-body">
          <Empty
            title="Nothing has been placed on a worker yet"
            detail={
              rows.some((r) => r.attempt?.pending_reason)
                ? 'Attempts exist but none has dispatched. The pending reason on each node says what it is waiting for.'
                : 'A node gets a worker when the scheduler dispatches its attempt. Until then there is nothing to place.'
            }
          />
        </div>
      ) : (
        <div className="worker-tasks">
          <DataTable
            rows={rows}
            columns={columns}
            rowKey={(r) => r.node.node_id}
            rowClass={(r) => {
              const s = r.attempt ? normalizeState(r.attempt.state) : null;
              return [
                s === 'FAILED' || s === 'LOST' ? 'row-failed' : '',
                r.node.node_id === selectedNodeId ? 'row-selected' : '',
              ]
                .filter(Boolean)
                .join(' ');
            }}
            noun="tasks"
            tableClass="table-fleet"
            count={PLACEMENT_COUNT}
            paging={{ mode: 'none' }}
          />
        </div>
      )}
    </div>
  );
}

/**
 * The worker, linked to its own page. An attempt with no worker says why in
 * its own words when it can (pending_reason) rather than showing a dash that
 * reads as "unknown".
 */
function WorkerCell({ attempt }: { attempt: Attempt | null }) {
  if (!attempt) return <span className="faint">not scheduled</span>;
  if (!attempt.worker_id) {
    const s = normalizeState(attempt.state);
    if (s === 'PENDING') {
      return (
        <span className="faint" title={attempt.pending_reason ?? undefined}>
          {attempt.pending_reason ? `waiting: ${attempt.pending_reason}` : 'waiting for a worker'}
        </span>
      );
    }
    if (attempt.cache_hit_of) return <span className="faint">cached · did not execute</span>;
    return <span className="faint">{EMPTY}</span>;
  }
  return (
    <div className="cell-stack">
      <Link to={`/workers/${encodeURIComponent(attempt.worker_id)}`} className="mono" title={attempt.worker_id}>
        {attempt.worker_hostname ?? shortId(attempt.worker_id, 12)}
      </Link>
      {attempt.worker_hostname && <span className="cell-sub mono">{shortId(attempt.worker_id, 12)}</span>}
    </div>
  );
}

/**
 * Why a task is where it is, labelled by WHO says so. Before placement the
 * answer is the scheduler's (pending_reason); after placement it is the
 * worker's executor (progress_message, e.g. a Kubernetes pod still Pending).
 * The two are different schedulers and must not read as one.
 */
export function StatusCell({ attempt }: { attempt: Attempt | null }) {
  if (!attempt) return <span className="faint">{EMPTY}</span>;
  const s = normalizeState(attempt.state);
  if (s === 'PENDING') {
    if (!attempt.pending_reason) return <span className="faint">{EMPTY}</span>;
    return (
      <span className="cell-stack">
        <span className="chip">scheduler</span>
        <span className="cell-sub mono" title={attempt.pending_reason}>
          {attempt.pending_reason}
        </span>
      </span>
    );
  }
  const status = executorStatus(attempt);
  if (!status) {
    // Only claim "nothing reported" when the gateway serves the field and
    // the attempt is on a worker; otherwise there is nothing honest to say.
    if (isInFlight(attempt) && attempt.progress_message === null) {
      return <span className="faint">no executor status reported</span>;
    }
    return <span className="faint">{EMPTY}</span>;
  }
  const waiting = isExecutorWaiting(attempt);
  return (
    <span className="cell-stack">
      <span className={`chip${waiting ? ' warn' : ''}`}>{status.stale ? 'executor · last' : 'executor'}</span>
      <span className="cell-sub mono" title={status.message}>
        {status.message.length > 90 ? `${status.message.slice(0, 89)}…` : status.message}
      </span>
    </span>
  );
}

function Duration({ attempt, now }: { attempt: Attempt | null; now: number }) {
  if (!attempt) return <span className="faint">{EMPTY}</span>;
  const s = normalizeState(attempt.state);
  const started = parseTs(attempt.started_at);
  const ended = parseTs(attempt.ended_at);
  if (s && ACTIVE.has(s)) {
    if (started === null) {
      return (
        <span className="chip warn" title="Handed to the worker but not started.">
          not started
        </span>
      );
    }
    return (
      <span className="mono" title={fmtTs(attempt.started_at)}>
        {fmtDurationMs(now - started)}
      </span>
    );
  }
  if (started === null || ended === null) return <span className="faint">{EMPTY}</span>;
  return (
    <span className="mono" title={fmtTs(attempt.ended_at)}>
      {fmtDurationMs(ended - started)}
    </span>
  );
}
