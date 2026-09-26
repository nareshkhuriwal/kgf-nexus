// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { useNavigate } from 'react-router-dom';
import { useMutation } from '@tanstack/react-query';
import { api } from '../api/client';
import { Permit, usePermission } from '../components/Permissions';
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
import { groupExpansions } from '../lib/expansion';
import { isTerminal, stateVisual } from '../lib/states';
import { durationBetween, fmtDurationMs, fmtTs, shortId } from '../lib/format';
import { StateBadge } from '../components/StateBadge';
import { AsyncPanel, ErrorBox, Loading, StaleWarning } from '../components/Status';
import { DagView } from '../components/DagView';
import { AttemptList } from '../components/AttemptDetail';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { PlacementPanel } from '../components/PlacementPanel';
import { RunUsagePanel } from '../components/UsageUnits';
import { LogTail } from '../components/LogTail';
import { EventTimeline } from '../components/EventTimeline';
import { ExecutionTimeline, type TimelineMode } from '../components/ExecutionTimeline';

/**
 * The node states a retry may name. The gateway enforces the same set; offering
 * the button only where it will be accepted keeps "Retry this task" from being a
 * control that mostly answers 409.
 */
const RETRYABLE_NODE_STATES: ReadonlySet<string> = new Set(['FAILED', 'UPSTREAM_FAILED', 'CANCELLED']);

