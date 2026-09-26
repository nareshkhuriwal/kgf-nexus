// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Notification channels: where a message can go.
 *
 * A channel is a destination plus the credential to reach it. The credential is
 * write-only: the list shows WHERE it lives (sealed by the gateway, or a
 * reference to an env var / file on the server) and a short hint, never the
 * value. Editing a channel with the secret field left blank keeps the stored
 * one, which the form says rather than leaving the operator to wonder whether
 * saving wiped it.
 *
 * "Send test" queues a real delivery and then follows it to delivered or dead,
 * so the answer to "does this channel work" is the dispatcher's own outcome and
 * not merely "the request was accepted".
 */

import { useState } from 'react';
import { ApiError } from '../api/client';
import {
  errorMessage,
  useChannels,
  useDeleteChannel,
  useDelivery,
  useSaveChannel,
  useTestChannel,
} from '../api/hooks';
import type { ChannelKind, ChannelRequest, NotificationChannel } from '../api/types';
import { DataTable, type Column } from '../components/DataTable';
import { Permit, usePermission } from '../components/Permissions';
import { ActionIcon, RowAction } from '../components/RowAction';
import { Empty, ErrorBox } from '../components/Status';
import {
  EnabledBadge,
  EventsPanel,
  OneTimeSecret,
  conflictHint,
} from '../components/EventsShared';
import { fmtTs } from '../lib/format';

export const CHANNEL_KINDS: { kind: ChannelKind; label: string }[] = [
  { kind: 'slack_webhook', label: 'Slack (incoming webhook)' },
  { kind: 'slack_bot', label: 'Slack (bot token)' },
  { kind: 'teams', label: 'Microsoft Teams' },
  { kind: 'pagerduty', label: 'PagerDuty' },
  { kind: 'webhook', label: 'Signed webhook' },
  { kind: 'email', label: 'Email' },
];

/** What the secret IS for each kind, or null when the kind takes none. */
const SECRET_LABEL: Record<ChannelKind, string | null> = {
  slack_webhook: 'Slack incoming-webhook URL',
  slack_bot: 'Slack bot token (xoxb-…)',
  teams: 'Teams incoming-webhook URL',
  pagerduty: 'PagerDuty Events v2 routing key',
  webhook: null,
  email: null,
};

const PD_SEVERITIES = ['critical', 'error', 'warning', 'info'] as const;

function kindLabel(kind: string): string {
  return CHANNEL_KINDS.find((k) => k.kind === kind)?.label ?? kind;
}

/** The destination, in one line, from the non-secret config. */
export function channelTarget(c: Pick<NotificationChannel, 'kind' | 'config'>): string {
  const cfg = c.config ?? {};
  switch (c.kind) {
    case 'slack_bot':
      return typeof cfg.channel === 'string' ? cfg.channel : '—';
    case 'pagerduty':
      return `severity ${typeof cfg.severity === 'string' ? cfg.severity : 'error'}`;
    case 'webhook':
      return typeof cfg.url === 'string' ? cfg.url : '—';
    case 'email':
      return Array.isArray(cfg.to) ? cfg.to.join(', ') : '—';
    default:
      return 'URL held in the secret';
  }
}

