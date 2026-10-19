// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Task runners (C3): every running task attempt with WHERE it runs — worker,
 * executor, cluster, pod and Kubernetes node.
 *
 * Placement is reported by the agent after the pod is scheduled, so a fresh
 * attempt legitimately has none yet: those cells say "not reported" (and the
 * filters offer it as a value of its own) instead of rendering blank or zero.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import type { TaskRunner } from '../../../api/platform';
import { TASK_RUNNER_DEFAULT_LIMIT } from '../../../api/platform';
import { INFRA_POLL_MS, useTaskRunners } from '../../../api/hooks';
import { DataTable, type Column } from '../../../components/DataTable';
import { StateBadge } from '../../../components/StateBadge';
import { ErrorBox, Loading, StaleWarning } from '../../../components/Status';
import { fmtAge, fmtDurationMs, fmtTs, parseTs } from '../../../lib/format';
import { distinctValues, filterRunners, NO_RUNNER_FILTER, NOT_REPORTED, type RunnerFilter } from './model';
import { CountChips, Notice, NotReported, OrNotReported, Panel, Updated } from './ui';

const LIMITS = [200, 500, 1000] as const;

function FilterSelect({ id, label, value, values, onChange, notReportedLabel }: { id: string; label: string; value: string; values: string[]; onChange: (v: string) => void; notReportedLabel: string }) {
  return (
    <label className="infra-filter" htmlFor={id}>
      <span>{label}</span>
      <select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
        <option value="">All</option>
        {values.map((v) => (
          <option key={v || NOT_REPORTED} value={v || NOT_REPORTED}>
            {v || notReportedLabel}
          </option>
        ))}
      </select>
    </label>
  );
}

