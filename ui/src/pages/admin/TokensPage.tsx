// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Scoped machine credentials.
 *
 * The screen exists to answer two operator questions the platform could not
 * answer before: WHICH machine credentials exist in this namespace, and WHICH of
 * them is still being used. `api_keys` has no last_used_at and no expiry, so
 * unused keys accumulated forever with no way to tell a live integration from an
 * abandoned one. That is why last-used is a column here rather than a detail.
 *
 * The listing renders NOTHING of a token's plaintext, and that survives the move
 * to the shared table: the secret exists in this file in exactly one place, the
 * reveal panel below, and there is a test pinning that it appears in no cell and
 * no tooltip. A column set is a tempting place to add "the value" to; it must
 * not be.
 */

import { useMemo, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authApi, type ScopedToken } from '../../api/auth';
import { errorMessage, shouldRetry } from '../../api/hooks';
import { AsyncPanel, ErrorBox } from '../../components/Status';
import { DataTable } from '../../components/DataTable';
import type { Column, RowCountKind } from '../../components/DataTable';
import { fmtAge, fmtTs } from '../../lib/format';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';
import { BulkActionBar } from '../../components/BulkActionBar';

/**
 * /tokens takes no `limit`, so the listing is every credential in the namespace
 * and the count may say so. That matters more here than on the other admin
 * screens: this table is read to answer "what can still reach this namespace",
 * and a count that might be hiding a page of live credentials would make the
 * answer worthless.
 */
const TOKENS_COUNT: RowCountKind = { kind: 'exact' };

/** The actions the gateway's authorization matrix actually enforces. */
const SCOPES = [
  { value: 'read', label: 'read — list and view runs, workers, logs' },
  { value: 'write', label: 'write — submit specs, create and control runs' },
  { value: 'admin_write', label: 'admin_write — drain workers, manage identity' },
  { value: 'worker_rpc', label: 'worker_rpc — speak the worker protocol' },
];

