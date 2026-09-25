// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Full attempt history for a node.
 *
 * Every attempt is a separate card because a retry is a NEW attempt row and a
 * terminal attempt is never mutated (invariant 9). Collapsing retries into "the
 * node failed 3 times" would throw away exactly the information that makes a
 * flaky task diagnosable: which worker, which error class, how long each try
 * lasted, whether the failures are identical or drifting.
 */

import type { Attempt, ResourceRequest, TaskError, ExecutionMetrics } from '../api/types';
import {
  EMPTY,
  durationBetween,
  fmtAge,
  fmtBytes,
  fmtDurationMs,
  fmtPercent,
  fmtResources,
  fmtTs,
  shortId,
} from '../lib/format';
import { executorStatus, isExecutorWaiting, isInFlight, progressFraction } from '../lib/progress';
import { normalizeState } from '../lib/states';
import { StateBadge } from './StateBadge';
import { Empty } from './Status';

export function AttemptList({
  attempts,
  selectedAttemptId,
  onSelectAttempt,
}: {
  attempts: Attempt[];
  selectedAttemptId?: string | null;
  onSelectAttempt?: (attemptId: string) => void;
}) {
  if (attempts.length === 0) {
    return (
      <Empty
        title="No attempts yet"
        detail="The DAG engine creates an attempt when the node's trigger rule is satisfied. A node with no attempts has not become ready."
      />
    );
  }
  // Newest attempt first: during an incident the current try is what matters,
  // and the history is read downward from it.
  const ordered = [...attempts].sort((a, b) => b.attempt_number - a.attempt_number);
  return (
    <div>
      {ordered.map((a) => (
        <AttemptCard
          key={a.id}
          attempt={a}
          isLatest={a.attempt_number === ordered[0]?.attempt_number}
          selected={a.id === selectedAttemptId}
          onSelect={onSelectAttempt}
        />
      ))}
    </div>
  );
}

