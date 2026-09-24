// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Worker quarantine (§5.5): why a worker is out of the pool, until when, and
 * the controls to put it there or bring it back.
 *
 * QUARANTINED means "receives no new dispatches" -- the scheduler only places
 * work on ACTIVE workers -- while what it already holds finishes. The detector
 * quarantines a worker whose attempts keep failing with infrastructure-class
 * errors across different tasks; an operator can do the same by hand.
 */

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../api/client';
import type { WorkerQuarantine } from '../api/types';
import { Permit } from './Permissions';
import { ErrorBox } from './Status';
import { EMPTY, fmtTs } from '../lib/format';

const DURATIONS: { label: string; ms: number }[] = [
  { label: '1 hour', ms: 3_600_000 },
  { label: '24 hours', ms: 86_400_000 },
  { label: 'Until released', ms: 0 },
];

export function QuarantinePanel({
  namespace,
  workerId,
  state,
}: {
  namespace: string;
  workerId: string;
  state: string;
}) {
  const qc = useQueryClient();
  const history = useQuery({
    queryKey: ['worker-quarantines', namespace, workerId],
    queryFn: () => api.listWorkerQuarantines(namespace, workerId),
  });
  const refresh = () => {
    void qc.invalidateQueries({ queryKey: ['worker-quarantines', namespace, workerId] });
    void qc.invalidateQueries({ queryKey: ['worker', namespace, workerId] });
    void qc.invalidateQueries({ queryKey: ['workers', namespace] });
  };
  const [reason, setReason] = useState('');
  const [durationMs, setDurationMs] = useState(DURATIONS[0]!.ms);
  const quarantine = useMutation({
    mutationFn: () => api.quarantineWorker(namespace, workerId, reason.trim(), durationMs),
    onSuccess: () => {
      setReason('');
      refresh();
    },
    retry: false,
  });
  const release = useMutation({
    mutationFn: () => api.releaseWorker(namespace, workerId, 'released from the console'),
    onSuccess: refresh,
    retry: false,
  });

  const open = (history.data ?? []).find((q) => !q.released_at);

  return (
    <div className="panel" aria-label="Quarantine">
      <div className="panel-head">
        <span>quarantine</span>
        <span className="spacer" />
        {state === 'QUARANTINED' && <span className="chip warn">no new work is dispatched to this worker</span>}
      </div>
      <div className="panel-body">
        {state === 'QUARANTINED' && open && <OpenEpisode q={open} />}
        {state === 'QUARANTINED' ? (
          <Permit permission="workers.quarantine">
            <button type="button" className="primary" disabled={release.isPending} onClick={() => release.mutate()}>
              Release to the pool
            </button>
          </Permit>
        ) : state === 'ACTIVE' ? (
          <Permit permission="workers.quarantine">
            <form
              className="row"
              onSubmit={(ev) => {
                ev.preventDefault();
                if (reason.trim()) quarantine.mutate();
              }}
            >
              <input
                aria-label="Quarantine reason"
                placeholder="Why (required), e.g. GPU driver errors"
                value={reason}
                maxLength={1000}
                onChange={(ev) => setReason(ev.target.value)}
              />
              <select
                aria-label="Quarantine duration"
                value={durationMs}
                onChange={(ev) => setDurationMs(Number(ev.target.value))}
              >
                {DURATIONS.map((d) => (
                  <option key={d.ms} value={d.ms}>
                    {d.label}
                  </option>
                ))}
              </select>
              <button type="submit" disabled={!reason.trim() || quarantine.isPending}>
                Quarantine
              </button>
            </form>
          </Permit>
        ) : null}
        {quarantine.error != null && <ErrorBox error={quarantine.error} what="the quarantine" />}
        {release.error != null && <ErrorBox error={release.error} what="the release" />}
        <QuarantineHistory rows={history.data ?? []} />
      </div>
    </div>
  );
}

function OpenEpisode({ q }: { q: WorkerQuarantine }) {
  return (
    <p className="sub" role="status">
      {q.trigger === 'auto' ? 'Quarantined automatically' : q.trigger === 'inherited' ? 'Inherited from its host' : 'Quarantined by ' + q.actor}
      {' '}— {q.reason}. {q.expires_at ? `Lifts at ${fmtTs(q.expires_at)}.` : 'Until an operator releases it.'}
    </p>
  );
}

function QuarantineHistory({ rows }: { rows: WorkerQuarantine[] }) {
  if (rows.length === 0) return <p className="sub">This worker has never been quarantined.</p>;
  return (
    <table className="table compact" aria-label="Quarantine history">
      <thead>
        <tr>
          <th>started</th>
          <th>trigger</th>
          <th>reason</th>
          <th>expires</th>
          <th>released</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((q) => (
          <tr key={q.id}>
            <td>{fmtTs(q.started_at)}</td>
            <td>{q.trigger}</td>
            <td>{q.reason}</td>
            <td>{q.expires_at ? fmtTs(q.expires_at) : EMPTY}</td>
            <td>{q.released_at ? `${fmtTs(q.released_at)} by ${q.released_by ?? EMPTY}` : 'open'}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
