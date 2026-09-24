import { RowAction, RowDetails } from '../../components/RowAction';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * The administrative audit trail.
 *
 * run_events records what happened to a RUN. Nothing recorded what happened to
 * the SYSTEM — who created an account, who minted a credential, who failed to
 * log in five times in a row. Those are exactly the events an incident review
 * needs and the ones no run's history contains.
 *
 * DENIED outcomes are given the same weight as successes, and are filterable in
 * one click, because a burst of them is what an attack in progress looks like.
 *
 * ── TWO FILTERS THAT ARE NOT THE SAME KIND OF THING ──────────────────────
 *
 * The outcome and action controls go to the SERVER — they are part of the query
 * key, so the gateway filters in SQL over the whole trail. The table's own
 * search box filters the FETCHED PAGE only. Those are different reaches and
 * would be indistinguishable if both were rendered as plain boxes in one bar,
 * so the page's filters stay the page's — a .filterbar above the table, with
 * their wording and the one-click "Show denials" shortcut unchanged — and the
 * table's own box is labelled as local. An operator who filters to zero rows
 * needs to know whether the trail has no such events or whether this page of it
 * does not.
 *
 * That distinction is not cosmetic here. The request is capped at 200 entries,
 * and the audit trail is the one list in this console where a missed row is a
 * missed security finding — so the count says "200+" the moment the cap binds
 * rather than presenting a page as the history.
 */

