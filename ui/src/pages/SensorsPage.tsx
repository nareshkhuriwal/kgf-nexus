// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Sensors: runs started by something Nexus observed — a new object under a
 * bucket prefix, or another workflow's run succeeding.
 *
 * The status column is the point of the page, and every value of it is said in
 * words. `waiting` and `stale` in particular look harmless as bare badges but
 * mean the opposite of "fine": nothing is polling the sensor, so nothing will
 * ever trigger, and the cause is outside this sensor (no workflow-manager
 * replica is running the sensor loop).
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { errorMessage, useCreateSensor, useDeleteSensor, useSensors, useUpdateSensor } from '../api/hooks';
import type { Sensor, SensorKind, SensorRequest } from '../api/types';
import { DataTable, type Column } from '../components/DataTable';
import { PageHeader } from '../components/PageHeader';
import { Forbidden } from '../components/Forbidden';
import { Permit, usePermission } from '../components/Permissions';
import { ActionIcon, RowAction } from '../components/RowAction';
import { Empty, ErrorBox } from '../components/Status';
import { WorkflowPicker } from '../components/WorkflowPicker';
import { EventsPanel, conflictHint, mappingText, parseMapping } from '../components/EventsShared';
import { fmtAge, fmtTs } from '../lib/format';

const STATUS_TONE: Record<string, string> = {
  disabled: 'tone-stopped',
  waiting: 'tone-warning',
  initializing: 'tone-queued',
  ok: 'tone-success',
  error: 'tone-danger',
  stale: 'tone-danger',
};

/** The honest sentence behind each status. */
export function sensorStatusText(s: Sensor): string {
  switch (s.status) {
    case 'disabled':
      return 'Disabled. No replica polls it until it is enabled again.';
    case 'waiting':
      return 'Enabled, but no workflow-manager replica has picked it up yet. If this lasts more than a poll interval, check that the workflow-manager is running with sensors enabled.';
    case 'initializing':
      return s.backfill
        ? 'Starting: the first pass is triggering runs for everything already present (backfill is on).'
        : 'Starting: the first pass is recording a baseline of what already exists. Those items will not trigger runs; only new arrivals will.';
    case 'ok':
      return 'Polling normally.';
    case 'error':
      return `The last poll failed${s.state?.consecutive_failures ? ` (${s.state.consecutive_failures} in a row)` : ''}: ${s.state?.last_error || 'no error text was recorded'}. It retries on the next interval.`;
    case 'stale':
      return 'No replica has polled it for over five minutes and none holds its lease. The workflow-manager may be down or not running the sensor loop; nothing will trigger until one picks it up.';
    default:
      return `Status reported as "${s.status}".`;
  }
}

export function SensorStatusBadge({ sensor }: { sensor: Sensor }) {
  return (
    <span className={`badge ${STATUS_TONE[sensor.status] ?? 'tone-neutral'}`} title={sensorStatusText(sensor)}>
      {sensor.status}
    </span>
  );
}

/** What the sensor watches, in one line. */
export function sensorSource(s: Pick<Sensor, 'kind' | 'config'>): string {
  const cfg = s.config ?? {};
  if (s.kind === 'object_store') {
    const uri = typeof cfg.uri === 'string' ? cfg.uri : '?';
    return typeof cfg.pattern === 'string' && cfg.pattern ? `${uri} (${cfg.pattern})` : uri;
  }
  return `succeeded runs of ${typeof cfg.upstream_workflow === 'string' ? cfg.upstream_workflow : '?'}`;
}