function AttemptCard({
  attempt,
  isLatest,
  selected,
  onSelect,
}: {
  attempt: Attempt;
  isLatest: boolean;
  selected: boolean;
  onSelect?: (id: string) => void;
}) {
  const state = normalizeState(attempt.state);
  const queueWait = durationBetween(attempt.scheduled_at, attempt.dispatched_at);
  const startupLag = durationBetween(attempt.dispatched_at, attempt.started_at);
  const runtime = durationBetween(attempt.started_at, attempt.ended_at);
  const total = durationBetween(attempt.scheduled_at, attempt.ended_at);

  return (
    <section
      className="attempt-card"
      style={selected ? { borderColor: 'var(--focus)', boxShadow: '0 0 0 1px var(--focus)' } : undefined}
    >
      <header className="attempt-head">
        <strong className="mono">attempt #{attempt.attempt_number}</strong>
        <StateBadge kind="attempt" state={attempt.state} />
        {isLatest && <span className="chip">latest</span>}
        {attempt.attempt_number > 1 && <span className="chip">retry</span>}
        {attempt.queue && <span className="chip">queue: {attempt.queue}</span>}
        {attempt.priority !== undefined && <span className="chip">prio {attempt.priority}</span>}
        <span className="spacer" style={{ marginLeft: 'auto' }} />
        <span className="faint mono" title={attempt.id}>
          {shortId(attempt.id, 12)}
        </span>
        {onSelect && (
          <button className="link" onClick={() => onSelect(attempt.id)}>
            logs
          </button>
        )}
      </header>

      {/* pending_reason is the difference between "unexplained stall" and
          "waiting_for_resources: gpu (need 4, free 3)". It leads the card. */}
      {attempt.pending_reason && state === 'PENDING' && (
        <div style={{ padding: '10px 11px 0' }}>
          <div className="attempt-section-title">Why this has not dispatched (scheduler placement)</div>
          <div className="pendingbox">{attempt.pending_reason}</div>
        </div>
      )}
      {attempt.pending_reason && state !== 'PENDING' && (
        <div style={{ padding: '10px 11px 0' }}>
          <div className="attempt-section-title">Last pending reason (scheduler placement, historical)</div>
          <div className="pendingbox muted">{attempt.pending_reason}</div>
        </div>
      )}

      <ExecutorStatus attempt={attempt} />

      <div className="attempt-body">
        <div>
          <div className="attempt-section-title">Placement</div>
          <dl className="kv">
            <dt>worker</dt>
            <dd title={attempt.worker_id ?? undefined}>
              {attempt.worker_hostname ?? (attempt.worker_id ? shortId(attempt.worker_id, 12) : EMPTY)}
            </dd>
            <dt>worker id</dt>
            <dd className="faint">{attempt.worker_id ? shortId(attempt.worker_id, 12) : EMPTY}</dd>
            <dt>idem key</dt>
            <dd className="faint" title={attempt.idempotency_key ?? undefined}>
              {shortId(attempt.idempotency_key, 16)}
            </dd>
          </dl>
        </div>

        <div>
          <div className="attempt-section-title">Timings</div>
          <dl className="kv">
            <dt>scheduled</dt>
            <dd>{fmtTs(attempt.scheduled_at)}</dd>
            <dt>dispatched</dt>
            <dd>{fmtTs(attempt.dispatched_at)}</dd>
            <dt>started</dt>
            <dd>{fmtTs(attempt.started_at)}</dd>
            <dt>ended</dt>
            <dd>{fmtTs(attempt.ended_at)}</dd>
            <dt>queue wait</dt>
            <dd>{fmtDurationMs(queueWait)}</dd>
            <dt>dispatch→start</dt>
            <dd>{fmtDurationMs(startupLag)}</dd>
            <dt>runtime</dt>
            <dd>{fmtDurationMs(runtime)}</dd>
            <dt>total</dt>
            <dd>{fmtDurationMs(total)}</dd>
          </dl>
        </div>

        <div>
          <div className="attempt-section-title">Resource request</div>
          <ResourceBlock resources={attempt.resource_request} />
        </div>

        <div>
          <div className="attempt-section-title">Metrics</div>
          <MetricsBlock metrics={attempt.metrics} />
        </div>

        {attempt.error && (
          <div style={{ gridColumn: '1 / -1' }}>
            <div className="attempt-section-title">Error</div>
            <ErrorBlock error={attempt.error} state={state} />
          </div>
        )}

        {!attempt.error && state === 'LOST' && (
          <div style={{ gridColumn: '1 / -1' }}>
            <div className="attempt-section-title">Outcome unknown</div>
            <div className="pendingbox">
              The lease expired without a result. We do not know whether this attempt ran, is still
              running, or had side effects — that is what LOST means, and it is not FAILED. Whether it
              was auto-retried depends on the task's idempotency mode.
            </div>
          </div>
        )}
      </div>
    </section>
  );
}

/**
 * What the worker's executor says about this attempt (F96), kept visibly apart
 * from the scheduler's pending reason above: that one explains why Nexus has
 * not placed the attempt, this one what the substrate (a Kubernetes pod, say)
 * is doing with an attempt Nexus DID place.
 *
 * Shown only once the attempt has reached a worker. A PENDING attempt has no
 * executor yet, so an empty state there would imply one was expected.
 */