export function ChannelsTab({ namespace }: { namespace: string }) {
  const can = usePermission();
  const canManage = can('notifications.manage');
  const channels = useChannels(namespace);
  const save = useSaveChannel(namespace);
  const remove = useDeleteChannel(namespace);
  const [editing, setEditing] = useState<NotificationChannel | 'new' | null>(null);
  const [created, setCreated] = useState<{ name: string; secret: string } | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [testing, setTesting] = useState<{ channel: string; deliveryId: string } | null>(null);
  const [tableControls, setTableControls] = useState<HTMLDivElement | null>(null);

  const columns: Column<NotificationChannel>[] = [
    {
      key: 'status',
      header: 'status',
      sort: (a, b) => Number(b.enabled) - Number(a.enabled),
      cell: (c) => (
        <EnabledBadge
          enabled={c.enabled}
          onTitle="Rules that name this channel deliver to it."
          offTitle="Disabled: rules that name it skip it until it is re-enabled."
        />
      ),
    },
    { key: 'name', header: 'name', cellClass: 'mono', sort: (a, b) => a.name.localeCompare(b.name), cell: (c) => c.name },
    { key: 'kind', header: 'kind', sort: (a, b) => a.kind.localeCompare(b.kind), cell: (c) => kindLabel(c.kind) },
    { key: 'target', header: 'target', cellClass: 'mono', cell: (c) => channelTarget(c) },
    {
      key: 'secret',
      header: 'secret',
      cell: (c) =>
        !c.secret?.configured ? (
          <span className="faint">{c.kind === 'email' ? 'not needed' : 'none'}</span>
        ) : (
          <span className="cell-stack">
            <span>{c.secret.source === 'ref' ? 'server reference' : 'sealed'}</span>
            {(c.secret.ref || c.secret.hint) && (
              <span className="cell-sub mono">{c.secret.ref ?? c.secret.hint}</span>
            )}
          </span>
        ),
    },
    {
      key: 'rate',
      header: 'rate limit',
      align: 'end',
      cellClass: 'nowrap',
      cell: (c) => `${c.rate_limit_per_minute}/min`,
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (c) => (
        <div className="cell-actions">
          <Permit permission="notifications.manage">
            <TestButton
              namespace={namespace}
              channel={c}
              onQueued={(deliveryId) => setTesting({ channel: c.name, deliveryId })}
            />
            <button
              type="button"
              className="icon-action"
              aria-label={`Edit channel ${c.name}`}
              title="Edit"
              onClick={() => {
                setCreated(null);
                setEditing(c);
              }}
            >
              <ActionIcon name="edit" />
            </button>
            <button
              type="button"
              className="icon-action"
              aria-label={c.enabled ? `Disable channel ${c.name}` : `Enable channel ${c.name}`}
              title={c.enabled ? 'Disable' : 'Enable'}
              disabled={save.isPending && save.variables?.name === c.name}
              onClick={() => {
                setActionError(null);
                save.mutate(
                  { name: c.name, body: { enabled: !c.enabled, revision: c.revision } },
                  { onError: (e) => setActionError(conflictHint(e) ?? errorMessage(e)) },
                );
              }}
            >
              <ActionIcon name={c.enabled ? 'disable' : 'enable'} />
            </button>
          </Permit>
          <RowAction
            permission="notifications.manage"
            label={`Delete channel ${c.name}`}
            icon="delete"
            confirm={`Delete channel ${c.name}? Rules that name it stop delivering there. Its delivery history is kept.`}
            onAction={() => remove.mutateAsync(c.name)}
          />
        </div>
      ),
    },
  ];

  return (
    <div className="col">
      {actionError && <ErrorBox error={actionError} what="that change" />}
      {created && (
        <OneTimeSecret
          title={`Signing secret for ${created.name}`}
          secret={created.secret}
          onDismiss={() => setCreated(null)}
        >
          <p className="form-hint">
            Every request this channel sends carries <code>X-Nexus-Timestamp</code> and{' '}
            <code>X-Nexus-Signature: v1=hex(HMAC-SHA256(secret, timestamp + &quot;.&quot; + body))</code>.
            Verify it in the receiver before trusting the body.
          </p>
        </OneTimeSecret>
      )}
      {editing && (
        <ChannelForm
          key={editing === 'new' ? 'new' : editing.id}
          channel={editing === 'new' ? null : editing}
          pending={save.isPending}
          error={save.error}
          onCancel={() => {
            save.reset();
            setEditing(null);
          }}
          onSubmit={(name, body) =>
            save.mutate(
              { name, body },
              {
                onSuccess: (out) => {
                  setEditing(null);
                  const secret = 'signing_secret' in out ? out.signing_secret : undefined;
                  if (!name && typeof secret === 'string' && secret) {
                    setCreated({ name: out.name, secret });
                  }
                },
              },
            )
          }
        />
      )}
      {testing && (
        <TestOutcome
          namespace={namespace}
          channel={testing.channel}
          deliveryId={testing.deliveryId}
          onClose={() => setTesting(null)}
        />
      )}
      <div className="panel">
        <div className="panel-head">
          <span>Channels</span>
          <div className="panel-head-controls" ref={setTableControls} />
          {canManage && !editing && (
            <span className="panel-tools">
              <button
                type="button"
                className="primary"
                onClick={() => {
                  setCreated(null);
                  setEditing('new');
                }}
              >
                New channel
              </button>
            </span>
          )}
        </div>
        <EventsPanel
          query={channels}
          what="notification channels"
          feature="Notifications"
          permission="notifications.view"
          isEmpty={(d) => d.channels.length === 0}
          empty={
            <Empty
              title="No notification channels yet"
              detail="A channel is a destination: Slack, Teams, PagerDuty, email or a signed webhook. Create one, then add a rule that sends to it."
            />
          }
        >
          {(d) => (
            <DataTable
              controlsTarget={tableControls}
              rows={d.channels}
              columns={columns}
              rowKey={(c) => c.id}
              noun="channels"
              count={{ kind: 'exact' }}
              search={{ mode: 'client', fields: (c) => [c.name, c.kind, channelTarget(c)] }}
            />
          )}
        </EventsPanel>
      </div>
    </div>
  );
}

