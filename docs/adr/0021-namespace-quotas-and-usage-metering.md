# ADR-0021: Namespace quotas enforced in the claim, and an append-only usage ledger

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0011 (table ownership), ADR-0017 (fair share), ADR-0018 (dispatch ceiling)
- **Closes:** ARCHITECTURE.md §5.3 step 3 (`quota_would_exceed`), which was
  specified but never implemented (CONTRACT-FINDINGS F123)

## Context

Cost management, AI token tracking and Cloud billing all need two things the
platform does not have:

1. **Limits a namespace cannot exceed**, so one tenant's burst cannot run away
   with the budget or the shared control plane. ARCHITECTURE.md §3.2 says the
   resource manager "enforces namespace quotas (max concurrent CPU / memory /
   GPU per namespace) as an admission check before dispatch", and §5.3 shows a
   `quota_would_exceed` step in the claim. Neither exists in code: nothing in
   `internal/store` reads a quota, and there is nowhere to configure one.
2. **A record of what was consumed**, per attempt, that a billing system can
   trust: complete, not double-counted, not lost in a crash, and attributable
   to a tenant, run, worker and pool. Today the only trace is `task_attempts`
   timestamps and `resource_request`; the worker's measured `ExecutionMetrics`
   are sent on every `ReportResult` and dropped (F122).

What the code establishes about where these can live:

- The claim (`Claim`, push; `ClaimForWorker`, pull) is one READ COMMITTED
  transaction whose correctness rests on row locks (`FOR UPDATE [SKIP LOCKED]`
  on the attempt and the worker), the `state = 'PENDING'` CAS and
  `UNIQUE(attempt_id)` on leases (ADR-0002, ADR-0010). **None of those rows is
  per-namespace.** Two replicas claiming two attempts of one namespace on two
  different workers share no lock at all; each can read the namespace's
  in-flight total, see room for one more, and commit. A quota checked that way
  is advisory: under concurrency it is exceeded by up to one attempt per
  replica per cycle. The concurrency semaphore (`concurrencyAdmit`) has the
  same shape and the same gap, which is why this ADR does not copy it.
- Attempts leave the in-flight states by exactly three code paths, each one
  transaction that also releases the reservation: `ApplyResult` (worker report:
  SUCCEEDED, FAILED, TIMED_OUT, LOST on INDETERMINATE), `expireOne` (LOST on
  lease expiry or worker death) and `cancelOneAttempt` (CANCELLED). A
  PENDING attempt that times out (`ExpireScheduleToStart`) or is cancelled
  never held capacity.
- Runs are created only through `WFMCreateRun` (workflow-manager), from the
  API, schedules and backfills alike. It already serializes `max_active_runs`
  per workflow with a transaction-scoped advisory lock.
- Namespace policy already lives in `platform_config` (audited, revisioned,
  one row per namespace); ADR-0017 put `fair_share_weight` there and exposed
  it to the scheduler through a clamping view.

## Decision

### 1. Quotas are part of the namespace's platform policy

`platform_config.config -> quotas`, a new sub-document (`platform.Quotas`):

| Field | Kind | Enforced |
|---|---|---|
| `max_inflight_attempts` | concurrent | in the claim |
| `max_cpu_millis`, `max_memory_bytes`, `max_gpu_count` | concurrent | in the claim |
| `max_custom` (`{name: qty}`) | concurrent | in the claim |
| `max_active_runs` | volume | at run creation |
| `max_runs_per_hour` | rate (sliding 60 min) | at run creation |
| `storage_soft_limit_bytes` | soft | reported only |
| `locked` | governance | see below |

**Zero or absent means unlimited**, and every existing row has no `quotas`
key, so an upgraded deployment behaves exactly as before. Concurrent usage is
"attempts in DISPATCHED or RUNNING and the resources they requested", the same
in-flight definition ADR-0017 counts, summed across every queue of the
namespace (a quota is per namespace, not per queue).

Quotas are written through a dedicated `PUT /v1/namespaces/{ns}/quotas`
(`quotas.manage`), which shares the platform-config revision, so the two
editors cannot silently overwrite each other (a stale save gets 409). The
generic `PUT .../platform/config` keeps the stored quotas when a client omits
them and refuses a request that tries to change them (400): quotas have their
own permission, and an older console that does not know the field must not
erase them. Every change writes a `quota.update` audit row with the old and
new values.

