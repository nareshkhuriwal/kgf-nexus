// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Notification rules: which events go to which channels.
 *
 * A rule matches on event type, and optionally narrows to named workflows and to
 * run labels. An empty workflow list and an empty label set mean "every run in
 * the namespace", which the table says in words rather than rendering two empty
 * cells that read as "nothing".
 *
 * `run.sla_exceeded` is the one event with a parameter: the SLA itself. The
 * field appears only when that event is chosen, and is required then, matching
 * the server's rule (60 seconds to 30 days).
 */

import { useState } from 'react';
import { errorMessage, useChannels, useDeleteRule, useRules, useSaveRule } from '../api/hooks';
import type { NotificationRule, RuleRequest } from '../api/types';
import { DataTable, type Column } from '../components/DataTable';
import { Permit, usePermission } from '../components/Permissions';
import { ActionIcon, RowAction } from '../components/RowAction';
import { Empty, ErrorBox } from '../components/Status';
import { EnabledBadge, EventsPanel, conflictHint } from '../components/EventsShared';

/** Fallback when the server's list omits `events`; the server's list wins. */
export const RULE_EVENTS = ['run.failed', 'run.succeeded', 'run.timed_out', 'run.sla_exceeded', 'attempt.lost'];

const EVENT_HINTS: Record<string, string> = {
  'run.failed': 'A run ended FAILED.',
  'run.succeeded': 'A run ended SUCCEEDED.',
  'run.timed_out': 'A run hit its timeout.',
  'run.sla_exceeded': 'A run is still going after the SLA below.',
  'attempt.lost': 'A worker stopped heartbeating mid-attempt.',
};

const SLA_MIN = 60;
const SLA_MAX = 2_592_000;

function fmtSla(seconds: number): string {
  if (seconds % 86400 === 0) return `${seconds / 86400}d`;
  if (seconds % 3600 === 0) return `${seconds / 3600}h`;
  if (seconds % 60 === 0) return `${seconds / 60}m`;
  return `${seconds}s`;
}

function ruleBody(r: NotificationRule, over: Partial<RuleRequest> = {}): RuleRequest {
  return {
    events: r.events,
    workflows: r.workflows ?? [],
    labels: r.labels ?? {},
    ...(r.sla_seconds ? { sla_seconds: r.sla_seconds } : {}),
    channels: r.channels,
    enabled: r.enabled,
    revision: r.revision,
    ...over,
  };
}

