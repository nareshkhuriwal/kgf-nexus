# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""ml_capacity_test -- synthetic ML training fan-out for Nexus capacity testing.

The workflow intentionally combines four kinds of pressure:

    generate_data
        |\
        | +--> train_00 --> evaluate_00 --\
        | +--> train_01 --> evaluate_01 ---+
        | ...                               +--> capacity_report
        | +--> train_15 --> evaluate_15 ---+
        |__________________________________/

* Result-store / network pressure:
  ``generate_data`` creates a multi-megabyte CSV which is consumed by every
  trainer and evaluator. This exercises artifact fan-out through the Nexus
  result store instead of tiny inline values.
* Memory pressure:
  every trainer materializes the full dataset in memory, similar to a normal
  scikit-learn style fit.
* CPU pressure:
  every trainer fits logistic regression from scratch for multiple epochs.
* Scheduler pressure:
  16 model candidates are ready at the same time, each requesting 1 vCPU and
  768 MiB. A smaller worker pool should queue them; a larger pool can execute
  more concurrently.

The final report does not merely merge model metrics. Each train/evaluate task
records epoch timestamps, allowing the report to calculate observed overlap,
training-wave wall time and aggregate row-pass throughput.

Suggested runs:

    # Fast functional check
    make submit-example EXAMPLE=ml_capacity_test \
        PARAMS="rows=20000 features=16 epochs=3 seed=7"

    # Default capacity run
    make submit-example EXAMPLE=ml_capacity_test \
        PARAMS="rows=60000 features=24 epochs=8 seed=7"

    # Heavier CPU / memory / artifact-store run
    make submit-example EXAMPLE=ml_capacity_test \
        PARAMS="rows=150000 features=32 epochs=15 seed=7"

Static fan-out is controlled by CANDIDATES. Increase it and recompile when you
want to test 32/64-way scheduler pressure.

