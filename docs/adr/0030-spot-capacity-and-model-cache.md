# ADR-0030: Spot/preemptible capacity, eviction handling, and node-local model caches

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0006 (retry semantics), ADR-0010 (leases), ADR-0015 (bounded uninterruptible tasks)
- **Related:** ADR-0029 (placement score), ADR-0031 (multi-cluster placement)

## Context

GPU capacity is expensive, and the cheapest GPU capacity is preemptible: AWS
Spot (a two-minute interruption notice), GCP Spot/preemptible VMs (about 30 s),
Azure Spot (Scheduled Events, at least 30 s). A platform that cannot tell spot
from on-demand either keeps work off spot entirely (cost) or charges every
eviction to the task's retry budget (a three-attempt task fails after three
reclaims that were not its fault). Separately, model weights and datasets are
the largest inputs GPU jobs have, often tens of GB, and re-downloading them on
every attempt dominates start-up time.

What the code establishes:

- Workers advertise `labels` at registration, and `node_selector` is a hard
  jsonb containment constraint in both claim paths. There is no notion of
  capacity type.
- The agent already has a graceful teardown: `Runner.Cancel` sends SIGTERM,
  waits the grace, then SIGKILL; ADR-0015 bounds uninterruptible tasks; DRAIN
  stops polling (`Registry.SetDraining`). There is no eviction source.
- A task's failure is classified on the worker (`ErrorClass`) and the retry
  decision (`domain.ShouldRetry`) compares `task_nodes.attempts_used` against
  `max_attempts`. The retry timer (`scheduler_timers.go`) charges
  `attempts_used` when it creates the next attempt. There is one budget.
- A lease that expires because the node vanished becomes LOST
  (`WORKER_LOST`), whose retry is gated by idempotency (§6.5). That must not
  change: an attempt whose worker disappeared without a word *might* have
  finished.
- `TaskSpec.prefer_same_worker_as` has been parsed since Phase 0 and is read by
  no code.

## Decision

### 1. Capacity type and task preference

Workers advertise capacity type with the well-known label
**`nexus.io/capacity-type`** = `on-demand` | `spot` (absent = on-demand). The
agent sets it from `NEXUS_CAPACITY_TYPE`, or discovers it from its Kubernetes
Node's labels (`karpenter.sh/capacity-type`, `eks.amazonaws.com/capacityType`,
`cloud.google.com/gke-spot`, `cloud.google.com/gke-preemptible`,
`kubernetes.azure.com/scalesetpriority`) when it can read its Node. A label,
not a new column or wire field: it flows through the existing registration,
storage, `node_selector` matching and console display, and it can be set by
operators who never upgrade the agent.

Tasks declare `placement.capacity` (new `PlacementSpec`, TaskSpec field 60):

| Value | Meaning |
|---|---|
| `spot_allowed` (default, and what every existing spec means) | runs on either |
| `spot_preferred` | runs on either; spot scores higher (ADR-0029 §4) |
| `on_demand_only` | never dispatched to a spot worker (hard eligibility, in the pull pre-filter and in `selectWorker`) |

`on_demand_preferred` is deliberately absent: it is `spot_allowed` plus the
cost hook (§5), and a fourth value would have to be defined against it.

### 2. Eviction notices: pluggable sources, one drain

`agent/internal/eviction` defines `Source` (`Watch(ctx, out chan<- Notice)
error`; a `Notice` carries the source, reason and the deadline by which the
node will be gone), and `Merge` delivers the first notice of any source.
Every source polls (default 5 s) with capped, jittered backoff on transient
errors. Implementations:

| Source | Signal |
|---|---|
| `kubernetes` | the agent's own Node (read with `get`) gains a configured taint (defaults: `cloud.google.com/impending-node-termination`, `aws-node-termination-handler/spot-itn`, `karpenter.sh/disruption`, `ToBeDeletedByClusterAutoscaler`), gets a deletion timestamp, or disappears. Risk signals -- `rebalance-recommendation`, `DeletionCandidateOfClusterAutoscaler` -- are deliberately not defaults: they mean "may go", and draining on them would checkpoint-kill work on nodes that usually survive |
| `aws` | IMDSv2 `latest/meta-data/spot/instance-action` (token first, cached; 404 = no action) |
| `gcp` | `computeMetadata/v1/instance/preempted` = `TRUE` (deadline now + 30 s) |
| `azure` | Scheduled Events `EventType` `Preempt` or `Terminate` for this instance |
| `file` | a file appears (`NEXUS_EVICTION_FILE`): simulation, tests, and node-local handlers |

The agent's own pod deletion needs no source: the kubelet sends the agent
SIGTERM, which already drains. When the process was told by an eviction source
first, the drain becomes an eviction drain (below).

On the first notice the agent:

1. stops admitting and polling (the DRAIN state of §5.5) and reports
   `HeartbeatRequest.drain_reason = "eviction:<source>"` on its next beat, which
   the gateway turns into `DRAINING` in the ledger so the push path stops too;
2. marks every running attempt *preempting* and sends it SIGTERM -- the
   existing checkpoint signal -- with grace `min(DefaultGraceAfterTerm,
   deadline − now − 5 s)`; an uninterruptible task (ADR-0015) waits until the
   same bound and is then killed;
3. reports each attempt that ends non-zero while preempting as `FAILED` with
   the new class **`ERROR_CLASS_PREEMPTED`** (10). An attempt that exits 0 is a
   success: the task decided it was done. A checkpointing task that wants to be
   resumed should exit non-zero (conventionally 143) after checkpointing.

