// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Assets: the datasets tasks declare they produce and consume, when each was
 * last materialized, and the lineage between them.
 *
 * One route, two views, addressed by the URL: `/assets?q=` is the searchable
 * list and `/assets?uri=<uri>` is one asset. A URI is an arbitrary string with
 * slashes and colons in it (`s3://lake/orders/`), which is why it rides in the
 * query string rather than a path segment — the gateway addresses it the same
 * way (`/assets/detail?uri=`).
 *
 * Search is SERVER-side: `q` goes to the gateway, which matches the whole
 * namespace, so the table is told `mode: 'server'` and filters nothing itself.
 */

import { useState } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { ApiError } from '../api/client';
import { useAsset, useAssetGraph, useAssets } from '../api/hooks';
import type { Asset, AssetMaterialization, AssetTask } from '../api/types';
import { AssetLineageGraph } from '../components/AssetLineageGraph';
import { DataTable, type Column } from '../components/DataTable';
import { Forbidden } from '../components/Forbidden';
import { PageHeader } from '../components/PageHeader';
import { usePermission } from '../components/Permissions';
import { Empty, Loading, StaleWarning } from '../components/Status';
import { StateBadge } from '../components/StateBadge';
import { EventsError, EventsPanel } from '../components/EventsShared';
import { EMPTY, fmtAge, fmtTs, shortId } from '../lib/format';
import { useDebounced } from '../lib/useDebounced';

const LIST_LIMIT = 200;
const DEPTHS = [1, 2, 3, 4, 5, 6];

export function AssetsPage({ namespace }: { namespace: string }) {
  const can = usePermission();
  const [params] = useSearchParams();
  const uri = params.get('uri') ?? '';

  if (!can('assets.view')) {
    return (
      <div className="col">
        <PageHeader title="Assets" />
        <Forbidden what="view assets" detail="This page needs the assets.view permission. Ask your administrator to add it to one of your roles." />
      </div>
    );
  }
  return uri ? <AssetDetailView key={`${namespace}:${uri}`} namespace={namespace} uri={uri} /> : <AssetList key={namespace} namespace={namespace} />;
}

function assetHref(uri: string): string {
  return `/assets?${new URLSearchParams({ uri }).toString()}`;
}

