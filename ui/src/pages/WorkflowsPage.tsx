// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The workflow catalog.
 *
 * The question this screen answers is "what workflows exist in this namespace",
 * which is not the same question as "what has run" — a workflow authored this
 * morning and never triggered is invisible everywhere else in the console. So
 * the list is one row per NAME rather than per submitted spec: against the
 * current database that is 15 rows instead of 21, and the gap widens with every
 * resubmit of an unchanged workflow.
 *
 * Search goes to the server rather than filtering the rows already fetched. A
 * client-side filter silently becomes a lie the moment the namespace holds more
 * workflows than one page, and finding a workflow by a fragment of its name is
 * the entire reason an operator opens this page. That is now declared rather
 * than merely implemented: the table is handed `mode: 'server'`, so the control
 * it renders says the search reached the gateway instead of leaving an operator
 * to assume it did.
 *
 * THE TABLE IS <DataTable>, not a hand-rolled <table>, and the division of
 * labour is unchanged from before: AsyncPanel still decides whether there are
 * rows and still owns this page's two distinct empty states (an empty namespace
 * and a search that matched nothing are different situations and do not share a
 * message), and the table draws whatever it is given.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useWorkflows } from '../api/hooks';
import type { WorkflowSummary } from '../api/types';
import { AsyncPanel, StaleWarning } from '../components/Status';
import { PageHeader } from '../components/PageHeader';
import { DataTable } from '../components/DataTable';
import type { Column, RowCountKind } from '../components/DataTable';
import { useDebounced } from '../lib/useDebounced';
import { fmtAge, fmtTs, shortId } from '../lib/format';

/**
 * MUST MATCH api/client.ts listWorkflows' default `limit`.
 *
 * Restated here rather than exported from the client because it is not a
 * parameter this page sets — it is the shape of the answer the page RECEIVES,
 * and the table needs it to know whether a full response means "200 workflows"
 * or "at least 200". Getting 200 back when 200 was the cap is indistinguishable
 * from a namespace holding exactly 200, so the count says "200+" rather than
 * claiming a total the gateway never sent. Under the cap — which every
 * namespace this has been pointed at so far is — the count is exact and says so.
 */
const CATALOG_LIMIT = 200;
const CATALOG_COUNT: RowCountKind = { kind: 'capped', limit: CATALOG_LIMIT };

export function WorkflowsPage({ namespace }: { namespace: string }) {
  const [search, setSearch] = useState('');
  // Debounced for the same reason the cron preview is: one request per
  // keystroke would spend a round trip per character of a name being typed.
  const settled = useDebounced(search);
  const workflows = useWorkflows(namespace, settled);
  const now = Date.now();

  // "Searching" covers the gap between the last keystroke and the settled term,
  // during which the table below is answering an older question.
  const settling = settled !== search;

  const columns: Column<WorkflowSummary>[] = [
    {
      key: 'name',
      header: 'name',
      // Sortable, and the only column whose default order this page already
      // relies on: the client sorts the catalog by name before handing it over,
      // so an ascending click is a no-op and a descending one is the reversal an
      // operator scanning from the bottom of an alphabet actually wants.
      sort: (a, b) => a.name.localeCompare(b.name),
      cell: (w) => (
        <Link to={`/workflows/${encodeURIComponent(w.name)}`} className="mono">
          {w.name}
        </Link>
      ),
    },
    {
      key: 'latest',
      header: 'latest',
      cellClass: 'mono',
      sort: (a, b) => a.latest_version - b.latest_version,
      cell: (w) => <>v{w.latest_version}</>,
    },
    {
      key: 'versions',
      header: 'versions',
      cellClass: 'nowrap',
      sort: (a, b) => a.version_count - b.version_count,
      // One version is the common case and needs no comment; several means the
      // workflow has a history worth opening. The distinction is carried by
      // weight and by being a link at all, never by colour — a version count is
      // not a state.
      cell: (w) =>
        w.version_count > 1 ? (
          <Link to={`/workflows/${encodeURIComponent(w.name)}`} className="wf-versions">
            {w.version_count} versions
          </Link>
        ) : (
          <span className="faint mono">1 version</span>
        ),
    },
    {
      key: 'updated',
      header: 'updated',
      cellClass: 'nowrap faint',
      // Sorted on the raw timestamp rather than the rendered "3h ago": the cell
      // shows an age and the comparator must agree with it, which a string
      // compare of "3h ago" against "10m ago" would not.
      sort: (a, b) => Date.parse(a.updated_at) - Date.parse(b.updated_at),
      cell: (w) => <span title={fmtTs(w.updated_at)}>{fmtAge(w.updated_at, now)}</span>,
    },
    {
      key: 'first_seen',
      header: 'first seen',
      cellClass: 'nowrap faint',
      sort: (a, b) => Date.parse(a.first_created_at) - Date.parse(b.first_created_at),
      cell: (w) => (
        <span title={fmtTs(w.first_created_at)}>{fmtAge(w.first_created_at, now)}</span>
      ),
    },
    {
      key: 'author',
      header: 'author',
      cellClass: 'mono faint truncate',
      sort: (a, b) => a.created_by.localeCompare(b.created_by),
      cell: (w) => w.created_by,
    },
    {
      key: 'spec_hash',
      header: 'spec hash',
      cellClass: 'mono faint',
      // Truncated, with the whole hash on hover: it identifies the exact bytes
      // of the latest version, which is what an operator compares against a
      // run's spec, but a 64-character column would own the table. Not sortable
      // — a hash has no order anyone would want to scan in.
      cell: (w) => <span title={w.spec_hash}>{shortId(w.spec_hash, 12)}</span>,
    },
  ];

  return (
    <div className="col">
      <PageHeader
        title="Workflows"
        subtitle={
          <>
            workflow definitions in <code>{namespace}</code> · authored through the SDK or{' '}
            <code>nexusctl</code>
          </>
        }
      />

      <div className="panel">
        <div className="panel-head">
          <span>catalog</span>
        </div>

        {/* Stale rows plus a warning beat an error box that hides them. */}
        {workflows.error != null && workflows.data !== undefined ? (
          <div className="panel-notice" style={{ paddingBottom: 0 }}>
            <StaleWarning error={workflows.error} />
          </div>
        ) : null}

        <AsyncPanel
          query={workflows}
          what="workflows"
          isEmpty={(rows) => rows.length === 0}
          emptyTitle={
            settled
              ? `No workflow name contains “${settled}”`
              : 'No workflows in this namespace'
          }
          emptyDetail={
            settled ? (
              <>
                The search matches the workflow NAME, case-insensitively. Clear it to see
                everything in <code>{namespace}</code>.
              </>
            ) : (
              <>
                Nothing has been authored here yet. Workflows are submitted from the SDK or the
                CLI: <code>nexusctl spec apply -f workflow.yaml</code>
              </>
            )
          }
          loadingRows={6}
        >
          {(rows) => (
            <DataTable
              rows={rows}
              columns={columns}
              rowKey={(w) => w.name}
              noun="workflows"
              count={CATALOG_COUNT}
              /* Server mode, because the gateway matches the whole spec table.
                 `pending` is the debounce gap: while it is true the rows on
                 screen answer the PREVIOUS term, so the count says what it is
                 doing instead of showing a number that has stopped being
                 about the question in the field. */
              search={{
                mode: 'server',
                value: search,
                onChange: setSearch,
                pending: settling,
              }}
            />
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}
