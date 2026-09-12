// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The run's append-only event history (run_events), newest first.
 *
 * This is the authoritative record: the current-state tables are a projection
 * of this log, and on disagreement the log wins. So the timeline shows actor
 * attribution prominently — "who caused this transition" is the question an
 * incident review actually asks, and the schema guarantees every row can answer
 * it (scheduler | worker:<id> | user:<sub>).
 */

import { useMemo, useState } from 'react';
import type { RunEvent } from '../api/types';
import { fmtAge, fmtTs, shortId } from '../lib/format';
import { Empty } from './Status';

/** Classifies an actor string for colour-coding without parsing it twice. */
export function actorKind(actor: string): 'scheduler' | 'worker' | 'user' | 'system' {
  if (actor.startsWith('worker:')) return 'worker';
  if (actor.startsWith('user:')) return 'user';
  if (actor === 'scheduler') return 'scheduler';
  return 'system';
}

function actorLabel(actor: string): string {
  const kind = actorKind(actor);
  if (kind === 'worker') return `worker:${shortId(actor.slice('worker:'.length), 8)}`;
  if (kind === 'user') return actor;
  return actor;
}

export function EventTimeline({
  events,
  onSelectNode,
}: {
  events: RunEvent[];
  onSelectNode?: (nodeId: string) => void;
}) {
  const [actorFilter, setActorFilter] = useState('');
  const [kindFilter, setKindFilter] = useState('');
  const now = Date.now();

  const kinds = useMemo(
    () => [...new Set(events.map((e) => e.kind))].sort(),
    [events],
  );
  const actors = useMemo(
    () => [...new Set(events.map((e) => actorKind(e.actor)))].sort(),
    [events],
  );

  // Newest first, by seq. seq is the run-scoped ordering the schema guarantees
  // unique; occurred_at can tie or skew, seq cannot.
  const ordered = useMemo(() => {
    const filtered = events.filter(
      (e) =>
        (kindFilter === '' || e.kind === kindFilter) &&
        (actorFilter === '' || actorKind(e.actor) === actorFilter),
    );
    return [...filtered].sort((a, b) => b.seq - a.seq);
  }, [events, kindFilter, actorFilter]);

  if (events.length === 0) {
    return (
      <Empty
        title="No events recorded"
        detail="Every state transition appends a row to run_events. An empty history means nothing has happened yet, or the event-service is not writing."
      />
    );
  }

  return (
    <>
      <div className="panel-head" style={{ borderTop: '1px solid var(--border)' }}>
        <label className="row" style={{ gap: 5 }}>
          <span style={{ textTransform: 'none', letterSpacing: 0 }}>kind</span>
          <select value={kindFilter} onChange={(e) => setKindFilter(e.target.value)}>
            <option value="">all ({events.length})</option>
            {kinds.map((k) => (
              <option key={k} value={k}>
                {k}
              </option>
            ))}
          </select>
        </label>
        <label className="row" style={{ gap: 5 }}>
          <span style={{ textTransform: 'none', letterSpacing: 0 }}>actor</span>
          <select value={actorFilter} onChange={(e) => setActorFilter(e.target.value)}>
            <option value="">all</option>
            {actors.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </select>
        </label>
        <span className="spacer" />
        <span className="chip">{ordered.length} shown</span>
      </div>
      <div className="timeline table-scroll">
        <table>
          <thead>
            <tr>
              <th style={{ width: 54 }}>seq</th>
              <th style={{ width: 150 }}>when</th>
              <th style={{ width: 180 }}>kind</th>
              <th style={{ width: 140 }}>actor</th>
              <th style={{ width: 130 }}>node</th>
              <th>payload</th>
            </tr>
          </thead>
          <tbody>
            {ordered.map((e) => (
              <tr key={`${e.seq}-${e.id}`}>
                <td className="num faint">{e.seq}</td>
                <td className="nowrap" title={fmtTs(e.occurred_at)}>
                  <div>{fmtTs(e.occurred_at)}</div>
                  <div className="faint" style={{ fontSize: 'var(--text-xs)' }}>
                    {fmtAge(e.occurred_at, now)}
                  </div>
                </td>
                <td className="mono">{e.kind}</td>
                <td>
                  <span className={`actor ${actorKind(e.actor)}`} title={e.actor}>
                    {actorLabel(e.actor)}
                  </span>
                </td>
                <td className="mono">
                  {e.node_id ? (
                    onSelectNode ? (
                      <button className="link" onClick={() => onSelectNode(e.node_id!)}>
                        {e.node_id}
                      </button>
                    ) : (
                      e.node_id
                    )
                  ) : (
                    <span className="faint">—</span>
                  )}
                  {e.attempt_id && (
                    <div className="faint" style={{ fontSize: 10.5 }} title={e.attempt_id}>
                      {shortId(e.attempt_id, 8)}
                    </div>
                  )}
                </td>
                <td>
                  <PayloadCell payload={e.payload} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

/**
 * Payloads are free-form JSONB. Flat scalars render as inline key=value, which
 * is far more scannable than pretty-printed JSON in a dense table; anything
 * nested falls back to compact JSON in an expandable cell.
 */
function PayloadCell({ payload }: { payload: Record<string, unknown> | undefined }) {
  const [open, setOpen] = useState(false);
  if (!payload || Object.keys(payload).length === 0) return <span className="faint">—</span>;

  const entries = Object.entries(payload);
  const allScalar = entries.every(
    ([, v]) => v === null || ['string', 'number', 'boolean'].includes(typeof v),
  );

  if (allScalar) {
    return (
      <span className="mono" style={{ fontSize: 11.5 }}>
        {entries.map(([k, v]) => `${k}=${String(v)}`).join('  ')}
      </span>
    );
  }
  return (
    <div>
      <button className="link" onClick={() => setOpen((o) => !o)}>
        {open ? 'hide' : `${entries.length} fields`}
      </button>
      {open && (
        <pre style={{ margin: 'var(--space-2) 0 0', whiteSpace: 'pre-wrap', fontSize: 'var(--text-xs)' }}>
          {JSON.stringify(payload, null, 2)}
        </pre>
      )}
    </div>
  );
}
