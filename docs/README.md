# Nexus — design proposal (Phase 0)

A reusable, application-agnostic distributed workflow orchestration platform.
**This directory is a design proposal. There is no implementation code, by
intent** — Phase 0 exists to settle the architecture before anything is built.

## Contents

| Document | What it covers |
| --- | --- |
| [ARCHITECTURE.md](ARCHITECTURE.md) | The full design: prior-art assessment, component architecture, data model, state machines, execution semantics, failure matrix, phased plan |
| [adr/](adr/README.md) | Ten decision records, each with alternatives and the reason for rejection |

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

- Reviewing the whole proposal → [ARCHITECTURE.md](ARCHITECTURE.md) top to bottom.
- Just the decisions and trade-offs → [adr/README.md](adr/README.md).
- Correctness under failure → architecture §5 (state machines), §6 (semantics), §10 (failure matrix).
- Scope of the first build → architecture §11, Phase 1.

## Open questions

Six decisions are flagged for reviewers in architecture §12, including the
control-plane language, the priority model (single integer with aging vs.
weighted fair-share from the start — unpleasant to retrofit), and the measured
throughput at which Postgres-as-queue needs a dedicated dispatch path.