Tasks see `NEXUS_PREEMPTIBLE=true` on spot workers so they can checkpoint more
often.

### 3. A separate budget for preemption

A `PREEMPTED` failure does **not** consume `attempts_used`. It consumes
`task_nodes.preemptions_used` (migration 0035), capped by
`placement.max_preemptions` (0 = the platform default,
`NEXUS_MAX_PREEMPTION_RETRIES`, default 5). Under the cap the retry is
scheduled with a short fixed delay (`NEXUS_PREEMPTION_RETRY_DELAY`, 2 s) and
reason `retry_preempted`, and the retry timer charges `preemptions_used`
instead of `attempts_used` (the timer payload says which). Over the cap, the
attempt falls through to the ordinary decision with `PREEMPTED` treated as a
retryable class (like `INFRASTRUCTURE`), so it then spends the normal budget:
a task that keeps landing on reclaimed nodes eventually fails visibly instead of
retrying forever. `no_retry_on: [preempted]` is honoured as for any class.

**LOST stays LOST.** If the node disappears before the agent reports, the lease
expires and the attempt is `WORKER_LOST`, gated by idempotency exactly as
before. The preemption budget is only for the case the agent *knows*: it sent
the signal and saw the process end.

### 4. Node-local model and dataset cache

Tasks declare cacheable artifacts in `placement.artifacts`
(`{name, key, uri, size_bytes}`), where `key` is `sha256:<hex>` of the
content -- or empty, in which case the agent keys by URI and verifies nothing
it cannot verify (reported as such). The agent keeps an **LRU disk cache**
(`agent/internal/artifactcache`):

- **Bounded** by `NEXUS_ARTIFACT_CACHE_BYTES` (and `..._MAX_ENTRIES`). An entry
  in use by a running attempt is **pinned** and never evicted; eviction takes
  unpinned least-recently-used entries until the new one fits, and refuses the
  fetch (INFRASTRUCTURE, retryable) if pinned entries alone exceed the bound.
- **Integrity**: content is hashed while it downloads into a temporary file in
  the cache directory and renamed into place only if the digest matches the
  declared key; a hit re-verifies size, and `NEXUS_ARTIFACT_CACHE_VERIFY=hit`
  re-hashes on every hit. A mismatch deletes the entry and fails the attempt
  with a VALIDATION error naming the expected and actual digest.
- **Concurrency**: one fetch per key at a time (single-flight); concurrent
  attempts wait for the same download.
- **Exposure**: `NEXUS_ARTIFACT_<NAME>` (path) and `NEXUS_ARTIFACT_DIR`;
  docker bind-mounts each entry read-only at `/nexus/artifacts/<name>`.
  The kubernetes executor's pods run on other nodes, so an agent that only runs
  the kubernetes executor advertises nothing and places no cache preference.
- **Advertising**: the heartbeat carries up to 256 cached keys
  (`HeartbeatRequest.cached_artifacts`, most recently used first), stored in
  `workers.cached_artifacts` (migration 0035) and only rewritten when the set
  changes. Old agents send nothing and their column stays empty.
- **Metrics**: `nexus_agent_artifact_cache_hits_total`, `..._misses_total`,
  `..._evictions_total`, `..._bytes`, `..._fetch_seconds`,
  `..._integrity_failures_total`.

Placement prefers workers holding the artifacts through the ADR-0029 score
(fraction of declared bytes already cached, weighted above packing), so it is a
**soft** preference: a missing cache costs a download, never a stall.

### 5. `prefer_same_worker_as`, finally read

The score adds a bonus when the candidate worker's *hostname* equals that of
the worker whose attempt of the named node most recently SUCCEEDED in the same
run. Hostname, not worker id: ids are per session (ADR-0010 point 7), and the
data a preference like this is about lives on the host. Soft, like every
preference.

### 6. Cost hook

`domain.CostModel` (`Cost(worker WorkerAttrs, request) float64`, lower is
cheaper) feeds the score. The default reads `nexus.io/cost-tier` (0 cheapest).
A cost layer can replace it later without touching the claim.

## Consequences

**Positive** — spot capacity is usable for work that tolerates it, evictions do
not burn task budgets, the task gets its checkpoint signal with a deadline it
can meet, and large models are downloaded once per node.

**Negative** — eviction handling depends on the notice arriving; a node that
dies without one produces LOST attempts as before. The kubernetes source needs
`get/watch` on its own Node (a ClusterRole the chart adds only when enabled).
The cache uses node disk that is not accounted in `disk_bytes` (it is
configured separately, and documented). A task that exits 0 after being told to
stop is a success; authors must follow the exit-code convention to be
resumed. Artifacts keyed only by URI cannot be integrity-checked.

## Alternatives considered

1. **A first-class `capacity_type` column and wire field.** Stronger typing,
   but a label already gives registration, storage, matching and display, and
   works with agents that predate this ADR. Rejected.
2. **Reclassifying LOST as preempted when the worker had announced an
   eviction.** Tempting, but LOST means "might have finished", and
   idempotency-NONE work must never be auto-retried on that basis. Rejected.
3. **Using the executor's image layer cache (bake models into images).** Good
   practice and still recommended; it cannot share one 70 GB model across
   images or tasks, and gives the scheduler nothing to prefer. Complementary.
4. **A cluster-wide cache service.** Out of scope; the agent cache composes with
   one (the URI can point at it).
