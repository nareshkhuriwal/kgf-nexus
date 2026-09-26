// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One webhook trigger: its contract, its secret lifecycle, and every recent
 * call it received — accepted, deduplicated or rejected, with the reason.
 *
 * The recent-deliveries table is the debugging surface for "my webhook does
 * nothing": a rejected row says why (bad signature, stale timestamp, replay,
 * disabled), which is otherwise only visible in the audit log.
 *
 * Rotation offers a grace period during which the previous secret still
 * verifies, so a sender can be updated without dropping calls. The new secret
 * is shown once, exactly like on create.
 */

import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import {
  errorMessage,
  useDeleteTrigger,
  useRotateTriggerSecret,
  useTrigger,
  useTriggerDeliveries,
  useUpdateTrigger,
} from '../api/hooks';
import type { TriggerDelivery, TriggerWithSecret, WebhookTrigger } from '../api/types';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { DataTable, type Column } from '../components/DataTable';
import { Permit } from '../components/Permissions';
import { Empty, ErrorBox, Loading, StaleWarning } from '../components/Status';
import {
  EnabledBadge,
  EventsError,
  EventsPanel,
  OneTimeSecret,
  conflictHint,
} from '../components/EventsShared';
import { EMPTY, fmtAge, fmtTs, shortId } from '../lib/format';
import { TriggerForm, TriggerSecretDetails, TriggerUrlSource, triggerUrl, type TriggerUrl } from './TriggersPage';

export const GRACE_CHOICES: { seconds: number; label: string }[] = [
  { seconds: 0, label: 'None — the old secret stops working now' },
  { seconds: 3600, label: '1 hour' },
  { seconds: 86400, label: '24 hours (default)' },
  { seconds: 259200, label: '3 days' },
  { seconds: 604800, label: '7 days (maximum)' },
];

const OUTCOME_TONE: Record<string, string> = {
  accepted: 'tone-success',
  deduplicated: 'tone-neutral',
  rejected: 'tone-danger',
};

