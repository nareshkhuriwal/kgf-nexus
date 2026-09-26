# Resources and GPUs

Each task declares a **request** and optionally a **limit**:

```yaml
resources:
  request: {cpu_millis: 2000, memory_bytes: 8589934592, gpu_count: 1,
            gpu_kind: nvidia-a100, gpu_memory_bytes: 42949672960,
            custom: {license_token: 1}}
  limit:   {cpu_millis: 4000, memory_bytes: 8589934592}
```

In the SDK: `Resources(cpu="2", memory="8Gi", gpu=Gpu(count=1, kind="nvidia-a100", memory="40Gi"), custom={"license_token": 1}, cpu_limit="4")`.
Units are parsed on your machine, and the spec carries base units only.

- **The request** is what the scheduler reserves against a worker's
  advertised capacity. The reservation is taken in the same transaction as
  the dispatch, so two schedulers cannot both place work into the same free
  capacity. A task that does not fit anywhere stays `PENDING`, with a
  structured reason such as `waiting_for_resources: gpu (need 4, free 3)`.
- **The limit** is what the executor enforces. `limit == request` gives
  predictable performance. `limit > request` allows bursting.
- **`gpu_kind` and `gpu_memory_bytes`** filter which workers are candidates.
  The ledger counts devices only. Placed GPU tasks get distinct device indices.
- **`custom`** covers any countable resource, such as licences, database
  connections or rate-limited API slots. Workers advertise them with
  `NEXUS_CUSTOM_RESOURCES=license_token=2`, and they are reserved like CPU.

## Advertised capacity versus enforced limits

These are different things, and Nexus keeps them apart:

| Executor | Memory | CPU | GPU |
| --- | --- | --- | --- |
| `docker` | hard (cgroup, OOM kill reported as `oom`) | shares | device isolation |
| `kubernetes` | pod requests and limits | pod requests and limits | device plugin (`NEXUS_K8S_GPU_RESOURCE`) |
| `subprocess` | **cannot be enforced** | **cannot be enforced** | none |

The subprocess executor **refuses** a task that declares limits it cannot
enforce, and the attempt fails as `infrastructure`. Refusing is better than
silently running without limits. A development agent can opt out with
`AllowUnenforceableLimits`.

A worker advertises capacity (`NEXUS_CPU_MILLIS`, `NEXUS_MEMORY_BYTES`,
`NEXUS_GPU_COUNT`, `NEXUS_GPU_KIND`, …). It is a ledger, not a measurement,
and `nexusctl capacity` shows advertised against committed. The console's
worker view also shows measured utilization next to reservations.

!!! warning "Simulated GPUs on a laptop"
    The local compose stack's `agent-gpu` and `agent-gpu-a10` agents advertise
    GPUs they do not have, and are labelled `simulated=true`. They exercise the
    scheduler's GPU accounting, queueing and device assignment, not CUDA. Real
    GPU execution needs compatible hardware; see the
    [GPU jobs recipe](../recipes/gpu-jobs.md).

## Head-of-line blocking and aging

A large task that cannot fit yet does not block smaller tasks that can: the
scheduler looks ahead a bounded window. Without a counterweight, though, a
steady stream of small tasks would starve the large one forever. So a
blocked task's effective priority grows with its age
(`priority + age_seconds × aging_rate`). Once it is high enough, the scheduler
stops back-filling and reserves capacity as it frees. The walk-through is in
[architecture §7.3](../design/architecture.md#73-resource-contention).
