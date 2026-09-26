// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The notification outbox: every message Nexus tried to send, and what became
 * of it.
 *
 * `dead` is the state that needs a person. A delivery goes dead after its last
 * retry failed, and nothing retries it again on its own — so "Dead letters" is
 * a one-click view, and Redrive (which queues a fresh copy linked to the dead
 * one) is offered only there. The dead row stays as it was: the history of what
 * failed is not rewritten by fixing it.
 *
 * Paging is keyset on the delivery id, using the gateway's own `next_before`.
 */

import { useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { errorMessage, useChannels, useDeliveries, useRedriveDelivery } from '../api/hooks';
import type { DeliveryState, NotificationDelivery } from '../api/types';
import { DataTable, type Column } from '../components/DataTable';
import { usePermission } from '../components/Permissions';
import { RowDetails } from '../components/RowAction';
import { Empty } from '../components/Status';
import { EventsPanel } from '../components/EventsShared';
import { fmtAge, fmtTs, shortId } from '../lib/format';

const PAGE = 50;

const STATE_TONE: Record<string, string> = {
  pending: 'tone-queued',
  delivered: 'tone-success',
  dead: 'tone-danger',
};

export function DeliveriesTab({ namespace }: { namespace: string }) {
  const can = usePermission();
  const canManage = can('notifications.manage');
  const [params, setParams] = useSearchParams();
  const state = (params.get('state') ?? '') as DeliveryState | '';
  const channel = params.get('channel') ?? '';
  const runId = params.get('run_id') ?? '';
  const [cursors, setCursors] = useState<string[]>([]);
  const before = cursors[cursors.length - 1];
  const [runDraft, setRunDraft] = useState(runId);
  const [notice, setNotice] = useState<{ ok: boolean; text: string } | null>(null);

  const deliveries = useDeliveries(namespace, { state, channel, run_id: runId, limit: PAGE, before });
  const channels = useChannels(namespace);
  const redrive = useRedriveDelivery(namespace);
  const [tableControls, setTableControls] = useState<HTMLDivElement | null>(null);

  function setFilter(key: string, value: string) {
    setCursors([]);
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value) next.set(key, value);
        else next.delete(key);
        return next;
      },
      { replace: true },
    );
  }

  const now = Date.now();
  const columns: Column<NotificationDelivery>[] = [
    { key: 'id', header: 'id', cellClass: 'mono', cell: (d) => `#${d.id}` },
    {
      key: 'state',
      header: 'state',
      cell: (d) => (
        <span className="cell-stack">
          <span className={`badge ${STATE_TONE[d.state] ?? 'tone-neutral'}`}>{d.state}</span>
          {d.redrive_of != null && <span className="cell-sub">redrive of #{d.redrive_of}</span>}
        </span>
      ),
    },
    {
      key: 'created',
      header: 'created',
      cellClass: 'nowrap',
      cell: (d) => <span title={fmtTs(d.created_at)}>{fmtAge(d.created_at, now)}</span>,
    },
    {
      key: 'channel',
      header: 'channel',
      cell: (d) => (
        <span className="cell-stack">
          <span className="mono">{d.channel}</span>
          <span className="cell-sub">{d.channel_kind}</span>
        </span>
      ),
    },
    {
      key: 'event',
      header: 'event',
      cell: (d) => (
        <span className="cell-stack">
          <span className="mono">{d.event_type}</span>
          <span className="cell-sub">{d.rule ? `rule ${d.rule}` : 'channel test'}</span>
        </span>
      ),
    },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      cell: (d) => (d.run_id ? <Link to={`/runs/${encodeURIComponent(d.run_id)}`}>{shortId(d.run_id)}</Link> : <span className="faint">—</span>),
    },
    {
      key: 'attempts',
      header: 'attempts',
      align: 'end',
      cell: (d) => `${d.attempts}/${d.max_attempts}`,
    },
    {
      key: 'outcome',
      header: 'last outcome',
      cell: (d) =>
        d.last_error ? (
          <span className="cell-stack">
            <span style={{ color: 'var(--tone-danger-fg)' }}>{d.last_error}</span>
            {d.last_status != null && <span className="cell-sub">HTTP {d.last_status}</span>}
          </span>
        ) : d.state === 'delivered' ? (
          <span>
            {d.last_status != null ? `HTTP ${d.last_status}` : 'sent'}
            {d.delivered_at && <span className="faint"> · {fmtAge(d.delivered_at, now)}</span>}
          </span>
        ) : d.next_attempt_at ? (
          <span className="faint">next try {fmtTs(d.next_attempt_at)}</span>
        ) : (
          <span className="faint">—</span>
        ),
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (d) => (
        <div className="cell-actions">
          <RowDetails label={`View delivery ${d.id}`} value={d} />
          {canManage && d.state === 'dead' && (
            <button
              type="button"
              aria-label={`Redrive delivery ${d.id}`}
              disabled={redrive.isPending && redrive.variables === d.id}
              title="Queue a fresh copy of this message. The dead row is kept as history."
              onClick={() => {
                setNotice(null);
                redrive.mutate(d.id, {
                  onSuccess: (r) => setNotice({ ok: true, text: `Delivery #${d.id} redriven as #${r.delivery_id}.` }),
                  onError: (e) => setNotice({ ok: false, text: `Could not redrive #${d.id}: ${errorMessage(e)}` }),
                });
              }}
            >
              {redrive.isPending && redrive.variables === d.id ? 'Redriving…' : 'Redrive'}
            </button>
          )}
        </div>
      ),
    },
  ];

  const page = deliveries.data;

  return (
    <div className="col">
      <div className="cluster-tabs" role="group" aria-label="Delivery state">
        {(
          [
            ['', 'All'],
            ['pending', 'Pending'],
            ['delivered', 'Delivered'],
            ['dead', 'Dead letters'],
          ] as const
        ).map(([value, label]) => (
          <button key={label} type="button" aria-pressed={state === value} onClick={() => setFilter('state', value)}>
            {label}
          </button>
        ))}
      </div>

      {notice && (
        <div className={`notice${notice.ok ? '' : ' error'}`} role={notice.ok ? 'status' : 'alert'}>
          <span className="notice-glyph" aria-hidden="true">
            {notice.ok ? '✓' : '!'}
          </span>
          <div className="notice-detail">{notice.text}</div>
        </div>
      )}

      <div className="panel">
        <div className="panel-head">
          <span>{state === 'dead' ? 'Dead letters' : 'Deliveries'}</span>
          <div className="panel-head-controls" ref={setTableControls} />
        </div>
        <div className="panel-body form-row" style={{ alignItems: 'end' }}>
          <label className="form-field">
            <span className="form-label">channel</span>
            <select value={channel} onChange={(e) => setFilter('channel', e.target.value)} aria-label="Filter by channel">
              <option value="">All channels</option>
              {(channels.data?.channels ?? []).map((c) => (
                <option key={c.id} value={c.name}>
                  {c.name}
                </option>
              ))}
              {channel && !(channels.data?.channels ?? []).some((c) => c.name === channel) && (
                <option value={channel}>{channel}</option>
              )}
            </select>
          </label>
          <form
            className="form-field"
            onSubmit={(e) => {
              e.preventDefault();
              setFilter('run_id', runDraft.trim());
            }}
          >
            <span className="form-label">run id</span>
            <div className="row" style={{ gap: 'var(--space-3)' }}>
              <input value={runDraft} onChange={(e) => setRunDraft(e.target.value)} className="mono" placeholder="run UUID" aria-label="Filter by run id" />
              <button type="submit">Apply</button>
            </div>
          </form>
        </div>
        <EventsPanel
          query={deliveries}
          what="notification deliveries"
          feature="Notifications"
          permission="notifications.view"
          isEmpty={(d) => d.deliveries.length === 0}
          empty={
            <Empty
              title={state === 'dead' ? 'No dead letters' : 'No deliveries match'}
              detail={
                state === 'dead'
                  ? 'Every message that was attempted either went through or is still retrying.'
                  : 'Deliveries appear when a rule matches a run event, or when a channel is tested.'
              }
            />
          }
        >
          {(d) => (
            <DataTable
              controlsTarget={tableControls}
              rows={d.deliveries}
              columns={columns}
              rowKey={(x) => String(x.id)}
              noun="deliveries"
              count={{ kind: 'unknown' }}
              paging={{ mode: 'client' }}
              defaultPageSize={50}
            />
          )}
        </EventsPanel>
        <div className="panel-body row" style={{ gap: 'var(--space-3)' }}>
          <button type="button" disabled={cursors.length === 0} onClick={() => setCursors((c) => c.slice(0, -1))}>
            Newer
          </button>
          <button
            type="button"
            disabled={!page?.nextBefore}
            onClick={() => page?.nextBefore && setCursors((c) => [...c, page.nextBefore!])}
          >
            Older
          </button>
          <span className="faint">
            {cursors.length === 0 ? 'Newest first' : `Page ${cursors.length + 1}`}
            {!page?.nextBefore && page ? ' · no older deliveries' : ''}
          </span>
        </div>
      </div>
    </div>
  );
}
