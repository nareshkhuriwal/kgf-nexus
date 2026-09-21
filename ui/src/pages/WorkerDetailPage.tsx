import { Permit } from '../components/Permissions';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One worker: what it is, what it is running, and what it just ran.
 *
 * The fleet table answers "how much capacity is there and is any of it stuck".
 * It cannot answer the question an operator asks next, which is the whole
 * reason this page exists: "that worker is busy — busy with WHAT?" A row of
 * committed-versus-advertised numbers says four CPUs are in use and says
 * nothing about which run is using them, so an operator who spots a pinned
 * worker has to go hunting through run pages to find the task on it. This page
 * is the join, served by the gateway in one read.
 *
 * Three sections, in the order the questions get asked:
 *
 *  1. identity and liveness — is this worker even there, and does the control
 *     plane agree with its heartbeat;
 *  2. the tasks on it — the drill-down proper, each linking through to its run;
 *  3. capacity per dimension — reusing CapacityGrid, because a worker at 40%
 *     CPU with every GPU committed is 100% blocked for GPU work and one
 *     aggregate percentage hides exactly that.
 *
 * The empty case is a first-class answer, not a failure. "This worker is
 * running nothing right now" is both common and frequently the finding — an
 * idle worker beside a queue that is not moving means the mismatch is in queues
 * or resource shape, not in the fleet's size. It is rendered as a statement,
 * never as an error box.
 */

import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { errorMessage, useDrainWorker, useWorker } from '../api/hooks';
import type { WorkerAttempt, WorkerDetail } from '../api/types';
import { StateBadge } from '../components/StateBadge';
import { CapacityGrid } from '../components/CapacityGrid';
import { AsyncPanel, Empty, ErrorBox, Loading, StaleWarning } from '../components/Status';
import { DataTable } from '../components/DataTable';
import type { Column, RowCountKind } from '../components/DataTable';
import { HEARTBEAT_STALE_MS, heartbeatStale } from './WorkersPage';
import { EMPTY, fmtAge, fmtDurationMs, fmtPercent, fmtTs, parseTs, shortId } from '../lib/format';

/**
 * What each embedded list may claim about its own completeness.
 *
 * THE TWO LISTS DIFFER, and pinning the difference is the whole reason these
 * are constants rather than an inline `{kind:'unknown'}` on both.
 *
 * `running_attempts` is EXACT. GatewayWorkerRunningAttempts applies no LIMIT at
 * all — the set is bounded by the worker's own capacity, so it is inherently
 * small and what comes back IS everything the worker is executing. That is a
 * real claim worth making: "3 attempts" on this page and the `3` in the fleet
 * table's running column are the same number by construction, and downgrading
 * this to a cautious "3+" would break that agreement in the reader's mind.
 *
 * `recent_attempts` is CAPPED at the gateway's `workerRecentLimit` of 25 (a
 * `recent_limit` query parameter this page does not set). A tail that fills is
 * indistinguishable from a worker that ran exactly 25 tasks, so it reads "25+"
 * and says on hover that more were not fetched. The difference between "this
 * worker ran 25 tasks" and "here are 25 of the tasks this worker ran" is
 * exactly the claim an operator would otherwise make on our behalf.
 *
 * MUST MATCH `workerRecentLimit` in services/api-gateway/rest/api.go.
 */
const RUNNING_COUNT: RowCountKind = { kind: 'exact' };
const RECENT_COUNT: RowCountKind = { kind: 'capped', limit: 25 };