export function RunDetailPage({ namespace }: { namespace: string }) {
  const { runId = '' } = useParams();
  const navigate = useNavigate();
  const can = usePermission();
  const rerun = useMutation({mutationFn: async () => {
    const current = await api.getRun(namespace, runId);
    return api.createRun(namespace, {spec_id: current.spec_id, params: current.params, priority: current.priority, trigger_kind: 'manual'});
  }, onSuccess: (created) => navigate('/runs/' + created.id)});
  // One mutation for both retries: no nodes is the whole run, a node list is
  // that node plus its dependents. Same endpoint, same refusal handling.
  const retry = useMutation({mutationFn: (nodes?: string[]) => api.retryRun(namespace, runId, nodes), onSuccess: () => { void runQuery.refetch(); }});
  // Which terminal-run action is awaiting confirmation. All of them start real
  // compute, so none fires on the first click; the dialog says what each one
  // re-executes before it does.
  const [confirming, setConfirming] = useState<'rerun' | 'retry' | 'retry-node' | null>(null);
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

  // Map templates start folded: a fan-out of hundreds drawn node by node is not
  // a graph anyone can read (lib/expansion).
  const [unfolded, setUnfolded] = useState<ReadonlySet<string>>(new Set());
  const toggleGroup = (templateId: string) =>
    setUnfolded((cur) => {
      const next = new Set(cur);
      if (next.has(templateId)) next.delete(templateId);
      else next.add(templateId);
      return next;
    });
  const graph = useMemo(() => {
    if (!runQuery.data) return null;
    const grouped = groupExpansions(runQuery.data, attemptsQuery.data ?? [], unfolded);
    return buildDagGraph({ nodes: grouped.nodes, edges: grouped.edges }, attemptsQuery.data ?? []);
  }, [runQuery.data, attemptsQuery.data, unfolded]);

  // The same run with every map child drawn individually. The DAG folds large
  // fan-outs, but the timeline lists every attempt, so a child selected there
  // (or by a deep link) must still resolve to a node with its attempts and
  // upstream -- otherwise the attempt panel would say "pick a node" while the
  // URL names one.
  const detailGraph = useMemo(
    () => (runQuery.data ? buildDagGraph(runQuery.data, attemptsQuery.data ?? []) : null),
    [runQuery.data, attemptsQuery.data],
  );
  const parentOf = useMemo(() => {
    const out: Record<string, string> = {};
    for (const n of runQuery.data?.nodes ?? []) if (n.parent_node_id) out[n.node_id] = n.parent_node_id;
    return out;
  }, [runQuery.data]);

  const timelineMode: TimelineMode = params.get('timeline') === 'table' ? 'table' : 'chart';
  const setTimelineMode = (mode: TimelineMode) => {
    const next = new URLSearchParams(params);
    if (mode === 'table') next.set('timeline', 'table');
    else next.delete('timeline');
    setParams(next, { replace: true });
  };

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
  /** Node and attempt in one URL write, so the two never disagree mid-render. */
  const selectNodeAttempt = (nodeId: string, attemptId: string) => {
    const next = new URLSearchParams(params);
    next.set('node', nodeId);
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

  const selectedNode =
    selectedNodeId && graph ? (graph.byId[selectedNodeId] ?? detailGraph?.byId[selectedNodeId]) : undefined;
  // A child folded into its template highlights the template in the graph.
  const dagSelectedId =
    selectedNodeId && graph && !graph.byId[selectedNodeId] && parentOf[selectedNodeId]
      ? parentOf[selectedNodeId]
      : selectedNodeId;
  const selectedAttempt =
    selectedNode?.attempts.find((a) => a.id === selectedAttemptId) ??
    selectedNode?.latestAttempt ??
    null;

  if (runQuery.error && runQuery.data === undefined) {
    return (
      <div className="col">
        <div className="run-head">
          <div className="run-head-id">
            <div className="run-head-line">
              <h1>{shortId(runId, 14)}</h1>
            </div>
          </div>
          <div className="run-head-actions"><Link to="/runs" className="btn">← Back to runs</Link></div>
        </div>
        <ErrorBox error={runQuery.error} what={`run ${shortId(runId, 12)}`} onRetry={runQuery.refetch} />
      </div>
    );
  }
  // The way back is part of the loading state, not something that appears once
  // the run arrives: a run that is slow to load is exactly when an operator
  // wants to give up and go back to the list.
  if (!runQuery.data) {
    return (
      <div className="col">
        <div className="run-head">
          <div className="run-head-id">
            <div className="run-head-line">
              <h1>{shortId(runId, 14)}</h1>
            </div>
          </div>
          <div className="run-head-actions"><Link to="/runs" className="btn">← Back to runs</Link></div>
        </div>
        <div className="panel">
          <Loading rows={8} label="Loading run" />
        </div>
      </div>
    );
  }

  const run = runQuery.data;
  const canCancel = !isTerminal('run', run.state) && run.state !== 'CANCELLING';
  const duration = terminal
    ? durationBetween(run.started_at ?? run.created_at, run.ended_at)
    : run.started_at
      ? Date.now() - Date.parse(run.started_at)
      : null;

  return (
    <div className="col">
      <div className="run-head">
        <div className="run-head-id">
          <div className="run-head-line">
            <StateBadge kind="run" state={run.state} />
            <h1>{shortId(run.id, 14)}</h1>
          </div>
          {/* The identity facts as discrete items rather than one run-on
              sentence: at phone width they wrap into a short list instead of a
              paragraph with interpuncts stranded at line ends. */}
          <div className="run-head-meta">
            <span>
              {run.spec_name ?? shortId(run.spec_id, 10)}
              {run.spec_version !== undefined && ` v${run.spec_version}`}
            </span>
            <span aria-hidden="true">·</span>
            <span className="nowrap">created {fmtTs(run.created_at)}</span>
            <span aria-hidden="true">·</span>
            <span className="nowrap">{fmtDurationMs(duration)}</span>
            {run.deadline_at && (
              <>
                <span aria-hidden="true">·</span>
                <span className="nowrap">deadline {fmtTs(run.deadline_at)}</span>
              </>
            )}
          </div>
          <div className="run-head-hint">{stateVisual('run', run.state).hint}</div>
        </div>
        <div className="run-head-actions">
          <Link to="/runs" className="btn">← Back to runs</Link>
          {isTerminal('run', run.state) ? <>
            {can('runs.create') && can('workflows.view') && <button className="primary" disabled={rerun.isPending} onClick={() => setConfirming('rerun')}>{rerun.isPending ? 'Starting…' : 'Re-run'}</button>}
            {(run.state === 'FAILED' || run.state === 'TIMED_OUT') && can('runs.retry') && <button disabled={retry.isPending} onClick={() => setConfirming('retry')}>{retry.isPending ? 'Retrying…' : 'Retry failed tasks'}</button>}
            {rerun.error && <ErrorBox error={rerun.error} what="re-run" />}{retry.error && <ErrorBox error={retry.error} what="retry" />}
            {confirming === 'rerun' && (
              <ConfirmDialog
                title="Start a new run?"
                confirmLabel="Start new run"
                onConfirm={() => { setConfirming(null); rerun.mutate(); }}
                onCancel={() => setConfirming(null)}
              >
                Every task in <code>{run.spec_name ?? shortId(run.spec_id, 10)}</code>
                {run.spec_version !== undefined && <> v{run.spec_version}</>} executes again with
                this run's original parameters, as a separate run. This run and its history are
                left as they are.
              </ConfirmDialog>
            )}
            {confirming === 'retry' && (
              <ConfirmDialog
                title="Retry the failed tasks?"
                confirmLabel="Retry failed tasks"
                onConfirm={() => { setConfirming(null); retry.mutate(); }}
                onCancel={() => setConfirming(null)}
              >
                Every task that did not succeed — failed, timed out, cancelled or never
                reached — gets a new attempt, in this same run. Tasks that succeeded keep
                their results and do not execute again.
              </ConfirmDialog>
            )}
            {confirming === 'retry-node' && selectedNode && (
              <ConfirmDialog
                title={`Retry ${selectedNode.node_id}?`}
                confirmLabel="Retry this task"
                onConfirm={() => { setConfirming(null); retry.mutate([selectedNode.node_id]); }}
                onCancel={() => setConfirming(null)}
              >
                <code>{selectedNode.node_id}</code> gets a new attempt, and so does every task
                downstream of it that did not succeed. Other failed tasks, and every task that
                succeeded, are left as they are.
              </ConfirmDialog>
            )}
          </> : can('runs.cancel') && <CancelButton
            canCancel={canCancel}
            state={run.state}
            pending={cancel.isPending}
            error={cancel.error}
            onCancel={() => cancel.mutate(run.id)}
          />}
        </div>
      </div>

      {runQuery.error != null ? <StaleWarning error={runQuery.error} /> : null}

      <div className="panel">
        <div className="panel-head">
          <span>Workflow graph</span>
          {graph && <span className="chip">{graph.nodes.length} nodes</span>}
          {graph && graph.rootCauses.length > 0 && (
            <span className="chip warn">root cause: {graph.rootCauses.join(', ')}</span>
          )}
          <span className="spacer" />
          <span className="panel-note">{graph ? summarizeDag(graph) : ''}</span>
        </div>
        {attemptsQuery.error != null && attemptsQuery.data === undefined ? (
          <div className="panel-notice">
            <ErrorBox error={attemptsQuery.error} what="attempts (node states still shown below)" />
          </div>
        ) : null}
        {graph ? (
          <DagView
            graph={graph}
            selectedNodeId={dagSelectedId}
            onSelect={selectNode}
            // Names the exported file and keys the graph's one-shot
            // fit-to-view, so this page's polling cannot re-fit the view under
            // an operator who has zoomed in on a node.
            exportName={runId}
            onToggleGroup={toggleGroup}
          />
        ) : null}
      </div>

      <div className="panel" data-testid="execution-timeline">
        {attemptsQuery.data === undefined && attemptsQuery.isPending ? (
          <Loading rows={4} label="Loading attempts" />
        ) : (
          <ExecutionTimeline
            run={run}
            attempts={attemptsQuery.data ?? []}
            live={!terminal}
            selectedNodeId={selectedNodeId}
            selectedAttemptId={selectedAttemptId}
            onSelectAttempt={selectNodeAttempt}
            onSelectNode={selectNode}
            mode={timelineMode}
            onModeChange={setTimelineMode}
          />
        )}
      </div>

      {graph && (
        <PlacementPanel
          graph={graph}
          selectedNodeId={selectedNodeId}
          onSelectNode={selectNode}
          live={!terminal}
        />
      )}

      {/* Beside placement (what the run asked the cluster for): what it spent
          at providers. Waits for the attempts so it never flashes "none". */}
      {attemptsQuery.data !== undefined && <RunUsagePanel attempts={attemptsQuery.data} />}

      <div className="panel">
        <div className="panel-head">
          <span>node · {selectedNode ? selectedNode.node_id : 'select a node'}</span>
          {selectedNode && <StateBadge kind="node" state={selectedNode.state} small />}
          {selectedNode?.isRootCause && <span className="chip warn">root cause</span>}
          <span className="spacer" />
          {selectedNode && (run.state === 'FAILED' || run.state === 'TIMED_OUT') && can('runs.retry') &&
            RETRYABLE_NODE_STATES.has(selectedNode.state) && (
            <button disabled={retry.isPending} onClick={() => setConfirming('retry-node')}>
              {retry.isPending ? 'Retrying…' : 'Retry this task'}
            </button>
          )}
          {selectedNode && (
            <span className="panel-note">
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
                <div className="upstream-walk">
                  <span>upstream</span>
                  {selectedNode.upstream.map((id) => (
                    <button key={id} className="link" onClick={() => selectNode(id)}>
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
        <Permit permission="tasks.logs.view"><LogTail
          namespace={namespace}
          attemptId={selectedAttempt.id}
          live={!isTerminal('attempt', selectedAttempt.state)}
        /></Permit>
      )}

      <div className="panel">
        <div className="panel-head">
          <span>event timeline</span>
          <span className="spacer" />
          <span className="panel-note">
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

      {/* The identifiers, verbatim. Needed in full when they are needed at all --
          pasted into a nexusctl invocation -- so they wrap rather than truncate. */}
      <dl className="run-ids">
        <dt>run id</dt>
        <dd>{run.id}</dd>
        <dt>spec id</dt>
        <dd>{run.spec_id}</dd>
        <dt>params</dt>
        <dd>
          {run.params && Object.keys(run.params).length > 0 ? (
            JSON.stringify(run.params)
          ) : (
            <span className="faint">none</span>
          )}
        </dd>
      </dl>
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
      <button disabled title={`A ${state} run cannot be cancelled.`}>
        cancel run
      </button>
    );
  }

  return (
    <>
      {confirming ? (
        // The prompt and both choices stay together: an operator must be able to
        // read what cancel costs without leaving the button they are about to
        // press, so this replaces the button in place rather than opening a modal.
        <div className="run-confirm">
          <span className="run-confirm-prompt">Cancel kills in-flight attempts. Sure?</span>
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
        <div className="notice error run-cancel-error" role="alert">
          <span className="notice-glyph" aria-hidden="true">
            !
          </span>
          <div>
            <div className="notice-title">Cancel failed</div>
            <div className="notice-detail">{errorMessage(error)}</div>
          </div>
        </div>
      )}
    </>
  );
}
