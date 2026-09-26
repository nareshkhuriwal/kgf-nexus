# llm_batch_inference — LLM calls that survive a crash without paying twice

```
plan --> answer_shard x N (one per shard, created at run time) --> verify
```

120 arithmetic prompts in 4 shards of 30, answered by the mock model server
through `nexus.llm.LLMClient`. Shard 1 **crashes on purpose** right after its
checkpoint at 10 answers; the platform retries it, and the retry calls
`ctx.resume()` and asks only the 20 prompts that were not answered. `verify`
fails the run unless:

- every shard reported exactly once, and every record was answered exactly
  once, in order, and correctly (the mock is deterministic: `What is a + b?`
  is answered `a+b`);
- the shards' token totals equal the sum of the per-answer counts, which equal
  the mock's documented rule (tokens are whitespace word counts);
- the crashed shard resumed (`resumed_from > 0`);
- no prompt was asked twice (`asked_total` equals the shard size for every
  shard, counted across attempts through the checkpoint).

What it exercises (ADR-0032): permits on the shared rate limiter for every
call (keys `mock` and `mock/<model>`), in-task 429/503 retries honouring
`Retry-After`, usage units (`tokens:mock/mock-large:input`, `...:output`,
`requests:mock/mock-large`) reported to the usage ledger per attempt,
checkpoints fenced to the attempt's lease, and the retry of a `transient`
failure.

## Run it locally (no stack)

```bash
python3 -m venv .venv && ./.venv/bin/pip install -e sdk/python
./.venv/bin/python examples/llm_batch_inference/check_local.py
```

`check_local.py` starts `nexus.testing.MockLLMServer` in-process (throttling
every 7th call with a 429), runs the workflow with the SDK's local runner, and
checks the verdict plus what only the mock can see: exactly one completion per
record across the crash, and token counts equal to the verdict's. It also runs
`model=mock-small` (which answers ~30% of arithmetic wrongly) and checks that
the verdict fails that run. It takes a few seconds.

## Run it on a stack

The tasks use the subprocess executor with
`python3 -m nexus.entrypoint --module /opt/nexus/examples/llm_batch_inference/dag.py`,
so the stack needs (the AI overlay provides them):

- an agent with Python 3 and the Nexus SDK installed, serving the ADR-0032
  workload API (the subprocess and docker executors do) — without it the
  deliberate crash is not declared `transient`, so it is not retried, and the
  run fails;
- this directory at `/opt/nexus/examples/llm_batch_inference` on the agent;
- the mock at `http://mock-llm:8089`:
  `python -m nexus.testing.mock_llm --host 0.0.0.0 --port 8089 [--rate-limit-every 7 --retry-after 0.2]`.

```bash
nexusctl workflow submit examples/llm_batch_inference/compiled.yaml
nexusctl run create llm-batch-inference --wait
nexusctl run create llm-batch-inference --param records=600 --param shard_size=50 --param crash_shard=3 --wait
```

To see the shared rate limiter work, give the namespace a limit on the mock
(`PUT /v1/namespaces/default/quotas` with
`"rate_limits": {"mock/mock-large": {"requests_per_second": 20}}`) and watch
the run slow to that rate across all shards.

| Param | Default | Meaning |
|---|---|---|
| `records`, `shard_size` | 120, 30 | prompts, and prompts per shard (at most 100; at most 32 shards) |
| `model` | `mock-large` | `mock-small` answers ~30% wrongly: the verdict then fails |
| `base_url` | `http://mock-llm:8089` | the mock |
| `checkpoint_every` | 5 | answers between checkpoints |
| `crash_shard`, `crash_after` | 1, 10 | the shard that crashes once, after its first checkpoint at or past `crash_after`; `crash_shard=-1` disables |
| `max_tokens` | 16 | per completion |

For a project to start from (JSONL input, real providers, cost estimates, a
task image), see `nexusctl init llm-batch-inference`.
