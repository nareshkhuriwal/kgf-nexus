# ADR-0029: Fractional GPUs (MIG and shared devices) and GPU-aware packing in the claim

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0009 (resource vector), ADR-0018 (dispatch ceiling), ADR-0021 (quotas in the claim)
- **Related:** ADR-0030 (spot and model cache), ADR-0031 (multi-cluster placement)

## Context

The resource model (`internal/domain/resource.go`, ADR-0009) knows one kind of
GPU: a whole device, counted by `gpu_count`, with two constraints that are not
quantities -- `gpu_kind` (exact model match) and `gpu_memory_bytes` (a per-device
VRAM *floor*, compared against capacity, never subtracted). The agent's
registry names whole devices by index (`CUDA_VISIBLE_DEVICES`, `--gpus
device=...`). That is the right model for training jobs that own a GPU. It
wastes most of an 80 GB H100 on an inference server that needs 10 GB, and it
cannot express the two ways operators actually share GPUs:

- **MIG** (A100, A30, H100, H200, B200): the GPU is partitioned by the
  administrator into fixed instances with **hardware isolation** of memory,
  cache and SMs (`1g.10gb`, `3g.40gb`, `7g.80gb` ...). Each instance has its own
  UUID (`MIG-...`) and is addressed exactly like a GPU. Kubernetes' NVIDIA device
  plugin exposes them as extended resources `nvidia.com/mig-<profile>` under
  the `mixed` strategy.
- **Shared devices** (MPS or time-slicing): several processes run on one
  physical GPU at once. With **MPS** the server can cap each client's memory
  (`CUDA_MPS_PINNED_DEVICE_MEM_LIMIT`) and SM share
  (`CUDA_MPS_ACTIVE_THREAD_PERCENTAGE`), but a fatal fault in one client can
  take down every client on that device, and all clients must share a UID.
  **Time-slicing** has **no** memory or fault isolation at all. The NVIDIA
  device plugin exposes both as replicated `nvidia.com/gpu` (or renamed
  `nvidia.com/gpu.shared`) resources.

What the code establishes about where sharing can be implemented:

- The ledger is `workers.capacity` / `workers.committed` (JSONB vectors), spent
  only under the worker's row lock in the claim transaction
  (`commitDispatch`, both paths) and returned by subtracting
  `reservations.resources` (`releaseCapacity`, `RMStore.Release`,
  `ReconcileWorker`). **Release subtracts the reservation row, not the
  attempt's request.** So a reservation that records *where* a request was
  placed is released exactly, with no new release path.
- Placement is decided per path. The push path (`Claim` → `selectWorker`)
  already scores every fitting worker with `domain.BestFitScore` and takes the
  minimum. The pull path (`ClaimForWorker`, the default deployment, F81) has
  the worker fixed by the poll: there is nothing to choose between -- a worker
  either takes the head of its eligible queue or it does not.
- The pull path's SQL pre-filter decides which attempts a worker can *ever*
  run (executor, labels, GPU model, VRAM floor). An attempt that passes it but
  fails `domain.Fits` stops the poll there ("wait your turn"). Anything a
  worker can never satisfy must therefore be excluded in SQL, or it blocks that
  worker's queue (the A10/A100 defect recorded in `workerCandidateSQL`).
- Quotas (ADR-0021) run after placement, under the tenant advisory lock, in the
  same transaction. Scoring must not move them.

## Decision

### 1. Three device modes, one vector

