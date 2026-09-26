# Batch inference

```bash
nexusctl init batch-inference scoring          # CPU
nexusctl init batch-inference scoring --gpu    # each shard requests a GPU
```

```text
plan_shards --> infer x N (one per shard) --> aggregate
```

The template ([source](repo:cli/internal/templates/files/batch-inference/dag.py)):

| Task | Does | Policy |
| --- | --- | --- |
| `plan_shards` | counts records and cuts them into `shard_size` slices | at most 256 shards |
| `infer` | **map** over shards; writes `output_prefix/part-NNNNN.jsonl` | memoized 30 days (the checkpoint); retried on a different worker; `max_bytes=1024` summary |
| `aggregate` | checks every shard reported; totals | `all_done`; fails naming missing shards |

## Checkpointing is memoization

Each `infer` child is memoized on (shard, model, output prefix, threshold),
and its small summary is the shard's checkpoint:

- If a run fails at shard 180 of 200, **retry** it (`nexusctl run retry <run>`).
  The finished shards come from the cache, and only the failed ones run.
- Changing the model (its JSON, including `version`) changes the cache key, so
  everything is scored again.
- If the input was corrected in place, invalidate explicitly:
  `nexusctl cache invalidate --task-ref scoring.infer --reason "input corrected"`.

Prediction files are written under a temporary name and renamed into place,
so a crash never leaves a truncated part file.

## Fan-in stays small by construction

Each child returns a summary of at most 1 KiB, never predictions. The gateway
proves at submit time that `max_fanout × max_bytes` fits the fan-in limit
([fan-in](../concepts/fan-out-fan-in.md#fan-in)). Predictions go to storage,
and the summary carries counts and the part file's name.

## GPUs

With `--gpu`, each shard requests `Gpu(count=1, kind=GPU_KIND)` and starts only
on a worker advertising a matching device. The worker sets
`CUDA_VISIBLE_DEVICES` to the reserved device. See [GPU jobs](gpu-jobs.md),
including what the local stack's simulated GPUs do and do not test.

[`examples/batch_inference`](repo:examples/batch_inference/) runs the same
shape in plain `sh` and `awk`. It verifies that every shard reported once,
every record was scored once, and the sharded positives equal one unsharded
pass.
