// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
import { EditUser } from './EditUser';
import { authRequest } from '../../api/auth';
import { Link } from 'react-router-dom';
import { Permit } from '../../components/Permissions';
import { BulkActionBar } from '../../components/BulkActionBar';
import { ActionIcon, RowDetails } from '../../components/RowAction';
/**
 * User administration.
 *
 * Every control here hits a real authorized endpoint with a tested effect; there
 * is no mock data and no button that does nothing. Where an action has a
 * consequence beyond the obvious one — disabling a user also ends their live
 * sessions, a password reset does the same — the UI says so BEFORE the click
 * rather than reporting it afterwards, because that consequence is the actual
 * reason an operator is on this screen during an offboarding.
 *
 * THE ACCOUNT COUNT IS EXACT HERE, which is unusual in this console and worth
 * saying. /users takes no `limit` parameter at all, so what comes back is the
 * namespace's accounts rather than a page of them — "12 accounts" is a fact and
 * the table is told so. Every other list on these screens is a capped fetch and
 * gets a count that admits it. The claim is pinned to the endpoint's shape, so
 * if pagination is ever added to /users this constant is what has to change
 * with it.
 */

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authApi, type User } from '../../api/auth';
import { errorMessage, shouldRetry } from '../../api/hooks';
import { AsyncPanel, ErrorBox } from '../../components/Status';
import { DataTable } from '../../components/DataTable';
import type { Column, RowCountKind } from '../../components/DataTable';
import { fmtTs } from '../../lib/format';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';

/** /users sends every account: no `limit`, so no cap to disclose. */
const USERS_COUNT: RowCountKind = { kind: 'exact' };

