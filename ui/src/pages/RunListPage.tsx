import { ActionIcon } from '../components/RowAction';
import { ConfirmDialog } from '../components/ConfirmDialog';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0



import { useEffect, useMemo, useState } from 'react';
import { Link, useSearchParams } from 'react-router-dom';
import { useQueryClient } from '@tanstack/react-query';
import { RUN_STATES, type NodeState, type RunSummary } from '../api/types';
import { useRuns } from '../api/hooks';
import { api } from '../api/client';
import { PageHeader } from '../components/PageHeader';
import { StateBadge } from '../components/StateBadge';
import { AsyncPanel, StaleWarning } from '../components/Status';
import { BulkActionBar } from '../components/BulkActionBar';
import { DataTable } from '../components/DataTable';
import type { Column, RowCountKind } from '../components/DataTable';
import { EMPTY, durationBetween, fmtAge, fmtDurationMs, fmtTs, shortId } from '../lib/format';
import { isTerminal, stateVisual } from '../lib/states';


const RUNS_COUNT: RowCountKind = { kind: 'unknown' };


const RUNS_PAGE_SIZE = 25;

export function RunListPage({ namespace, canAdmin = false }: { namespace: string; canAdmin?: boolean }) {
  const [controlsTarget, setControlsTarget] = useState<HTMLDivElement | null>(null);
  const [pageSize, setPageSize] = useState(RUNS_PAGE_SIZE);
  const [actionError, setActionError] = useState('');
  const qc = useQueryClient();
  const [params, setParams] = useSearchParams();
  const state = params.get('state') ?? '';
  const spec = params.get('spec') ?? '';


  const [cursors, setCursors] = useState<{ state: string; spec: string; stack: string[] }>({
    state,
    spec,
    stack: [],
  });
  if (cursors.state !== state || cursors.spec !== spec) {
    setCursors({ state, spec, stack: [] });
  }
  const stack = cursors.state === state && cursors.spec === spec ? cursors.stack : [];
  const before = stack[stack.length - 1];


  const pushCursor = (cursor: string) =>
    setCursors((c) => ({ ...c, stack: [...c.stack, cursor] }));

  const popCursor = () => setCursors((c) => ({ ...c, stack: c.stack.slice(0, -1) }));

  const query = useRuns(namespace, {
    state: state || undefined,
    spec: spec || undefined,
    limit: pageSize,
    before,
  });
  const now = Date.now();
  const filtered = Boolean(state || spec);

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value) next.set(key, value);
    else next.delete(key);
    setParams(next, { replace: true });
  };

  const nextBefore = query.data?.nextBefore;
  const rows = query.data?.runs;


  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());


  useEffect(() => setSelected(new Set()), [state, spec, before, pageSize]);
  const [deleting, setDeleting] = useState<RunSummary | null>(null);
  async function deleteOne(r: RunSummary) {
    setDeleting(null);
    setActionError('');
    try { await api.deleteRun(namespace,r.id); setCursors({state,spec,stack:[]}); await qc.invalidateQueries({queryKey:['runs',namespace]}); }
    catch(error) { setActionError(error instanceof Error ? error.message : 'Unable to delete run'); }
  }
  const cancellable = (r: RunSummary) => !isTerminal('run', r.state);
  const selectedRuns = useMemo(
    () => (rows ?? []).filter((r) => selected.has(r.id)),
    [rows, selected],
  );

  const columns: Column<RunSummary>[] = [
    {key:'actions', header:'Actions', cell:r=><div className="run-actions"><Link to={`/runs/${encodeURIComponent(r.id)}`} aria-label="View run" title="View run" className="icon-action"><ActionIcon name="view"/></Link>{canAdmin&&<button className="icon-action" aria-label="Delete run" disabled={cancellable(r)} title={cancellable(r)?'Cancel or finish this run before deleting':'Remove from listing; execution history is retained'} onClick={()=>setDeleting(r)}><ActionIcon name="delete"/></button>}</div>},
    {
      key: 'state',
      header: 'state',
      cell: (r) => <StateBadge kind="run" state={r.state} />,
    },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      cell: (r) => (
        <Link to={`/runs/${encodeURIComponent(r.id)}`} title={r.id}>
          {shortId(r.id, 12)}
        </Link>
      ),
    },
    {
      key: 'spec',
      header: 'spec',
      cell: (r) => (
        <div className="cell-stack">
          <span>{r.spec_name ?? <span className="faint">{shortId(r.spec_id, 10)}</span>}</span>
          {r.spec_version !== undefined && <span className="cell-sub">v{r.spec_version}</span>}
        </div>
      ),
    },
    {
      key: 'progress',
      header: 'progress',
      cell: (r) => <NodeCounts counts={r.node_counts} />,
    },
    {
      key: 'trigger',
      header: 'trigger',
      cellClass: 'mono faint',
      cell: (r) => <>{r.trigger_kind ?? EMPTY}</>,
    },
    {
      key: 'created',
      header: 'created',
      cellClass: 'nowrap',
      cell: (r) => <span title={fmtTs(r.created_at)}>{fmtAge(r.created_at, now)}</span>,
    },
    {
      key: 'duration',
      header: 'duration',
      cellClass: 'num',
      cell: (r) => <>{fmtDurationMs(runDuration(r, now))}</>,
    },
  ];



  return (
    <div className="col runs-list-page">
      <PageHeader
        title="Runs"
        actions={<div className="runs-title-controls" ref={setControlsTarget} />}
        subtitle={
          <>
            namespace <code>{namespace}</code> · newest first · refreshes every 4s
          </>
        }
      />

      {actionError && <p role="alert">{actionError}</p>}
      {deleting && <ConfirmDialog title={`Delete run ${shortId(deleting.id,12)}?`} confirmLabel="Delete run" tone="danger" onConfirm={() => void deleteOne(deleting)} onCancel={() => setDeleting(null)}>It disappears from this list. Execution history is retained for audit and idempotency.</ConfirmDialog>}
      <div className="panel">

        <div className="filterbar">
          <RunFilters
            state={state}
            spec={spec}
            filtered={filtered}
            onChange={setParam}
            onClear={() => setParams(new URLSearchParams(), { replace: true })}
          />
        </div>


        {query.error != null && query.data !== undefined ? (
          <div className="panel-notice">
            <StaleWarning error={query.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={{ ...query, data: rows }}
          what="runs"
          isEmpty={(runs) => runs.length === 0}
          emptyTitle={filtered ? 'No runs match these filters' : 'No runs yet'}
          emptyDetail={
            filtered ? (
              'Loosen the filters, or check that the run you expect is in this namespace.'
            ) : (
              <>
                Submit a spec and create a run: <code>nexusctl run create --spec example-etl</code>
              </>
            )
          }
          loadingRows={6}
        >
          {(runs) => (
            <>

              <BulkActionBar<RunSummary>
                selected={selectedRuns}
                rowId={(r) => r.id}
                rowLabel={(r) => shortId(r.id, 12)}
                noun={['run', 'runs']}
                actions={[
                  {
                    permission: canAdmin ? 'runs.delete' : 'runs.cancel',
                    verb: canAdmin ? 'Delete' : 'Cancel',
                    destructive: true,
                    consequence: canAdmin ? 'Remove selected completed runs from the list. Execution records remain available for audit and idempotency. Active runs must be cancelled first.' : 'Stop selected active runs. This cannot be undone.',
                    run: (r) => canAdmin ? api.deleteRun(namespace, r.id) : api.cancelRun(namespace, r.id),
                  },
                ]}
                onClear={() => setSelected(new Set())}
                onDone={() => {
                  setSelected(new Set()); setCursors({state,spec,stack:[]});

                  void qc.invalidateQueries({ queryKey: ['runs', namespace] });
                  void qc.invalidateQueries({ queryKey: ['attention', namespace] });
                }}
              />
              <DataTable
              controlsTarget={controlsTarget}
              rows={runs}
              columns={columns}
              rowKey={(r) => r.id}
              rowClass={rowTone}
              noun="runs"
              tableClass="table-runs"
              count={RUNS_COUNT}

              selection={{
                selected,
                onChange: setSelected,
                rowId: (r) => r.id,
                isSelectable: canAdmin ? (r) => !cancellable(r) : cancellable,
                label: (r) => `run ${shortId(r.id, 12)}`,

                emptyHint: canAdmin ? 'Only completed runs can be deleted.' : 'These runs have already finished — only a run still in flight can be cancelled.',
              }}

              paging={{
                mode: 'server',
                totalRows: query.data?.total,
                pageSize, onPageSize: n => { setPageSize(n); setCursors({state,spec,stack:[]}); setSelected(new Set()); },
                onFirst: () => setCursors({state,spec,stack:[]}),
                pageNumber: stack.length + 1,

                onNext: nextBefore && !query.isPlaceholderData ? () => pushCursor(nextBefore) : undefined,

                onPrev: stack.length > 0 && !query.isPlaceholderData ? popCursor : undefined,
              }}
              />
            </>
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}


function RunFilters({
  state,
  spec,
  filtered,
  onChange,
  onClear,
}: {
  state: string;
  spec: string;
  filtered: boolean;
  onChange: (key: string, value: string) => void;
  onClear: () => void;
}) {
  return (
    <>

      <label className="filter-field">
        <span className="filter-label">state</span>
        <select
          value={state}
          onChange={(e) => onChange('state', e.target.value)}
          aria-label="filter runs by state"
        >
          <option value="">any</option>
          {RUN_STATES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
      </label>
      <label className="filter-field">
        <span className="filter-label">spec</span>
        <input
          type="search"
          placeholder="workflow name"
          value={spec}
          onChange={(e) => onChange('spec', e.target.value)}
          aria-label="filter runs by workflow name"
        />
      </label>
      {filtered && (
        <button type="button" onClick={onClear}>
          clear filters
        </button>
      )}
    </>
  );
}


function runDuration(run: RunSummary, now: number): number | null {
  if (isTerminal('run', run.state)) {
    return durationBetween(run.started_at ?? run.created_at, run.ended_at);
  }
  return run.started_at ? now - Date.parse(run.started_at) : null;
}


function rowTone(run: RunSummary): string {
  const tone = stateVisual('run', run.state).tone;
  if (tone === 'danger') return 'row-failed';
  // A run that succeeded overall while a node inside it failed: the row's own
  // badge says SUCCEEDED, so this edge is the only thing that flags it.
  if ((run.node_counts?.FAILED ?? 0) > 0) return 'row-failed';
  // Stopped and ambiguous outcomes did not do what was asked, but they are not
  // the failure to look at first, so they take the blocked tone rather than
  // competing with it.
  if (tone === 'stopped' || tone === 'warning') return 'row-blocked';
  return '';
}


function NodeCounts({ counts }: { counts: Partial<Record<NodeState, number>> | undefined }) {
  if (!counts) return <span className="faint">{EMPTY}</span>;
  const entries = (Object.entries(counts) as [NodeState, number][]).filter(([, n]) => n > 0);
  if (entries.length === 0) return <span className="faint">{EMPTY}</span>;
  // Failure-bearing states first: they are why anyone is looking at this row.
  const rank: Record<string, number> = {
    FAILED: 0,
    UPSTREAM_FAILED: 1,
    RUNNING: 2,
    READY: 3,
    PENDING: 4,
    CANCELLED: 5,
    SKIPPED: 6,
    SUCCEEDED: 7,
  };
  entries.sort((a, b) => (rank[a[0]] ?? 9) - (rank[b[0]] ?? 9));
  return (
    <span className="node-tally">
      {entries.map(([s, n]) => (
        <span key={s} className="node-tally-item">
          <StateBadge kind="node" state={s} small showGlyph={false} />
          <span className="node-tally-n">{n}</span>
        </span>
      ))}
    </span>
  );
}
