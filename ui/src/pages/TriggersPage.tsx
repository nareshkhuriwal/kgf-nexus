// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Webhook triggers: a signed HTTP call from outside starts a run.
 *
 * A trigger is a standing authorization to create runs, so its secret is the
 * whole of its security. The server returns the secret exactly once, on create
 * (and again on rotate); this page holds it only in component state, shows it
 * with the URL and a working signing example, and says plainly that it cannot
 * be shown again.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { errorMessage, useCreateTrigger, useDeleteTrigger, useTriggers, useUpdateTrigger } from '../api/hooks';
import type { TriggerRequest, TriggerWithSecret, WebhookTrigger } from '../api/types';
import { DataTable, type Column } from '../components/DataTable';
import { PageHeader } from '../components/PageHeader';
import { Forbidden } from '../components/Forbidden';
import { Permit, usePermission } from '../components/Permissions';
import { ActionIcon, RowAction } from '../components/RowAction';
import { Empty, ErrorBox } from '../components/Status';
import { WorkflowPicker } from '../components/WorkflowPicker';
import {
  CopyButton,
  EnabledBadge,
  EventsPanel,
  OneTimeSecret,
  conflictHint,
  mappingText,
  parseMapping,
} from '../components/EventsShared';
import { fmtAge, fmtTs } from '../lib/format';

export interface TriggerUrl {
  /** The absolute URL, or the bare path when the public address is unknown. */
  value: string;
  known: boolean;
  source: 'forwarded' | 'config' | 'unknown';
}

/**
 * The address a sender posts to, exactly as the gateway reported it (F156).
 *
 * The console never builds it from its own page origin: behind a proxy the
 * console and the hook endpoint can be served under different names, and a
 * guessed URL handed to a third party next to a fresh secret fails silently.
 * When the gateway does not know its public address the path is shown with a
 * warning instead.
 */
export function triggerUrl(t: Pick<WebhookTrigger, 'url' | 'url_source' | 'path'>): TriggerUrl {
  if (t.url) return { value: t.url, known: true, source: t.url_source === 'forwarded' ? 'forwarded' : 'config' };
  return { value: t.path, known: false, source: 'unknown' };
}

const URL_SOURCE_TEXT: Record<TriggerUrl['source'], string> = {
  forwarded: 'Reported by the gateway from the trusted proxy in front of it.',
  config: "Reported by the gateway from its configured public address (NEXUS_PUBLIC_URL).",
  unknown: '',
};

export function TriggerUrlSource({ url }: { url: TriggerUrl }) {
  return url.known ? <span className="form-hint">{URL_SOURCE_TEXT[url.source]}</span> : <UnknownUrlWarning />;
}

/** Shown wherever a hook URL would be, when the gateway cannot say what it is. */
export function UnknownUrlWarning() {
  return (
    <div className="notice warn" role="alert">
      <span className="notice-glyph" aria-hidden="true">
        !
      </span>
      <div>
        <div className="notice-title">The gateway&apos;s public address is unknown</div>
        <div className="notice-detail">
          Only the path is shown. Senders must post to it on the address the gateway is reachable at from outside, which
          this console cannot know. Set <code>NEXUS_PUBLIC_URL</code> on the gateway, or list the proxy in front of it in{' '}
          <code>NEXUS_TRUSTED_PROXIES</code>, and the full URL appears here.
        </div>
      </div>
    </div>
  );
}

export function signingExample(url: string): string {
  return [
    `SECRET='<the secret above>'`,
    `BODY='{"ref":"refs/heads/main","repository":{"name":"etl"}}'`,
    `TS=$(date +%s)`,
    `SIG=$(printf '%s.%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')`,
    `curl -sS -X POST '${url}' \\`,
    `  -H 'Content-Type: application/json' \\`,
    `  -H "X-Nexus-Timestamp: $TS" \\`,
    `  -H "X-Nexus-Signature: v1=$SIG" \\`,
    `  --data "$BODY"`,
  ].join('\n');
}

