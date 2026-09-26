# ADR-0032: AI workloads — usage units, shared rate limits and task checkpoints

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-26
- **Deciders:** platform team
- **Refines:** ADR-0005 (pull-based dispatch), ADR-0008 (result store), ADR-0010
  (lease fencing), ADR-0018 (dispatch ceiling), ADR-0021 (quotas and the usage ledger)
- **Findings:** F148, F149 (CONTRACT-FINDINGS.md)
- **Operations:** OPERATIONS §29. **Migration:** 0036.

## Context

Batch LLM inference, evaluation and fine-tuning pipelines are ordinary DAGs of
the kind Nexus already runs, with three needs the platform does not meet:

1. **Usage that is not time.** A call to a hosted model is billed per input and
   output token, not per reserved CPU-second. ADR-0021's ledger records the
   reserved vector and derived seconds; nothing a task *consumes externally*
   reaches it. A parallel workstream builds price books that price ledger rows,
   so this ADR owns only the *recording* side, and fixes the key convention
   both sides depend on.
2. **Provider rate limits shared by the whole fleet.** A 5,000-way fan-out over
   prompts, spread over 40 workers and three scheduler replicas, must stay
   under one provider's requests-per-second and tokens-per-minute limit. A
   per-process limiter admits `workers × limit`. The limit must be enforced
   where every caller meets: the control plane.
3. **Resuming mid-task.** A shard of 20,000 prompts that dies at prompt 14,000
   (a lost worker, a provider outage past the in-task retries, a deadline)
   restarts from zero. Memoization (§6.4) reuses *finished* tasks, not partial
   ones. A checkpoint must be written durably, read back by the retry, and must
   not be overwritten by a zombie attempt that still thinks it owns the work.

What the code establishes:

- **A workload has no channel to the control plane (F148).** The Python SDK's
  `ctx.heartbeat()` and `ctx.progress()` call `_heartbeat_fn`, which nothing in
  `nexus.entrypoint` or `nexus.worker` sets: under a real agent they are
  silent no-ops. The only workload → agent path is the output directory, read
  once, after exit. The agent (`agent/internal/worker`) holds the lease token,
  the worker session credential and the result-store client; the workload holds
  none of them, and should not (a user task must not be able to renew leases,
  report results for other attempts, or read the bucket at large).
- Every attempt RPC is fenced twice: by the gateway on the lease token
  (`workergrpc.fence`) and again by the scheduler inside its write when the
  write stores worker-supplied data (`RecordProgress`, F96).
- Attempts end by exactly three scheduler transactions (`ApplyResult`,
  `expireOne`, `cancelOneAttempt`), each of which locks the attempt row
  `FOR UPDATE` first and writes the ledger row (`recordUsage`) in the same
  transaction (ADR-0021 §4). `ApplyResult` deletes the lease; cancel leaves it.
- The claim is one READ COMMITTED transaction whose cost sets the dispatch
  ceiling (ADR-0018); every statement added there is paid on every dispatch.
- Custom resource names are `^[a-zA-Z0-9]([a-zA-Z0-9._/-]{0,61}[a-zA-Z0-9])?$`
  (platform.customNamePattern, the spec schema): they cannot contain `:`.

## Decision

### 1. The workload API: an agent-local, per-attempt socket

The agent serves each attempt a small HTTP/1.1 JSON API on a **Unix domain
socket** it creates for that attempt, and exports two variables to the
workload:

| Variable | Value |
|---|---|
| `NEXUS_TASK_API` | `unix://<path>` of the attempt's socket, as the workload sees it |
| `NEXUS_TASK_API_TOKEN` | 256 random bits; every request must send `Authorization: Bearer <token>` |

The agent is the only thing that talks to the control plane: it adds the
lease token and forwards over the existing authenticated worker connection.
One socket per attempt means the attempt identity is structural — a request
on attempt A's socket can only ever act as attempt A — and the token stops any
other local process that can reach the path. The socket directory is created
`0711`, the socket `0666` so a container user with another uid (the task image
runs as uid 10001) can connect; the token, not file mode, is the credential.
The socket is closed and removed when the attempt's `Run` returns, so a
lingering grandchild process cannot act on a finished attempt.