export function TokensPage({ namespace, canAdmin }: { namespace: string; canAdmin: boolean }) {
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [issued, setIssued] = useState<ScopedToken | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  /**
   * Selection lives HERE, not in DataTable.
   *
   * DataTable has no selection support yet and is being restyled in parallel, so
   * adding it there would be a conflict on the component every list depends on.
   * The checkbox is therefore an ordinary column this page owns, and the exact
   * props DataTable should grow are written up in the handoff — at which point
   * this block collapses into `selection={{ selected, onChange, rowId }}` and the
   * column below goes away.
   *
   * Held as a Set of ids rather than of rows, so a poll that replaces the row
   * objects does not silently empty the selection.
   */
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());

  const tokens = useQuery({
    queryKey: ['tokens', namespace],
    queryFn: () => authApi.listTokens(namespace),
    retry: shouldRetry,
    enabled: canAdmin,
  });

  const revoke = useMutation({
    mutationFn: (t: ScopedToken) => authApi.revokeToken(namespace, t.id),
    onSuccess: () => {
      setActionError(null);
      void qc.invalidateQueries({ queryKey: ['tokens', namespace] });
      void qc.invalidateQueries({ queryKey: ['audit', namespace] });
    },
    onError: (e) => setActionError(errorMessage(e)),
    retry: false,
  });

  /**
   * Rebuilt each render rather than memoised: the revoke button reads
   * `revoke.isPending`, and a stale memo would leave it clickable during a
   * request already in flight.
   */
  const list = tokens.data ?? [];

  /**
   * Only LIVE credentials are selectable. A revoked token cannot be revoked
   * again, so including it would let the operator build a selection whose count
   * over-promises what the action will achieve — and then report failures for
   * rows that were never actionable. Same honesty rule the count claims follow.
   */
  const revocable = useMemo(() => list.filter((t) => t.revoked_at === null), [list]);
  const selectedTokens = useMemo(
    () => revocable.filter((t) => selected.has(t.id)),
    [revocable, selected],
  );

  /**
   * Select-all covers the rows the CLIENT ACTUALLY HAS, and /tokens is
   * uncapped, so here that genuinely is every credential in the namespace —
   * which is why this table can offer it plainly. On a capped list it would have
   * to say "all N loaded"; see the handoff for why that distinction is not
   * optional.
   */
  const allSelected = revocable.length > 0 && selected.size === revocable.length;
  const someSelected = selected.size > 0 && !allSelected;

  function toggleAll() {
    setSelected(allSelected ? new Set() : new Set(revocable.map((t) => t.id)));
  }

  function toggleOne(id: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }

  const columns: Column<ScopedToken>[] = [
    {
      key: 'select',
      /*
        Blank, and the select-all control lives in the TOOLBAR instead.

        Column.header is typed `string`, so the header cell cannot hold a
        checkbox without changing DataTable — which this task deliberately does
        not touch. The toolbar slot is a caller-owned region and takes arbitrary
        nodes, so the control goes there and keeps its indeterminate state. When
        DataTable grows the selection prop described in the handoff, the checkbox
        moves back into the header where it belongs and this column disappears.
      */
      header: '',
      cell: (t) =>
        t.revoked_at ? (
          // Not rendered at all rather than rendered disabled: a disabled
          // checkbox invites the operator to work out why, and the status column
          // beside it already says "revoked".
          <span className="faint" aria-hidden="true" />
        ) : (
          <input
            type="checkbox"
            checked={selected.has(t.id)}
            onChange={() => toggleOne(t.id)}
            aria-label={`Select ${t.name}`}
          />
        ),
    },
    {
      key: 'name',
      header: 'name',
      cellClass: 'mono',
      sort: (a, b) => a.name.localeCompare(b.name),
      cell: (t) => t.name,
    },
    {
      key: 'role',
      header: 'role',
      sort: (a, b) => a.role.localeCompare(b.role),
      // Role and scopes are capabilities, not states: neutral chips throughout,
      // so only `status` carries a tone.
      cell: (t) => <span className="chip">{t.role}</span>,
    },
    {
      key: 'scopes',
      header: 'scopes',
      // Not sortable: a set has no order, and sorting by its first element or
      // its size would be sorting by something the column does not show.
      cell: (t) =>
        t.scopes.length === 0 ? (
          <span className="faint" title="A token with no scopes can do nothing.">
            none
          </span>
        ) : (
          <div className="chip-set">
            {t.scopes.map((s) => (
              <span key={s} className="chip mono">
                {s}
              </span>
            ))}
          </div>
        ),
    },
    {
      key: 'expires',
      header: 'expires',
      cellClass: 'nowrap faint',
      /**
       * A never-expiring token sorts LAST ascending, not first.
       *
       * Null is not "expires at the beginning of time"; it is "expires never",
       * which is further in the future than any date. Infinity says that
       * explicitly — and it puts the unbounded credentials, which are the ones
       * this screen exists to replace, at the end of an ascending sort where a
       * naive null-as-zero would have buried them among the long-expired.
       */
      sort: (a, b) =>
        (a.expires_at ? Date.parse(a.expires_at) : Infinity) -
        (b.expires_at ? Date.parse(b.expires_at) : Infinity),
      cell: (t) => (
        <span title={t.expires_at ? fmtTs(t.expires_at) : ''}>
          {t.expires_at ? (
            fmtTs(t.expires_at)
          ) : (
            <span title="This credential never expires.">never</span>
          )}
        </span>
      ),
    },
    {
      key: 'last_used',
      header: 'last used',
      cellClass: 'nowrap faint',
      // The column api_keys could never answer, and the one worth sorting on:
      // ascending puts the never-used credentials first, which is the list an
      // operator cleaning up abandoned integrations came for.
      sort: (a, b) =>
        (a.last_used_at ? Date.parse(a.last_used_at) : -Infinity) -
        (b.last_used_at ? Date.parse(b.last_used_at) : -Infinity),
      cell: (t) =>
        t.last_used_at ? (
          <>{fmtAge(t.last_used_at, Date.now())}</>
        ) : (
          <span title="This token has never authenticated a request.">never</span>
        ),
    },
    {
      key: 'status',
      header: 'status',
      cell: (t) =>
        t.revoked_at ? (
          <span className="badge tone-stopped">revoked</span>
        ) : t.live ? (
          <span className="badge tone-success">live</span>
        ) : (
          <span className="badge tone-warning">expired</span>
        ),
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (t) => (
        <div className="cell-actions">
          <button
            type="button"
            className="danger"
            onClick={() => revoke.mutate(t)}
            disabled={revoke.isPending || t.revoked_at !== null}
            title="Stop this credential working immediately and permanently."
          >
            Revoke
          </button>
        </div>
      ),
    },
  ];

  if (!canAdmin) {
    return (
      <Forbidden
        what="machine credentials"
        detail="Listing tokens names every integration in this namespace and when each was last used, so it needs an admin credential."
      />
    );
  }

  return (
    <div className="col">
      <PageHeader
        title="Tokens"
        subtitle={
          <>
            scoped machine credentials in <code>{namespace}</code> · narrower and expiring, unlike
            a static API key
          </>
        }
        actions={
          <button type="button" className="primary" onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New token'}
          </button>
        }
      />

      {/*
        Shown exactly once. The secret is not recoverable from the server -- only
        its hash is stored -- so this panel is the ONLY chance to copy it, and it
        says so rather than letting the operator discover that later.
      */}
      {issued?.token && (
        <div className="panel token-reveal">
          <div className="panel-head">
            <span>copy this token now</span>
          </div>
          <div className="token-reveal-body">
            {/* Warning-toned, and role=alert rather than status: an operator who
                scrolls past this panel has permanently lost the credential, so
                it interrupts rather than waiting to be noticed. */}
            <div className="notice login-warning" role="alert">
              <span className="notice-glyph" aria-hidden="true">
                !
              </span>
              <div>
                <div className="notice-title">Shown once and never again</div>
                <div className="notice-detail">
                  Only a hash is stored, so this value cannot be recovered. If you lose it,
                  revoke this token and create another.
                </div>
              </div>
            </div>
            {/* The same single value, in the same element, revealed in the same
                one moment. Nothing about this restyle widens what is shown: the
                secret is never put in the table, in a title, or in a log. */}
            <code className="token-secret">{issued.token}</code>
            <div className="cell-actions">
              <button
                type="button"
                onClick={() => {
                  void navigator.clipboard?.writeText(issued.token ?? '');
                }}
              >
                Copy
              </button>
              <button type="button" className="primary" onClick={() => setIssued(null)}>
                I have saved it
              </button>
            </div>
          </div>
        </div>
      )}

      {actionError && <ErrorBox error={actionError} what="that action" />}

      {creating && (
        <CreateToken
          namespace={namespace}
          onDone={(t) => {
            setCreating(false);
            setIssued(t);
            void qc.invalidateQueries({ queryKey: ['tokens', namespace] });
            void qc.invalidateQueries({ queryKey: ['audit', namespace] });
          }}
        />
      )}

      <div className="panel">
        <div className="panel-head">
          <span>tokens</span>
        </div>
        <AsyncPanel
          query={tokens}
          what="tokens"
          isEmpty={(t) => t.length === 0}
          emptyTitle="No scoped tokens"
          emptyDetail="Integrations here still use static API keys, which never expire and carry their role's full authority. A scoped token is the narrower alternative."
        >
          {(rows) => (
            <>
              {/*
                REVOKE, not delete. The store has no delete for a token and must
                not: the row is the audit evidence that the credential existed
                and when it was last used, and destroying it would erase the
                trail at the moment it matters. Revocation stops the credential
                working immediately and permanently, which is the operator's
                actual intent.
              */}
              <BulkActionBar<ScopedToken>
                selected={selectedTokens}
                rowId={(t) => t.id}
                rowLabel={(t) => t.name}
                noun={['token', 'tokens']}
                actions={[
                  {
                    verb: 'Revoke',
                    destructive: true,
                    consequence:
                      'Each credential stops working immediately and cannot be restored. Anything still using one will start failing to authenticate.',
                    run: (t) => authApi.revokeToken(namespace, t.id),
                  },
                ]}
                onClear={() => setSelected(new Set())}
                onDone={() => {
                  void qc.invalidateQueries({ queryKey: ['tokens', namespace] });
                  void qc.invalidateQueries({ queryKey: ['audit', namespace] });
                }}
              />
              <DataTable
                /*
                  The select-all control, with its INDETERMINATE state. Merely
                  unchecked when some rows are selected would claim "nothing is
                  selected" while the bar above says otherwise, and the operator
                  would have to choose which control to believe.

                  It says "all live tokens" and means it: /tokens is uncapped, so
                  the client holds every credential in the namespace and the
                  claim is true. On a capped list it would have to say "all N
                  loaded" — selecting rows the client never received is the same
                  dishonesty the count claims avoid.
                */
                toolbar={
                  <label className="form-check">
                    <input
                      type="checkbox"
                      checked={allSelected}
                      ref={(el) => {
                        if (el) el.indeterminate = someSelected;
                      }}
                      onChange={toggleAll}
                      disabled={revocable.length === 0}
                      aria-label={
                        allSelected ? 'Clear selection' : 'Select all live tokens'
                      }
                    />
                    <span className="faint">
                      select all {revocable.length} live
                    </span>
                  </label>
                }
                rows={rows}
              columns={columns}
              rowKey={(t) => t.id}
              noun="tokens"
              count={TOKENS_COUNT}
              /* The name and the role are the haystack. The SCOPES are
                 deliberately not: "read" is a substring of a dozen unrelated
                 names, and a filter that quietly matched on capability would
                 make an operator believe they had seen every token carrying a
                 scope when they had only seen the ones whose name mentioned
                 it. */
                search={{ mode: 'client', fields: (t) => [t.name, t.role] }}
              />
            </>
          )}
        </AsyncPanel>
      </div>
    </div>
  );
}

