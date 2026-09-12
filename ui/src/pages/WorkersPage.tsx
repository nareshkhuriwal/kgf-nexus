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
 */

import { useCapacity, useWorkers } from '../api/hooks';
import type { Worker } from '../api/types';
import { StateBadge } from '../components/StateBadge';
import { CapacityGrid, deriveCapacity } from '../components/CapacityGrid';
import { AsyncPanel, ErrorBox, StaleWarning } from '../components/Status';
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

/** Heartbeat is every 5s (§11); 3 missed beats is the conventional liveness cut. */
const HEARTBEAT_STALE_MS = 15_000;

export function WorkersPage({ namespace }: { namespace: string }) {
  const workers = useWorkers(namespace);
  const capacity = useCapacity(namespace);
  const now = Date.now();

  return (
    <div className="col">
      <div>
        <h1>Worker fleet</h1>
        <div className="sub">
          namespace <code>{namespace}</code> · refreshes every 5s, matching the agent heartbeat interval
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>capacity summary</span>
          <span className="spacer" />
          <span className="sub" style={{ textTransform: 'none', letterSpacing: 0 }}>
            advertised = registered capacity · committed = sum of active reservations
          </span>
        </div>
        {capacity.error && capacity.data === undefined ? (
          <div className="panel-body">
            {/* Fall back to summing the worker rows: the aggregate endpoint being
                absent should not hide information we already have. */}
            {workers.data ? (
              <CapacityGrid summary={deriveCapacity(workers.data)} derived />
            ) : (
              <ErrorBox error={capacity.error} what="capacity summary" onRetry={capacity.refetch} />
            )}
          </div>
        ) : (
          <div className="panel-body">
            {capacity.data ? (
              <CapacityGrid summary={capacity.data} />
            ) : (
              <div className="sub">loading…</div>
            )}
          </div>
        )}
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>workers</span>
          <span className="spacer" />
          <span className="chip">{workers.data?.length ?? 0} registered</span>
        </div>

        {workers.error != null && workers.data !== undefined ? (
          <div style={{ padding: 'var(--space-5)', paddingBottom: 0 }}>
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
              <table>
                <thead>
                  <tr>
                    <th>state</th>
                    <th>host</th>
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

function WorkerRow({ worker, now }: { worker: Worker; now: number }) {
  const hb = parseTs(worker.last_heartbeat_at);
  const hbAge = hb === null ? null : now - hb;
  // A worker the control plane still calls ACTIVE while its heartbeat is stale
  // is the interesting case: it means the liveness sweep has not caught up.
  const stale = hbAge !== null && hbAge > HEARTBEAT_STALE_MS && worker.state === 'ACTIVE';

  return (
    <tr>
      <td>
        <StateBadge kind="worker" state={worker.state} />
      </td>
      <td>
        <div className="mono">{worker.hostname}</div>
        <div className="faint mono" style={{ fontSize: 10.5 }} title={worker.id}>
          {shortId(worker.id, 8)}
        </div>
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
          <>
            <DimensionCell
              committed={worker.committed?.gpu_count}
              advertised={worker.capacity.gpu_count}
              format={(n) => String(n ?? 0)}
            />
            {worker.capacity.gpu_kind && <div className="faint mono">{worker.capacity.gpu_kind}</div>}
          </>
        ) : (
          <span className="faint">none</span>
        )}
      </td>
      <td className="nowrap">
        <div className="faint" style={{ fontSize: 'var(--text-xs)' }}>
          cpu {fmtPercent(worker.cpu_utilization)} · mem {fmtPercent(worker.memory_utilization)}
        </div>
      </td>
      <td className="nowrap" title={fmtTs(worker.last_heartbeat_at)}>
        <span className={`pulse ${stale ? 'stale' : worker.state === 'DEAD' ? 'off' : ''}`} />{' '}
        {fmtAge(worker.last_heartbeat_at, now)}
        {stale && <div className="chip warn">stale</div>}
      </td>
      <td className="mono" style={{ fontSize: 'var(--text-xs)' }}>
        {Object.entries(worker.labels ?? {}).length === 0 ? (
          <span className="faint">{EMPTY}</span>
        ) : (
          Object.entries(worker.labels ?? {}).map(([k, v]) => (
            <div key={k}>
              {k}={v}
            </div>
          ))
        )}
      </td>
      <td className="faint mono" style={{ fontSize: 'var(--text-xs)' }}>
        {worker.agent_version ?? EMPTY}
        {worker.executors && worker.executors.length > 0 && <div>{worker.executors.join(',')}</div>}
      </td>
    </tr>
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
    <div>
      <div className="mono nowrap" style={{ fontSize: 11.5 }}>
        {format(committed ?? 0)} / {format(advertised)}
      </div>
      <div className={`meter ${cls}`} title={`${fmtPercent(frac)} committed`}>
        <span style={{ width: `${(frac ?? 0) * 100}%` }} />
      </div>
    </div>
  );
}
