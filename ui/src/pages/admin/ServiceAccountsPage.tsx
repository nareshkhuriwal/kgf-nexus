// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/**
 * Service accounts (ADR-0024 §6): non-human identities for agents and CI.
 *
 * An account holds roles; a token is minted FOR it with a permission ceiling,
 * and the token's effective grants are the account's CURRENT roles
 * intersected with that ceiling. That is the reason to prefer one over a
 * free-standing token: narrowing the roles, or disabling the account, takes
 * effect on the next request of every token it has.
 *
 * Every control here calls a real endpoint. The gateway is the one that
 * refuses a role or ceiling outside the operator's own permissions; its
 * message is shown as is.
 */

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { authRequest, type ScopedToken } from '../../api/auth';
import { grantsOf, serviceAccountsApi, type RoleGrants, type ServiceAccount } from '../../api/serviceAccounts';
import { errorMessage, shouldRetry } from '../../api/hooks';
import { AsyncPanel, ErrorBox } from '../../components/Status';
import { DataTable, type Column } from '../../components/DataTable';
import { PageHeader } from '../../components/PageHeader';
import { Forbidden } from '../../components/Forbidden';
import { Permit, usePermission } from '../../components/Permissions';
import { fmtTs } from '../../lib/format';

type RoleCatalog = { roles: RoleGrants[] };

export function ServiceAccountsPage({ namespace }: { namespace: string }) {
  const can = usePermission();
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [minting, setMinting] = useState<ServiceAccount | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [controls, setControls] = useState<HTMLDivElement | null>(null);

  const accounts = useQuery({
    queryKey: ['service-accounts', namespace],
    queryFn: () => serviceAccountsApi.list(namespace),
    retry: shouldRetry,
    enabled: can('service_accounts.view'),
  });
  const roles = useQuery({
    queryKey: ['roles', namespace],
    queryFn: () => authRequest<RoleCatalog>('/v1/namespaces/' + encodeURIComponent(namespace) + '/roles'),
    retry: shouldRetry,
    enabled: can('service_accounts.view') && can('roles.view'),
  });
  const toggle = useMutation({
    mutationFn: (a: ServiceAccount) => serviceAccountsApi.setDisabled(namespace, a.id, !a.disabled),
    onSuccess: (_r, a) => {
      setActionError(null);
      setNotice(a.disabled ? `${a.name} is enabled; its tokens work again.` : `${a.name} is disabled; every token it has is refused from now on.`);
      void qc.invalidateQueries({ queryKey: ['service-accounts', namespace] });
    },
    onError: (e) => { setNotice(null); setActionError(errorMessage(e)); },
    retry: false,
  });

  if (!can('service_accounts.view')) {
    return <Forbidden what="service accounts" detail="Viewing service accounts needs the service_accounts.view permission." />;
  }

  const columns: Column<ServiceAccount>[] = [
    { key: 'name', header: 'name', cellClass: 'mono', sort: (a, b) => a.name.localeCompare(b.name), cell: (a) => a.name },
    { key: 'description', header: 'description', cell: (a) => a.description || <span className="faint">—</span> },
    { key: 'roles', header: 'roles', cell: (a) => (a.roles.length ? a.roles.map((r) => <span key={r} className="chip">{r}</span>) : <span className="faint">no roles: its tokens can do nothing</span>) },
    { key: 'tokens', header: 'live tokens', sort: (a, b) => a.live_tokens - b.live_tokens, cell: (a) => a.live_tokens },
    { key: 'created', header: 'created', cellClass: 'nowrap faint', cell: (a) => fmtTs(a.created_at) },
    {
      key: 'status', header: 'status',
      cell: (a) => (a.disabled ? <span className="badge tone-stopped">disabled</span> : <span className="badge tone-success">active</span>),
    },
    {
      key: 'actions', header: 'actions',
      cell: (a) => (
        <div className="cell-actions">
          <Permit permission="tokens.manage">
            <button type="button" aria-label={`Mint token for ${a.name}`} disabled={a.disabled} title={a.disabled ? 'Enable the account first' : 'Mint a token for this account'}
              onClick={() => { setNotice(null); setMinting(a); }}>Mint token</button>
          </Permit>
          <Permit permission="service_accounts.manage">
            <button type="button" disabled={toggle.isPending} onClick={() => toggle.mutate(a)}
              title={a.disabled ? 'Allow its tokens again' : 'Refuse every token of this account at once'}>
              {a.disabled ? 'Enable' : 'Disable'}
            </button>
          </Permit>
        </div>
      ),
    },
  ];

  return (
    <div className="col">
      <PageHeader
        title="Service accounts"
        subtitle={<>non-human identities for agents and CI in <code>{namespace}</code></>}
        actions={<Permit permission="service_accounts.manage"><button type="button" className="primary" onClick={() => setCreating((v) => !v)}>{creating ? 'Cancel' : 'New service account'}</button></Permit>}
      />
      {notice && <div className="notice admin-notice" role="status"><span className="notice-glyph" aria-hidden="true">✓</span><div className="notice-detail">{notice}</div></div>}
      {actionError && <ErrorBox error={actionError} what="that action" />}
      {creating && (
        <CreateServiceAccount namespace={namespace} roles={roles.data?.roles ?? []} rolesError={roles.error}
          onDone={(name) => { setCreating(false); setNotice(`Created ${name}.`); void qc.invalidateQueries({ queryKey: ['service-accounts', namespace] }); }} />
      )}
      {minting && (
        <MintToken namespace={namespace} account={minting} roles={roles.data?.roles ?? []}
          onClose={() => { setMinting(null); void qc.invalidateQueries({ queryKey: ['service-accounts', namespace] }); }} />
      )}
      <div className="panel">
        <div className="panel-head"><span>Accounts</span><div className="panel-head-controls" ref={setControls} /></div>
        <AsyncPanel query={accounts} what="service accounts" isEmpty={(l) => l.length === 0}
          emptyTitle="No service accounts yet"
          emptyDetail="Create one for each agent pool or CI pipeline, give it the narrowest role that works, and mint its tokens here.">
          {(list) => <DataTable controlsTarget={controls} rows={list} columns={columns} rowKey={(a) => a.id} noun="service accounts"
            count={{ kind: 'capped', limit: 500 }} search={{ mode: 'client', fields: (a) => [a.name, a.description, ...a.roles] }} />}
        </AsyncPanel>
      </div>
    </div>
  );
}

