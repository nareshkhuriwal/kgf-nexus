import { authRequest } from '../api/auth';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { usePermission } from '../components/Permissions';
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
import { DataTable } from '../components/DataTable';
import type { Column, RowCountKind } from '../components/DataTable';
import { PageHeader } from '../components/PageHeader';
import { SpecDagView } from '../components/SpecDagView';
import { SubmitRunForm } from './SubmitRunForm';
import { buildSpecGraph, specQueues, summarizeSpecGraph } from '../lib/specgraph';
import type { SpecGraph } from '../lib/specgraph';
import { EMPTY, fmtAge, fmtDurationMs, fmtResources, fmtTs, shortId } from '../lib/format';

export function WorkflowDetailPage({ namespace }: { namespace: string }) {
  const { name = '' } = useParams();
  const can = usePermission();
  const qc = useQueryClient();
  const [publishing, setPublishing] = useState(false);
  const [document, setDocument] = useState('');
  const publish = useMutation({mutationFn: async () => { const spec = JSON.parse(document); if(spec.name !== name && spec.metadata?.name !== name) throw new Error('The workflow name must match ' + name); return authRequest('/v1/namespaces/' + encodeURIComponent(namespace) + '/specs', {method:'POST', headers:{'Content-Type':'application/json'}, body:document}); }, onSuccess: () => { setPublishing(false); void qc.invalidateQueries(); }});
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
        subtitle={<>workflow definition in <code>{namespace}</code></>}
        actions={
          <>
          <Link to="/workflows" className="btn">← Back to workflows</Link>
          {can('workflows.publish') && <button disabled={!definition.data} onClick={() => { setDocument(JSON.stringify(definition.data?.spec, null, 2)); setPublishing(v => !v); }}>Publish version</button>}
          {can('schedules.create') && can('runs.create') && <Link className="btn" to={'/schedules?new=1&spec=' + encodeURIComponent(name)}>Schedule</Link>}
          {can('runs.create') && <button
            type="button"
            className="primary"
            onClick={() => setSubmitting((v) => !v)}
            // Nothing to pin a submission to until a definition has loaded.
            disabled={definition.data === undefined}
          >
            {submitting ? 'Cancel' : 'Run this workflow'}
          </button>}
          </>
        }
      />

      {publishing && <form className="panel" onSubmit={e => { e.preventDefault(); publish.mutate(); }}><div className="panel-head">Publish a workflow version</div><div className="col" style={{padding:20}}><p>Edit the definition below. Publishing validates and stores an immutable workflow version.</p><label htmlFor="publish-definition">Workflow definition (JSON)</label><textarea id="publish-definition" rows={18} value={document} onChange={e => setDocument(e.target.value)} spellCheck={false} style={{fontFamily:'monospace',width:'100%'}} />{publish.error && <ErrorBox error={publish.error} what="publish" />}<div className="detail-actions"><button type="button" onClick={() => setPublishing(false)}>Cancel</button><button className="primary" disabled={publish.isPending}>{publish.isPending ? 'Publishing…' : 'Publish version'}</button></div></div></form>}
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
          {spec.description && <p className="spec-description">{spec.description}</p>}
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
          // Identifies the exported file and keys the one-shot fit-to-view.
          exportName={`${spec.name ?? def.id}-v${def.version}`}
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
        </div>
        {graph.nodes.length === 0 ? (
          <Empty title="This definition declares no tasks" />
        ) : (
          <TaskTable
            nodes={graph.nodes}
            selectedTaskId={selectedTaskId}
            onSelectTask={onSelectTask}
          />
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
    <div className="spec-task-detail">
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
        <div className="attempt-section-title spec-section-gap">retry policy</div>
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
            <div className="attempt-section-title spec-section-gap">inputs</div>
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
            <div className="attempt-section-title spec-section-gap">outputs</div>
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
          <DataTable
            rows={rows}
            columns={versionColumns({ name, latestId, now, onSelect })}
            rowKey={(v) => v.id}
            /* The selected version is marked on the row, which is what the
               `version-current` rule has always coloured. It is a SELECTION,
               not a state, so it takes no tone from the state palette. No
               `clickable`: the link in the first cell is the control now, and a
               pointer cursor over the whole row would advertise an affordance
               the row no longer has. */
            rowClass={(v) => (v.id === selectedId ? 'version-current' : '')}
            noun="versions"
            count={VERSIONS_COUNT}
            /* A workflow's history is short — one row per resubmit of changed
               bytes — and it is read as a sequence, newest first, which paging
               would cut in the middle of. No search either: the column that
               would be searched is a version number already in view. */
            paging={{ mode: 'none' }}
          />
        )}
      </AsyncPanel>
    </div>
  );
}

