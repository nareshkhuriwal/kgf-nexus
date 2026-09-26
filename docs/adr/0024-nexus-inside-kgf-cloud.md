# ADR-0024: Nexus inside KGF Cloud — context authentication and tenancy mapping

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-26
- **Deciders:** platform team
- **Refines:** ADR-0012 (module permissions), migration 0007 (identity),
  migrations 0013–0015 (tenants and explicit tenant ownership)
- **Supersedes:** the first draft of this ADR (tenant OIDC/SAML SSO inside
  Nexus), withdrawn and reverted in `e3dc95c`: sign-in is KGF's
- **Closes:** CONTRACT-FINDINGS F136–F137

## Context

Nexus is a service of the KGF Cloud application (`kgf-cloud`), not a
standalone product. In KGF:

- **kgf-identity** owns people: sign-in, organization membership, role
  assignments, service principals, sessions and the access journal
  (`kgf-cloud/docs/kgf-identity.md`).
- **kgf-gateway** authenticates every request, resolves the organization,
  subscription and resource group, and forwards to the resource provider
  registered for the namespace with a freshly minted, signed **KGF context**
  in `X-KGF-Context`, deleting any context the caller sent
  (`docs/platform-services.md`, `services/gateway/internal/api`).
- The context format is `kgf-cloud/pkg/tenantctx`: a compact JWS,
  header `{"alg":"EdDSA","typ":"JWT","kid":…}`, claims
  `{org, sub, rg, principal, role, request_id, obo, obo_role, iss, aud, jti,
  iat, exp}`, Ed25519, lifetime ≤ 5 minutes (the gateway mints 1 minute),
  `kid = "kgf-" + hex(sha256(publicKey)[:6])`. The gateway publishes its public
  key at `/.well-known/kgf-context.json` as
  `{"keys":[{"kid","alg":"EdDSA","key":<base64url>}]}`.
- Roles are flat today: `operator`, `viewer`, `service`. KGF's built-in role
  definitions (`pkg/authz/builtin.go`) turn them into action sets: operator =
  Contributor (`*` minus the `KGF.Identity` grant actions), viewer = Reader
  (`*/read`, `*/list`). Actions are `KGF.<Namespace>/<type>/<verb>`; the
  Nexus manifest (`services/nexus/manifest.json`) declares namespace
  `KGF.Nexus` with `pipelines` (`read`, `list`, `write`, `delete`, `run`) and
  `pipelines/runs`.

Nexus meanwhile authenticates humans with its own passwords and sessions and
authorizes with its own namespace RBAC (ADR-0012). Two identity systems for one
person, two audit trails that cannot be joined, and two places to offboard.

## Decision

### 1. Three authentication modes

`NEXUS_AUTH_MODE` selects what the REST listener accepts. The worker gRPC
listener is unchanged in every mode (API keys and worker tokens).

| Mode | Humans | Machines (API keys, scoped and service-account tokens) | Password login, registration, email flows |
| --- | --- | --- | --- |
| `standalone` (default) | Nexus sessions | yes | registered |
| `kgf` | KGF context only | yes | **not registered**; Nexus sessions refused |
| `both` | KGF context or Nexus session | yes | registered |

Defaulting to `standalone` leaves every existing deployment unchanged.

**Credential confusion is refused, never resolved.** A request carrying both
`X-KGF-Context` and an `Authorization` header is refused with
`400 ambiguous_credentials` in `kgf` and `both` (the KGF gateway strips the
customer's `Authorization` when it forwards to a context-trusting provider, so
both together means the caller is not the gateway). In `standalone` a request
carrying `X-KGF-Context` is refused with `400 kgf_context_not_accepted`:
silently ignoring it would authenticate the caller as whatever bearer it also
sent while it believed it was acting as a KGF principal.

### 2. Verifying the context

