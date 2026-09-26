# Module access: what is implemented

First shipped September 13, 2026 (ADR-0012, migration 0012); extended
September 26, 2026 by ADR-0024 (migration 0032): granular identity
permissions and delegation, service accounts and token ceilings,
cross-namespace owners, IdP group mapping, separate cluster permissions and
users pagination.

## Administration

Open **Govern → Roles & permissions** (`/admin/roles`). Built-in roles are Administrator, Viewer, Workflow Operator, Workflow Developer, Infrastructure Operator, and Auditor. Inspect their matrix or create a custom role. View permissions control navigation and direct route access; action permissions control operations. A custom role must include the prerequisites reported by the server.

**Users → New user** loads the live role catalog, including custom roles and Administrator. The account and initial role assignment are committed together; a missing role or failed assignment leaves no account behind. Administrator selection creates an admin account with the immutable system role, and only an Administrator (or Owner) may make that selection. Other selections create reader accounts with explicit module bindings.

Under **User role assignments**, search for a reader account (server-side, paginated) and save one or more roles. Grants combine within that account's namespace. An explicit empty assignment denies all modules. Existing accounts without assignments retain their legacy permissions for compatibility. Administrator is a single system-defined role per namespace, available automatically during account setup. Every admin account, including existing ones, belongs to it automatically. It always includes the complete registered application permission catalog, including permissions added in later releases. Its name, grants and existence cannot be changed through role APIs or the UI, even by administrators. It is not a mutable database role, so no onboarding seed or migration can accidentally create duplicate copies. Existing administrators retain this role and cannot receive restricted role assignments.

Role changes are read from PostgreSQL for every authenticated request. The console refreshes identity every ten seconds. Missing/unknown permissions and unregistered protected routes deny access. API checks remain authoritative for every mutation, including bulk operations. Human sessions cannot use the worker protocol; neither can any grant-bearing credential. Raw event payloads are redacted without `tasks.results.view`; log access requires `tasks.logs.view`.

Role writes and assignments serialize within the namespace; revisions reject stale saves. Roles in use (by a user, an IdP sync, a service account or an IdP group mapping) cannot be deleted. An actor cannot change their own role bindings, delegate grants their credential does not hold, or disable the last active human administrator. Role/assignment writes and their audit entries commit together.

## Identity permissions and delegation (ADR-0024 §7)

The coarse grants were split per action. The old identifiers were never storable in a custom role, so no stored role changed meaning.

| Module | Permissions |
|---|---|
| users | `view`, `create`, `update`, `enable`, `disable`, `sessions.revoke`, `password.reset`, `roles.assign` |
| roles | `view`, `create`, `update`, `delete` |
| tokens | `view`, `create`, `revoke` |
| service_accounts | `view`, `create`, `update`, `disable`, `roles.assign` |
| sso | `view`, `configure`, `mappings.update` |
| owners | `view`, `grant` |
| clusters | `view`, `configure` (split from `infrastructure.*`; 0032 added them to every custom role that held the infrastructure grants) |

Custom roles may hold identity permissions. Delegation is bounded by three rules, enforced in the handlers inside the namespace's advisory-lock transaction:

- **Content:** a role, assignment, service-account binding, IdP group mapping or token ceiling may contain only grants the actor holds.
- **Target:** a delegated (non-Administrator) actor may act only on accounts whose effective grants are a subset of its own, and never on an Administrator (`403 target_above_actor`, audited as denied). This is what stops a delegate with `users.password.reset` from taking over the recovery administrator.
- **Reserved:** `owners.grant` is never placeable in a custom role, a service-account binding or a token ceiling, and needs an Administrator or Owner *session* (admin API keys cannot grant ownership). Creating an Administrator account also needs an Administrator.

Budgets: the namespace budget is the quota (`quotas.view`, `quotas.manage`, `usage.view`, ADR-0021), already separate from infrastructure. No `budgets.*` grant exists because there is no budget API behind it.

## Service accounts and token ceilings (ADR-0024 §8)

**Govern → Service accounts** creates non-human identities with role bindings. Their tokens carry an explicit permission ceiling; effective grants are the account's *current* roles intersected with the ceiling, re-read on every request, so narrowing or disabling the account takes effect immediately. Legacy scoped tokens may also carry a ceiling (legacy role/scope allowances ∩ ceiling); a non-Administrator must set one. Legacy tokens without a ceiling and API keys are unchanged.

## Cross-namespace owners (ADR-0024 §9)

An Administrator of namespace B can grant Owner of B to a user from another namespace, by user ID and email. That user's session then acts with Administrator authority in B only, recorded as role `Owner` and attributed to the real person in B's audit trail. Every namespace is an explicit grant; there is no wildcard or global owner. `GET /v1/auth/memberships` lists the namespaces a session may act in.

## IdP group mapping (ADR-0024 §6)

Each SSO provider maps IdP groups to roles. At every SSO login the account's IdP-sourced roles (`rbac_user_access.idp_roles`) are replaced by the roles its current groups map to; administrator-assigned roles are kept separately. Removing a group removes the role at the next login, optionally revoking the user's sessions. Administrator cannot be mapped. See OPERATIONS §24.

## Users pagination

`GET /v1/namespaces/{ns}/users?limit=&cursor=&q=` is keyset-paginated (newest first, at most 200 per page) with server-side search on email and display name. The 500-account picker cap is gone.

## Current boundary against the broader design

RBAC-DESIGN.md remains the target contract. Still not implemented:

- Binding expiry (`expires_at` on role bindings), the `authorization_revision` cache-invalidation protocol, and the `authorization/evaluate` preview endpoint.
- A global Platform Owner and namespace lifecycle; owners are granted per namespace.
- Granular grants for workflows/runs/schedules beyond the existing catalog (`runs.export`, `tasks.logs.download`, `schedules.enable`, …) and `audit.export`.
- Worker Agent as an RBAC role with queue scopes; worker credentials keep the legacy `worker` role.
- Group-driven revocation is bounded by the next login (no SCIM, no back-channel logout).
- Cloud application controllers; this change does not start AKS, provision resources, or apply infrastructure scaling plans.

## Migration and deployment

Apply `services/migrations/0012_rbac.up.sql` and, for the ADR-0024 features, `0032_sso_and_rbac.up.sql` before deploying the new gateway. Deploy UI and gateway together: the UI uses the gateway's `effective_permissions`, whose identity identifiers changed. Mixed-version behaviour and rollback are in OPERATIONS §24.10.

## Validation

- API gateway, authn and store Go suites; `sso` package unit tests with real signatures.
- PostgreSQL integration tests: assignment, stale revision, role deletion protection, direct API denial, cross-namespace denial, immediate revocation, protected admin bindings, last-admin disable prevention; delegated identity administration (content, target and reserved rules); service accounts and ceilings; owners; pagination; the cluster split; SSO flows and attacks.
- Keycloak end-to-end suite (`tests/sso/run-keycloak-e2e.sh`).
- UI permission tests proving forbidden routes do not mount, hidden navigation, delegated audit visibility and revocation.

## Permissions added with expansion, memoization and quarantine

- `workers.quarantine` — quarantine and release a worker (`POST …/workers/{id}/quarantine|release`).
  Admin-level in the legacy (unbound) mapping, like `workers.drain`; granted to the built-in
  Infrastructure Operator. Reading a worker's quarantine history needs only `workers.view`.
- `workflows.cache.invalidate` — record a memo invalidation (`POST …/cache/invalidations`).
  Write-level in the legacy mapping (it can only cause work to run again); granted to the
  built-in Workflow Developer.