`ResourceVector` gains four additive fields (field numbers from 60, like
ADR-0015's 50, so parallel branches cannot collide):

| Field | On a request | On capacity / committed / a reservation |
|---|---|---|
| `mig` `map<string,int64>` | instances needed per MIG profile, e.g. `{"1g.10gb": 1}` | instances per profile the worker has / has reserved |
| `gpu_share_milli` `int32` | fraction of ONE shared device, in thousandths (250 = a quarter) | the sum over reservations (informational; quotas and dashboards) |
| `gpu_share_memory_bytes` `int64` | VRAM needed on that ONE shared device | the sum (informational) |
| `gpu_shares` `map<string,GPUShare{milli,memory_bytes}>` | — (never set by authors) | per shared device: its full size (capacity), what is reserved on it (committed), or **the one device this reservation was placed on** (reservation) |

Whole devices keep `gpu_count`. A physical device is in exactly one mode on a
worker: whole (counted), MIG (its instances appear under `mig`) or shared (it
appears in `gpu_shares`). The agent validates this at start-up.

**MIG instances are pooled per profile.** Every instance of a profile is
interchangeable (same memory, same SM slice), so a per-profile count is exact.
The agent names the instance (its UUID) at admission.

**Shared-device VRAM and compute are subtracted per device.** The fit rule is:
there exists a device `d` with `capacity[d].milli − committed[d].milli ≥ milli`
**and** `capacity[d].memory − committed[d].memory ≥ memory`. A pooled VRAM
total would let two 30 GB jobs "fit" in 20 GB free on each of three devices;
per-device arithmetic cannot. A request that declares only VRAM is charged 0
milli and packs by memory; one that declares only a fraction packs by milli.

**The reservation stores the placed vector** -- the request plus
`gpu_shares{d: {milli, memory}}` -- and `committed` adds it. Release subtracts
exactly that, through the existing release paths. `domain.Place` returns the
placed vector; `commitDispatch` writes it to `reservations.resources` and adds it
to `committed`. `domain.SubVec` clamps per device exactly as it clamps every
other dimension, so a double release stays a no-op.

`gpu_kind` now constrains every GPU form (whole, MIG, shared). `gpu_memory_bytes`
keeps its meaning (a floor on a whole device) and is **not** used for sharing:
shared VRAM is `gpu_share_memory_bytes`, a quantity. Overloading the floor
would have changed the meaning of every existing spec.

### 2. The agent's device ledger names devices and slices

`Registry.Admit` extends the whole-device ledger: it assigns MIG instance UUIDs
per profile and records the shared device the control plane chose, in the same
critical section as the count. The chosen device reaches the agent in a new
`TaskAssignment.reserved` vector (the reservation as committed). The agent
**refuses** an assignment whose device does not fit its own ledger rather than
placing it elsewhere: a silent re-placement would make the control plane's
per-device ledger and the real usage diverge -- the exact over-allocation this
ADR exists to prevent. A refusal lets the lease expire (LOST, the existing
path for a stale-ledger mismatch), and the heartbeat reconciliation releases
the reservation.

Executors expose what was assigned:

| Executor | MIG | Shared device |
|---|---|---|
| docker | `--gpus "device=MIG-…"`, `NVIDIA_VISIBLE_DEVICES`/`CUDA_VISIBLE_DEVICES` = the instance UUIDs | `--gpus device=<idx>`; MPS mode adds `CUDA_MPS_ACTIVE_THREAD_PERCENTAGE` and `CUDA_MPS_PINNED_DEVICE_MEM_LIMIT=<idx>=<MiB>M` and mounts the MPS pipe directory |
| kubernetes | extended resource `nvidia.com/mig-<profile>: n` (device plugin `mixed` strategy) | one unit of the configured shared resource (`NEXUS_K8S_GPU_SHARED_RESOURCE`, e.g. `nvidia.com/gpu.shared`) |
| subprocess | `CUDA_VISIBLE_DEVICES` only -- no isolation, refused unless `NEXUS_ALLOW_UNENFORCEABLE_LIMITS` (as for whole GPUs today) | same |

Every workload also sees `NEXUS_GPU_MODE`, `NEXUS_GPU_SHARE_MILLI` and
`NEXUS_GPU_SHARE_MEMORY_BYTES` so a framework can size itself (for example
`PYTORCH_CUDA_ALLOC_CONF` or vLLM's `gpu_memory_utilization`).

**On Kubernetes the pod requests the reservation.** The device plugin
allocates exactly what the pod spec requests, so the request is built from
what the agent's registry admitted (whole-GPU count, MIG instances per
profile, the shared slice), never from the enforcement ceiling, which may
name no GPU at all (F158). Resource names are validated when the agent starts.

**On Kubernetes the per-device choice is advisory.** The device plugin, not
the agent, decides which physical GPU a pod's replica lands on. The Nexus
ledger still bounds how much is packed onto the *worker*, and MIG profiles are
exact because each instance is its own resource, but per-device VRAM packing is
only enforced by MPS's per-replica limit. The operator documentation says so.

### 3. Isolation is declared, not assumed

| Mode | Memory isolation | Compute isolation | Fault isolation | Enforced by |
|---|---|---|---|---|
| whole | full | full | full | device assignment |
| MIG | hardware | hardware (SM slices) | yes | GPU partitioning |
| MPS | pinned memory limit per client (Volta+) | active-thread percentage | **no** -- one client's fatal fault can kill every client on the device; clients must share a UID | MPS control daemon |
| time-slicing | **none** | none (round-robin) | **none** | nothing: VRAM is an honour system |

A job that exceeds its declared VRAM on a time-sliced device can OOM its
neighbours. Shared devices are therefore opt-in per worker, and a namespace
that runs untrusted code must use MIG or whole devices (MULTITENANCY.md).

### 4. GPU-aware packing: a score, not a new transaction

`domain.PlacementScore` (higher is better) combines, for one worker and one
request:

- **packing**: `1 − BestFitScore` under the `pack` policy (least waste after
  placement; the default, and the behaviour the push path already had), or
  `BestFitScore` under `spread`; GPU dimensions (whole, MIG, shared) weigh 4×
  as they do today;
- the soft preferences of ADR-0030 and ADR-0031 (cache affinity, same worker,
  data locality, spot preference, cost).

The device inside a worker is chosen by the same policy (`pack` = the fitting
device with the least VRAM left after placement; ties by milli, then key).
The policy is scheduler configuration, `NEXUS_PLACEMENT_POLICY=pack|spread`.

**Where the score acts:**

- **Push path:** `selectWorker` already locks every candidate worker; it now
  takes the highest `PlacementScore` among those that fit and pass hard
  eligibility, instead of the lowest `BestFitScore`. Nothing else changes.
- **Pull path: bounded delay scheduling.** The polling worker is fixed, so the
  only decision is *whether this worker should take this attempt now*. For an
  attempt with anything to prefer (a GPU request, a cache or locality
  preference, spot preference, `prefer_same_worker_as`), the claim reads the
  other live, ACTIVE workers of the same tenant and queue **without locking
  them**, and if one would score strictly higher (by a margin) and currently
  fits, it **skips** the attempt -- leaving it PENDING for that worker's own
  poll -- unless the attempt has already waited `NEXUS_PLACEMENT_WAIT` (default
  3 s) since it became due. This is Spark's delay scheduling. It never locks a
  second worker, never commits anything for another worker, and a skip is
  exactly the look-ahead skip the claim already performs. The wait bounds
  starvation: after it, the attempt runs on the first worker that fits. An
  attempt with nothing to prefer -- every CPU-only task without placement
  hints -- never reads other workers, so non-GPU dispatch pays nothing.

**Why this preserves every claim invariant:** the score only chooses (push) or
declines (pull). The candidate row lock, the worker row lock, the fit check
against the locked ledger, the quota check under the tenant advisory lock, the
`state = 'PENDING'` CAS, `UNIQUE(attempt_id)` on leases and the single
transaction are untouched. Over-allocation is decided by `domain.Fits` under
the worker row lock, which the score does not see. A stale read of another
worker's ledger can only produce a needless skip (bounded by the wait) or a
missed preference, never an over-commit.

### 5. The pull pre-filter learns the new dimensions

`workerCandidateSQL` excludes attempts the polling worker can **never** run:
a MIG profile it does not have enough instances of, a shared request larger
than every one of its shared devices, and the hard placement constraints of
ADR-0030/0031. Each clause is guarded by a key-existence test
(`resource_request ? 'mig'`), and new keys are written only when non-zero, so
a CPU-only attempt evaluates one `?` per clause and nothing else.

### 6. Quotas

ADR-0021's dimensions are unchanged. `gpu_count` quotas count whole devices
only; MIG instances and shared fractions are **not** a quota dimension yet
(a follow-up: `max_gpu_share_milli` and `max_mig.<profile>` fit the same
`CheckConcurrent` shape). A namespace that must be capped on fractional GPUs
today can use `max_custom` with a mirrored custom resource. The gap is listed
in OPERATIONS §28.9.

## Evidence

- Unit and property tests (`internal/domain/gpu_share_test.go`,
  `placement_test.go`): for random fleets and random request streams, placing
  and releasing in random order never drives any device's committed milli or
  VRAM, any MIG profile or any whole-GPU count above capacity, and releasing
  everything returns `committed` to zero on every device.
- Integration (`services/scheduler/gpu_share_integration_test.go`, Postgres 16):
  many pull and push replicas race for MIG instances and shared-device VRAM on
  a small fleet while a completer releases at random; a sampler reads every
  worker's reservations continuously; no sample exceeds any device's capacity,
  every reservation's device exists, and a namespace quota set in the same run
  is never exceeded.
- Simulated end to end (`tests/gpusim`, compose): `examples/gpu_sharing`
  packs up to 3 slices on one 40 GiB device (peak 38 GiB) with zero audit
  violations across MIG and shared jobs, and the ledger drains to zero.
- Mutation check (F89): pinning every slice to one device (a pooled model)
  makes the race test report per-device over-allocation within seconds.
- Benchmarks before and after at the ADR-0018 cells, alternated binaries, three
  rounds: OPERATIONS §28.6. Pull-path single-replica p50 is unchanged within
  0.3 ms at 1k/10k/100k backlogs; no regression is distinguishable from the
  (contended) machine's noise.

## Consequences

**Positive** — inference-sized jobs can share a GPU with exact, per-device
accounting; MIG is first class on both executors; packing is explicit and
configurable; no new lock, transaction or release path.

**Negative** — the vector grows two maps, so every claim decodes a little
more JSON (measured). Per-device placement is advisory under Kubernetes.
Delay scheduling can hold an attempt for up to `NEXUS_PLACEMENT_WAIT` while a
better worker is busy; the push path does not wait at all. A worker's GPU mode
is fixed for the life of its session (re-partitioning MIG requires an agent
restart, which is a new session). Time-slicing gives no isolation and is only
as safe as the VRAM figures authors declare.

**Neutral** — `gpu_share_milli`/`gpu_share_memory_bytes` sums on `committed`
are informational; the per-device map is the ledger.

## Alternatives considered

1. **Model MIG profiles and shared GPUs as `custom` resources.** Works for
   counting today and needs no schema, but cannot express "these three numbers
   must fit on the *same* device", cannot carry `gpu_kind`, and the agent could
   not tell which custom keys are GPUs to expose. Rejected; `custom` remains the
   documented escape hatch for quotas.
2. **A pooled VRAM total per worker.** Simple, and wrong: it admits requests no
   single device can hold.
3. **Let the agent choose the shared device.** The agent sees ground truth, but
   the control plane's ledger would then not know which device a reservation
   holds, and could not keep per-device totals honest. Rejected.
4. **Score in SQL.** One source of truth for policy is `internal/domain`, the
   same reason `selectWorker` scores in Go (§5.3 comment). Rejected.
5. **Global push placement for GPU work** (one scheduler computing an optimal
   assignment). Better packing in theory; it gives up ADR-0005's pull model
   and makes a single component responsible for the fleet. Rejected here; see
   ADR-0031.
