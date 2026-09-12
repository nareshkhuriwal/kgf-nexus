# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""ten_node_mixed -- the Phase 1 exit-criteria DAG.

Architecture §11 states the Phase 1 exit criterion as: "a 10-node DAG with mixed
CPU/GPU tasks completes correctly". This is that DAG, and it is shaped to actually
exercise the scheduler rather than merely to contain ten nodes:

    ingest ──┬── validate_schema ──┬── train_small  (1 GPU, a10)   ──┐
             │                     ├── train_large  (2 GPU, a100)  ──┤
             └── build_features    └── train_baseline (CPU only)   ──┤
                    (CPU, 16Gi)                                      │
                                        evaluate (1 GPU) ────────────┘
                                              │
                                    ┌─────────┴─────────┐
                                 register            publish_report
                                 (license_token)        (CPU)

Ten nodes: ingest, validate_schema, build_features, train_small, train_large,
train_baseline, evaluate, register, publish_report, notify.

What each shape is here to test:

  * FAN-OUT from validate_schema to three trainers, so the resource-aware
    matcher has to place several eligible attempts at once and cannot just run
    a queue in order.
  * FAN-IN into evaluate, so the DAG engine's trigger-rule evaluation has to
    wait on three upstreams rather than one.
  * MIXED CPU/GPU with DIFFERENT gpu_kind values. train_large wants 2x a100 and
    train_small wants 1x a10: a scheduler that only counted GPUs would happily
    place train_large on an a10 host, so the kind constraint is the interesting
    part, not the count.
  * A CUSTOM COUNTABLE resource on register (license_token: 1), which is a
    dimension with no relationship to hardware at all -- it is there to confirm
    the ledger is genuinely multi-dimensional rather than CPU/memory with GPUs
    bolted on.
  * A CONCURRENCY KEY on the trainers, capping how many train simultaneously
    regardless of how much capacity exists. Capacity and concurrency are
    different limits and both have to hold.
  * trigger_rule="all_done" on notify, so the run still notifies when an
    upstream FAILED. all_success would skip exactly when you most want to know.

    nexusctl workflow compile examples/ten_node_mixed/dag.py --show
