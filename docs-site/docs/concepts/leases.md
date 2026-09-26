# Leases and at-least-once

Workers **pull** work. The control plane never opens a connection to a worker
([ADR-0005](../design/adr/0005-pull-based-dispatch.md)). An agent registers
over gRPC, heartbeats, and calls `PollTask` with its free capacity. NATS
notifications only shorten the wait between polls. If the broker is lost,
latency suffers but correctness does not.

## The atomic claim

Dispatch is **one PostgreSQL transaction**. It covers the attempt's state
change, its lease, its resource reservation, its event and its timers. Scheduler
replicas share the queue through `FOR UPDATE SKIP LOCKED`, with no leader
election ([architecture §5.3](../design/architecture.md#53-dispatch-the-atomic-claim)).
`UNIQUE(attempt_id)` on leases means at most one worker is authorized for an
attempt at any instant.

## Fencing

The lease carries an opaque token ([ADR-0010](../design/adr/0010-lease-based-task-ownership.md)):

- The worker presents the token on `ExtendLease`, `ReportStarted`,
  `ReportProgress`, `ReportResult` and `ReportLogs`. A token that does not
  match the current lease is **rejected**. So a "zombie" worker that was
  partitioned away and later comes back cannot overwrite the outcome of the
  retry that replaced it, or add to its log.
- The worker renews the lease continuously, independent of task progress. A
  six-hour task renews every few seconds for six hours. Progress is a separate
  signal: the task's `heartbeat` timeout detects a task that is alive but
  wedged.
- Lease expiry is a **durable timer**, not an in-memory timeout. On expiry the
  attempt becomes `LOST`, its reservation is released, and the
  [retry policy](retries-idempotency.md#lost-attempts) decides what happens.
- A restarted agent registers under a new worker id and can never reclaim its
  predecessor's leases.

## The worker's write-ahead log

Before it reports, the agent fsyncs each attempt's start and outcome to a
local WAL ([architecture §6.3](../design/architecture.md#63-the-worker-write-ahead-log)).
After a restart it re-reports completed outcomes (the server treats this
idempotently). Attempts that started with no recorded outcome are reported as
**indeterminate**, never guessed. Keep `NEXUS_WAL_DIR` on durable storage.

## Draining and quarantine

- `nexusctl worker drain <id>`: the agent takes no new work, finishes what it
  has (up to `--drain-grace`, default 5 minutes), then exits.
- `nexusctl worker quarantine <id> --reason …`: removes the worker from
  placement without stopping it. The resource manager can also quarantine
  workers automatically ([runbook §14.3](../operations/runbook.md#143-worker-quarantine-55)).

## What this adds up to

Duplicate **execution** is possible: a zombie can run a task at the same time
as its retry. Duplicate **state** is not: only the current lease holder's
report counts. That is the at-least-once contract, and the reason
[idempotency keys](retries-idempotency.md#idempotency) are mandatory.
The chaos suite (`make chaos`) kills workers mid-task and checks for exactly
one logical effect.
