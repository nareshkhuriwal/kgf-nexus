# ADR-0012: Namespace-scoped roles and module permissions

Status: Proposed — not yet implemented

## Context

The current gateway authorizes admin/reader/worker through read, write, admin_write and worker_rpc. Console visibility primarily depends on admin_write. This cannot express an operator who controls runs but cannot administer users, or an auditor who reads audit evidence without mutation authority.

## Decision

Use exact module/action permission identifiers, versioned built-in roles, custom role permission sets, and namespace-scoped user/service-account bindings. PostgreSQL is authoritative. Token ceilings intersect with current service-account grants. Resolve authorization in the shared authentication layer and enforce explicit endpoint requirements on REST and gRPC, plus resource namespace/state checks. The UI consumes effective permissions; it does not maintain a second role-to-permission matrix.

Protect delegation and the last administrator in transactions. Keep audit immutable. Do not add wildcard grants, role inheritance or conflicting per-user overrides in this version. Cloud IAM and Kubernetes RBAC remain additional boundaries.

## Consequences

The change requires a coordinated migration, backend enforcement and UI rollout. Navigation-only restrictions are insufficient. Role changes must affect existing sessions; token creation and legacy user-role mutation must not bypass delegation checks. New permissions are not implicitly granted to custom roles. A rollback to legacy broad grants after restrictions take effect is unsafe.

The complete catalog, presets, data model, API contract, migration sequence and acceptance criteria are specified in [RBAC-DESIGN.md](../RBAC-DESIGN.md).
