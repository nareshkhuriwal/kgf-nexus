# Nexus — design and operations documentation

A reusable, application-agnostic distributed workflow orchestration platform.

This directory began as the Phase 0 design proposal, written before any code.
The design is now implemented in this repository and the documents here
describe the system as it is built, including the parts that are not finished.

## Current state

**Built:** six control-plane services sharing one PostgreSQL schema (API gateway,
workflow manager, DAG engine, scheduler, resource manager, event service), plus
artifact GC and a cluster controller; the Go worker agent and `nexus` CLI; the
Python SDK; the React operator console; MinIO-backed result storage; an optional
NATS JetStream relay fed by a transactional outbox; Docker Compose and Helm
deployment.

**Not production-ready.** The project has not passed its production acceptance
gates, and it should not be described as production-ready until it does. Known
gaps include:

- **Identity:** email + password sessions, API keys and scoped tokens exist. OIDC
  and SAML single sign-on, identity-provider group mapping and SCIM are planned
  but not implemented ([ARCHITECTURE.md §3.2](ARCHITECTURE.md#api-gateway),
  [RBAC-IMPLEMENTATION.md](RBAC-IMPLEMENTATION.md)).
- **Transport security:** inter-service TLS/mTLS is supported but opt-in, and
  plaintext is only fatal with `NEXUS_REQUIRE_TLS` set ([TLS.md](TLS.md)).
- **Authorization:** namespace-scoped module RBAC for human accounts is
  implemented. The broader model in [RBAC-DESIGN.md](RBAC-DESIGN.md) is the
  target, not the current behaviour.

Open defects, decisions and their verification live in
[CONTRACT-FINDINGS.md](../CONTRACT-FINDINGS.md). When a document here disagrees
with the code, treat the document as the bug.

## Contents

| Document | What it covers |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | The full design: prior-art assessment, component architecture, data model, state machines, execution semantics, failure matrix, phased plan |
| [adr/](adr/README.md) | Decision records, each with alternatives and the reason for rejection |
| [OPERATIONS.md](OPERATIONS.md) | Running a deployment: configuration, backup and restore, migrations and upgrades, artifact GC |
| [TLS.md](TLS.md) | Inter-service transport security and how to roll it out |
| [MULTITENANCY.md](MULTITENANCY.md) | Tenant and namespace isolation |
| [RBAC-DESIGN.md](RBAC-DESIGN.md) / [RBAC-IMPLEMENTATION.md](RBAC-IMPLEMENTATION.md) | The target authorization model, and what is implemented today |
| [CLUSTER-SCALING.md](CLUSTER-SCALING.md) | The cluster registry and controller |
| [THIRD-PARTY-LICENSES.md](THIRD-PARTY-LICENSES.md) | Dependency licences |
| [AIRGAP.md](AIRGAP.md) | Building, carrying and loading the air-gapped installation bundle |
| [SOC2-READINESS.md](SOC2-READINESS.md) | Nexus service controls and evidence for KGF Cloud's SOC 2 program (not an attestation) |

## The design in brief

**Two planes, one narrow contract.** The control plane (API gateway, workflow
manager, DAG engine, scheduler, resource manager, metadata store, event bus)
decides *what* runs *where*. The execution plane is a fleet of lightweight
agents that register, heartbeat, pull leased work, run it, and report results.
Three invariants make the boundary real rather than decorative: no user code in
the control plane, no database credentials in the execution plane, and no task
payloads transiting the control plane.

**Postgres is the source of truth, including the dispatch queue.** Dispatch is
one ACID transaction covering the attempt's state, its lease, its capacity
reservation, and its event — so N scheduler replicas can share a queue via
`SKIP LOCKED` with no leader election. NATS carries notifications only; losing
it costs latency, never correctness.

**At-least-once plus mandated idempotency.** Stated honestly: exactly-once
side effects are unachievable over a network, so we provide at-least-once and
make idempotency a platform requirement. The platform derives the idempotency
key (deliberately excluding the attempt number, so retries share one key), and
lease fencing rejects stale reports from zombie workers. Those two mechanisms
are two halves of one solution — see [ADR-0006](adr/0006-at-least-once-with-idempotency.md)
and [ADR-0010](adr/0010-lease-based-task-ownership.md) together.

**Learn, don't copy.** §2 of the architecture assesses Airflow, Prefect,
Temporal, Dagster, Argo, Flyte, Kestra, Celery, Ray, and Luigi, naming the
specific decision borrowed from each and the specific one rejected. The short
version: Flyte's typed, versioned, resource-aware task spec; Temporal's
event-sourced history and four-timeout model; Prefect's control-plane/user-code
separation; Airflow's operational surface; Celery's lessons about ack timing.
No Ray dependency ([ADR-0009](adr/0009-no-ray-dependency.md)); no reinvented
databases or brokers.

## Where to start

- Running it locally → [deploy/README.md](../deploy/README.md).
- Operating a deployment → [OPERATIONS.md](OPERATIONS.md).
- The whole design → [ARCHITECTURE.md](ARCHITECTURE.md) top to bottom.
- Just the decisions and trade-offs → [adr/README.md](adr/README.md).
- Correctness under failure → architecture §5 (state machines), §6 (semantics), §10 (failure matrix).

## Questions raised by the proposal

Architecture §12 lists the questions the Phase 0 proposal left open. Several are
now addressed by later ADRs, which are implemented but still marked *Proposed —
pending owner sign-off*: for example the control-plane language
([ADR-0013](adr/0013-go-control-plane-and-agent.md)), weighted fair share
across namespaces ([ADR-0017](adr/0017-weighted-fair-share-across-namespaces.md))
and the Postgres dispatch ceiling ([ADR-0018](adr/0018-postgres-dispatch-ceiling.md)).
