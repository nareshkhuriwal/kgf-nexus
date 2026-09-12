// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Worker fleet view plus a capacity summary.
 *
 * The operator question this screen exists for: "why is nothing being
 * dispatched?" The answer is almost always either (a) no worker advertises the
 * shape of resource the pending task needs, or (b) the capacity is committed.
 * So advertised-vs-committed is shown per dimension per worker, not as a single
 * percentage — a fleet at 40% CPU with every GPU committed is 100% blocked for
 * GPU work, and one aggregate number hides exactly that.
 *
 * The fleet's headline figures sit above the table because the second question,
 * once dispatch has stalled, is "how much of the fleet is even there" — and the
 * two figures that answer it (dead workers, and workers whose heartbeat has gone
 * quiet while the control plane still calls them ACTIVE) are otherwise buried in
 * a ten-column table.
 */

import { Link } from 'react-router-dom';
import { useCapacity, useWorkers } from '../api/hooks';
import type { Worker } from '../api/types';
import { PageHeader } from '../components/PageHeader';
import { StateBadge } from '../components/StateBadge';
import { CapacityGrid, deriveCapacity } from '../components/CapacityGrid';
import { AsyncPanel, ErrorBox, Loading, StaleWarning } from '../components/Status';
import {
  EMPTY,
  fmtAge,
  fmtBytes,
  fmtCpu,
  fmtPercent,
  fmtTs,
  parseTs,
  shortId,
  utilization,
} from '../lib/format';

/**
 * Re-exported because the capacity summary now also feeds the overview, so it
 * lives in components/. Callers that already import it from the fleet page keep
 * working rather than being churned for a move.
 */
export { deriveCapacity };

/**
 * Heartbeat is every 5s (§11); 3 missed beats is the conventional liveness cut.
 *
 * Exported alongside the predicate below because the worker DETAIL view applies
 * the same test and must state the same threshold. Two screens each holding
 * their own copy of the number is how one of them ends up calling a worker
 * quiet while the other calls it healthy — a disagreement that costs an operator
 * far more than the export costs us.
 */
export const HEARTBEAT_STALE_MS = 15_000;

/**
 * A worker the control plane still calls ACTIVE while its heartbeat is stale.
 *
 * This is the interesting liveness case and the reason the check is a named
 * function rather than an inline condition: it means the liveness sweep has not
 * caught up, so the worker looks schedulable and is not. A DEAD worker is
 * deliberately excluded — its state already says so, and flagging it twice would
 * dilute the flag on the workers whose state does NOT.
 *
 * Takes a Worker rather than the detail type so the fleet table and the drill-
 * down can both pass what they already hold: WorkerDetail extends Worker, and
 * the predicate needs nothing the narrower shape lacks.
 */
export function heartbeatStale(worker: Worker, now: number): boolean {
  const hb = parseTs(worker.last_heartbeat_at);
  if (hb === null) return false;
  return now - hb > HEARTBEAT_STALE_MS && worker.state === 'ACTIVE';
}

