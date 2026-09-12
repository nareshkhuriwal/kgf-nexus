/**
 * Backfill listing, progress and controls.
 *
 * A backfill is the one long-running thing on the schedule side, and the
 * operator question is always "how far has it got, and will it finish?" The
 * honest answer is the CURSOR against the interval — how far through the window
 * it has walked — not the number of runs created, which says nothing about how
 * much is left. So progress is measured against time, and runs_created is
 * reported beside it as the other half of the picture.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { errorMessage, useBackfillAction, useStartBackfill } from '../api/hooks';
import type { Backfill } from '../api/types';
import { isTerminal } from '../lib/states';
import { StateBadge } from './StateBadge';
import { ErrorBox } from './Status';
import { EMPTY, fmtTs, parseTs, shortId } from '../lib/format';

export function BackfillTable({
  namespace,
  scheduleName,
  backfills,
}: {
  namespace: string;
  scheduleName: string;
  backfills: Backfill[];
}) {
  const [actionError, setActionError] = useState<string | null>(null);
  const action = useBackfillAction(namespace, scheduleName);

  return (
    <>
      {actionError && (
        <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
          <ErrorBox error={actionError} what="that backfill action" />
        </div>
      )}
      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">state</th>
              <th scope="col">id</th>
              <th scope="col">interval</th>
              <th scope="col">progress</th>
              <th scope="col">runs</th>
              <th scope="col">started</th>
              <th scope="col">actions</th>
            </tr>
          </thead>
          <tbody>
            {backfills.map((b) => (
              <BackfillRow
                key={b.id}
                backfill={b}
                pending={action.isPending && action.variables?.id === b.id}
                onAction={(kind) => {
                  setActionError(null);
                  action.mutate(
                    { id: b.id, action: kind },
                    { onError: (e) => setActionError(errorMessage(e)) },
                  );
                }}
              />
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

function BackfillRow({
  backfill: b,
  pending,
  onAction,
}: {
  backfill: Backfill;
  pending: boolean;
  onAction: (action: 'pause' | 'resume' | 'cancel') => void;
}) {
  const terminal = isTerminal('backfill', b.state);

  return (
    <tr>
      <td>
        <StateBadge kind="backfill" state={b.state} />
      </td>
      <td className="mono">
        <Link to={`/backfills/${encodeURIComponent(b.id)}`} title={b.id}>
          {shortId(b.id, 12)}
        </Link>
      </td>
      <td className="nowrap">
        <div className="backfill-window-ends">
          <span title={b.from}>{fmtTs(b.from)}</span>
          <span aria-hidden="true">→</span>
          <span title={b.to}>{fmtTs(b.to)}</span>
        </div>
      </td>
      <td>
        <BackfillProgress backfill={b} />
      </td>
      <td className="num mono">
        {b.runs_created}
        {b.max_runs > 0 && <span className="faint"> / {b.max_runs}</span>}
      </td>
      <td className="nowrap faint" title={fmtTs(b.created_at)}>
        {fmtTs(b.created_at)}
      </td>
      <td>
        <div className="row" style={{ gap: 'var(--space-3)' }}>
          {b.state === 'RUNNING' && (
            <button
              type="button"
              onClick={() => onAction('pause')}
              disabled={pending}
              title="Stop generating new runs. Runs already created keep going."
            >
              Pause
            </button>
          )}
          {b.state === 'PAUSED' && (
            <button
              type="button"
              onClick={() => onAction('resume')}
              disabled={pending}
              title="Continue generating from the cursor."
            >
              Resume
            </button>
          )}
          {!terminal && (
            <CancelBackfillButton
              pending={pending}
              runsCreated={b.runs_created}
              onCancel={() => onAction('cancel')}
            />
          )}
          {terminal && <span className="faint">{EMPTY}</span>}
        </div>
      </td>
    </tr>
  );
}

/**
 * How far through the interval the cursor has walked.
 *
 * Measured against TIME rather than runs: a backfill of a year that has created
 * 40 runs tells an operator nothing about whether it is nearly done, while
 * "cursor at March" does. A null cursor on a live backfill means the interval is
 * exhausted, which is why it reads as complete rather than as missing data.
 */
function BackfillProgress({ backfill: b }: { backfill: Backfill }) {
  const from = parseTs(b.from);
  const to = parseTs(b.to);
  const cursor = parseTs(b.cursor_at);

  if (from === null || to === null || to <= from) {
    return <span className="faint">{EMPTY}</span>;
  }

  const fraction = cursor === null ? 1 : Math.max(0, Math.min(1, (cursor - from) / (to - from)));
  const percent = Math.round(fraction * 100);

  return (
    <div className="backfill-window">
      <div className="meter" title={`cursor ${percent}% through the interval`}>
        <span style={{ width: `${percent}%` }} />
      </div>
      <div className="sub">
        {cursor === null ? (
          'interval exhausted'
        ) : (
          <>
            {percent}% · cursor at <span className="mono">{fmtTs(b.cursor_at)}</span>
          </>
        )}
      </div>
    </div>
  );
}

