# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""batch_inference -- sharded scoring with per-shard checkpoints, checked exactly.

    plan --> infer (one child per shard, created at run time) --> verify

  * plan    cuts `records` records into shards of `shard_size` and emits the
            shard DESCRIPTORS ({"shard", "start", "stop"}), never the data.
  * infer   is a MAP template: each child regenerates its slice of the
            deterministic dataset, scores every record with a fixed logistic
            model, and returns a small summary. It is memoized, so the summary
            is the shard's checkpoint: a re-run with the same inputs reuses
            every finished shard instead of scoring it again.
  * verify  CHECKS THE FAN-IN EXACTLY and fails the run otherwise: every shard
            reported exactly once (no nulls, shard indices sum to the closed
            form), every record was scored exactly once (row count, and record
            ids summing to records*(records-1)/2), and the positives counted
            across shards equal a single unsharded pass over the whole dataset.
            A lost, duplicated or mis-bounded shard changes one of those.

What it exercises: dynamic expansion with a mandatory max_fanout, memoized map
children, a fan-in whose size is bounded by `max_bytes` on the child output (so
submit-time validation proves the fan-in fits), and trigger_rule=all_done so a
failed shard is reported by the verifier rather than hidden. Plain sh + awk, CPU
only, so it runs on the stock agent image.

For a real model, see `nexusctl init batch-inference`, which generates a Python
project with the same shape (and an optional GPU request).

    make submit-example EXAMPLE=batch_inference PARAMS="records=2000 shard_size=250"
"""

from __future__ import annotations

from nexus import Cache, Resources, Retry, Runtime, TaskContext, Timeouts, task, workflow

_TINY = Resources(cpu="100m", memory="64Mi")
_READ = 'v() { tr -d \'"\\n \' < "$NEXUS_INPUT_DIR/$1"; }; '

# At most this many shards: a plan that would exceed it fails the infer node
# explicitly (max_fanout_exceeded) instead of creating an unbounded fan-out.
MAX_SHARDS = 64

# The dataset and the model, as awk functions shared by infer and verify so the
# sharded and unsharded passes compute exactly the same thing. Record i has
# three features in [-2, 2), derived deterministically from i.
_MODEL = (
    "function feat(i, j) { return ((i * (7919 + 104729 * j) + 13 * j) % 1000) / 250.0 - 2 }\n"
    "function positive(i,   z) {\n"
    "  z = 1.2 * feat(i, 1) - 0.8 * feat(i, 2) + 0.5 * feat(i, 3) - 0.1\n"
    "  return (1 / (1 + exp(-z))) >= 0.5\n"
    "}\n"
)


@task(
    task_ref="infer.plan",
    resources=_TINY,
    timeouts=Timeouts(start_to_close="5m"),
    inputs={"records": {"type": "json", "required": True}, "shard_size": {"type": "json", "required": True}},
    outputs={"shards": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "awk -v n=\"$(v records)\" -v size=\"$(v shard_size)\" 'BEGIN {\n"
            '  if (n < 1 || size < 1) { print "records and shard_size must be positive" > "/dev/stderr"; exit 1 }\n'
            '  printf "["\n'
            "  for (s = 0; s * size < n; s++) {\n"
            "    stop = (s + 1) * size; if (stop > n) stop = n\n"
            '    printf "%s{\\"shard\\":%d,\\"start\\":%d,\\"stop\\":%d}", (s ? "," : ""), s, s * size, stop\n'
            "  }\n"
            '  printf "]"\n'
            "}' > \"$NEXUS_OUTPUT_DIR/shards\""
        ],
    ),
)
def plan(ctx: TaskContext, records: int, shard_size: int) -> list:
    """Shard descriptors covering [0, records)."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="infer.score_shard",
    resources=_TINY,
    timeouts=Timeouts(start_to_close="5m"),
    retry=Retry(max_attempts=3, retry_on=["worker_lost", "infrastructure"]),
    # The per-shard checkpoint: same shard, same result, reused for an hour.
    cache=Cache(enabled=True, ttl="1h"),
    inputs={"shard": {"type": "json", "required": True}},
    # Bounded so that max_fanout x max_bytes is provably within the fan-in limit.
    outputs={"summary": {"type": "json", "required": True, "max_bytes": 256}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            "tr -d '{}\" \\n' < \"$NEXUS_INPUT_DIR/shard\" | tr ',' '\\n' | awk -F: '\n"
            + _MODEL
            + "{ f[$1] = $2 }\n"
            "END {\n"
            "  for (i = f[\"start\"]; i < f[\"stop\"]; i++) { rows++; ids += i; pos += positive(i) }\n"
            # %.0f, not %d: id sums pass 2^31 at ~65k records, and some awks
            # (busybox) truncate %d to a 32-bit int.
            '  printf "{\\"shard\\":%d,\\"rows\\":%d,\\"positives\\":%d,\\"id_sum\\":%.0f}", f["shard"], rows, pos, ids\n'
            "}' > \"$NEXUS_OUTPUT_DIR/summary\" && cat \"$NEXUS_OUTPUT_DIR/summary\""
        ],
    ),
)
def score_shard(ctx: TaskContext, shard: dict) -> dict:
    """Score records [start, stop) and summarize."""
    raise NotImplementedError("runs as a shell workload")