export function WorkersPage({ namespace }: { namespace: string }) {
  const workers = useWorkers(namespace);
  const capacity = useCapacity(namespace);
  const now = Date.now();

  return (
    <div className="col">
      <PageHeader
        title="Worker fleet"
        subtitle={
          <>
            namespace <code>{namespace}</code> · refreshes every 5s, matching the agent heartbeat
            interval
          </>
        }
      />

      <div className="panel">
        <div className="panel-head">
          <span>capacity summary</span>
          <span className="spacer" />
          <span className="panel-note">
            advertised = registered capacity · committed = sum of active reservations
          </span>
        </div>
        {capacity.error && capacity.data === undefined ? (
          <div className="panel-body">
            {/* Fall back to summing the worker rows: the aggregate endpoint being
                absent should not hide information we already have. CapacityGrid
                takes `derived` as part of its contract, so the fallback cannot be
                presented as the server's own answer. */}
            {workers.data ? (
              <CapacityGrid summary={deriveCapacity(workers.data)} derived />
            ) : (
              <ErrorBox error={capacity.error} what="capacity summary" onRetry={capacity.refetch} />
            )}
          </div>
        ) : capacity.data ? (
          <>
            {/* Numbers on screen plus a warning beats an error box that hides
                them, the same rule the run list follows. */}
            {capacity.error != null ? (
              <div className="panel-notice">
                <StaleWarning error={capacity.error} />
              </div>
            ) : null}
            <div className="panel-body">
              <CapacityGrid summary={capacity.data} />
            </div>
          </>
        ) : (
          <Loading rows={2} label="Loading capacity summary" />
        )}
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>workers</span>
        </div>

        <FleetFigures workers={workers.data} now={now} />

        {workers.error != null && workers.data !== undefined ? (
          <div className="panel-notice">
            <StaleWarning error={workers.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={workers}
          what="workers"
          isEmpty={(w) => w.length === 0}
          emptyTitle="No workers registered"
          emptyDetail="Nothing can be dispatched until an agent registers. Check the agent containers: docker compose logs -f agent-cpu agent-gpu"
          loadingRows={4}
        >
          {(list) => (
            <div className="table-scroll">
              <table className="table-fleet">
                <thead>
                  <tr>
                    <th>state</th>
                    <th>host</th>
                    <th>running</th>
                    <th>queues</th>
                    <th>cpu</th>
                    <th>memory</th>
                    <th>gpu</th>
                    <th>util (reported)</th>
                    <th>heartbeat</th>
                    <th>labels</th>
                    <th>agent</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((w) => (
                    <WorkerRow key={w.id} worker={w} now={now} />
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
 * The fleet at a glance.
 *
 * Four counts, large enough to read across a room during an incident. Only the
 * two that can BE a problem are ever toned, and only when they are non-zero: a
 * fleet with no dead workers has not achieved anything worth colouring, and a
 * dashboard where a figure is always red teaches operators to stop seeing it.
 *
 * The tones are --tone-danger-* and --tone-warning-*, never the accent. The
 * accent is operator-switchable brand, so a figure coloured with it would stop
 * meaning anything the moment someone changed their theme.
 *
 * Rendered only once the rows have arrived: a count of zero and a count we do
 * not have yet are different claims, and AsyncPanel below owns the second one.
 */
function FleetFigures({ workers, now }: { workers: Worker[] | undefined; now: number }) {
  if (workers === undefined || workers.length === 0) return null;

  const active = workers.filter((w) => w.state === 'ACTIVE').length;
  const dead = workers.filter((w) => w.state === 'DEAD').length;
  const quiet = workers.filter((w) => heartbeatStale(w, now)).length;

  return (
    <div className="fleet-figures">
      <div className="fleet-figure">
        <span className="fleet-figure-n">{workers.length}</span>
        <span className="fleet-figure-label">registered</span>
      </div>
      <div className="fleet-figure">
        <span className="fleet-figure-n">{active}</span>
        <span className="fleet-figure-label">active</span>
      </div>
      <div className="fleet-figure">
        <span className={`fleet-figure-n${dead > 0 ? ' alarm' : ''}`}>{dead}</span>
        <span className="fleet-figure-label">dead</span>
      </div>
      <div className="fleet-figure">
        <span
          className={`fleet-figure-n${quiet > 0 ? ' warn' : ''}`}
          title="ACTIVE according to the control plane, but the heartbeat has stopped — the liveness sweep has not caught up."
        >
          {quiet}
        </span>
        <span className="fleet-figure-label">gone quiet</span>
      </div>
    </div>
  );
}

function WorkerRow({ worker, now }: { worker: Worker; now: number }) {
  const stale = heartbeatStale(worker, now);
  // The row's left rule, from the worker's own state tone so it can never
  // disagree with the badge in its first cell. A quiet heartbeat is a warning
  // even while the state still claims ACTIVE — that disagreement IS the finding.
  const rowClass = worker.state === 'DEAD' ? 'row-failed' : stale ? 'row-warning' : '';

  return (
    <tr className={rowClass}>
      <td>
        <StateBadge kind="worker" state={worker.state} />
      </td>
      <td>
        {/* A real <Link>, on the host cell, rather than a click handler on the
            <tr>. A row-wide onClick is unreachable by keyboard, cannot be
            middle-clicked into a new tab, and offers no destination on hover —
            all three of which an operator triaging a fleet actually uses. The
            host is the anchor because it is what the eye lands on; the id
            beneath it stays plain text so the two do not read as two links. */}
        <Link to={`/workers/${encodeURIComponent(worker.id)}`} className="fleet-host" title={`${worker.hostname} — what is running on it`}>
          <span className="fleet-host-name">{worker.hostname}</span>
          <span className="fleet-host-id">{shortId(worker.id, 8)}</span>
        </Link>
      </td>
      <td>
        {/* The count the drill-down expands. Shown even when zero: "this worker
            is running nothing" is the answer to "why is my queue not moving",
            and an empty cell would leave it unsaid. */}
        <RunningCell worker={worker} />
      </td>
      <td className="mono">{worker.queues?.join(', ') || EMPTY}</td>
      <td>
        <DimensionCell
          committed={worker.committed?.cpu_millis}
          advertised={worker.capacity?.cpu_millis}
          format={fmtCpu}
        />
      </td>
      <td>
        <DimensionCell
          committed={worker.committed?.memory_bytes}
          advertised={worker.capacity?.memory_bytes}
          format={fmtBytes}
        />
      </td>
      <td>
        {worker.capacity?.gpu_count ? (
          <div className="dim">
            <DimensionCell
              committed={worker.committed?.gpu_count}
              advertised={worker.capacity.gpu_count}
              format={(n) => String(n ?? 0)}
            />
            {worker.capacity.gpu_kind && <span className="dim-kind">{worker.capacity.gpu_kind}</span>}
          </div>
        ) : (
          <span className="faint">none</span>
        )}
      </td>
      <td>
        {/* Reported by the agent, not measured here -- the column header says so,
            because a utilisation an operator cannot act on must not read like
            the committed/advertised figures beside it, which they can. */}
        <div className="fleet-util">
          <span>cpu {fmtPercent(worker.cpu_utilization)}</span>
          <span>mem {fmtPercent(worker.memory_utilization)}</span>
        </div>
      </td>
      <td title={fmtTs(worker.last_heartbeat_at)}>
        <div className="fleet-beat">
          <span
            className={`pulse ${stale ? 'stale' : worker.state === 'DEAD' ? 'off' : ''}`}
            aria-hidden="true"
          />
          <span>{fmtAge(worker.last_heartbeat_at, now)}</span>
        </div>
        {stale && <div className="chip warn">stale</div>}
      </td>
      <td>
        {Object.entries(worker.labels ?? {}).length === 0 ? (
          <span className="faint">{EMPTY}</span>
        ) : (
          <div className="fleet-labels">
            {Object.entries(worker.labels ?? {}).map(([k, v]) => (
              <span key={k}>
                {k}={v}
              </span>
            ))}
          </div>
        )}
      </td>
      <td>
        <div className="fleet-agent">
          <span>{worker.agent_version ?? EMPTY}</span>
          {worker.executors && worker.executors.length > 0 && (
            <span>{worker.executors.join(',')}</span>
          )}
        </div>
      </td>
    </tr>
  );
}

/**
 * How many tasks this worker is executing.
 *
 * Three distinct claims, kept distinct:
 *
 * - a number, when the gateway counted one;
 * - `idle` when that number is zero — the word rather than a bare "0", because
 *   an idle worker next to a growing queue is a finding, and a digit in a
 *   ten-column table does not read as one;
 * - an em dash when the field is absent, which is what a gateway predating
 *   `active_attempts` sends. Rendering that as 0 would invent an answer.
 */
function RunningCell({ worker }: { worker: Worker }) {
  const n = worker.active_attempts;
  if (n === undefined) return <span className="faint">{EMPTY}</span>;
  if (n === 0) return <span className="faint">idle</span>;
  return (
    <span className="mono" title={`${n} attempt${n === 1 ? '' : 's'} DISPATCHED or RUNNING`}>
      {n}
    </span>
  );
}

/** committed / advertised for one dimension, with a meter and exact numbers. */
function DimensionCell({
  committed,
  advertised,
  format,
}: {
  committed: number | null | undefined;
  advertised: number | null | undefined;
  format: (n: number | null | undefined) => string;
}) {
  if (!advertised) return <span className="faint">{EMPTY}</span>;
  const frac = utilization(committed, advertised);
  const cls = frac === null ? '' : frac >= 1 ? 'full' : frac >= 0.8 ? 'hot' : '';
  return (
    <div className="dim">
      <span className="dim-numbers">
        {format(committed ?? 0)} / {format(advertised)}
      </span>
      <div className={`meter ${cls}`} title={`${fmtPercent(frac)} committed`}>
        <span style={{ width: `${(frac ?? 0) * 100}%` }} />
      </div>
    </div>
  );
}
