# Module access shipped September 13, 2026

## Administration

Open **Govern → Roles & permissions** (`/admin/roles`). Built-in roles are Administrator, Viewer, Workflow Operator, Workflow Developer, Infrastructure Operator, and Auditor. Inspect their matrix or create a custom role. View permissions control navigation and direct route access; action permissions control operations. A custom role must include the prerequisites reported by the server.

**Users → New user** loads the live role catalog, including custom roles and Administrator. The account and initial role assignment are committed together; a missing role or failed assignment leaves no account behind. Administrator selection creates an admin account with the immutable system role. Other selections create reader accounts with explicit module bindings.

Under **User role assignments**, select a reader account and save one or more roles. Grants combine within that account's namespace. An explicit empty assignment denies all modules. Existing accounts without assignments retain their legacy permissions for compatibility. Administrator is a single system-defined role per namespace, available automatically during account setup. Every admin account, including existing ones, belongs to it automatically. It always includes the complete registered application permission catalog, including permissions added in later releases. Its name, grants and existence cannot be changed through role APIs or the UI, even by administrators. It is not a mutable database role, so no onboarding seed or migration can accidentally create duplicate copies. Existing administrators retain this role and cannot receive restricted role assignments. The Users page links to this editor.

Role changes are read from PostgreSQL for every authenticated session request. The console refreshes identity every ten seconds. Missing/unknown permissions and unregistered protected routes deny access. API checks remain authoritative for every mutation, including bulk operations. Human sessions cannot use the worker protocol. Raw event payloads are redacted without `tasks.results.view`; log access requires `tasks.logs.view`.

Role writes and assignments serialize within the namespace; revisions reject stale saves. Assigned roles cannot be deleted. An administrator cannot change their own role bindings, delegate grants their credential does not hold, or disable the last active human administrator. Role/assignment writes and their audit entries commit together.

## Added with KGF Cloud integration (ADR-0024, migration 0032)

- **Service accounts** (`service_accounts.view`, `service_accounts.manage`): non-human identities for agents and CI with role bindings (`/v1/namespaces/{ns}/service-accounts`, `.../{id}/roles`). Their tokens (`POST .../tokens` with `service_account_id` and `permissions`) carry a permission ceiling; effective grants are the account's current roles intersected with the ceiling, re-read on every request, and disabling the account refuses its tokens immediately. Legacy scoped tokens may also carry a ceiling. Bindings and ceilings may hold only grants the actor holds and never `users.manage`, `roles.manage`, `tokens.manage` or `service_accounts.manage`; an unceilinged token can be minted only by an Administrator.
- **Users pagination:** `GET /v1/namespaces/{ns}/users?limit=&cursor=&q=` is keyset-paginated (newest first, at most 200 per page) with server-side search.
- **KGF principals:** in `NEXUS_AUTH_MODE=kgf|both`, a request with a verified `X-KGF-Context` becomes a principal of the namespace mapped from its KGF organization/subscription/resource group, holding the Nexus permissions whose `KGF.Nexus/*` action its KGF role (operator = Contributor, viewer = Reader) covers. Unmapped permissions are denied, and `users.*` is never granted: KGF owns people. A KGF operator may manage roles, service accounts and tokens in its namespace (there is no Nexus Administrator there), bounded by the same delegation rule. The mapping table is ADR-0024 §4; configuration is OPERATIONS §24.

## Current boundary against the broader design

This is namespace-scoped **human-account module RBAC**, plus service accounts and KGF principals. The larger RBAC-DESIGN.md remains the target contract, not a claim that every proposed feature exists:

- Identity administration for Nexus accounts is reserved for existing namespace administrators; custom roles cannot hold `users.manage`, `tokens.manage`, `roles.manage` or `service_accounts.manage`. Several mutations share `manage`, `update` or `configure` grants rather than the more granular proposed catalog. Inside KGF Cloud, people and their delegation are KGF's (kgf-identity), not Nexus's.
- Cross-namespace Owner roles, identity-provider group mapping and membership management are not implemented in Nexus; in KGF Cloud they belong to kgf-identity.
- Separate cluster/budget permissions are not implemented: cluster registration uses `infrastructure.*`, and the namespace budget is the quota (`quotas.*`, `usage.view`).
- Binding expiry, authorization revisions and the evaluate endpoint are not implemented.
- This change does not start AKS, provision resources, or apply infrastructure scaling plans.

## Migration and deployment

Apply `services/migrations/0012_rbac.up.sql` (and, for service accounts, token ceilings and KGF mode, `0032_kgf_context_and_service_accounts.up.sql`) before deploying the new gateway. It adds roles and account assignments without changing existing users or credentials. Deploy UI and gateway together: the UI uses the gateway's `effective_permissions`. Rollback requires reverting gateway/UI before dropping the new tables; export assignments before dropping them.

## Validation

- API gateway and store Go suites.
- PostgreSQL integration test covering assignment, stale revision, role deletion protection, direct API denial, cross-namespace denial, immediate revocation, protected admin bindings and last-admin disable prevention.
- UI permission tests proving forbidden routes do not mount, hidden navigation, delegated audit visibility and revocation.
- Run/workflow detail and workflow list regression tests, including terminal run actions.
- TypeScript and production bundle build. Browser interaction and cloud deployments are not part of this verification.

## Permissions added with expansion, memoization and quarantine

- `workers.quarantine` — quarantine and release a worker (`POST …/workers/{id}/quarantine|release`).
  Admin-level in the legacy (unbound) mapping, like `workers.drain`; granted to the built-in
  Infrastructure Operator. Reading a worker's quarantine history needs only `workers.view`.
- `workflows.cache.invalidate` — record a memo invalidation (`POST …/cache/invalidations`).
  Write-level in the legacy mapping (it can only cause work to run again); granted to the
  built-in Workflow Developer.
