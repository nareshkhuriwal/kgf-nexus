// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Advertised-versus-committed capacity, per dimension.
 *
 * Extracted from the worker fleet view because the overview asks the same
 * question and must not answer it differently: a fleet at 40% CPU with every
 * GPU committed is 100% blocked for GPU work, and one aggregate number hides
 * exactly that. Two screens each rolling their own aggregation is how the two
 * screens end up disagreeing mid-incident.
 *
 * `derived` is part of the contract rather than a styling flag. A summed
 * client-side estimate and a server-computed reservation total are not the same
 * claim, and the component refuses to let a caller present one as the other.
 */

import type { CapacitySummary, ResourceVector, Worker } from '../api/types';
import { EMPTY, fmtBytes, fmtCpu, fmtPercent, utilization } from '../lib/format';

/**
 * Disk is included here but not on the fleet table: the per-worker table is
 * already at its column budget, whereas the summary has room, and a disk-bound
 * namespace is otherwise invisible until a task fails to place.
 */
const DIMENSIONS: {
  label: string;
  pick: (v: ResourceVector | undefined) => number | undefined;
  fmt: (n: number | null | undefined) => string;
}[] = [
  { label: 'cpu', pick: (v) => v?.cpu_millis, fmt: fmtCpu },
  { label: 'memory', pick: (v) => v?.memory_bytes, fmt: fmtBytes },
  { label: 'gpu', pick: (v) => v?.gpu_count, fmt: (n) => String(n ?? 0) },
  { label: 'disk', pick: (v) => v?.disk_bytes, fmt: fmtBytes },
];

export function CapacityGrid({
  summary,
  derived = false,
  showWorkerCount = true,
}: {
  summary: CapacitySummary;
  derived?: boolean;
  showWorkerCount?: boolean;
}) {
  return (
    <div>
      {derived && (
        <div className="sub" style={{ marginBottom: 'var(--space-3)' }}>
          Derived by summing the worker rows — the <code>/capacity</code> endpoint did not answer.
        </div>
      )}
      <div className="grid-2">
        {DIMENSIONS.map((d) => {
          const c = d.pick(summary.committed);
          const a = d.pick(summary.advertised);
          // A dimension nothing advertises is not "0% used", it is absent. The
          // fleet has no GPUs; saying 0% invites someone to schedule GPU work.
          if (!a) return null;
          const frac = utilization(c, a);
          const cls = frac === null ? '' : frac >= 1 ? 'full' : frac >= 0.8 ? 'hot' : '';
          return (
            <div key={d.label} className="panel" style={{ background: 'var(--bg-inset)' }}>
              <div className="panel-body">
                <div className="attempt-section-title">{d.label}</div>
                <div className="row" style={{ justifyContent: 'space-between' }}>
                  <strong className="mono">
                    {d.fmt(c ?? 0)} / {d.fmt(a)}
                  </strong>
                  <span className="mono">{fmtPercent(frac)}</span>
                </div>
                <div className={`meter ${cls}`} style={{ marginTop: 'var(--space-3)' }}>
                  <span style={{ width: `${(frac ?? 0) * 100}%` }} />
                </div>
              </div>
            </div>
          );
        })}
      </div>
      {showWorkerCount &&
        (summary.worker_count !== undefined || summary.active_worker_count !== undefined) && (
          <div className="sub" style={{ marginTop: 'var(--space-3)' }}>
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