export function RulesTab({ namespace }: { namespace: string }) {
  const can = usePermission();
  const canManage = can('notifications.manage');
  const rules = useRules(namespace);
  const channels = useChannels(namespace);
  const save = useSaveRule(namespace);
  const remove = useDeleteRule(namespace);
  const [editing, setEditing] = useState<NotificationRule | 'new' | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [tableControls, setTableControls] = useState<HTMLDivElement | null>(null);

  const events = rules.data && rules.data.events.length > 0 ? rules.data.events : RULE_EVENTS;
  const channelNames = channels.data?.channels.map((c) => c.name) ?? [];

  const columns: Column<NotificationRule>[] = [
    {
      key: 'status',
      header: 'status',
      sort: (a, b) => Number(b.enabled) - Number(a.enabled),
      cell: (r) => <EnabledBadge enabled={r.enabled} offTitle="Disabled: matching events send nothing." />,
    },
    { key: 'name', header: 'name', cellClass: 'mono', sort: (a, b) => a.name.localeCompare(b.name), cell: (r) => r.name },
    {
      key: 'events',
      header: 'events',
      cell: (r) => (
        <span className="chip-set">
          {r.events.map((e) => (
            <span key={e} className="chip">
              {e}
              {e === 'run.sla_exceeded' && r.sla_seconds ? ` > ${fmtSla(r.sla_seconds)}` : ''}
            </span>
          ))}
        </span>
      ),
    },
    {
      key: 'scope',
      header: 'scope',
      cell: (r) => {
        const wf = r.workflows ?? [];
        const labels = Object.entries(r.labels ?? {});
        if (wf.length === 0 && labels.length === 0) return <span className="faint">every run</span>;
        return (
          <span className="cell-stack">
            {wf.length > 0 && <span className="mono">{wf.join(', ')}</span>}
            {labels.length > 0 && (
              <span className="cell-sub mono">{labels.map(([k, v]) => `${k}=${v}`).join(', ')}</span>
            )}
          </span>
        );
      },
    },
    {
      key: 'channels',
      header: 'channels',
      cellClass: 'mono',
      cell: (r) => r.channels.join(', '),
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (r) => (
        <div className="cell-actions">
          <Permit permission="notifications.manage">
            <button type="button" className="icon-action" aria-label={`Edit rule ${r.name}`} title="Edit" onClick={() => setEditing(r)}>
              <ActionIcon name="edit" />
            </button>
            <button
              type="button"
              className="icon-action"
              aria-label={r.enabled ? `Disable rule ${r.name}` : `Enable rule ${r.name}`}
              title={r.enabled ? 'Disable' : 'Enable'}
              disabled={save.isPending && save.variables?.name === r.name}
              onClick={() => {
                setActionError(null);
                save.mutate(
                  { name: r.name, body: ruleBody(r, { enabled: !r.enabled }) },
                  { onError: (e) => setActionError(conflictHint(e) ?? errorMessage(e)) },
                );
              }}
            >
              <ActionIcon name={r.enabled ? 'disable' : 'enable'} />
            </button>
          </Permit>
          <RowAction
            permission="notifications.manage"
            label={`Delete rule ${r.name}`}
            icon="delete"
            confirm={`Delete rule ${r.name}? Matching events stop notifying. Past deliveries are kept.`}
            onAction={() => remove.mutateAsync(r.name)}
          />
        </div>
      ),
    },
  ];

  return (
    <div className="col">
      {actionError && <ErrorBox error={actionError} what="that change" />}
      {editing && (
        <RuleForm
          key={editing === 'new' ? 'new' : editing.id}
          rule={editing === 'new' ? null : editing}
          events={events}
          channelNames={channelNames}
          channelsLoading={channels.isLoading}
          pending={save.isPending}
          error={save.error}
          onCancel={() => {
            save.reset();
            setEditing(null);
          }}
          onSubmit={(name, body) => save.mutate({ name, body }, { onSuccess: () => setEditing(null) })}
        />
      )}
      <div className="panel">
        <div className="panel-head">
          <span>Rules</span>
          <div className="panel-head-controls" ref={setTableControls} />
          {canManage && !editing && (
            <span className="panel-tools">
              <button type="button" className="primary" onClick={() => setEditing('new')}>
                New rule
              </button>
            </span>
          )}
        </div>
        <EventsPanel
          query={rules}
          what="notification rules"
          feature="Notifications"
          permission="notifications.view"
          isEmpty={(d) => d.rules.length === 0}
          empty={
            <Empty
              title="No notification rules yet"
              detail="A rule says which run events (failed, timed out, SLA exceeded…) are sent to which channels. Nothing is sent until one exists."
            />
          }
        >
          {(d) => (
            <DataTable
              controlsTarget={tableControls}
              rows={d.rules}
              columns={columns}
              rowKey={(r) => r.id}
              noun="rules"
              count={{ kind: 'exact' }}
              search={{ mode: 'client', fields: (r) => [r.name, ...r.events, ...r.channels, ...(r.workflows ?? [])] }}
            />
          )}
        </EventsPanel>
      </div>
    </div>
  );
}

