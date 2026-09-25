"""__WORKFLOW_NAME__ -- hyperparameter search, model selection and a quality gate.

    load_dataset --+--> train (one child per trial, created at run time) --> select_best
    make_grid -----+                                                            |
                                                                                v
                                                         evaluate --> quality_gate

  * load_dataset  loads a CSV (last column is the target) or generates a
                  synthetic regression problem, and splits train/test by seed.
  * make_grid     expands the learning-rate x L2 grid into trials.
  * train         a MAP over the trials: ridge regression by gradient descent,
                  scored on a validation slice of the training set. Each trial
                  is pure and memoized, so re-running with the same data and
                  grid reuses every finished trial and only runs new ones.
  * select_best   picks the best validation R^2. It runs even if some trials
                  failed (trigger_rule=all_done) and uses the ones that did not.
  * evaluate      refits the winner on the whole training set and scores the
                  held-out test set -- data no trial ever saw.
  * quality_gate  FAILS the run when test R^2 is below `min_r2`, so a bad model
                  cannot be mistaken for a finished one.

The model is dependency-free on purpose (a template should run anywhere). Swap
`_fit` / `_predict` for scikit-learn, XGBoost or PyTorch; the DAG does not change.

    pytest
    nexusctl workflow compile dag.py --show
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --param rows=5000 --param min_r2=0.9 --wait
"""

from __future__ import annotations

import csv
import math
import random
from typing import Any

from nexus import Cache, Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow

IMAGE = "__WORKFLOW_NAME__:0.1.0"

# Hard ceiling on the grid. The map's max_fanout is MANDATORY: a runaway grid
# fails the node loudly instead of flooding the cluster with trials.
MAX_TRIALS = 64


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


# ── model: ridge regression by full-batch gradient descent ───────────────────


def _standardize(rows: list[list[float]]) -> tuple[list[float], list[float]]:
    n_features = len(rows[0]) - 1
    means, stds = [], []
    for j in range(n_features):
        col = [r[j] for r in rows]
        mu = sum(col) / len(col)
        sd = math.sqrt(sum((x - mu) ** 2 for x in col) / len(col)) or 1.0
        means.append(mu)
        stds.append(sd)
    return means, stds


def _fit(rows: list[list[float]], lr: float, l2: float, epochs: int, ctx: TaskContext | None = None) -> dict:
    means, stds = _standardize(rows)
    n, k = len(rows), len(means)
    xs = [[(r[j] - means[j]) / stds[j] for j in range(k)] for r in rows]
    ys = [r[-1] for r in rows]
    w, b = [0.0] * k, sum(ys) / n
    for epoch in range(epochs):
        grad_w, grad_b = [0.0] * k, 0.0
        for x, y in zip(xs, ys):
            err = sum(wj * xj for wj, xj in zip(w, x)) + b - y
            for j in range(k):
                grad_w[j] += err * x[j]
            grad_b += err
        w = [wj - lr * (gj / n + l2 * wj) for wj, gj in zip(w, grad_w)]
        b -= lr * grad_b / n
        if ctx is not None and epoch % 50 == 0:
            ctx.progress(epoch / epochs, f"epoch {epoch}")
    if any(math.isnan(v) or math.isinf(v) for v in w + [b]):
        raise ArithmeticError(f"training diverged at lr={lr}; lower the learning rate")
    return {"weights": w, "bias": b, "means": means, "stds": stds}


def _predict(model: dict, x: list[float]) -> float:
    return model["bias"] + sum(
        w * (xj - mu) / sd for w, xj, mu, sd in zip(model["weights"], x, model["means"], model["stds"])
    )


def _r2(model: dict, rows: list[list[float]]) -> float:
    ys = [r[-1] for r in rows]
    mean = sum(ys) / len(ys)
    ss_tot = sum((y - mean) ** 2 for y in ys) or 1e-12
    ss_res = sum((_predict(model, r[:-1]) - r[-1]) ** 2 for r in rows)
    return 1.0 - ss_res / ss_tot