export function TaskRunnersTab({ namespace, now }: { namespace: string; now: number }) {
  const [limit, setLimit] = useState<number>(TASK_RUNNER_DEFAULT_LIMIT);
  const [filter, setFilter] = useState<RunnerFilter>(NO_RUNNER_FILTER);
  const q = useTaskRunners(namespace, limit);

  const report = q.data?.kind === 'ok' ? q.data.data : null;
  const runners = report?.runners ?? [];
  const rows = filterRunners(runners, filter);
  const filtered = Object.values(filter).some((v) => v !== '');

  const columns: Column<TaskRunner>[] = [
    {
      key: 'task',
      header: 'task',
      cell: (r) => (
        <div className="cell-stack">
          <Link to={`/runs/${encodeURIComponent(r.run_id)}?node=${encodeURIComponent(r.node_id)}`} title="Open the run at this task">
            {r.node_id || r.attempt_id}
          </Link>
          <span className="sub">{r.workflow || <NotReported>workflow not reported</NotReported>}</span>
        </div>
      ),
      sort: (a, b) => a.node_id.localeCompare(b.node_id),
    },
    {
      key: 'attempt',
      header: 'attempt / run',
      cell: (r) => (
        <div className="cell-stack">
          <code className="infra-id" title={r.attempt_id}>
            {r.attempt_id.slice(0, 12)}
          </code>
          <Link to={`/runs/${encodeURIComponent(r.run_id)}`} className="sub" aria-label={`Open run ${r.run_id}`}>
            run {r.run_id.slice(0, 12)}
          </Link>
        </div>
      ),
    },
    { key: 'state', header: 'state', cell: (r) => <StateBadge kind="attempt" state={r.state} small />, sort: (a, b) => a.state.localeCompare(b.state) },
    {
      key: 'worker',
      header: 'worker',
      cell: (r) =>
        r.worker_id ? (
          <div className="cell-stack">
            <Link to={`/workers/${encodeURIComponent(r.worker_id)}`}>{r.worker_host || r.worker_id}</Link>
            <span className="sub">{r.executor ? `${r.executor} executor` : <NotReported>executor not reported</NotReported>}</span>
          </div>
        ) : (
          <NotReported />
        ),
      sort: (a, b) => (a.worker_host || a.worker_id).localeCompare(b.worker_host || b.worker_id),
    },
    { key: 'cluster', header: 'cluster', cell: (r) => <OrNotReported value={r.cluster} />, sort: (a, b) => a.cluster.localeCompare(b.cluster) },
    {
      key: 'pod',
      header: 'pod',
      cell: (r) =>
        r.pod ? (
          <code className="infra-id">
            {r.k8s_namespace ? `${r.k8s_namespace}/` : ''}
            {r.pod}
          </code>
        ) : (
          <NotReported>{r.executor && r.executor !== 'kubernetes' ? `no pod (${r.executor})` : 'not reported'}</NotReported>
        ),
    },
    { key: 'node', header: 'k8s node', cell: (r) => <OrNotReported value={r.k8s_node} mono />, sort: (a, b) => a.k8s_node.localeCompare(b.k8s_node) },
    {
      key: 'running',
      header: 'running for',
      cell: (r) => {
        const t = parseTs(r.started_at);
        return t === null ? <NotReported /> : <span title={fmtTs(r.started_at)}>{fmtDurationMs(now - t)}</span>;
      },
      sort: (a, b) => (parseTs(a.started_at) ?? 0) - (parseTs(b.started_at) ?? 0),
      align: 'end',
    },
    { key: 'progress', header: 'progress', cell: (r) => r.progress_message || <span className="faint">—</span> },
    {
      key: 'placement',
      header: 'placement reported',
      cell: (r) => (parseTs(r.placement_reported_at) === null ? <NotReported>not yet</NotReported> : <span title={fmtTs(r.placement_reported_at)}>{fmtAge(r.placement_reported_at, now)}</span>),
    },
  ];

  return (
    <div className="col">
      <Panel
        title="Running task attempts"
        note={
          <>
            Refreshes every {INFRA_POLL_MS.taskRunners / 1000}s · <Updated at={q.dataUpdatedAt || null} now={now} />
          </>
        }
        actions={
          <>
            <label className="infra-filter" htmlFor="infra-runner-limit">
              <span>Fetch up to</span>
              <select id="infra-runner-limit" value={limit} onChange={(e) => setLimit(Number(e.target.value))}>
                {LIMITS.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
            </label>
            <button type="button" onClick={() => void q.refetch()} disabled={q.isFetching}>
              {q.isFetching ? 'Refreshing…' : 'Refresh'}
            </button>
          </>
        }
      >
        {q.error != null && q.data !== undefined && <StaleWarning error={q.error} />}
        {q.data === undefined ? (
          q.error ? (
            <ErrorBox error={q.error} what="task runners" onRetry={() => void q.refetch()} />
          ) : (
            <Loading rows={4} label="Loading task runners" />
          )
        ) : q.data.kind === 'unsupported' ? (
          <Notice title="This control plane does not report task placement (upgrade)">
            Upgrade KGF Nexus to see which worker, pod and node runs each task. Running attempts are still listed per worker on the worker
            fleet page.
          </Notice>
        ) : (
          report && (
            <>
              <div className="infra-summary" aria-label="Running task summary">
                <span className="infra-summary-total">
                  <strong>{report.summary.running}</strong> running
                  {report.runners.length < report.summary.running && <span className="sub"> · {report.runners.length} listed (limit {report.limit})</span>}
                </span>
                <CountChips counts={report.summary.by_executor} label="By executor" none="executor not reported" />
                <CountChips counts={report.summary.by_cluster} label="By cluster" none="cluster not reported" />
              </div>
              {!runners.length ? (
                <Notice title="No task is running">Running attempts appear here with their pod and node as soon as an agent starts them.</Notice>
              ) : (
                <DataTable
                  rows={rows}
                  columns={columns}
                  rowKey={(r) => r.attempt_id}
                  noun="task runners"
                  count={filtered ? { kind: 'unknown' } : { kind: 'capped', limit: report.limit }}
                  search={{ mode: 'client', fields: (r) => [r.node_id, r.workflow, r.attempt_id, r.run_id, r.worker_host, r.worker_id, r.pod, r.k8s_node, r.cluster, r.progress_message] }}
                  defaultSort={{ key: 'running', dir: 'desc' }}
                  tableClass="infra-runners"
                  noMatchTitle="No running task matches"
                  toolbar={
                    <div className="infra-filters" role="group" aria-label="Filter task runners">
                      <FilterSelect id="infra-f-executor" label="Executor" value={filter.executor} values={distinctValues(runners, 'executor')} notReportedLabel="Not reported" onChange={(executor) => setFilter({ ...filter, executor })} />
                      <FilterSelect id="infra-f-cluster" label="Cluster" value={filter.cluster} values={distinctValues(runners, 'cluster')} notReportedLabel="Not reported" onChange={(cluster) => setFilter({ ...filter, cluster })} />
                      <FilterSelect id="infra-f-node" label="K8s node" value={filter.node} values={distinctValues(runners, 'k8s_node')} notReportedLabel="Not reported" onChange={(node) => setFilter({ ...filter, node })} />
                      <FilterSelect id="infra-f-state" label="State" value={filter.state} values={distinctValues(runners, 'state')} notReportedLabel="Not reported" onChange={(state) => setFilter({ ...filter, state })} />
                      {filtered && (
                        <button type="button" className="link" onClick={() => setFilter(NO_RUNNER_FILTER)}>
                          Clear filters
                        </button>
                      )}
                    </div>
                  }
                />
              )}
              {filtered && runners.length > 0 && rows.length === 0 && (
                <Notice title="No running task matches these filters">Clear a filter to see more.</Notice>
              )}
            </>
          )
        )}
      </Panel>
    </div>
  );
}