**Who may loosen.** Worker capacity is per tenant (0013–0015), so a concurrent
quota mostly governs a tenant's own spend; run-creation limits also protect
the shared control plane. Both uses are legitimate, so quotas start
**unlocked**: a namespace administrator with `quotas.manage` sets and changes
them freely (self-governance, budget guard rails). A **platform operator** can
set `locked: true`; from then on a namespace administrator may only *tighten*
limits (lower a limit, or add one where there was none) and cannot unlock.
This mirrors ADR-0017's rule for the weight: whatever takes from others, or
from the operator's budget, needs the operator.

### 2. Concurrent quotas are enforced in the claim, serialized per namespace

A namespace **with at least one concurrent limit** takes a transaction-scoped
advisory lock keyed on its tenant id before the claim reads its usage. Under
that lock the claim reads the namespace's in-flight usage with a fresh READ
COMMITTED statement, checks `used + request <= limit` on every configured
dimension, and only then commits the dispatch. The lock is released by the
same COMMIT that makes the dispatch visible, so the next holder's usage read
sees it. Releases (completion, LOST, cancel) do not take the lock: they only
lower usage, and a claim that reads usage a moment too high is conservative.
This makes the check exact — no two claims can both pass and together exceed
the quota — with no new hot row and no counter that can drift.

Deadlock freedom comes from the acquisition point, which differs by path:

- **Pull (`ClaimForWorker`).** The worker's tenant is known before anything is
  locked, so the lock is taken **blocking, as the first statement** of the
  claim transaction, before the worker row. A waiter holds nothing, so no wait
  cycle can pass through it.
- **Push (`Claim`).** The tenant is known only after the candidate attempt row
  is locked, so the lock is taken with `pg_try_advisory_xact_lock` and never
  waited on. A candidate whose namespace is being claimed by another replica
  right now is skipped for this cycle (no pending reason is written; it is not
  blocked, only contended).

A namespace **without** concurrent limits takes no lock and reads no usage.
The only cost it pays is one indexed lookup returning no row (measured below).
Nothing else in the claim changes: READ COMMITTED, the SKIP LOCKED reads, the
CAS and the single transaction all stand.

When the check fails the attempt stays PENDING with
`pending_reason = 'quota_exceeded: <dimension> (used X of Y)'` (dimension one
of `inflight_attempts`, `cpu_millis`, `memory_bytes`, `gpu_count`,
`custom:<name>`; `, request R exceeds the quota` is added inside the
parentheses when one attempt alone asks for more than the limit, so it can
never run until the quota is raised). It is an annotation like
`waiting_for_resources`, not a failure: no retry budget is spent. When the
in-flight count is at its limit -- which every attempt counts against -- the
push path sets the namespace aside for the rest of the cycle (as ADR-0017 does
for a blocked namespace) and the pull path ends the poll, so a quota-bound
namespace costs one usage read per cycle, not one per look-ahead slot. A full
CPU, memory, GPU or custom dimension does not end the search, because an
attempt that does not request that resource still fits.

**The index.** ADR-0017's `idx_attempts_inflight_by_tenant` is keyed
`(queue, tenant_id)`; a quota spans queues, and PostgreSQL 16 cannot skip-scan
the leading column, so it would read every in-flight row of every tenant.
Migration 0027 adds `idx_attempts_inflight_tenant ON task_attempts
(tenant_id) WHERE state IN ('DISPATCHED','RUNNING')`, the same partial
predicate with the tenant leading. The usage read is O(the namespace's
in-flight attempts).

**Limits are read from a view**, `namespace_quota_limits` (migration 0027),
never parsed in the claim, and malformed values degrade to 0 (unlimited)
instead of raising, for ADR-0017's reason: an error there would stop dispatch
for every tenant. The API validates on write, so a malformed value can only
come from hand-edited SQL.

### 3. Run-level limits are enforced at run creation

