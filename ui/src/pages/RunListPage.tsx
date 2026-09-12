// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Run list: filter by state and spec, newest first, live.
 *
 * Filters live in the URL query string so an operator can paste "the failed runs
 * of daily-etl" into a chat window and have the recipient see the same view.
 *
 * The filters sit in a bar of their own rather than in the panel's label band:
 * .panel-head is uppercase and letter-spaced, which is the wrong container for
 * controls people read and click, and fighting it needed an inline override on
 * every label.
 */

import { Link, useSearchParams } from 'react-router-dom';
import { RUN_STATES, type NodeState, type RunSummary } from '../api/types';
import { useRuns } from '../api/hooks';
import { PageHeader } from '../components/PageHeader';
import { StateBadge } from '../components/StateBadge';
import { AsyncPanel, StaleWarning } from '../components/Status';
import { EMPTY, durationBetween, fmtAge, fmtDurationMs, fmtTs, shortId } from '../lib/format';
import { isTerminal, stateVisual } from '../lib/states';

export function RunListPage({ namespace }: { namespace: string }) {
  const [params, setParams] = useSearchParams();
  const state = params.get('state') ?? '';
  const spec = params.get('spec') ?? '';

  const query = useRuns(namespace, { state: state || undefined, spec: spec || undefined });
  const now = Date.now();
  const filtered = Boolean(state || spec);

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };

  return (
    <div className="col">
      <PageHeader
        title="Runs"
        subtitle={
          <>
            namespace <code>{namespace}</code> · newest first · refreshes every 4s
          </>
        }
      />

      <div className="panel">
        <div className="filterbar">
          <label className="filter-field">
            <span className="filter-label">state</span>
            <select value={state} onChange={(e) => setParam('state', e.target.value)}>
              <option value="">any</option>
              {RUN_STATES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>
          <label className="filter-field">
            <span className="filter-label">spec</span>
            <input
              type="search"
              placeholder="workflow name"
              value={spec}
              onChange={(e) => setParam('spec', e.target.value)}
            />
          </label>
          {filtered && (
            <button
              onClick={() => {
                setParams(new URLSearchParams(), { replace: true });
              }}
            >
              clear filters
            </button>
          )}
          <span className="spacer" />
          {/* The count is the page's one figure worth reading at a glance: how
              much the current filters actually produced. */}
          <span className="filter-count">
            <span className="filter-count-n">{query.data?.length ?? EMPTY}</span>
            <span className="filter-count-label">
              {filtered ? 'matching runs' : 'runs'}
            </span>
          </span>
        </div>

        {/* Stale data plus a warning beats an error box that hides the data. */}
        {query.error != null && query.data !== undefined ? (
          <div className="panel-notice">
            <StaleWarning error={query.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={query}
          what="runs"
          isEmpty={(runs) => runs.length === 0}
          emptyTitle={filtered ? 'No runs match these filters' : 'No runs yet'}
          emptyDetail={
            filtered ? (
              'Loosen the filters, or check that the run you expect is in this namespace.'
            ) : (
              <>
                Submit a spec and create a run: <code>nexusctl run create --spec example-etl</code>
              </>
            )
          }
          loadingRows={6}
        >
          {(runs) => (
            <div className="table-scroll">
              <table className="table-runs">
                <thead>
                  <tr>
                    <th>state</th>
                    <th>run</th>
                    <th>spec</th>
                    <th>progress</th>
                    <th>trigger</th>
                    <th>created</th>
                    <th>duration</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.map((r) => (
                    <RunRow key={r.id} run={r} now={now} />
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

/**
 * Which rows carry a tone-coloured left rule.
 *
 * The rule is redundant with the badge in the same row — it exists so a
 * screenful can be triaged by edge colour without reading a label. It is derived
 * from the run's own tone rather than from a second list of state names here,
 * which is what stops the edge and the badge from ever disagreeing: adding a run
 * state to lib/states.ts gives it the right edge for free.
 *
 * Only the two tones that mean "something is wrong" get a rule. An in-flight or
 * succeeded run gets none, because a rule on every row is a rule on no row. And
 * neither tone is the accent: the accent is operator-switchable, so spending it
 * here would let a theme change make a FAILED run read as a finished one.
 */
function rowTone(run: RunSummary): string {
  const tone = stateVisual('run', run.state).tone;
  if (tone === 'danger') return 'row-failed';
  // A run that succeeded overall while a node inside it failed: the row's own
  // badge says SUCCEEDED, so this edge is the only thing that flags it.
  if ((run.node_counts?.FAILED ?? 0) > 0) return 'row-failed';
  // Stopped and ambiguous outcomes did not do what was asked, but they are not
  // the failure to look at first, so they take the blocked tone rather than
  // competing with it.
  if (tone === 'stopped' || tone === 'warning') return 'row-blocked';
  return '';
}

function RunRow({ run, now }: { run: RunSummary; now: number }) {
  const terminal = isTerminal('run', run.state);
  // A running run's duration is measured against now, so the column ticks.
  const duration = terminal
    ? durationBetween(run.started_at ?? run.created_at, run.ended_at)
    : run.started_at
      ? now - Date.parse(run.started_at)
      : null;

  return (
    <tr className={rowTone(run)}>
      <td>
        <StateBadge kind="run" state={run.state} />
      </td>
      <td className="mono">
        <Link to={`/runs/${encodeURIComponent(run.id)}`} title={run.id}>
          {shortId(run.id, 12)}
        </Link>
      </td>
      <td>
        <div className="cell-stack">
          <span>{run.spec_name ?? <span className="faint">{shortId(run.spec_id, 10)}</span>}</span>
          {run.spec_version !== undefined && (
            <span className="cell-sub">v{run.spec_version}</span>
          )}
        </div>
      </td>
      <td>
        <NodeCounts counts={run.node_counts} />
      </td>
      <td className="mono faint">{run.trigger_kind ?? EMPTY}</td>
      <td className="nowrap" title={fmtTs(run.created_at)}>
        {fmtAge(run.created_at, now)}
      </td>
      <td className="num">{fmtDurationMs(duration)}</td>
    </tr>
  );
}

/**
 * Compact per-state node tally. Only non-zero states are shown, so a healthy
 * run reads as "8 SUCCEEDED" rather than a row of zeroes to scan past.
 */
function NodeCounts({ counts }: { counts: Partial<Record<NodeState, number>> | undefined }) {
  if (!counts) return <span className="faint">{EMPTY}</span>;
  const entries = (Object.entries(counts) as [NodeState, number][]).filter(([, n]) => n > 0);
  if (entries.length === 0) return <span className="faint">{EMPTY}</span>;
  // Failure-bearing states first: they are why anyone is looking at this row.
  const rank: Record<string, number> = {
    FAILED: 0,
    UPSTREAM_FAILED: 1,
    RUNNING: 2,
    READY: 3,
    PENDING: 4,
    CANCELLED: 5,
    SKIPPED: 6,
    SUCCEEDED: 7,
  };
  entries.sort((a, b) => (rank[a[0]] ?? 9) - (rank[b[0]] ?? 9));
  return (
    <span className="node-tally">
      {entries.map(([s, n]) => (
        <span key={s} className="node-tally-item">
          <StateBadge kind="node" state={s} small showGlyph={false} />
          <span className="node-tally-n">{n}</span>
        </span>
      ))}
    </span>
  );
}
