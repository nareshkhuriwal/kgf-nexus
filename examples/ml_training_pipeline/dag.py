# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""ml_training_pipeline -- a hyperparameter search that really trains models.

    make_dataset --> train_trial (x24, one per grid point) --> select_best --> quality_gate

  * make_dataset  synthesizes a regression problem with KNOWN coefficients,
                  y = 3.2*x1 - 1.7*x2 + 0.8*x3 + 0.5 + noise, splits it 80/20
                  into train/test CSVs, and emits the hyperparameter grid
                  (learning rate x epochs x L2) as a JSON array.
  * train_trial   is a MAP template: one child per grid point, created at run
                  time. Each child trains linear regression by full-batch
                  gradient descent (awk) on the train split and scores MSE and
                  R^2 on the held-out test split. Memoized: re-running with the
                  same data re-uses every finished trial.
  * select_best   fan-in over all trials; picks the lowest test MSE (a failed
                  trial arrives as null and is skipped).
  * quality_gate  fails the run if the winner's R^2 is below min_r2, and emits a
                  model card comparing the learned weights with the true ones.

What it exercises: dynamic expansion over a runtime collection, shared blob
inputs to every child (the train/test CSVs live in MinIO), memoization, fan-in
and a gate that can fail the run. Pure sh + awk, so it runs on the stock agent.

    make submit-example EXAMPLE=ml_training_pipeline PARAMS="rows=20000 seed=11 min_r2=0.95"