`WFMCreateRun` reads `max_active_runs` / `max_runs_per_hour` from the view,
and when either is set, takes a per-tenant advisory lock (a different key
class from the claim's) before counting and inserting, so concurrent creations
cannot both pass. A replayed idempotency key returns its existing run even
when the namespace is at its limit. A refusal is `429 Too Many Requests`,
problem code `namespace_quota_exceeded`, naming the dimension and `used X of
Y`; for the hourly rate it carries `Retry-After` (seconds until the oldest run
in the window ages out). A schedule tick refused this way is not lost: the
tick is not claimed, so the next scheduler pass fires it (the same path
`max_active_runs` already takes).

### 4. Usage metering: one append-only ledger row per attempt that held capacity

`usage_ledger` (migration 0027) is written **in the same transaction as the
terminal transition** by the three paths above, with `INSERT ... ON CONFLICT
(attempt_id) DO NOTHING`. It is idempotent twice over: the terminal CAS lets
exactly one transition happen, and `UNIQUE(attempt_id)` makes a second insert
a no-op even if a future path forgets the CAS. A late or duplicate report
finds the attempt terminal and writes nothing. Only attempts that were
dispatched (`dispatched_at IS NOT NULL`) get a row; an attempt that never held
a reservation consumed nothing billable.

Each row carries tenant, namespace, run, node, attempt number, outcome,
queue, pool (the namespace policy's pool for that queue, when one exists),
worker id and hostname, executor, the reserved vector (CPU millis, memory
bytes, GPU count and kind, custom), `dispatched_at` / `started_at` /
`ended_at`, and derived quantities in exact `NUMERIC`:

- `wall_seconds` = ended − started (or − dispatched when it never started);
- `reserved_seconds` = ended − dispatched: the interval the reservation held
  capacity, which is what a reservation-based bill charges for;
- `cpu_core_seconds`, `memory_gib_seconds`, `gpu_seconds` = reserved vector ×
  `reserved_seconds`;
- the worker's measured `ExecutionMetrics` (CPU seconds, peak memory, wall
  ms, OOM, exit code) when the attempt ended by a worker report. LOST and
  cancelled attempts have none, and the columns are NULL rather than 0.

`ApplyResult` now also stores the metrics on `task_attempts.metrics`, a
column that has existed since 0001 and was never written (F122).

**Ownership (ADR-0011).** The **scheduler owns `usage_ledger`**: every
terminal transition that ends a reservation is already the scheduler's
transaction. The scheduler role gets INSERT and SELECT only; every other
service gets SELECT. Nobody gets UPDATE or DELETE, and a trigger refuses
UPDATE even for the table owner. Rows are deliberately not foreign-keyed to
runs or attempts, so deleting a run (0011) does not delete what it consumed.
Retention is an operator action by the database owner (OPERATIONS §21).

**Export hook.** The same statement writes an outbox row, topic
`usage_recorded`, which the event-service relay publishes as
`nexus.usage.recorded.<namespace>` into a new file-backed stream
`NEXUS_USAGE` (30 days). The message carries the ledger id and the row's
quantities, and `Nats-Msg-Id` makes re-publication harmless. For consumers
that prefer pulling, `internal/metering` defines `Exporter` (`Export(ctx,
[]Record) error`) and `store.UsageRecordsAfter(cursor, limit)`, a keyset read
over the ledger's monotonically increasing id with a commit-lag guard. The
ledger is the truth; the bus is the notification. No billing is built here.

**Rollups.** `GET /v1/namespaces/{ns}/usage` groups by day, queue, pool,
worker, executor or outcome over a time range, keyset-paginated, attributing
each attempt to the UTC day it **ended** in. A platform operator can roll up
by namespace across tenants with `GET /v1/platform/usage`.

### 5. Permissions

New catalog entries `quotas.view`, `quotas.manage`, `usage.view`. Viewer and
Workflow roles get `quotas.view` (it explains `quota_exceeded` reasons);
Infrastructure Operator gets all three; Auditor gets `quotas.view` and
`usage.view`; Administrator has the whole catalog. Legacy (unbound) readers
see quotas but not usage, which is cost data. Every route authorizes the path
namespace first (F101) and every query is keyed by the principal's namespace.

## Evidence

- `services/scheduler/quota_integration_test.go` (real Postgres 16):
  `TestQuotaHoldsUnderConcurrentReplicas` runs 4 pull-path and 4 push-path
  replicas, each with its own pool, against 200 attempts of one namespace
  capped at 5 in flight and 2,000 CPU millis (mixed 300/700 m requests)
  across 16 workers, while a completer finishes work at random. A sampler
  reads the namespace's in-flight count and CPU sum in one statement
  continuously; across five consecutive runs no sample exceeded either limit,
  both peaks sat exactly at the limit (5 and 2,000), and 28–99 claims per run
  were held back by the quota. **Mutation check (F89):** with the advisory lock
  replaced by a no-op in both paths, the same test fails every run, with
  3,487–3,701 samples over quota and a peak of 7 in flight / 3,300 CPU millis.
- `TestQuotaBlockedAttemptExplainsItself`, `TestQuotaDimensionsAndOversizeRequest`,
  `TestNoQuotasMeansUnlimited`: the reason text on both paths, per-dimension
  reasons (CPU, GPU, custom), the oversize message, and unchanged behaviour
  with no quotas or all-zero quotas.
- `internal/store/quota_integration_test.go`: 20 concurrent run creations
  against `max_active_runs = 5` create exactly the free slots; the hourly
  rate returns Retry-After; an idempotent replay at the limit returns its run;
  the quota document's revision, lock and audit rules; rollups and pagination;
  the grants (scheduler INSERT-only, nobody UPDATE/DELETE).
- Idempotent metering: duplicate and late reports, a report racing a lease
  expiry, cancel of a DISPATCHED and of a PENDING attempt, and worker death;
  exactly one ledger row per dispatched attempt, none for the never-dispatched.
- Claim latency before and after (`tests/bench`, OPERATIONS §21.8, two
  alternated rounds): an unlimited namespace is within this laptop's
  run-to-run noise of `origin/main` on both paths (e.g. pull, 10k backlog, 4
  replicas: 485 vs 509 claims/s, p50 6.6 vs 6.8 ms). A quota-bound namespace
  on the pull path with 4 replicas dispatches about 140/s with p50 24–29 ms,
  the cost of the per-namespace serialization; single-replica and push-path
  cells are close to baseline.

## Consequences

**Positive** — quotas are exact under any number of scheduler replicas, with
no counter to drift and no cost to namespaces that do not use them. The
ledger is complete by construction (same transaction as the transition),
cannot double count, survives run deletion, and already emits the event a
billing service will consume.

**Negative** — a namespace **with** concurrent quotas has its dispatches
serialized across replicas: its throughput is bounded by one claim
transaction at a time (a few milliseconds, so hundreds per second), and on
the push path a contended namespace is skipped for a cycle rather than waited
for. Every pull claim pays one extra indexed lookup even when unlimited. A
quota is on *requested* resources, not measured use. Usage is attributed to
the end day, so a long attempt crossing midnight is billed wholly to the
second day. Storage is soft and measured only from durable log bytes;
result-store artifacts are not metered yet. The ledger grows by one row per
dispatched attempt until an operator prunes it.

**Neutral** — the pull-path look-ahead, fair-share order and head-of-line
guard are unchanged; `quota_exceeded` reasons do not match the
`waiting_for_resources:` prefix the head-of-line guard reserves capacity for,
so a quota-blocked attempt never idles a worker.

## Alternatives considered

1. **Check usage without a lock** (like `concurrencyAdmit`). Rejected: exceeded
   under concurrency, demonstrably (the mutation check above).
2. **A per-namespace counter row updated in the claim.** Exact, but every
   release path would have to update it too (lock contention with claims, a
   new deadlock surface among release transactions that already hold worker
   rows), and a missed decrement is a permanent drift that silently shrinks the
   quota. The aggregate cannot drift.
3. **SERIALIZABLE isolation for quota-bound claims.** Rejected for ADR-0002's
   reason: SKIP LOCKED plus 40001 aborts turns every claim into a retry loop.
4. **`SELECT ... FOR UPDATE` on the `platform_config` row as the mutex.**
   Equivalent serialization, but it needs UPDATE privilege on a gateway-owned
   table for the scheduler role and makes quota edits wait behind dispatch.
5. **Blocking lock on the push path.** The push path already holds the
   candidate attempt row and worker rows when it learns the tenant; waiting
   there can form a cycle with a release path waiting on those rows. Try-lock
   costs a skipped candidate instead of a deadlock.
6. **Metering from `run_events` after the fact** (a consumer that folds
   events into usage). Rejected as the source of truth: the bus is a hint
   (ADR-0003) and run history is deleted with its run; billing needs a record
   written atomically with the fact.
