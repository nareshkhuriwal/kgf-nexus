# GPU jobs

## Requesting GPUs

```python
from nexus import Gpu, Resources, task

@task(resources=Resources(cpu="4", memory="32Gi", gpu=Gpu(count=2, kind="nvidia-a100", memory="40Gi")),
      queue="gpu")
def train(ctx, ...): ...
```

```yaml
resources:
  request: {cpu_millis: 4000, memory_bytes: 34359738368,
            gpu_count: 2, gpu_kind: nvidia-a100, gpu_memory_bytes: 42949672960}
queue: gpu
```

- `gpu_count` is reserved from the worker's ledger atomically at dispatch,
  and a task that cannot fit waits with a structured reason.
- `gpu_kind` and `gpu_memory_bytes` restrict which workers are candidates.
- Placed tasks get **distinct device indices**. The executor sets
  `CUDA_VISIBLE_DEVICES`. The docker executor also isolates the devices, the
  kubernetes executor requests `NEXUS_K8S_GPU_RESOURCE` (for example
  `nvidia.com/gpu`) and maps `gpu_kind` to a node selector, and the
  subprocess executor pins by convention only.
- Big jobs are not starved by a stream of small ones:
  [aging](../concepts/resources.md#head-of-line-blocking-and-aging).

## Advertising GPUs

A worker advertises `NEXUS_GPU_COUNT`, `NEXUS_GPU_KIND` and
`NEXUS_GPU_MEMORY_BYTES`, and serves a queue such as `gpu`. On Kubernetes,
dedicate a node pool: set the agent's `NEXUS_K8S_NODE_SELECTOR` and
`NEXUS_K8S_TOLERATIONS` so its pods land on GPU nodes.

## What the local stack can and cannot tell you

!!! warning "Simulated GPUs"
    The compose stack's `agent-gpu` (2 × "A100") and `agent-gpu-a10` agents are
    labelled `simulated=true` and have no GPUs. They are good for testing the
    **scheduler**: accounting, queueing under contention, device-index
    assignment, and `gpu_kind` placement. They say nothing about CUDA, drivers,
    device memory, or whether your image runs on a real device.

- [`examples/gpu_scheduling`](repo:examples/gpu_scheduling/): 8 jobs (1×A100,
  2×A100 and A10) contending for 2 A100s and 1 A10. An audit fails the run on
  oversubscription, a wrong GPU model, or two concurrent jobs sharing a device.
- [`examples/image_processing_gpu`](repo:examples/image_processing_gpu/): the
  stages reserve real (simulated) GPU slots, but the pixel maths runs on the
  CPU in `awk`. On real nodes, swap each stage's runtime for a CUDA image.

Running GPU workloads on real, compatible hardware has **not** been qualified
in this repository. The project brief lists it as a requirement, and it still
needs doing. Budget for doing it on your hardware before you rely on GPU
placement in production.
