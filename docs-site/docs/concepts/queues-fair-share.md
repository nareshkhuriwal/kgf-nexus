# Queues and fair share

## Queues route work

A task names a `queue` (`default` if omitted). A worker serves the queues in
`NEXUS_QUEUES`, for example `default,gpu`. The scheduler hands a worker only
attempts from its queues that also match its executors, labels
(`node_selector` is a hard constraint on worker labels) and free capacity.
Use queues to separate pools that differ in hardware, network access or
trust, not to express priority.

## Order within a namespace

Within a queue and namespace, pending attempts are tried in order of
**effective priority** (the task or run priority plus the
[aging term](resources.md#head-of-line-blocking-and-aging)), then by
`scheduled_at`. `max_active_runs` (per workflow) and `concurrency_key` /
`concurrency_limit` (a global semaphore across runs) cap parallelism
independently of capacity.

## Weighted fair share across namespaces

When several namespaces share a queue, the scheduler tries the namespace with
the smallest **share** first. A namespace's share is its in-flight attempts
divided by its weight ([ADR-0017](../design/adr/0017-weighted-fair-share-across-namespaces.md)):

- `fair_share_weight` is 1 to 100, default 1, in the namespace's platform
  configuration (console: Cluster & scaling → Scheduler). Changing it needs a
  **platform operator**, because raising your own weight takes share from
  every other namespace.
- Ties between namespaces are broken by a salted hash that rotates each cycle,
  so the same namespace does not always win.
- Fair share only changes which unlocked `PENDING` row is tried first.
  Dispatch stays one atomic claim, so fairness can be off by at most one
  dispatch, and correctness is unaffected.
- `NEXUS_FAIR_SHARE=false` on the scheduler restores pure priority order. It
  is the rollback switch.

## Dispatch notifications

Workers long-poll. When NATS JetStream is configured, a transactional outbox
publishes dispatch hints that cut the wait between polls. Losing the broker
costs latency, never work: PostgreSQL remains the queue, and reconciliation
catches up ([ADR-0003](../design/adr/0003-nats-as-event-bus.md),
[runbook §11](../operations/runbook.md#11-the-event-bus-nats-jetstream)).

How far one PostgreSQL queue goes is measured, with its limits stated, in
[ADR-0018](../design/adr/0018-postgres-dispatch-ceiling.md). Those are laptop
measurements, not production capacity figures.
