// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One backfill, watched while it runs.
 *
 * Addressed by its own id rather than through its schedule, matching the
 * gateway: a backfill outlives the interval that named it, and an operator
 * tracks it by identity. It polls while it is live and stops once it reaches a
 * terminal state — a finished backfill will never change again, and polling one
 * from last Tuesday is pure waste.
 */

import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { errorMessage, useBackfill, useBackfillAction } from '../api/hooks';
import { isTerminal } from '../lib/states';
import { StateBadge } from '../components/StateBadge';
import { ErrorBox, Loading, StaleWarning } from '../components/Status';
import { EMPTY, durationBetween, fmtDurationMs, fmtTs, parseTs } from '../lib/format';

export function BackfillDetailPage({ namespace }: { namespace: string }) {
  const { backfillId = '' } = useParams();
  const [actionError, setActionError] = useState<string | null>(null);

  const backfill = useBackfill(namespace, backfillId);
  // The gateway resolves the owning schedule's name on this route, so the
  // action hook can be keyed on it and invalidate that schedule's backfill
  // list too -- not just this backfill's own query. It falls back to an empty
  // name when the gateway could not resolve it, which still invalidates what
  // this page reads.
  const action = useBackfillAction(namespace, backfill.data?.schedule_name ?? '');

  if (backfill.error && backfill.data === undefined) {
    return (
      <div className="col">
        <Link to="/schedules">← all schedules</Link>
        <ErrorBox error={backfill.error} what="that backfill" onRetry={backfill.refetch} />
      </div>
    );
  }
  if (!backfill.data) return <Loading rows={6} label="Loading backfill" />;

  const b = backfill.data;
  const terminal = isTerminal('backfill', b.state);
  const from = parseTs(b.from);
  const to = parseTs(b.to);
  const cursor = parseTs(b.cursor_at);
  const fraction =
    from === null || to === null || to <= from
      ? null
      : cursor === null
        ? 1
        : Math.max(0, Math.min(1, (cursor - from) / (to - from)));

  const run = (kind: 'pause' | 'resume' | 'cancel') => {
    setActionError(null);
    action.mutate({ id: b.id, action: kind }, { onError: (e) => setActionError(errorMessage(e)) });
  };

  return (
    <div className="col">
      <div className="row" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="row" style={{ gap: 'var(--space-4)' }}>
            {b.schedule_name ? (
              <Link to={`/schedules/${encodeURIComponent(b.schedule_name)}`} className="sub">
                ← {b.schedule_name}
              </Link>
            ) : (
              <Link to="/schedules" className="sub">
                ← all schedules
              </Link>
            )}
            <StateBadge kind="backfill" state={b.state} />
            <h1 style={{ margin: 0 }} className="mono">
              {b.id}
            </h1>
          </div>
          <div className="sub" style={{ marginTop: 'var(--space-1)' }}>
            {fmtTs(b.from)} → {fmtTs(b.to)} · started by {b.created_by} ·{' '}
            {terminal ? 'finished' : 'refreshes every 3s'}
          </div>
        </div>
        <span className="spacer" style={{ marginLeft: 'auto' }} />
        <div className="row" style={{ gap: 'var(--space-3)' }}>
          {b.state === 'RUNNING' && (
            <button type="button" onClick={() => run('pause')} disabled={action.isPending}>
              Pause
            </button>
          )}
          {b.state === 'PAUSED' && (
            <button type="button" onClick={() => run('resume')} disabled={action.isPending}>
              Resume
            </button>
          )}
          {!terminal && (
            <button
              type="button"
              className="danger"
              onClick={() => run('cancel')}
              disabled={action.isPending}
              title="Stops generating new runs. Runs already created keep going."
            >
              Cancel
            </button>
          )}
        </div>
      </div>

      {actionError && <ErrorBox error={actionError} what="that backfill action" />}
      {backfill.error != null ? <StaleWarning error={backfill.error} /> : null}

      <div className="panel">
        <div className="panel-head">
          <span>progress</span>
          <span className="spacer" />
          <span className="chip">
            {b.runs_created} run{b.runs_created === 1 ? '' : 's'} created
          </span>
        </div>
        <div className="panel-body col" style={{ gap: 'var(--space-4)' }}>
          {fraction === null ? (
            <div className="sub">This backfill's interval is empty, so there is nothing to walk.</div>
          ) : (
            <>
              <div className="meter">
                <span style={{ width: `${Math.round(fraction * 100)}%` }} />
              </div>
              <div className="backfill-window-ends">
                <span>{fmtTs(b.from)}</span>
                <span>{fmtTs(b.to)}</span>
              </div>
              <div className="sub">
                {/* Progress is the CURSOR through the interval, not a run count:
                    a year-long backfill that made 40 runs says nothing about how
                    much is left, while "cursor at March" does. */}
                {b.cursor_at ? (
                  <>
                    {Math.round(fraction * 100)}% through the interval · next tick to generate{' '}
                    <span className="mono">{fmtTs(b.cursor_at)}</span>
                  </>
                ) : (
                  'Interval exhausted — every tick in the window has been generated.'
                )}
              </div>
            </>
          )}
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>details</span>
        </div>
        <div className="panel-body">
          <dl className="kv">
            <dt>schedule</dt>
            <dd>
              {b.schedule_name ? (
                <Link to={`/schedules/${encodeURIComponent(b.schedule_name)}`}>
                  {b.schedule_name}
                </Link>
              ) : (
                <span className="sub">unresolved</span>
              )}
            </dd>
            <dt>schedule id</dt>
            <dd className="mono">{b.schedule_id}</dd>
            <dt>interval</dt>
            <dd>
              {fmtTs(b.from)} → {fmtTs(b.to)}
            </dd>
            <dt>cursor</dt>
            <dd>{b.cursor_at ? fmtTs(b.cursor_at) : `${EMPTY} (exhausted)`}</dd>
            <dt>runs created</dt>
            <dd>
              {b.runs_created}
              {b.max_runs > 0 && <span className="faint"> of at most {b.max_runs}</span>}
            </dd>
            <dt>max active</dt>
            <dd>{b.max_active}</dd>
            <dt>created</dt>
            <dd>{fmtTs(b.created_at)}</dd>
            <dt>updated</dt>
            <dd>{fmtTs(b.updated_at)}</dd>
            <dt>ended</dt>
            <dd>
              {b.ended_at ? (
                <>
                  {fmtTs(b.ended_at)}
                  <span className="faint">
                    {' '}
                    (ran {fmtDurationMs(durationBetween(b.created_at, b.ended_at))})
                  </span>
                </>
              ) : (
                <span className="faint">{EMPTY}</span>
              )}
            </dd>
            <dt>detail</dt>
            <dd>
              {/* Free-form server-side context — for a FAILED backfill this is
                  where the reason lives, so it is rendered rather than dropped. */}
              {b.detail && Object.keys(b.detail).length > 0 ? JSON.stringify(b.detail) : EMPTY}
            </dd>
          </dl>
        </div>
      </div>

      <div className="sub">
        Cancelling a backfill stops it generating runs; the runs it already created are ordinary
        runs and continue. Find them on the{' '}
        <Link to="/runs">runs</Link> page.
      </div>
    </div>
  );
}
