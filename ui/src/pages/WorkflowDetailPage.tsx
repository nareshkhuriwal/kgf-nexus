// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * One workflow: its version history, the definition of a chosen version, that
 * version's dependency graph, and the form that runs it.
 *
 * The organising idea is that a workflow is not one document but a SEQUENCE of
 * immutable ones, and every other panel on this page is a view of whichever one
 * is selected. So the selected version lives in the URL rather than in state:
 * the definition an operator is reading, and the version they are about to
 * submit a run of, is the single most important thing to be able to paste to a
 * colleague. Landing with no version in the URL selects the latest, which is
 * what "open this workflow" means.
 *
 * As on the overview, each panel owns its own loading and error state. The
 * version list and the definition are separate endpoints, so a definition that
 * fails to load costs the graph and the submit form and nothing else — the
 * history stays readable, which is usually enough to tell whether the version
 * being asked for is the one that broke.
 */

import { useMemo, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { useSpecDefinition, useSpecVersions } from '../api/hooks';
import type { SpecDefinition, SpecTask, SpecVersion } from '../api/types';
import { AsyncPanel, Empty, ErrorBox, Loading } from '../components/Status';
import { PageHeader } from '../components/PageHeader';
import { SpecDagView } from '../components/SpecDagView';
import { SubmitRunForm } from './SubmitRunForm';
import { buildSpecGraph, specQueues, summarizeSpecGraph } from '../lib/specgraph';
import { EMPTY, fmtAge, fmtDurationMs, fmtResources, fmtTs, shortId } from '../lib/format';

export function WorkflowDetailPage({ namespace }: { namespace: string }) {
  const { name = '' } = useParams();
  const [params, setParams] = useSearchParams();
  const versions = useSpecVersions(namespace, name);

  // The URL wins; the newest version is the fallback. Reading it this way
  // rather than syncing into state means a pasted link and a click produce
  // exactly the same page.
  const requestedId = params.get('spec');
  const latest = versions.data?.[0];
  const specId = requestedId ?? latest?.id ?? '';

  const definition = useSpecDefinition(namespace, specId);
  const [selectedTaskId, setSelectedTaskId] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  function selectVersion(id: string) {
    const next = new URLSearchParams(params);
    next.set('spec', id);
    setParams(next, { replace: true });
    // A task id is only meaningful within one version: a task renamed or
    // removed between versions would otherwise leave a selection pointing at
    // nothing, which reads as a broken panel rather than a changed spec.
    setSelectedTaskId(null);
  }

  return (
    <div className="col">
      <PageHeader
        title={name}
        subtitle={
          <>
            <Link to="/workflows" className="sub">
              ← catalog
            </Link>{' '}
            · workflow definition in <code>{namespace}</code>
          </>
        }
        actions={
          <button
            type="button"
            className="primary"
            onClick={() => setSubmitting((v) => !v)}
            // Nothing to pin a submission to until a definition has loaded.
            disabled={definition.data === undefined}
          >
            {submitting ? 'Cancel' : 'Run this workflow'}
          </button>
        }
      />

      {submitting && definition.data && (
        <div className="panel">
          <div className="panel-head">
            <span>submit a run</span>
          </div>
          <SubmitRunForm
            namespace={namespace}
            definition={definition.data}
            onDone={() => setSubmitting(false)}
          />
        </div>
      )}

      <div className="catalog-split">
        <div className="col">
          <DefinitionPanels
            query={definition}
            specId={specId}
            selectedTaskId={selectedTaskId}
            onSelectTask={setSelectedTaskId}
          />
        </div>

        <div className="col">
          <VersionPanel
            query={versions}
            name={name}
            selectedId={specId}
            onSelect={selectVersion}
          />
        </div>
      </div>
    </div>
  );
}

/**
 * Everything derived from the one selected definition: its header facts, the
 * dependency graph, the per-task detail, and the raw document.
 *
 * Grouped into one component because they all fail together — they are four
 * renderings of a single response — and splitting them would mean four copies
 * of the same error box.
 */
function DefinitionPanels({
  query,
  specId,
  selectedTaskId,
  onSelectTask,
}: {
  query: { data: SpecDefinition | undefined; isLoading: boolean; error: unknown; refetch?: () => void };
  specId: string;
  selectedTaskId: string | null;
  onSelectTask: (id: string | null) => void;
}) {
  // Hooks must not sit behind the early returns below, so the graph is derived
  // before any of them; an absent definition yields an empty graph, which the
  // view already renders honestly.
  const graph = useMemo(() => buildSpecGraph(query.data?.spec), [query.data]);

  if (specId === '' && !query.isLoading) {
    return (
      <div className="panel">
        <Empty
          title="No version selected"
          detail="This workflow has no versions to show. If that is unexpected, the version list beside this panel will say why."
        />
      </div>
    );
  }
  if (query.error && query.data === undefined) {
    return <ErrorBox error={query.error} what="this definition" onRetry={query.refetch} />;
  }
  if (query.isLoading || query.data === undefined) {
    return <Loading rows={8} label="Loading the definition" />;
  }

  const def = query.data;
  const spec = def.spec;
  const selected = selectedTaskId ? graph.byId[selectedTaskId] : undefined;
  const queues = specQueues(graph);

  return (
    <>
      <div className="panel">
        <div className="panel-head">
          <span>
            v{def.version} · definition
          </span>
          <span className="spacer" />
          <span className="chip" title={`spec id ${def.id}`}>
            {shortId(def.id, 8)}
          </span>
        </div>
        <div className="panel-body">
          {spec.description && <p style={{ marginTop: 0 }}>{spec.description}</p>}
          <dl className="kv">
            <dt>spec id</dt>
            <dd>{def.id}</dd>
            <dt>spec hash</dt>
            <dd>{def.spec_hash}</dd>
            <dt>author</dt>
            <dd>{def.created_by || EMPTY}</dd>
            <dt>submitted</dt>
            <dd>{fmtTs(def.created_at)}</dd>
            <dt>api version</dt>
            <dd>{spec.api_version || EMPTY}</dd>
            <dt>run timeout</dt>
            <dd>{spec.run_timeout_ms ? fmtDurationMs(spec.run_timeout_ms) : EMPTY}</dd>
            <dt>max active runs</dt>
            {/* Absent is not zero: no declared cap means unbounded, and
                rendering it as 0 would read as "this can never run". */}
            <dd>{spec.max_active_runs ?? 'unbounded'}</dd>
            <dt>queues</dt>
            <dd>{queues.length > 0 ? queues.join(', ') : EMPTY}</dd>
            {spec.labels && Object.keys(spec.labels).length > 0 && (
              <>
                <dt>labels</dt>
                <dd>
                  {Object.entries(spec.labels)
                    .map(([k, v]) => `${k}=${v}`)
                    .join(' · ')}
                </dd>
              </>
            )}
          </dl>
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>dependency graph</span>
          <span className="spacer" />
          <span className="sub">{summarizeSpecGraph(graph)}</span>
        </div>
        <SpecDagView
          graph={graph}
          selectedTaskId={selectedTaskId}
          onSelect={(id) => onSelectTask(id === selectedTaskId ? null : id)}
        />
      </div>

      {selected && (
        <div className="panel">
          <div className="panel-head">
            <span className="mono">{selected.id}</span>
            <span className="spacer" />
            <button type="button" onClick={() => onSelectTask(null)}>
              Clear selection
            </button>
          </div>
          <div className="panel-body">
            <TaskDetail task={selected.task} upstream={selected.upstream} downstream={selected.downstream} />
          </div>
        </div>
      )}

      <div className="panel">
        <div className="panel-head">
          <span>tasks</span>
          <span className="spacer" />
          <span className="chip">{graph.nodes.length}</span>
        </div>
        {graph.nodes.length === 0 ? (
          <Empty title="This definition declares no tasks" />
        ) : (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">task</th>
                  <th scope="col">task ref</th>
                  <th scope="col">trigger rule</th>
                  <th scope="col">queue</th>
                  <th scope="col">requests</th>
                  <th scope="col">retry</th>
                </tr>
              </thead>
              <tbody>
                {graph.nodes.map((n) => (
                  <tr
                    key={n.id}
                    className="clickable"
                    onClick={() => onSelectTask(n.id === selectedTaskId ? null : n.id)}
                  >
                    <td className="mono">{n.id}</td>
                    <td className="mono">{n.task.task_ref || EMPTY}</td>
                    {/* The trigger rule decides whether a task runs when a
                        parent fails, so an unset one is worth showing as unset
                        rather than as the engine's default. */}
                    <td className="mono">{n.task.trigger_rule || <span className="faint">unset</span>}</td>
                    <td className="mono">{n.task.queue || <span className="faint">unset</span>}</td>
                    <td className="mono faint">{fmtResources(n.task.resources?.request)}</td>
                    <td className="mono faint">{retrySummary(n.task)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>raw definition</span>
          <span className="spacer" />
          {/* The stored document is canonicalized and content-addressed, so the
              raw form is the only view that is exactly what the hash covers. */}
          <span className="sub">exactly the stored document</span>
        </div>
        <pre className="spec-json">{JSON.stringify(spec, null, 2)}</pre>
      </div>
    </>
  );
}

function TaskDetail({
  task,
  upstream,
  downstream,
}: {
  task: SpecTask;
  upstream: string[];
  downstream: string[];
}) {
  const inputs = Object.entries(task.inputs ?? {});
  const outputs = Object.entries(task.outputs ?? {});
  return (
    <div className="attempt-body" style={{ padding: 0 }}>
      <div>
        <div className="attempt-section-title">dispatch</div>
        <dl className="kv">
          <dt>task ref</dt>
          <dd>{task.task_ref || EMPTY}</dd>
          <dt>queue</dt>
          <dd>{task.queue || EMPTY}</dd>
          <dt>trigger rule</dt>
          <dd>{task.trigger_rule || EMPTY}</dd>
          <dt>executor</dt>
          <dd>{task.runtime?.executor || EMPTY}</dd>
          <dt>idempotency</dt>
          <dd>{task.idempotency || EMPTY}</dd>
        </dl>
      </div>
      <div>
        <div className="attempt-section-title">resources</div>
        <dl className="kv">
          <dt>request</dt>
          <dd>{fmtResources(task.resources?.request)}</dd>
          {/* Request and limit are different promises — one is what the
              scheduler reserves, the other what the runtime enforces — so they
              are never collapsed into one line. */}
          <dt>limit</dt>
          <dd>{task.resources?.limit ? fmtResources(task.resources.limit) : EMPTY}</dd>
        </dl>
        <div className="attempt-section-title" style={{ marginTop: 'var(--space-5)' }}>
          retry policy
        </div>
        <dl className="kv">
          <dt>max attempts</dt>
          <dd>{task.retry?.max_attempts ?? <span className="faint">not set</span>}</dd>
          <dt>strategy</dt>
          <dd>{task.retry?.strategy || EMPTY}</dd>
          <dt>backoff</dt>
          <dd>
            {task.retry?.initial_ms
              ? `${fmtDurationMs(task.retry.initial_ms)} → ${fmtDurationMs(task.retry.max_ms ?? task.retry.initial_ms)}`
              : EMPTY}
          </dd>
          <dt>retry on</dt>
          <dd>{task.retry?.retry_on?.join(', ') || EMPTY}</dd>
          {task.retry?.no_retry_on && (
            <>
              <dt>never retry on</dt>
              <dd>{task.retry.no_retry_on.join(', ')}</dd>
            </>
          )}
        </dl>
      </div>
      <div>
        <div className="attempt-section-title">dependencies</div>
        <dl className="kv">
          <dt>depends on</dt>
          <dd>{upstream.length > 0 ? upstream.join(', ') : <span className="faint">nothing</span>}</dd>
          <dt>feeds</dt>
          <dd>{downstream.length > 0 ? downstream.join(', ') : <span className="faint">nothing</span>}</dd>
        </dl>
        {inputs.length > 0 && (
          <>
            <div className="attempt-section-title" style={{ marginTop: 'var(--space-5)' }}>
              inputs
            </div>
            <dl className="kv">
              {inputs.map(([k, v]) => (
                <div key={k} style={{ display: 'contents' }}>
                  <dt>{k}</dt>
                  <dd>
                    {v.type ?? 'any'}
                    {v.required ? ' · required' : ' · optional'}
                  </dd>
                </div>
              ))}
            </dl>
          </>
        )}
        {outputs.length > 0 && (
          <>
            <div className="attempt-section-title" style={{ marginTop: 'var(--space-5)' }}>
              outputs
            </div>
            <dl className="kv">
              {outputs.map(([k, v]) => (
                <div key={k} style={{ display: 'contents' }}>
                  <dt>{k}</dt>
                  <dd>
                    {v.type ?? 'any'}
                    {v.required ? ' · required' : ' · optional'}
                  </dd>
                </div>
              ))}
            </dl>
          </>
        )}
      </div>
    </div>
  );
}

/**
 * Version history.
 *
 * The latest version is marked explicitly rather than left to be inferred from
 * position: "which one is live" is the question this panel exists to answer,
 * and a table sorted the other way by a future change would silently answer it
 * wrong.
 */
function VersionPanel({
  query,
  name,
  selectedId,
  onSelect,
}: {
  query: { data: SpecVersion[] | undefined; isLoading: boolean; error: unknown; refetch?: () => void };
  name: string;
  selectedId: string;
  onSelect: (id: string) => void;
}) {
  const latestId = query.data?.[0]?.id;
  const now = Date.now();

  return (
    <div className="panel">
      <div className="panel-head">
        <span>version history</span>
        <span className="spacer" />
        <span className="chip">{query.data?.length ?? 0}</span>
      </div>
      <AsyncPanel
        query={query}
        what="version history"
        isEmpty={(rows) => rows.length === 0}
        emptyTitle={`No versions of ${name}`}
        emptyDetail="The catalog listed this workflow, so an empty history means the versions endpoint disagrees with the list. Worth reporting."
        loadingRows={3}
      >
        {(rows) => (
          <div className="table-scroll">
            <table>
              <thead>
                <tr>
                  <th scope="col">version</th>
                  <th scope="col">submitted</th>
                  <th scope="col">author</th>
                  <th scope="col">hash</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((v) => (
                  <tr
                    key={v.id}
                    className={`clickable${v.id === selectedId ? ' version-current' : ''}`}
                    onClick={() => onSelect(v.id)}
                  >
                    <td>
                      {/* A real link, not just a row handler: the version an
                          operator is reading has to be shareable. */}
                      <Link
                        to={`/workflows/${encodeURIComponent(name)}?spec=${encodeURIComponent(v.id)}`}
                        className="mono"
                        onClick={(e) => e.stopPropagation()}
                      >
                        v{v.version}
                      </Link>
                      {v.id === latestId && (
                        <span className="badge tone-success" style={{ marginLeft: 'var(--space-3)' }}>
                          latest
                        </span>
                      )}
                    </td>
                    <td className="nowrap faint" title={fmtTs(v.created_at)}>
                      {fmtAge(v.created_at, now)}
                    </td>
                    <td className="mono faint truncate">{v.created_by}</td>
                    <td className="mono faint" title={v.spec_hash}>
                      {shortId(v.spec_hash, 8)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </AsyncPanel>
    </div>
  );
}

/** Compact retry description, or an honest blank when none is declared. */
function retrySummary(task: SpecTask): string {
  const r = task.retry;
  if (!r || r.max_attempts === undefined) return EMPTY;
  if (r.max_attempts <= 1) return 'no retries';
  const strategy = r.strategy ? `, ${r.strategy}` : '';
  return `${r.max_attempts} attempts${strategy}`;
}