export function SensorsPage({ namespace }: { namespace: string }) {
  const can = usePermission();
  const sensors = useSensors(namespace);
  const create = useCreateSensor(namespace);
  const update = useUpdateSensor(namespace);
  const remove = useDeleteSensor(namespace);
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [tableControls, setTableControls] = useState<HTMLDivElement | null>(null);
  const now = Date.now();

  if (!can('sensors.view')) {
    return (
      <div className="col">
        <PageHeader title="Sensors" />
        <Forbidden what="view sensors" detail="This page needs the sensors.view permission. Ask your administrator to add it to one of your roles." />
      </div>
    );
  }

  const columns: Column<Sensor>[] = [
    {
      key: 'status',
      header: 'status',
      sort: (a, b) => a.status.localeCompare(b.status),
      cell: (s) => <SensorStatusBadge sensor={s} />,
    },
    {
      key: 'name',
      header: 'name',
      sort: (a, b) => a.name.localeCompare(b.name),
      cell: (s) => (
        <Link to={`/sensors/${encodeURIComponent(s.name)}`} className="mono">
          {s.name}
        </Link>
      ),
    },
    {
      key: 'source',
      header: 'watches',
      cell: (s) => (
        <span className="cell-stack">
          <span className="mono">{sensorSource(s)}</span>
          <span className="cell-sub">
            {s.kind === 'object_store' ? 'object store' : 'upstream run'} · every {s.poll_interval_seconds}s
          </span>
        </span>
      ),
    },
    { key: 'workflow', header: 'starts', cellClass: 'mono', sort: (a, b) => a.workflow.localeCompare(b.workflow), cell: (s) => s.workflow },
    {
      key: 'activity',
      header: 'triggered / observed',
      align: 'end',
      cell: (s) =>
        s.state ? (
          <span className="cell-stack">
            <span>
              {s.state.triggered_total} / {s.state.observed_total}
            </span>
            {s.state.pending > 0 && <span className="cell-sub">{s.state.pending} pending</span>}
          </span>
        ) : (
          <span className="faint">—</span>
        ),
    },
    {
      key: 'last',
      header: 'last success',
      cellClass: 'nowrap',
      cell: (s) =>
        s.state?.last_success_at ? (
          <span title={fmtTs(s.state.last_success_at)}>{fmtAge(s.state.last_success_at, now)}</span>
        ) : (
          <span className="faint">never</span>
        ),
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (s) => (
        <div className="cell-actions">
          <Permit permission="sensors.update">
            <button
              type="button"
              className="icon-action"
              aria-label={s.enabled ? `Disable sensor ${s.name}` : `Enable sensor ${s.name}`}
              title={s.enabled ? 'Disable' : 'Enable'}
              disabled={update.isPending && update.variables?.name === s.name}
              onClick={() => {
                setActionError(null);
                update.mutate(
                  { name: s.name, body: { enabled: !s.enabled } },
                  { onError: (e) => setActionError(errorMessage(e)) },
                );
              }}
            >
              <ActionIcon name={s.enabled ? 'disable' : 'enable'} />
            </button>
          </Permit>
          <RowAction
            permission="sensors.delete"
            label={`Delete sensor ${s.name}`}
            icon="delete"
            confirm={`Delete sensor ${s.name}? It stops watching and its observation history is removed. Runs it started are not affected.`}
            onAction={() => remove.mutateAsync(s.name)}
          />
        </div>
      ),
    },
  ];

  return (
    <div className="col">
      <PageHeader
        title="Sensors"
        subtitle={
          <>
            runs started by what Nexus observes in <code>{namespace}</code> · refreshes every 10s
          </>
        }
        actions={
          <Permit permission="sensors.create">
            <button
              type="button"
              className="primary"
              onClick={() => {
                create.reset();
                setCreating((v) => !v);
              }}
            >
              {creating ? 'Cancel' : 'New sensor'}
            </button>
          </Permit>
        }
      />
      {actionError && <ErrorBox error={actionError} what="that change" />}
      {creating && (
        <Permit permission="sensors.create">
          <SensorForm
            namespace={namespace}
            sensor={null}
            pending={create.isPending}
            error={create.error}
            onCancel={() => setCreating(false)}
            onSubmit={(body) => create.mutate(body, { onSuccess: () => setCreating(false) })}
          />
        </Permit>
      )}
      <div className="panel">
        <div className="panel-head">
          <span>Sensors</span>
          <div className="panel-head-controls" ref={setTableControls} />
        </div>
        <EventsPanel
          query={sensors}
          what="sensors"
          feature="Sensors"
          permission="sensors.view"
          isEmpty={(rows) => rows.length === 0}
          empty={
            <Empty
              title="No sensors in this namespace"
              detail="A sensor watches an object-store prefix for new files, or another workflow for successful runs, and starts a run for each new thing it sees."
            />
          }
        >
          {(rows) => (
            <DataTable
              controlsTarget={tableControls}
              rows={rows}
              columns={columns}
              rowKey={(s) => s.id}
              noun="sensors"
              count={{ kind: 'exact' }}
              search={{ mode: 'client', fields: (s) => [s.name, s.workflow, s.status, sensorSource(s)] }}
            />
          )}
        </EventsPanel>
      </div>
    </div>
  );
}

