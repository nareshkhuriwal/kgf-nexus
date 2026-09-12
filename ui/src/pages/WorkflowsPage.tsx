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
 * the entire reason an operator opens this page.
 */

import { useState } from 'react';
import { Link } from 'react-router-dom';
import { useWorkflows } from '../api/hooks';
import type { WorkflowSummary } from '../api/types';
import { AsyncPanel, StaleWarning } from '../components/Status';
import { PageHeader } from '../components/PageHeader';
import { useDebounced } from '../lib/useDebounced';
import { fmtAge, fmtTs, shortId } from '../lib/format';

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
          <span className="spacer" />
          {/* The field lives in the head rather than above the table: it is a
              control ON this list, and a toolbar band of its own would put a
              second horizontal rule between the title and the rows. */}
          <span className="panel-tools">
            <label className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="sub">search</span>
              <input
                type="search"
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="name contains…"
                aria-label="search workflows by name"
              />
            </label>
            {/* While the debounce settles, the count belongs to the PREVIOUS
                question — so it says so instead of showing a stale number. */}
            <span className="chip">
              {settling ? 'searching…' : `${workflows.data?.length ?? 0}`}
            </span>
          </span>
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
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">name</th>
                    <th scope="col">latest</th>
                    <th scope="col">versions</th>
                    <th scope="col">updated</th>
                    <th scope="col">first seen</th>
                    <th scope="col">author</th>
                    <th scope="col">spec hash</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((w) => (
                    <WorkflowRow key={w.name} workflow={w} now={now} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}

function WorkflowRow({ workflow, now }: { workflow: WorkflowSummary; now: number }) {
  return (
    <tr>
      <td>
        <Link to={`/workflows/${encodeURIComponent(workflow.name)}`} className="mono">
          {workflow.name}
        </Link>
      </td>
      <td className="mono">v{workflow.latest_version}</td>
      <td className="nowrap">
        {/* One version is the common case and needs no comment; several means
            the workflow has a history worth opening. The distinction is carried
            by weight and by being a link at all, never by colour — a version
            count is not a state. */}
        {workflow.version_count > 1 ? (
          <Link to={`/workflows/${encodeURIComponent(workflow.name)}`} className="wf-versions">
            {workflow.version_count} versions
          </Link>
        ) : (
          <span className="faint mono">1 version</span>
        )}
      </td>
      <td className="nowrap faint" title={fmtTs(workflow.updated_at)}>
        {fmtAge(workflow.updated_at, now)}
      </td>
      <td className="nowrap faint" title={fmtTs(workflow.first_created_at)}>
        {fmtAge(workflow.first_created_at, now)}
      </td>
      <td className="mono faint truncate">{workflow.created_by}</td>
      {/* Truncated, with the whole hash on hover: it identifies the exact bytes
          of the latest version, which is what an operator compares against a
          run's spec, but a 64-character column would own the table. */}
      <td className="mono faint" title={workflow.spec_hash}>
        {shortId(workflow.spec_hash, 12)}
      </td>
    </tr>
  );
}
