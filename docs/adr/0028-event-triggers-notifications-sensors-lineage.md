# ADR-0028: Notifications, inbound webhook triggers, sensors and a first asset-lineage layer

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0003 (the outbox is the event source), ADR-0006 (at-least-once, keyed
  idempotency), ADR-0011 (table ownership; this record amends its matrix)
- **Delivers:** ARCHITECTURE.md §11 Phase 3 "event-driven triggers (webhook, bus, sensor
  tasks)" and a minimal version of Phase 4's "asset/lineage layer over the task core"
- **Findings:** CONTRACT-FINDINGS F142 (no budget/quota fact to notify on), F143
  (schedules ignored their pinned `spec_version`)

## Context

Four features were requested together because they share one question: **how does
something outside a run's own DAG react to a fact, durably and exactly once in effect,
with N replicas and no leader?**

1. **Notifications.** Operators want Slack, Teams, PagerDuty, signed webhooks and email
   when a run fails, succeeds, times out or overruns an SLA, and when an attempt is LOST.
2. **Inbound webhook triggers.** External systems (CI, a SaaS, a data vendor) want to
   start a run by POSTing a signed payload.
3. **Sensors.** A run should start when data lands in an object-store prefix, or when
   another workflow's run succeeds.
4. **Asset lineage.** Tasks should declare what they read and write, so an operator can
   ask "who produces this table, who reads it, and when was it last built".

What the code already establishes:

- **Every fact these features react to is already announced transactionally.** A run
  state change writes a `run_state` outbox row in the same statement as the transition
  (`WFMTransitionRun`), and every run event — `AttemptLost`, `AttemptSucceeded`,
  `NodeStateChanged` — writes a `run_event` outbox row in the same statement as the
  event (`appendEvent`, event-service `Append`). The run-state `RunStateChanged` *event*
  is emitted afterwards in a separate call and can be lost; the outbox row cannot.
- **The outbox rows are not a durable queue for a second consumer.** The relay marks a
  row published and the janitor deletes it an hour later, and deletes rows it never
  published after 24 h. With NATS disabled, nothing is ever marked published. A consumer
  that tracked "the highest id I have seen" would skip rows whose transaction committed
  late: ids are assigned at INSERT and become visible at COMMIT, in a different order
  (the argument in migration 0016).
- **Schedules already solve durable, multi-replica firing** without leader election:
  due work is claimed `FOR UPDATE SKIP LOCKED`, the tick's identity is a unique row, and
  the run's idempotency key is derived from the tick, so a race converges on one run.
- **There is no budget or quota fact to notify on.** A run-quota refusal is a synchronous
  429 at run creation and a claim-time quota is a pending reason. Neither writes a
  durable row or an outbox topic (F142).
- **There is no secret-encryption facility.** Nothing in the tree encrypts at rest; the
  spec's `SecretRef` is a *reference* resolved on the worker.

## Decision

### 1. The outbox is the event source, and a second marker makes it durable for notifications

Migration 0034 adds `outbox.notified_at`. The **notifier** (event-service, beside the
relay) claims `run_state` and `run_event` rows where `notified_at IS NULL`, `FOR UPDATE
SKIP LOCKED`, evaluates them, writes its outputs and sets `notified_at` — **all in one
transaction**. The janitor no longer deletes a published row until it has also been
notified. So:

- Every announced fact is evaluated **at least once** (a crash before COMMIT leaves the
  row unmarked) and its outputs are written **exactly once** (they commit with the mark).
- The notifier needs **neither NATS nor the relay**: with the bus disabled the rows are
  still written, still evaluated, and still bounded by the 24 h expiry.
- The relay and the notifier lock the same rows with `SKIP LOCKED` on different
  predicates; neither ever waits on the other.

The same pass records **asset lineage** (below), because it is the one place that sees
every run start and every node success exactly once in effect.

A rule only matches facts that happened after the rule was created
(`outbox.created_at >= rule.created_at`), so a notifier catching up on a backlog does not
page anyone about history that predates the rule.

**SLA overruns are not facts** (nothing happens when a run is late), so a separate sweep
in the notifier compares active runs against rules with `sla_seconds` and enqueues
`run.sla_exceeded` under the event key `sla:<rule>:<run>`. The unique key makes the sweep
idempotent across replicas and passes.

### 2. Deliveries are their own durable queue, with a dedup key per (event, channel)

`notification_deliveries` is a queue in the outbox style, owned by the event-service:

- `UNIQUE(channel_id, event_key)`. The event key is derived from the fact
  (`run_state:<run>:<state_version>`, `run_event:<id>`, `sla:<rule>:<run>`), so two rules
  that match one event on one channel produce **one** delivery, and a re-evaluated row
  inserts nothing new.
- A dispatcher claims due rows with a **lease** (`UPDATE … SET lease_until = now()+30s`
  over a `SKIP LOCKED` sub-select), sends them **outside** any transaction, then records
  the outcome under a fence on its own lease. A crash mid-send leaves the lease to
  expire and the row is sent again. That is **at-least-once**, so every send carries the
  delivery's external dedup key: `Idempotency-Key` and `X-Nexus-Delivery` headers on
  webhooks, `dedup_key` on PagerDuty. Slack, Teams and SMTP have no dedup primitive; a
  crash in the few hundred milliseconds between their acknowledgement and our COMMIT can
  duplicate one message. This is stated, not hidden (ADR-0006).
