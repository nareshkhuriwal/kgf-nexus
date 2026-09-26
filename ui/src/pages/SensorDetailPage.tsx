// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One sensor: its status in words, the poll loop's own state (lease, cursor,
 * last error), and what it has observed.
 *
 * Baseline observations are hidden by default: on a sensor started without
 * backfill they are every object that already existed, which can be thousands
 * of rows that never triggered anything and bury the ones that did. The toggle
 * brings them back for "why did this file not start a run?".
 */

import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import {
  errorMessage,
  useDeleteSensor,
  useSensor,
  useSensorObservations,
  useUpdateSensor,
} from '../api/hooks';
import type { Sensor, SensorObservation } from '../api/types';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { DataTable, type Column } from '../components/DataTable';
import { Permit } from '../components/Permissions';
import { RowDetails } from '../components/RowAction';
import { Empty, ErrorBox, Loading, StaleWarning } from '../components/Status';
import { EventsError, EventsPanel } from '../components/EventsShared';
import { EMPTY, fmtAge, fmtTs, shortId } from '../lib/format';
import { SensorForm, SensorStatusBadge, sensorSource, sensorStatusText } from './SensorsPage';

const OUTCOME_TONE: Record<string, string> = {
  pending: 'tone-queued',
  triggered: 'tone-success',
  baseline: 'tone-neutral',
  failed: 'tone-danger',
};