import { useMemo, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { authApi, type AuditEntry } from '../../api/auth';
import { shouldRetry } from '../../api/hooks';
import { AsyncPanel } from '../../components/Status';
import { BulkActionBar } from '../../components/BulkActionBar';
import { DataTable } from '../../components/DataTable';
import type { Column, RowCountKind } from '../../components/DataTable';
import { fmtTs } from '../../lib/format';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';

/**
 * The row cap, stated once and used twice: it is sent as the request's `limit`
 * AND handed to the table so the count can disclose it. Two copies of this
 * number is how the request and the label drift apart, and a label claiming a
 * 200-row total against a 500-row request would be the exact failure this whole
 * exercise is about.
 */
const AUDIT_LIMIT = 200;
const AUDIT_COUNT: RowCountKind = { kind: 'capped', limit: AUDIT_LIMIT };

/**
 * One audit entry as a row of CSV fields, in the column order the table shows.
 *
 * `detail` is serialised as JSON rather than flattened into columns: its shape
 * differs per action, so any flattening would either invent a union of every
 * key or silently drop the ones it did not anticipate — and this file is
 * evidence, so dropping a field is the one thing it must not do.
 */
function auditRow(e: AuditEntry): string[] {
  return [
    e.occurred_at,
    e.action,
    e.outcome,
    e.actor_label,
    e.actor_kind,
    e.client_ip,
    e.target_kind,
    e.target_id,
    e.detail == null ? '' : JSON.stringify(e.detail),
  ];
}

const AUDIT_CSV_HEADER = [
  'occurred_at',
  'action',
  'outcome',
  'actor_label',
  'actor_kind',
  'client_ip',
  'target_kind',
  'target_id',
  'detail',
];

/**
 * Quotes one CSV field.
 *
 * Everything is quoted rather than only the fields that need it. An audit
 * export carries operator-supplied target ids and JSON detail, so the cases
 * that need quoting are the common ones here, and a rule with no exceptions
 * cannot be applied inconsistently.
 *
 * The leading apostrophe on a field starting with = + - @ is deliberate: a
 * spreadsheet reads those as formulas, and an audit trail is precisely where an
 * attacker-controlled string like `=HYPERLINK(...)` must not become live when a
 * reviewer opens the file. It changes the displayed value, which is the correct
 * trade against executing it.
 */
function csvField(v: string): string {
  const injectable = /^[=+\-@\t\r]/.test(v) ? `'${v}` : v;
  return `"${injectable.replace(/"/g, '""')}"`;
}

function toCsv(rows: string[][]): string {
  // CRLF, which is what RFC 4180 specifies and what Excel expects.
  return rows.map((r) => r.map(csvField).join(',')).join('\r\n');
}

/**
 * Hands the browser a file. Mirrors DagCanvas's exporter, including the
 * deferred revoke: some browsers have not read the blob when click() returns
 * and revoking synchronously produces a zero-byte file.
 */
function downloadCsv(csv: string, filename: string) {
  // The BOM makes Excel read the file as UTF-8. Without it, an actor label with
  // a non-ASCII character is mojibake in the one tool most reviewers open.
  const blob = new Blob([`\ufeff${csv}`], { type: 'text/csv;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
  window.setTimeout(() => URL.revokeObjectURL(url), 0);
}

/** Outcome -> tone, reusing the state palette so colour means the same thing here. */
function outcomeTone(outcome: string): string {
  if (outcome === 'denied') return 'tone-warning';
  if (outcome === 'error') return 'tone-danger';
  return 'tone-success';
}

export function AuditPage({ namespace, canAdmin }: { namespace: string; canAdmin: boolean }) {
  const [tableControls,setTableControls] = useState<HTMLDivElement|null>(null);
  const [outcome, setOutcome] = useState('');
  const [action, setAction] = useState('');

  /**
   * The selected entry ids.
   *
   * Audit rows are immutable and never change under the operator — unlike the
   * run list, a selected entry cannot become ineligible — so there is no
   * eligibility predicate here and every row is selectable.
   */
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());

  const audit = useQuery({
    queryKey: ['audit', namespace, outcome, action],
    queryFn: () => authApi.listAudit(namespace, { outcome, action, limit: AUDIT_LIMIT }),
    retry: shouldRetry,
    enabled: canAdmin,
    // Refreshes on its own: during an incident this screen is being watched, and
    // a stale audit view is actively misleading about what is happening now.
    refetchInterval: 10_000,
  });

  /**
   * The selected rows, resolved against the latest fetch.
   *
   * Derived from the ids rather than held as rows: this list refetches every
   * 10s, and holding row objects would export a stale copy of an entry whose
   * server-side representation had since been re-read.
   */
  const selectedEntries = useMemo(
    () => (audit.data ?? []).filter((e) => selected.has(String(e.id))),
    [audit.data, selected],
  );

  /**
   * Newest first is the server's order and the page's subtitle promises it, so
   * `when` is the only column given a comparator that could REPLACE that order —
   * and it is offered because the one thing a reviewer does with a trail is walk
   * it backwards from an incident, then forwards from the first suspicious
   * entry. Every other column is left unsortable: sorting an append-only history
   * by actor or outcome breaks the chronology that makes it readable as a
   * sequence of events, which is the whole value of a trail over a table.
   */
  const columns: Column<AuditEntry>[] = [
    {
      key: 'when',
      header: 'when',
      cellClass: 'nowrap faint',
      sort: (a, b) => Date.parse(a.occurred_at) - Date.parse(b.occurred_at),
      cell: (e) => <span title={e.occurred_at}>{fmtTs(e.occurred_at)}</span>,
    },
    {
      key: 'action',
      header: 'action',
      cellClass: 'mono',
      cell: (e) => e.action,
    },
    {
      key: 'outcome',
      header: 'outcome',
      cell: (e) => <span className={`badge ${outcomeTone(e.outcome)}`}>{e.outcome}</span>,
    },
    {
      key: 'actor',
      header: 'actor',
      cell: (e) => (
        <>
          <div className="mono audit-actor">
            {e.actor_label || <span className="faint">—</span>}
          </div>
          {/*
            The credential KIND is shown, not just the name: an admin action
            taken from a browser session and the same action from a long-lived CI
            key are different facts during a review.
          */}
          <div className="cell-sub mono">
            {e.actor_kind}
            {e.client_ip ? ` · ${e.client_ip}` : ''}
          </div>
        </>
      ),
    },
    {
      key: 'target',
      header: 'target',
      cellClass: 'mono audit-narrow',
      cell: (e) =>
        e.target_kind ? (
          <div className="cell-stack">
            <span>{e.target_kind}</span>
            <span className="cell-sub">{e.target_id}</span>
          </div>
        ) : (
          <span className="faint">—</span>
        ),
    },
    {
      key: 'detail',
      header: 'detail',
      cellClass: 'mono audit-narrow',
      cell: (e) => <AuditDetail detail={e.detail} />,
    },
    {key:'actions',header:'Actions',cell:e=><div className="cell-actions"><RowDetails label={`View audit entry ${e.id}`} value={e}/><RowAction label={`Export audit entry ${e.id}`} icon="export" onAction={()=>downloadCsv(toCsv([AUDIT_CSV_HEADER,auditRow(e)]),`audit-${e.id}.csv`)}/></div>},
  ];

  if (!canAdmin) {
    return (
      <Forbidden
        what="the audit trail"
        detail="The audit log names accounts and the actions taken against them, so reading it needs an admin credential."
      />
    );
  }

  return (
    <div className="col">
      <PageHeader
        title="Audit"
        subtitle={
          <>
            administrative history for <code>{namespace}</code> · append-only, newest first
          </>
        }
      />

      <div className="panel">
        <div className="panel-head">
          <span>Events</span><div className="panel-head-controls" ref={setTableControls} />
        </div>
        {/* The SERVER-side filters, and they live OUTSIDE <AsyncPanel> on
            purpose — the one structural change this refactor makes to the page.
            Inside it they were rendered only alongside rows, so filtering down
            to zero events took the Clear button off the screen with them and
            left the operator in a filtered empty state with no control to leave
            it by, short of a reload. An empty result is exactly when a filter
            control is most needed.

            They stay a .filterbar rather than becoming the table's `toolbar`
            for the same reason, and the band is the sibling of .panel-head that
            the run list already uses: .panel-head is uppercase and
            letter-spaced, the wrong container for controls people read and
            click. Filtering itself still happens in SQL — see the query key. */}
        <div className="filterbar">
          <label className="filter-field">
            <span className="filter-label">outcome</span>
            <select value={outcome} onChange={(e) => setOutcome(e.target.value)}>
              <option value="">any</option>
              <option value="success">success</option>
              <option value="denied">denied</option>
              <option value="error">error</option>
            </select>
          </label>
          <label className="filter-field">
            <span className="filter-label">action</span>
            <input
              value={action}
              onChange={(e) => setAction(e.target.value)}
              placeholder="e.g. session.login"
              className="mono"
            />
          </label>
          {(outcome || action) && (
            <button
              type="button"
              onClick={() => {
                setOutcome('');
                setAction('');
              }}
            >
              Clear
            </button>
          )}
          {/* A one-click shortcut to the query that matters: a burst of
              refusals is what an attack in progress looks like. */}
          <button
            type="button"
            onClick={() => setOutcome('denied')}
            title="Show refused attempts — a burst of these is what an attack looks like."
          >
            Show denials
          </button>
        </div>

        <AsyncPanel
          query={audit}
          what="audit history"
          isEmpty={(e) => e.length === 0}
          emptyTitle={outcome || action ? 'No events match these filters' : 'No administrative events yet'}
          emptyDetail={
            outcome || action
              ? 'Clear the filters to see the full history.'
              : 'Signing in, creating a user or minting a token will appear here.'
          }
        >
          {(entries) => (
            <>
              {/*
                EXPORT, and nothing else. The audit trail is append-only by
                design — there is no delete or edit endpoint for an entry and
                there must not be, because a log an admin can prune is not
                evidence. So the one bulk operation that exists here is taking
                rows OUT: a reviewer assembling an incident timeline selects the
                relevant entries and gets a file to attach to the report.

                Not destructive, so no confirmation: it writes a file and
                changes nothing on the server.

                It is a local operation rather than N requests, so `run` is
                called per row only to satisfy the bar's contract — the file is
                built once, below, from the selection.
              */}
              <BulkActionBar<AuditEntry>
                selected={selectedEntries}
                rowId={(e) => String(e.id)}
                rowLabel={(e) => `${e.action} · ${fmtTs(e.occurred_at)}`}
                noun={['entry', 'entries']}
                actions={[
                  {
                    verb: 'Export',
                    run: async () => {
                      /* No-op per row. The download happens once in onDone:
                         firing it per row would hand the operator N files. */
                    },
                  },
                ]}
                onClear={() => setSelected(new Set())}
                onDone={(outcome_) => {
                  if (outcome_.failed > 0 || selectedEntries.length === 0) return;
                  const csv = toCsv([
                    AUDIT_CSV_HEADER,
                    ...selectedEntries.map(auditRow),
                  ]);
                  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
                  downloadCsv(csv, `nexus-audit-${namespace}-${stamp}.csv`);
                }}
              />
              <DataTable
              controlsTarget={tableControls}
              rows={entries}
              columns={columns}
              rowKey={(e) => String(e.id)}
              noun="events"
              count={AUDIT_COUNT}
              /* 25, the same as every other table in the console. The trail
                 used to open at 50 on the argument that a review scrolls, but a
                 page size that differs per table makes "the second page" mean a
                 different amount of history on each one, and an operator
                 comparing an audit page against a run page was silently
                 comparing different-sized windows. One size everywhere; the
                 rows selector is still there for a reviewer who wants more. */
              /* Local, over the actor and target the SQL filters do not cover.
                 It reaches only the fetched page, which is why it is declared
                 client-side and labelled as such: on a capped 200-row trail an
                 unqualified "2 results" would read as a claim about the history
                 and be wrong in the direction that matters. */
              search={{
                mode: 'client',
                fields: (e) => [e.actor_label, e.actor_kind, e.target_id, e.client_ip],
              }}
              noMatchTitle="No event on this page matches that text"
              noMatchDetail="This box filters only the entries already fetched. To search the whole trail, use the outcome and action filters above — those run in SQL."
              /* Every row is selectable: an audit entry is immutable, so there
                 is no state in which one is not a valid thing to export. */
              selection={{
                selected,
                onChange: setSelected,
                rowId: (e) => String(e.id),
                label: (e) => `${e.action} at ${fmtTs(e.occurred_at)}`,
              }}
            />
            </>
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}

/** Renders the structured detail object. It never contains secret material. */
function AuditDetail({ detail }: { detail: unknown }) {
  if (!detail || typeof detail !== 'object') return <span className="faint">—</span>;
  const entries = Object.entries(detail as Record<string, unknown>);
  if (entries.length === 0) return <span className="faint">—</span>;
  return (
    <>
      {entries.map(([k, v]) => (
        <div key={k} className="audit-detail-row">
          <span className="faint">{k}=</span>
          {typeof v === 'object' ? JSON.stringify(v) : String(v)}
        </div>
      ))}
    </>
  );
}