# ── tasks ────────────────────────────────────────────────────────────────────


@_task(
    "load_dataset",
    resources=Resources(cpu="1", memory="2Gi"),
    timeouts=Timeouts(start_to_close="20m"),
    retry=Retry(max_attempts=3, retry_on=["transient", "timeout", "infrastructure"]),
    cache=Cache(enabled=True, ttl="1d"),
)
def load_dataset(ctx: TaskContext, source: str, rows: int, seed: int, test_fraction: float = 0.2) -> dict:
    """Load or synthesize numeric rows [x1..xk, y] and split train/test."""
    rng = random.Random(seed)
    if source == "synthetic":
        data = []
        for _ in range(rows):
            x = [rng.uniform(-3, 3) for _ in range(4)]
            y = 3.0 * x[0] - 2.0 * x[1] + 0.5 * x[2] + 0.0 * x[3] + 4.0 + rng.gauss(0, 0.5)
            data.append(x + [y])
    else:
        path = source[len("file://"):] if source.startswith("file://") else source
        with open(path, newline="") as f:
            reader = csv.reader(f)
            next(reader)  # header
            data = [[float(v) for v in r] for r in reader if r]
        if rows > 0:
            data = data[:rows]
    if len(data) < 20:
        raise ValueError(f"need at least 20 rows to train and test, got {len(data)}")
    rng.shuffle(data)
    cut = int(len(data) * (1 - test_fraction))
    ctx.log.info("dataset ready", train=cut, test=len(data) - cut, features=len(data[0]) - 1)
    return {"train": data[:cut], "test": data[cut:]}


@_task(
    "make_grid",
    resources=Resources(cpu="100m", memory="128Mi"),
    timeouts=Timeouts(start_to_close="2m", heartbeat="1m"),
    retry=Retry(max_attempts=1),
)
def make_grid(ctx: TaskContext, learning_rates: list, l2: list, epochs: int) -> list:
    """The cartesian product of the grid, one dict per trial."""
    trials = [{"lr": float(lr), "l2": float(reg), "epochs": int(epochs)} for lr in learning_rates for reg in l2]
    if not trials:
        raise ValueError("the grid is empty: give at least one learning rate and one l2 value")
    if len(trials) > MAX_TRIALS:
        raise ValueError(f"{len(trials)} trials exceed MAX_TRIALS={MAX_TRIALS}; shrink the grid")
    return trials


@_task(
    "train",
    resources=Resources(cpu="1", memory="1Gi"),
    timeouts=Timeouts(start_to_close="1h", heartbeat="5m"),
    retry=Retry(max_attempts=2, retry_on=["worker_lost", "infrastructure", "oom"]),
    # Pure in (hparams, dataset): memoizing makes a re-run reuse finished trials.
    cache=Cache(enabled=True, ttl="7d"),
    # The array of trial results is assembled in the control plane, so each
    # one's size is declared: 64 trials x 4 KiB fits the fan-in limit by proof.
    outputs={"result": {"type": "json", "required": True, "max_bytes": 4096}},
)
def train(ctx: TaskContext, hparams: dict, dataset: dict, validation_fraction: float = 0.2) -> dict:
    """Fit one trial and score it on a validation slice of the training set."""
    rows = dataset["train"]
    cut = int(len(rows) * (1 - validation_fraction))
    fit_rows, val_rows = rows[:cut], rows[cut:]
    model = _fit(fit_rows, hparams["lr"], hparams["l2"], hparams["epochs"], ctx)
    score = _r2(model, val_rows)
    ctx.log.info("trial done", **hparams, val_r2=round(score, 5))
    return {"hparams": hparams, "val_r2": score}


