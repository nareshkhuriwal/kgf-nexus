# ADR-0010: Lease-based task ownership with fencing tokens

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

When the control plane dispatches an attempt to a worker, it must eventually answer: *is that worker still working on it?* The answer drives everything — whether to retry, whether to release reserved capacity, whether to accept a late-arriving result.

The naive approach is a boolean: mark the attempt `RUNNING` with a `worker_id`, and trust the worker to report back. It fails when the worker never reports — killed, OOMed, node lost, or partitioned. The attempt is `RUNNING` forever, its capacity is reserved forever, and the workflow stalls with no error.

The second approach is a timeout on the attempt: if no result arrives within N minutes, retry. This conflates two very different things — *the task is taking a long time* (legitimate for a 6-hour training job) and *the worker is gone* (needs immediate recovery). Choosing one N for both is impossible.

There is also a subtler failure that boolean ownership cannot address. A partitioned worker is not necessarily dead. It may still be executing, and may reconnect later and report success for an attempt the control plane already gave up on and retried. If that late report is accepted, the control plane's state is corrupted — potentially marking `SUCCEEDED` an attempt whose retry is still running, and releasing capacity twice.

## Decision

**Task ownership is a time-bounded lease with an opaque fencing token.**

1. **Lease creation is atomic with dispatch.** The `leases` row, the `reservations` row, the attempt's state change, the event, and the timers are one transaction (architecture §5.3). `UNIQUE(attempt_id)` on `leases` means at most one worker is *authorized* for an attempt at any instant.
2. **The lease carries an opaque token.** The worker must present it on `ExtendLease`, `ReportStarted`, `ReportProgress`, and `ReportResult`. A token that does not match the current lease is **rejected**.
3. **The worker renews continuously** while the task runs (`ExtendLease`), on a schedule well inside the lease TTL. Renewal is cheap and independent of task progress.
4. **Lease expiry is a durable timer**, not an in-memory timeout. On expiry: attempt → `LOST`, reservation released, capacity returned, event emitted, retry evaluated per ADR-0006 §6.5.
5. **Lease renewal is decoupled from task duration.** A 6-hour task renews its lease every few seconds for 6 hours. Liveness and progress are orthogonal concerns, which is precisely what the single-timeout approach gets wrong. Task-level progress is a *separate* signal (`heartbeat` timeout in the spec) for detecting a wedged-but-alive task.
6. **An `epoch` counter** increments per lease generation on an attempt, giving a monotonic ordering for debugging and for reasoning about which generation a report belongs to.
7. **Worker identity is per-session, not per-host.** A restarted agent re-registers with a new `worker_id` and can never reclaim its predecessor's leases.

## Consequences

**Positive**
- **No stuck attempts.** Every dispatched attempt either completes or has its lease expire. There is no state in which an attempt waits forever with no error — the failure mode that makes naive designs unusable in production.
- **Capacity cannot leak.** Reservation lifetime is bound to lease lifetime, and both are released by the same expiry path.
- **Zombie workers cannot corrupt state.** This is the load-bearing property. Fencing does not prevent a partitioned worker from *executing* — nothing can, over a network — but it guarantees the stale result is rejected rather than applied. Combined with idempotency (ADR-0006), which handles the duplicate *side effect*, the two mechanisms together cover both halves of the problem. Neither alone is sufficient, and this is the most common gap in home-grown orchestrators.
- **Long tasks and liveness are independent**, so a 6-hour task needs no special configuration.
- Lease expiry rate is an excellent single metric for fleet instability.

**Negative**
- Renewal traffic scales with concurrent tasks, not with fleet size. At 10k concurrent tasks and a 5s renewal that is 2k RPS of pure overhead. Mitigated by **batching renewals into the worker's heartbeat** — one RPC per worker carries renewals for all its attempts, which collapses the cost to per-worker. Worth building this way from the start rather than retrofitting.
- Lease TTL is a genuine trade-off: short TTL detects death fast but risks false expiry under transient network trouble (causing needless duplicate execution); long TTL is the reverse. Proposed default 30s TTL with 5s renewal — six missed renewals before expiry, tolerant of brief blips. Should be tunable per queue.
- A false expiry under a network blip creates real duplicate execution. This is the strongest practical argument for why idempotency is mandatory and not advisory.
- Clock discipline matters: all deadlines are computed from **control-plane time**, and worker clocks are never authoritative (architecture §10).

## Alternatives considered

**Boolean ownership (`worker_id` set, trust the report).** Rejected: stuck attempts and leaked capacity on any worker loss.

**Single attempt timeout.** Rejected: cannot distinguish a slow task from a dead worker, so no single value is correct.

**Heartbeat-only liveness, no per-attempt lease.** Simpler — one signal per worker, and on worker death expire all its attempts. Genuinely close to sufficient, and tempting. Rejected because it loses per-attempt fencing: without a per-attempt token, a worker that is alive but has *lost track of* one attempt (an executor crash, a WAL replay bug, a restarted container) can still submit a report for it, and the control plane cannot tell that report is stale. The per-attempt token closes that gap. We keep worker heartbeat *as well*, for fast fleet-level detection and for capacity reconciliation.

**Distributed lock manager (etcd/ZooKeeper leases).** Rejected: a new consensus system to operate, for a guarantee Postgres already provides transactionally. Leases must be atomic with the capacity reservation and the attempt state change, which are Postgres rows — putting the lease elsewhere reintroduces the dual-write problem ADR-0002 exists to avoid.
