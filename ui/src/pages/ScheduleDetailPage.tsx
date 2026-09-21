import { Permit, usePermission } from '../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One schedule: what it is, when it fires next, and its backfills.
 *
 * The upcoming fire times are shown here and not only on the create form,
 * because the question "when does this next run?" outlives the moment of
 * authoring — and because a stored expression can stop parsing (a zone that
 * vanished from the tzdata, an expression written by an older build). The
 * gateway reports that as cron_error instead of next_fires, and this page leads
 * with it: a schedule that looks armed and can never fire is the failure nobody
 * notices until a report is missing.
 */

import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import {
  errorMessage,
  useBackfills,
  useDeleteSchedule,
  useSchedule,
  useSetScheduleEnabled,
} from '../api/hooks';
import { CreateScheduleForm } from './CreateScheduleForm';
import type { Schedule } from '../api/types';
import { AsyncPanel, ErrorBox, Loading, StaleWarning } from '../components/Status';
import { CronPreview } from '../components/CronPreview';
import { BackfillTable, StartBackfillForm } from '../components/Backfills';
import { EMPTY, fmtTs } from '../lib/format';

export function ScheduleDetailPage({ namespace }: { namespace: string }) {
  const { name = '' } = useParams();
  const navigate = useNavigate();
  const can=usePermission();
  const [actionError, setActionError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [startingBackfill, setStartingBackfill] = useState(false);

  const schedule = useSchedule(namespace, name);
  const backfills = useBackfills(namespace, can("backfills.view") ? name : "");
  const setEnabled = useSetScheduleEnabled(namespace);
  const remove = useDeleteSchedule(namespace);

  if (schedule.error && schedule.data === undefined) {
    return (
      <div className="col">
        <div className="detail-actions detail-back-only"><Link to="/schedules" className="btn">← Back to schedules</Link></div>
        <ErrorBox error={schedule.error} what={`schedule ${name}`} onRetry={schedule.refetch} />
      </div>
    );
  }
  if (!schedule.data) return <div className="col"><div className="detail-actions detail-back-only"><Link to="/schedules" className="btn">← Back to schedules</Link></div><Loading rows={8} label="Loading schedule" /></div>;

  const s = schedule.data;

  return (
    <div className="col">
      <div className="detail-head">
        <div className="detail-head-text">
          {/* The state badge leads the name: on a schedule page reached
              mid-incident, "is this armed?" is read before "what is it called". */}
          <div className="detail-title-row">
            {s.enabled ? (
              <span className="badge tone-success">enabled</span>
            ) : (
              <span className="badge tone-stopped">disabled</span>
            )}
            <h1 className="detail-title mono">{s.name}</h1>
          </div>
          <div className="detail-meta">
            <code>{s.spec_name}</code>
            {s.spec_version !== undefined ? ` v${s.spec_version}` : ' (latest version)'} ·{' '}
            <code>{s.cron}</code> in <code>{s.timezone}</code> · created by {s.created_by}
          </div>
        </div>
        <div className="detail-actions">
          <Link to="/schedules" className="btn">← Back to schedules</Link>
          <Permit permission="schedules.update"><button type="button" disabled={!!editing || setEnabled.isPending} onClick={() => setEditing(s)}>Edit schedule</button></Permit>
          <Permit permission="schedules.update"><button
            type="button"
            disabled={setEnabled.isPending || !!editing}
            onClick={() => {
              setActionError(null);
              setEnabled.mutate(
                { name: s.name, enabled: !s.enabled },
                { onError: (e) => setActionError(errorMessage(e)) },
              );
            }}
            title={
              s.enabled
                ? 'Stop it firing. Runs already created are unaffected.'
                : 'Arm it again. The next fire time is recomputed from now, so no missed ticks fire as a burst.'
            }
          >
            {s.enabled ? 'Disable' : 'Enable'}
          </button></Permit>
          <Permit permission="schedules.delete"><DeleteScheduleButton
            name={s.name}
            pending={remove.isPending || !!editing}
            onDelete={() => {
              setActionError(null);
              remove.mutate(s.name, {
                onSuccess: () => navigate('/schedules'),
                onError: (e) => setActionError(errorMessage(e)),
              });
            }}
          /></Permit>
        </div>
      </div>

      {editing && <Permit permission="schedules.update"><CreateScheduleForm key={editing.id} namespace={namespace} schedule={editing} onDone={() => setEditing(null)} /></Permit>}
      {actionError && <ErrorBox error={actionError} what="that action" />}
      {schedule.error != null ? <StaleWarning error={schedule.error} /> : null}

      <div className="grid-2">
        <div className="panel">
          <div className="panel-head">
            <span>configuration</span>
          </div>
          <div className="panel-body">
            <ScheduleFacts schedule={s} />
          </div>
        </div>

        <div className="panel">
          <div className="panel-head">
            <span>upcoming fires</span>
          </div>
          <div className="panel-body">
            {s.cron_error ? (
              /*
               * A stored expression that no longer compiles. The schedule still
               * says "enabled" everywhere else, so this is the only place the
               * truth shows up — it leads, rather than being a footnote.
               */
              <div className="notice error" role="alert">
                <span className="notice-glyph" aria-hidden="true">
                  !
                </span>
                <div>
                  <div className="notice-title">This schedule cannot fire</div>
                  <div className="notice-detail">{s.cron_error}</div>
                  <div className="notice-hint">
                    Its stored expression no longer compiles, so it will never fire again — and
                    enabling it will be refused. Edit this schedule to use a valid
                    expression.
                  </div>
                </div>
              </div>
            ) : (
              <CronPreview
                preview={{
                  cron: s.cron,
                  timezone: s.timezone,
                  valid: true,
                  next_fires: s.next_fires,
                }}
              />
            )}
          </div>
        </div>
      </div>

      <Permit permission="backfills.view"><div className="panel">
        <div className="panel-head">
          <span>backfills</span>
          <span className="spacer" />
          <span className="chip">{backfills.data?.length ?? 0}</span>
          <span className="panel-tools">
            <Permit permission="backfills.create"><button type="button" onClick={() => setStartingBackfill((v) => !v)}>
              {startingBackfill ? 'Cancel' : 'New backfill'}
            </button></Permit>
          </span>
        </div>

        {startingBackfill && (
          <div className="panel-body">
            <StartBackfillForm
              namespace={namespace}
              scheduleName={s.name}
              onDone={() => setStartingBackfill(false)}
            />
          </div>
        )}

        {backfills.error != null && backfills.data !== undefined ? (
          <div className="panel-notice" style={{ paddingBottom: 0 }}>
            <StaleWarning error={backfills.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={backfills}
          what="backfills"
          isEmpty={(rows) => rows.length === 0}
          emptyTitle="No backfills for this schedule"
          emptyDetail="A backfill replays an interval this schedule did not cover — after an outage, or when the workflow is new and its history is not."
          loadingRows={3}
        >
          {(rows) => (
            <BackfillTable namespace={namespace} scheduleName={s.name} backfills={rows} />
          )}
        </AsyncPanel>
      </div></Permit>
    </div>
  );
}

function ScheduleFacts({ schedule: s }: { schedule: Schedule }) {
  const params = s.params ?? {};
  return (
    <dl className="kv">
      <dt>id</dt>
      <dd>{s.id}</dd>
      <dt>next fire</dt>
      <dd>
        {s.next_fire_at ? (
          fmtTs(s.next_fire_at)
        ) : s.enabled ? (
          // Enabled and unarmed: a replica is firing it right now, or one died
          // mid-fire. The gateway refuses to hide this, so neither does the UI.
          // Warning-toned rather than plain text: the fact that it is NOT armed
          // is the finding, and a bare phrase in a definition list reads as a
          // value rather than as a problem.
          <span
            className="chip warn"
            title="Enabled but not armed. Either a replica is firing it at this instant, or one died mid-fire and left it unarmed."
          >
            not armed
          </span>
        ) : (
          <span className="faint">{EMPTY} (disabled)</span>
        )}
      </dd>
      <dt>last fire</dt>
      <dd>{s.last_fire_at ? fmtTs(s.last_fire_at) : 'never'}</dd>
      <dt>overlap policy</dt>
      <dd>{s.overlap_policy}</dd>
      <dt>catchup limit</dt>
      <dd>
        {s.catchup_limit}
        {s.catchup_limit === 0 && <span className="faint"> (no catch-up burst)</span>}
      </dd>
      <dt>priority</dt>
      <dd>{s.priority}</dd>
      <dt>params</dt>
      <dd>{Object.keys(params).length === 0 ? EMPTY : JSON.stringify(params)}</dd>
      <dt>created</dt>
      <dd>{fmtTs(s.created_at)}</dd>
      <dt>updated</dt>
      <dd>{fmtTs(s.updated_at)}</dd>
    </dl>
  );
}

/**
 * Delete is confirmed, and the confirmation names the schedule.
 *
 * Deleting a schedule is irreversible and there is no undo on the wire, so the
 * confirmation says what survives it: the runs it has already created are
 * ordinary runs and keep going. Without that, an operator hesitating over this
 * button has to guess.
 */
function DeleteScheduleButton({
  name,
  pending,
  onDelete,
}: {
  name: string;
  pending: boolean;
  onDelete: () => void;
}) {
  const [confirming, setConfirming] = useState(false);

  if (!confirming) {
    return (
      <button type="button" className="danger" onClick={() => setConfirming(true)}>
        Delete
      </button>
    );
  }

  return (
    // Danger-toned and in place, so the sentence about what survives cannot be
    // dismissed unread the way a dialog can.
    <div className="confirm-inline">
      <span className="confirm-inline-text">
        Delete <code>{name}</code> permanently? Runs it already created keep going.
      </span>
      <button type="button" className="danger" onClick={onDelete} disabled={pending}>
        {pending ? 'Deleting…' : 'Yes, delete'}
      </button>
      <button type="button" onClick={() => setConfirming(false)} disabled={pending}>
        Keep it
      </button>
    </div>
  );
}