export function WorkerDetailPage({ namespace }: { namespace: string }) {
  const { workerId = '' } = useParams();
  const worker = useWorker(namespace, workerId);
  const now = Date.now();

  // The error case takes the whole page rather than a panel: with no worker
  // there is no identity to head the page with, and rendering the crumb over an
  // empty frame would suggest the worker exists and is merely quiet.
  if (worker.error && worker.data === undefined) {
    return (
      <div className="col">
        <div className="detail-actions detail-back-only"><Link to="/workers" className="btn">← Back to workers</Link></div>
        <ErrorBox
          error={worker.error}
          what={`worker ${shortId(workerId, 12)}`}
          onRetry={worker.refetch}
        />
      </div>
    );
  }
  if (!worker.data) return <div className="col"><div className="detail-actions detail-back-only"><Link to="/workers" className="btn">← Back to workers</Link></div><Loading rows={8} label="Loading worker" /></div>;

  const w = worker.data;
  const stale = heartbeatStale(w, now);

  return (
    <div className="col">
      <WorkerHead worker={w} namespace={namespace} stale={stale} now={now} />

      {worker.error != null ? <StaleWarning error={worker.error} /> : null}

      {/* The liveness contradiction leads the page when it applies. A worker the
          control plane calls ACTIVE while its heartbeat has stopped looks
          schedulable and is not, and the task list below it is then a list of
          work nobody is confirming progress on — which changes how everything
          under it should be read, so it is said first. */}
      {stale && (
        <div className="notice warn" role="status">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">
              This worker is ACTIVE but stopped heartbeating {fmtAge(w.last_heartbeat_at, now)}
            </div>
            <div className="notice-detail">
              The liveness sweep has not caught up, so the scheduler still treats it as
              schedulable. Any task listed below may already be orphaned — the control plane will
              re-drive it once the worker is declared DEAD.
            </div>
          </div>
        </div>
      )}

      <RunningTasksPanel query={worker} namespace={namespace} now={now} />

      <div className="panel">
        <div className="panel-head">
          <span>capacity</span>
          <span className="spacer" />
          <span className="panel-note">
            advertised = what this worker registered · committed = reservations held against it
          </span>
        </div>
        <div className="panel-body">
          {/* Reusing the fleet's own grid rather than a per-worker variant: the
              advertised/committed question is identical at one worker and at a
              namespace, and two implementations of it are how two screens come
              to disagree. showWorkerCount is off because "1 of 1 workers" is
              noise on a page about one worker. */}
          <CapacityGrid
            summary={{ advertised: w.capacity ?? {}, committed: w.committed ?? {} }}
            showWorkerCount={false}
          />
        </div>
      </div>

      <div className="grid-2">
        <IdentityPanel worker={w} now={now} />
        <RecentTasksPanel worker={w} namespace={namespace} />
      </div>
    </div>
  );
}

/**
 * Identity, state, and the drain control.
 *
 * The state badge precedes the hostname for the same reason the schedule page
 * leads with enabled/disabled: on a page reached mid-incident, "is this thing
 * alive" is read before "what is it called".
 */
function WorkerHead({
  worker,
  namespace,
  stale,
  now,
}: {
  worker: WorkerDetail;
  namespace: string;
  stale: boolean;
  now: number;
}) {
  return (
    <div className="detail-head">
      <div className="detail-head-text">
        <div className="detail-title-row">
          <StateBadge kind="worker" state={worker.state} />
          <h1 className="detail-title mono">{worker.hostname}</h1>
        </div>
        <div className="detail-meta">
          <code title={worker.id}>{shortId(worker.id, 12)}</code> · heartbeat{' '}
          {fmtAge(worker.last_heartbeat_at, now)} ago
          {stale && ' (stale)'} · agent {worker.agent_version ?? EMPTY}
        </div>
      </div>
      <div className="detail-actions">
        <Link to="/workers" className="btn">← Back to workers</Link>
        <Permit permission="workers.drain"><DrainButton worker={worker} namespace={namespace} /></Permit>
      </div>
    </div>
  );
}

/**
 * Drain, behind a confirmation.
 *
 * Confirmed in place rather than fired on the first click because draining
 * removes a worker from the schedulable pool — a fleet-shaping action, which is
 * why the gateway gates it on admin_write rather than plain write. The prompt
 * names the consequence that is easy to get wrong: a drain does not stop what
 * is already running, and an operator expecting it to kill a stuck task needs
 * to know that before clicking, not after.
 *
 * Offered only for a worker that can still take work. DRAINING is already
 * draining, and a DEAD worker has nothing to drain — a live button on either
 * would be an action with no effect, which teaches operators to distrust the
 * controls that DO work.
 */