/**
 * THE TASK TABLE IS EXACT, AND IT IS THE ONLY TABLE IN THIS CONSOLE THAT IS
 * EXACT FOR A REASON THAT HAS NOTHING TO DO WITH AN ENDPOINT.
 *
 * Every other count on these screens is a statement about a fetch — how many
 * rows a capped request returned, how many a page holds. This one is not a
 * fetch at all: the tasks are read out of the spec DOCUMENT already in hand,
 * and that document is immutable and content-addressed. A definition declaring
 * nine tasks declares nine tasks, permanently; there is no cap that could have
 * truncated it and no later page that could hold a tenth. "9 tasks" is as close
 * to a fact as anything in this UI gets.
 */
const TASKS_COUNT: RowCountKind = { kind: 'exact' };

/**
 * MUST MATCH api/client.ts listSpecVersions' default `limit` of 100.
 *
 * Capped rather than exact because the console sets that limit, so a full
 * response is indistinguishable from a workflow with exactly 100 versions. In
 * practice every workflow is far under it — a version exists only per resubmit
 * of changed bytes — so the count reads exactly and the cap never binds, which
 * is `countClaim`'s second half doing the work that stops a permanent "+".
 */
const VERSIONS_COUNT: RowCountKind = { kind: 'capped', limit: 100 };

/**
 * The tasks a definition declares.
 *
 * ── WHY THIS ONE GETS A DataTable AND KEEPS ALMOST NONE OF ITS CONTROLS ───
 *
 * It is an embedded list read alongside the diagram beside it, so `paging` is
 * `none` and there is no `search`: a spec's task count is bounded by what a
 * human wrote, the whole set is meant to be read at once against the graph, and
 * paging it would put a task on page 2 while its box sits on screen in the
 * diagram. What DataTable is here FOR is the other half — sortable headers with
 * real `<th scope="col">` semantics, and a count that is entitled to its claim.
 *
 * Sorting IS offered here, unlike on the fleet or the run list, and the
 * difference is that this list is COMPLETE. Sorting a complete set by queue
 * answers "what runs on gpu" truthfully; sorting a page of an unknown number of
 * runs by duration answers nothing. The default is left unset so the rows keep
 * the graph's own topological order — the order in which the tasks actually
 * run, which is the one order the diagram and the table must agree on.
 */
function TaskTable({
  nodes,
  selectedTaskId,
  onSelectTask,
}: {
  nodes: SpecGraph['nodes'];
  selectedTaskId: string | null;
  onSelectTask: (id: string | null) => void;
}) {
  type Node = SpecGraph['nodes'][number];

  const columns: Column<Node>[] = [
    {
      key: 'task',
      header: 'task',
      cellClass: 'mono',
      sort: (a, b) => a.id.localeCompare(b.id),
      /* THE SELECTION IS A REAL <button>, NOT AN onClick ON THE ROW.
         It used to be the latter, and the move to a shared table is what forced
         the question — DataTable has no row-click prop, deliberately, for the
         same reason the fleet table links from a cell rather than a row: a
         click handler on a <tr> is unreachable by keyboard, announces nothing,
         and gives no hint that the row does anything at all. A button is
         tab-reachable, activates on Enter and Space for free, and takes the
         focus ring the rest of the console uses. aria-pressed carries the
         selected state to a screen reader, which the `version-current` rule
         only ever carried visually. */
      cell: (n) => (
        <button
          type="button"
          className="link mono"
          aria-pressed={n.id === selectedTaskId}
          onClick={() => onSelectTask(n.id === selectedTaskId ? null : n.id)}
          title={
            n.id === selectedTaskId
              ? 'Clear this selection'
              : `Show ${n.id} in the graph above and open its detail`
          }
        >
          {n.id}
        </button>
      ),
    },
    {
      key: 'task_ref',
      header: 'task ref',
      cellClass: 'mono',
      sort: (a, b) => (a.task.task_ref || '').localeCompare(b.task.task_ref || ''),
      cell: (n) => <>{n.task.task_ref || EMPTY}</>,
    },
    {
      key: 'trigger_rule',
      header: 'trigger rule',
      cellClass: 'mono',
      /* The trigger rule decides whether a task runs when a parent fails, so an
         unset one is worth showing as unset rather than as the engine's
         default. */
      cell: (n) => n.task.trigger_rule || <span className="faint">unset</span>,
    },
    {
      key: 'queue',
      header: 'queue',
      cellClass: 'mono',
      /* Sortable because "which tasks land on the gpu queue" is a real question
         about a definition, and this list is complete enough to answer it. */
      sort: (a, b) => (a.task.queue || '').localeCompare(b.task.queue || ''),
      cell: (n) => n.task.queue || <span className="faint">unset</span>,
    },
    {
      key: 'requests',
      header: 'requests',
      cellClass: 'mono faint',
      cell: (n) => <>{fmtResources(n.task.resources?.request)}</>,
    },
    {
      key: 'retry',
      header: 'retry',
      cellClass: 'mono faint',
      cell: (n) => <>{retrySummary(n.task)}</>,
    },
  ];

  return (
    <DataTable
      rows={nodes}
      columns={columns}
      rowKey={(n) => n.id}
      /* No `clickable` any more: the ROW is not the control, the button in its
         first cell is, and a pointer cursor over the whole row would advertise
         an affordance that no longer exists. The selected row still takes
         `version-current`, which is a selection marker rather than a state tone
         and borrows nothing from the state palette. */
      rowClass={(n) => (n.id === selectedTaskId ? 'version-current' : '')}
      noun="tasks"
      count={TASKS_COUNT}
      paging={{ mode: 'none' }}
    />
  );
}