"""

from __future__ import annotations

from nexus import Concurrency, Gpu, Resources, Retry, TaskContext, Timeouts, task, workflow

# Every GPU task shares this. Declared once because a copy-pasted retry policy is
# a policy that drifts: the numbers here are deliberate, and they should stay the
# same across the trainers.
GPU_RETRY = Retry(
    max_attempts=3,
    backoff="exponential",
    initial="30s",
    max="10m",
    multiplier=2.0,
    jitter="full",
    # OOM is retryable because a retry may land on a larger device. A validation
    # error never is -- the input will be just as bad the second time, and three
    # attempts at it is three GPU-hours nobody gets back.
    retry_on=["transient", "timeout", "worker_lost", "oom"],
    no_retry_on=["validation_error", "permission_denied", "not_found"],
    # A GPU that has started throwing ECC errors will keep doing so, so move.
    retry_on_different_worker=True,
)

GPU_TIMEOUTS = Timeouts(
    schedule_to_start="1h",   # GPUs queue; an hour of waiting is normal, not broken
    start_to_close="4h",
    schedule_to_close="8h",   # the real SLA, across every retry
    heartbeat="5m",
)


# ── 1. ingest (CPU, I/O bound) ───────────────────────────────────────────────
@task(
    task_ref="ml.ingest",
    resources=Resources(cpu="2", memory="4Gi", disk="100Gi"),
    timeouts=Timeouts(schedule_to_start="30m", start_to_close="2h", schedule_to_close="4h"),
    retry=Retry(max_attempts=4, initial="15s"),
    queue="default",
)
def ingest(ctx: TaskContext, dataset_uri: str) -> dict:
    """Pull the raw dataset into scratch. Disk-heavy, so disk is a real request."""
    ctx.heartbeat(f"downloading {dataset_uri}")
    return {"uri": dataset_uri, "bytes": 42_000_000, "records": 1_000_000}


# ── 2. validate_schema (CPU, cheap, fails fast) ──────────────────────────────
@task(
    task_ref="ml.validate_schema",
    resources=Resources(cpu="1", memory="2Gi"),
    timeouts=Timeouts(start_to_close="20m"),
    # A schema violation is deterministic: one attempt is enough, and failing here
    # is how the expensive GPU work downstream gets skipped rather than wasted.
    retry=Retry(max_attempts=1, no_retry_on=["validation_error"]),
)
def validate_schema(ctx: TaskContext, raw: dict) -> dict:
    """Reject a bad dataset before anything expensive touches it."""
    if raw["records"] <= 0:
        raise ValueError("dataset has no records")
    return {"records": raw["records"], "schema_version": 3}


# ── 3. build_features (CPU, memory-heavy) ────────────────────────────────────
@task(
    task_ref="ml.build_features",
    # request is what the scheduler subtracts; limit is what cgroups enforce.
    # Allowing the limit to exceed the request lets this burst into spare memory
    # on a quiet host instead of being killed at exactly 16Gi.
    resources=Resources(cpu="4", memory="16Gi", cpu_limit="8", memory_limit="24Gi", disk="50Gi"),
    timeouts=Timeouts(start_to_close="3h", schedule_to_close="6h", heartbeat="10m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "timeout", "worker_lost", "oom"]),
    cache=True,  # pure function of the validated dataset, so memoization is valid
)
def build_features(ctx: TaskContext, validated: dict) -> dict:
    """Featurize. Cacheable because it is a pure function of its input."""
    for i in range(10):
        ctx.progress((i + 1) / 10, "featurizing")
    return {"feature_count": 512, "records": validated["records"]}


# ── 4-6. the three trainers: the fan-out, and the mixed-resource core ────────
@task(
    task_ref="ml.train_small",
    resources=Resources(cpu="4", memory="16Gi", gpu=Gpu(count=1, kind="nvidia-a10", memory="24Gi")),
    timeouts=GPU_TIMEOUTS,
    retry=GPU_RETRY,
    queue="gpu",
    # Caps simultaneous training across the whole cluster, independently of how
    # much GPU capacity exists. Capacity says "can this fit"; concurrency says
    # "should this many run at once" -- often a licence or a shared-NAS limit.
    concurrency=Concurrency(key="model-training", limit=2),
)
def train_small(ctx: TaskContext, features: dict) -> dict:
    """Train the small model on one A10."""
    ctx.heartbeat("epoch 1")
    return {"model": "small", "accuracy": 0.91, "gpu_hours": 1.5}


@task(
    task_ref="ml.train_large",
    # Two A100s with a 40Gi VRAM floor. gpu_kind and gpu_memory_bytes are
    # PLACEMENT constraints, not ledger dimensions: the scheduler filters hosts
    # by them, then decrements the device count. A scheduler that only counted
    # devices would place this on two A10s and OOM four hours in.
    resources=Resources(
        cpu="8", memory="64Gi", disk="200Gi",
        gpu=Gpu(count=2, kind="nvidia-a100", memory="40Gi"),
    ),
    timeouts=Timeouts(
        schedule_to_start="2h",   # two A100s on one host is a long wait, legitimately
        start_to_close="8h",
        schedule_to_close="20h",
        heartbeat="10m",
    ),
    retry=GPU_RETRY,
    queue="gpu",
    node_selector={"gpu-interconnect": "nvlink"},  # multi-GPU wants fast peer links
    concurrency=Concurrency(key="model-training", limit=2),
)
def train_large(ctx: TaskContext, features: dict) -> dict:
    """Train the large model on two NVLink-connected A100s."""
    ctx.heartbeat("epoch 1")
    return {"model": "large", "accuracy": 0.96, "gpu_hours": 12.0}


@task(
    task_ref="ml.train_baseline",
    # No GPU at all. Present so the DAG genuinely mixes: a CPU-only sibling has to
    # be placeable on a host with no GPU, and must not be blocked behind the GPU
    # queue.
    resources=Resources(cpu="8", memory="32Gi"),
    timeouts=Timeouts(start_to_close="2h", schedule_to_close="5h"),
    retry=Retry(max_attempts=2),
    queue="default",
)
def train_baseline(ctx: TaskContext, features: dict) -> dict:
    """A CPU-only baseline, so there is something to compare the GPUs against."""
    return {"model": "baseline", "accuracy": 0.78, "gpu_hours": 0.0}


# ── 7. evaluate (GPU, the fan-in) ────────────────────────────────────────────
@task(
    task_ref="ml.evaluate",
    resources=Resources(cpu="4", memory="16Gi", gpu=Gpu(count=1)),  # any GPU will do
    timeouts=Timeouts(start_to_close="1h", schedule_to_close="3h"),
    retry=GPU_RETRY,
    queue="gpu",
    # Data-locality hint, soft: the features are already on that host's scratch
    # disk. A hint rather than a constraint, because waiting for one specific
    # worker is usually worse than copying the data.
    prefer_same_worker_as="build_features",
)
def evaluate(ctx: TaskContext, small: dict, large: dict, baseline: dict) -> dict:
    """Compare all three models. Three upstreams, hence the fan-in."""
    candidates = [small, large, baseline]
    best = max(candidates, key=lambda m: m["accuracy"])
    ctx.log.info("evaluated", best=best["model"], accuracy=best["accuracy"])
    return {"best_model": best["model"], "accuracy": best["accuracy"]}


# ── 8. register (CPU, external effect, custom resource) ──────────────────────
@task(
    task_ref="ml.register",
    resources=Resources(
        cpu="1", memory="2Gi",
        # An arbitrary countable constraint with no hardware meaning. The model
        # registry is licensed for three concurrent writers; the scheduler treats
        # this exactly like CPU, which is the point of a typed resource vector.
        custom={"license_token": 1},
    ),
    timeouts=Timeouts(start_to_close="30m"),
    retry=Retry(max_attempts=5, initial="10s", retry_on=["transient", "timeout", "worker_lost"]),
    # The only task here that mutates something outside the run. `required` is a
    # claim the handler has to earn -- see examples/idempotent_effect.
    idempotency="required",
)
def register(ctx: TaskContext, evaluation: dict) -> dict:
    """Register the winning model. Guarded, because a double-register is visible."""
    if ctx.effect_recorded("register"):
        return ctx.previous_result("register")
    receipt = {"registered": evaluation["best_model"], "version": 7}
    ctx.record_effect("register", receipt)
    return receipt


# ── 9. publish_report (CPU) ──────────────────────────────────────────────────
@task(
    task_ref="ml.publish_report",
    resources=Resources(cpu="1", memory="2Gi"),
    timeouts=Timeouts(start_to_close="20m"),
    retry=Retry(max_attempts=3),
)
def publish_report(ctx: TaskContext, evaluation: dict) -> dict:
    """Render and publish the comparison report."""
    return {"report_url": f"s3://reports/{evaluation['best_model']}.html"}


# ── 10. notify (CPU, all_done) ───────────────────────────────────────────────
@task(
    task_ref="ml.notify",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    retry=Retry(max_attempts=3, initial="5s"),
    # all_done, not all_success: this runs whether the upstreams succeeded or
    # failed. With all_success it would be SKIPPED on failure -- precisely when a
    # notification is most wanted.
    trigger_rule="all_done",
)
def notify(ctx: TaskContext, registration: dict) -> dict:
    """Tell someone the pipeline finished, whichever way it finished."""
    ctx.log.info("notifying")
    return {"notified": True}


@workflow(
    name="ten-node-mixed",
    description="10-node mixed CPU/GPU DAG with fan-out and fan-in (Phase 1 exit criteria).",
    params={"dataset_uri": {"type": "string", "required": True}},
    run_timeout="24h",
    # fail_fast is deliberately OFF. The trainers are independent: if the A100
    # pair never becomes available, the small and baseline models are still worth
    # having, and killing them would throw away hours of finished GPU work.
    fail_fast=False,
    max_active_runs=2,
    labels={"team": "ml-platform", "tier": "production"},
)
def ten_node_mixed(dataset_uri: str):
    """The DAG. Read the module docstring for the shape and what it exercises."""
    raw = ingest(dataset_uri)
    validated = validate_schema(raw)
    features = build_features(validated)

    # Fan-out: three trainers from one upstream. They are independent, so the
    # scheduler must be able to have all three eligible simultaneously.
    small = train_small(features)
    large = train_large(features)
    baseline = train_baseline(features)

    # Fan-in: evaluate waits on all three. The parameter NAMES here become the
    # downstream input names in each edge's output_to_input mapping.
    evaluation = evaluate(small=small, large=large, baseline=baseline)

    registration = register(evaluation)
    publish_report(evaluation)
    return notify(registration)


if __name__ == "__main__":
    compiled = ten_node_mixed.compile()
    print(compiled.to_json(indent=2))
    print(f"\ntasks: {len(compiled.spec['tasks'])}  edges: {len(compiled.spec['edges'])}")
    print(f"spec hash: {compiled.hash}")