export function SensorForm({
  namespace,
  sensor,
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  namespace: string;
  sensor: Sensor | null;
  pending: boolean;
  error: unknown;
  onSubmit: (body: SensorRequest) => void;
  onCancel: () => void;
}) {
  const cfg = sensor?.config ?? {};
  const [name, setName] = useState(sensor?.name ?? '');
  const [kind, setKind] = useState<SensorKind>(sensor?.kind ?? 'object_store');
  const [uri, setUri] = useState(typeof cfg.uri === 'string' ? cfg.uri : '');
  const [pattern, setPattern] = useState(typeof cfg.pattern === 'string' ? cfg.pattern : '');
  const [upstream, setUpstream] = useState(typeof cfg.upstream_workflow === 'string' ? cfg.upstream_workflow : '');
  const [workflow, setWorkflow] = useState(sensor?.workflow ?? '');
  const [mapping, setMapping] = useState(mappingText(sensor?.params_mapping));
  const [pollInterval, setPollInterval] = useState(String(sensor?.poll_interval_seconds ?? 60));
  const [backfill, setBackfill] = useState(sensor?.backfill ?? false);
  const [enabled, setEnabled] = useState(sensor?.enabled ?? true);
  const parsed = parseMapping(mapping);
  const intervalNum = Number(pollInterval);
  const intervalInvalid = !Number.isInteger(intervalNum) || intervalNum < 5 || intervalNum > 86400;
  const uriInvalid = kind === 'object_store' && uri.trim() !== '' && !/^[a-z0-9]+:\/\/[^/]+/.test(uri.trim());

  function submit() {
    const config: Record<string, unknown> =
      kind === 'object_store'
        ? { uri: uri.trim(), ...(pattern.trim() ? { pattern: pattern.trim() } : {}) }
        : { upstream_workflow: upstream.trim() };
    const body: SensorRequest = {
      workflow: workflow.trim(),
      config,
      params_mapping: parsed.value,
      poll_interval_seconds: intervalNum,
      backfill,
      enabled,
    };
    if (sensor) body.revision = sensor.revision;
    else {
      body.name = name.trim();
      body.kind = kind;
    }
    onSubmit(body);
  }

  const itemHint =
    kind === 'object_store'
      ? <>The object is under <code>$.object</code>: <code>uri</code>, <code>bucket</code>, <code>key</code>, <code>relative_key</code>, <code>size</code>, <code>etag</code>, <code>last_modified</code>.</>
      : <>The upstream run is under <code>$.run</code>: <code>id</code>, <code>workflow</code>, <code>params</code>, <code>ended_at</code>, <code>logical_date</code>.</>;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>{sensor ? `Edit sensor ${sensor.name}` : 'New sensor'}</span>
      </div>
      <form
        className="panel-body form-body"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <div className="form-row">
          <label className="form-field">
            <span className="form-label">name</span>
            <input value={name} readOnly={!!sensor} onChange={(e) => setName(e.target.value)} placeholder="landing-files" pattern="[a-z0-9][a-z0-9_.\-]{0,62}" required />
          </label>
          <fieldset className="form-fieldset">
            <legend>watches</legend>
            <label className="form-check">
              <input type="radio" name="sensor-kind" checked={kind === 'object_store'} disabled={!!sensor} onChange={() => setKind('object_store')} />
              <span>New objects in an object store</span>
            </label>
            <label className="form-check">
              <input type="radio" name="sensor-kind" checked={kind === 'run_succeeded'} disabled={!!sensor} onChange={() => setKind('run_succeeded')} />
              <span>Successful runs of another workflow</span>
            </label>
            {sensor && <span className="form-hint">A sensor&apos;s kind cannot change.</span>}
          </fieldset>
        </div>

        {kind === 'object_store' ? (
          <div className="form-row">
            <label className="form-field">
              <span className="form-label">URI</span>
              <input value={uri} onChange={(e) => setUri(e.target.value)} className="mono" placeholder="s3://landing/incoming/" required aria-label="object store URI" />
              {uriInvalid ? (
                <span className="form-hint invalid" role="alert">
                  Use scheme://bucket/prefix/, for example s3://landing/incoming/.
                </span>
              ) : (
                <span className="form-hint">Bucket and prefix to list.</span>
              )}
            </label>
            <label className="form-field">
              <span className="form-label">pattern</span>
              <input value={pattern} onChange={(e) => setPattern(e.target.value)} className="mono" placeholder="*.parquet" />
              <span className="form-hint">Optional glob on the key. Blank matches every object.</span>
            </label>
          </div>
        ) : (
          <WorkflowPicker namespace={namespace} value={upstream} onChange={setUpstream} label="upstream workflow" />
        )}

        <div className="form-row">
          <WorkflowPicker namespace={namespace} value={workflow} onChange={setWorkflow} label="starts workflow" />
          <label className="form-field">
            <span className="form-label">poll interval (seconds)</span>
            <input type="number" min={5} max={86400} value={pollInterval} onChange={(e) => setPollInterval(e.target.value)} required />
            {intervalInvalid && (
              <span className="form-hint invalid" role="alert">
                Between 5 seconds and one day (86400).
              </span>
            )}
          </label>
        </div>

        <label className="form-field">
          <span className="form-label">params mapping (JSON)</span>
          <textarea value={mapping} onChange={(e) => setMapping(e.target.value)} rows={3} className="mono" aria-label="params mapping" placeholder={kind === 'object_store' ? '{ "path": "$.object.uri" }' : '{ "upstream_run": "$.run.id" }'} />
          {parsed.error ? (
            <span className="form-hint invalid" role="alert">
              Not valid: {parsed.error}
            </span>
          ) : (
            <span className="form-hint">JSONPath, <code>{'{{ $.path }}'}</code> templates or literals. {itemHint}</span>
          )}
        </label>

        <label className="form-check">
          <input type="checkbox" checked={backfill} onChange={(e) => setBackfill(e.target.checked)} />
          <span>
            Backfill:{' '}
            {backfill
              ? 'trigger a run for everything already present, then for each new arrival.'
              : 'record what already exists as a baseline without triggering; only new arrivals start runs.'}
          </span>
        </label>
        <label className="form-check">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          <span>Enabled</span>
        </label>

        {error != null && <ErrorBox error={conflictHint(error) ?? error} what={sensor ? 'the sensor changes' : 'the new sensor'} />}

        <div className="form-actions">
          <button
            type="submit"
            className="primary"
            disabled={
              pending ||
              parsed.error !== null ||
              intervalInvalid ||
              uriInvalid ||
              workflow.trim() === '' ||
              (kind === 'run_succeeded' && upstream.trim() === '')
            }
          >
            {pending ? 'Saving…' : sensor ? 'Save changes' : 'Create sensor'}
          </button>
          <button type="button" onClick={onCancel} disabled={pending}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