/**
 * The version history columns.
 *
 * A function rather than a constant because the "latest" badge and the relative
 * ages both close over values that change — `latestId` arrives with the
 * response, `now` is captured per render — and a hoisted array would freeze
 * both. Same reason the fleet rebuilds its own.
 */
function versionColumns({
  name,
  latestId,
  now,
  onSelect,
}: {
  name: string;
  latestId: string | undefined;
  now: number;
  onSelect: (id: string) => void;
}): Column<SpecVersion>[] {
  return [
    {
      key: 'version',
      header: 'version',
      sort: (a, b) => a.version - b.version,
      cell: (v) => (
        <div className="cell-actions">
          {/* A real link, not just a row handler: the version an operator is
              reading has to be shareable. */}
          {/* THE LINK IS THE SELECTOR, and it is now the ONLY selector.
              The row used to carry an onClick alongside this anchor, which made
              the same action reachable two ways and only one of them by
              keyboard. Selecting a version IS navigation — it writes ?spec= to
              the URL, because the definition an operator is reading has to be
              pasteable — so an anchor is the correct control and the row
              handler was redundant with it. onSelect is called alongside the
              navigation so the page can clear the task selection, which belongs
              to the version being left. */}
          <Link
            to={`/workflows/${encodeURIComponent(name)}?spec=${encodeURIComponent(v.id)}`}
            className="mono"
            onClick={() => onSelect(v.id)}
          >
            v{v.version}
          </Link>
          {v.id === latestId && (
            // Success-toned because "this is the one that runs" is the closest
            // thing a definition has to a live state.
            <span className="badge tone-success">latest</span>
          )}
        </div>
      ),
    },
    {
      key: 'submitted',
      header: 'submitted',
      cellClass: 'nowrap faint',
      // Sorted on the raw timestamp rather than the rendered "3h ago": the cell
      // shows an age and the comparator must agree with it.
      sort: (a, b) => Date.parse(a.created_at) - Date.parse(b.created_at),
      cell: (v) => <span title={fmtTs(v.created_at)}>{fmtAge(v.created_at, now)}</span>,
    },
    {
      key: 'author',
      header: 'author',
      cellClass: 'mono faint truncate',
      sort: (a, b) => a.created_by.localeCompare(b.created_by),
      cell: (v) => v.created_by,
    },
    {
      key: 'hash',
      header: 'hash',
      cellClass: 'mono faint',
      // Not sortable: a hash has no order anyone would want to scan in.
      cell: (v) => <span title={v.spec_hash}>{shortId(v.spec_hash, 8)}</span>,
    },
  ];
}

/** Compact retry description, or an honest blank when none is declared. */
function retrySummary(task: SpecTask): string {
  const r = task.retry;
  if (!r || r.max_attempts === undefined) return EMPTY;
  if (r.max_attempts <= 1) return 'no retries';
  const strategy = r.strategy ? `, ${r.strategy}` : '';
  return `${r.max_attempts} attempts${strategy}`;
}