function TestButton({
  namespace,
  channel,
  onQueued,
}: {
  namespace: string;
  channel: NotificationChannel;
  onQueued: (deliveryId: string) => void;
}) {
  const test = useTestChannel(namespace);
  return (
    <>
      <button
        type="button"
        onClick={() => test.mutate(channel.name, { onSuccess: (r) => onQueued(String(r.delivery_id)) })}
        disabled={test.isPending || !channel.enabled}
        title={channel.enabled ? 'Queue a test message and follow it to its outcome' : 'Enable the channel to test it'}
        aria-label={`Send test to ${channel.name}`}
      >
        {test.isPending ? 'Queuing…' : 'Send test'}
      </button>
      {test.error != null && (
        <span role="alert" className="row-action-error">
          {errorMessage(test.error)}
        </span>
      )}
    </>
  );
}

/** Follows a test delivery until the dispatcher settles it. */
export function TestOutcome({
  namespace,
  channel,
  deliveryId,
  onClose,
}: {
  namespace: string;
  channel: string;
  deliveryId: string;
  onClose: () => void;
}) {
  const delivery = useDelivery(namespace, deliveryId);
  const d = delivery.data;
  let tone = 'tone-queued';
  let text = 'Queued. Waiting for the event service to send it…';
  if (d?.state === 'delivered') {
    tone = 'tone-success';
    text = `Delivered${d.delivered_at ? ` at ${fmtTs(d.delivered_at)}` : ''}${d.last_status ? ` (HTTP ${d.last_status})` : ''}.`;
  } else if (d?.state === 'dead') {
    tone = 'tone-danger';
    text = `Failed after ${d.attempts} attempt${d.attempts === 1 ? '' : 's'}: ${d.last_error ?? 'no error recorded'}`;
  } else if (d && d.attempts > 0) {
    tone = 'tone-warning';
    text = `Attempt ${d.attempts} of ${d.max_attempts} failed (${d.last_error ?? 'no error recorded'}); retrying${d.next_attempt_at ? ` at ${fmtTs(d.next_attempt_at)}` : ''}.`;
  }
  return (
    <div className="panel" role="status" aria-live="polite" aria-label={`Test delivery to ${channel}`}>
      <div className="panel-head">
        <span>
          Test delivery to <code>{channel}</code> · #{deliveryId}
        </span>
        <span className="spacer" />
        <button type="button" onClick={onClose}>
          Close
        </button>
      </div>
      <div className="panel-body">
        {delivery.error && !d ? (
          <ErrorBox error={delivery.error} what="the test delivery" onRetry={() => void delivery.refetch()} />
        ) : (
          <p>
            <span className={`badge ${tone}`}>{d?.state ?? 'pending'}</span> {text}
          </p>
        )}
      </div>
    </div>
  );
}