@task(
    task_ref="infer.verify",
    resources=_TINY,
    timeouts=Timeouts(start_to_close="5m"),
    retry=Retry(max_attempts=1),  # a failed check is a verdict, not a glitch
    # Run even when shards failed, so the verdict names what is missing.
    trigger_rule="all_done",
    inputs={
        "summaries": {"type": "json", "required": True},
        "records": {"type": "json", "required": True},
        "shard_size": {"type": "json", "required": True},
    },
    outputs={"result": {"type": "json", "required": True}},
    runtime=Runtime(
        executor="subprocess",
        command=["sh", "-c"],
        args=[
            _READ
            + "tr -d '[]{}\" \\n' < \"$NEXUS_INPUT_DIR/summaries\" | tr ',' '\\n' | "
            "awk -F: -v n=\"$(v records)\" -v size=\"$(v shard_size)\" '\n"
            + _MODEL
            + '$0 == "null" { missing++; next }\n'
            '$1 == "shard" { shards++; shard_sum += $2 }\n'
            '$1 == "rows" { rows += $2 }\n'
            '$1 == "positives" { pos += $2 }\n'
            '$1 == "id_sum" { ids += $2 }\n'
            "END {\n"
            "  want_shards = int((n + size - 1) / size)\n"
            "  for (i = 0; i < n; i++) want_pos += positive(i)\n"
            "  want_ids = n * (n - 1) / 2; want_shard_sum = want_shards * (want_shards - 1) / 2\n"
            "  ok = (missing == 0 && shards == want_shards && shard_sum == want_shard_sum &&"
            " rows == n && ids == want_ids && pos == want_pos)\n"
            '  printf "{\\"records\\":%d,\\"shards\\":%d,\\"missing_shards\\":%d,\\"rows\\":%d,'
            '\\"positives\\":%d,\\"positives_unsharded\\":%d,\\"positive_rate\\":%.4f,\\"ok\\":%s}\\n",'
            ' n, shards, missing, rows, pos, want_pos, (n ? want_pos / n : 0), ok ? "true" : "false"\n'
            "  if (!ok) {\n"
            '    printf "batch inference check failed: %d/%d shards reported (%d missing), %d/%d rows, "'
            ' , shards, want_shards, missing, rows, n > "/dev/stderr"\n'
            '    printf "id sum %.0f vs %.0f, positives %d vs %d unsharded\\n", ids, want_ids, pos, want_pos > "/dev/stderr"\n'
            "    exit 1\n"
            "  }\n"
            "}' > \"$NEXUS_OUTPUT_DIR/result\" && cat \"$NEXUS_OUTPUT_DIR/result\""
        ],
    ),
)
def verify(ctx: TaskContext, summaries: list, records: int, shard_size: int) -> dict:
    """Fan-in: every shard once, every record once, same answer as one pass."""
    raise NotImplementedError("runs as a shell workload")


@workflow(
    name="batch-inference",
    description="Shard a dataset, score each shard in parallel with memoized checkpoints, verify the fan-in exactly.",
    params={
        "records": {"type": "int", "required": False, "default": 2000},
        "shard_size": {"type": "int", "required": False, "default": 250},
    },
    run_timeout="30m",
)
def batch_inference(records: int, shard_size: int):
    shards = plan(records=records, shard_size=shard_size)
    summaries = score_shard.map(shards["shards"], item="shard", max_fanout=MAX_SHARDS)
    return verify(summaries=summaries, records=records, shard_size=shard_size)


if __name__ == "__main__":
    print(batch_inference.compile().to_yaml())
