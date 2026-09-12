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
import type { CapacitySummary, ResourceVector, Worker } from '../api/types';
import { StateBadge } from '../components/StateBadge';
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
          <div style={{ padding: 12, paddingBottom: 0 }}>
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
        <div className="faint" style={{ fontSize: 11 }}>
          cpu {fmtPercent(worker.cpu_utilization)} · mem {fmtPercent(worker.memory_utilization)}
        </div>
      </td>
      <td className="nowrap" title={fmtTs(worker.last_heartbeat_at)}>
        <span className={`pulse ${stale ? 'stale' : worker.state === 'DEAD' ? 'off' : ''}`} />{' '}
        {fmtAge(worker.last_heartbeat_at, now)}
        {stale && <div className="chip warn">stale</div>}
      </td>
      <td className="mono" style={{ fontSize: 11 }}>
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
      <td className="faint mono" style={{ fontSize: 11 }}>
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

function CapacityGrid({ summary, derived = false }: { summary: CapacitySummary; derived?: boolean }) {
  const dims: { label: string; c?: number; a?: number; fmt: (n: number | null | undefined) => string }[] = [
    { label: 'cpu', c: summary.committed?.cpu_millis, a: summary.advertised?.cpu_millis, fmt: fmtCpu },
    { label: 'memory', c: summary.committed?.memory_bytes, a: summary.advertised?.memory_bytes, fmt: fmtBytes },
    {
      label: 'gpu',
      c: summary.committed?.gpu_count,
      a: summary.advertised?.gpu_count,
      fmt: (n) => String(n ?? 0),
    },
  ];
  return (
    <div>
      {derived && (
        <div className="sub" style={{ marginBottom: 9 }}>
          Derived by summing the worker rows — the <code>/capacity</code> endpoint did not answer.
        </div>
      )}
      <div className="grid-2">
        {dims.map((d) => (
          <div key={d.label} className="panel" style={{ background: 'var(--bg-inset)' }}>
            <div className="panel-body">
              <div className="attempt-section-title">{d.label}</div>
              <div className="row" style={{ justifyContent: 'space-between' }}>
                <strong className="mono">
                  {d.fmt(d.c ?? 0)} / {d.fmt(d.a)}
                </strong>
                <span className="mono">{fmtPercent(utilization(d.c, d.a))}</span>
              </div>
              <div className="meter" style={{ marginTop: 6 }}>
                <span style={{ width: `${(utilization(d.c, d.a) ?? 0) * 100}%` }} />
              </div>
            </div>
          </div>
        ))}
      </div>
      {(summary.worker_count !== undefined || summary.active_worker_count !== undefined) && (
        <div className="sub" style={{ marginTop: 9 }}>
          {summary.active_worker_count ?? EMPTY} active of {summary.worker_count ?? EMPTY} workers
        </div>
      )}
    </div>
  );
}

/** Client-side aggregate used when the /capacity endpoint is unavailable. */
export function deriveCapacity(workers: Worker[]): CapacitySummary {
  const add = (acc: ResourceVector, v: ResourceVector | undefined): ResourceVector => ({
    cpu_millis: (acc.cpu_millis ?? 0) + (v?.cpu_millis ?? 0),
    memory_bytes: (acc.memory_bytes ?? 0) + (v?.memory_bytes ?? 0),
    gpu_count: (acc.gpu_count ?? 0) + (v?.gpu_count ?? 0),
    disk_bytes: (acc.disk_bytes ?? 0) + (v?.disk_bytes ?? 0),
  });
  // Only schedulable workers contribute advertised capacity: counting a DEAD
  // worker's CPUs would claim headroom that does not exist.
  const schedulable = workers.filter((w) => w.state === 'ACTIVE' || w.state === 'DRAINING');
  return {
    advertised: schedulable.reduce<ResourceVector>((a, w) => add(a, w.capacity), {}),
    committed: schedulable.reduce<ResourceVector>((a, w) => add(a, w.committed), {}),
    worker_count: workers.length,
    active_worker_count: workers.filter((w) => w.state === 'ACTIVE').length,
  };
}
