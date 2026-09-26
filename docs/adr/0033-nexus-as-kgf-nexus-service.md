# ADR-0033: Nexus as the KGF.Nexus service of KGF Cloud

- **Status:** Proposed — pending owner sign-off. Design only: no code is changed
  in either repository by this ADR.
- **Date:** 2026-09-26
- **Deciders:** owner (open decisions in KGF-INTEGRATION.md §14), platform team
- **Relates to:** ADR-0005, ADR-0011, ADR-0012, ADR-0014, ADR-0021; KGF Cloud
  ADR-0001, ADR-0002 and ADR-0005 (kgf-cloud: docs/adrs)
- **Detail:** [docs/KGF-INTEGRATION.md](../KGF-INTEGRATION.md)

## Context

Nexus will not be offered as its own hosted product. It will ship as a service of KGF
Cloud. KGF Cloud already owns the gateway and its signed request context
(`X-KGF-Context`), identity, the tenant directory (organizations,
subscriptions, resource groups, quotas), billing, policy, the provider
registry, the portal and audit export. KGF Cloud ADR-0005 (Accepted) lists
`KGF.Nexus` as a provider namespace. Today it is served by a generated
**simulation** (`kgf-cloud:services/nexus`) that stores pipeline definitions
and produces runs that do nothing, under a database CHECK that forbids
anything else.

The two systems overlap:

- **Both** have transactional outboxes, idempotency keys, ARM- versus REST-style
  APIs, per-tenant authorization, quotas, and an audit table.
- **Only Nexus** has a DAG engine, a lease-fenced dispatch claim, a worker
  protocol, and a per-attempt usage ledger.
- **Only KGF** has Temporal-based lifecycle workflows, forced row-level
  security by organization, a module-federated portal, and a registry.

## Decision (proposed)

1. **Replace the simulation's behaviour with the Nexus engine, and keep KGF's
   front door.**
   - `kgf-cloud:services/nexus` becomes a thin `KGF.Nexus` provider front. It
     keeps the ARM routes, `api-version`, `pkg/authz` enforcement, manifest
     registration and the S-08 conformance test, and translates each call to
     the Nexus API.
   - The Nexus control plane runs in the KGF platform cluster as the provider
     backend, with no public REST ingress.
2. **Workers stay Nexus workers.** They are customer-run or KGF-run agents
   that dial a dedicated Nexus gRPC endpoint with Nexus worker tokens and
   optional mTLS, **not** through the KGF gateway. The gateway's HTTP/JSON,
   1 MiB and 60 s limits are incompatible with ADR-0014's worker protocol.
   KGF operators issue and revoke those tokens through `KGF.Nexus` actions.
3. **Authentication is `X-KGF-Context`.**
   - The Nexus API verifies the Ed25519 context, requires `aud=kgf-nexus`,
     guards against replay by `jti`, and maps `org`/`sub` to a namespace and
     `role`/`obo` to a principal.
   - Human login, registration and signup are switched off in KGF mode.
   - The authorization decision is KGF's. Nexus keeps only the
     namespace-matches-principal check.
4. **Tenancy.** One Nexus tenant and namespace per KGF subscription, which is
   KGF's billing and quota boundary. The organization is recorded on the
   tenant. The resource group is a label and a path check.
5. **KGF owns policy values, Nexus enforces execution.**
   - Quota limits come from KGF and are reconciled into locked ADR-0021 quotas.
   - Usage leaves through the ADR-0021 ledger, pulled by an exporter into a
     new kgf-billing ingest route.
   - Audit rows go to KGF's audit export once KGF defines an ingest contract.
6. **Temporal and the DAG engine do not overlap.** Pipeline runs execute only
   in the Nexus engine and are never Temporal workflows. Temporal remains for
   KGF resource lifecycle, such as provisioning KGF-run worker pools, and for
   KGF workflows that wait on a Nexus run through its idempotent API.
7. **Defer overlapping features to KGF.** SSO/SCIM (ADR-0024), cost management
   and budgets (ADR-0023), audit sinks (ADR-0025), human credentials, signup
   and quota editing are not part of the KGF build. The unmerged branches that
   build them are an open decision (OPEN-10).
8. **The UI is a KGF portal extension** (`kgf_ext_nexus`) and uses only ARM
   routes. The standalone console is not deployed in KGF.
9. **No open-core tier split in this repository.** Tiering is a KGF product
   question (OPEN-9). This repository remains Apache-2.0.

## Options considered

| Option | Why not |
|---|---|
| A separate "Nexus Cloud" control plane | Rejected by the owner: it would duplicate KGF's gateway, identity, tenancy, billing and portal. (An earlier draft of this ADR number proposed it; it was withdrawn before merge.) |
| Keep the simulation and run Nexus beside it | Two `KGF.Nexus` truths. The simulation's contract (`simulation: true`) forbids real runs. |
| Run Nexus DAG runs as Temporal workflows | Two schedulers, retry policies and histories for one run. It discards Nexus's lease fencing and claim-time quota enforcement. |
| Serve ARM routes directly from the Nexus gateway (no front) | Viable, and it removes a hop. But it moves KGF conventions and conformance into this repository. Kept as OPEN-1. |
| Route workers through the KGF gateway | Protocol mismatch (gRPC, long polls, 4 MiB messages versus HTTP/JSON, 60 s, 1 MiB). |
| Namespace per resource group | Quotas, worker pools and the ledger are per namespace, and KGF's quota boundary is the subscription. |

## Consequences

**Positive.** KGF gets a real engine behind an existing, conformance-tested
contract. Nexus stops maintaining identity, signup, billing and audit export
of its own. Execution semantics (at-least-once, fencing, LOST, quotas in the
claim) are unchanged.

**Negative (the honest gaps; details in KGF-INTEGRATION.md §8–§9).**

- **Row-level security.** KGF requires forced RLS by organization. Nexus has
  `tenant_id` everywhere but no RLS, and its push claim, sweeps and GC span
  tenants by design. This needs its own ADR and a measurement.
- **Database roles.** Nexus services must stop running as the migration owner
  or server admin (F115 item 5; `fix/f115-chart-db-hardening`, unmerged).
- **Missing KGF contracts.** KGF has no usage-ingest route, no audit-ingest
  contract, and no per-type quota model. Each is new KGF work.
- **Simulation contract.** The KGF manifest, the simulation's CHECK
  constraints and the AKS smoke test all assert simulation and must change
  together.
- **Replay guards.** Both sides' replay guards are per process.
- **Audience check.** KGF's verifier accepts `aud="*"` despite its comment
  (kgf-cloud: pkg/tenantctx/tenantctx.go:287). Nexus should be stricter.

**Neutral.** No Nexus schema, API or wire change is made by this ADR. Each
merge step in KGF-INTEGRATION.md §13 carries its own change and tests.
