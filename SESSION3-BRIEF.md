# Session 3 — Identity & Administration vertical slice

## Isolation (binding)
Worktree `/Users/nareshkhuriwal/Khuriwalgroup/kgf-nexus-identity`, branch
`f77-identity-admin`, based on `d3b987d`. TWO OTHER SESSIONS are writing to
`/Users/nareshkhuriwal/Khuriwalgroup/kgf-nexus` on branch `f61-multi-queue-dispatch`.
NEVER read-modify-write files in that directory. Stay in this worktree.

Session 2 is actively building CRON SCHEDULING bottom-up (migration 0006,
internal/store/wfm_schedules.go). DO NOT touch scheduling: no `schedules` tables,
no cron REST routes, no scheduling UI. That is their slice.

## Files you own
- `services/migrations/0007_*.sql` (new; 0001-0006 are taken)
- `services/api-gateway/authn/**`, `services/api-gateway/rest/admin*.go`
- `internal/store/gateway_identity*.go` (new files only; do not rewrite
  gateway_apikeys.go — extend alongside it)
- `ui/src/pages/admin/**`, `ui/src/pages/LoginPage.tsx`, `ui/src/api/auth.ts`
- `ui/src/App.tsx` — you may edit; no other session has it open.

## Frozen contract (CONTRACT.md) — read it first
Migrations are ADDITIVE ONLY and must pass `internal/migrations` safety checks
(ADR-0011 rule 5): no full-table backfill in the same txn as DDL, no column drops.
Run the migration-safety test before committing.
Table ownership is strict per ADR-0011: api-gateway OWNS `api_keys` and
`idempotency_keys`. Your new identity tables belong to api-gateway. Migration
0005 installs per-service DB roles — add grants for new tables there.

## Current state (verified, do not re-derive)
- Stack is LIVE: gateway `http://localhost:8090` (healthz 200, 401s correctly),
  UI on `:3000`, postgres on `:55433` (compose) / `:55432` (bare test DB), 15 containers up.
- `make seed` mints a dev API key; `NEXUS_API_KEY` env var is the convention.
- Auth today = static API keys, sha256 at rest, 3 roles (admin/worker/reader),
  one enforcement point: `Principal.Authorize` + the `guard()` wrapper in
  rest/api.go. Every route is registered through `guard`, so a route cannot
  exist without an authz decision. PRESERVE that property.
- `api_keys` columns: id, namespace, name, key_hash, role, created_at, revoked_at.
  No expiry, no scopes, no last_used_at.
- UI "auth" = paste an API key into localStorage (App.tsx). 1,900 lines of
  React 18 + Vite + TanStack Query + react-router-dom v6 already exist:
  RunList, RunDetail, Workers pages; DagView, AttemptDetail, LogTail,
  EventTimeline components. IMPROVE, never duplicate or rewrite these.

## Deliver (vertical slice, in this order)
1. Migration `0007_identity`: users, sessions, scoped tokens (expires_at,
   scopes, last_used_at), audit_log. Additive. Grants in the 0005 style.
2. Store layer + tests: password hashing (argon2id/bcrypt for PASSWORDS —
   note gateway_apikeys.go explains why plain sha256 is right for high-entropy
   API keys and wrong for passwords; respect that distinction), session
   create/resolve/revoke, audit append (append-only, like run_events).
3. Gateway: session login/logout/refresh, real RBAC extending the existing
   `permitted` matrix, admin routes for users/tokens/audit/quotas. API keys
   MUST keep working unchanged — CLI and workers depend on them.
4. UI: LoginPage, session-backed auth replacing localStorage key paste (keep an
   API-key escape hatch for CLI parity), `/admin/*` pages for users, tokens,
   audit history. Honest loading/empty/error/permission-denied states.

## Non-negotiable
- Never expose secret values in definitions, URLs, logs or bundles. Tokens are
  shown exactly once at creation.
- Authn failures stay ONE opaque 401 — never an oracle for probing valid keys.
- A DB outage is 503, not 401 (see authn.go rationale).
- Every UI control must hit a real authorized API with a tested effect. No mock
  data, no unwired buttons.
- `go build ./...`, `go vet ./...`, `npm run typecheck`, `npm test` must pass.
- Do NOT claim production-readiness. Report what you verified and what you did not.

## Reference patterns (borrow narrowly, do not adopt frameworks)
Airflow RBAC + Connections/Variables for secret references; Argo's
ServiceAccount-scoped tokens. ADR-0009 rules out Ray. Do not claim parity with
any of them.