/** URL, scheme and example, shown with a freshly issued secret. */
export function TriggerSecretDetails({ trigger }: { trigger: WebhookTrigger }) {
  const url = triggerUrl(trigger);
  const example = signingExample(url.known ? url.value : `<public address>${url.value}`);
  return (
    <>
      <label className="form-field">
        <span className="form-label">webhook URL</span>
        <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <input readOnly value={url.value} className="mono" aria-label="Webhook URL" style={{ flex: '1 1 260px', minWidth: 0 }} />
          <CopyButton text={url.value} label={url.known ? 'Copy URL' : 'Copy path'} />
        </div>
      </label>
      <TriggerUrlSource url={url} />
      <div className="form-field">
        <span className="form-label">signing</span>
        <span className="form-hint">
          Send <code>X-Nexus-Timestamp: &lt;unix seconds&gt;</code> and{' '}
          <code>X-Nexus-Signature: v1=hex(HMAC-SHA256(secret, timestamp + &quot;.&quot; + body))</code>. A request
          older than {trigger.tolerance_seconds}s, or a replayed signature, is refused.
        </span>
        <pre className="mono" style={{ overflowX: 'auto', whiteSpace: 'pre', maxWidth: '100%' }} aria-label="Signing example">
          {example}
        </pre>
        <div>
          <CopyButton text={example} label="Copy example" />
        </div>
      </div>
    </>
  );
}

