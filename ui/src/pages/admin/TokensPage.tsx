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
 */

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authApi, type ScopedToken } from '../../api/auth';
import { errorMessage, shouldRetry } from '../../api/hooks';
import { AsyncPanel, ErrorBox } from '../../components/Status';
import { fmtAge, fmtTs } from '../../lib/format';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';

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
          <span className="spacer" />
          <span className="chip">{tokens.data?.length ?? 0}</span>
        </div>
        <AsyncPanel
          query={tokens}
          what="tokens"
          isEmpty={(t) => t.length === 0}
          emptyTitle="No scoped tokens"
          emptyDetail="Integrations here still use static API keys, which never expire and carry their role's full authority. A scoped token is the narrower alternative."
        >
          {(list) => (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">name</th>
                    <th scope="col">role</th>
                    <th scope="col">scopes</th>
                    <th scope="col">expires</th>
                    <th scope="col">last used</th>
                    <th scope="col">status</th>
                    <th scope="col">actions</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((t) => (
                    <tr key={t.id}>
                      <td className="mono">{t.name}</td>
                      <td>
                        {/* Role and scopes are capabilities, not states: neutral
                            chips throughout, so only `status` carries a tone. */}
                        <span className="chip">{t.role}</span>
                      </td>
                      <td>
                        {t.scopes.length === 0 ? (
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
                        )}
                      </td>
                      <td className="nowrap faint" title={t.expires_at ? fmtTs(t.expires_at) : ''}>
                        {t.expires_at ? (
                          fmtTs(t.expires_at)
                        ) : (
                          <span title="This credential never expires.">never</span>
                        )}
                      </td>
                      <td className="nowrap faint">
                        {/* The column api_keys could never answer. */}
                        {t.last_used_at ? (
                          fmtAge(t.last_used_at, Date.now())
                        ) : (
                          <span title="This token has never authenticated a request.">never</span>
                        )}
                      </td>
                      <td>
                        {t.revoked_at ? (
                          <span className="badge tone-stopped">revoked</span>
                        ) : t.live ? (
                          <span className="badge tone-success">live</span>
                        ) : (
                          <span className="badge tone-warning">expired</span>
                        )}
                      </td>
                      <td>
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
                      </td>
                    </tr>
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
