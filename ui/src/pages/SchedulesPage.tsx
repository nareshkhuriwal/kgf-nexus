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
 * THE CREATE FORM IS URL-ADDRESSED: /schedules?new=1 opens it, and an optional
 * &spec=<name> starts it on a workflow. That is what makes scheduling reachable
 * from the WORKFLOW side of the console — a workflow's own page can link
 * straight into a pre-filled form instead of leaving an operator to remember
 * the spec name across a navigation, which is the trip that produces typos.
 *
 * SEARCH HERE IS CLIENT-SIDE, and that is a statement about the gateway rather
 * than a shortcut. /schedules takes only `limit` — there is no `search`
 * parameter to send — so a search box on this page can only ever filter the
 * rows already fetched. The table is told `mode: 'client'` so it says exactly
 * that on screen instead of letting an operator read a filtered count as a fact
 * about the namespace. If the endpoint grows a real search parameter, the fix
 * is one word here and the label follows.
 */

import { useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import {
  errorMessage,
  qk,
  useSchedules,
  useSetScheduleEnabled,
} from '../api/hooks';
import { api } from '../api/client';
import type { Schedule } from '../api/types';
import { AsyncPanel, ErrorBox, StaleWarning } from '../components/Status';
import { PageHeader } from '../components/PageHeader';
import { BulkActionBar } from '../components/BulkActionBar';
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
  /**
   * The create form's open state lives in the URL, not in useState.
   *
   * It has to, because scheduling is now reachable from a WORKFLOW: a "Schedule
   * this workflow" button elsewhere in the console links to
   * /schedules?new=1&spec=<name>, and that link can only open this form and
   * pre-fill it if the form's state is addressable. The same choice RunDetailPage
   * makes for `?node=`, and it pays the same dividends — the half-filled form
   * survives a reload, and the link is something an operator can paste to a
   * colleague rather than a sequence of clicks they have to describe.
   *
   * `new=1` and `spec=` are separate parameters rather than one, because they
   * answer different questions: `new` opens the form, `spec` says which workflow
   * it starts on. A link may carry the first without the second (the plain
   * "create a schedule" entry point), and a stale `spec` with no `new` simply
   * does nothing rather than opening a form nobody asked for.
   */
  const [params, setParams] = useSearchParams();
  const creating = params.get('new') === '1';
  const presetSpec = params.get('spec') ?? '';

  const [actionError, setActionError] = useState<string | null>(null);

  /**
   * The selected schedule IDS. Ids rather than rows because this list polls
   * every 10s and a selection of row objects would empty itself on each refetch.
   */
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());

  function setCreating(open: boolean) {
    // `replace` so opening and closing the form does not build a back-button
    // history of the same page in two states; Back should leave the schedule
    // list, not step through it.
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (open) {
          next.set('new', '1');
        } else {
          next.delete('new');
          // The preset goes with the form it belonged to. Leaving it behind
          // would silently pre-fill the NEXT form an operator opened with a
          // workflow they chose minutes ago and have since moved on from.
          next.delete('spec');
        }
        return next;
      },
      { replace: true },
    );
  }

  const qc = useQueryClient();
  const schedules = useSchedules(namespace);
  const setEnabled = useSetScheduleEnabled(namespace);
  const now = Date.now();

  /**
   * The rows the bulk bar may act on: the selected ones it can still RESOLVE.
   *
   * Resolved against the fetched list rather than carried as a parallel array,
   * so an id whose row has gone (deleted by someone else, or dropped by a
   * refetch) is simply absent here — not counted, not actioned, and not reported
   * as a phantom failure. The bar never claims to act on a row the page cannot
   * hand it.
   *
   * Every schedule is deletable, so there is no eligibility filter to apply:
   * unlike a revoked token or a terminal run, a disabled schedule is still a
   * stored row that DELETE removes.
   */
  const list = schedules.data ?? [];
  const selectedSchedules = useMemo(
    () => list.filter((s) => selected.has(s.id)),
    [list, selected],
  );

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
          <span
            className="badge tone-success"
            title="This schedule fires on its cron."
          >
            enabled
          </span>
        ) : (
          <span
            className="badge tone-stopped"
            title="Disabled: it will not fire until re-enabled."
          >
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
          {s.spec_version !== undefined && (
            <span className="faint"> v{s.spec_version}</span>
          )}
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
            disabled={
              setEnabled.isPending && setEnabled.variables?.name === s.name
            }
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
            cron-triggered workflows in <code>{namespace}</code> · refreshes
            every 10s
          </>
        }
        actions={
          <button
            type="button"
            className="primary"
            onClick={() => setCreating(!creating)}
          >
            {creating ? 'Cancel' : 'New schedule'}
          </button>
        }
      />

      {actionError && <ErrorBox error={actionError} what="that action" />}

      {creating && (
        <CreateScheduleForm
          /* Keyed on the preset so following a second "Schedule this workflow"
             link while the form is already open re-seeds it. Without the key
             the form is already mounted, initialSpecName is only an INITIAL
             value, and the new link would appear to do nothing at all. */
          key={presetSpec}
          namespace={namespace}
          initialSpecName={presetSpec}
          onDone={() => setCreating(false)}
        />
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
              Nothing here fires on a timer yet. Create one above, or from the
              CLI:{' '}
              <code>
                nexusctl schedule create --spec example-etl --cron &quot;0 2 * *
                *&quot;
              </code>
            </>
          }
          loadingRows={5}
        >
          {(rows) => (
            <>
              {/*
                DELETE, and the word is exact. A schedule is a stored rule, not
                a record of anything that happened: removing it stops future
                fires and takes its own tick history with it, which the
                confirmation says because "Delete 3 schedules" does not.

                The runs it already CREATED are untouched — they are their own
                immutable rows and are never reachable from here. That is worth
                stating in the consequence too, because an operator hesitating
                over this button is usually hesitating about exactly that.
              */}
              <BulkActionBar<Schedule>
                selected={selectedSchedules}
                rowId={(s) => s.id}
                rowLabel={(s) => s.name}
                noun={['schedule', 'schedules']}
                actions={[
                  {
                    verb: 'Delete',
                    destructive: true,
                    consequence:
                      'Each schedule stops firing and is removed permanently, along with its own fire history. Runs it has already created are not affected.',
                    run: (s) => api.deleteSchedule(namespace, s.name),
                  },
                ]}
                onClear={() => setSelected(new Set())}
                onDone={() => {
                  /* Refetch rather than remove the rows optimistically. A
                     delete the gateway refused must still be on screen —
                     dropping it locally would show the operator a table that
                     disagrees with the failure report directly above it. */
                  void qc.invalidateQueries({
                    queryKey: qk.schedules(namespace),
                  });
                }}
              />
              <DataTable
                rows={rows}
                columns={columns}
                rowKey={(s) => s.id}
                noun="schedules"
                count={SCHEDULE_COUNT}
                /* Selection. The count kind is `capped`, so DataTable's select-all
                 says "loaded" rather than claiming the namespace: /schedules
                 takes a `limit` and the client cannot speak for rows it was
                 never sent. */
                selection={{
                  selected,
                  onChange: setSelected,
                  rowId: (s) => s.id,
                  label: (s) => s.name,
                }}
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
            </>
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
    return (
      <span title={fmtTs(schedule.next_fire_at)}>
        {fmtTs(schedule.next_fire_at)}
      </span>
    );
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
