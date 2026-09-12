# ADR-0002: Postgres as metadata store and dispatch queue

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

The platform needs durable storage for specs, runs, nodes, attempts, workers, leases, reservations, timers, and an append-only event log. The dispatch path additionally needs a queue-like operation: "atomically claim the highest-priority pending task that fits an available worker."

A stated constraint is that we do not reinvent databases or brokers.

The critical requirement is that **dispatch must be atomic across four facts**: the attempt's state, the lease, the capacity reservation, and the emitted event. If these can diverge, the system either double-dispatches or leaks capacity. Coordinating that across a database *and* a separate queue system requires distributed transactions or an outbox with reconciliation — real complexity for a problem one database solves for free.

## Decision

**Postgres is the single source of truth, including the dispatch queue.** Claiming uses `SELECT ... FOR UPDATE SKIP LOCKED` (architecture §5.3).

Specifics:
- Partial indexes on non-terminal states keep hot-path queries bounded regardless of history volume (`WHERE state = 'PENDING'`).
- `run_events` and `task_attempts` partitioned by month so retention is a partition drop, not a mass `DELETE`.
- Optimistic concurrency via `state_version` columns for cross-transaction reads.
- A `timers` table as the durable deadline queue; no in-memory timer wheels.
- JSONB for specs, resource vectors, and event payloads: schema-flexible where the shape genuinely evolves, relational where we need constraints.

## Consequences

**Positive**
- Dispatch is one ACID transaction. No distributed-transaction problem, no outbox, no dual-write reconciliation.
- `SKIP LOCKED` lets N scheduler replicas share a queue without leader election or coordination. Scheduler replicas become disposable.
- One system to operate, back up, and monitor — a decisive advantage for an MVP.
- Rich querying for the UI and for operators without a separate analytics path.
- Postgres transactional semantics are well understood, which matters for code that must be correct under concurrency.

**Negative**
- Postgres is a throughput ceiling for dispatch. `SKIP LOCKED` handles thousands of claims/second on modest hardware, which covers our target envelope, but it is not a million/second queue. Recorded as open question §12.6 with an explicit migration path (a dedicated dispatch path in front of Postgres, with Postgres still the record of truth).
- High-frequency `UPDATE`s on `task_attempts` produce table bloat; needs autovacuum tuning and is an operational burden we accept knowingly.
- Long-running transactions in the scheduler could block vacuum. Mitigated by keeping claim transactions short — they contain no network I/O, which is why the NATS publish in §5.3 is deliberately *after* commit.

**Neutral**
- Requires real Postgres operational competence (connection pooling, replication, failover). This is commodity expertise.

## Alternatives considered

**Postgres for state + Redis/RabbitMQ/SQS for the dispatch queue.** The conventional split. Rejected: it makes the atomic four-fact dispatch impossible without an outbox pattern and a reconciler, adds a second system to operate, and introduces a class of state-drift bugs (message enqueued but transaction rolled back, or vice versa) that the single-database design simply does not have.

**etcd / Kubernetes CRDs (Argo-style).** Rejected: etcd is a coordination store, not a workflow database. Object size limits, heavy watch traffic at scale, and no real query capability for the UI.

**A purpose-built storage engine.** Rejected outright per the stated constraint, and correctly so.

**Cassandra/DynamoDB (Temporal-style, for scale).** Rejected for MVP: no multi-row transactions means the atomicity above must be hand-built via careful state-machine design. That is the right trade at Temporal's scale and the wrong one at ours. If we reach that scale, the event-sourced design in §4.2 makes the migration tractable.