/** Kind-specific form. Name and kind are immutable once created. */
export function ChannelForm({
  channel,
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  channel: NotificationChannel | null;
  pending: boolean;
  error: unknown;
  onSubmit: (name: string | undefined, body: ChannelRequest) => void;
  onCancel: () => void;
}) {
  const editing = channel !== null;
  const cfg = channel?.config ?? {};
  const [name, setName] = useState(channel?.name ?? '');
  const [kind, setKind] = useState<ChannelKind>(channel?.kind ?? 'slack_webhook');
  const [slackChannel, setSlackChannel] = useState(typeof cfg.channel === 'string' ? cfg.channel : '');
  const [severity, setSeverity] = useState(typeof cfg.severity === 'string' ? cfg.severity : 'error');
  const [url, setUrl] = useState(typeof cfg.url === 'string' ? cfg.url : '');
  const [to, setTo] = useState(Array.isArray(cfg.to) ? cfg.to.join(', ') : '');
  const [secretMode, setSecretMode] = useState<'sealed' | 'ref'>(channel?.secret?.source === 'ref' ? 'ref' : 'sealed');
  const [secret, setSecret] = useState('');
  const [secretRef, setSecretRef] = useState(channel?.secret?.ref ?? '');
  const [rate, setRate] = useState(channel ? String(channel.rate_limit_per_minute) : '');
  const [enabled, setEnabled] = useState(channel?.enabled ?? true);

  const secretLabel = SECRET_LABEL[kind];
  const recipients = to
    .split(/[\s,;]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  const refValid = secretRef.trim() === '' || /^(env:[A-Za-z_][A-Za-z0-9_]*|file:\/.+)$/.test(secretRef.trim());
  const needsSecret = secretLabel !== null && !editing;
  const secretMissing =
    needsSecret && (secretMode === 'sealed' ? secret.trim() === '' : secretRef.trim() === '');
  const rateNum = rate.trim() === '' ? undefined : Number(rate);
  const rateInvalid = rateNum !== undefined && (!Number.isInteger(rateNum) || rateNum < 1 || rateNum > 6000);

  function config(): Record<string, unknown> {
    switch (kind) {
      case 'slack_bot':
        return { channel: slackChannel.trim() };
      case 'pagerduty':
        return { severity };
      case 'webhook':
        return { url: url.trim() };
      case 'email':
        return { to: recipients };
      default:
        return {};
    }
  }

  function submit() {
    const body: ChannelRequest = { config: config(), enabled };
    if (channel) {
      body.revision = channel.revision;
    } else {
      body.name = name.trim();
      body.kind = kind;
    }
    if (rateNum !== undefined) body.rate_limit_per_minute = rateNum;
    if (secretLabel !== null) {
      if (secretMode === 'sealed' && secret.trim() !== '') body.secret = secret.trim();
      if (secretMode === 'ref' && secretRef.trim() !== '' && secretRef.trim() !== channel?.secret?.ref) {
        body.secret_ref = secretRef.trim();
      }
    }
    onSubmit(channel ? channel.name : undefined, body);
  }

  const keyMissing = error instanceof ApiError && error.status === 503 && error.body.includes('secret_key_unconfigured');

  return (
    <div className="panel">
      <div className="panel-head">
        <span>{editing ? `Edit channel ${channel?.name ?? ""}` : 'New notification channel'}</span>
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
            <input
              value={name}
              readOnly={editing}
              onChange={(e) => setName(e.target.value)}
              placeholder="oncall-slack"
              pattern="[a-z0-9][a-z0-9_.\-]{0,62}"
              required
            />
            <span className="form-hint">Lower-case letters, digits, &apos;.&apos;, &apos;_&apos; or &apos;-&apos;. Rules refer to it by this name.</span>
          </label>
          <label className="form-field">
            <span className="form-label">kind</span>
            <select value={kind} disabled={editing} onChange={(e) => setKind(e.target.value as ChannelKind)}>
              {CHANNEL_KINDS.map((k) => (
                <option key={k.kind} value={k.kind}>
                  {k.label}
                </option>
              ))}
            </select>
            {editing && <span className="form-hint">A channel&apos;s kind cannot change. Create a new channel instead.</span>}
          </label>
        </div>

        {kind === 'slack_bot' && (
          <label className="form-field">
            <span className="form-label">Slack channel</span>
            <input value={slackChannel} onChange={(e) => setSlackChannel(e.target.value)} placeholder="#oncall or C0123456" required />
          </label>
        )}
        {kind === 'pagerduty' && (
          <label className="form-field">
            <span className="form-label">severity</span>
            <select value={severity} onChange={(e) => setSeverity(e.target.value)}>
              {PD_SEVERITIES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>
        )}
        {kind === 'webhook' && (
          <label className="form-field">
            <span className="form-label">URL</span>
            <input type="url" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="https://hooks.example.com/nexus" required className="mono" />
            <span className="form-hint">
              Nexus signs every request. The signing secret is generated by the server and shown once, after you create the channel.
            </span>
          </label>
        )}
        {kind === 'email' && (
          <label className="form-field">
            <span className="form-label">recipients</span>
            <input value={to} onChange={(e) => setTo(e.target.value)} placeholder="oncall@example.com, lead@example.com" required />
            <span className="form-hint">Comma-separated addresses. Email uses the server&apos;s SMTP settings, so it needs no secret here.</span>
          </label>
        )}
        {(kind === 'slack_webhook' || kind === 'teams') && (
          <p className="form-hint">This kind has no settings beyond its URL, which is a credential and goes in the secret below.</p>
        )}

        {secretLabel !== null && (
          <fieldset className="form-fieldset">
            <legend>{secretLabel}</legend>
            <label className="form-check">
              <input type="radio" name="secret-mode" checked={secretMode === 'sealed'} onChange={() => setSecretMode('sealed')} />
              <span>Store it sealed on the server</span>
            </label>
            <label className="form-check">
              <input type="radio" name="secret-mode" checked={secretMode === 'ref'} onChange={() => setSecretMode('ref')} />
              <span>Reference a value the gateway reads (env var or file)</span>
            </label>
            {secretMode === 'sealed' ? (
              <label className="form-field">
                <span className="form-label">value</span>
                <input
                  type="password"
                  autoComplete="off"
                  value={secret}
                  onChange={(e) => setSecret(e.target.value)}
                  aria-label={secretLabel}
                  placeholder={channel?.secret?.configured ? 'Leave blank to keep the current secret' : ''}
                />
                <span className="form-hint">
                  {channel?.secret?.configured
                    ? `Currently ${channel.secret.source === 'ref' ? 'a server reference' : 'sealed'}${channel.secret.hint ? ` (${channel.secret.hint})` : ''}. Blank keeps it.`
                    : 'Write-only: it is never shown again, only a short hint.'}
                </span>
              </label>
            ) : (
              <label className="form-field">
                <span className="form-label">reference</span>
                <input value={secretRef} onChange={(e) => setSecretRef(e.target.value)} className="mono" placeholder="env:SLACK_WEBHOOK_URL or file:/run/secrets/slack" aria-label="secret reference" />
                {!refValid ? (
                  <span className="form-hint invalid" role="alert">
                    Use env:NAME or file:/absolute/path.
                  </span>
                ) : (
                  <span className="form-hint">Resolved by the gateway when it sends; the value never enters the database.</span>
                )}
              </label>
            )}
          </fieldset>
        )}

        <div className="form-row">
          <label className="form-field">
            <span className="form-label">rate limit per minute</span>
            <input type="number" min={1} max={6000} value={rate} onChange={(e) => setRate(e.target.value)} placeholder="server default" />
            {rateInvalid ? (
              <span className="form-hint invalid" role="alert">
                A whole number from 1 to 6000.
              </span>
            ) : (
              <span className="form-hint">Messages beyond this wait for the next minute rather than being dropped.</span>
            )}
          </label>
        </div>
        <label className="form-check">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          <span>Enabled</span>
        </label>

        {keyMissing ? (
          <div className="notice error" role="alert">
            <span className="notice-glyph" aria-hidden="true">
              !
            </span>
            <div>
              <div className="notice-title">The server cannot store secrets</div>
              <div className="notice-detail">
                The gateway has no <code>NEXUS_SECRET_KEY</code>, so it cannot seal a secret. Choose
                &quot;Reference a value the gateway reads&quot; and give an <code>env:</code> or{' '}
                <code>file:</code> reference instead, or ask an operator to configure the key.
              </div>
            </div>
          </div>
        ) : error ? (
          <ErrorBox error={conflictHint(error) ?? error} what={editing ? 'the channel changes' : 'the new channel'} />
        ) : null}

        <div className="form-actions">
          <button type="submit" className="primary" disabled={pending || secretMissing || !refValid || rateInvalid}>
            {pending ? 'Saving…' : editing ? 'Save changes' : 'Create channel'}
          </button>
          <button type="button" onClick={onCancel} disabled={pending}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
