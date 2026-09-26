# Nexus in KGF Cloud

Nexus is the workflow engine behind the **`KGF.Nexus`** service of KGF Cloud,
not a separately hosted product. KGF Cloud owns everything around the engine:
the gateway and its signed request context, identity, the tenant directory
(organizations, subscriptions, resource groups), billing, policy, the provider
registry and the portal. Nexus provides the DAG engine, dispatch, the worker
agents and their protocol.

!!! note "Status"
    The Nexus side of the integration -- context authentication, the tenancy
    mapping, KGF-attributed audit export -- is implemented. The KGF side (the
    provider front that replaces the simulated `KGF.Nexus`, the usage and
    audit ingest contracts, the portal extension) is a design, not code. See
    [ADR-0033](../design/adr/0033-nexus-as-kgf-nexus-service.md) and the
    [integration design](../design/kgf-integration.md).

## Authentication: the KGF context

With `NEXUS_AUTH_MODE=kgf`, people reach Nexus only through the KGF gateway,
which authenticates them and forwards each request with a freshly signed
**`X-KGF-Context`**: an Ed25519 token naming the organization, subscription,
resource group, principal, role and request id. Nexus verifies it exactly as
KGF's own `tenantctx` does -- signature against the gateway's published keys,
audience `kgf-nexus` (the wildcard audience refused), lifetime, the issuers
allowed to assert a person, and replay -- and refuses a request that also
carries an `Authorization` header. Nexus's own sign-in, registration and
email flows are not served in that mode; machine credentials (API keys,
scoped and service-account tokens) and the worker protocol are unchanged.
`both` accepts either, for migrating a standalone installation;
`standalone` (the default) changes nothing.
[ADR-0024](../design/adr/0024-nexus-inside-kgf-cloud.md) is the design;
[runbook §24](../operations/runbook.md#24-running-nexus-inside-kgf-cloud-context-authentication-tenancy-mapping-and-service-accounts)
has the configuration.

## Tenancy: one namespace per resource group

Each KGF **(organization, subscription, resource group)** maps to exactly one
Nexus tenant and namespace, derived from the triple by hash and provisioned
on the first request for it; `GET /v1/auth/whoami` shows the readable triple.

A resource group is KGF's unit of isolation, lifecycle and access, and every
Nexus guarantee -- tenant ownership, per-object checks, fair share, quotas,
the audit chain, worker identities -- is per namespace, so each holds at that
scope with nothing added. The costs: KGF's subscription-level quota is divided
among its resource groups' namespaces by the provider front, and a worker pool
serves one resource group
([integration design §6](../design/kgf-integration.md#6-tenancy-mapping)).

## Authorization

The context's role is KGF's: `operator` (Contributor) or `viewer` (Reader).
Nexus maps every permission it checks to one `KGF.Nexus/*` action and grants
it when the role's built-in action set covers it; anything unmapped is denied.
A KGF role never grants user administration (KGF owns people) or audit
retention, legal hold and sinks (`audit.manage`, which stays with the
platform owner's Nexus credential). KGF services acting for themselves are
refused unless listed in `NEXUS_KGF_SERVICE_PRINCIPALS`.

Agents and CI keep Nexus-internal **service accounts**: non-human identities
with role bindings, whose tokens carry a permission ceiling, managed by a KGF
operator within their own grants.

## What Nexus keeps, and what moves to KGF

| Capability | Where |
| --- | --- |
| Quota enforcement in the claim, and shared provider rate limits | Nexus; KGF sets the limits ([quotas](quotas.md)) |
| Usage metering (one ledger row per attempt, with AI usage units) and its export | Nexus exports; KGF billing rates it |
| Price books, budgets, cost reporting | **KGF billing** -- held out of Nexus ([runbook §23](../operations/runbook.md#23-cost-management-moved-to-kgf-billing)) |
| Audit trail: hash chain, export in KGF's audit-event shape, signed sinks | Nexus, attributed to the verified KGF context ([runbook §25.2](../operations/runbook.md#252-the-audit-trail-pagination-export-kgf-cloud-sinks-retention-hash-chain)) |
| Sign-in, people, organizations | KGF identity |
| Secrets for tasks | Nexus workers, from the platform Key Vault through workload identity ([secrets](../recipes/secrets.md)) |