- **Retries** use exponential backoff with full jitter (30 s doubling, capped at 1 h),
  honour `Retry-After` on a 429, and stop after `max_attempts` (default 8, roughly 4 h).
  A permanent failure (4xx other than 408/429, a refused address, a malformed channel)
  goes straight to the **dead-letter state** (`state = 'dead'`) with its last error. An
  operator redrives a dead delivery through the API, which inserts a **new** row
  (`redrive_of`) that keeps the original external dedup key, so the receiver can still
  de-duplicate and the failed row stays as evidence.
- **Rate limiting per channel** is a fixed one-minute window in
  `notification_channel_windows`, taken with an atomic conditional `UPDATE` at claim
  time. It is exact across replicas. A row that finds its channel's window full is deferred
  to the next window without spending an attempt.
- **PagerDuty resolve on recovery.** A `trigger` sent to PagerDuty opens a row in
  `notification_incidents` keyed by (rule, channel, workflow). The next `run.succeeded`
  of that workflow enqueues a `resolve` with the same PagerDuty `dedup_key`, and closes the
  row in the same transaction. A success with no open incident sends nothing.

### 3. Channel and trigger secrets are sealed or referenced, and never read back

`internal/secretbox` seals secrets with AES-256-GCM under `NEXUS_SECRET_KEY` (or
`_FILE`). The additional authenticated data binds each ciphertext to its table, namespace
and row id, so a ciphertext copied onto another row fails to open. The key id is carried
in the ciphertext, and `NEXUS_SECRET_KEY_PREVIOUS` opens ciphertexts sealed under a
retired key during a rotation. A channel may instead hold a **reference**
(`env:NAME` or `file:/path`) that the event-service resolves when it sends. No API read
returns a secret: reads report `{configured, source, hint}` only. A webhook-trigger
secret is generated by the server and shown **once**, on creation and on rotation.
Without a configured key the gateway refuses to store an inline secret (503
`secret_key_unconfigured`) instead of storing it in clear.

Outbound HTTP refuses loopback, link-local, private and metadata addresses (checked on the
resolved address at dial time, so DNS rebinding cannot bypass it), follows no redirects,
and caps response reads. `NEXUS_NOTIFY_ALLOW_PRIVATE_TARGETS=true` lifts the address
check for on-premises receivers and for tests. It defaults to false.

### 4. Inbound webhook triggers terminate at the gateway

`POST /v1/hooks/{ns}/{trigger-id}` is the second unguarded route family after login.
It authenticates by signature, not by credential:

- `X-Nexus-Timestamp: <unix seconds>` and `X-Nexus-Signature: v1=<hex>`, where
  `hex = HMAC-SHA256(secret, timestamp + "." + raw_body)`, compared in constant time.
  During a rotation's grace period, both the current and the previous secret are accepted.
- The timestamp must fall within the trigger's tolerance (default 300 s, max 3600 s).
- **Replay protection:** the SHA-256 of the signature is inserted into
  `webhook_receipts` under `PRIMARY KEY (trigger_id, signature_digest)`. A replay within
  the retention window conflicts and is refused with 409. A replay outside the window
  fails the timestamp check. Either way it cannot start a run.
- **Idempotency:** `idempotency_key_path` (a JSONPath into the payload) yields the
  run's idempotency key `webhook:<trigger>:<value>`. A sender that retries a delivery with
  a fresh signature converges on the same run, because the run layer deduplicates on the
  key (ADR-0006). Without a key path, each distinct signed delivery is one run.
- **Params** come from `params_mapping`. Each value is a JSONPath (`$.a.b[0]`) or a
  template containing `{{ $.path }}`. Any other value passes through as a literal.
- Runs record `trigger_kind = "webhook"` and `trigger_detail = {trigger_id,
  trigger_name, delivery}`. They are created through a new JSON RPC on the
  workflow-manager, `WorkflowSpecService/CreateTriggeredRun`, which carries typed params
  and trigger detail. The protobuf `CreateRun` flattens params to strings and has no
  trigger detail.
- Each trigger records its creator. Creation, update, deletion and rotation are audited
  in the same transaction as the change, and every *rejected* delivery is audited as
  `trigger.fire` with outcome `denied`. RBAC gains `triggers.{view,create,update,delete}`,
  and creating a trigger also requires `runs.create`: a trigger is a standing
  authorization to create runs.

### 5. Sensors are schedules whose tick is an observation