function CreateToken({
  namespace,
  onDone,
}: {
  namespace: string;
  onDone: (t: ScopedToken) => void;
}) {
  const [name, setName] = useState('');
  const [role, setRole] = useState('reader');
  const [scopes, setScopes] = useState<string[]>(['read']);
  const [days, setDays] = useState(90);

  const create = useMutation({
    mutationFn: () =>
      authApi.createToken(namespace, {
        name: name.trim(),
        role,
        scopes,
        expires_in_days: days,
      }),
    onSuccess: onDone,
    retry: false,
  });

  function toggle(scope: string) {
    setScopes((prev) =>
      prev.includes(scope) ? prev.filter((s) => s !== scope) : [...prev, scope],
    );
  }

  return (
    <div className="panel">
      <div className="panel-head">
        <span>new token</span>
      </div>
      <form
        className="panel-body form-body"
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
      >
        <div className="form-row">
          <label className="form-field">
            <span className="form-label">name</span>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="ci-pipeline"
              required
            />
            <span className="form-hint">
              Names the integration, so an abandoned credential can be identified later.
            </span>
          </label>
          <label className="form-field">
            <span className="form-label">role</span>
            <select value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="reader">reader</option>
              <option value="admin">admin</option>
              <option value="worker">worker</option>
            </select>
            <span className="form-hint">The ceiling on what the scopes below can reach.</span>
          </label>
        </div>

        <fieldset className="form-fieldset">
          <legend>scopes</legend>
          <div className="form-hint">
            Scopes <strong>narrow</strong> the role and never extend it. An action the role
            does not permit stays refused even if it is ticked here.
          </div>
          {SCOPES.map((s) => (
            <label key={s.value} className="form-check">
              <input
                type="checkbox"
                checked={scopes.includes(s.value)}
                onChange={() => toggle(s.value)}
              />
              <span className="mono">{s.label}</span>
            </label>
          ))}
          {/* A verdict on what is selected, so it takes the danger tone the rest
              of the console uses for one. It does not block submit: the gateway
              is the enforcement point and its refusal is the honest one. */}
          {scopes.length === 0 && (
            <div className="form-hint invalid" role="alert">
              A token with no scopes can do nothing — the gateway will refuse to create it.
            </div>
          )}
        </fieldset>

        <div className="form-row">
          <label className="form-field">
            <span className="form-label">expires in (days)</span>
            <input
              type="number"
              value={days}
              min={-1}
              onChange={(e) => setDays(Number(e.target.value))}
            />
            {/* A never-expiring token is the thing this screen exists to replace,
                so choosing one reads as a warning. Warning and not danger: it is
                a deliberate, sometimes-correct choice, not an error. */}
            <span className={`form-hint${days < 0 ? ' caution' : ''}`}>
              {days < 0
                ? 'Never expires — an unbounded credential is what scoped tokens exist to replace. Use only for an integration that genuinely needs it.'
                : `Expires ${days} days from now.`}
            </span>
          </label>
        </div>

        {create.error && <ErrorBox error={create.error} what="the new token" />}
        <div className="form-actions">
          <button type="submit" className="primary" disabled={create.isPending}>
            {create.isPending ? 'Creating…' : 'Create token'}
          </button>
        </div>
      </form>
    </div>
  );
}