Runtime: every step is Python, so every step runs in the DOCKER executor from
a pinned Python image (PYTHON_IMAGE). It used to run `python3` in the
subprocess executor, which assumes the agent host has Python: the compose
agents are Alpine images without it, so generate_data failed with
ERROR_CLASS_NOT_FOUND (F150). Declaring `docker` is also a placement
constraint -- the scheduler only offers these tasks to workers that advertise
the docker executor (compose agents do once their Docker socket is usable;
an agent with NEXUS_EXECUTORS set must list docker) -- so a fleet without one
leaves the run PENDING with that reason instead of failing it. The steps need no network and run under the executor's default
`--network none`.
"""

from __future__ import annotations

from nexus import Resources, Runtime, TaskContext, Timeouts, task, workflow

CANDIDATES = 16
LEARNING_RATES = (0.005, 0.01, 0.02, 0.04)
L2_VALUES = (0.0, 0.0001, 0.001, 0.005)

_GENERATE_RES = Resources(cpu="500m", memory="512Mi")
_TRAIN_RES = Resources(cpu="1000m", memory="768Mi")
_EVAL_RES = Resources(cpu="500m", memory="512Mi")
_REPORT_RES = Resources(cpu="250m", memory="256Mi")
_TIMEOUT = Timeouts(start_to_close="30m")

# Official Python image, pinned by digest (the multi-arch index, so amd64 and
# arm64 workers resolve the same release). Standard library only.
PYTHON_IMAGE = "python:3.13-alpine@sha256:79e7a9b9ff1cbceff819f856fb374477792a5967759d94df266de7b7b4120e6f"


def _python(script: str) -> Runtime:
    return Runtime(executor="docker", image=PYTHON_IMAGE, command=["python3", "-c"], args=[script])

_GENERATE_SCRIPT = r'''
import csv
import json
import math
import os
import random
import time

inp = os.environ["NEXUS_INPUT_DIR"]
out = os.environ["NEXUS_OUTPUT_DIR"]

rows = int(json.load(open(os.path.join(inp, "rows"))))
features = int(json.load(open(os.path.join(inp, "features"))))
seed = int(json.load(open(os.path.join(inp, "seed"))))

if rows < 1000:
    raise SystemExit("rows must be >= 1000")
if features < 2 or features > 128:
    raise SystemExit("features must be between 2 and 128")

rng = random.Random(seed)
truth = [rng.uniform(-2.0, 2.0) for _ in range(features)]
bias = rng.uniform(-0.5, 0.5)

path = os.path.join(out, "dataset")
started = time.time()
positives = 0

with open(path, "w", newline="") as f:
    w = csv.writer(f)
    w.writerow(["row_id", *[f"x{i:02d}" for i in range(features)], "label"])
    for row_id in range(rows):
        x = [rng.uniform(-1.0, 1.0) for _ in range(features)]
        z = bias + sum(a * b for a, b in zip(truth, x)) + rng.gauss(0.0, 0.35)
        p = 1.0 / (1.0 + math.exp(-max(-30.0, min(30.0, z))))
        y = 1 if rng.random() < p else 0
        positives += y
        w.writerow([row_id, *[f"{v:.6f}" for v in x], y])

manifest = {
    "rows": rows,
    "features": features,
    "positive_rows": positives,
    "negative_rows": rows - positives,
    "dataset_bytes": os.path.getsize(path),
    "generate_seconds": round(time.time() - started, 6),
    "seed": seed,
}
with open(os.path.join(out, "manifest"), "w") as f:
    json.dump(manifest, f, separators=(",", ":"))
print(json.dumps(manifest, separators=(",", ":")))
'''

_TRAIN_SCRIPT = r'''
import csv
import json
import math
import os
import random
import time

inp = os.environ["NEXUS_INPUT_DIR"]
out = os.environ["NEXUS_OUTPUT_DIR"]

candidate = int(json.load(open(os.path.join(inp, "candidate"))))
epochs = int(json.load(open(os.path.join(inp, "epochs"))))
lr = float(json.load(open(os.path.join(inp, "learning_rate"))))
l2 = float(json.load(open(os.path.join(inp, "l2"))))
seed = int(json.load(open(os.path.join(inp, "seed"))))

started_epoch = time.time()
started_perf = time.perf_counter()

# Materialize the full data set to create realistic per-trainer memory pressure.
X = []
y = []
with open(os.path.join(inp, "dataset"), newline="") as f:
    r = csv.reader(f)
    next(r)
    for idx, row in enumerate(r):
        # Keep 20% for evaluate; training uses the remaining 80%.
        if idx % 5 == 0:
            continue
        X.append([float(v) for v in row[1:-1]])
        y.append(int(row[-1]))

if not X:
    raise SystemExit("training set is empty")

features = len(X[0])
rng = random.Random(seed * 1000 + candidate)
weights = [rng.uniform(-0.01, 0.01) for _ in range(features)]
bias = 0.0
indices = list(range(len(X)))
last_loss = 0.0

for epoch in range(epochs):
    # Stable deterministic shuffle for each candidate/epoch.
    rng.shuffle(indices)
    loss = 0.0
    for j in indices:
        row = X[j]
        target = y[j]
        z = bias
        for k, value in enumerate(row):
            z += weights[k] * value
        z = max(-30.0, min(30.0, z))
        p = 1.0 / (1.0 + math.exp(-z))
        err = p - target
        # Logistic loss for observability.
        p_clip = min(1.0 - 1e-12, max(1e-12, p))
        loss += -(target * math.log(p_clip) + (1 - target) * math.log(1 - p_clip))
        for k, value in enumerate(row):
            weights[k] -= lr * (err * value + l2 * weights[k])
        bias -= lr * err
    last_loss = loss / len(X)

ended_perf = time.perf_counter()
ended_epoch = time.time()

model = {
    "candidate": candidate,
    "learning_rate": lr,
    "l2": l2,
    "epochs": epochs,
    "features": features,
    "train_rows": len(X),
    "train_row_passes": len(X) * epochs,
    "train_loss": round(last_loss, 8),
    "train_seconds": round(ended_perf - started_perf, 6),
    "train_start_epoch": round(started_epoch, 6),
    "train_end_epoch": round(ended_epoch, 6),
    "weight_norm": round(math.sqrt(sum(v * v for v in weights)), 8),
    "bias": bias,
    "weights": weights,
}
with open(os.path.join(out, "model"), "w") as f:
    json.dump(model, f, separators=(",", ":"))
print(json.dumps({k: model[k] for k in model if k != "weights"}, separators=(",", ":")))
'''

_EVAL_SCRIPT = r'''
import csv
import json
import math
import os
import time

inp = os.environ["NEXUS_INPUT_DIR"]
out = os.environ["NEXUS_OUTPUT_DIR"]
model = json.load(open(os.path.join(inp, "model")))
weights = model["weights"]
bias = float(model["bias"])

started_epoch = time.time()
started_perf = time.perf_counter()

count = correct = tp = tn = fp = fn = 0
loss = 0.0

with open(os.path.join(inp, "dataset"), newline="") as f:
    r = csv.reader(f)
    next(r)
    for idx, row in enumerate(r):
        if idx % 5 != 0:
            continue
        x = [float(v) for v in row[1:-1]]
        target = int(row[-1])
        z = bias + sum(a * b for a, b in zip(weights, x))
        z = max(-30.0, min(30.0, z))
        p = 1.0 / (1.0 + math.exp(-z))
        pred = 1 if p >= 0.5 else 0
        p_clip = min(1.0 - 1e-12, max(1e-12, p))
        loss += -(target * math.log(p_clip) + (1 - target) * math.log(1 - p_clip))
        count += 1
        correct += int(pred == target)
        if pred == 1 and target == 1:
            tp += 1
        elif pred == 0 and target == 0:
            tn += 1
        elif pred == 1:
            fp += 1
        else:
            fn += 1

if count == 0:
    raise SystemExit("evaluation set is empty")

ended_perf = time.perf_counter()
ended_epoch = time.time()
eval_seconds = ended_perf - started_perf
precision = tp / (tp + fp) if (tp + fp) else 0.0
recall = tp / (tp + fn) if (tp + fn) else 0.0
f1 = 2 * precision * recall / (precision + recall) if (precision + recall) else 0.0

metrics = {
    "candidate": model["candidate"],
    "learning_rate": model["learning_rate"],
    "l2": model["l2"],
    "epochs": model["epochs"],
    "train_rows": model["train_rows"],
    "train_row_passes": model["train_row_passes"],
    "train_loss": model["train_loss"],
    "train_seconds": model["train_seconds"],
    "train_start_epoch": model["train_start_epoch"],
    "train_end_epoch": model["train_end_epoch"],
    "eval_rows": count,
    "accuracy": round(correct / count, 8),
    "log_loss": round(loss / count, 8),
    "precision": round(precision, 8),
    "recall": round(recall, 8),
    "f1": round(f1, 8),
    "tp": tp,
    "tn": tn,
    "fp": fp,
    "fn": fn,
    "eval_seconds": round(eval_seconds, 6),
    "eval_rows_per_second": round(count / eval_seconds, 3) if eval_seconds > 0 else 0.0,
    "eval_start_epoch": round(started_epoch, 6),
    "eval_end_epoch": round(ended_epoch, 6),
}
with open(os.path.join(out, "metrics"), "w") as f:
    json.dump(metrics, f, separators=(",", ":"))
print(json.dumps(metrics, separators=(",", ":")))
'''

_REPORT_INPUTS = {"manifest": {"type": "json", "required": True}}
for _i in range(CANDIDATES):
    _REPORT_INPUTS[f"metrics_{_i:02d}"] = {"type": "json", "required": True}

_REPORT_SCRIPT = r'''
import glob
import json
import math
import os
import statistics

inp = os.environ["NEXUS_INPUT_DIR"]
out = os.environ["NEXUS_OUTPUT_DIR"]
manifest = json.load(open(os.path.join(inp, "manifest")))
metrics = [json.load(open(p)) for p in sorted(glob.glob(os.path.join(inp, "metrics_*")))]

if not metrics:
    raise SystemExit("no model metrics received")

expected = __EXPECTED__
if len(metrics) != expected:
    raise SystemExit(f"expected {expected} model metrics, got {len(metrics)}")

def overlap(items, start_key, end_key):
    events = []
    for m in items:
        events.append((float(m[start_key]), 1))
        events.append((float(m[end_key]), -1))
    # End (-1) before start (+1) at identical timestamps.
    events.sort(key=lambda x: (x[0], x[1]))
    current = best = 0
    for _, delta in events:
        current += delta
        best = max(best, current)
    return best

train_start = min(m["train_start_epoch"] for m in metrics)
train_end = max(m["train_end_epoch"] for m in metrics)
eval_start = min(m["eval_start_epoch"] for m in metrics)
eval_end = max(m["eval_end_epoch"] for m in metrics)
train_wave = max(0.000001, train_end - train_start)
eval_wave = max(0.000001, eval_end - eval_start)

best = min(metrics, key=lambda m: (m["log_loss"], -m["accuracy"], m["candidate"]))
train_seconds = [float(m["train_seconds"]) for m in metrics]
eval_seconds = [float(m["eval_seconds"]) for m in metrics]
total_row_passes = sum(int(m["train_row_passes"]) for m in metrics)
total_eval_rows = sum(int(m["eval_rows"]) for m in metrics)

sorted_train = sorted(train_seconds)
p95_index = max(0, math.ceil(0.95 * len(sorted_train)) - 1)

report = {
    "status": "ok",
    "configured_model_candidates": expected,
    "completed_model_candidates": len(metrics),
    "dataset": manifest,
    "capacity": {
        "max_observed_parallel_trainers": overlap(metrics, "train_start_epoch", "train_end_epoch"),
        "max_observed_parallel_evaluators": overlap(metrics, "eval_start_epoch", "eval_end_epoch"),
        "training_wave_seconds": round(train_wave, 6),
        "evaluation_wave_seconds": round(eval_wave, 6),
        "mean_train_seconds": round(statistics.mean(train_seconds), 6),
        "p95_train_seconds": round(sorted_train[p95_index], 6),
        "slowest_train_seconds": round(max(train_seconds), 6),
        "mean_eval_seconds": round(statistics.mean(eval_seconds), 6),
        "aggregate_train_row_passes": total_row_passes,
        "aggregate_training_row_passes_per_second": round(total_row_passes / train_wave, 3),
        "aggregate_eval_rows": total_eval_rows,
        "aggregate_eval_rows_per_second": round(total_eval_rows / eval_wave, 3),
    },
    "best_model": {
        "candidate": best["candidate"],
        "learning_rate": best["learning_rate"],
        "l2": best["l2"],
        "accuracy": best["accuracy"],
        "log_loss": best["log_loss"],
        "f1": best["f1"],
    },
    "models": [
        {
            "candidate": m["candidate"],
            "learning_rate": m["learning_rate"],
            "l2": m["l2"],
            "accuracy": m["accuracy"],
            "log_loss": m["log_loss"],
            "f1": m["f1"],
            "train_seconds": m["train_seconds"],
            "eval_seconds": m["eval_seconds"],
        }
        for m in sorted(metrics, key=lambda x: x["candidate"])
    ],
}

with open(os.path.join(out, "report"), "w") as f:
    json.dump(report, f, separators=(",", ":"))
print(json.dumps(report, separators=(",", ":")))
'''
_REPORT_SCRIPT = _REPORT_SCRIPT.replace("__EXPECTED__", str(CANDIDATES))


@task(
    task_ref="ml.generate_data",
    resources=_GENERATE_RES,
    timeouts=_TIMEOUT,
    inputs={
        "rows": {"type": "json", "required": True},
        "features": {"type": "json", "required": True},
        "seed": {"type": "json", "required": True},
    },
    outputs={
        "dataset": {"type": "string", "required": True},
        "manifest": {"type": "json", "required": True},
    },
    runtime=_python(_GENERATE_SCRIPT),
)
def generate_data(ctx: TaskContext, rows: int, features: int, seed: int) -> dict:
    """Generate a deterministic binary-classification CSV."""
    raise NotImplementedError("runs as a docker workload")


@task(
    task_ref="ml.train_logistic",
    resources=_TRAIN_RES,
    timeouts=_TIMEOUT,
    inputs={
        "dataset": {"type": "string", "required": True},
        "candidate": {"type": "json", "required": True},
        "epochs": {"type": "json", "required": True},
        "learning_rate": {"type": "json", "required": True},
        "l2": {"type": "json", "required": True},
        "seed": {"type": "json", "required": True},
    },
    outputs={"model": {"type": "json", "required": True}},
    runtime=_python(_TRAIN_SCRIPT),
)
def train_model(
    ctx: TaskContext,
    dataset: str,
    candidate: int,
    epochs: int,
    learning_rate: float,
    l2: float,
    seed: int,
) -> dict:
    """Fit one logistic-regression candidate using full in-memory training data."""
    raise NotImplementedError("runs as a docker workload")


@task(
    task_ref="ml.evaluate",
    resources=_EVAL_RES,
    timeouts=_TIMEOUT,
    inputs={
        "dataset": {"type": "string", "required": True},
        "model": {"type": "json", "required": True},
    },
    outputs={"metrics": {"type": "json", "required": True}},
    runtime=_python(_EVAL_SCRIPT),
)
def evaluate_model(ctx: TaskContext, dataset: str, model: dict) -> dict:
    """Evaluate one candidate on the deterministic 20% holdout."""
    raise NotImplementedError("runs as a docker workload")


@task(
    task_ref="ml.capacity_report",
    resources=_REPORT_RES,
    timeouts=_TIMEOUT,
    inputs=_REPORT_INPUTS,
    outputs={"report": {"type": "json", "required": True}},
    runtime=_python(_REPORT_SCRIPT),
)
def capacity_report(ctx: TaskContext, **parts: dict) -> dict:
    """Merge model quality and execution-timing metrics into one capacity report."""
    raise NotImplementedError("runs as a docker workload")


@workflow(
    name="ml-capacity-test",
    description=(
        "16-way ML training/evaluation stress test with measurable CPU, memory, "
        "artifact fan-out, scheduler concurrency and aggregate throughput."
    ),
    params={
        "rows": {"type": "int", "required": False, "default": 60000},
        "features": {"type": "int", "required": False, "default": 24},
        "epochs": {"type": "int", "required": False, "default": 8},
        "seed": {"type": "int", "required": False, "default": 7},
    },
    run_timeout="2h",
)
def ml_capacity_test(rows: int, features: int, epochs: int, seed: int):
    generated = generate_data(rows=rows, features=features, seed=seed, node_id="generate_data")

    report_inputs = {"manifest": generated["manifest"]}
    candidate = 0
    for lr in LEARNING_RATES:
        for l2 in L2_VALUES:
            i = candidate
            trained = train_model(
                dataset=generated["dataset"],
                candidate=i,
                epochs=epochs,
                learning_rate=lr,
                l2=l2,
                seed=seed,
                node_id=f"train_{i:02d}",
            )
            evaluated = evaluate_model(
                dataset=generated["dataset"],
                model=trained["model"],
                node_id=f"evaluate_{i:02d}",
            )
            report_inputs[f"metrics_{i:02d}"] = evaluated["metrics"]
            candidate += 1

    return capacity_report(**report_inputs)


if __name__ == "__main__":
    print(ml_capacity_test.compile().to_yaml())