function ExecutorStatus({ attempt }: { attempt: Attempt }) {
  const state = normalizeState(attempt.state);
  const status = executorStatus(attempt);
  const fraction = progressFraction(attempt);
  const inFlight = isInFlight(attempt);
  if (!status && fraction === null) {
    // `undefined` means this gateway does not serve the field at all, which is
    // not the same as "the worker reported nothing"; say nothing rather than
    // claim the latter.
    if (!inFlight || attempt.progress_message === undefined) return null;
    return (
      <div style={{ padding: '10px 11px 0' }}>
        <div className="attempt-section-title">Executor status (reported by the worker)</div>
        <div className="muted">
          No status reported. The worker has not sent a progress message for this attempt; the
          Kubernetes executor reports pod phase changes, the subprocess and docker executors send none.
        </div>
      </div>
    );
  }
  const waiting = isExecutorWaiting(attempt);
  return (
    <div style={{ padding: '10px 11px 0' }} data-testid="executor-status">
      <div className="attempt-section-title">
        {status?.stale ? 'Last executor status (historical, reported by the worker)' : 'Executor status (reported by the worker)'}
      </div>
      {status && (
        <div className={`pendingbox${waiting ? '' : ' muted'}`} title={status.message}>
          {status.message}
        </div>
      )}
      <dl className="kv" style={{ marginTop: 6 }}>
        {fraction !== null && (
          <>
            <dt>progress</dt>
            <dd>
              <progress max={1} value={fraction} aria-label="reported progress" /> {fmtPercent(fraction)}
            </dd>
          </>
        )}
        {attempt.progress_message_at && (
          <>
            <dt>{status?.stale ? 'status since' : 'in this status'}</dt>
            <dd title={fmtTs(attempt.progress_message_at)}>
              {status?.stale ? fmtTs(attempt.progress_message_at) : fmtAge(attempt.progress_message_at).replace(/ ago$/, '')}
            </dd>
          </>
        )}
        {attempt.progress_at && (
          <>
            <dt>last report</dt>
            <dd title={fmtTs(attempt.progress_at)}>{fmtAge(attempt.progress_at)}</dd>
          </>
        )}
      </dl>
      {waiting && state === 'RUNNING' && (
        <div className="faint" style={{ marginTop: 4 }}>
          Nexus placed this attempt, but its pod has not started. The reason above comes from the
          cluster's scheduler, not from Nexus.
        </div>
      )}
    </div>
  );
}

function ResourceBlock({ resources }: { resources: ResourceRequest | null | undefined }) {
  if (!resources || (!resources.request && !resources.limit)) {
    return <div className="muted">{EMPTY}</div>;
  }
  return (
    <dl className="kv">
      <dt>request</dt>
      <dd>{fmtResources(resources.request)}</dd>
      <dt>limit</dt>
      <dd>{resources.limit ? fmtResources(resources.limit) : EMPTY}</dd>
    </dl>
  );
}

function MetricsBlock({ metrics }: { metrics: ExecutionMetrics | null | undefined }) {
  if (!metrics) {
    return (
      <div className="muted">
        {EMPTY} <span className="faint">(reported by the worker on completion)</span>
      </div>
    );
  }
  return (
    <dl className="kv">
      <dt>peak memory</dt>
      <dd>{fmtBytes(metrics.peak_memory_bytes)}</dd>
      <dt>cpu seconds</dt>
      <dd>{metrics.cpu_seconds !== undefined ? metrics.cpu_seconds.toFixed(2) : EMPTY}</dd>
      <dt>wall</dt>
      <dd>{fmtDurationMs(metrics.wall_ms)}</dd>
      <dt>exit code</dt>
      <dd>{metrics.exit_code !== undefined ? metrics.exit_code : EMPTY}</dd>
      <dt>oom killed</dt>
      <dd>{metrics.oom_killed ? 'YES' : metrics.oom_killed === 0 ? 'no' : EMPTY}</dd>
    </dl>
  );
}

function ErrorBlock({ error, state }: { error: TaskError; state: string }) {
  const cls = normalizeState(error.class);
  return (
    <div className="errbox">
      <div className="row" style={{ gap: 'var(--space-4)', marginBottom: 5 }}>
        {cls && <span className="chip warn">{cls}</span>}
        {error.type && <strong className="mono">{error.type}</strong>}
        {error.retryable !== undefined && (
          <span className="chip">worker says {error.retryable ? 'retryable' : 'terminal'}</span>
        )}
        {state === 'TIMED_OUT' && <span className="chip warn">deadline fired</span>}
      </div>
      <div className="mono" style={{ whiteSpace: 'pre-wrap', fontSize: 11.5 }}>
        {error.message || '(no message reported)'}
      </div>
      {error.detail_ref && (
        <div className="faint mono" style={{ marginTop: 5 }}>
          full diagnostic: {error.detail_ref}
        </div>
      )}
    </div>
  );
}