export function SensorDetailPage({ namespace }: { namespace: string }) {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const sensor = useSensor(namespace, name);
  const [includeBaseline, setIncludeBaseline] = useState(false);
  const observations = useSensorObservations(namespace, sensor.data ? name : '', includeBaseline);
  const update = useUpdateSensor(namespace);
  const remove = useDeleteSensor(namespace);
  const [editing, setEditing] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const now = Date.now();

  const back = (
    <div className="detail-actions detail-back-only">
      <Link to="/sensors" className="btn">
        ← Back to sensors
      </Link>
    </div>
  );

  if (sensor.error && sensor.data === undefined) {
    return (
      <div className="col">
        {back}
        <EventsError error={sensor.error} what={`sensor ${name}`} feature="Sensors" permission="sensors.view" onRetry={() => void sensor.refetch()} />
      </div>
    );
  }
  if (!sensor.data) {
    return (
      <div className="col">
        {back}
        <Loading rows={8} label="Loading sensor" />
      </div>
    );
  }
  const s = sensor.data;
  const bad = s.status === 'error' || s.status === 'stale' || s.status === 'waiting';

  const columns: Column<SensorObservation>[] = [
    {
      key: 'observed',
      header: 'observed',
      cellClass: 'nowrap',
      cell: (o) => <span title={fmtTs(o.observed_at)}>{fmtAge(o.observed_at, now)}</span>,
    },
    { key: 'item', header: 'item', cellClass: 'mono', cell: (o) => o.item_key },
    {
      key: 'outcome',
      header: 'outcome',
      cell: (o) => (
        <span
          className={`badge ${OUTCOME_TONE[o.outcome] ?? 'tone-neutral'}`}
          title={o.outcome === 'baseline' ? 'Existed when the sensor started; recorded without a run.' : undefined}
        >
          {o.outcome}
        </span>
      ),
    },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      cell: (o) => (o.run_id ? <Link to={`/runs/${encodeURIComponent(o.run_id)}`}>{shortId(o.run_id)}</Link> : <span className="faint">{EMPTY}</span>),
    },
    { key: 'attempts', header: 'attempts', align: 'end', cell: (o) => o.attempts },
    {
      key: 'error',
      header: 'last error',
      cell: (o) => (o.last_error ? <span style={{ color: 'var(--tone-danger-fg)' }}>{o.last_error}</span> : <span className="faint">{EMPTY}</span>),
    },
    { key: 'details', header: '', cell: (o) => <RowDetails label={`View observation ${o.item_key}`} value={o} /> },
  ];

  return (
    <div className="col">
      <div className="detail-head">
        <div className="detail-head-text">
          <div className="detail-title-row">
            <SensorStatusBadge sensor={s} />
            <h1 className="detail-title mono">{s.name}</h1>
          </div>
          <div className="detail-meta">
            watches <code>{sensorSource(s)}</code> · starts <code>{s.workflow}</code> · created by {s.created_by}
          </div>
        </div>
        <div className="detail-actions">
          <Link to="/sensors" className="btn">
            ← Back to sensors
          </Link>
          <Permit permission="sensors.update">
            <button type="button" disabled={editing} onClick={() => setEditing(true)}>
              Edit
            </button>
            <button
              type="button"
              disabled={update.isPending}
              onClick={() => {
                setActionError(null);
                update.mutate({ name: s.name, body: { enabled: !s.enabled } }, { onError: (e) => setActionError(errorMessage(e)) });
              }}
            >
              {s.enabled ? 'Disable' : 'Enable'}
            </button>
          </Permit>
          <Permit permission="sensors.delete">
            <button type="button" className="danger" onClick={() => setDeleting(true)}>
              Delete
            </button>
          </Permit>
        </div>
      </div>

      {deleting && (
        <ConfirmDialog
          title={`Delete sensor ${s.name}?`}
          confirmLabel="Delete"
          tone="danger"
          pending={remove.isPending}
          onCancel={() => setDeleting(false)}
          onConfirm={() =>
            remove.mutate(s.name, {
              onSuccess: () => navigate('/sensors'),
              onError: (e) => {
                setDeleting(false);
                setActionError(errorMessage(e));
              },
            })
          }
        >
          It stops watching and its observation history is removed. Runs it already started are not affected.
        </ConfirmDialog>
      )}

      {actionError && <ErrorBox error={actionError} what="that action" />}
      {sensor.error != null && <StaleWarning error={sensor.error} />}

      <div className={`notice${bad ? ' error' : ''}`} role={bad ? 'alert' : 'status'}>
        <span className="notice-glyph" aria-hidden="true">
          {bad ? '!' : 'i'}
        </span>
        <div>
          <div className="notice-title">Status: {s.status}</div>
          <div className="notice-detail">{sensorStatusText(s)}</div>
          {s.state && !s.state.definition_current && (
            <div className="notice-hint">
              The poll loop is still running the previous definition; the latest edit applies from its next pass.
            </div>
          )}
        </div>
      </div>

      {editing && (
        <Permit permission="sensors.update">
          <SensorForm
            namespace={namespace}
            sensor={s}
            pending={update.isPending}
            error={update.error}
            onCancel={() => {
              update.reset();
              setEditing(false);
            }}
            onSubmit={(body) => update.mutate({ name: s.name, body }, { onSuccess: () => setEditing(false) })}
          />
        </Permit>
      )}

      <div className="grid-2">
        <div className="panel">
          <div className="panel-head">
            <span>definition</span>
          </div>
          <div className="panel-body">
            <SensorFacts sensor={s} />
          </div>
        </div>
        <div className="panel">
          <div className="panel-head">
            <span>poll loop</span>
          </div>
          <div className="panel-body">
            {s.state ? (
              <dl className="kv">
                <dt>last poll</dt>
                <dd>{s.state.last_poll_at ? fmtTs(s.state.last_poll_at) : 'never'}</dd>
                <dt>last success</dt>
                <dd>{s.state.last_success_at ? fmtTs(s.state.last_success_at) : 'never'}</dd>
                <dt>next poll</dt>
                <dd>{s.state.next_poll_at ? fmtTs(s.state.next_poll_at) : EMPTY}</dd>
                <dt>lease</dt>
                <dd>
                  {s.state.lease_owner && s.state.lease_until && Date.parse(s.state.lease_until) > now
                    ? `${s.state.lease_owner} until ${fmtTs(s.state.lease_until)}`
                    : 'not held'}
                </dd>
                <dt>baseline</dt>
                <dd>{s.state.baseline_done ? 'recorded' : s.backfill ? 'not used (backfill)' : 'in progress'}</dd>
                <dt>{s.kind === 'object_store' ? 'cursor' : 'watermark'}</dt>
                <dd>{(s.kind === 'object_store' ? s.state.cursor : s.state.watermark) || EMPTY}</dd>
                <dt>observed</dt>
                <dd>
                  {s.state.observed_total} ({s.state.triggered_total} triggered, {s.state.pending} pending)
                </dd>
                <dt>failures</dt>
                <dd>{s.state.consecutive_failures} in a row</dd>
                <dt>last error</dt>
                <dd>{s.state.last_error || EMPTY}</dd>
              </dl>
            ) : (
              <Empty title="No poll state yet" detail="No workflow-manager replica has run this sensor, so there is no lease, cursor or history to show." />
            )}
          </div>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>observations</span>
          <span className="spacer" />
          <label className="form-check" style={{ margin: 0 }}>
            <input type="checkbox" checked={includeBaseline} onChange={(e) => setIncludeBaseline(e.target.checked)} />
            <span>Include baseline</span>
          </label>
        </div>
        <EventsPanel
          query={observations}
          what="sensor observations"
          feature="Sensors"
          permission="sensors.view"
          isEmpty={(rows) => rows.length === 0}
          empty={
            <Empty
              title="Nothing observed yet"
              detail={
                includeBaseline
                  ? 'The sensor has not recorded any items.'
                  : 'No item has triggered or is pending. Items recorded as the starting baseline are hidden; tick "Include baseline" to see them.'
              }
            />
          }
        >
          {(rows) => (
            <DataTable
              rows={rows}
              columns={columns}
              rowKey={(o) => o.item_key}
              noun="observations"
              count={{ kind: 'capped', limit: 100 }}
              search={{ mode: 'client', fields: (o) => [o.item_key, o.outcome, o.run_id] }}
            />
          )}
        </EventsPanel>
      </div>
    </div>
  );
}

function SensorFacts({ sensor: s }: { sensor: Sensor }) {
  const mapping = Object.entries(s.params_mapping ?? {});
  return (
    <dl className="kv">
      <dt>kind</dt>
      <dd>{s.kind === 'object_store' ? 'object store' : 'upstream run succeeded'}</dd>
      <dt>watches</dt>
      <dd>{sensorSource(s)}</dd>
      <dt>starts</dt>
      <dd>
        {s.workflow}
        {s.workflow_version != null ? ` v${s.workflow_version}` : ' (latest)'}
      </dd>
      <dt>poll interval</dt>
      <dd>{s.poll_interval_seconds}s</dd>
      <dt>backfill</dt>
      <dd>{s.backfill ? 'yes — existing items trigger' : 'no — existing items are baseline'}</dd>
      <dt>priority</dt>
      <dd>{s.priority}</dd>
      <dt>params</dt>
      <dd>
        {mapping.length === 0 ? (
          <span className="faint">none</span>
        ) : (
          mapping.map(([k, v]) => (
            <div key={k}>
              {k} ← {typeof v === 'string' ? v : JSON.stringify(v)}
            </div>
          ))
        )}
      </dd>
    </dl>
  );
}