A sensor is either `object_store` (poll `s3://bucket/prefix`, match a glob) or
`run_succeeded` (another workflow's run succeeded). Definitions are gateway-owned config;
their **state** is workflow-manager-owned:

- **Leader-safe and multi-replica safe without election.** A pass claims due sensors by
  leasing `sensor_state` (`lease_owner`, `lease_until`, `SKIP LOCKED`), and every write
  of the cursor is fenced on `lease_owner = me AND lease_until > now()`. A replica that
  lost its lease writes nothing.
- **At most one run per object.** Each observed object (or upstream run) is claimed by
  inserting `sensor_observations (sensor_id, item_key)` with a unique constraint. The
  run is then created under the deterministic idempotency key `sensor:<id>:<sha(item)>`
  and its id recorded on the observation. A crash between the claim and the create leaves
  an observation without a run. The next pass retries it under the same key, so the
  outcome is exactly one run, never zero and never two. The concurrency test runs several
  replicas against one bucket and asserts this.
- **Durable cursor.** Object listing resumes from `cursor` (the last key listed; `S3
  StartAfter`) in bounded pages and wraps at the end of the prefix. Keys that arrive
  behind the cursor are found on the next sweep, and the observation table makes that
  harmless. Upstream-run sensors keep an `ended_at` watermark with a lookback, for the
  same commit-order reason as the outbox.
- **Backfill.** `backfill: true` triggers for everything already present. `false` (the
  default) first records a baseline — every existing object is observed with outcome
  `baseline` and no run — and only new arrivals trigger.
- **Pluggable object stores.** The sensor depends on a two-method `ObjectLister`
  interface, and a registry maps a URI scheme to an implementation. S3-compatible (minio-go,
  the result-store client library) ships now; native Azure/GCS listers register under
  their schemes without touching the sensor.

### 6. Lineage is declared in the spec and recorded per run

`TaskSpec.assets = {produces: [{uri, name}], consumes: [{uri, name}]}` is an additive,
optional schema field. It is part of the author's document and therefore of the content
address when present; it is absent (and the hash unchanged) otherwise. The SDK's `@task`
gains `produces=` and `consumes=`. The notifier records the run's edges
(`asset_lineage_edges`) the first time it sees the run, and a **materialization** when a
producing node succeeds (`NodeStateChanged` → `SUCCEEDED`), keyed `UNIQUE(run, node,
asset)`. The API answers producers, consumers, latest materialization, and a bounded
lineage graph. This is declared lineage: Nexus records what a task *said* it reads and
writes, not what it touched. Observed lineage is out of scope.

### Ownership (amends ADR-0011's matrix)

| Owner | New tables |
|---|---|
| api-gateway | `notification_channels`, `notification_rules`, `notification_rule_channels`, `webhook_triggers`, `webhook_receipts`, `sensors` (definitions) |
| event-service | `notification_deliveries`, `notification_channel_windows`, `notification_incidents`, `assets`, `asset_lineage_edges`, `asset_materializations`; `outbox.notified_at` |
| workflow-manager | `sensor_state`, `sensor_observations` |

One documented cross-owner grant: the gateway may **INSERT** (only) into
`notification_deliveries`, for a channel test and a dead-letter redrive. Both are new
rows. The gateway can never mark, lease or delete a delivery.

## Consequences

**Positive**
- No new infrastructure and no leader election. Every loop is a `SKIP LOCKED` claim plus
  a unique key, the same shape as the dispatch claim and the schedule fire loop.
- Notifications work with NATS disabled, survive broker outages, and are evaluated
  exactly once in effect per fact.
- A dead letter is visible, explained and redrivable, and the failure stays on record.
- Sensors and webhooks inherit run idempotency, quotas, max-active-runs and RBAC, because
  they create runs through the same service path as a manual submission.

**Negative**
- The notifier adds one UPDATE per announced fact to the outbox. This is the hottest
  write path's side table, though never the claim transaction itself.
- Delivery to Slack, Teams and SMTP is at-least-once without a receiver-side dedup
  primitive, so a rare duplicate message is possible.
- A shared encryption key must be distributed to the gateway (seal/open trigger secrets,
  seal channel secrets) and the event-service (open channel secrets).
- Object-store sensors pay a LIST per poll per sensor, and the observation table grows
  with the number of objects seen. It is pruned only when the sensor is deleted.
- Lineage is declared, not observed, so it is only as honest as the declarations.

**Neutral**
- Rule evaluation reads the run's spec (name and labels) on each fact. A cache would
  help at very high event rates, and none is needed at the ADR-0018 envelope.

## Alternatives considered

**Consume the NATS stream (`NEXUS_RUNS`) for notifications.** Rejected. It would make
notifications depend on the component ADR-0003 allows to be down, and a durable consumer's
ack is the only exactly-once seam. The outbox gives the same facts transactionally.

**Emit notifications from the writers (workflow-manager, scheduler).** Rejected: it puts
rule evaluation on the run-transition and claim paths, and every writer would need the
channel table and the secret key.

**A high-water mark over `run_events.id`.** Rejected for the commit-order gap described
in migration 0016: a late-committing row below the mark is skipped forever.

**Send synchronously from the evaluator.** Rejected: one slow Slack endpoint would stall
evaluation of every other fact, and there would be nowhere to hold a retry.

**Leader election for sensors.** Rejected: leases per sensor give the same safety with
no single point of stall, and they spread sensors across replicas.

**Hash the webhook secret like an API key.** Impossible. HMAC verification needs the
secret itself, so it is sealed rather than hashed.