/**
 * Cancel is confirmed, and the confirmation states the thing operators get
 * wrong: cancelling stops the backfill GENERATING runs, it does not cancel the
 * runs it has already created. Someone cancelling a runaway backfill to stop the
 * load needs to know they have a second job to do.
 */
function CancelBackfillButton({
  pending,
  runsCreated,
  onCancel,
}: {
  pending: boolean;
  runsCreated: number;
  onCancel: () => void;
}) {
  const [confirming, setConfirming] = useState(false);

  if (!confirming) {
    return (
      <button type="button" className="danger" onClick={() => setConfirming(true)}>
        Cancel
      </button>
    );
  }

  return (
    <div className="row" style={{ gap: 'var(--space-3)' }}>
      <span className="sub">
        Stops generating new runs. The {runsCreated} run{runsCreated === 1 ? '' : 's'} it already
        created keep going — cancel those separately.
      </span>
      <button type="button" className="danger" onClick={onCancel} disabled={pending}>
        {pending ? 'Cancelling…' : 'Yes, cancel'}
      </button>
      <button type="button" onClick={() => setConfirming(false)} disabled={pending}>
        Keep going
      </button>
    </div>
  );
}

/**
 * Start a backfill over an interval.
 *
 * max_active is the control that matters: a backfill of a year at one run per
 * hour is 8,760 runs, and without a concurrency cap it will take the whole
 * cluster. The field is therefore not optional-looking, and the form says what
 * the number does.
 */
export function StartBackfillForm({
  namespace,
  scheduleName,
  onDone,
}: {
  namespace: string;
  scheduleName: string;
  onDone: () => void;
}) {
  // Datetime-local values, converted to RFC3339 on submit: the gateway parses
  // RFC3339 strictly and rejects anything else with invalid_from / invalid_to.
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');
  const [maxActive, setMaxActive] = useState(2);
  const [maxRuns, setMaxRuns] = useState(100);

  const start = useStartBackfill(namespace, scheduleName);

  const fromTs = from === '' ? null : Date.parse(from);
  const toTs = to === '' ? null : Date.parse(to);
  // Caught here rather than server-side so the operator sees it beside the
  // fields: an inverted interval produces a backfill that generates nothing,
  // which looks like a broken scheduler rather than a typo.
  const intervalInvalid =
    fromTs !== null && toTs !== null && !Number.isNaN(fromTs) && !Number.isNaN(toTs) && toTs <= fromTs;

  return (
    <form
      className="col"
      style={{ gap: 'var(--space-5)' }}
      onSubmit={(e) => {
        e.preventDefault();
        if (intervalInvalid) return;
        start.mutate(
          {
            from: new Date(from).toISOString(),
            to: new Date(to).toISOString(),
            max_active: maxActive,
            max_runs: maxRuns,
          },
          { onSuccess: onDone },
        );
      }}
    >
      <div className="sub">
        Replays every tick of this schedule's cron between the two instants, as if it had been
        running then. Times are entered in your browser's local zone and sent as UTC.
      </div>

      <div className="grid-2">
        <label className="col" style={{ gap: 'var(--space-2)' }}>
          <span className="sub">from</span>
          <input
            type="datetime-local"
            value={from}
            onChange={(e) => setFrom(e.target.value)}
            aria-label="backfill from"
            required
          />
        </label>
        <label className="col" style={{ gap: 'var(--space-2)' }}>
          <span className="sub">to</span>
          <input
            type="datetime-local"
            value={to}
            onChange={(e) => setTo(e.target.value)}
            aria-label="backfill to"
            required
          />
          {intervalInvalid && (
            <span className="sub" role="alert">
              &quot;to&quot; must be after &quot;from&quot; — this interval contains no ticks.
            </span>
          )}
        </label>
      </div>

      <div className="grid-2">
        <label className="col" style={{ gap: 'var(--space-2)' }}>
          <span className="sub">max active</span>
          <input
            type="number"
            min={1}
            value={maxActive}
            onChange={(e) => setMaxActive(Number(e.target.value))}
            aria-label="max active"
          />
          <span className="sub">
            How many of its runs may be in flight at once. This is the throttle that keeps a long
            backfill from taking the whole cluster.
          </span>
        </label>
        <label className="col" style={{ gap: 'var(--space-2)' }}>
          <span className="sub">max runs</span>
          <input
            type="number"
            min={1}
            value={maxRuns}
            onChange={(e) => setMaxRuns(Number(e.target.value))}
            aria-label="max runs"
          />
          <span className="sub">
            Hard cap on the total. The backfill completes when it hits this, even if the interval
            has ticks left.
          </span>
        </label>
      </div>

      {start.error && <ErrorBox error={start.error} what="the backfill" />}

      <div className="row" style={{ gap: 'var(--space-3)' }}>
        <button type="submit" className="primary" disabled={start.isPending || intervalInvalid}>
          {start.isPending ? 'Starting…' : 'Start backfill'}
        </button>
        <button type="button" onClick={onDone} disabled={start.isPending}>
          Cancel
        </button>
      </div>
    </form>
  );
}