export function TriggerDetailPage({ namespace }: { namespace: string }) {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const trigger = useTrigger(namespace, name);
  const deliveries = useTriggerDeliveries(namespace, name, trigger.data !== undefined);
  const update = useUpdateTrigger(namespace);
  const remove = useDeleteTrigger(namespace);
  const rotate = useRotateTriggerSecret(namespace);
  const [editing, setEditing] = useState(false);
  const [rotating, setRotating] = useState(false);
  const [grace, setGrace] = useState(86400);
  const [deleting, setDeleting] = useState(false);
  const [issued, setIssued] = useState<TriggerWithSecret | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const now = Date.now();

  const back = (
    <div className="detail-actions detail-back-only">
      <Link to="/triggers" className="btn">
        ← Back to triggers
      </Link>
    </div>
  );

  if (trigger.error && trigger.data === undefined) {
    return (
      <div className="col">
        {back}
        <EventsError
          error={trigger.error}
          what={`trigger ${name}`}
          feature="Webhook triggers"
          permission="triggers.view"
          onRetry={() => void trigger.refetch()}
        />
      </div>
    );
  }
  if (!trigger.data) {
    return (
      <div className="col">
        {back}
        <Loading rows={8} label="Loading trigger" />
      </div>
    );
  }
  const t = trigger.data;

  const columns: Column<TriggerDelivery>[] = [
    {
      key: 'received',
      header: 'received',
      cellClass: 'nowrap',
      cell: (d) => <span title={fmtTs(d.received_at)}>{fmtAge(d.received_at, now)}</span>,
    },
    {
      key: 'outcome',
      header: 'outcome',
      cell: (d) => <span className={`badge ${OUTCOME_TONE[d.outcome] ?? 'tone-neutral'}`}>{d.outcome}</span>,
    },
    { key: 'status', header: 'HTTP', align: 'end', cell: (d) => d.http_status },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      cell: (d) => (d.run_id ? <Link to={`/runs/${encodeURIComponent(d.run_id)}`}>{shortId(d.run_id)}</Link> : <span className="faint">{EMPTY}</span>),
    },
    { key: 'key', header: 'idempotency key', cellClass: 'mono', cell: (d) => d.idempotency_key || <span className="faint">{EMPTY}</span> },
    { key: 'detail', header: 'detail', cell: (d) => d.detail || <span className="faint">{EMPTY}</span> },
    { key: 'digest', header: 'signature', cellClass: 'mono faint', cell: (d) => d.signature_digest },
  ];

  const url = triggerUrl(t);
  const graceActive = t.previous_secret_expires_at && Date.parse(t.previous_secret_expires_at) > now;

  return (
    <div className="col">
      <div className="detail-head">
        <div className="detail-head-text">
          <div className="detail-title-row">
            <EnabledBadge enabled={t.enabled} />
            <h1 className="detail-title mono">{t.name}</h1>
          </div>
          <div className="detail-meta">
            starts <code>{t.workflow}</code>
            {t.workflow_version != null ? ` v${t.workflow_version}` : ' (latest version)'} · created by {t.created_by}
          </div>
        </div>
        <div className="detail-actions">
          <Link to="/triggers" className="btn">
            ← Back to triggers
          </Link>
          <Permit permission="triggers.update">
            <button type="button" disabled={editing} onClick={() => setEditing(true)}>
              Edit
            </button>
            <button
              type="button"
              disabled={update.isPending}
              onClick={() => {
                setActionError(null);
                update.mutate(
                  { name: t.name, body: { enabled: !t.enabled, revision: t.revision } },
                  { onError: (e) => setActionError(conflictHint(e) ?? errorMessage(e)) },
                );
              }}
            >
              {t.enabled ? 'Disable' : 'Enable'}
            </button>
            <button type="button" onClick={() => setRotating((v) => !v)} aria-expanded={rotating}>
              Rotate secret
            </button>
          </Permit>
          <Permit permission="triggers.delete">
            <button type="button" className="danger" onClick={() => setDeleting(true)}>
              Delete
            </button>
          </Permit>
        </div>
      </div>

      {deleting && (
        <ConfirmDialog
          title={`Delete trigger ${t.name}?`}
          confirmLabel="Delete"
          tone="danger"
          pending={remove.isPending}
          onCancel={() => setDeleting(false)}
          onConfirm={() =>
            remove.mutate(t.name, {
              onSuccess: () => navigate('/triggers'),
              onError: (e) => {
                setDeleting(false);
                setActionError(errorMessage(e));
              },
            })
          }
        >
          Its URL stops working immediately and senders get 404. Runs it already started are not affected.
        </ConfirmDialog>
      )}

      {actionError && <ErrorBox error={actionError} what="that action" />}
      {trigger.error != null && <StaleWarning error={trigger.error} />}

      {issued?.secret && (
        <OneTimeSecret title={`New secret for ${issued.name}`} secret={issued.secret} onDismiss={() => setIssued(null)}>
          {issued.previous_secret_expires_at && (
            <p className="form-hint">
              The previous secret keeps verifying until {fmtTs(issued.previous_secret_expires_at)}. Update the sender before then.
            </p>
          )}
          <TriggerSecretDetails trigger={issued} />
        </OneTimeSecret>
      )}

      {rotating && (
        <Permit permission="triggers.update">
          <div className="panel">
            <div className="panel-head">
              <span>Rotate secret</span>
            </div>
            <form
              className="panel-body form-body"
              onSubmit={(e) => {
                e.preventDefault();
                rotate.mutate(
                  { name: t.name, graceSeconds: grace },
                  {
                    onSuccess: (out) => {
                      setRotating(false);
                      setIssued(out);
                    },
                  },
                );
              }}
            >
              <label className="form-field">
                <span className="form-label">keep accepting the old secret for</span>
                <select value={grace} onChange={(e) => setGrace(Number(e.target.value))} aria-label="Grace period">
                  {GRACE_CHOICES.map((g) => (
                    <option key={g.seconds} value={g.seconds}>
                      {g.label}
                    </option>
                  ))}
                </select>
                <span className="form-hint">
                  During the grace period both secrets verify, so senders can be switched over without dropped calls.
                </span>
              </label>
              {rotate.error != null && <ErrorBox error={rotate.error} what="the rotation" />}
              <div className="form-actions">
                <button type="submit" className="primary" disabled={rotate.isPending}>
                  {rotate.isPending ? 'Rotating…' : 'Issue new secret'}
                </button>
                <button type="button" onClick={() => setRotating(false)} disabled={rotate.isPending}>
                  Cancel
                </button>
              </div>
            </form>
          </div>
        </Permit>
      )}

      {editing && (
        <Permit permission="triggers.update">
          <TriggerForm
            namespace={namespace}
            trigger={t}
            pending={update.isPending}
            error={update.error}
            onCancel={() => {
              update.reset();
              setEditing(false);
            }}
            onSubmit={(body) => update.mutate({ name: t.name, body }, { onSuccess: () => setEditing(false) })}
          />
        </Permit>
      )}

      <div className="grid-2">
        <div className="panel">
          <div className="panel-head">
            <span>configuration</span>
          </div>
          <div className="panel-body">
            <TriggerFacts trigger={t} url={url} />
          </div>
        </div>
        <div className="panel">
          <div className="panel-head">
            <span>secret</span>
          </div>
          <div className="panel-body">
            <dl className="kv">
              <dt>last rotated</dt>
              <dd>{t.secret_rotated_at ? fmtTs(t.secret_rotated_at) : 'never (original secret)'}</dd>
              <dt>previous secret</dt>
              <dd>
                {graceActive ? (
                  <span className="chip warn">accepted until {fmtTs(t.previous_secret_expires_at)}</span>
                ) : (
                  'not accepted'
                )}
              </dd>
              <dt>scheme</dt>
              <dd>{t.signature_scheme}</dd>
            </dl>
            <p className="form-hint">
              The secret itself is never shown after it is issued. Rotate it to get a new one.
            </p>
          </div>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>recent deliveries</span>
          <span className="spacer" />
          <span className="faint">refreshes every 5s</span>
        </div>
        <EventsPanel
          query={deliveries}
          what="trigger deliveries"
          feature="Webhook triggers"
          permission="triggers.view"
          isEmpty={(rows) => rows.length === 0}
          empty={<Empty title="No calls received yet" detail="Signed requests to this trigger's URL appear here, including rejected ones and the reason." />}
        >
          {(rows) => <DataTable rows={rows} columns={columns} rowKey={(d) => `${d.received_at}:${d.signature_digest}`} noun="deliveries" count={{ kind: 'unknown' }} />}
        </EventsPanel>
      </div>
    </div>
  );
}

function TriggerFacts({ trigger: t, url }: { trigger: WebhookTrigger; url: TriggerUrl }) {
  const mapping = Object.entries(t.params_mapping ?? {});
  return (
    <dl className="kv">
      <dt>URL</dt>
      <dd>
        <span className="mono">{url.value}</span>
        <div>
          <TriggerUrlSource url={url} />
        </div>
      </dd>
      <dt>idempotency key</dt>
      <dd>{t.idempotency_key_path || <span className="faint">none — every signed call is a new run</span>}</dd>
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
      <dt>priority</dt>
      <dd>{t.priority}</dd>
      <dt>tolerance</dt>
      <dd>{t.tolerance_seconds}s</dd>
      <dt>fired</dt>
      <dd>
        {t.fire_count} time{t.fire_count === 1 ? '' : 's'}
        {t.last_fired_at ? `, last ${fmtTs(t.last_fired_at)}` : ''}
      </dd>
      <dt>created</dt>
      <dd>{fmtTs(t.created_at)}</dd>
    </dl>
  );
}