function AssetList({ namespace }: { namespace: string }) {
  const [params, setParams] = useSearchParams();
  const term = params.get('q') ?? '';
  const [draft, setDraft] = useState(term);
  const settled = useDebounced(draft.trim(), 300);
  const assets = useAssets(namespace, settled);
  const [tableControls, setTableControls] = useState<HTMLDivElement | null>(null);
  const now = Date.now();

  function onSearch(value: string) {
    setDraft(value);
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value.trim()) next.set('q', value.trim());
        else next.delete('q');
        return next;
      },
      { replace: true },
    );
  }

  const columns: Column<Asset>[] = [
    {
      key: 'name',
      header: 'asset',
      sort: (a, b) => (a.name || a.uri).localeCompare(b.name || b.uri),
      cell: (a) => (
        <span className="cell-stack">
          <Link to={assetHref(a.uri)}>{a.name || a.uri}</Link>
          {a.name && a.name !== a.uri && <span className="cell-sub mono">{a.uri}</span>}
        </span>
      ),
    },
    { key: 'producers', header: 'producers', align: 'end', sort: (a, b) => a.producers - b.producers, cell: (a) => a.producers },
    { key: 'consumers', header: 'consumers', align: 'end', sort: (a, b) => a.consumers - b.consumers, cell: (a) => a.consumers },
    {
      key: 'materialized',
      header: 'last materialized',
      cellClass: 'nowrap',
      sort: (a, b) => Date.parse(a.last_materialized_at ?? '') - Date.parse(b.last_materialized_at ?? ''),
      cell: (a) =>
        a.last_materialized_at ? (
          <span className="cell-stack">
            <span title={fmtTs(a.last_materialized_at)}>{fmtAge(a.last_materialized_at, now)}</span>
            {a.last_materialized_run_id && (
              <Link className="cell-sub mono" to={`/runs/${encodeURIComponent(a.last_materialized_run_id)}`}>
                run {shortId(a.last_materialized_run_id)}
              </Link>
            )}
          </span>
        ) : (
          <span className="faint" title="Declared by a task, but no producing task has succeeded since lineage was recorded.">
            never
          </span>
        ),
    },
    {
      key: 'declared',
      header: 'last declared',
      cellClass: 'nowrap faint',
      cell: (a) => <span title={fmtTs(a.last_declared_at)}>{fmtAge(a.last_declared_at, now)}</span>,
    },
  ];

  return (
    <div className="col">
      <PageHeader
        title="Assets"
        subtitle={
          <>
            datasets declared by tasks in <code>{namespace}</code>, and their lineage
          </>
        }
      />
      <div className="panel">
        <div className="panel-head">
          <span>Assets</span>
          <div className="panel-head-controls" ref={setTableControls} />
        </div>
        <EventsPanel
          query={assets}
          what="assets"
          feature="Asset lineage features"
          permission="assets.view"
          isEmpty={(rows) => rows.length === 0 && settled === '' && draft.trim() === ''}
          empty={
            <Empty
              title="No assets recorded yet"
              detail={
                <>
                  Assets appear when a task declares them, e.g. <code>@task(produces=[&quot;s3://lake/orders/&quot;])</code> in the
                  SDK, and a run of that workflow starts.
                </>
              }
            />
          }
        >
          {(rows) => (
            <DataTable
              controlsTarget={tableControls}
              rows={rows}
              columns={columns}
              rowKey={(a) => a.uri}
              noun="assets"
              count={{ kind: 'capped', limit: LIST_LIMIT }}
              search={{ mode: 'server', value: draft, onChange: onSearch, pending: draft.trim() !== settled || assets.isFetching }}
            />
          )}
        </EventsPanel>
      </div>
    </div>
  );
}

function AssetDetailView({ namespace, uri }: { namespace: string; uri: string }) {
  const navigate = useNavigate();
  const [depth, setDepth] = useState(2);
  const asset = useAsset(namespace, uri);
  const graph = useAssetGraph(namespace, uri, depth, asset.data !== undefined);
  const now = Date.now();

  const back = (
    <div className="detail-actions detail-back-only">
      <Link to="/assets" className="btn">
        ← All assets
      </Link>
    </div>
  );

  if (asset.error && asset.data === undefined) {
    if (asset.error instanceof ApiError && asset.error.status === 404) {
      return (
        <div className="col">
          {back}
          <Empty title="Unknown asset" detail={<>No task in <code>{namespace}</code> has declared <code>{uri}</code>.</>} />
        </div>
      );
    }
    return (
      <div className="col">
        {back}
        <EventsError error={asset.error} what={`asset ${uri}`} feature="Asset lineage features" permission="assets.view" onRetry={() => void asset.refetch()} />
      </div>
    );
  }
  if (!asset.data) {
    return (
      <div className="col">
        {back}
        <Loading rows={8} label="Loading asset" />
      </div>
    );
  }
  const a = asset.data;
  const latest = a.latest_materialization;

  const taskColumns: Column<AssetTask>[] = [
    {
      key: 'workflow',
      header: 'workflow',
      cellClass: 'mono',
      sort: (x, y) => x.workflow.localeCompare(y.workflow),
      cell: (t) => <Link to={`/workflows/${encodeURIComponent(t.workflow)}`}>{t.workflow}</Link>,
    },
    { key: 'task', header: 'task', cellClass: 'mono', cell: (t) => t.node_id },
    { key: 'runs', header: 'runs', align: 'end', cell: (t) => t.runs },
    {
      key: 'last',
      header: 'last run',
      cellClass: 'mono nowrap',
      cell: (t) =>
        t.last_run_id ? (
          <span className="cell-stack">
            <Link to={`/runs/${encodeURIComponent(t.last_run_id)}`}>{shortId(t.last_run_id)}</Link>
            {t.last_recorded_at && <span className="cell-sub">{fmtAge(t.last_recorded_at, now)}</span>}
          </span>
        ) : (
          <span className="faint">{EMPTY}</span>
        ),
    },
  ];

  const matColumns: Column<AssetMaterialization>[] = [
    {
      key: 'at',
      header: 'materialized',
      cellClass: 'nowrap',
      cell: (m) => <span title={fmtTs(m.materialized_at)}>{fmtAge(m.materialized_at, now)}</span>,
    },
    {
      key: 'run',
      header: 'run',
      cellClass: 'mono',
      cell: (m) => <Link to={`/runs/${encodeURIComponent(m.run_id)}`}>{shortId(m.run_id)}</Link>,
    },
    { key: 'state', header: 'run state', cell: (m) => (m.run_state ? <StateBadge kind="run" state={m.run_state} small /> : <span className="faint">{EMPTY}</span>) },
    {
      key: 'by',
      header: 'produced by',
      cellClass: 'mono',
      cell: (m) => (
        <>
          <Link to={`/workflows/${encodeURIComponent(m.workflow)}`}>{m.workflow}</Link> · {m.node_id}
        </>
      ),
    },
  ];

  return (
    <div className="col">
      <div className="detail-head">
        <div className="detail-head-text">
          <div className="detail-title-row">
            <span className="badge tone-queued">asset</span>
            <h1 className="detail-title">{a.name || a.uri}</h1>
          </div>
          <div className="detail-meta">
            <code>{a.uri}</code> · first seen {fmtTs(a.first_seen_at)} · last declared {fmtTs(a.last_declared_at)}
          </div>
        </div>
        <div className="detail-actions">
          <Link to="/assets" className="btn">
            ← All assets
          </Link>
        </div>
      </div>

      {asset.error != null && <StaleWarning error={asset.error} />}

      <div className="grid-2">
        <div className="panel">
          <div className="panel-head">
            <span>latest materialization</span>
          </div>
          <div className="panel-body">
            {latest ? (
              <dl className="kv">
                <dt>when</dt>
                <dd>
                  {fmtTs(latest.materialized_at)} ({fmtAge(latest.materialized_at, now)})
                </dd>
                <dt>run</dt>
                <dd>
                  <Link to={`/runs/${encodeURIComponent(latest.run_id)}`}>{latest.run_id}</Link>
                </dd>
                <dt>run state</dt>
                <dd>{latest.run_state ? <StateBadge kind="run" state={latest.run_state} /> : EMPTY}</dd>
                <dt>produced by</dt>
                <dd>
                  {latest.workflow} · {latest.node_id}
                </dd>
              </dl>
            ) : (
              <Empty
                title="Never materialized"
                detail="Tasks declare this asset, but no producing task has succeeded since lineage started being recorded."
              />
            )}
          </div>
        </div>
        <div className="panel">
          <div className="panel-head">
            <span>recent materializations</span>
            <span className="spacer" />
            <span className="chip">{a.materializations.length}</span>
          </div>
          {a.materializations.length === 0 ? (
            <Empty title="No materializations" />
          ) : (
            <DataTable rows={a.materializations} columns={matColumns} rowKey={(m) => `${m.run_id}:${m.node_id}`} noun="materializations" count={{ kind: 'unknown' }} defaultPageSize={25} />
          )}
        </div>
      </div>

      <div className="grid-2">
        <div className="panel">
          <div className="panel-head">
            <span>producers</span>
            <span className="spacer" />
            <span className="chip">{a.producer_tasks.length}</span>
          </div>
          {a.producer_tasks.length === 0 ? (
            <Empty title="No producers" detail="No task declares that it writes this asset; it is a source." />
          ) : (
            <DataTable rows={a.producer_tasks} columns={taskColumns} rowKey={(t) => `${t.workflow}/${t.node_id}`} noun="producers" count={{ kind: 'exact' }} />
          )}
        </div>
        <div className="panel">
          <div className="panel-head">
            <span>consumers</span>
            <span className="spacer" />
            <span className="chip">{a.consumer_tasks.length}</span>
          </div>
          {a.consumer_tasks.length === 0 ? (
            <Empty title="No consumers" detail="No task declares that it reads this asset." />
          ) : (
            <DataTable rows={a.consumer_tasks} columns={taskColumns} rowKey={(t) => `${t.workflow}/${t.node_id}`} noun="consumers" count={{ kind: 'exact' }} />
          )}
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>lineage</span>
          <span className="spacer" />
          <label className="row" style={{ gap: 'var(--space-3)' }}>
            <span className="faint">depth</span>
            <select value={depth} onChange={(e) => setDepth(Number(e.target.value))} aria-label="Lineage depth">
              {DEPTHS.map((d) => (
                <option key={d} value={d}>
                  {d} hop{d === 1 ? '' : 's'}
                </option>
              ))}
            </select>
          </label>
        </div>
        {graph.error && graph.data === undefined ? (
          graph.error instanceof ApiError && graph.error.status === 404 ? (
            <Empty title="No lineage recorded" detail="No task edges touch this asset yet." />
          ) : (
            <EventsError error={graph.error} what="asset lineage" feature="Asset lineage features" permission="assets.view" onRetry={() => void graph.refetch()} />
          )
        ) : !graph.data ? (
          <Loading rows={4} label="Loading lineage" />
        ) : (
          <>
            {graph.error != null && <StaleWarning error={graph.error} />}
            <AssetLineageGraph
              graph={graph.data}
              focusId={`asset:${uri}`}
              exportName={`lineage-${a.name || 'asset'}`}
              onSelectAsset={(next) => {
                if (next !== uri) navigate(assetHref(next));
              }}
              onSelectTask={(node) => node.workflow && navigate(`/workflows/${encodeURIComponent(node.workflow)}`)}
            />
          </>
        )}
      </div>
    </div>
  );
}