export function TriggersPage({ namespace }: { namespace: string }) {
  const can = usePermission();
  const triggers = useTriggers(namespace);
  const create = useCreateTrigger(namespace);
  const update = useUpdateTrigger(namespace);
  const remove = useDeleteTrigger(namespace);
  const [creating, setCreating] = useState(false);
  const [issued, setIssued] = useState<TriggerWithSecret | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [tableControls, setTableControls] = useState<HTMLDivElement | null>(null);
  const now = Date.now();

  if (!can('triggers.view')) {
    return (
      <div className="col">
        <PageHeader title="Webhook triggers" />
        <Forbidden what="view webhook triggers" detail="This page needs the triggers.view permission. Ask your administrator to add it to one of your roles." />
      </div>
    );
  }

  const columns: Column<WebhookTrigger>[] = [
    {
      key: 'status',
      header: 'status',
      sort: (a, b) => Number(b.enabled) - Number(a.enabled),
      cell: (t) => <EnabledBadge enabled={t.enabled} offTitle="Disabled: signed calls are refused and start nothing." />,
    },
    {
      key: 'name',
      header: 'name',
      sort: (a, b) => a.name.localeCompare(b.name),
      cell: (t) => (
        <Link to={`/triggers/${encodeURIComponent(t.name)}`} className="mono">
          {t.name}
        </Link>
      ),
    },
    {
      key: 'workflow',
      header: 'workflow',
      cellClass: 'mono',
      sort: (a, b) => a.workflow.localeCompare(b.workflow),
      cell: (t) => (
        <>
          {t.workflow}
          {t.workflow_version != null && <span className="faint"> v{t.workflow_version}</span>}
        </>
      ),
    },
    {
      key: 'fires',
      header: 'fires',
      align: 'end',
      sort: (a, b) => a.fire_count - b.fire_count,
      cell: (t) => t.fire_count,
    },
    {
      key: 'last',
      header: 'last fired',
      cellClass: 'nowrap',
      sort: (a, b) => Date.parse(a.last_fired_at ?? '') - Date.parse(b.last_fired_at ?? ''),
      cell: (t) =>
        t.last_fired_at ? <span title={fmtTs(t.last_fired_at)}>{fmtAge(t.last_fired_at, now)}</span> : <span className="faint">never</span>,
    },
    {
      key: 'secret',
      header: 'secret',
      cell: (t) =>
        t.previous_secret_expires_at && Date.parse(t.previous_secret_expires_at) > now ? (
          <span className="chip warn" title={`The previous secret is accepted until ${fmtTs(t.previous_secret_expires_at)}`}>
            rotating
          </span>
        ) : t.secret_rotated_at ? (
          <span className="faint" title={fmtTs(t.secret_rotated_at)}>
            rotated {fmtAge(t.secret_rotated_at, now)}
          </span>
        ) : (
          <span className="faint">original</span>
        ),
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (t) => (
        <div className="cell-actions">
          <Permit permission="triggers.update">
            <button
              type="button"
              className="icon-action"
              aria-label={t.enabled ? `Disable trigger ${t.name}` : `Enable trigger ${t.name}`}
              title={t.enabled ? 'Disable' : 'Enable'}
              disabled={update.isPending && update.variables?.name === t.name}
              onClick={() => {
                setActionError(null);
                update.mutate(
                  { name: t.name, body: { enabled: !t.enabled, revision: t.revision } },
                  { onError: (e) => setActionError(conflictHint(e) ?? errorMessage(e)) },
                );
              }}
            >
              <ActionIcon name={t.enabled ? 'disable' : 'enable'} />
            </button>
          </Permit>
          <RowAction
            permission="triggers.delete"
            label={`Delete trigger ${t.name}`}
            icon="delete"
            confirm={`Delete trigger ${t.name}? Its URL stops working immediately. Runs it already started are not affected.`}
            onAction={() => remove.mutateAsync(t.name)}
          />
        </div>
      ),
    },
  ];

  return (
    <div className="col">
      <PageHeader
        title="Webhook triggers"
        subtitle={
          <>
            signed HTTP calls that start runs in <code>{namespace}</code>
          </>
        }
        actions={
          <Permit permission="triggers.create">
            <button
              type="button"
              className="primary"
              onClick={() => {
                create.reset();
                setCreating((v) => !v);
              }}
            >
              {creating ? 'Cancel' : 'New trigger'}
            </button>
          </Permit>
        }
      />

      {actionError && <ErrorBox error={actionError} what="that change" />}

      {issued?.secret && (
        <OneTimeSecret title={`Secret for trigger ${issued.name}`} secret={issued.secret} onDismiss={() => setIssued(null)}>
          <TriggerSecretDetails trigger={issued} />
          <p className="form-hint">
            <Link to={`/triggers/${encodeURIComponent(issued.name)}`}>Open {issued.name}</Link> to watch its deliveries.
          </p>
        </OneTimeSecret>
      )}

      {creating && (
        <Permit permission="triggers.create">
          <TriggerForm
            namespace={namespace}
            trigger={null}
            pending={create.isPending}
            error={create.error}
            onCancel={() => setCreating(false)}
            onSubmit={(body) =>
              create.mutate(body, {
                onSuccess: (t) => {
                  setCreating(false);
                  setIssued(t);
                },
              })
            }
          />
        </Permit>
      )}

      <div className="panel">
        <div className="panel-head">
          <span>Triggers</span>
          <div className="panel-head-controls" ref={setTableControls} />
        </div>
        <EventsPanel
          query={triggers}
          what="webhook triggers"
          feature="Webhook triggers"
          permission="triggers.view"
          isEmpty={(rows) => rows.length === 0}
          empty={
            <Empty
              title="No webhook triggers in this namespace"
              detail="A trigger gives an outside system (a Git host, a CI job, a SaaS webhook) a signed URL that starts a workflow run, with params mapped from the request body."
            />
          }
        >
          {(rows) => (
            <DataTable
              controlsTarget={tableControls}
              rows={rows}
              columns={columns}
              rowKey={(t) => t.id}
              noun="triggers"
              count={{ kind: 'exact' }}
              search={{ mode: 'client', fields: (t) => [t.name, t.workflow] }}
            />
          )}
        </EventsPanel>
      </div>
    </div>
  );
}