- **subprocess**: the socket lives under the agent's task-API root (a short
  `/tmp` path by default: `sun_path` is 104 bytes on Darwin).
- **docker**: the socket's directory is bind-mounted read-write at
  `/run/nexus-task` and `NEXUS_TASK_API` names the container path. This works
  with `--network none` (the executor's default), which is the point: a task
  that declared no network still reaches its own agent and nothing else.
- **kubernetes**: pods run on other nodes and have no path to the socket. The
  agent serves them the same API over a TLS listener, routed by the token (see
  the addendum below; F148). Without a listener configured the variables are
  not exported, and the SDK degrades as described in §6.

Endpoints (all JSON; errors are `{"error": "<code>", "message": "..."}`):

| Method, path | Body → response | Upstream |
|---|---|---|
| `GET /v1/info` | → `{attempt_id, capabilities: {usage, rate_limits, checkpoints, progress}}` | none |
| `POST /v1/usage` | `{"units": {key: int}}` **increments** → `{"totals": {...}}` | batched `ReportUsage` |
| `POST /v1/progress` | `{"message": str, "fraction": float|null}` → 204 | `ReportProgress` |
| `POST /v1/permits` | `{"keys": [..], "requests": n, "tokens": n}` → `{"granted", "retry_after_ms", "unlimited_keys"}` | `AcquirePermits` |
| `POST /v1/permits/settle` | `{"keys": [..], "tokens": ±n}` → 204 | `AcquirePermits(settle)` |
| `PUT /v1/checkpoint` | raw bytes → `{"seq", "key", "sha256", "size_bytes"}` | result store, then `SaveCheckpoint` |
| `GET /v1/checkpoint` | → bytes + `X-Nexus-Checkpoint-{Seq,Attempt,Saved-At}`, or 404 | `GetCheckpoint`, then result store |
| `POST /v1/error` | `{"class", "type", "message", "retry_after_ms"}` → 204 | carried on `ReportResult` |

Status codes are the workload's contract: 400 invalid request, 401 bad token,
409 `stale_lease` (the attempt no longer owns its lease: stop), 413 checkpoint
too large, 501 capability not offered by this control plane, 503 control plane
unreachable (retry).

### 2. Usage units: key convention, recording, and the ledger

**The key convention** (shared with the price-book workstream, which prices
exactly these keys):

```
tokens:<provider>/<model>:input      input (prompt) tokens
tokens:<provider>/<model>:output     output (completion) tokens
tokens:<provider>/<model>:cached     cached-input tokens, when the provider reports them
requests:<provider>/<model>          provider calls made
<kind>:<subject>[:<qualifier>]       anything else (e.g. images:<provider>/<model>, units:embeddings)
```

Grammar: `kind` is `[a-z][a-z0-9_]{0,31}`; `subject` is
`[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}`; `qualifier` is `[a-z][a-z0-9_]{0,31}`;
the whole key is at most 200 bytes. `provider` is lower-case by convention
(`openai`, `anthropic`, `azure-openai`, `bedrock`, `mock`), and `model` is the
provider's model id verbatim, except that neither may contain `:` (the price
book treats every `custom` key with a `:` as a usage unit, and the grammar
needs the colon as its separator): the SDK maps any character outside the
subject grammar to `_`, so a fine-tuned id `ft:gpt-4o-mini:org:x` is reported
as `openai/ft_gpt-4o-mini_org_x`, and the scheduler refuses a key that does not
parse. Usage keys cannot be quota-limited yet (`max_custom` names refuse `:`);
that is a follow-up. Values are **non-negative integers** (int64,
≤ 10^15 per key per attempt); a fractional quantity uses a smaller unit. At
most **64 keys per attempt**. Because every usage key contains `:` and no
custom resource name can, the two share the ledger's `custom` object without
ever colliding.

