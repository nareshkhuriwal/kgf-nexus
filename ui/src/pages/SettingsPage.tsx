import type { Principal } from '../api/auth';
// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

/** Browser appearance, workspace scope, and account information. */
import { useQuery } from '@tanstack/react-query';
import { authRequest } from '../api/auth';
import { PageHeader } from '../components/PageHeader';
import { AppearancePanel } from '../components/AppearancePanel';
import { ErrorBox } from '../components/Status';

export interface SettingsPageProps {
  principal?: Principal;
  /** The namespace every page's data is scoped to. */
  namespace: string;
  /**
   * Applied as typed, with no save step, exactly as the old menu field was: the
   * whole page re-scopes as the value changes, so the result IS the
   * confirmation. App owns the value and its persistence.
   */
  onNamespaceChange: (next: string) => void;

}

export function SettingsPage({
  namespace,
  principal,
  onNamespaceChange,
}: SettingsPageProps) {
  const account = useQuery({
    queryKey: ['current-tenant', principal?.namespace, principal?.name],
    queryFn: () => authRequest<{tenant:{id:string;name:string;namespace:string;contact_email:string}}>('/v1/account/tenant'),
    enabled: !!principal,
    retry: false,
  });
  return (
    <div className="col">
      <PageHeader
        title="Settings"
        subtitle="Manage your appearance preferences and view your account details."
      />
      <a className="btn" href="/customer">Customer account & tenant details →</a>
      <section className="panel credential-card">
        <div className="panel-head">Current authentication <span className="chip">{principal ? 'Verified by gateway' : 'Not verified'}</span></div>
        <div className="panel-body">
          {principal ? <><dl className="credential-facts">
            <div><dt>{principal.kind === 'session' || principal.kind === 'user' ? 'Signed-in account' : 'Active token alias'}</dt><dd>{principal.name || 'Unnamed credential'}</dd></div>
            <div><dt>Authentication</dt><dd>{principal.kind === 'session' || principal.kind === 'user' ? 'User session' : 'Token'}</dd></div>
            <div><dt>Role</dt><dd>{principal.roles?.length ? principal.roles.join(", ") : principal.role}</dd></div>
            <div><dt>Credential namespace</dt><dd>{principal.namespace}</dd></div>
          </dl><details className="permission-disclosure"><summary>View effective permissions</summary><div className="credential-permissions">{(principal.effective_permissions ?? Object.entries(principal.permissions ?? {}).filter(([,allowed])=>allowed).map(([name])=>name)).map(name=><span className="chip" key={name}>{name.replaceAll('_',' ')}</span>)}</div></details>
          <p className="settings-hint">This is the credential currently used for requests. Token secrets are never displayed here.</p></> : <p className="settings-hint">Current credential details are unavailable. Connect a valid token or sign in to verify your identity.</p>}
        </div>
      </section>


      <div className="panel">
        <div className="panel-head">
          <span>appearance</span>
          <span className="spacer" />
          <span className="panel-note">
            applies immediately · stored in this browser, not on the server
          </span>
        </div>
        <div className="panel-body">
          {/* The accent is offered at all only because index.css keeps the brand
              hue (--accent) away from the state palette (--tone-*), so no
              choice made here can make a FAILED run read as SUCCEEDED. */}
          <AppearancePanel />
        </div>
      </div>

      <div className="panel">
        <div className="panel-head">
          <span>namespace</span>
          <span className="spacer" />
          <span className="panel-note">every endpoint is namespace-scoped</span>
        </div>
        <div className="panel-body">
          <div className="settings-field">
            <label className="settings-label" htmlFor="settings-namespace">
              Namespace
            </label>
            <input
              id="settings-namespace"
              type="text"
              value={namespace}
              readOnly={!!principal}
              onChange={(e) => onNamespaceChange(e.target.value)}
              aria-label="namespace"
            />
            {/* Stated because the failure it prevents is silent: a wrong
                namespace returns an empty, entirely plausible dashboard. */}
            <p className="settings-hint">
              A namespace that does not exist returns empty lists rather than an
              error, so an empty overview here means either nothing is running
              or this value is wrong.
            </p>
          </div>
        </div>
      </div>

      <section className="panel">
        <div className="panel-head">Account details</div>
        <div className="panel-body">
          {account.isLoading && <p>Loading account details…</p>}
          {account.error && <ErrorBox error={account.error} what="account details" onRetry={account.refetch} />}
          {account.data ? <dl className="credential-facts">
            <div><dt>Organization</dt><dd>{account.data.tenant.name}</dd></div>
            <div><dt>Tenant ID</dt><dd><code>{account.data.tenant.id}</code></dd></div>
            <div><dt>Namespace</dt><dd>{account.data.tenant.namespace}</dd></div>
            <div><dt>Contact email</dt><dd>{account.data.tenant.contact_email || 'Not configured'}</dd></div>
          </dl> : !principal && <p>Sign in to view your account details.</p>}
        </div>
      </section>
    </div>
  );
}
