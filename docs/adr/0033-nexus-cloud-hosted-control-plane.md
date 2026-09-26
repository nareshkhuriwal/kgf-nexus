# ADR-0033: Nexus Cloud — a hosted, cell-based control plane with customer-run workers, and an open-core split

- **Status:** Proposed — pending owner sign-off. **Design only:** no code, no
  infrastructure, nothing deployed.
- **Date:** 2026-09-26
- **Deciders:** owner (the ten open decisions in CLOUD-ARCHITECTURE.md §15), platform team
- **Builds on:** ADR-0003, ADR-0005, ADR-0006, ADR-0011, ADR-0017, ADR-0018, ADR-0021
- **Detail:** [docs/CLOUD-ARCHITECTURE.md](../CLOUD-ARCHITECTURE.md)

## Context

The repository ships a self-hosted product under Apache-2.0. A hosted offering
has been discussed but not designed. What the code already establishes:

- The control plane never runs user code, workers hold no database credentials,
  and task payloads go from agent to object store, not through the control
  plane (ARCHITECTURE.md §3.1). Workers dial out and pull their work (ADR-0005).
  Losing the event bus costs latency, never correctness (ADR-0003).
- Tenancy is application-enforced and logical, on one shared database:
  `tenant_id` foreign keys on every customer-owned table, namespace
  authorization on every route (F101), and no row-level security
  (MULTITENANCY.md).
- A tenant's share of dispatch is bounded by fair share (ADR-0017) and by exact
  quotas (ADR-0021). But dispatch throughput is one shared Postgres resource with a
  measured, backlog-dependent ceiling (ADR-0018; laptop figures in
  [BENCHMARKS.md](../BENCHMARKS.md)).
- Usage is recorded in an append-only ledger, one row per dispatched attempt,
  and exported on `nexus.usage.recorded.<namespace>` (ADR-0021).
- Worker identity is a bearer credential (scoped `nxtok_` token or API key,
  F114). mTLS client certificates are supported, but the certificate's identity
  is **not** checked against the tenant (TLS.md §5.6). NATS identities are per
  tenant and subscribe-only (OPERATIONS §11.7).
- Rate limits are per gateway replica and held in memory (OPERATIONS §16.6).
  Signup controls are one registration per email plus email verification.

## Decision (proposed)

1. **Topology.** Nexus Cloud hosts the control plane and console. Customers run
   agents in their own networks with **outbound-only** connectivity: gRPC over
   TLS to the gateway, and optionally NATS over TLS for hints. Task code never
   runs on vendor infrastructure. Vendor-run workers are out of scope.
2. **Isolation: cells.** A cell is a complete, independent control plane (its own
   PostgreSQL, NATS and services). Each tenant belongs to exactly one cell, and
   a thin global layer routes to it. Launch with one cell per region. That is
   today's shared model, with the router in place so the service can split
   without a redesign. **Dedicated control planes** (a cell of one) are the
   enterprise option. Cell size comes from a production-class run of the
   ADR-0018 harness, not from laptop numbers.
3. **Identity.** Each tenant gets its own CA (an intermediate under an offline
   root). The gateway **must** match the certificate's tenant against the
   credential's namespace before shared cells accept any external tenant. That
   closes TLS.md §5.6 for workers. Agents enrol with a one-time token and a
   locally generated key, and use short-lived leaves.
4. **Residency.** A tenant's region is chosen at signup. All run metadata,
   inline results, logs held by the vendor, usage rows and backups stay in the
   region. Global services hold only routing and billing aggregates. A per-tenant
   inline threshold and bring-your-own bucket keep result bytes in the
   customer's network.
5. **Metering and plans.** Build on the ledger and the export hook unchanged. A per-cell
   forwarder deduplicates on `attempt_id`, and only aggregates leave the region.
   Every Cloud namespace is created with **plan quotas, locked**. The
   self-hosted "absent means unlimited" default does not apply in a shared
   cell.
6. **Reliability targets** are proposed, not promised: SLOs computed from
   metrics that already exist, per-cell error budgets, and backup/DR built on the
   drilled pgBackRest and logical-backup paths or on managed-provider PITR, with a drill
   in the real environment before any figure goes into a contract.
7. **Open core.** Whatever one team needs to run Nexus correctly and safely
   stays Apache-2.0. Multi-team governance features and vendor-only services
   become enterprise or cloud-only. They live in a separate `ee/` Go module
   behind interfaces the core defines, with its own migration stream, so the
   core builds and runs without it. **The licence of the features now on
   unmerged branches (SSO, cost management, audit export, multi-cluster) must
   be decided before they merge,** because released Apache-2.0 code cannot be
   withdrawn.

## Options considered

| Option | Why not chosen as the default |
|---|---|
| **Single shared control plane forever** (logical isolation only) | One database outage, bad migration or missed `tenant_id` filter hits every tenant. The dispatch ceiling is shared by all. It stays as the launch state *with* the router. |
| **Dedicated control plane for every tenant** | Cost and operational load scale with tenant count. Right for enterprise, wrong for a free tier. |
| **Vendor-run workers (full SaaS execution)** | Needs hostile-code sandboxing the platform does not have. It also contradicts the "your code and data stay with you" position that the pull model makes cheap. |
| **Database-per-tenant inside a shared cell** | It isolates data but not the services, and multiplies migrations and connections. Cells give most of the blast-radius benefit at far lower cost. |
| **Relicense the core** (e.g. BSL or ELv2) instead of open core | It needs every contributor's consent, and there is no CLA today. It also discards the trust the permissive licence buys. Kept as an open question, not the default. |
| **Build-tag enterprise code inside the core module** | It mixes licences in one module and makes an accidental import from core easy. A separate module makes the boundary mechanical. |

## Consequences

**Positive.** The hosted design reuses the architecture's existing boundaries
rather than inventing new ones. Blast radius is bounded by cells. The billing
hook exists and is correct by construction (ADR-0021). The self-hosted product
loses nothing.

**Negative.** Real work before any external tenant can be accepted
(CLOUD-ARCHITECTURE.md §14): certificate-to-tenant binding, a shared rate
limiter and abuse controls, locked plan quotas, per-tenant result store
configuration, a cell router, tenant suspension and deletion, and the billing
forwarder. Cells multiply deployments. An `ee/` split means refactoring branches that were
built directly in the core tree. Some customer data (specs, parameters,
inline results, error text, and logs unless BYO) is necessarily held by the vendor,
which the data-protection position must state.

**Neutral.** No schema, API or wire change is made by this ADR. Each item in
§14 that changes them gets its own ADR.

## Open decisions (owner)

OPEN-1 isolation per tier and cell size · OPEN-2 billing unit and plan limits ·
OPEN-3 regions and provider · OPEN-4 BYO-bucket log access · OPEN-5 NATS
identity at scale · OPEN-6 which features are enterprise (SSO in particular) ·
OPEN-7 enterprise licence, repository layout, CLA · OPEN-8 first compliance
framework · OPEN-9 managed workers ever · OPEN-10 SLO targets and SLA
exclusions. Each is in CLOUD-ARCHITECTURE.md §15 with a proposed default.
