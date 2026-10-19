// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Live control-plane instances (C3): one row per reporting pod.
 *
 * Shared by the Overview (every component) and the Scheduler tab (scheduler
 * replicas only). Uptime and last-seen are relative to `now`, which the
 * caller ticks, so a pod that stops reporting visibly ages between polls.
 */

import type { ComponentGroup, ComponentInstance } from '../../../api/platform';
import { DataTable, type Column } from '../../../components/DataTable';
import { fmtAge, fmtDurationMs, fmtTs, parseTs } from '../../../lib/format';
import { COMPONENT_LABEL } from './model';
import { InstanceBadge, KeyValues, NotReported, OrNotReported } from './ui';

type Row = ComponentInstance & { component: string };

const STATE_ORDER: Record<string, number> = { not_ready: 0, stale: 1, ready: 2 };

export function InstancesTable({ groups, now, showComponent = true, noun = 'instances' }: { groups: ComponentGroup[]; now: number; showComponent?: boolean; noun?: string }) {
  const rows: Row[] = groups.flatMap((g) => g.instances.map((i) => ({ ...i, component: g.component })));
  const columns: Column<Row>[] = [
    {
      key: 'instance',
      header: 'instance / pod',
      cell: (r) => (
        <div className="cell-stack">
          <code className="infra-id">{r.instance || '—'}</code>
          <span className="sub">{r.pod_namespace ? `namespace ${r.pod_namespace}` : <NotReported>namespace not reported</NotReported>}</span>
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
    { key: 'node', header: 'node', cell: (r) => <OrNotReported value={r.node} mono />, sort: (a, b) => a.node.localeCompare(b.node) },
    { key: 'cluster', header: 'cluster', cell: (r) => <OrNotReported value={r.cluster} />, sort: (a, b) => a.cluster.localeCompare(b.cluster) },
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
    { key: 'detail', header: 'detail', cell: (r) => <KeyValues data={r.detail} empty="—" /> },
  ];
  return (
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
  );
}