function DrainButton({ worker, namespace }: { worker: WorkerDetail; namespace: string }) {
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const drain = useDrainWorker(namespace);

  if (worker.state !== 'ACTIVE') {
    return (
      <span className="sub">
        {worker.state === 'DRAINING'
          ? 'Already draining — it finishes what it holds and takes nothing new.'
          : 'Nothing to drain.'}
      </span>
    );
  }

  if (!confirming) {
    return (
      <>
        <button
          type="button"
          onClick={() => {
            setError(null);
            setConfirming(true);
          }}
          title="Stop this worker taking new work. What it is already running continues."
        >
          Drain
        </button>
        {error && <div className="run-cancel-error">{<ErrorBox error={error} what="the drain" />}</div>}
      </>
    );
  }

  return (
    <div className="run-confirm">
      <span className="run-confirm-prompt">
        Drain <code>{worker.hostname}</code>? It stops accepting new tasks and finishes the{' '}
        {worker.running_attempts?.length ?? 0} it holds. The directive rides the next heartbeat, so
        this is not instant.
      </span>
      <button
        type="button"
        disabled={drain.isPending}
        onClick={() =>
          drain.mutate(worker.id, {
            onSuccess: () => setConfirming(false),
            onError: (e) => {
              setError(errorMessage(e));
              setConfirming(false);
            },
          })
        }
      >
        {drain.isPending ? 'Draining…' : 'Confirm drain'}
      </button>
      <button type="button" onClick={() => setConfirming(false)} disabled={drain.isPending}>
        Keep it
      </button>
    </div>
  );
}

/**
 * The tasks running on this worker. This is the point of the page.
 *
 * Routed through AsyncPanel so loading, error and empty each get their own
 * honest rendering — and the empty one is written as a statement about an idle
 * worker, because that is what it is. An operator who arrives here from a
 * stalled queue and reads "running nothing" has learned the answer: the
 * mismatch is in the queues or the resource shape, not in the fleet's size.
 */
function RunningTasksPanel({
  query,
  namespace,
  now,
}: {
  query: { data: WorkerDetail | undefined; isLoading: boolean; error: unknown; refetch?: () => void };
  namespace: string;
  now: number;
}) {
  const w = query.data;
  const dead = w?.state === 'DEAD';

  return (
    <div className="panel">
      <div className="panel-head">
        <span>running now</span>
        <span className="spacer" />
        <span className="panel-note">
          attempts DISPATCHED or RUNNING — the same set the fleet table counts
        </span>
      </div>

      {/* A gateway that predates this route serves the worker without either
          attempt list. That is "not reported", which is a different claim from
          "nothing is running", so it is said differently. */}
      {w && w.running_attempts === undefined ? (
        <div className="panel-body">
          <Empty
            title="This gateway does not report per-worker tasks"
            detail={
              <>
                The fleet listing answered, but <code>GET /workers/{'{id}'}</code> returned no task
                lists — an older api-gateway. The capacity figures below are still this worker's
                own.
              </>
            }
          />
        </div>
      ) : (
        <AsyncPanel
          query={{
            ...query,
            data: w?.running_attempts,
          }}
          what="this worker's tasks"
          isEmpty={(list) => list.length === 0}
          emptyTitle={dead ? 'This worker is running nothing — it is DEAD' : 'This worker is idle'}
          emptyDetail={
            dead ? (
              <>
                A dead worker holds no work. Anything it was running has been re-driven or failed;
                the history below is what it did before it went.
              </>
            ) : (
              <>
                Nothing is dispatched to it right now. If work is queued and this worker is idle,
                the mismatch is in the queues it serves or the resource shape the task needs —
                compare its capacity below against the pending task, rather than adding workers.
              </>
            )
          }
          loadingRows={3}
        >
          {(list) => (
            <AttemptTable
              attempts={list}
              namespace={namespace}
              now={now}
              live
              count={RUNNING_COUNT}
            />
          )}
        </AsyncPanel>
      )}
    </div>
  );
}

/**
 * What this worker ran recently, bounded by the server.
 *
 * Here rather than absent because it is the only thing a DEAD worker can say
 * about itself, and because a worker that is idle NOW may still have failed
 * four tasks in the last minute — which is the difference between "idle because
 * there is no work" and "idle because it keeps losing the work it gets".
 */
