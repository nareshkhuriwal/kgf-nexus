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
 */

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { authApi, type AuditEntry } from '../../api/auth';
import { shouldRetry } from '../../api/hooks';
import { AsyncPanel } from '../../components/Status';
import { fmtTs } from '../../lib/format';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';

/** Outcome -> tone, reusing the state palette so colour means the same thing here. */
function outcomeTone(outcome: string): string {
  if (outcome === 'denied') return 'tone-warning';
  if (outcome === 'error') return 'tone-danger';
  return 'tone-success';
}

export function AuditPage({ namespace, canAdmin }: { namespace: string; canAdmin: boolean }) {
  const [outcome, setOutcome] = useState('');
  const [action, setAction] = useState('');

  const audit = useQuery({
    queryKey: ['audit', namespace, outcome, action],
    queryFn: () => authApi.listAudit(namespace, { outcome, action, limit: 200 }),
    retry: shouldRetry,
    enabled: canAdmin,
    // Refreshes on its own: during an incident this screen is being watched, and
    // a stale audit view is actively misleading about what is happening now.
    refetchInterval: 10_000,
  });

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

      {/* The filters live in the events panel rather than in a panel of their
          own: they are controls ON this list, and a separate card put two
          horizontal rules between the operator and the rows they came to read.
          Filtering itself still happens in SQL — see the query key above. */}
      <div className="panel">
        <div className="panel-head">
          <span>events</span>
          <span className="spacer" />
          <span className="panel-tools">
            <label className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="sub">outcome</span>
              <select value={outcome} onChange={(e) => setOutcome(e.target.value)}>
                <option value="">any</option>
                <option value="success">success</option>
                <option value="denied">denied</option>
                <option value="error">error</option>
              </select>
            </label>
            <label className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="sub">action</span>
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
            <span className="chip">{audit.data?.length ?? 0}</span>
          </span>
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
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">when</th>
                    <th scope="col">action</th>
                    <th scope="col">outcome</th>
                    <th scope="col">actor</th>
                    <th scope="col">target</th>
                    <th scope="col">detail</th>
                  </tr>
                </thead>
                <tbody>
                  {entries.map((e) => (
                    <AuditRow key={e.id} entry={e} />
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

function AuditRow({ entry }: { entry: AuditEntry }) {
  return (
    <tr>
      <td className="nowrap faint" title={entry.occurred_at}>
        {fmtTs(entry.occurred_at)}
      </td>
      <td className="mono">{entry.action}</td>
      <td>
        <span className={`badge ${outcomeTone(entry.outcome)}`}>{entry.outcome}</span>
      </td>
      <td>
        <div className="mono audit-actor">{entry.actor_label || <span className="faint">—</span>}</div>
        {/*
          The credential KIND is shown, not just the name: an admin action taken
          from a browser session and the same action from a long-lived CI key are
          different facts during a review.
        */}
        <div className="cell-sub mono">
          {entry.actor_kind}
          {entry.client_ip ? ` · ${entry.client_ip}` : ''}
        </div>
      </td>
      <td className="mono audit-narrow">
        {entry.target_kind ? (
          <div className="cell-stack">
            <span>{entry.target_kind}</span>
            <span className="cell-sub">{entry.target_id}</span>
          </div>
        ) : (
          <span className="faint">—</span>
        )}
      </td>
      <td className="mono audit-narrow">
        <AuditDetail detail={entry.detail} />
      </td>
    </tr>
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
