/**
 * Run detail: DAG, attempt history, log tail, event timeline, and cancel.
 *
 * Layout reasoning: the graph answers "what broke", the attempt list answers
 * "why", the log answers "what did it say", the timeline answers "who did what
 * when". They are on one screen because during an incident the operator moves
 * between those four questions in seconds, and a tab switch that loses the
 * selected node breaks that loop.
 */

import { useEffect, useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import {
  errorMessage,
  useCancelRun,
  useInvalidateOnRevision,
  useRun,
  useRunAttempts,
  useRunEvents,
} from '../api/hooks';
import { useRunLive } from '../api/live';
import { buildDagGraph, summarizeDag } from '../lib/dag';
import { isTerminal, stateVisual } from '../lib/states';
import { durationBetween, fmtDurationMs, fmtTs, shortId } from '../lib/format';
import { StateBadge } from '../components/StateBadge';
import { AsyncPanel, ErrorBox, Loading, StaleWarning } from '../components/Status';
import { DagView } from '../components/DagView';
import { AttemptList } from '../components/AttemptDetail';
import { LogTail } from '../components/LogTail';
import { EventTimeline } from '../components/EventTimeline';

export function RunDetailPage({ namespace }: { namespace: string }) {
  const { runId = '' } = useParams();
  const [params, setParams] = useSearchParams();
  const selectedNodeId = params.get('node');
  const selectedAttemptId = params.get('attempt');

  const live = useRunLive(namespace, runId || undefined);
  useInvalidateOnRevision(live.revision, namespace, runId || undefined);

  const runQuery = useRun(namespace, runId, live.transport);
  const terminal = runQuery.terminal;
  const attemptsQuery = useRunAttempts(namespace, runId, live.transport, terminal);
  const eventsQuery = useRunEvents(namespace, runId, live.transport, terminal);
  const cancel = useCancelRun(namespace);

  const graph = useMemo(
    () =>
      runQuery.data
        ? buildDagGraph(runQuery.data, attemptsQuery.data ?? [])
        : null,
    [runQuery.data, attemptsQuery.data],
  );

  const selectNode = (nodeId: string) => {
    const next = new URLSearchParams(params);
    next.set('node', nodeId);
    next.delete('attempt');
    setParams(next, { replace: true });
  };
  const selectAttempt = (attemptId: string) => {
    const next = new URLSearchParams(params);
    next.set('attempt', attemptId);
    setParams(next, { replace: true });
  };

  // Default the selection to the root cause when there is one: the first thing
  // an operator opening a failed run wants is the node that actually broke.
  useEffect(() => {
    if (selectedNodeId || !graph) return;
    const target = graph.rootCauses[0];
    if (target) {
      const next = new URLSearchParams(params);
      next.set('node', target);
      setParams(next, { replace: true });
    }
    // `params` intentionally omitted: this should run once per graph identity.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [graph, selectedNodeId]);

  const selectedNode = selectedNodeId && graph ? graph.byId[selectedNodeId] : undefined;
  const selectedAttempt =
    selectedNode?.attempts.find((a) => a.id === selectedAttemptId) ??
    selectedNode?.latestAttempt ??
    null;

  if (runQuery.error && runQuery.data === undefined) {
    return (
      <div className="col">
        <Link to="/runs">← all runs</Link>
        <ErrorBox error={runQuery.error} what={`run ${shortId(runId, 12)}`} onRetry={runQuery.refetch} />
      </div>
    );
  }
  if (!runQuery.data) return <Loading rows={8} label="Loading run" />;

  const run = runQuery.data;
  const canCancel = !isTerminal('run', run.state) && run.state !== 'CANCELLING';
  const duration = terminal
    ? durationBetween(run.started_at ?? run.created_at, run.ended_at)
    : run.started_at
      ? Date.now() - Date.parse(run.started_at)
      : null;

  return (
    <div className="col">
      <div className="row" style={{ alignItems: 'flex-start' }}>
        <div style={{ minWidth: 0 }}>
          <div className="row" style={{ gap: 9 }}>
            <Link to="/runs" className="sub">
              ← all runs
            </Link>
            <StateBadge kind="run" state={run.state} />
            <h1 style={{ margin: 0 }} className="mono">
              {shortId(run.id, 14)}
            </h1>
          </div>
          <div className="sub" style={{ marginTop: 3 }}>
            {run.spec_name ?? shortId(run.spec_id, 10)}
            {run.spec_version !== undefined && ` v${run.spec_version}`} · created {fmtTs(run.created_at)} ·{' '}
            {fmtDurationMs(duration)}
            {run.deadline_at && ` · deadline ${fmtTs(run.deadline_at)}`}
          </div>
          <div className="sub" title={stateVisual('run', run.state).hint}>
            {stateVisual('run', run.state).hint}
          </div>
        </div>
        <span className="spacer" style={{ marginLeft: 'auto' }} />
        <CancelButton
          canCancel={canCancel}
          state={run.state}
          pending={cancel.isPending}
          error={cancel.error}
          onCancel={() => cancel.mutate(run.id)}
        />
      </div>

      {runQuery.error != null ? <StaleWarning error={runQuery.error} /> : null}

      <div className="panel">
        <div className="panel-head">
          <span>dag</span>
          {graph && <span className="chip">{graph.nodes.length} nodes</span>}
          {graph && graph.rootCauses.length > 0 && (
            <span className="chip warn">root cause: {graph.rootCauses.join(', ')}</span>
          )}
          <span className="spacer" />
          <span className="sub" style={{ textTransform: 'none', letterSpacing: 0 }}>
            {graph ? summarizeDag(graph) : ''}
          </span>
        </div>
        {attemptsQuery.error != null && attemptsQuery.data === undefined ? (
          <div style={{ padding: 12, paddingBottom: 0 }}>
            <ErrorBox error={attemptsQuery.error} what="attempts (node states still shown below)" />
          </div>
        ) : null}
        {graph ? (
          <DagView graph={graph} selectedNodeId={selectedNodeId} onSelect={selectNode} />
        ) : null}
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>node · {selectedNode ? selectedNode.node_id : 'select a node'}</span>
          {selectedNode && <StateBadge kind="node" state={selectedNode.state} small />}
          {selectedNode?.isRootCause && <span className="chip warn">root cause</span>}
          <span className="spacer" />
          {selectedNode && (
            <span className="sub" style={{ textTransform: 'none', letterSpacing: 0 }}>
              <code>{selectedNode.task_ref}</code> · trigger {selectedNode.trigger_rule ?? 'all_success'} ·{' '}
              {selectedNode.attempts_used ?? selectedNode.attempts.length}/{selectedNode.max_attempts ?? '?'}{' '}
              attempts used
            </span>
          )}
        </div>
        <div className="panel-body">
          {!selectedNode ? (
            <div className="notice empty">
              <span className="notice-glyph">↑</span>
              <div>
                <div className="notice-title">Pick a node in the graph</div>
                <div className="notice-detail">
                  Every attempt for that node, including retries, appears here with its worker, timings,
                  resources, error and metrics.
                </div>
              </div>
            </div>
          ) : (
            <>
              {selectedNode.upstream.length > 0 && (
                <div className="sub" style={{ marginBottom: 9 }}>
                  upstream:{' '}
                  {selectedNode.upstream.map((id) => (
                    <button key={id} className="link" onClick={() => selectNode(id)} style={{ marginRight: 7 }}>
                      {id}
                    </button>
                  ))}
                </div>
              )}
              <AttemptList
                attempts={selectedNode.attempts}
                selectedAttemptId={selectedAttempt?.id ?? null}
                onSelectAttempt={selectAttempt}
              />
            </>
          )}
        </div>
      </div>

      {selectedAttempt && (
        <LogTail
          namespace={namespace}
          attemptId={selectedAttempt.id}
          live={!isTerminal('attempt', selectedAttempt.state)}
        />
      )}

      <div className="panel">
        <div className="panel-head">
          <span>event timeline</span>
          <span className="spacer" />
          <span className="sub" style={{ textTransform: 'none', letterSpacing: 0 }}>
            append-only; the projections above are derived from this
          </span>
        </div>
        <AsyncPanel
          query={eventsQuery}
          what="events"
          isEmpty={(e) => e.length === 0}
          emptyTitle="No events recorded"
          emptyDetail="Every state transition appends a row to run_events."
          loadingRows={5}
        >
          {(events) => <EventTimeline events={events} onSelectNode={selectNode} />}
        </AsyncPanel>
      </div>

      <div className="sub">
        run id <code>{run.id}</code> · spec id <code>{run.spec_id}</code> ·{' '}
        {run.params && Object.keys(run.params).length > 0 ? (
          <>params <code>{JSON.stringify(run.params)}</code></>
        ) : (
          <>no params</>
        )}
      </div>
    </div>
  );
}

/**
 * Cancel is the only mutation in Phase 1, so it gets a confirmation step: it
 * kills in-flight work (unlike pause), and an accidental click costs real
 * compute.
 */
function CancelButton({
  canCancel,
  state,
  pending,
  error,
  onCancel,
}: {
  canCancel: boolean;
  state: string;
  pending: boolean;
  error: unknown;
  onCancel: () => void;
}) {
  const [confirming, setConfirming] = useState(false);

  if (!canCancel) {
    return (
      <div className="row" style={{ gap: 8 }}>
        <button disabled title={`A ${state} run cannot be cancelled.`}>
          cancel run
        </button>
      </div>
    );
  }

  return (
    <div style={{ textAlign: 'right' }}>
      {confirming ? (
        <div className="row" style={{ gap: 7, justifyContent: 'flex-end' }}>
          <span className="sub">Cancel kills in-flight attempts. Sure?</span>
          <button className="danger" onClick={onCancel} disabled={pending}>
            {pending ? 'cancelling…' : 'yes, cancel'}
          </button>
          <button onClick={() => setConfirming(false)} disabled={pending}>
            keep running
          </button>
        </div>
      ) : (
        <button className="danger" onClick={() => setConfirming(true)}>
          cancel run
        </button>
      )}
      {error !== null && error !== undefined && (
        <div className="notice error" style={{ marginTop: 8, textAlign: 'left' }} role="alert">
          <span className="notice-glyph">!</span>
          <div>
            <div className="notice-title">Cancel failed</div>
            <div className="notice-detail">{errorMessage(error)}</div>
          </div>
        </div>
      )}
    </div>
  );
}