`internal/kgfctx` mirrors `pkg/tenantctx` field for field and check
for check (Nexus cannot import kgf-cloud's module; a conformance test verifies
a token minted by kgf-cloud's own code):

- exactly three base64url segments, ≤ 8 KiB; header `alg` must be `EdDSA`;
  `kid` must name a trusted key; Ed25519 signature over `header.payload`;
- `Validate()` of the context (org and principal required; role one of
  operator/viewer/service; delegation only by a service, naming
  operator/viewer);
- `exp > now` and `iat ≤ now + 60 s` (tenantctx's window, unchanged);
- `aud` must equal `NEXUS_KGF_AUDIENCE` (default `kgf-nexus`, the name the
  gateway derives from the manifest id `nexus`). tenantctx also accepts the
  wildcard audience `*`; Nexus refuses it unless
  `NEXUS_KGF_ACCEPT_ANY_AUDIENCE=true`, because a wildcard token is replayable
  against every provider;
- a human principal (anything but a pure service context) only from an issuer
  in `NEXUS_KGF_USER_ISSUERS` (default `kgf-gateway`);
- `jti` not seen before within its lifetime (per-replica memory guard, as
  tenantctx does).

**Keys.** Either static (`NEXUS_KGF_CONTEXT_PUBLIC_KEYS`, base64url, kid
derived) or fetched from `NEXUS_KGF_CONTEXT_KEYS_URL`
(`…/.well-known/kgf-context.json`), refreshed every
`NEXUS_KGF_CONTEXT_KEYS_REFRESH` (5 min) and immediately on an unknown `kid`
(at most once per 30 s, so garbage kids cannot turn Nexus into a fetch
amplifier). A key that disappears from the document stays trusted for 5 minutes
(tenantctx's maximum token lifetime), so tokens minted just before a rotation
still verify. A fetched key whose `kid` does not equal its derived id is
ignored. A failed refresh keeps the previous keys; with none ever loaded every
context is refused (fail closed) and `/readyz` reports it.

### 3. Tenancy mapping

Each KGF **(organization, subscription, resource group)** maps to exactly one
Nexus tenant and namespace:

```
namespace = "kgf-" + hex(sha256("kgf-nexus/v1" 0x00 org 0x00 sub 0x00 rg))[:32]   (all three lower-cased)
```

provisioned on first use in one transaction: a `tenants` row (the registration
0013 performs, which the 0014 triggers require before any namespace-owned row)
and a `kgf_scopes` row recording the triple. Idempotent and safe under
concurrent first requests (`ON CONFLICT DO NOTHING`, then read back and compare
the triple). A namespace that exists but was not provisioned for that triple
is refused (`namespace_conflict`), never adopted.

*Why not tenant = organization, namespace = subscription/resource group.*
0013–0015 make a tenant and a namespace the same thing: `tenants.namespace` is
unique, every namespace-owned table resolves `tenant_id` from the namespace by
trigger, and scheduling fair share, quotas and every F101 ownership check are
per namespace. Letting one tenant own many namespaces would rewrite that
trigger family on every owned table for no isolation gain. So the Nexus tenant
is the finest KGF scope that owns workloads, the resource group, and the
organization is recorded beside it (`kgf_scopes.organization`) for
organization-level queries. The namespace is a hash rather than readable text
because it must be a Nexus identifier (`^[a-z0-9]([-a-z0-9_.]*[a-z0-9])?$`)
and must not collide when KGF names differ only in characters an identifier
cannot hold; `GET /v1/auth/whoami` and `kgf_scopes` give the readable triple.
The organization is inside the hash, so two organizations with the same
subscription and group names get different namespaces.

A context without subscription and resource group has no workload scope and is
refused (`403 kgf_scope_required`). The request path's `{ns}` must equal the
mapped namespace; the existing guard already refuses anything else, and F101's
per-object namespace checks are untouched because the principal is an ordinary
principal of that namespace.

### 4. Authorization mapping

The caller's effective role (the delegated person's for an `obo` context) is
turned into KGF's built-in action set exactly as kgf-gateway does:
operator → Contributor (`*`, minus the `KGF.Identity` grant actions), viewer →
Reader (`*/read`, `*/list`). Every Nexus permission a route requires is mapped
to one `KGF.Nexus/*` action; the Nexus principal holds the permission when the
role's action set covers the action. **Unmapped permissions are denied.**

| Nexus permission | KGF action |
| --- | --- |
| `workflows.view`, `overview.view` | `KGF.Nexus/pipelines/read` |
| `workflows.publish` | `KGF.Nexus/pipelines/write` |
| `workflows.delete` | `KGF.Nexus/pipelines/delete` |
| `workflows.cache.invalidate` | `KGF.Nexus/pipelines/cache/action` |
| `runs.create` | `KGF.Nexus/pipelines/run` |
| `runs.view`, `tasks.view` | `KGF.Nexus/pipelines/runs/read` |
| `tasks.logs.view` | `KGF.Nexus/pipelines/runs/logs/read` |
| `tasks.results.view` | `KGF.Nexus/pipelines/runs/results/read` |
| `runs.cancel` / `pause` / `resume` / `retry` | `KGF.Nexus/pipelines/runs/{cancel,pause,resume,retry}/action` |
| `runs.delete` | `KGF.Nexus/pipelines/runs/delete` |
| `schedules.view` | `KGF.Nexus/schedules/read` |
| `schedules.create`, `schedules.update` | `KGF.Nexus/schedules/write` |
| `schedules.delete` | `KGF.Nexus/schedules/delete` |
| `backfills.view` | `KGF.Nexus/schedules/backfills/read` |
| `backfills.create`, `backfills.update` | `KGF.Nexus/schedules/backfills/write` |
| `workers.view` | `KGF.Nexus/workers/read` |
| `workers.drain`, `workers.quarantine` | `KGF.Nexus/workers/{drain,quarantine}/action` |
| `infrastructure.view` / `.configure` | `KGF.Nexus/infrastructure/read` / `write` |
| `quotas.view` / `quotas.manage` | `KGF.Nexus/quotas/read` / `write` |
| `usage.view` | `KGF.Nexus/usage/read` |
| `notifications.view` / `.manage` | `KGF.Nexus/notifications/read` / `write` |
| `triggers.view`, `triggers.create` / `.update`, `triggers.delete` | `KGF.Nexus/triggers/read`, `write`, `delete` |
| `sensors.view`, `sensors.create` / `.update`, `sensors.delete` | `KGF.Nexus/sensors/read`, `write`, `delete` |
| `assets.view` | `KGF.Nexus/assets/read` |
| `audit.view` | `KGF.Nexus/auditEvents/read` |
| `roles.view` / `roles.manage` | `KGF.Nexus/roles/read` / `write` |
| `service_accounts.view` / `.manage`, `tokens.view` / `.manage` | `KGF.Nexus/serviceAccounts/read` / `write` |
| `users.view`, `users.manage` | **none** — KGF owns people |
| `audit.manage` | **none** — retention, legal hold, purge and sinks (ADR-0025) stay with the platform owner's Nexus machine credential, not a Contributor wildcard |

The mapping names actions beyond the ones the manifest declares today
(`pipelines/*`); kgf-cloud should declare them (OPERATIONS §24.6 lists them).
Until it does, a KGF custom role cannot name them individually, but the
built-in Contributor and Reader cover them by wildcard, which is what the flat
roles in contexts resolve to.

A **pure service context** (a KGF service acting for itself) is denied unless
its principal is listed in `NEXUS_KGF_SERVICE_PRINCIPALS`
(`svc:kgf-billing=viewer,…`), which then maps it to that role. tenantctx's own
rule ("the key is its authorization") is not copied: Nexus is least-privilege
by default and no KGF service needs Nexus today.

Coarse identity grants stay reserved to Nexus administrators for bound Nexus
principals (ADR-0012). For a KGF principal, `roles.manage`,
`service_accounts.manage` and `tokens.manage` follow the KGF role instead,
because in KGF mode there is no Nexus administrator: an operator manages the
Nexus-internal machine credentials of their resource group, bounded by the
delegation rule (a binding or token ceiling may hold only grants the operator
holds). `users.*` is never granted to a KGF principal.

### 5. Audit attribution

`audit_log` gains `request_id`. Every audit row written for a KGF principal has
`actor_kind = 'kgf'`, `actor_label` = the effective KGF principal,
`request_id` = the context's request id, and `detail.kgf` = organization,
subscription, resource group, role, token id, issuer and — for a delegated
context — the acting service. The response carries `X-Request-ID` with the
same value, so a KGF support reference resolves to Nexus rows with one index
lookup. Runs and specs created by a KGF principal record it as their actor.

### 6. RBAC that stays Nexus-internal

- **Service accounts** for agents and CI: non-human Nexus identities with role
  bindings; their tokens carry a permission ceiling and resolve to the
  account's current roles ∩ the ceiling. Disabling the account stops its tokens
  at once.
- **Users pagination:** `GET /users?limit=&cursor=&q=` is keyset-paginated.

Delegated user administration and cross-namespace owners are dropped: KGF owns
people and organization-level access.

## Consequences

**Positive.** One sign-in, one set of role assignments and one offboarding for
Nexus users inside KGF; audit rows join KGF's by request id; no Nexus password
exists for a KGF user; tenancy is derived, so KGF resource groups need no
Nexus-side onboarding.

**Negative.**

- Nexus trusts KGF's signing key completely for humans. A compromised gateway
  key is a compromise of every KGF-mode namespace (mitigated by the issuer and
  audience checks, not eliminated).
- Authorization follows KGF's flat role until contexts carry per-scope
  assignments; a KGF custom role scoped to `KGF.Nexus/pipelines/read` alone is
  enforced by kgf-gateway's coarse check but Nexus re-derives from the flat
  role.
- The replay guard is per replica (tenantctx's own limit); the audience and
  60-second lifetime bound the remaining window.
- Namespaces are opaque hashes; operators need `kgf_scopes` to read them.
- Tenant rows are created by traffic; nothing deletes one when KGF deletes the
  resource group (retention follows Nexus's existing rules).

**Neutral.** API keys, worker credentials and the worker protocol are
unchanged; standalone deployments are unchanged.

## Alternatives considered

- **Nexus as an OIDC relying party of kgf-identity** (the first draft of this
  ADR, reverted): a second sign-in surface and session store for the same
  people, and KGF's gateway would still sit in front.
- **Trusting headers from the gateway** (`X-KGF-Principal`): any network peer
  could set them; the signed context exists precisely to avoid that.
- **tenant = organization, namespace per resource group:** rejected in §3.
- **Readable namespaces** (`kgf-<org>-<sub>-<rg>`): collisions after
  identifier normalisation and names over 253 characters; the hash is total
  and fixed length.
