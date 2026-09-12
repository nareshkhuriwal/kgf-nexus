/**
 * Overview: the first screen, and the one an operator lands on mid-incident.
 *
 * The organising constraint is that this page aggregates FOUR independent
 * queries, and the failure mode that would make it worthless is one dead
 * endpoint blanking the whole dashboard. So every panel owns its own
 * loading/error/empty state and renders what it has; a 500 from /capacity costs
 * the capacity panel and nothing else.
 *
 * The second constraint is honesty about provenance. Two of the six things the
 * overview is asked to show are not backend metrics:
 *   - queue AGE is derived here from the oldest non-terminal run, because the
 *     platform exposes queue DEPTH (capacity.pending_attempts) and no age at all;
 *   - service health is not exposed through the gateway in any form, so what
 *     this page reports is API reachability from this browser plus the worker
 *     states the fleet already publishes.
 * Both are labelled as what they are. A dashboard that quietly upgrades an
 * inference into a measurement is worse than one with a visible gap, because
 * the gap is the thing an operator needs to know about.
 */

import { Link } from 'react-router-dom';
import { useCapacity, useRuns, useSchedules, useWorkers } from '../api/hooks';
import type { RunSummary, Worker, WorkerState } from '../api/types';
import { PageHeader } from '../components/PageHeader';
import { StateBadge } from '../components/StateBadge';
import { CapacityGrid, deriveCapacity } from '../components/CapacityGrid';
import { AsyncPanel, Empty, ErrorBox, StaleWarning } from '../components/Status';
import { EMPTY, fmtAge, fmtDurationMs, fmtTs, parseTs, shortId } from '../lib/format';
import { isTerminal } from '../lib/states';

/**
 * Enough rows to characterise the namespace without turning the landing page
 * into a second run list — the run explorer is one click away and does paging
 * properly. The split into active/recent happens client-side from this window,
 * so it has to be wide enough that a busy namespace still shows recent history
 * below its in-flight work.
 */
const RUN_WINDOW = 50;

/** Only enough failures to answer "is something broken right now?". */
const FAILURE_WINDOW = 5;

/** What each panel shows at a glance before you drill into it. */
const ACTIVE_PREVIEW = 6;
const RECENT_PREVIEW = 6;

export function OverviewPage({ namespace }: { namespace: string }) {
  const runs = useRuns(namespace, { limit: RUN_WINDOW });
  const failures = useRuns(namespace, { state: 'FAILED', limit: FAILURE_WINDOW });
  const workers = useWorkers(namespace);
  const capacity = useCapacity(namespace);
  const schedules = useSchedules(namespace);
  const now = Date.now();

  const active = (runs.data ?? []).filter((r) => !isTerminal('run', r.state));
  const recent = (runs.data ?? []).filter((r) => isTerminal('run', r.state));

  return (
    <div className="col">
      <PageHeader
        title="Overview"
        subtitle={
          <>
            namespace <code>{namespace}</code> · runs refresh every 4s, fleet every 5s
          </>
        }
      />

      <HealthPanel
        queries={[
          { what: 'runs', query: runs, to: '/runs' },
          { what: 'workers', query: workers, to: '/workers' },
          { what: 'capacity', query: capacity, to: '/workers' },
          { what: 'schedules', query: schedules, to: '/schedules' },
        ]}
        workers={workers.data}
      />

      <div className="grid-2">
        <QueuePanel
          capacity={capacity}
          oldestWaiting={oldestWaiting(runs.data)}
          activeCount={active.length}
          now={now}
        />
        <WorkerPanel workers={workers} capacity={capacity} />
      </div>

      <CapacityPanel capacity={capacity} workers={workers} />

      <FailurePanel query={failures} now={now} />

      <div className="grid-2">
        <RunPanel
          title="active runs"
          query={runs}
          rows={active.slice(0, ACTIVE_PREVIEW)}
          total={active.length}
          now={now}
          emptyTitle="Nothing in flight"
          emptyDetail="No run is PENDING, RUNNING, PAUSED or CANCELLING in this namespace."
          to="/runs"
          seeAll="see all runs"
        />
        <RunPanel
          title="recently finished"
          query={runs}
          rows={recent.slice(0, RECENT_PREVIEW)}
          total={recent.length}
          now={now}
          emptyTitle="No finished runs yet"
          emptyDetail={
            <>
              Submit one: <code>nexusctl run create --spec example-etl</code>
            </>
          }
          to="/runs"
          seeAll="see all runs"
        />
      </div>
    </div>
  );
}

