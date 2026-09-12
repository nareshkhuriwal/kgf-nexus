# ten_node_mixed — the Phase 1 exit-criteria DAG

Architecture §11 states the Phase 1 exit criterion as: *"a 10-node DAG with mixed
CPU/GPU tasks completes correctly."* This is that DAG. It is shaped to actually
exercise the resource-aware scheduler rather than merely to contain ten nodes.

```
ingest ─── validate_schema ─── build_features ──┬── train_small    (1× A10)   ──┐
                                                ├── train_large    (2× A100)  ──┤
                                                └── train_baseline (CPU only) ──┤
                                                                                │
                                                        evaluate (1× any GPU) ──┘
                                                              │
                                                    ┌─────────┴─────────┐
                                                 register          publish_report
                                                    │
                                                 notify  (trigger_rule: all_done)
```

10 tasks, 11 edges.

## Run it

```bash
python3 -m venv .venv && ./.venv/bin/pip install -e sdk/python

# The table view shows the resource columns, which is the point of this example.
nexusctl workflow compile examples/ten_node_mixed/dag.py --python ./.venv/bin/python

nexusctl workflow submit examples/ten_node_mixed/dag.py --dry-run --python ./.venv/bin/python
nexusctl workflow submit examples/ten_node_mixed/dag.py --python ./.venv/bin/python

nexusctl run create ten-node-mixed \
  --param dataset_uri=s3://datasets/images-2026-09.tar \
  --idempotency-key ten-node-2026-09-12

nexusctl events <run-id> --follow

# When GPU tasks sit in schedule_to_start, this is the command that tells you
# whether it is a capacity shortage or an unsatisfiable constraint.
nexusctl capacity
nexusctl worker ls
```

## Files

| File | What it is |
|---|---|
| `dag.py` | The workflow. The module docstring explains what each shape tests. |
| `compiled.yaml` | The compiled WorkflowSpec, committed for review. |

## What this DAG exercises, and why each piece is here

**Fan-out to three trainers.** The scheduler must handle three eligible attempts
at once, so it cannot simply drain a queue in order.

**Fan-in to `evaluate`.** Three upstreams, so the DAG engine's trigger-rule
evaluation has to wait on a set rather than a single predecessor.

**`gpu_kind`, not just `gpu_count`.** `train_large` wants 2× `nvidia-a100`;
`train_small` wants 1× `nvidia-a10`. A scheduler that only counted GPUs would
happily place `train_large` on A10 hosts and OOM four hours in. The *kind* is the
interesting constraint, not the count. `evaluate` requests one GPU of any kind,
which must still be placeable.

**A CPU-only sibling.** `train_baseline` requests no GPU and must be placeable on
a GPU-less host without queueing behind GPU work.

**A custom countable resource.** `register` requests `license_token: 1` — a
dimension with no hardware meaning at all. It is there to confirm the ledger is
genuinely multi-dimensional rather than CPU/memory with GPUs bolted on.

**A concurrency key.** The trainers share `concurrency_key: model-training` with
`limit: 2`. Capacity asks "can this fit"; concurrency asks "should this many run
at once". Both limits have to hold, and they are different questions.

**`request` below `limit`.** `build_features` requests 16Gi but is limited to
24Gi, so it bursts into spare memory on a quiet host instead of being killed at
exactly its request.

**`trigger_rule: all_done` on `notify`.** It runs whether upstreams succeeded or
failed. With the default `all_success` it would be SKIPPED precisely when a
notification is most wanted.

**`fail_fast: false`.** Deliberate. The trainers are independent: if the A100 pair
never frees up, the small and baseline models are still worth having, and killing
them would discard hours of finished GPU work.

## Verifying determinism

```bash
nexusctl workflow compile examples/ten_node_mixed/dag.py --hash-only --python ./.venv/bin/python
nexusctl workflow compile examples/ten_node_mixed/compiled.yaml --hash-only
```

Both print the same hash. That is ADR-0004's requirement that the Python and YAML
authoring surfaces produce the identical document — if they diverged, one workflow
would register as two content-addressed spec versions.
