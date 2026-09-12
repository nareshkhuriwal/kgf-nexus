// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * User administration.
 *
 * Every control here hits a real authorized endpoint with a tested effect; there
 * is no mock data and no button that does nothing. Where an action has a
 * consequence beyond the obvious one — disabling a user also ends their live
 * sessions, a password reset does the same — the UI says so BEFORE the click
 * rather than reporting it afterwards, because that consequence is the actual
 * reason an operator is on this screen during an offboarding.
 */

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authApi, type User } from '../../api/auth';
import { errorMessage, shouldRetry } from '../../api/hooks';
import { AsyncPanel, ErrorBox } from '../../components/Status';
import { fmtTs } from '../../lib/format';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';

export function UsersPage({ namespace, canAdmin }: { namespace: string; canAdmin: boolean }) {
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const users = useQuery({
    queryKey: ['users', namespace],
    queryFn: () => authApi.listUsers(namespace),
    retry: shouldRetry,
    // Only attempted when the credential may actually read it: firing a request
    // we know will 403 just to render an error is noise in the gateway's logs
    // and in the audit trail.
    enabled: canAdmin,
  });

  const disable = useMutation({
    mutationFn: (u: User) => authApi.setUserDisabled(namespace, u.id, !u.disabled),
    onSuccess: (updated) => {
      setActionError(null);
      setNotice(
        updated.disabled
          ? `${updated.email} is disabled. Every session they held has been ended.`
          : `${updated.email} is enabled again.`,
      );
      void qc.invalidateQueries({ queryKey: ['users', namespace] });
      void qc.invalidateQueries({ queryKey: ['audit', namespace] });
    },
    onError: (e) => {
      setNotice(null);
      setActionError(errorMessage(e));
    },
    retry: false,
  });

  const revoke = useMutation({
    mutationFn: (u: User) => authApi.revokeUserSessions(namespace, u.id),
    onSuccess: (res) => {
      setActionError(null);
      // The COUNT is reported: "3 sessions ended" is evidence, "done" is not.
      setNotice(
        res.sessions_revoked === 0
          ? 'That account had no live sessions.'
          : `Ended ${res.sessions_revoked} session${res.sessions_revoked === 1 ? '' : 's'}.`,
      );
      void qc.invalidateQueries({ queryKey: ['audit', namespace] });
    },
    onError: (e) => {
      setNotice(null);
      setActionError(errorMessage(e));
    },
    retry: false,
  });

  if (!canAdmin) {
    return (
      <Forbidden
        what="user administration"
        detail="Managing accounts needs an admin credential. A reader credential can see runs and workers but not identity."
      />
    );
  }

  return (
    <div className="col">
      <PageHeader
        title="Users"
        subtitle={
          <>
            human accounts in namespace <code>{namespace}</code>
          </>
        }
        actions={
          <button type="button" className="primary" onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New user'}
          </button>
        }
      />

      {/* An action that succeeded says so in the success tone. It reports the
          CONSEQUENCE (sessions ended, and how many), which is the evidence an
          offboarding actually needs — "done" is not evidence. */}
      {notice && (
        <div className="notice admin-notice" role="status">
          <span className="notice-glyph" aria-hidden="true">
            ✓
          </span>
          <div className="notice-detail">{notice}</div>
        </div>
      )}
      {actionError && <ErrorBox error={actionError} what="that action" />}

      {creating && (
        <CreateUser
          namespace={namespace}
          onDone={(email) => {
            setCreating(false);
            setNotice(`Created ${email}.`);
            void qc.invalidateQueries({ queryKey: ['users', namespace] });
            void qc.invalidateQueries({ queryKey: ['audit', namespace] });
          }}
        />
      )}

      <div className="panel">
        <div className="panel-head">
          <span>accounts</span>
          <span className="spacer" />
          <span className="chip">{users.data?.length ?? 0}</span>
        </div>
        <AsyncPanel
          query={users}
          what="users"
          isEmpty={(u) => u.length === 0}
          emptyTitle="No user accounts yet"
          emptyDetail="Only API keys can authenticate here so far. Create a user to sign in with an email and password."
        >
          {(list) => (
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">email</th>
                    <th scope="col">name</th>
                    <th scope="col">role</th>
                    <th scope="col">created</th>
                    <th scope="col">status</th>
                    <th scope="col">actions</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((u) => (
                    <tr key={u.id}>
                      <td className="mono">{u.email}</td>
                      <td>{u.display_name || <span className="faint">—</span>}</td>
                      <td>
                        {/* A role is a capability, not a state, so it stays a
                            neutral chip. Only `status` below is tone-coloured. */}
                        <span className="chip">{u.role}</span>
                      </td>
                      <td className="nowrap faint">{fmtTs(u.created_at)}</td>
                      <td>
                        {u.disabled ? (
                          <span className="badge tone-stopped">disabled</span>
                        ) : (
                          <span className="badge tone-success">active</span>
                        )}
                      </td>
                      <td>
                        <div className="cell-actions">
                          <button
                            type="button"
                            onClick={() => {
                              setNotice(null);
                              disable.mutate(u);
                            }}
                            disabled={disable.isPending}
                            title={
                              u.disabled
                                ? 'Allow this account to sign in again.'
                                : 'Block sign-in AND end every session this account currently holds.'
                            }
                          >
                            {u.disabled ? 'Enable' : 'Disable'}
                          </button>
                          <button
                            type="button"
                            onClick={() => {
                              setNotice(null);
                              revoke.mutate(u);
                            }}
                            disabled={revoke.isPending || u.disabled}
                            title="End every session this account holds, without disabling it."
                          >
                            Sign out everywhere
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

/**
 * Create-user form.
 *
 * The password is set by the admin rather than generated and mailed: this
 * deployment has no mail path, and inventing one that silently dropped the
 * message would be worse than asking the admin to convey it.
 */
function CreateUser({
  namespace,
  onDone,
}: {
  namespace: string;
  onDone: (email: string) => void;
}) {
  const [email, setEmail] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [role, setRole] = useState('reader');
  const [password, setPassword] = useState('');

  const create = useMutation({
    mutationFn: () =>
      authApi.createUser(namespace, {
        email: email.trim(),
        display_name: displayName.trim(),
        role,
        password,
      }),
    onSuccess: (u) => onDone(u.email),
    retry: false,
  });

  return (
    <div className="panel">
      <div className="panel-head">
        <span>new user</span>
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
            <span className="form-label">email</span>
            <input type="email" value={email} onChange={(e) => setEmail(e.target.value)} required />
            <span className="form-hint">Also the sign-in identity. Unique in this namespace.</span>
          </label>
          <label className="form-field">
            <span className="form-label">display name</span>
            <input value={displayName} onChange={(e) => setDisplayName(e.target.value)} />
            <span className="form-hint">Optional. Shown in the audit trail beside the address.</span>
          </label>
        </div>
        <div className="form-row">
          <label className="form-field">
            <span className="form-label">role</span>
            <select value={role} onChange={(e) => setRole(e.target.value)}>
              <option value="reader">reader — read everything, change nothing</option>
              <option value="admin">admin — full control of this namespace</option>
            </select>
            {/* 'worker' is deliberately absent: it is a machine credential for an
                execution host, and the gateway refuses it for a human anyway. */}
            <span className="form-hint">
              A worker role is not offered: it is a machine credential for an execution host, and
              the gateway refuses it for a human account.
            </span>
          </label>
          <label className="form-field">
            <span className="form-label">password (min 12 characters)</span>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
              minLength={12}
              required
            />
            <span className="form-hint">
              Length is the only rule. Composition rules push people toward predictable
              substitutions and away from length, which is what actually resists an offline attack.
            </span>
          </label>
        </div>
        {create.error && <ErrorBox error={create.error} what="the new user" />}
        <div className="form-actions">
          <button type="submit" className="primary" disabled={create.isPending}>
            {create.isPending ? 'Creating…' : 'Create user'}
          </button>
        </div>
      </form>
    </div>
  );
}
