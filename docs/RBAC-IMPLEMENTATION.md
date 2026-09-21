# Module access shipped September 13, 2026

## Administration

Open **Govern → Roles & permissions** (`/admin/roles`). Built-in roles are Administrator, Viewer, Workflow Operator, Workflow Developer, Infrastructure Operator, and Auditor. Inspect their matrix or create a custom role. View permissions control navigation and direct route access; action permissions control operations. A custom role must include the prerequisites reported by the server.

**Users → New user** loads the live role catalog, including custom roles and Administrator. The account and initial role assignment are committed together; a missing role or failed assignment leaves no account behind. Administrator selection creates an admin account with the immutable system role. Other selections create reader accounts with explicit module bindings.

Under **User role assignments**, select a reader account and save one or more roles. Grants combine within that account's namespace. An explicit empty assignment denies all modules. Existing accounts without assignments retain their legacy permissions for compatibility. Administrator is a single system-defined role per namespace, available automatically during account setup. Every admin account, including existing ones, belongs to it automatically. It always includes the complete registered application permission catalog, including permissions added in later releases. Its name, grants and existence cannot be changed through role APIs or the UI, even by administrators. It is not a mutable database role, so no onboarding seed or migration can accidentally create duplicate copies. Existing administrators retain this role and cannot receive restricted role assignments. The Users page links to this editor.

Role changes are read from PostgreSQL for every authenticated session request. The console refreshes identity every ten seconds. Missing/unknown permissions and unregistered protected routes deny access. API checks remain authoritative for every mutation, including bulk operations. Human sessions cannot use the worker protocol. Raw event payloads are redacted without `tasks.results.view`; log access requires `tasks.logs.view`.

Role writes and assignments serialize within the namespace; revisions reject stale saves. Assigned roles cannot be deleted. An administrator cannot change their own role bindings, delegate grants their credential does not hold, or disable the last active human administrator. Role/assignment writes and their audit entries commit together.

## Current boundary against the broader design

This is namespace-scoped **human-account module RBAC**. The larger RBAC-DESIGN.md remains the target contract, not a claim that every proposed feature exists:

- Existing static/scoped machine tokens retain their existing role/scope intersection. Service-account role bindings and per-token granular ceilings are not implemented.
- Identity administration is reserved for existing namespace administrators. Custom roles cannot delegate user/token/role management. Several mutations currently share `manage`, `update`, or `configure` grants rather than the more granular proposed catalog.
- Cross-namespace Owner roles, identity-provider group mapping, membership management, separate cluster/budget permissions, and cloud application controllers are not implemented.
- User assignment picker shows up to 500 recent accounts. The existing users API does not support cursor pagination.
- This change does not start AKS, provision resources, or apply infrastructure scaling plans.

## Migration and deployment

Apply `services/migrations/0012_rbac.up.sql` before deploying the new gateway. It adds roles and account assignments without changing existing users or credentials. Deploy UI and gateway together: the UI uses the gateway's `effective_permissions`. Rollback requires reverting gateway/UI before dropping the new tables; export assignments before dropping them.

## Validation

- API gateway and store Go suites.
- PostgreSQL integration test covering assignment, stale revision, role deletion protection, direct API denial, cross-namespace denial, immediate revocation, protected admin bindings and last-admin disable prevention.
- UI permission tests proving forbidden routes do not mount, hidden navigation, delegated audit visibility and revocation.
- Run/workflow detail and workflow list regression tests, including terminal run actions.
- TypeScript and production bundle build. Browser interaction and cloud deployments are not part of this verification.
