// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Schedule list, plus the create form.
 *
 * The operator question this screen answers is "what will fire, and when?" —
 * which is why next_fire_at is a first-class column rather than a detail. A
 * schedule whose next fire time is null is the interesting row: the gateway
 * reports that value as stored, and null does NOT simply mean disabled. It also
 * covers a schedule claimed by a replica firing it right now, and one left
 * unarmed by a replica that died mid-fire. The last case is a schedule that
 * looks healthy and will never fire again, so this page calls it out rather than
 * rendering an em dash and moving on.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { errorMessage, useSchedules, useSetScheduleEnabled } from '../api/hooks';
import type { Schedule } from '../api/types';
import { AsyncPanel, ErrorBox, StaleWarning } from '../components/Status';
import { PageHeader } from '../components/PageHeader';
import { CreateScheduleForm } from './CreateScheduleForm';
import { EMPTY, fmtAge, fmtTs } from '../lib/format';

export function SchedulesPage({ namespace }: { namespace: string }) {
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const schedules = useSchedules(namespace);
  const setEnabled = useSetScheduleEnabled(namespace);
  const now = Date.now();

  return (
    <div className="col">
      <PageHeader
        title="Schedules"
        subtitle={
          <>
            cron-triggered workflows in <code>{namespace}</code> · refreshes every 10s
          </>
        }
        actions={
          <button type="button" className="primary" onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New schedule'}
          </button>
        }
      />

      {actionError && <ErrorBox error={actionError} what="that action" />}

      {creating && (
        <CreateScheduleForm namespace={namespace} onDone={() => setCreating(false)} />
      )}

      <div className="panel">
        <div className="panel-head">
          <span>schedules</span>
          <span className="spacer" />
          <span className="chip">{schedules.data?.length ?? 0}</span>
        </div>

        {/* Stale rows plus a warning beat an error box that hides them. */}
        {schedules.error != null && schedules.data !== undefined ? (
          <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
            <StaleWarning error={schedules.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={schedules}
          what="schedules"
          isEmpty={(rows) => rows.length === 0}
          emptyTitle="No schedules in this namespace"
          emptyDetail={
            <>
              Nothing here fires on a timer yet. Create one above, or from the CLI:{' '}
              <code>nexusctl schedule create --spec example-etl --cron &quot;0 2 * * *&quot;</code>
            </>
          }
          loadingRows={5}
        >
          {(rows) => (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">status</th>
                    <th scope="col">name</th>
                    <th scope="col">spec</th>
                    <th scope="col">cron</th>
                    <th scope="col">next fire</th>
                    <th scope="col">last fire</th>
                    <th scope="col">overlap</th>
                    <th scope="col">actions</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((s) => (
                    <ScheduleRow
                      key={s.id}
                      schedule={s}
                      now={now}
                      pending={setEnabled.isPending && setEnabled.variables?.name === s.name}
                      onToggle={() => {
                        setActionError(null);
                        setEnabled.mutate(
                          { name: s.name, enabled: !s.enabled },
                          { onError: (e) => setActionError(errorMessage(e)) },
                        );
                      }}
                    />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}

function ScheduleRow({
  schedule,
  now,
  pending,
  onToggle,
}: {
  schedule: Schedule;
  now: number;
  pending: boolean;
  onToggle: () => void;
}) {
  // Enabled with no armed fire time is the row worth looking at twice: either a
  // replica is firing it at this instant, or one died mid-fire and left it
  // unarmed. Both are invisible if this renders as a plain em dash.
  const unarmed = schedule.enabled && schedule.next_fire_at === null;

  return (
    <tr>
      <td>
        {schedule.enabled ? (
          <span className="badge tone-success" title="This schedule fires on its cron.">
            enabled
          </span>
        ) : (
          <span className="badge tone-stopped" title="Disabled: it will not fire until re-enabled.">
            disabled
          </span>
        )}
      </td>
      <td>
        <Link to={`/schedules/${encodeURIComponent(schedule.name)}`} className="mono">
          {schedule.name}
        </Link>
      </td>
      <td className="mono">
        {schedule.spec_name}
        {schedule.spec_version !== undefined && <span className="faint"> v{schedule.spec_version}</span>}
      </td>
      <td>
        <div className="mono">{schedule.cron}</div>
        <div className="faint" style={{ fontSize: 'var(--text-xs)' }}>{schedule.timezone}</div>
      </td>
      <td className="nowrap">
        {/* A stored expression that no longer parses cannot fire at all, and this
            column is where someone will look when it stops. */}
        {schedule.cron_error ? (
          <span className="chip warn" title={schedule.cron_error}>
            cron broken
          </span>
        ) : schedule.next_fire_at ? (
          <span title={fmtTs(schedule.next_fire_at)}>{fmtTs(schedule.next_fire_at)}</span>
        ) : unarmed ? (
          <span
            className="chip warn"
            title="Enabled but not armed. Either a replica is firing it right now, or one died mid-fire and left it unarmed."
          >
            not armed
          </span>
        ) : (
          <span className="faint">{EMPTY}</span>
        )}
      </td>
      <td className="nowrap faint" title={schedule.last_fire_at ? fmtTs(schedule.last_fire_at) : ''}>
        {schedule.last_fire_at ? fmtAge(schedule.last_fire_at, now) : 'never'}
      </td>
      <td className="mono faint">{schedule.overlap_policy}</td>
      <td>
        <button type="button" onClick={onToggle} disabled={pending}>
          {schedule.enabled ? 'Disable' : 'Enable'}
        </button>
      </td>
    </tr>
  );
}