function CreateServiceAccount({ namespace, roles, rolesError, onDone }: {
  namespace: string; roles: RoleGrants[]; rolesError: unknown; onDone: (name: string) => void;
}) {
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [chosen, setChosen] = useState<string[]>([]);
  const create = useMutation({
    mutationFn: () => serviceAccountsApi.create(namespace, { name: name.trim(), description: description.trim(), roles: chosen }),
    onSuccess: (a) => onDone(a.name),
    retry: false,
  });
  const assignable = roles.filter((r) => r.name !== 'Administrator' && r.assignable !== false);
  return (
    <form className="panel form-body" onSubmit={(e) => { e.preventDefault(); create.mutate(); }}>
      <div className="panel-head"><span>new service account</span></div>
      <div className="panel-body col">
        <label className="form-field"><span className="form-label">name</span>
          <input aria-label="Service account name" value={name} onChange={(e) => setName(e.target.value)} required pattern="[a-z0-9][a-z0-9._\-]{1,62}" />
          <span className="form-hint">Lower-case letters, digits, dots, dashes or underscores; unique in this namespace.</span>
        </label>
        <label className="form-field"><span className="form-label">description</span>
          <input value={description} maxLength={500} onChange={(e) => setDescription(e.target.value)} />
        </label>
        <fieldset className="rbac-matrix"><legend>roles</legend>
          {rolesError ? <ErrorBox error={rolesError} what="the role catalog" /> : null}
          {!rolesError && assignable.length === 0 && <p className="faint">Loading roles…</p>}
          {assignable.map((r) => (
            <label key={r.name}><input type="checkbox" checked={chosen.includes(r.name)}
              onChange={(e) => setChosen(e.target.checked ? [...chosen, r.name] : chosen.filter((x) => x !== r.name))} />{r.name}</label>
          ))}
        </fieldset>
        <p className="form-hint">You can only give roles whose every permission you hold yourself.</p>
        {create.error && <ErrorBox error={create.error} what="the service account" />}
        <div className="form-actions"><button type="submit" className="primary" disabled={create.isPending}>{create.isPending ? 'Creating…' : 'Create service account'}</button></div>
      </div>
    </form>
  );
}

function MintToken({ namespace, account, roles, onClose }: {
  namespace: string; account: ServiceAccount; roles: RoleGrants[]; onClose: () => void;
}) {
  const available = grantsOf(account.roles, roles);
  const [name, setName] = useState(account.name + '-token');
  const [days, setDays] = useState(90);
  const [ceiling, setCeiling] = useState<string[] | null>(null);
  const chosen = ceiling ?? available;
  const [minted, setMinted] = useState<ScopedToken | null>(null);
  const mint = useMutation({
    mutationFn: () => serviceAccountsApi.mintToken(namespace, { name: name.trim(), service_account_id: account.id, permissions: chosen, expires_in_days: days }),
    onSuccess: (t) => setMinted(t),
    retry: false,
  });
  if (minted?.token) {
    return (
      <div className="panel" role="status">
        <div className="panel-head"><span>token for {account.name}</span></div>
        <div className="panel-body col">
          <p><strong>Copy it now: it is shown once and cannot be recovered.</strong></p>
          <code className="mono" style={{ wordBreak: 'break-all' }}>{minted.token}</code>
          <p className="faint">Ceiling: {(minted.permissions ?? []).join(' · ')}. Expires {minted.expires_at ? new Date(minted.expires_at).toLocaleString() : 'never'}.</p>
          <div className="form-actions"><button type="button" onClick={onClose}>Done</button></div>
        </div>
      </div>
    );
  }
  return (
    <form className="panel form-body" onSubmit={(e) => { e.preventDefault(); mint.mutate(); }}>
      <div className="panel-head"><span>mint a token for {account.name}</span></div>
      <div className="panel-body col">
        <label className="form-field"><span className="form-label">token name</span>
          <input value={name} onChange={(e) => setName(e.target.value)} required /></label>
        <label className="form-field"><span className="form-label">expires in (days)</span>
          <input type="number" min={1} max={365} value={days} onChange={(e) => setDays(Number(e.target.value))} /></label>
        <fieldset className="rbac-matrix"><legend>permission ceiling</legend>
          {available.length === 0 && <p className="faint">This account holds no permissions yet; give it a role first.</p>}
          {available.map((g) => (
            <label key={g}><input type="checkbox" checked={chosen.includes(g)}
              onChange={(e) => setCeiling(e.target.checked ? [...chosen, g] : chosen.filter((x) => x !== g))} />{g}</label>
          ))}
        </fieldset>
        <p className="form-hint">The token can never do more than both this ceiling and the account's roles at the time of each request.</p>
        {mint.error && <ErrorBox error={mint.error} what="the token" />}
        <div className="form-actions">
          <button type="button" onClick={onClose} disabled={mint.isPending}>Cancel</button>
          <button type="submit" className="primary" disabled={mint.isPending || chosen.length === 0}>{mint.isPending ? 'Minting…' : 'Mint token'}</button>
        </div>
      </div>
    </form>
  );
}