@_task(
    "select_best",
    resources=Resources(cpu="100m", memory="128Mi"),
    timeouts=Timeouts(start_to_close="2m", heartbeat="1m"),
    retry=Retry(max_attempts=1),
    # Run even if some trials failed: a diverged learning rate should not sink
    # the whole search. Failed trials arrive as null.
    trigger_rule="all_done",
)
def select_best(ctx: TaskContext, results: list) -> dict:
    ok = [r for r in results if r is not None]
    if not ok:
        raise ValueError(f"all {len(results)} trials failed; nothing to select")
    best = max(ok, key=lambda r: r["val_r2"])
    ctx.log.info("best trial", trials=len(results), failed=len(results) - len(ok), **best["hparams"])
    return {"best": best, "trials": len(results), "failed_trials": len(results) - len(ok)}


@_task(
    "evaluate",
    resources=Resources(cpu="1", memory="2Gi"),
    timeouts=Timeouts(start_to_close="1h", heartbeat="5m"),
    retry=Retry(max_attempts=2, retry_on=["worker_lost", "infrastructure"]),
    cache=Cache(enabled=True, ttl="7d"),
)
def evaluate(ctx: TaskContext, selection: dict, dataset: dict) -> dict:
    """Refit the winner on all training data and score the untouched test set."""
    hp = selection["best"]["hparams"]
    model = _fit(dataset["train"], hp["lr"], hp["l2"], hp["epochs"], ctx)
    test = dataset["test"]
    rmse = math.sqrt(sum((_predict(model, r[:-1]) - r[-1]) ** 2 for r in test) / len(test))
    return {
        "hparams": hp,
        "val_r2": selection["best"]["val_r2"],
        "test_r2": _r2(model, test),
        "test_rmse": rmse,
        "model": model,
        "trials": selection["trials"],
        "failed_trials": selection["failed_trials"],
    }


@_task(
    "quality_gate",
    resources=Resources(cpu="100m", memory="128Mi"),
    timeouts=Timeouts(start_to_close="2m", heartbeat="1m"),
    retry=Retry(max_attempts=1),  # a failed gate is a verdict, not a glitch
)
def quality_gate(ctx: TaskContext, evaluation: dict, min_r2: float) -> dict:
    """Fail the run unless the model clears the bar; otherwise emit a model card."""
    passed = evaluation["test_r2"] >= min_r2
    card = {
        "passed": passed,
        "min_r2": min_r2,
        "test_r2": round(evaluation["test_r2"], 6),
        "test_rmse": round(evaluation["test_rmse"], 6),
        "hparams": evaluation["hparams"],
        "trials": evaluation["trials"],
        "failed_trials": evaluation["failed_trials"],
        "model": evaluation["model"],
    }
    ctx.log.info("quality gate", passed=passed, test_r2=card["test_r2"], min_r2=min_r2)
    if not passed:
        raise ValueError(f"test R^2 {evaluation['test_r2']:.4f} is below min_r2 {min_r2}; the model is not promoted")
    return card


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Grid search over ridge-regression hyperparameters, best-model selection and a quality gate.",
    params={
        "source": {"type": "string", "required": False, "default": "synthetic"},
        "rows": {"type": "int", "required": False, "default": 4000},
        "seed": {"type": "int", "required": False, "default": 7},
        "learning_rates": {"type": "json", "required": False, "default": [0.01, 0.05, 0.1, 0.3]},
        "l2": {"type": "json", "required": False, "default": [0.0, 0.001, 0.01]},
        "epochs": {"type": "int", "required": False, "default": 200},
        "min_r2": {"type": "float", "required": False, "default": 0.9},
    },
    run_timeout="4h",
)
def training(source: str, rows: int, seed: int, learning_rates: list, l2: list, epochs: int, min_r2: float):
    data = load_dataset(source=source, rows=rows, seed=seed)
    trials = make_grid(learning_rates=learning_rates, l2=l2, epochs=epochs)
    results = train.map(trials, item="hparams", max_fanout=MAX_TRIALS, dataset=data)
    selection = select_best(results=results)
    evaluation = evaluate(selection=selection, dataset=data)
    return quality_gate(evaluation=evaluation, min_r2=min_r2)


if __name__ == "__main__":
    compiled = training.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
