"""__WORKFLOW_NAME__ -- sharded batch inference with per-shard checkpoints.

    plan_shards --> infer (one child per shard, created at run time) --> aggregate

  * plan_shards  counts the input records and cuts them into shards of
                 `shard_size`. Only shard DESCRIPTORS travel through the
                 platform (a few dozen bytes each), never the records.
  * infer        a MAP over the shards. Each child scores its slice with the
                 model, writes predictions to `output_prefix/part-NNNNN.jsonl`
                 (atomically, under a deterministic name), and returns a small
                 summary. Memoized: that summary is the shard's CHECKPOINT. If
                 the run fails part way, or you re-run it, finished shards are
                 reused and only missing ones are scored again.
  * aggregate    checks that every shard reported and totals the results. A
                 missing shard fails the run by name, instead of producing a
                 total that silently covers part of the data.

Input: JSON Lines, one record per line, `{"id": ..., "features": [f1, f2, ...]}`,
or `synthetic://N` for N generated records (handy for load-testing the fleet).

GPU: set USE_GPU below (or generate with `nexusctl init batch-inference --gpu`).
The scoring here is plain Python so the template runs anywhere; with a GPU,
replace `_score_batch` with your framework's batched forward pass.

    pytest
    nexusctl workflow compile dag.py --show
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --param source=synthetic://20000 --wait
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import random
from typing import Any, Iterator

from nexus import Cache, Gpu, Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow

IMAGE = "__WORKFLOW_NAME__:0.1.0"

# Request a GPU for each inference shard. Off by default so the template runs
# on any fleet; a worker must advertise a matching GPU for a GPU task to start.
USE_GPU = __USE_GPU__
GPU_KIND = "nvidia-a10"

# The ceiling on shards. max_fanout is mandatory: an input far larger than
# expected fails the plan loudly instead of creating a million tasks.
MAX_SHARDS = 256

# A small default model so the template works out of the box: logistic
# regression over 4 features. Pass your own with --param 'model={...}'.
DEFAULT_MODEL = {"weights": [1.2, -0.8, 0.5, 0.0], "bias": -0.1, "version": "demo-1"}


def _task(name: str, **kwargs: Any):
    ref = f"__WORKFLOW_NAME__.{name}"
    return task(
        task_ref=ref,
        runtime=Runtime(
            executor="docker",
            image=IMAGE,
            command=["python", "-m", "nexus.entrypoint", "--module", "dag"],
            args=[ref],
        ),
        **kwargs,
    )


# ── input ────────────────────────────────────────────────────────────────────


def _synthetic_count(source: str) -> int | None:
    if not source.startswith("synthetic://"):
        return None
    n = int(source[len("synthetic://"):])
    if n <= 0:
        raise ValueError(f"{source}: the record count must be positive")
    return n


def _synthetic_record(i: int) -> dict:
    # Seeded per record, so any shard can regenerate exactly its own slice.
    rng = random.Random(i)
    return {"id": f"rec-{i:08d}", "features": [round(rng.uniform(-2, 2), 4) for _ in range(4)]}


def _path(source: str) -> str:
    return source[len("file://"):] if source.startswith("file://") else source


def _records(source: str, start: int, stop: int) -> Iterator[dict]:
    """Records [start, stop) of the input, streamed rather than loaded whole."""
    if _synthetic_count(source) is not None:
        for i in range(start, stop):
            yield _synthetic_record(i)
        return
    with open(_path(source)) as f:
        for i, line in enumerate(f):
            if i >= stop:
                break
            if i >= start:
                yield json.loads(line)


# ── model ────────────────────────────────────────────────────────────────────


def _score_batch(model: dict, batch: list[dict]) -> list[float]:
    """Logistic regression. Swap for a batched forward pass on the GPU."""
    w, b = model["weights"], model["bias"]
    out = []
    for rec in batch:
        feats = rec["features"]
        if len(feats) != len(w):
            raise ValueError(f"record {rec.get('id')} has {len(feats)} features, the model expects {len(w)}")
        z = b + sum(wi * xi for wi, xi in zip(w, feats))
        out.append(1.0 / (1.0 + math.exp(-z)))
    return out


# ── tasks ────────────────────────────────────────────────────────────────────


@_task(
    "plan_shards",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "infrastructure"]),
)
def plan_shards(ctx: TaskContext, source: str, shard_size: int) -> list:
    """Cut the input into shard descriptors [{shard, source, start, stop}]."""
    if shard_size <= 0:
        raise ValueError("shard_size must be positive")
    total = _synthetic_count(source)
    if total is None:
        with open(_path(source)) as f:
            total = sum(1 for _ in f)
    if total == 0:
        raise ValueError(f"{source} has no records")
    shards = [
        {"shard": i, "source": source, "start": start, "stop": min(start + shard_size, total)}
        for i, start in enumerate(range(0, total, shard_size))
    ]
    if len(shards) > MAX_SHARDS:
        raise ValueError(
            f"{total} records at shard_size={shard_size} is {len(shards)} shards, over MAX_SHARDS={MAX_SHARDS}; "
            "raise shard_size"
        )
    ctx.log.info("planned", records=total, shards=len(shards), shard_size=shard_size)
    return shards


_INFER_RESOURCES = (
    Resources(cpu="2", memory="8Gi", gpu=Gpu(count=1, kind=GPU_KIND))
    if USE_GPU
    else Resources(cpu="1", memory="1Gi")
)


@_task(
    "infer",
    resources=_INFER_RESOURCES,
    timeouts=Timeouts(start_to_close="1h", heartbeat="5m"),
    # Lost workers, OOM and infrastructure errors are worth another attempt,
    # preferably on a different worker in case the host is the problem.
    retry=Retry(max_attempts=3, retry_on=["worker_lost", "infrastructure", "oom", "transient"],
                retry_on_different_worker=True),
    # The per-shard checkpoint: a shard scored once with this model and these
    # inputs is not scored again for 30 days.
    cache=Cache(enabled=True, ttl="30d"),
    outputs={"summary": {"type": "json", "required": True, "max_bytes": 1024}},
)
def infer(ctx: TaskContext, shard: dict, model: dict, output_prefix: str, threshold: float = 0.5,
          batch_size: int = 512) -> dict:
    """Score one shard, write its predictions file, return a checkpoint summary."""
    if USE_GPU:
        ctx.log.info("gpu assignment", cuda_visible_devices=os.environ.get("CUDA_VISIBLE_DEVICES", ""))
    os.makedirs(output_prefix, exist_ok=True)
    final = os.path.join(output_prefix, f"part-{shard['shard']:05d}.jsonl")
    tmp = f"{final}.{ctx.attempt_id or 'local'}.tmp"
    digest = hashlib.sha256()
    rows = positives = 0
    score_sum = 0.0
    expected = shard["stop"] - shard["start"]

    def flush(batch: list[dict], fh) -> None:
        nonlocal rows, positives, score_sum
        for rec, p in zip(batch, _score_batch(model, batch)):
            line = json.dumps({"id": rec["id"], "score": round(p, 6), "label": int(p >= threshold)},
                              sort_keys=True) + "\n"
            fh.write(line)
            digest.update(line.encode())
            rows += 1
            positives += int(p >= threshold)
            score_sum += p

    with open(tmp, "w") as fh:
        batch: list[dict] = []
        for rec in _records(shard["source"], shard["start"], shard["stop"]):
            batch.append(rec)
            if len(batch) == batch_size:
                flush(batch, fh)
                batch = []
                ctx.progress(min(rows / expected, 1.0))
        if batch:
            flush(batch, fh)
    if rows != expected:
        os.unlink(tmp)
        raise ValueError(f"shard {shard['shard']} read {rows} records, expected {expected}; did the input change?")
    # Rename last: a crash mid-write leaves only a .tmp, never a truncated part
    # file that a downstream reader could mistake for complete.
    os.replace(tmp, final)
    return {
        "shard": shard["shard"],
        "rows": rows,
        "positives": positives,
        "score_sum": round(score_sum, 6),
        "output": final,
        "sha256": digest.hexdigest(),
        "model_version": model.get("version", ""),
    }


@_task(
    "aggregate",
    resources=Resources(cpu="250m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m", heartbeat="2m"),
    retry=Retry(max_attempts=1),
    # Run even if shards failed, so the error names every missing shard at once.
    trigger_rule="all_done",
)
def aggregate(ctx: TaskContext, summaries: list, shards: list) -> dict:
    """Verify every shard reported, and total the results."""
    missing = [s["shard"] for s, r in zip(shards, summaries) if r is None]
    if missing or len(summaries) != len(shards):
        raise ValueError(
            f"{len(missing)} of {len(shards)} shards have no result (shards {missing[:20]}); "
            "retry the run: finished shards are reused from their checkpoints"
        )
    rows = sum(r["rows"] for r in summaries)
    expected = sum(s["stop"] - s["start"] for s in shards)
    if rows != expected:
        raise ValueError(f"shards scored {rows} records, expected {expected}")
    positives = sum(r["positives"] for r in summaries)
    report = {
        "records": rows,
        "shards": len(summaries),
        "positives": positives,
        "positive_rate": round(positives / rows, 6),
        "mean_score": round(sum(r["score_sum"] for r in summaries) / rows, 6),
        "outputs": [r["output"] for r in summaries],
    }
    ctx.log.info("aggregated", **{k: v for k, v in report.items() if k != "outputs"})
    return report


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Shard a dataset, score each shard in parallel with per-shard checkpoints, aggregate.",
    params={
        "source": {"type": "string", "required": False, "default": "synthetic://20000"},
        "shard_size": {"type": "int", "required": False, "default": 2000},
        "model": {"type": "json", "required": False, "default": DEFAULT_MODEL},
        "output_prefix": {"type": "string", "required": False, "default": "/data/predictions"},
        "threshold": {"type": "float", "required": False, "default": 0.5},
    },
    run_timeout="6h",
)
def batch_inference(source: str, shard_size: int, model: dict, output_prefix: str, threshold: float):
    shards = plan_shards(source=source, shard_size=shard_size)
    summaries = infer.map(
        shards, item="shard", max_fanout=MAX_SHARDS,
        model=model, output_prefix=output_prefix, threshold=threshold,
    )
    return aggregate(summaries=summaries, shards=shards)


if __name__ == "__main__":
    compiled = batch_inference.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
