// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Live control-plane instances (C3): one row per reporting pod.
 *
 * Shared by the Overview (every component) and the Scheduler tab (scheduler
 * replicas only). Uptime and last-seen are relative to `now`, which the
 * caller ticks, so a pod that stops reporting visibly ages between polls.
 *
 * Two things the registry does that the table must not misreport:
 *  - A replaced pod's row lingers for up to 24h as `stale`. While a ready
 *    replacement exists those rows are HISTORY, listed below the table, not
 *    faults in it (model.splitHistory).
 *  - Unless the caller may see the topology, instance names are opaque and
 *    pod namespace, node and cluster are empty ON PURPOSE: those cells say
 *    "hidden", not "not reported", and the table says who can see them.
 */

import type { ComponentGroup, ComponentInstance } from '../../../api/platform';
import { DataTable, type Column } from '../../../components/DataTable';
import { fmtAge, fmtDurationMs, fmtTs, parseTs } from '../../../lib/format';
import { COMPONENT_LABEL, detailSummary, splitHistory } from './model';
import { InstanceBadge, KeyValues, NotReported, OrNotReported } from './ui';

type Row = ComponentInstance & { component: string };

const STATE_ORDER: Record<string, number> = { not_ready: 0, stale: 1, ready: 2 };

export const TOPOLOGY_NOTE =
  'Pod, node and cluster are shown to platform operators, or to every infrastructure viewer when the gateway runs with NEXUS_COMPONENT_TOPOLOGY=infrastructure-viewers.';

function Hidden() {
  return <NotReported>hidden</NotReported>;
}

export function InstancesTable({
  groups,
  now,
  topologyVisible,
  showComponent = true,
  noun = 'instances',
}: {
  groups: ComponentGroup[];
  now: number;
  topologyVisible: boolean;
  showComponent?: boolean;
  noun?: string;
}) {
  const split = groups.map((g) => ({ g, ...splitHistory(g.instances) }));
  const rows: Row[] = split.flatMap(({ g, current }) => current.map((i) => ({ ...i, component: g.component })));
  const history: Row[] = split.flatMap(({ g, history: h }) => h.map((i) => ({ ...i, component: g.component })));
  const place = (v: string, mono = true) => (topologyVisible ? <OrNotReported value={v} mono={mono} /> : <Hidden />);
  const columns: Column<Row>[] = [
    {
      key: 'instance',
      header: topologyVisible ? 'instance / pod' : 'instance',
      cell: (r) => (
        <div className="cell-stack">
          <code className="infra-id">{r.instance || '—'}</code>
          {topologyVisible && <span className="sub">{r.pod_namespace ? `namespace ${r.pod_namespace}` : <NotReported>namespace not reported</NotReported>}</span>}
        </div>
      ),
      sort: (a, b) => a.instance.localeCompare(b.instance),
    },
    ...(showComponent
      ? [
          {
            key: 'component',
            header: 'component',
            cell: (r: Row) => COMPONENT_LABEL[r.component] ?? r.component,
            sort: (a: Row, b: Row) => a.component.localeCompare(b.component),
          } satisfies Column<Row>,
        ]
      : []),
    { key: 'state', header: 'state', cell: (r) => <InstanceBadge state={r.state} />, sort: (a, b) => (STATE_ORDER[a.state] ?? 3) - (STATE_ORDER[b.state] ?? 3) },
    { key: 'node', header: 'node', cell: (r) => place(r.node), sort: (a, b) => a.node.localeCompare(b.node) },
    { key: 'cluster', header: 'cluster', cell: (r) => place(r.cluster, false), sort: (a, b) => a.cluster.localeCompare(b.cluster) },
    { key: 'version', header: 'version', cell: (r) => <OrNotReported value={r.version} mono /> },
    {
      key: 'uptime',
      header: 'uptime',
      cell: (r) => {
        const t = parseTs(r.started_at);
        return t === null ? <NotReported /> : <span title={fmtTs(r.started_at)}>{fmtDurationMs(now - t)}</span>;
      },
      sort: (a, b) => (parseTs(b.started_at) ?? 0) - (parseTs(a.started_at) ?? 0),
    },
    {
      key: 'seen',
      header: 'last seen',
      cell: (r) => (parseTs(r.last_seen_at) === null ? <NotReported /> : <span title={fmtTs(r.last_seen_at)}>{fmtAge(r.last_seen_at, now)}</span>),
      sort: (a, b) => (parseTs(b.last_seen_at) ?? 0) - (parseTs(a.last_seen_at) ?? 0),
    },
    {
      key: 'detail',
      header: 'detail',
      cell: (r) => {
        const summary = detailSummary(r.component, r.detail);
        const count = Object.keys(r.detail).length;
        return (
          <div className="cell-stack">
            {summary.length ? <span>{summary.join(' · ')}</span> : null}
            {count > 0 ? (
              <details className="infra-detail-disclosure">
                <summary>
                  All detail ({count})<span className="sr-only"> for {r.instance}</span>
                </summary>
                <KeyValues data={r.detail} />
              </details>
            ) : (
              <NotReported>none</NotReported>
            )}
          </div>
        );
      },
    },
  ];
  return (
    <>
      {!topologyVisible && <p className="sub infra-measure">{TOPOLOGY_NOTE}</p>}
      <DataTable
        rows={rows}
        columns={columns}
        rowKey={(r) => `${r.component}/${r.instance}/${r.pod_namespace}/${r.cluster}`}
        rowClass={(r) => (r.state === 'not_ready' ? 'row-failed' : r.state === 'stale' ? 'row-warning' : '')}
        noun={noun}
        count={{ kind: 'exact' }}
        search={{ mode: 'client', fields: (r) => [r.instance, r.component, r.node, r.cluster, r.version, r.state, r.pod_namespace] }}
        defaultSort={{ key: 'state', dir: 'asc' }}
        tableClass="table-fleet infra-instances"
        paging={{ mode: 'client' }}
      />
      {history.length > 0 && (
        <details className="infra-history">
          <summary>
            {history.length} previous instance{history.length === 1 ? '' : 's'} — replaced pods that stopped reporting (kept up to 24h)
          </summary>
          <ul className="infra-plain-list">
            {history.map((r) => (
              <li key={`${r.component}/${r.instance}`}>
                <code className="infra-id">{r.instance}</code>
                {showComponent && <span className="sub"> · {COMPONENT_LABEL[r.component] ?? r.component}</span>}
                <span className="sub">
                  {' '}
                  · {r.version || 'version not reported'} · last seen {fmtAge(r.last_seen_at, now)}
                </span>
              </li>
            ))}
          </ul>
        </details>
      )}
    </>
  );
}