function parseLabels(text: string): { value: Record<string, string>; error: string | null } {
  const value: Record<string, string> = {};
  for (const raw of text.split(/[\n,]+/)) {
    const line = raw.trim();
    if (!line) continue;
    const eq = line.indexOf('=');
    if (eq <= 0) return { value, error: `"${line}" is not key=value` };
    value[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return { value, error: null };
}

export function RuleForm({
  rule,
  events,
  channelNames,
  channelsLoading,
  pending,
  error,
  onSubmit,
  onCancel,
}: {
  rule: NotificationRule | null;
  events: string[];
  channelNames: string[];
  channelsLoading: boolean;
  pending: boolean;
  error: unknown;
  onSubmit: (name: string | undefined, body: RuleRequest) => void;
  onCancel: () => void;
}) {
  const [name, setName] = useState(rule?.name ?? '');
  const [chosenEvents, setChosenEvents] = useState<string[]>(rule?.events ?? ['run.failed']);
  const [chosenChannels, setChosenChannels] = useState<string[]>(rule?.channels ?? []);
  const [workflows, setWorkflows] = useState((rule?.workflows ?? []).join(', '));
  const [labels, setLabels] = useState(
    Object.entries(rule?.labels ?? {})
      .map(([k, v]) => `${k}=${v}`)
      .join('\n'),
  );
  const [sla, setSla] = useState(rule?.sla_seconds ? String(rule.sla_seconds) : '3600');
  const [enabled, setEnabled] = useState(rule?.enabled ?? true);

  const wantsSla = chosenEvents.includes('run.sla_exceeded');
  const slaNum = Number(sla);
  const slaInvalid = wantsSla && (!Number.isInteger(slaNum) || slaNum < SLA_MIN || slaNum > SLA_MAX);
  const parsedLabels = parseLabels(labels);
  // A rule may keep naming a channel that was since deleted; keep it visible so
  // saving does not silently drop it without the operator seeing why.
  const allChannels = Array.from(new Set([...channelNames, ...chosenChannels])).sort();

  function toggle(list: string[], value: string, on: boolean): string[] {
    return on ? Array.from(new Set([...list, value])) : list.filter((v) => v !== value);
  }

  function submit() {
    const body: RuleRequest = {
      events: chosenEvents,
      workflows: workflows
        .split(/[\s,]+/)
        .map((w) => w.trim())
        .filter(Boolean),
      labels: parsedLabels.value,
      channels: chosenChannels,
      enabled,
    };
    if (wantsSla) body.sla_seconds = slaNum;
    if (rule) body.revision = rule.revision;
    else body.name = name.trim();
    onSubmit(rule ? rule.name : undefined, body);
  }

  const blocked =
    pending || chosenEvents.length === 0 || chosenChannels.length === 0 || slaInvalid || parsedLabels.error !== null;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>{rule ? `Edit rule ${rule.name}` : 'New notification rule'}</span>
      </div>
      <form
        className="panel-body form-body"
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <label className="form-field">
          <span className="form-label">name</span>
          <input
            value={name}
            readOnly={!!rule}
            onChange={(e) => setName(e.target.value)}
            placeholder="prod-failures"
            pattern="[a-z0-9][a-z0-9_.\-]{0,62}"
            required
          />
        </label>

        <fieldset className="form-fieldset">
          <legend>events</legend>
          {events.map((ev) => (
            <label key={ev} className="form-check">
              <input
                type="checkbox"
                checked={chosenEvents.includes(ev)}
                onChange={(e) => setChosenEvents((l) => toggle(l, ev, e.target.checked))}
              />
              <span>
                <code>{ev}</code> {EVENT_HINTS[ev] && <span className="faint">— {EVENT_HINTS[ev]}</span>}
              </span>
            </label>
          ))}
          {chosenEvents.length === 0 && (
            <span className="form-hint invalid" role="alert">
              Choose at least one event.
            </span>
          )}
        </fieldset>

        {wantsSla && (
          <label className="form-field">
            <span className="form-label">SLA (seconds)</span>
            <input type="number" min={SLA_MIN} max={SLA_MAX} value={sla} onChange={(e) => setSla(e.target.value)} required aria-label="SLA seconds" />
            {slaInvalid ? (
              <span className="form-hint invalid" role="alert">
                Between {SLA_MIN} seconds and 30 days ({SLA_MAX} seconds).
              </span>
            ) : (
              <span className="form-hint">
                run.sla_exceeded fires once for a run still going after {fmtSla(slaNum)}.
              </span>
            )}
          </label>
        )}

        <fieldset className="form-fieldset">
          <legend>channels</legend>
          {channelsLoading ? (
            <span className="form-hint">Loading channels…</span>
          ) : allChannels.length === 0 ? (
            <span className="form-hint invalid" role="alert">
              There are no channels to send to. Create one on the Channels tab first.
            </span>
          ) : (
            allChannels.map((c) => (
              <label key={c} className="form-check">
                <input
                  type="checkbox"
                  checked={chosenChannels.includes(c)}
                  onChange={(e) => setChosenChannels((l) => toggle(l, c, e.target.checked))}
                />
                <span className="mono">
                  {c}
                  {!channelNames.includes(c) && <span className="faint"> (no longer exists)</span>}
                </span>
              </label>
            ))
          )}
        </fieldset>

        <div className="form-row">
          <label className="form-field">
            <span className="form-label">workflows</span>
            <input value={workflows} onChange={(e) => setWorkflows(e.target.value)} className="mono" placeholder="all workflows" />
            <span className="form-hint">Comma-separated workflow names. Blank matches every workflow.</span>
          </label>
          <label className="form-field">
            <span className="form-label">labels</span>
            <textarea value={labels} onChange={(e) => setLabels(e.target.value)} rows={2} className="mono" placeholder="env=prod" aria-label="labels" />
            {parsedLabels.error ? (
              <span className="form-hint invalid" role="alert">
                {parsedLabels.error}
              </span>
            ) : (
              <span className="form-hint">One key=value per line. A run must carry all of them. Blank matches any labels.</span>
            )}
          </label>
        </div>

        <label className="form-check">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          <span>Enabled</span>
        </label>

        {error != null && <ErrorBox error={conflictHint(error) ?? error} what={rule ? 'the rule changes' : 'the new rule'} />}

        <div className="form-actions">
          <button type="submit" className="primary" disabled={blocked}>
            {pending ? 'Saving…' : rule ? 'Save changes' : 'Create rule'}
          </button>
          <button type="button" onClick={onCancel} disabled={pending}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