/** The oldest run that has not reached a terminal state, or null. */
function oldestWaiting(runs: RunSummary[] | undefined): RunSummary | null {
  const waiting = (runs ?? []).filter((r) => !isTerminal('run', r.state));
  if (waiting.length === 0) return null;
  return waiting.reduce((oldest, r) =>
    (parseTs(r.created_at) ?? Infinity) < (parseTs(oldest.created_at) ?? Infinity) ? r : oldest,
  );
}

/**
 * What this browser can actually observe about the platform.
 *
 * Deliberately NOT called "service health". The gateway exposes no health or
 * dependency endpoint, so the console cannot say whether the scheduler, the DAG
 * engine or Postgres are well — only whether each read it makes is answered.
 * The panel says so in its own subtitle, because an operator who reads a green
 * row as "the fleet is healthy" has been misled by this screen, not informed.
 */
function HealthPanel({
  queries,
  workers,
}: {
  queries: { what: string; query: { error: unknown; isLoading: boolean; data: unknown }; to: string }[];
  workers: Worker[] | undefined;
}) {
  const failing = queries.filter((q) => q.query.error != null);

  return (
    <div className="panel">
      <div className="panel-head">
        <span>api reachability</span>
        <span className="spacer" />
        <span className="chip" title="Counted from this browser's own requests.">
          {queries.length - failing.length}/{queries.length} endpoints answering
        </span>
      </div>
      <div className="panel-body">
        <div className="sub" style={{ marginBottom: 'var(--space-5)' }}>
          Observed from this browser only. The gateway exposes no service-health or dependency
          endpoint, so this is not a fleet health check: a row reads “answering” when the console’s
          own request to it succeeded, and says nothing about the services behind it.
        </div>

        <div className="grid-2">
          {queries.map((q) => (
            <Link key={q.what} to={q.to} className="ov-health-row">
              <span
                className={`pulse ${q.query.error != null ? 'off' : q.query.isLoading ? 'stale' : ''}`}
                aria-hidden="true"
              />
              <span className="mono">{q.what}</span>
              <span className="spacer" />
              <span className={q.query.error != null ? 'tone-text-danger' : 'faint'}>
                {q.query.error != null
                  ? 'not answering'
                  : q.query.isLoading
                    ? 'checking…'
                    : 'answering'}
              </span>
            </Link>
          ))}
        </div>

        {/* Worker states ARE published by the platform, so this half is a real
            measurement rather than an inference, and is kept visually separate
            from the reachability rows above for that reason. */}
        <div className="ov-divider" />
        <div className="attempt-section-title">worker states (reported by the control plane)</div>
        {workers === undefined ? (
          <span className="faint">—</span>
        ) : workers.length === 0 ? (
          <span className="faint">No workers registered.</span>
        ) : (
          <div className="row" style={{ gap: 'var(--space-5)' }}>
            {workerStateCounts(workers).map(([state, n]) => (
              <Link key={state} to="/workers" className="row ov-count-link" style={{ gap: 'var(--space-2)' }}>
                <StateBadge kind="worker" state={state} small />
                <span className="mono">{n}</span>
              </Link>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

/** Non-zero worker-state tallies, worst first — DEAD is why you are looking. */
function workerStateCounts(workers: Worker[]): [WorkerState, number][] {
  const order: WorkerState[] = ['DEAD', 'QUARANTINED', 'DRAINING', 'ACTIVE'];
  return order
    .map((s) => [s, workers.filter((w) => w.state === s).length] as [WorkerState, number])
    .filter(([, n]) => n > 0);
}

/**
 * Queue depth (measured) and queue age (derived).
 *
 * These are two different kinds of claim on one tile and the difference is
 * load-bearing: pending_attempts is a number the scheduler publishes, whereas
 * the age is this console subtracting the oldest non-terminal run's created_at
 * from the browser clock. That age is an UPPER BOUND on how long anything has
 * been queued — a run can be old because it is long-running, not because it is
 * starved — so it is labelled derived and the run it came from is linked, which
 * is the only way to check the inference.
 */
function QueuePanel({
  capacity,
  oldestWaiting: oldest,
  activeCount,
  now,
}: {
  capacity: { data: { pending_attempts?: number } | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  oldestWaiting: RunSummary | null;
  activeCount: number;
  now: number;
}) {
  const depth = capacity.data?.pending_attempts;
  const age = oldest ? now - (parseTs(oldest.created_at) ?? now) : null;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>queue</span>
        <span className="spacer" />
        <Link to="/runs" className="chip">
          {activeCount} active
        </Link>
      </div>
      <div className="panel-body">
        <div className="ov-stats">
          <div>
            <div className="attempt-section-title">pending attempts</div>
            {capacity.error != null && capacity.data === undefined ? (
              <span className="faint">unavailable</span>
            ) : (
              <strong className="ov-stat mono">{depth ?? EMPTY}</strong>
            )}
            <div className="sub">admitted, not yet dispatched</div>
          </div>
          <div>
            <div className="attempt-section-title">oldest waiting run</div>
            <strong className="ov-stat mono">{age === null ? EMPTY : fmtDurationMs(age)}</strong>
            <div className="sub">
              {oldest ? (
                <>
                  derived from{' '}
                  <Link to={`/runs/${encodeURIComponent(oldest.id)}`} title={oldest.id}>
                    {shortId(oldest.id, 8)}
                  </Link>
                </>
              ) : (
                'nothing waiting'
              )}
            </div>
          </div>
        </div>

        <div className="ov-derived-note">
          <strong>Derived, not measured.</strong> There is no queue-age endpoint. The age above is
          this console subtracting the oldest non-terminal run’s <code>created_at</code> from the
          browser clock, so it is an upper bound on wait time — a long-running run is old without
          being starved. Only <code>pending_attempts</code> is reported by the platform.
        </div>
      </div>
    </div>
  );
}

/** Fleet availability: how many workers, and how many can actually take work. */
function WorkerPanel({
  workers,
  capacity,
}: {
  workers: { data: Worker[] | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  capacity: { data: { workers?: number; worker_count?: number } | undefined; error: unknown };
}) {
  const list = workers.data;
  // ACTIVE is the only state that accepts new work: DRAINING finishes what it
  // holds, DEAD and QUARANTINED are out of scheduling entirely. Reporting the
  // registered total as "available" is the mistake this split exists to avoid.
  const available = list?.filter((w) => w.state === 'ACTIVE').length;
  // The two sources disagree when the liveness sweep is mid-flight; showing both
  // is more useful than picking one and hiding the skew.
  const reported = capacity.data?.workers ?? capacity.data?.worker_count;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>workers</span>
        <span className="spacer" />
        <Link to="/workers" className="chip">
          fleet detail
        </Link>
      </div>
      <AsyncPanel
        query={workers}
        what="workers"
        isEmpty={(w) => w.length === 0}
        emptyTitle="No workers registered"
        emptyDetail="Nothing can be dispatched until an agent registers."
        loadingRows={2}
      >
        {() => (
          <div className="panel-body">
            <div className="ov-stats">
              <div>
                <div className="attempt-section-title">accepting work</div>
                <strong className="ov-stat mono">{available ?? EMPTY}</strong>
                <div className="sub">ACTIVE workers</div>
              </div>
              <div>
                <div className="attempt-section-title">registered</div>
                <strong className="ov-stat mono">{list?.length ?? EMPTY}</strong>
                <div className="sub">
                  {reported !== undefined && reported !== list?.length ? (
                    <span className="tone-text-warning">
                      /capacity says {reported} — liveness sweep may be mid-flight
                    </span>
                  ) : (
                    'all states'
                  )}
                </div>
              </div>
            </div>
          </div>
        )}
      </AsyncPanel>
    </div>
  );
}

/**
 * Resource reservations. Falls back to summing the worker rows exactly as the
 * fleet page does, so a missing /capacity costs precision rather than the panel.
 */
function CapacityPanel({
  capacity,
  workers,
}: {
  capacity: { data: Parameters<typeof CapacityGrid>[0]['summary'] | undefined; error: unknown; refetch?: () => void };
  workers: { data: Worker[] | undefined };
}) {
  return (
    <div className="panel">
      <div className="panel-head">
        <span>resource reservations</span>
        <span className="spacer" />
        <span className="sub" style={{ textTransform: 'none', letterSpacing: 0 }}>
          advertised = registered capacity · committed = sum of active reservations
        </span>
      </div>
      <div className="panel-body">
        {capacity.error != null && capacity.data === undefined ? (
          workers.data ? (
            <CapacityGrid summary={deriveCapacity(workers.data)} derived showWorkerCount={false} />
          ) : (
            <ErrorBox error={capacity.error} what="capacity summary" onRetry={capacity.refetch} />
          )
        ) : capacity.data ? (
          <CapacityGrid summary={capacity.data} showWorkerCount={false} />
        ) : (
          <div className="sub">loading…</div>
        )}
      </div>
    </div>
  );
}

/** Recent failures — the reason most people open this page. */
function FailurePanel({
  query,
  now,
}: {
  query: { data: RunSummary[] | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  now: number;
}) {
  return (
    <div className="panel">
      <div className="panel-head">
        <span>recent failures</span>
        <span className="spacer" />
        <Link to="/runs?state=FAILED" className="chip">
          all failed runs
        </Link>
      </div>

      {query.error != null && query.data !== undefined ? (
        <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
          <StaleWarning error={query.error} />
        </div>
      ) : null}

      <AsyncPanel
        query={query}
        what="failed runs"
        isEmpty={(rows) => rows.length === 0}
        emptyTitle="No failed runs"
        emptyDetail="Nothing in this namespace has terminally failed."
        loadingRows={3}
      >
        {(rows) => (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>state</th>
                  <th>run</th>
                  <th>spec</th>
                  <th>failed</th>
                  <th>duration</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <StateBadge kind="run" state={r.state} />
                    </td>
                    <td className="mono">
                      <Link to={`/runs/${encodeURIComponent(r.id)}`} title={r.id}>
                        {shortId(r.id, 12)}
                      </Link>
                    </td>
                    <td>
                      <Link to={`/runs?spec=${encodeURIComponent(r.spec_name ?? '')}`}>
                        {r.spec_name ?? <span className="faint">{shortId(r.spec_id, 10)}</span>}
                      </Link>
                      {r.spec_version !== undefined && <div className="faint">v{r.spec_version}</div>}
                    </td>
                    <td className="nowrap" title={fmtTs(r.ended_at)}>
                      {fmtAge(r.ended_at ?? r.created_at, now)}
                    </td>
                    <td className="num">
                      {fmtDurationMs(runDuration(r, now))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncPanel>
    </div>
  );
}

/** A run still in flight is measured against now, so the column ticks. */
function runDuration(run: RunSummary, now: number): number | null {
  const start = parseTs(run.started_at ?? run.created_at);
  if (start === null) return null;
  const end = parseTs(run.ended_at) ?? (isTerminal('run', run.state) ? null : now);
  return end === null ? null : end - start;
}

/**
 * A compact run list. The active and recent panels are the same shape over a
 * different slice, so they share one component: two near-identical run tables
 * that drift apart is precisely the inconsistency the state table elsewhere in
 * this codebase exists to prevent.
 */
function RunPanel({
  title,
  query,
  rows,
  total,
  now,
  emptyTitle,
  emptyDetail,
  to,
  seeAll,
}: {
  title: string;
  query: { data: RunSummary[] | undefined; error: unknown; isLoading: boolean; refetch?: () => void };
  rows: RunSummary[];
  total: number;
  now: number;
  emptyTitle: string;
  emptyDetail: React.ReactNode;
  to: string;
  seeAll: string;
}) {
  return (
    <div className="panel">
      <div className="panel-head">
        <span>{title}</span>
        <span className="spacer" />
        <Link to={to} className="chip">
          {seeAll}
        </Link>
      </div>

      {query.error != null && query.data !== undefined ? (
        <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
          <StaleWarning error={query.error} />
        </div>
      ) : null}

      {/* The emptiness that matters is of this SLICE, not of the response: a
          namespace with only finished runs has a non-empty payload and an empty
          active panel, and must say "nothing in flight" rather than show a
          skeleton forever. */}
      {query.error != null && query.data === undefined ? (
        <div className="panel-body">
          <ErrorBox error={query.error} what={title} onRetry={query.refetch} />
        </div>
      ) : query.data === undefined ? (
        <AsyncPanel query={query} what={title} loadingRows={3}>
          {() => null}
        </AsyncPanel>
      ) : rows.length === 0 ? (
        <Empty title={emptyTitle} detail={emptyDetail} />
      ) : (
        <>
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th>state</th>
                  <th>run</th>
                  <th>spec</th>
                  <th>age</th>
                  <th>duration</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <StateBadge kind="run" state={r.state} />
                    </td>
                    <td className="mono">
                      <Link to={`/runs/${encodeURIComponent(r.id)}`} title={r.id}>
                        {shortId(r.id, 12)}
                      </Link>
                    </td>
                    <td>
                      <Link to={`/runs?spec=${encodeURIComponent(r.spec_name ?? '')}`}>
                        {r.spec_name ?? <span className="faint">{shortId(r.spec_id, 10)}</span>}
                      </Link>
                    </td>
                    <td className="nowrap" title={fmtTs(r.created_at)}>
                      {fmtAge(r.created_at, now)}
                    </td>
                    <td className="num">{fmtDurationMs(runDuration(r, now))}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {total > rows.length && (
            <div className="panel-body">
              <Link to={to} className="sub">
                {total - rows.length} more — {seeAll}
              </Link>
            </div>
          )}
        </>
      )}
    </div>
  );
}