function RecentTasksPanel({
  worker,
  namespace,
}: {
  worker: WorkerDetail;
  namespace: string;
}) {
  const recent = worker.recent_attempts;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>recently finished</span>
        <span className="spacer" />
        <span className="panel-note">most recent first · a bounded tail, not full history</span>
      </div>
      {recent === undefined ? (
        <div className="panel-body">
          <span className="sub">Not reported by this gateway.</span>
        </div>
      ) : recent.length === 0 ? (
        <div className="panel-body">
          <Empty
            title="No finished tasks recorded"
            detail="This worker has not completed an attempt. A worker that registered moments ago looks exactly like this."
          />
        </div>
      ) : (
        <AttemptTable
          attempts={recent}
          namespace={namespace}
          now={Date.now()}
          count={RECENT_COUNT}
        />
      )}
    </div>
  );
}

/**
 * One list of attempts, with the link through to the run.
 *
 * The run link is the reason the table exists: "worker X is busy" is only
 * actionable once it becomes "with run Y", and the operator's next click is
 * always into that run. task_ref leads the row over node_id because `step-3`
 * names a position in a DAG the reader is not currently looking at, whereas
 * `ml.train:v2` names the work.
 *
 * `live` switches the last column between elapsed-so-far and took: a running
 * task has no duration yet, and showing a blank there would read as missing
 * data rather than as work still in progress.
 *
 * ── WHY THIS ONE IS A DataTable, AND WHAT IS SWITCHED OFF ─────────────────
 *
 * This is an EMBEDDED list, not a top-level browser, so most of what DataTable
 * offers would be noise on it: `paging: {mode:'none'}` and no `search`, because
 * a page-size selector and a filter box over four rows are controls that cost
 * more attention than they save. What is kept is the part that is not cosmetic
 * — one set of header semantics (scope="col" on every `<th>`, which a nine-
 * column operator table needs to be readable at all) and a count that states
 * what it is entitled to state.
 *
 * BOTH LISTS ARE `capped`, NOT `exact`, AND THE CAPS DIFFER. The gateway bounds
 * each of these server-side and sends no total, so neither may claim to be all
 * there is. `running_attempts` is bounded by the worker's own concurrency and
 * in practice never reaches its cap, so its count reads exactly — which is
 * correct, and is `countClaim`'s second half doing its job. `recent_attempts`
 * is a tail and routinely IS full, so it reads "10+" and says on hover that
 * more were not fetched. Calling that one "10 attempts" would present a window
 * as a history.
 */
function AttemptTable({
  attempts,
  namespace,
  now,
  live = false,
  count,
}: {
  attempts: WorkerAttempt[];
  namespace: string;
  now: number;
  live?: boolean;
  count: RowCountKind;
}) {
  /**
   * Rebuilt per render: the duration column closes over `now`, so a hoisted
   * array would freeze the clock and a running task's elapsed time would stop
   * advancing. The same reason the fleet table rebuilds its own.
   */
  const columns: Column<WorkerAttempt>[] = [
    {
      key: 'state',
      header: 'state',
      cell: (a) => <StateBadge kind="attempt" state={a.state} />,
    },
    {
      key: 'task',
      header: 'task',
      cell: (a) => (
        <div className="cell-stack">
          <span className="mono">{a.task_ref || a.node_id}</span>
          <span className="cell-sub">
            {a.node_id}
            {a.attempt_number > 1 && ` · attempt ${a.attempt_number}`}
          </span>
        </div>
      ),
    },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      /* Straight to the run. The node is not addressable on its own in the
         current route set — /runs/:runId is where the DAG lives, and the
         node_id beside the task above is what the operator matches against it
         once there. */
      cell: (a) => (
        <Link to={`/runs/${encodeURIComponent(a.run_id)}`} title={a.run_id}>
          {shortId(a.run_id, 10)}
        </Link>
      ),
    },
    {
      key: 'queue',
      header: 'queue',
      cellClass: 'mono',
      cell: (a) => <>{a.queue || EMPTY}</>,
    },
    {
      key: 'duration',
      header: live ? 'running for' : 'took',
      cell: (a) => (
        <span title={fmtTs(live ? a.started_at : a.ended_at)}>
          <AttemptDuration attempt={a} now={now} live={live} />
        </span>
      ),
    },
  ];

  return (
    <div className="worker-tasks">
      <DataTable
        rows={attempts}
        columns={columns}
        rowKey={(a) => a.id}
        /* A failed or lost attempt takes the danger rule, the same treatment
           the fleet and run tables give a row that needs looking at. LOST is
           included because an attempt whose worker vanished mid-flight is a
           failure the run's own state may not yet reflect. */
        rowClass={(a) => (a.state === 'FAILED' || a.state === 'LOST' ? 'row-failed' : '')}
        noun="attempts"
        tableClass="table-fleet"
        count={count}
        paging={{ mode: 'none' }}
      />
      {/* Namespace is stated once, under the table, rather than per row: every
          attempt here belongs to the worker's own namespace by construction. */}
      <div className="worker-tasks-note">
        namespace <code>{namespace}</code>
      </div>
    </div>
  );
}

