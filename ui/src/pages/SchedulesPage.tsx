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
 *
 * SEARCH HERE IS CLIENT-SIDE, and that is a statement about the gateway rather
 * than a shortcut. /schedules takes only `limit` — there is no `search`
 * parameter to send — so a search box on this page can only ever filter the
 * rows already fetched. The table is told `mode: 'client'` so it says exactly
 * that on screen instead of letting an operator read a filtered count as a fact
 * about the namespace. If the endpoint grows a real search parameter, the fix
 * is one word here and the label follows.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { errorMessage, useSchedules, useSetScheduleEnabled } from '../api/hooks';
import type { Schedule } from '../api/types';
import { AsyncPanel, ErrorBox, StaleWarning } from '../components/Status';
import { PageHeader } from '../components/PageHeader';
import { DataTable } from '../components/DataTable';
import type { Column, RowCountKind } from '../components/DataTable';
import { CreateScheduleForm } from './CreateScheduleForm';
import { EMPTY, fmtAge, fmtTs } from '../lib/format';

/**
 * MUST MATCH api/client.ts listSchedules' default `limit`.
 *
 * Same reasoning as the catalog's: 100 rows back from a 100-row request is
 * "at least 100", not "100". The distinction costs nothing to state and is the
 * difference between a count an operator can plan against and one they cannot.
 */
const SCHEDULE_LIMIT = 100;
const SCHEDULE_COUNT: RowCountKind = { kind: 'capped', limit: SCHEDULE_LIMIT };

export function SchedulesPage({ namespace }: { namespace: string }) {
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const schedules = useSchedules(namespace);
  const setEnabled = useSetScheduleEnabled(namespace);
  const now = Date.now();

  const columns: Column<Schedule>[] = [
    {
      key: 'status',
      header: 'status',
      // Sorted so that the disabled schedules gather at one end. enabled-first
      // ascending, because "what is armed" is the question the page exists for.
      sort: (a, b) => Number(b.enabled) - Number(a.enabled),
      cell: (s) =>
        // Both states are badges, and neither is the accent: enabled/disabled is
        // the row's state, so it takes a --tone-*, which no theme change can
        // reassign.
        s.enabled ? (
          <span className="badge tone-success" title="This schedule fires on its cron.">
            enabled
          </span>
        ) : (
          <span className="badge tone-stopped" title="Disabled: it will not fire until re-enabled.">
            disabled
          </span>
        ),
    },
    {
      key: 'name',
      header: 'name',
      sort: (a, b) => a.name.localeCompare(b.name),
      cell: (s) => (
        <Link to={`/schedules/${encodeURIComponent(s.name)}`} className="mono">
          {s.name}
        </Link>
      ),
    },
    {
      key: 'spec',
      header: 'spec',
      cellClass: 'mono',
      sort: (a, b) => a.spec_name.localeCompare(b.spec_name),
      cell: (s) => (
        <>
          {s.spec_name}
          {s.spec_version !== undefined && <span className="faint"> v{s.spec_version}</span>}
        </>
      ),
    },
    {
      key: 'cron',
      header: 'cron',
      // The expression and the zone it is evaluated in are one fact split over
      // two lines: the zone qualifies the expression, it is not a separate
      // column's worth of value.
      cell: (s) => (
        <div className="cell-stack">
          <span className="mono">{s.cron}</span>
          <span className="cell-sub mono">{s.timezone}</span>
        </div>
      ),
    },
    {
      key: 'next_fire',
      header: 'next fire',
      cellClass: 'nowrap',
      /**
       * Sorted so the soonest fire is first, and the rows that CANNOT fire sort
       * last rather than first. A null next_fire_at parses as NaN, which a naive
       * subtraction would scatter unpredictably — so the unfireable rows are
       * given Infinity explicitly. They are not "soonest"; they are the ones to
       * look at after the timeline, which the flags in the cell already say.
       */
      sort: (a, b) => {
        const at = a.next_fire_at ? Date.parse(a.next_fire_at) : Infinity;
        const bt = b.next_fire_at ? Date.parse(b.next_fire_at) : Infinity;
        return at - bt;
      },
      cell: (s) => <NextFire schedule={s} />,
    },
    {
      key: 'last_fire',
      header: 'last fire',
      cellClass: 'nowrap faint',
      // A schedule that has never fired sorts as the oldest, not the newest: it
      // is further from "recently ran" than anything that has run at all.
      sort: (a, b) =>
        (a.last_fire_at ? Date.parse(a.last_fire_at) : -Infinity) -
        (b.last_fire_at ? Date.parse(b.last_fire_at) : -Infinity),
      cell: (s) => (
        <span title={s.last_fire_at ? fmtTs(s.last_fire_at) : ''}>
          {s.last_fire_at ? fmtAge(s.last_fire_at, now) : 'never'}
        </span>
      ),
    },
    {
      key: 'overlap',
      header: 'overlap',
      cellClass: 'mono faint',
      cell: (s) => s.overlap_policy,
    },
    {
      key: 'actions',
      header: 'actions',
      // Not sortable: a column of buttons has no order, and offering one would
      // be a control that does nothing legible.
      cell: (s) => (
        <div className="cell-actions">
          <button
            type="button"
            onClick={() => {
              setActionError(null);
              setEnabled.mutate(
                { name: s.name, enabled: !s.enabled },
                { onError: (e) => setActionError(errorMessage(e)) },
              );
            }}
            disabled={setEnabled.isPending && setEnabled.variables?.name === s.name}
          >
            {s.enabled ? 'Disable' : 'Enable'}
          </button>
        </div>
      ),
    },
  ];

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
        </div>

        {/* Stale rows plus a warning beat an error box that hides them. */}
        {schedules.error != null && schedules.data !== undefined ? (
          <div className="panel-notice" style={{ paddingBottom: 0 }}>
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
            <DataTable
              rows={rows}
              columns={columns}
              rowKey={(s) => s.id}
              noun="schedules"
              count={SCHEDULE_COUNT}
              /* Client mode: /schedules has no `search` parameter, so this can
                 only filter what was fetched — and the table says so rather
                 than letting the filtered count read as a namespace total. The
                 spec name is in the haystack alongside the schedule's own name
                 because "which schedules fire example-etl" is the second thing
                 anyone asks on this page. */
              search={{
                mode: 'client',
                fields: (s) => [s.name, s.spec_name, s.cron, s.timezone],
              }}
            />
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}

/**
 * The next-fire cell, which carries three distinct claims and must not blur
 * them into an em dash.
 *
 * Kept a component rather than an inline ternary in the column because the
 * unarmed case needs a named condition: enabled with no armed fire time is the
 * row worth looking at twice — either a replica is firing it at this instant, or
 * one died mid-fire and left it unarmed. Both are invisible if this renders as a
 * plain dash.
 */
function NextFire({ schedule }: { schedule: Schedule }) {
  const unarmed = schedule.enabled && schedule.next_fire_at === null;

  // A stored expression that no longer parses cannot fire at all, and this
  // column is where someone will look when it stops.
  if (schedule.cron_error) {
    return (
      <span className="chip warn" title={schedule.cron_error}>
        cron broken
      </span>
    );
  }
  if (schedule.next_fire_at) {
    return <span title={fmtTs(schedule.next_fire_at)}>{fmtTs(schedule.next_fire_at)}</span>;
  }
  if (unarmed) {
    return (
      <span
        className="chip warn"
        title="Enabled but not armed. Either a replica is firing it right now, or one died mid-fire and left it unarmed."
      >
        not armed
      </span>
    );
  }
  return <span className="faint">{EMPTY}</span>;
}