export function TriggerForm({
  namespace,
  trigger,
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  namespace: string;
  trigger: WebhookTrigger | null;
  pending: boolean;
  error: unknown;
  onSubmit: (body: TriggerRequest) => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState(trigger?.name ?? '');
  const [workflow, setWorkflow] = useState(trigger?.workflow ?? '');
  const [version, setVersion] = useState(trigger?.workflow_version != null ? String(trigger.workflow_version) : '');
  const [mapping, setMapping] = useState(mappingText(trigger?.params_mapping));
  const [keyPath, setKeyPath] = useState(trigger?.idempotency_key_path ?? '');
  const [priority, setPriority] = useState(trigger ? String(trigger.priority) : '');
  const [tolerance, setTolerance] = useState(trigger ? String(trigger.tolerance_seconds) : '');
  const [enabled, setEnabled] = useState(trigger?.enabled ?? true);
  const parsed = parseMapping(mapping);

  function submit() {
    const body: TriggerRequest = {
      workflow: workflow.trim(),
      params_mapping: parsed.value,
      idempotency_key_path: keyPath.trim(),
      enabled,
    };
    if (!trigger) body.name = name.trim();
    else body.revision = trigger.revision;
    if (version.trim() !== '') body.workflow_version = Number(version);
    if (priority.trim() !== '') body.priority = Number(priority);
    if (tolerance.trim() !== '') body.tolerance_seconds = Number(tolerance);
    onSubmit(body);
  }

  return (
    <div className="panel">
      <div className="panel-head">
        <span>{trigger ? `Edit trigger ${trigger.name}` : 'New webhook trigger'}</span>
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
            <input value={name} readOnly={!!trigger} onChange={(e) => setName(e.target.value)} placeholder="github-push" pattern="[a-z0-9][a-z0-9_.\-]{0,62}" required />
          </label>
          <WorkflowPicker namespace={namespace} value={workflow} onChange={setWorkflow} />
        </div>
        <div className="form-row">
          <label className="form-field">
            <span className="form-label">workflow version</span>
            <input type="number" min={1} value={version} onChange={(e) => setVersion(e.target.value)} placeholder="latest" />
            <span className="form-hint">Blank always runs the latest version.</span>
          </label>
          <label className="form-field">
            <span className="form-label">idempotency key path</span>
            <input value={keyPath} onChange={(e) => setKeyPath(e.target.value)} className="mono" placeholder="$.payload.delivery_id" />
            <span className="form-hint">
              JSONPath into the request. Retries carrying the same value converge on one run. Blank makes every signed call a new run.
            </span>
          </label>
        </div>
        <label className="form-field">
          <span className="form-label">params mapping (JSON)</span>
          <textarea
            value={mapping}
            onChange={(e) => setMapping(e.target.value)}
            rows={4}
            className="mono"
            aria-label="params mapping"
            placeholder={'{\n  "repo": "$.payload.repository.name",\n  "label": "build {{ $.payload.ref }}"\n}'}
          />
          {parsed.error ? (
            <span className="form-hint invalid" role="alert">
              Not valid: {parsed.error}
            </span>
          ) : (
            <span className="form-hint">
              Each value is a JSONPath (<code>$.payload.repo</code>), a template (<code>build {'{{ $.payload.ref }}'}</code>) or a
              literal. The request body is under <code>$.payload</code>.
            </span>
          )}
        </label>
        <div className="form-row">
          <label className="form-field">
            <span className="form-label">priority</span>
            <input type="number" min={0} max={1000} value={priority} onChange={(e) => setPriority(e.target.value)} placeholder="server default" />
          </label>
          <label className="form-field">
            <span className="form-label">timestamp tolerance (seconds)</span>
            <input type="number" min={1} value={tolerance} onChange={(e) => setTolerance(e.target.value)} placeholder="server default" />
            <span className="form-hint">How old a signed request may be. Smaller narrows the replay window.</span>
          </label>
        </div>
        <label className="form-check">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          <span>Enabled</span>
        </label>
        {error != null && <ErrorBox error={conflictHint(error) ?? error} what={trigger ? 'the trigger changes' : 'the new trigger'} />}
        <div className="form-actions">
          <button type="submit" className="primary" disabled={pending || parsed.error !== null || workflow.trim() === ''}>
            {pending ? 'Saving…' : trigger ? 'Save changes' : 'Create trigger'}
          </button>
          <button type="button" onClick={onCancel} disabled={pending}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