/**
 * How long an attempt has been going, or how long it took.
 *
 * A DISPATCHED attempt with no started_at is called out rather than shown as a
 * dash: an attempt assigned to a worker that never started is a stall with a
 * specific cause (the worker accepted it and has not picked it up), and it is
 * one of the main things this page is opened to find.
 */
function AttemptDuration({
  attempt,
  now,
  live,
}: {
  attempt: WorkerAttempt;
  now: number;
  live: boolean;
}) {
  const started = parseTs(attempt.started_at);

  if (live) {
    if (started === null) {
      return (
        <span className="chip warn" title="Handed to this worker but not started — it has accepted the attempt and not begun it.">
          not started
        </span>
      );
    }
    return <span className="mono">{fmtDurationMs(now - started)}</span>;
  }

  const ended = parseTs(attempt.ended_at);
  if (started === null || ended === null) return <span className="faint">{EMPTY}</span>;
  return <span className="mono">{fmtDurationMs(ended - started)}</span>;
}

/**
 * Identity and the reported-utilisation figures.
 *
 * Utilisation is labelled "reported by the agent" exactly as the fleet table
 * labels it, and for the same reason: it is self-reported rather than measured
 * by the control plane, so it must not read like the committed/advertised
 * figures beside it, which an operator can act on. It is also frequently absent
 * — the fleet listing does not carry it today — and an absent figure renders as
 * a dash rather than as 0%.
 */
function IdentityPanel({ worker, now }: { worker: WorkerDetail; now: number }) {
  const labels = Object.entries(worker.labels ?? {});
  const hb = parseTs(worker.last_heartbeat_at);
  const overdue = hb !== null && now - hb > HEARTBEAT_STALE_MS;

  return (
    <div className="panel">
      <div className="panel-head">
        <span>identity</span>
      </div>
      <div className="panel-body">
        <dl className="kv">
          <dt>worker id</dt>
          <dd>{worker.id}</dd>
          <dt>host</dt>
          <dd>{worker.hostname}</dd>
          <dt>state</dt>
          <dd>
            <StateBadge kind="worker" state={worker.state} small />
          </dd>
          <dt>queues</dt>
          <dd>{worker.queues?.join(', ') || EMPTY}</dd>
          <dt>executors</dt>
          <dd>{worker.executors?.join(', ') || EMPTY}</dd>
          <dt>agent</dt>
          <dd>{worker.agent_version ?? EMPTY}</dd>
          <dt>registered</dt>
          <dd>{fmtTs(worker.registered_at)}</dd>
          <dt>last heartbeat</dt>
          <dd>
            {fmtTs(worker.last_heartbeat_at)}
            {/* The threshold is named so the reader can judge the number rather
                than take our word for it. */}
            <span className="cell-sub">
              {' '}
              {fmtAge(worker.last_heartbeat_at, now)} ago
              {overdue && ` · over the ${Math.round(HEARTBEAT_STALE_MS / 1000)}s liveness cut`}
            </span>
          </dd>
          <dt>util (reported)</dt>
          <dd>
            cpu {fmtPercent(worker.cpu_utilization)} · mem {fmtPercent(worker.memory_utilization)}
          </dd>
          <dt>labels</dt>
          <dd>
            {labels.length === 0 ? (
              <span className="faint">{EMPTY}</span>
            ) : (
              <div className="fleet-labels">
                {labels.map(([k, v]) => (
                  <span key={k}>
                    {k}={v}
                  </span>
                ))}
              </div>
            )}
          </dd>
        </dl>
      </div>
    </div>
  );
}
