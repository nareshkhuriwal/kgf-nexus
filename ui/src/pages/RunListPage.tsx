// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Run list: filter by state and spec, newest first, live.
 *
 * Filters live in the URL query string so an operator can paste "the failed runs
 * of daily-etl" into a chat window and have the recipient see the same view.
 */

import { Link, useSearchParams } from 'react-router-dom';
import { RUN_STATES, type NodeState, type RunSummary } from '../api/types';
import { useRuns } from '../api/hooks';
import { StateBadge } from '../components/StateBadge';
import { AsyncPanel, StaleWarning } from '../components/Status';
import { EMPTY, durationBetween, fmtAge, fmtDurationMs, fmtTs, shortId } from '../lib/format';
import { isTerminal } from '../lib/states';

export function RunListPage({ namespace }: { namespace: string }) {
  const [params, setParams] = useSearchParams();
  const state = params.get('state') ?? '';
  const spec = params.get('spec') ?? '';

  const query = useRuns(namespace, { state: state || undefined, spec: spec || undefined });
  const now = Date.now();

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };

  return (
    <div className="col">
      <div>
        <h1>Runs</h1>
        <div className="sub">
          namespace <code>{namespace}</code> · newest first · refreshes every 4s
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <label className="row" style={{ gap: 'var(--space-3)' }}>
            <span style={{ textTransform: 'none', letterSpacing: 0 }}>state</span>
            <select value={state} onChange={(e) => setParam('state', e.target.value)}>
              <option value="">any</option>
              {RUN_STATES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </label>
          <label className="row" style={{ gap: 'var(--space-3)' }}>
            <span style={{ textTransform: 'none', letterSpacing: 0 }}>spec</span>
            <input
              type="search"
              placeholder="workflow name"
              value={spec}
              onChange={(e) => setParam('spec', e.target.value)}
              style={{ width: 190 }}
            />
          </label>
          <span className="spacer" />
          {(state || spec) && (
            <button
              onClick={() => {
                setParams(new URLSearchParams(), { replace: true });
              }}
            >
              clear filters
            </button>
          )}
          <span className="chip">{query.data?.length ?? 0} runs</span>
        </div>

        {/* Stale data plus a warning beats an error box that hides the data. */}
        {query.error != null && query.data !== undefined ? (
          <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
            <StaleWarning error={query.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={query}
          what="runs"
          isEmpty={(runs) => runs.length === 0}
          emptyTitle={state || spec ? 'No runs match these filters' : 'No runs yet'}
          emptyDetail={
            state || spec ? (
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
              <table>
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

function RunRow({ run, now }: { run: RunSummary; now: number }) {
  const terminal = isTerminal('run', run.state);
  // A running run's duration is measured against now, so the column ticks.
  const duration = terminal
    ? durationBetween(run.started_at ?? run.created_at, run.ended_at)
    : run.started_at
      ? now - Date.parse(run.started_at)
      : null;

  return (
    <tr>
      <td>
        <StateBadge kind="run" state={run.state} />
      </td>
      <td className="mono">
        <Link to={`/runs/${encodeURIComponent(run.id)}`} title={run.id}>
          {shortId(run.id, 12)}
        </Link>
      </td>
      <td>
        <div>{run.spec_name ?? <span className="faint">{shortId(run.spec_id, 10)}</span>}</div>
        {run.spec_version !== undefined && <div className="faint">v{run.spec_version}</div>}
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
    <span className="row" style={{ gap: 'var(--space-2)' }}>
      {entries.map(([s, n]) => (
        <span key={s} className="row" style={{ gap: 3 }}>
          <StateBadge kind="node" state={s} small showGlyph={false} />
          <span className="mono faint">{n}</span>
        </span>
      ))}
    </span>
  );
}