**Recording.** The workload posts *increments*; the agent keeps the attempt's
cumulative totals and sends them upstream as **cumulative snapshots** with
`ReportUsage` — at most every 10 s while they change, and once more,
synchronously, before `ReportResult`. The scheduler stores them in
`attempt_usage` (migration 0036), one row per attempt, merging each snapshot
**per key with `max`**. Totals only grow, so the merge is idempotent,
commutative and immune to duplicates and reordering: a resent or late snapshot
can never lower or double a total. Validation (grammar, bounds, key count) is
applied by the scheduler, and a report that fails it is refused whole.

**Fencing and the terminal race.** `RecordUsage` runs in one transaction that
first takes `FOR NO KEY UPDATE` on the attempt row (bounded by a 2 s
`lock_timeout`), then re-reads the lease and the attempt state *after* the lock,
and only then upserts `attempt_usage`. Every terminal path locks the attempt
`FOR UPDATE` first, which conflicts with `FOR NO KEY UPDATE`, and reads
`attempt_usage` only later in its transaction. So either the usage report
commits first and the terminal transaction sees it, or the report waits, then
sees a terminal state (or no lease) and is refused as stale. No unit is ever
stored on the attempt and missing from the ledger. The report does not touch
timers, the run row or the lease row, so it cannot form a cycle with any
terminal path (the deadlock `RecordProgress` hit was a timer/lease ordering).
The lease-expiry reclaim skips locked attempt rows; a usage report can delay a
reclaim by one sweep, never block it.

`FOR KEY SHARE` was the first choice and was measured to be wrong: share
lockers do not conflict with each other, so PostgreSQL lets a new one join the
row's multixact without queueing behind a waiting `FOR UPDATE`. Four
overlapping reporters starved `ApplyResult` indefinitely in
`TestUsageNeverEscapesTheLedger`, and `lock_timeout` never fired because each
single wait was short. `FOR NO KEY UPDATE` conflicts with itself, so every
locker queues on the tuple lock in FIFO order behind the terminal transition.
It still does not conflict with the `FOR KEY SHARE` of a foreign-key check.

**The ledger.** `recordUsageSQL` now writes
`custom = <reserved custom resources> || <attempt_usage.units>` and adds
`usage_units` to the `usage_recorded` outbox payload, in the same statement and
therefore the same transaction as the terminal transition (ADR-0021 §4). No
ledger column is added or changed. LOST and CANCELLED attempts carry whatever
units were reported before they ended: tokens spent by an attempt that later
failed were still spent.

### 3. Shared rate limits: a lease-fenced permit service

**Configuration** is namespace policy, next to the quotas it resembles:
`platform_config.config -> quotas -> rate_limits`, written through the existing
audited, revisioned `PUT /v1/namespaces/{ns}/quotas` (`quotas.manage`), and
subject to the quotas' lock (a locked namespace may only tighten a rate limit
or add one):

```json
"rate_limits": {
  "openai/gpt-4o-mini": {"requests_per_second": 50, "tokens_per_minute": 2000000},
  "anthropic":          {"requests_per_second": 10, "burst_requests": 20}
}
```

