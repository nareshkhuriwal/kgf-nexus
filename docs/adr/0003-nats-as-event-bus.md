# ADR-0003: NATS + JetStream as event bus, never as source of truth

- **Status:** Accepted (implemented; see "Implementation" below)
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

Two needs that look similar and are not:

1. **Dispatch latency.** With poll-only workers, a task waits up to one poll interval before starting. At a 2s interval a 20-node sequential DAG accrues ~40s of pure scheduling latency. Unacceptable for interactive workloads.
2. **Event fan-out.** The UI wants live run updates; notification systems want state-change hooks; external consumers want an integration point. Polling the API from every consumer does not scale.

The hazard is the Celery failure mode: treating the broker as workflow state. Then broker loss is data loss, queue depth becomes the only view of pending work, and there is no durable answer to "what is the state of this run."

## Decision

**NATS with JetStream, for notification and fan-out only. The bus is never the source of truth.**

Two subject families:
- `task.dispatch.<queue>` — core NATS (at-most-once, no persistence). A hint that work exists. Payload is an attempt ID, not the task itself; the worker still calls `PollTask` to obtain the authoritative assignment and lease.
- `run.events.<namespace>.<run_id>` — JetStream (persistent, replayable) for UI live updates, notifications, and audit fan-out.

**Load-bearing invariants:**
- Dispatch notifications are published **after** the claim transaction commits (§5.3). Never inside.
- A lost notification costs **latency only**. Workers poll on a fallback interval, and the reconciler sweeps for `PENDING` attempts with no lease.
- NATS being wholly unavailable degrades the system to poll-only. It does not stop it.
- Nothing reads state *from* the bus. `run_events` in Postgres is the authoritative history; JetStream carries a projection.

## Implementation (2026-09)

What was built, and where it deliberately differs from the decision above:

- **Transactional outbox, not publish-after-commit** (migration 0016). Every announced fact writes an `outbox` row in the same SQL statement as the fact, and the **event-service** relays it. Publishing after commit from the writer loses the message whenever the process dies between COMMIT and publish; the outbox cannot. The relay lives in the event-service, not the scheduler, so a broker problem can never touch a claim transaction.
- **Dispatch hints go through JetStream too**, into a small in-memory stream (`NEXUS_DISPATCH`, 2-minute max age), for the publish ack and `Nats-Msg-Id` de-duplication. Workers still receive them with plain core subscriptions, so the at-most-once, no-consumer-state semantics above hold for them.
- **Subjects carry the namespace**: `nexus.task.dispatch.<ns>.<queue>`, `nexus.run.events.<ns>.<run_id>`, `nexus.run.state.<ns>.<run_id>` (stream `NEXUS_RUNS`, 7 days, 10-minute duplicate window). Tokens are escaped so a name cannot become a wildcard.
- **The hint means "work may exist", not "attempt X was dispatched to you".** Dispatch is pull (ADR-0005): the worker that claims an attempt receives it in the same `PollTask` response, so the useful signal is that dispatchable work appeared. It is written when an attempt becomes `PENDING` (published at its `scheduled_at`), and the relay re-announces every queue with pending work every 10s so transitions no outbox row covers are still hinted. The scheduler's push dispatch loop stays disabled.
- **Delivery is effectively-once into the stream, at-least-once beyond it, unordered.** Consumers order run events by `seq` and run state by `state_version`.
- `NEXUS_NATS_URL` empty disables the bus; nothing needs NATS to start. Operating it: docs/OPERATIONS.md §11.

## Consequences

**Positive**
- Sub-second dispatch latency without push connections to workers or the trade-offs of ADR-0005.
- A natural integration seam for external consumers.
- JetStream replay lets a UI client that reconnects catch up without a full refetch.
- Bus failure is a latency event, not an outage. This property is the entire reason the design is shaped this way.

**Negative**
- A second piece of infrastructure to run. Mitigated: NATS is a single small static binary, and it is *optional* in Phase 1 (poll-only MVP), so it does not add MVP setup burden.
- Events exist in two places (Postgres and JetStream), so the projection can lag. Acceptable because Postgres is unambiguously authoritative.

**Neutral**
- JetStream retention is a tuning decision (proposed 7 days), independent of Postgres retention.

## Alternatives considered

**Postgres `LISTEN/NOTIFY`.** Attractive — no new infrastructure. Rejected: notifications are not durable, payloads are capped at 8 KB, every listener holds an open connection (poor at fleet scale), and it does not survive failover. Fine for a small deployment; not a foundation.

**Kafka.** Rejected: far heavier to operate, and its strengths (long retention, high-throughput log replay, partition-ordered streams) are not what we need. We want lightweight notification, not a durable log — Postgres already holds our log.

**Redis pub/sub.** Rejected: no persistence for the fan-out case, and a weaker operational story than NATS for this purpose.

**No bus (poll-only).** Simplest, and it is what Phase 1 actually ships. Rejected as the end state purely on dispatch latency.