"""

from __future__ import annotations

from nexus import Cache, Resources, Runtime, TaskContext, Timeouts, task, workflow

_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '

# awk helper: pull a numeric field out of a one-line JSON object by key.
_GET = (
    "function get(s, k,   m) { if (match(s, \"\\\"\" k \"\\\":-?[0-9.eE+-]+\")) "
    "{ m = substr(s, RSTART, RLENGTH); sub(/^\"[^\"]+\":/, \"\", m); return m + 0 } return \"\" }\n"
)


@task(
    task_ref="ml.make_dataset",
    resources=Resources(cpu="500m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="10m"),
    inputs={"rows": {"type": "json", "required": True}, "seed": {"type": "json", "required": True}},
    outputs={
        "train": {"type": "string", "required": True},
        "test": {"type": "string", "required": True},
        "grid": {"type": "json", "required": True},
    },
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "awk -v rows=\"$(v rows)\" -v seed=\"$(v seed)\" -v dir=\"$NEXUS_OUTPUT_DIR\" '\n"
            "BEGIN {\n"
            "  srand(seed);\n"
            '  h = "x1,x2,x3,y"; print h > (dir "/train"); print h > (dir "/test");\n'
            "  for (i = 1; i <= rows; i++) {\n"
            "    x1 = rand() * 4 - 2; x2 = rand() * 4 - 2; x3 = rand() * 4 - 2;\n"
            # Box-Muller Gaussian noise, sigma 0.3.
            "    u = rand(); if (u < 1e-12) u = 1e-12;\n"
            "    e = sqrt(-2 * log(u)) * cos(6.283185307 * rand()) * 0.3;\n"
            "    y = 3.2 * x1 - 1.7 * x2 + 0.8 * x3 + 0.5 + e;\n"
            '    line = sprintf("%.5f,%.5f,%.5f,%.5f", x1, x2, x3, y);\n'
            '    if (i % 5 == 0) print line > (dir "/test"); else print line > (dir "/train");\n'
            "  }\n"
            # 4 learning rates x 3 epoch budgets x 2 L2 strengths = 24 trials.
            '  n = split("0.005 0.01 0.02 0.05", lr, " "); split("40 100 200", ep, " "); split("0 0.01", l2, " ");\n'
            '  printf "[" > (dir "/grid"); sep = "";\n'
            "  for (a = 1; a <= 4; a++) for (b = 1; b <= 3; b++) for (c = 1; c <= 2; c++) {\n"
            '    printf "%s{\\"lr\\":%s,\\"epochs\\":%s,\\"l2\\":%s}", sep, lr[a], ep[b], l2[c] > (dir "/grid"); sep = ",";\n'
            "  }\n"
            '  printf "]" > (dir "/grid");\n'
            "}'"
        ],
    ),
)
def make_dataset(ctx: TaskContext, rows: int, seed: int) -> dict:
    """Synthesize a regression dataset with known coefficients, plus the grid."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="ml.train_trial",
    resources=Resources(cpu="500m", memory="256Mi"),
    timeouts=Timeouts(start_to_close="20m"),
    # Pure function of (config, train, test): safe to memoize.
    cache=Cache(enabled=True, ttl="24h"),
    inputs={
        "config": {"type": "json", "required": True},
        "train": {"type": "string", "required": True},
        "test": {"type": "string", "required": True},
    },
    outputs={"metrics": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            'cfg=$(tr -d "\\n" < "$NEXUS_INPUT_DIR/config"); '
            "awk -F, -v cfg=\"$cfg\" '\n"
            + _GET
            + "BEGIN { lr = get(cfg, \"lr\"); epochs = get(cfg, \"epochs\"); l2 = get(cfg, \"l2\") }\n"
            # Load the train split into memory (first file), then the test split.
            'FNR == 1 { f++; next }\n'
            "f == 1 { n++; X1[n] = $1; X2[n] = $2; X3[n] = $3; Y[n] = $4; next }\n"
            "f == 2 { t++; T1[t] = $1; T2[t] = $2; T3[t] = $3; TY[t] = $4; next }\n"
            "END {\n"
            "  w1 = w2 = w3 = b = 0;\n"
            "  for (e = 1; e <= epochs; e++) {\n"
            "    g1 = g2 = g3 = gb = 0;\n"
            "    for (i = 1; i <= n; i++) {\n"
            "      r = w1 * X1[i] + w2 * X2[i] + w3 * X3[i] + b - Y[i];\n"
            "      g1 += r * X1[i]; g2 += r * X2[i]; g3 += r * X3[i]; gb += r;\n"
            "    }\n"
            "    w1 -= lr * (2 * g1 / n + 2 * l2 * w1); w2 -= lr * (2 * g2 / n + 2 * l2 * w2);\n"
            "    w3 -= lr * (2 * g3 / n + 2 * l2 * w3); b -= lr * 2 * gb / n;\n"
            "  }\n"
            "  for (i = 1; i <= t; i++) { m += TY[i] }\n"
            "  m /= t;\n"
            "  for (i = 1; i <= t; i++) {\n"
            "    p = w1 * T1[i] + w2 * T2[i] + w3 * T3[i] + b;\n"
            "    sse += (TY[i] - p) ^ 2; sst += (TY[i] - m) ^ 2;\n"
            "  }\n"
            '  printf "{\\"lr\\":%s,\\"epochs\\":%d,\\"l2\\":%s,\\"mse\\":%.6f,\\"r2\\":%.6f,\\"w1\\":%.4f,\\"w2\\":%.4f,\\"w3\\":%.4f,\\"b\\":%.4f,\\"train_rows\\":%d,\\"test_rows\\":%d}", '
            "lr, epochs, l2, sse / t, 1 - sse / sst, w1, w2, w3, b, n, t;\n"
            "}' \"$NEXUS_INPUT_DIR/train\" \"$NEXUS_INPUT_DIR/test\" > \"$NEXUS_OUTPUT_DIR/metrics\""
        ],
    ),
)
def train_trial(ctx: TaskContext, config: dict, train: str, test: str) -> dict:
    """Train one configuration and score it on the held-out split."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="ml.select_best",
    resources=Resources(cpu="100m", memory="64Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    # Choose among whatever finished; a failed trial arrives as null.
    trigger_rule="all_done",
    inputs={"trials": {"type": "json", "required": True}},
    outputs={"best": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            # One trial object per line, then the lowest-MSE one wins.
            "tr -d '\\n' < \"$NEXUS_INPUT_DIR/trials\" | sed 's/},/}\\n/g' | awk '\n"
            + _GET
            + '/"mse"/ { s = $0; sub(/^[^{]*/, "", s); sub(/}[^}]*$/, "}", s); ok++;\n'
            "  m = get(s, \"mse\"); if (best == \"\" || m < bm) { bm = m; best = s } }\n"
            '/null/ { failed++ }\n'
            "END {\n"
            '  if (best == "") { print "no trial succeeded" > "/dev/stderr"; exit 1 }\n'
            "  sub(/}$/, \"\", best);\n"
            '  printf "%s,\\"trials_succeeded\\":%d,\\"trials_failed\\":%d}", best, ok, failed;\n'
            "}' > \"$NEXUS_OUTPUT_DIR/best\" && cat \"$NEXUS_OUTPUT_DIR/best\""
        ],
    ),
)
def select_best(ctx: TaskContext, trials: list) -> dict:
    """Fan-in: the lowest test MSE across every successful trial."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="ml.quality_gate",
    resources=Resources(cpu="100m", memory="64Mi"),
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"best": {"type": "json", "required": True}, "min_r2": {"type": "json", "required": True}},
    outputs={"model_card": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "awk -v min=\"$(v min_r2)\" -v s=\"$(tr -d '\\n' < \"$NEXUS_INPUT_DIR/best\")\" '\n"
            + _GET
            + "BEGIN {\n"
            '  r2 = get(s, "r2");\n'
            # Distance from the true generating coefficients.
            '  err = (get(s,"w1") - 3.2)^2 + (get(s,"w2") + 1.7)^2 + (get(s,"w3") - 0.8)^2 + (get(s,"b") - 0.5)^2;\n'
            '  status = (r2 >= min) ? "approved" : "rejected";\n'
            '  printf "{\\"status\\":\\"%s\\",\\"r2\\":%.6f,\\"min_r2\\":%s,\\"coefficient_error\\":%.6f,\\"true\\":{\\"w1\\":3.2,\\"w2\\":-1.7,\\"w3\\":0.8,\\"b\\":0.5},\\"learned\\":{\\"w1\\":%s,\\"w2\\":%s,\\"w3\\":%s,\\"b\\":%s},\\"lr\\":%s,\\"epochs\\":%s,\\"l2\\":%s}\\n", '
            'status, r2, min, sqrt(err), get(s,"w1"), get(s,"w2"), get(s,"w3"), get(s,"b"), get(s,"lr"), get(s,"epochs"), get(s,"l2");\n'
            '  if (status != "approved") { printf "model rejected: R^2 %.4f < %s\\n", r2, min > "/dev/stderr"; exit 1 }\n'
            "}' > \"$NEXUS_OUTPUT_DIR/model_card\" && cat \"$NEXUS_OUTPUT_DIR/model_card\""
        ],
    ),
)
def quality_gate(ctx: TaskContext, best: dict, min_r2: float) -> dict:
    """Approve the model only if it generalizes; emit a model card."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="ml-training-pipeline",
    description="Synthetic regression, 24-trial hyperparameter search, best-model selection and a quality gate.",
    params={
        "rows": {"type": "int", "required": False, "default": 20000},
        "seed": {"type": "int", "required": False, "default": 11},
        "min_r2": {"type": "float", "required": False, "default": 0.95},
    },
    run_timeout="2h",
)
def ml_training_pipeline(rows: int, seed: int, min_r2: float):
    data = make_dataset(rows=rows, seed=seed)
    trials = train_trial.map(
        data["grid"], item="config", max_fanout=64, train=data["train"], test=data["test"]
    )
    best = select_best(trials=trials)
    return quality_gate(best=best, min_r2=min_r2)


if __name__ == "__main__":
    print(ml_training_pipeline.compile().to_yaml())