A key is a subject (`<provider>` or `<provider>/<model>`, the usage-key subject
grammar). `requests_per_second` (> 0, ≤ 10^6, fractional allowed) and
`tokens_per_minute` (integer, ≤ 10^12) are each optional, but at least one is
required. Bursts default to one second of requests (at least 1) and one minute
of tokens. At most 64 keys per namespace. The scheduler reads them through a
view, `namespace_rate_limits`, that never raises (ADR-0017's rule).

**Acquisition** is a new worker RPC, `AcquirePermits(attempt, lease, keys,
requests, tokens)`, fenced on the lease like every attempt RPC and served by
the scheduler. For each configured key, in sorted order, one transaction
upserts and locks the key's row in `rate_limit_buckets`, refills both levels by
`rate × (clock_timestamp() − refreshed_at)` capped at the burst, and grants
**all keys or none**. A denial returns `retry_after_ms`, the exact time until
the shortest-refilling missing level suffices, computed from the database clock
(no scheduler or agent clock is involved). A request larger than a burst is
granted only from a full bucket and drives it negative (bounded at −burst), so
a single oversized call is possible but paid for by the calls after it.
Unconfigured keys are granted immediately and reported in `unlimited_keys`.
The call never waits server-side: the SDK sleeps `retry_after_ms` (plus
jitter) and asks again, so no control-plane connection or row lock is held
while a task waits.

**Settlement.** A caller asks for its *estimated* tokens (prompt + max output);
after the response it settles the difference with
`AcquirePermits(settle=true, tokens=actual−estimate)`, applied without a check
(debt or refund, clamped to [−burst, burst]). Tokens-per-minute is therefore
exact over time, not merely an upper bound.

**Why a permit service and not claim-time admission.**

- *Granularity.* Admission at claim meters attempts. A shard task makes
  thousands of calls; a claim-time bucket cannot express that without also
  making the shard hold a reservation for its whole run, and tokens-per-minute
  is unknowable at claim time (the prompt is built by the task).
- *The dispatch ceiling.* ADR-0018 measured the claim as the platform's
  throughput limit. A bucket in the claim puts a hot-row update (or ADR-0021's
  per-namespace advisory lock) on every dispatch of every tenant. The permit
  service adds **nothing** to the claim transaction; its cost is paid only by
  tasks that call it, on the scheduler's report path.
- *No drift.* ADR-0021 rejected a counter row for quotas because every release
  path must decrement it. A token bucket has no release path: a permit is
  consumed, never returned. The row is correct after any crash, and a lost
  update is impossible because the row is locked for the read-modify-write.
- *Concurrency limits already exist.* "At most N in flight" is the existing
  `concurrency_key`/`concurrency_limit` semaphore (§5.3 step 4), which remains
  the right tool and composes with this: `concurrency_limit` bounds parallel
  calls, `rate_limits` bounds their rate.

**Why a row lock and not an advisory lock.** The advisory-lock pattern of
ADR-0021 serializes a *read of an aggregate* that has no row of its own. A
bucket is one row; `SELECT … FOR UPDATE` on it is the same serialization,
scoped to one key, released by the same COMMIT, and it stores the state.
Throughput per key is bounded by one short statement at a time, so the whole
take -- lock, refill, all-or-none decision, write -- is one autocommit SQL
statement (`permitSQL`) that never holds the row across a client round trip.
It is measured in `tests/bench` (OPERATIONS §29.6) and is far above any
provider limit this protects. Different keys never contend.

**The retry class.** `ERROR_CLASS_RATE_LIMITED` (common.proto, additive, spec
spelling `rate_limited`) is for a provider 429 or 503 that in-task retries did
not absorb. `TaskError.retry_after_ms` (additive) carries the provider's
`Retry-After`. The workload declares both through `POST /v1/error`; the agent
honours a declared class only when the process exited non-zero and was not
OOM-killed or timed out, and never lets a workload claim `worker_lost`, `oom`,
`timeout` or `infrastructure`, which are the agent's observations. The
scheduler retries a RATE_LIMITED failure **without charging the retry
budget**, up to a cap (`NEXUS_RATE_LIMITED_FREE_RETRIES`, default 10, per node
and run, reset by an operator retry), after `retry_after_ms` clamped to [1 s,
15 min] (or the task's backoff when absent). The free retries are counted in
`task_nodes.free_retries` (migration 0036) in the same transaction that creates
the attempt, and both budget checks subtract them: `ShouldRetry`
(`attempts_used − free_retries`) and the DAG engine's exhaustion test
(`attempt_number − retry_floor − free_retries`). Past the cap a RATE_LIMITED
failure is charged like TRANSIENT, so a provider that never recovers still
ends the node. `no_retry_on: [rate_limited]` vetoes it; an explicit `retry_on`
list that omits it still gets the free retries, because a throttle is the
platform's concern, not the author's error. The attempt history stays
immutable: each retry is a new attempt row.

### 4. Checkpoints: fenced pointers to immutable objects

`ctx.checkpoint(state)` sends the serialized state to the agent, which:

1. uploads it to the result store at
   `<namespace>/checkpoints/<idempotency_key>/<attempt_id>/<seq>` (a new,
   never-overwritten object; `seq` is the agent's per-attempt counter), then
2. registers it with `SaveCheckpoint(attempt, lease, key, sha256, size, seq)`.

The scheduler stores **one pointer per idempotency key** in `task_checkpoints`
(migration 0036). Registration is fenced three ways, in one transaction with
the attempt row held `FOR NO KEY UPDATE` as for usage: the lease must be live, the
attempt in flight, and the new pointer must be **newer** than the stored one —
a higher attempt number, or the same attempt and a higher `seq`. The key must
be exactly under the attempt's own prefix. A stale attempt (reclaimed lease,
superseded by a retry) is refused and its object becomes an orphan. Because
objects are immutable and the pointer only moves forward, a zombie can neither
overwrite a newer checkpoint's bytes nor move the pointer back.

`ctx.resume()` asks the agent, which calls `GetCheckpoint` (fenced; served by
the gateway from `task_checkpoints` by the attempt's idempotency key), fetches
the object, verifies its sha256, and returns the state — or `None` when there
is none or the object is missing (logged; resuming from scratch is always
correct). The idempotency key excludes the attempt number (invariant 1), so
every retry and an operator retry of the node (§18) resume from the same
pointer; a new run has a new key and starts clean (memoization covers reuse
across runs). The maximum checkpoint size is advertised at registration
(`WorkloadApiConfig.max_checkpoint_bytes`, default 64 MiB).

**Cleanup (artifact-gc).** `task_checkpoints` is a reference source. A
checkpoint object is live only while it is the pointer of a node that has not
SUCCEEDED; superseded objects, fenced uploads, the pointers of succeeded nodes
and everything of a deleted run (the pointer row cascades) are orphans, swept
under the existing list-then-query ordering and grace period (ADR-0008). The
agent writes the object strictly before the RPC that references it, which is
the ordering the sweep's safety argument requires.

### 5. Wire and schema changes (all additive)

- `worker.proto`: RPCs `ReportUsage`, `AcquirePermits`, `SaveCheckpoint`,
  `GetCheckpoint`; `RegisterResponse.workload_api` (`WorkloadApiConfig`). A
  gateway that predates them answers UNIMPLEMENTED; the agent only calls them
  when `workload_api` advertised them, and answers the workload 501 otherwise.
- `common.proto`: `ERROR_CLASS_RATE_LIMITED = 10`, `TaskError.retry_after_ms = 6`.
- Spec schema, SDK, `specenum`: the `rate_limited` error-class spelling.
- Migration 0036: `attempt_usage`, `rate_limit_buckets`, `task_checkpoints`,
  the `namespace_rate_limits` view, `task_nodes.free_retries`, grants. Nothing
  existing is altered. A scheduler started ahead of 0036 detects its absence
  and keeps the ledger SQL and retry accounting it had (the ADR-0021
  `DetectQuotaSchema` pattern); the new RPCs then answer Unavailable.
- REST: attempts gain `usage_units` and `usage_reported_at`.

**Ownership (ADR-0011).** The scheduler owns `attempt_usage`,
`rate_limit_buckets` and `task_checkpoints` and writes `task_nodes.free_retries`
(column-level UPDATE, like `attempts_used`). Every service may read them.

### 6. SDK

- `ctx.report_usage({key: n})`, `ctx.report_tokens(provider, model, input=,
  output=, cached=, requests=1)`, `nexus.usage.token_units(...)` and
  `ctx.usage_totals`.
- `ctx.acquire_permits(keys, requests=, tokens=, timeout=)` /
  `ctx.settle_permits(...)`; `nexus.llm.LLMClient`, an OpenAI- and
  Anthropic-compatible client (stdlib `urllib`, no new dependency) that
  acquires permits, retries 429/503 in-task honouring `Retry-After` within a
  bound, settles tokens, reports usage, and raises `nexus.RateLimited` when the
  bound is exhausted.
- `ctx.checkpoint(state)` / `ctx.resume()`.
- `ctx.heartbeat()` / `ctx.progress()` now reach the agent (closes F148).
- Without a workload API (local runner, unit tests, the kubernetes executor)
  the same calls work in-process: usage accumulates on the context, permits use
  a process-local bucket (logged once as not shared), checkpoints go to
  `ctx.store`. `nexus.testing.MockLLMServer` is an OpenAI/Anthropic-compatible
  mock with deterministic token counts and injectable 429s; every test and
  example in this repository uses it, and none calls a real provider.

## Evidence

- `services/scheduler/workload_integration_test.go` (Postgres 16.15, `-race`):
  usage max-merge, validation, stale-lease refusal and the copy into the
  ledger and the outbox; `TestUsageNeverEscapesTheLedger` races four reporters
  against `ApplyResult`, lease expiry and cancel, 24 rounds, and the ledger's
  units always equal the attempt's; checkpoint fencing (stale token, zombie
  after LOST, older seq, foreign prefix, a retry resuming and superseding);
  `TestPermitSQLMatchesTheDomainModel` holds the permit statement to
  `domain.BucketLimit`; four replicas × 8 callers on a 50 req/s key granted
  107–110 permits in 2 s against a ceiling of 112; free RATE_LIMITED retries,
  the provider's Retry-After as the delay, and the cap; the F149 grant.
- **The lock mode was found by that race test.** The first version fenced
  with `FOR KEY SHARE`; overlapping reporters starved `ApplyResult` for
  minutes (see §2). With `FOR NO KEY UPDATE` the same test passes in ~2 s.
- `tests/bench/ratelimit_test.go` (`make bench-ratelimit`, OPERATIONS §29.6):
  one hot key, 8 callers, 817 permits/s (p99 36 ms); 16 spread keys × 4
  replicas, 1,624/s; one hot key with 128 callers, 373/s (p99 1.65 s). The
  first implementation, which held the row lock across three client round
  trips, managed 506 and 118/s on the same cells. Under a saturated 100 req/s
  limit the ceiling held at every replica count. Pull claims beside ~300
  permits/s on one key measured 75–161 claims/s against 175–184 alone (two
  noisy runs on a shared laptop): the claim transaction is unchanged, but the
  permit service shares database CPU.
- `internal/artifactgc/checkpoints_integration_test.go` against Silo: a
  deleting pass keeps only the pointer of an unfinished node.
- On a full stack (`deploy/e2e`, AI overlay, 14 containers):
  `llm-batch-inference` crashed shard 1 after 10 of 30 prompts; the retry
  resumed and asked only 20, and the ledger holds 10 and 20 requests on the
  two attempts. With `rate_limits` of 5 req/s on `mock`, the eval and a second
  batch run succeeded through 693 denials. `llm-eval` with `mock-small`
  failed at its quality gate (0.68 < 0.9).
- Agent tests for the socket API; SDK tests (391+, mypy --strict); the seven
  templates' compile/validate/pytest test; the examples' local self-checks.

## Consequences

**Positive** — token usage reaches the ledger atomically with the attempt's
end, under a key convention a price book can rely on; a provider limit holds
across any number of workers and replicas with no cost to dispatch; a long
shard resumes where it stopped and a zombie cannot corrupt that; Python
heartbeats finally reach the control plane.

**Negative** — every permit is a control-plane round trip (a few ms) and one
row lock; a key's throughput is bounded by one transaction at a time. Rate
limits are enforced only for calls made through the permit API: a task that
calls a provider directly is unmetered and unthrottled. Usage units are
self-reported by the workload and are exactly as trustworthy as the task code
(fine for cost attribution inside a tenant, not a substitute for the
provider's invoice). The kubernetes executor's workload API needs a network
path from task pods to the agent (the addendum; F148).
A checkpoint is at-least-once state, not a transaction with the task's external
effects: the effect journal and idempotency keys still apply.

**Neutral** — the claim path, fair share, quotas and the concurrency semaphore
are unchanged. Usage keys ride in the ledger's existing `custom` object.

## Alternatives considered

1. **Workload talks to the gateway directly with the lease token.** Rejected:
   hands a user process the fencing credential (it could renew its own lease or
   forge its own result) and needs network egress the docker executor denies by
   default.
2. **File-based IPC** (usage and checkpoints written to the work directory).
   Rejected as the primary channel: no request/response for permits, and usage
   in a file is lost when the process or host dies; the socket gives the agent
   the numbers as they happen.
3. **Claim-time admission in the semaphore.** See §3: wrong granularity, and it
   taxes every dispatch.
4. **A leaky bucket in Redis / NATS KV.** Adds a stateful dependency and a
   second source of truth next to Postgres (ADR-0002) for a load Postgres
   carries comfortably at provider-limit rates.
5. **Charging RATE_LIMITED to the normal retry budget.** A burst of throttling
   would exhaust `max_attempts` of correct tasks; unbounded free retries would
   let a dead provider hold a run forever. A capped free budget is the middle.
6. **Usage as a new ledger column.** Cleaner in isolation, but the agreed
   convention with the pricing workstream is the `custom` dimension, and the
   `:` in every key keeps the two namespaces disjoint.
7. **Checkpoint object overwritten in place** (one key per node). Rejected:
   a zombie's late PUT would replace the live attempt's bytes, and object
   stores offer no conditional write we can rely on across providers.

## Addendum (2026-09-26): the workload API for kubernetes pods (F148)

- **Status:** Proposed with the ADR; implemented and verified on a local
  cluster (OPERATIONS §29.10).

### Context

A kubernetes attempt's pod runs on another node, so the per-attempt Unix
socket of §1 is unreachable: F148 stayed open for that executor, and its
tasks kept usage in-process (never in the ledger), took process-local permits
(`workers × limit` against a provider) and wrote checkpoints to `ctx.store`.
The requirements for a fix: the task must never hold an agent credential (the
worker session, the lease token, result-store keys); a credential it does hold
must act only as its own attempt, only for the workload RPCs (usage, permits,
checkpoints, heartbeat and progress, the declared error), and stop working when
the attempt's lease ends; and nothing another tenant's pod holds may work.

### Decision: the agent serves the same API on a TLS listener, routed by the attempt token

- The agent (`NEXUS_K8S_TASK_API_ADVERTISE`) runs ONE TLS listener
  (`taskapi.Network`) and serves every remote attempt's existing per-attempt
  server (`taskapi.Server`: the same handlers, usage batching, fencing and
  close/final-flush ordering as the socket) behind it. A request is routed by
  its bearer token, looked up by SHA-256 so map timing leaks nothing; the
  attempt's own handler then compares the whole header in constant time.
- The token is the socket's: 256 random bits minted by the agent when the
  attempt starts, registered for exactly that attempt, and **revoked when the
  attempt's Run returns** (finished, cancelled, lease revoked): `Close`
  unregisters it before anything else and refuses requests that race it, so no
  usage increment can follow the final snapshot. Until then every upstream call
  carries the attempt's current lease token and is fenced by the gateway and
  again by the scheduler (409 `stale_lease` once the lease is gone).
- The pod receives `NEXUS_TASK_API=https://<advertised host:port>`,
  `NEXUS_TASK_API_CA` (the agent's CA certificate, public) and
  `NEXUS_TASK_API_TOKEN` by `secretKeyRef` to the attempt's existing immutable
  Secret, never as a literal in the pod spec. Task env cannot override them.
- TLS (1.2+): the agent mints a P-256 CA and a serving certificate for the
  advertised host at start, in memory only; the SDK pins the CA and never
  falls back to plain HTTP or to unverified TLS. A restart makes new keys,
  which is also when the previous session's attempts are LOST.
- Tenancy is structural: an agent serves one namespace and routes only tokens
  it minted, so another tenant's pod token is 401 here, and the CA a pod pins
  validates no other agent.
- Network: the one hole in a default-deny task namespace is egress to the
  agent pods on that port (chart: `<agent>-task-api` NetworkPolicy;
  `deploy/k8s/executor-policies.yaml`), the analogue of the docker executor's
  socket mount under `--network none`.

### Alternatives considered

1. **A gateway endpoint with an attempt-scoped token minted at dispatch.** The
   claim would mint a token bound to the lease (a row or a signed claim),
   the pod would call the gateway, and lease deletion would revoke it.
   Rejected, for four reasons:
   - *It loses usage.* A pod's last increments race the agent's
     `ReportResult`; the terminal transaction refuses usage after it (§2), and
     with no agent in the path nothing can order "every increment, then the
     result". The agent's final flush exists precisely for this. The declared
     error class (`POST /v1/error`) is also carried on the agent's result
     report and has no gateway equivalent.
   - *Increments are not idempotent.* The workload posts increments; the
     scheduler stores cumulative snapshots merged by max (§2), which is what
     makes retries and reordering harmless. A stateless, replicated gateway
     would need per-request dedupe state in Postgres to accept increments.
   - *Checkpoints need the result store.* The agent writes the object before
     registering it (the ordering the GC's safety argument rests on); the
     gateway would have to proxy bodies up to 64 MiB into the store.
   - *It opens the control plane to task pods and taxes dispatch.* Pods would
     need egress to the gateway, which ADR-0032's alternative 1 already
     rejected for docker, and minting in the claim adds a write to the one
     transaction ADR-0018 measured as the dispatch ceiling.
2. **A per-attempt sidecar or proxy in the pod.** It would still need a network
   path and a credential to something outside the pod -- the agent (then it is
   this design plus a hop, a second image and its resources per attempt) or
   the gateway (then it holds a lease-capable credential inside a pod the task
   shares a network namespace with, which is what the requirements forbid).
   Rejected: no isolation gained, a moving part added.
3. **An agent-side listener per attempt (one port each).** Structural identity
   by port instead of by token, at the cost of a port range to open, allocate
   and police. The token already gives the same one-attempt identity.

### Consequences

- Kubernetes tasks get usage in the ledger, shared permits, checkpoints and
  resume, heartbeats and declared error classes, with the same guarantees as
  the socket.
- Task pods must reach the agent pod's advertised address (the chart and
  manifests configure it; an agent outside the cluster advertises a routable
  host, e.g. `host.docker.internal` on Docker Desktop). Without it the agent
  warns at start and the SDK's in-process fallback applies, as before.
- The agent's token lives only in its memory: an attempt re-attached after an
  agent restart has no workload API, which matches its fate (it is LOST).

### Evidence

- `agent/internal/taskapi/network_test.go`: TLS only (plain HTTP and TLS 1.1
  refused), verified only against the agent's own CA, a token acts as exactly
  its attempt, revocation on close, 409 on a stale lease, another agent's
  tokens refused, the certificate names the advertised host.
- `agent/internal/executor/kubernetes_taskapi_test.go`: the pod spec carries
  the URL and CA, the token only by Secret reference, nothing in annotations or
  the collector, and task env cannot override them.
- SDK `tests/test_workload_api.py`: https with a pinned CA round-trips usage,
  checkpoints and heartbeats; no CA or another agent's CA is refused, never
  downgraded; plain http is ignored.
- `agent/e2e/k8staskapi` (docker-desktop, Kubernetes 1.34.3, the real
  control-plane binaries, Postgres 18.3, Silo): a pod took three permits,
  reported 30/15 tokens and 3 requests, checkpointed and failed TRANSIENT; the
  retry resumed from the checkpoint and did only the other three calls. Each
  attempt's `attempt_usage` and ledger row hold exactly its units, the
  checkpoint pointer is the retry's, both checkpoint objects exist, the bucket
  granted 6 permits, and the task's heartbeats reached the attempt. A token
  after its attempt ended, a token used while another attempt runs, and each
  tenant's token on the other tenant's agent were refused (401).