export function UsersPage({ namespace, canAdmin }: { namespace: string; canAdmin: boolean }) {
  const [selected,setSelected] = useState<ReadonlySet<string>>(new Set());
  const qc = useQueryClient();
  const [tableControls,setTableControls] = useState<HTMLDivElement|null>(null);
  const [editing, setEditing] = useState<User | null>(null);
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

  /**
   * The column set, declared after the mutations because the actions column
   * closes over both of them.
   *
   * Rebuilt on every render rather than memoised, and deliberately so: the cells
   * read `disable.isPending` and `revoke.isPending`, so a memo would have to
   * list those in its dependencies and a stale one would leave a button enabled
   * during a request that is still in flight. This array is six objects; the
   * cost of building it is not worth the chance of that bug.
   */
  const columns: Column<User>[] = [
    {
      key: 'email',
      header: 'email',
      cellClass: 'mono',
      // The identity, and the column an admin scans. Sortable because an
      // offboarding starts from a name someone was given in a ticket.
      sort: (a, b) => a.email.localeCompare(b.email),
      cell: (u) => u.email,
    },
    {
      key: 'name',
      header: 'name',
      sort: (a, b) => (a.display_name || '').localeCompare(b.display_name || ''),
      cell: (u) => u.display_name || <span className="faint">—</span>,
    },
    {
      key: 'role',
      header: 'role',
      sort: (a, b) => a.role.localeCompare(b.role),
      // A role is a capability, not a state, so it stays a neutral chip. Only
      // `status` below is tone-coloured.
      cell: (u) => <span className="chip">{u.role}</span>,
    },
    {
      key: 'created',
      header: 'created',
      cellClass: 'nowrap faint',
      sort: (a, b) => Date.parse(a.created_at) - Date.parse(b.created_at),
      cell: (u) => fmtTs(u.created_at),
    },
    {
      key: 'status',
      header: 'status',
      // Disabled accounts gather at one end, which is what an audit of "who
      // still has access" is actually reading for.
      sort: (a, b) => Number(a.disabled) - Number(b.disabled),
      cell: (u) =>
        u.disabled ? (
          <span className="badge tone-stopped">disabled</span>
        ) : (
          <span className="badge tone-success">active</span>
        ),
    },
    {
      key: 'actions',
      header: 'actions',
      cell: (u) => (
        <div className="cell-actions"><Permit permission="users.manage"><button type="button" className="icon-action" aria-label={`Edit user ${u.email}`} title="Edit user" onClick={() => setEditing(u)}><ActionIcon name="edit"/></button></Permit><RowDetails label={`View user ${u.email}`} value={u}/>
          <Permit permission="users.manage"><button
            type="button"
            onClick={() => {
              setNotice(null);
              disable.mutate(u);
            }}
            className="icon-action" aria-label={u.disabled ? "Enable user" : "Disable user"} disabled={disable.isPending}
            title={
              u.disabled
                ? 'Allow this account to sign in again.'
                : 'Block sign-in AND end every session this account currently holds.'
            }
          >
            <ActionIcon name={u.disabled ? "enable" : "disable"}/>
          </button></Permit>
          <Permit permission="users.manage"><button
            type="button"
            onClick={() => {
              setNotice(null);
              revoke.mutate(u);
            }}
            className="icon-action" aria-label="Sign out everywhere" disabled={revoke.isPending || u.disabled}
            title="End every session this account holds, without disabling it."
          >
            <ActionIcon name="logout"/>
          </button></Permit>
        </div>
      ),
    },
  ];

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
        actions={<>
          <Permit permission="roles.manage"><Link className="btn" to="/admin/roles">Manage role assignments</Link></Permit>
          <Permit permission="users.manage"><button type="button" className="primary" onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New user'}
          </button></Permit></>
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

      {editing && <EditUser key={editing.id} namespace={namespace} user={editing} onClose={() => setEditing(null)} />}
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
          <span>Accounts</span><div className="panel-head-controls" ref={setTableControls} />
        </div>
        <AsyncPanel
          query={users}
          what="users"
          isEmpty={(u) => u.length === 0}
          emptyTitle="No user accounts yet"
          emptyDetail="Only API keys can authenticate here so far. Create a user to sign in with an email and password."
        >
          {(list) => (
            <>
            <BulkActionBar<User> selected={list.filter(u=>selected.has(u.id))} rowId={u=>u.id} rowLabel={u=>u.email} noun={['user','users']} actions={[
              {permission: "users.manage", verb:'Disable',destructive:true,consequence:'Block sign-in and end existing sessions for selected accounts.',run:u=>authApi.setUserDisabled(namespace,u.id,true)},
              {permission: "users.manage", verb:'Enable',run:u=>authApi.setUserDisabled(namespace,u.id,false)}
            ]} onClear={()=>setSelected(new Set())} onDone={()=>{void qc.invalidateQueries({queryKey:['users',namespace]})}}/>
            <DataTable
              controlsTarget={tableControls}
              rows={list}
              selection={{selected,onChange:setSelected,rowId:u=>u.id,label:u=>u.email}}
              columns={columns}
              rowKey={(u) => u.id}
              noun="accounts"
              /* Exact, uniquely on this page: see the constant's note. */
              count={USERS_COUNT}
              /* Client-side, and honestly so. /users has no `search`
                 parameter — but unlike the capped lists this one holds every
                 account, so filtering it really does search the whole
                 namespace. The table still labels the filter as local, which
                 understates rather than overstates what it did: the failure
                 mode worth preventing is a filter that claims more reach than
                 it has, never one that claims less. */
              search={{ mode: 'client', fields: (u) => [u.email, u.display_name, u.role] }}
            />
            </>
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
  const [role, setRole] = useState('Viewer');
  const roles = useQuery({queryKey:['roles',namespace],queryFn:()=>authRequest<{roles:{name:string;builtin:boolean;permissions:string[]}[]}>('/v1/namespaces/'+encodeURIComponent(namespace)+'/roles')});
  const validRole = roles.data?.roles.some(r=>r.name===role) === true;
  const [password, setPassword] = useState('');

  const create = useMutation({
    mutationFn: () =>
      authApi.createUser(namespace, {
        email: email.trim(),
        display_name: displayName.trim(),
        role: role === "Administrator" ? "admin" : "reader",
        role_name: role,
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
          if(validRole) create.mutate();
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
            <select value={role} disabled={roles.isPending || roles.isError} onChange={(e) => setRole(e.target.value)}>
              {!roles.data && <option value="Viewer">{roles.isError ? "Unable to load roles" : "Loading roles…"}</option>}
              {roles.data?.roles.map(r=><option key={r.name} value={r.name}>{r.name}{r.name === "Administrator" ? " — all permissions (protected)" : r.builtin ? "" : " — custom"}</option>)}
            </select>
            {/* 'worker' is deliberately absent: it is a machine credential for an
                execution host, and the gateway refuses it for a human anyway. */}
            <span className="form-hint">
              The selected role is applied when the account is created. Administrator has all permissions and its definition cannot be changed.
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
        {roles.error && <><ErrorBox error={roles.error} what="role catalog" /><button type="button" onClick={()=>void roles.refetch()}>Reload roles</button></>}
        {create.error && <ErrorBox error={create.error} what="the new user" />}
        <div className="form-actions">
          <button type="submit" className="primary" disabled={create.isPending || !validRole}>
            {create.isPending ? 'Creating…' : 'Create user'}
          </button>
        </div>
      </form>
    </div>
  );
}
