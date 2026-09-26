# nexus-sdk — Python SDK for Nexus

**This SDK is a compiler, not a scheduler hook.**

The decorators run in *your* process and emit a `WorkflowSpec` JSON document,
which is submitted over HTTP. The control plane parses that document and never
imports, executes, or evals your code (ADR-0004). Two consequences worth
internalising:

- The control plane needs **none** of your dependencies installed.
- A slow import, an infinite loop at module level, or a crash in your module
  **cannot** degrade scheduling for anyone else.

That is the single largest reliability divergence from Airflow, and it is
structural rather than a promise.

## Install

```bash
pip install -e sdk/python          # from the repo root
pip install -e 'sdk/python[yaml]'  # + PyYAML, only needed to RENDER a spec
```

No runtime dependencies. The SDK is installed into every task image, and each
dependency would be a version conflict waiting to happen with your own
requirements; the HTTP client is built on `urllib` from the standard library.

## Authoring

```python
from nexus import Resources, Retry, TaskContext, Timeouts, task, workflow

@task(
    resources=Resources(cpu="2", memory="8Gi"),           # "2000m" also works
    timeouts=Timeouts(start_to_close="4h", heartbeat="5m"),
    retry=Retry(max_attempts=3, backoff="exponential", jitter="full"),
    idempotency="required",
    cache=True,
)
def extract(ctx: TaskContext, source: str) -> dict:
    ctx.heartbeat("connecting")
    ctx.log.info("extracting", source=source)
    return {"rows": fetch(source)}

@workflow(name="daily-etl", params={"source": {"type": "string"}})
def daily_etl(source: str):
    return load(transform(extract(source)))

compiled = daily_etl.compile()   # runs HERE, in your process
print(compiled.hash)             # the server's content address
print(compiled.to_json(indent=2))
```

Human-friendly units are parsed in your process: CPU accepts `"2"` or `"2000m"`,
memory accepts `"8Gi"` or `"512Mi"`, durations accept `"10s"`, `"30m"`, `"4h"`,
`"1h30m"`.

### How the DAG is discovered

The workflow body is **traced once, at compile time, on your machine**. Task calls
do not execute handlers — they record nodes and return `OutputRef` handles.
Passing a handle to another task creates the edge, and the downstream parameter
*name* becomes the input name in the edge's `output_to_input` map.

Tracing rather than AST analysis means loops, comprehensions, and helper functions
in the body all just work, because they are plain Python. The cost is that the body
runs, so side effects in a body happen at compile time — on your machine, never on
the control plane.

Runtime values do not exist at compile time, so `if task_result:` raises rather
than silently compiling a graph you did not write.

### Determinism is a correctness requirement

The emitted document is canonical — tasks sorted by id, edges sorted by
`(from, to)`, every map key sorted, no insignificant whitespace. The server
content-addresses it and treats a byte-identical resubmission as a no-op. An
unstable document would mint a spurious new spec version on every compile of an
unchanged file, and every run would pin a version nobody intended to create.

Compiling twice — in different processes, under different `PYTHONHASHSEED` values —
produces byte-identical JSON. There is a test for exactly that.

### Typed handles

The SDK ships a `py.typed` marker, and `mypy --strict` passes on it
(`make sdk-typecheck`, which `make sdk-test` runs first). A task's return type
flows through tracing:

```python
@task
def extract(ctx: TaskContext, source: str) -> dict[str, int]: ...

rows = extract(source)             # TaskInvocation[dict[str, int]]
rows.primary                       # OutputRef[dict[str, int]]
scores = score.map(ids, item="item", max_fanout=100)   # TaskInvocation[list[float | None]]
big = extract.with_overrides(resources=Resources(cpu="4"))  # TaskDefinition[dict[str, int]]
node["model"]                      # OutputRef[Any]; node.output("model", Model) states it
```

A fan-in is `list[R | None]` because a child that did not succeed contributes
`null`. `Ref[T]` (`OutputRef[T] | TaskInvocation[T]`) annotates helpers that wire
tasks together. `typecheck/typed_usage.py` is the example CI type-checks.

### Value bounds and captured stdout

```python
@task(
    outputs={"summary": {"type": "json", "max_bytes": "4Ki"}},   # or ValueSpec(max_bytes=4096)
    runtime=Runtime(command=["sh", "-c"], args=["./report.sh"], stdout_output="summary"),
)
```

`max_bytes` bounds a value's size. On a map template's output it lets
validation reject a fan-in (`max_fanout x max_bytes`) that could exceed the
platform's fan-in limit before any child runs. `stdout_output` names a
DECLARED output that receives the task's stdout. Both are checked at the
decorator the way the gateway checks them at submit.

The in-task result store (`ctx.store`) inlines at the control plane's
configured threshold (ADR-0016). The agent exports it as
`NEXUS_INLINE_THRESHOLD_BYTES`, and a compiled-in 64 KiB is only the fallback.

## Running locally, without a cluster

```python
from nexus import local
result = local.run(daily_etl, {"source": "s3://in"})
result.state                  # "SUCCEEDED" | "FAILED" | "TIMED_OUT" | ...
result.output("load")         # a node's output value
print(result.summary())       # per-node table
```

or `nexusctl run --local dag.py --param source=s3://in`, or
`python -m nexus.local compiled.yaml --param source=demo`. The runner decides
what runs next exactly as the platform's DAG engine does: trigger rules,
SKIPPED and UPSTREAM_FAILED propagation, `fail_fast`, map expansion with
`max_fanout`, the fan-in and its size limit, retries with backoff, timeouts,
and memoization within the run. Fixtures shared with the Go engine keep it
that way. Shell tasks run their `runtime.command` with the agent's NEXUS_*
environment. Python handlers run through `python -m nexus.worker`, or
in-process with `isolation="inprocess"` when you want a debugger. Results,
logs, `events.jsonl` and `run.json` go to the run directory.

It is not the platform: there are no leases or heartbeats, no queues,
priorities or fair share, no resource reservation or GPU placement, no
docker/kubernetes executors, and memoization does not cross runs. See
ADR-0022.

## Replaying a failed attempt

```python
doc = client.get_assignment(attempt_id)            # inputs + TaskSpec, no secrets
result = nexus.replay.replay(doc, modules=["mypkg.tasks"])
```

or `nexusctl replay <attempt-id> --local`. The attempt runs again on your
machine, with the inputs it was given and its original idempotency key. Blob
inputs stay references; pass their bytes with `inputs={"name": "/path"}`.

## Idempotency: three mechanisms, in descending order of strength

Execution is **at-least-once** (ADR-0006). Your handler can run more than once for
one logical unit of work. Which mechanism you use determines what guarantee you
actually have, so the SDK states the ordering rather than hiding it — a user who
believes they have mechanism 1 while relying on mechanism 3 has a correctness bug
they cannot see.

**1. Downstream-native dedup — strongest, no window.**

```python
receipt = payments.charge(..., idempotency_key=ctx.idempotency_key)
```

The downstream system stores the key atomically with the effect. Even a crash one
instruction later is safe, because the retry carries the same key.

**2. Conditional write — no window.**

```sql
INSERT INTO t (idem_key, ...) VALUES (%s, ...) ON CONFLICT (idem_key) DO NOTHING
```

Atomic in the target store. Requires that you control the schema. A
`SELECT`-then-`INSERT` is *not* equivalent — two attempts can both pass the SELECT.

**3. Platform effect journal — weakest; window narrowed, not closed.**

```python
if ctx.effect_recorded("charge"):
    return ctx.previous_result("charge")
receipt = do_the_thing()          # the effect
ctx.record_effect("charge", receipt)   # the journal write
```

For systems with neither of the above. **The write-then-crash window still
exists**: die between the effect and `record_effect`, and the retry repeats the
effect. What the journal buys is that the window shrinks from the whole task to a
single write. That is a real improvement and not a guarantee.

Record *after* the effect, never before: journaling first means a crash in between
skips the effect forever, which is worse than repeating it.

### The key

```
idempotency_key = sha256(run_id ‖ node_id ‖ map_index ‖ task_version)
```

`attempt_number` is deliberately excluded, so all retries of the same logical work
share one key — that exclusion is the mechanism. The platform derives it; you never
invent one. `idempotency="required"` (the default) permits auto-retry of an
indeterminate outcome; `"none"` refuses it and requires a human.

See `examples/idempotent_effect/` for a runnable demonstration, including the
window that mechanism 3 does *not* close.

## TaskContext

| Member | Purpose |
|---|---|
| `ctx.idempotency_key` | Platform-derived, stable across retries. |
| `ctx.heartbeat(msg)` | Liveness. Silence beyond `timeouts.heartbeat` means retry. |
| `ctx.progress(frac)` | Fractional progress; also a heartbeat. |
| `ctx.log.info(msg, **fields)` | Structured JSON with run/attempt correlation on every line. |
| `ctx.store.put/get` | Result store. Payloads never transit the control plane. |
| `ctx.effect_recorded` / `record_effect` / `previous_result` | The effect journal (mechanism 3). |
| `ctx.remaining_ms()` | Time left before `start_to_close`, per control-plane time. |
| `ctx.report_usage` / `report_tokens` / `usage_totals` | Usage units: tokens, requests, anything countable. See below. |
| `ctx.acquire_permits` / `settle_permits` | Shared provider rate limits. See below. |
| `ctx.checkpoint(state)` / `ctx.resume()` | Resume a retry mid-task. See below. |

## AI workloads: usage, rate limits, checkpoints

Batch inference, evaluation and fine-tuning are ordinary DAGs. What they need
beyond that is on the context (ADR-0032, OPERATIONS §29):

```python
from nexus import Retry, TaskContext, task
from nexus.llm import LLMClient

@task(retry=Retry(max_attempts=3, retry_on=["transient", "rate_limited"]))
def label(ctx: TaskContext, prompts: list[str]) -> list[str]:
    llm = LLMClient(ctx, provider="openai", model="gpt-4o-mini",
                    base_url="https://api.openai.com", api_key_env="OPENAI_API_KEY")
    state = ctx.resume() or {"next": 0, "out": []}      # a retry picks up here
    for i in range(state["next"], len(prompts)):
        state["out"].append(llm.complete(prompts[i], max_tokens=64).text)
        state["next"] = i + 1
        if i % 100 == 99:
            ctx.checkpoint(state)
        ctx.progress(state["next"] / len(prompts))
    return state["out"]
```

### The workload API

Under a worker agent (subprocess and docker executors) each attempt gets a
private HTTP/1.1 JSON API on a Unix socket: `NEXUS_TASK_API=unix:///…/api.sock`
and `NEXUS_TASK_API_TOKEN` (sent as `Authorization: Bearer`). The agent adds
the lease token and forwards to the control plane; the task never holds a
platform credential. The context methods below use it when it is there, and
nothing in a handler needs to know. Endpoints: `GET /v1/info`,
`POST /v1/usage`, `POST /v1/progress`, `POST /v1/permits`,
`POST /v1/permits/settle`, `PUT`/`GET /v1/checkpoint`, `POST /v1/error`
(ADR-0032 §1 has the table and the status codes).

| Member | Purpose |
|---|---|
| `ctx.report_usage({key: n})` | Report usage **increments**; returns the attempt's totals. |
| `ctx.report_tokens(provider, model, *, input_tokens=0, output_tokens=0, cached_tokens=0, requests=1)` | One call's tokens under the agreed keys. |
| `ctx.usage_totals` | This attempt's totals so far (a copy). |
| `ctx.acquire_permits(keys, *, requests=1, tokens=0, timeout=None)` → `PermitGrant` | Block until the platform grants all keys; raises `nexus.RateLimited` past `timeout` or the task's deadline. |
| `ctx.settle_permits(keys, tokens)` | Settle an estimate with `actual - estimate` (debt or refund). |
| `ctx.checkpoint(state)` → `CheckpointInfo` | Durably save JSON state (canonical JSON, at most 64 MiB). |
| `ctx.resume()` → state or `None` | The latest checkpoint of this work, from any earlier attempt. |
| `ctx.heartbeat()` / `ctx.progress()` | Now reach the control plane (F148); calls within 1 s are coalesced. |

`StaleLease` (and its subclass `StaleCheckpoint`) from any of these means the
platform has taken the attempt away; a retry may already be running. Stop.
When a handler fails, the worker entrypoints declare its class to the agent
(`POST /v1/error`), including `rate_limited` with the provider's retry-after.

### Usage keys

Model calls are billed per token, not per CPU-second, so usage units ride into
the usage ledger with the attempt's end, including for a failed attempt. The
key convention is shared with the price books, so it is a contract:

```
tokens:<provider>/<model>:input      tokens:<provider>/<model>:output
tokens:<provider>/<model>:cached     requests:<provider>/<model>
<kind>:<subject>[:<qualifier>]       e.g. images:openai/dall-e-3, units:embeddings
```

`kind` and `qualifier` are `[a-z][a-z0-9_]{0,31}`, `subject` is
`[A-Za-z0-9][A-Za-z0-9._/@+-]{0,127}`, a key is at most 200 bytes, values are
integers in `[0, 10^15]`, and an attempt has at most 64 keys.
`nexus.usage.token_units(provider, model, *, input_tokens=0, output_tokens=0,
cached_tokens=0, requests=0)` builds the token keys, omitting zeros;
`usage_subject` lower-cases the provider and maps characters outside the
subject alphabet to `_` (the `:` in an OpenAI fine-tuned model id).

### Rate limits

Limits are namespace policy in the quotas document
(`PUT /v1/namespaces/{ns}/quotas`), shared by every worker and scheduler
replica:

```json
"rate_limits": {
  "openai/gpt-4o-mini": {"requests_per_second": 50, "tokens_per_minute": 2000000},
  "anthropic":          {"requests_per_second": 10, "burst_requests": 20}
}
```

A permit ask is all keys or none and never waits on the server: the SDK
sleeps `retry_after_ms` plus jitter and asks again. Keys with no configured
limit are granted at once. A task that gives up raises `nexus.RateLimited`
(error class `rate_limited`, also a valid `Retry(retry_on=[...])` spelling),
which the scheduler retries after the provider's `Retry-After` without
charging the retry budget, up to a cap. A task that calls a provider without
permits is not throttled at all: use `LLMClient`, or call `acquire_permits`
yourself.

### LLMClient

```python
LLMClient(ctx, *, provider="openai" | "anthropic", model, base_url,
          api_key=None, api_key_env=None, rate_limit_keys=None, usage_provider=None,
          max_retries=5, max_retry_wait_s=60.0, timeout_s=60.0)
llm.complete(prompt_or_messages, *, max_tokens=256, temperature=0.0, system=None) -> Completion
# Completion(text, input_tokens, output_tokens, model, finish_reason, raw, cached_tokens)
```

Per call it estimates tokens (`ceil(chars/4) + max_tokens`), acquires permits
on `rate_limit_keys` (default `[provider, provider/model]`), posts with the
standard library, retries 429/503 honouring `Retry-After` (seconds or an HTTP
date) with exponential backoff and jitter, and on success settles
`actual - estimate` and reports usage under `usage_provider` (default
`provider`). Exhausted retries raise `RateLimited`; 401/403 raise
`LLMAuthError` (terminal), other 4xx `LLMRequestError` (terminal), persistent
5xx or network errors `LLMUnavailable` (transient). The key is never logged
and never shown by `repr`.

### Pointing at a real provider

| Provider | `base_url` | Key |
|---|---|---|
| OpenAI (or compatible) | `https://api.openai.com` | `api_key_env="OPENAI_API_KEY"` |
| Anthropic | `https://api.anthropic.com` | `api_key_env="ANTHROPIC_API_KEY"` |

Supply the key through the platform's secret mechanism (a `SecretRef`,
resolved by the worker at execution time and never stored by the control
plane) so that it reaches the task as the environment variable `api_key_env`
names, **never in a spec, a parameter or an input**: those are stored and
shown. Give the task network egress (the
docker executor defaults to `--network none`), and configure the namespace's
`rate_limits` for the model's real limits before fanning out. Nothing in this
repository calls a real provider; every test and example uses the mock below.

### Without a workload API

The local runner, unit tests and the kubernetes executor (F149) have no
workload API. The same calls then work in-process:

- **usage** accumulates on the context and is logged (`"msg": "usage"`) when
  the task ends;
- **permits** come from a process-local token bucket with the platform's
  semantics, **unlimited** unless `NEXUS_LOCAL_RATE_LIMITS` sets limits in the
  quotas' shape (`{"openai": {"requests_per_second": 2}}`); a WARN says once
  that nothing is shared with other processes;
- **checkpoints** go to `ctx.store` under `checkpoints/<idempotency_key>`. The
  local runner keeps one result store per run and every attempt of a node has
  the same idempotency key, so `ctx.resume()` in a local retry returns what the
  failed attempt checkpointed. A unit test does the same by building two
  contexts over one store with one key. The local runner retries
  `rate_limited` like `transient` (charged to the budget, without honouring
  `retry_after`).

### The mock model server

`nexus.testing.MockLLMServer` speaks OpenAI (`/v1/chat/completions`) and
Anthropic (`/v1/messages`), plus OpenAI-style fine-tuning (`/v1/files` as a
JSON body `{purpose, filename, content}`, not multipart;
`/v1/fine_tuning/jobs`, whose status advances
`validating_files → queued → running → succeeded` one step per poll) and
`/v1/models`:

```python
from nexus.testing import MockLLMServer

with MockLLMServer(rate_limit_every=5, retry_after_s=0.1) as mock:
    llm = LLMClient(ctx, provider="openai", model="mock-large",
                    base_url=mock.base_url, api_key="test")
    llm.complete("What is 12 + 7?").text      # "19"
    mock.stats.requests["/v1/chat/completions"], mock.stats.total_tokens
```

Answers are deterministic: arithmetic ("What is 12 + 7?"), sentiment
("Classify the sentiment …" → positive/negative/neutral), a strict grader
(system "You are a strict grader", lines `Reference:` and `Candidate:` → PASS
or FAIL), otherwise `Summary: <first 8 words>`. Tokens are whitespace word
counts. A base model named `*small*` gets about 30% of arithmetic wrong (by a
hash of the question) so an eval gate can fail it; `ft:` fine-tunes answer
correctly. `fail_first=N` answers the first N completions 503 and
`rate_limit_every=N` every Nth 429, both with `Retry-After`. A missing key is
401. As a compose sidecar:
`python -m nexus.testing.mock_llm --host 0.0.0.0 --port 8089 [--rate-limit-every N] [--retry-after S] [--fail-first N]`
(health check: `GET /healthz`).

## Client

```python
from nexus import NexusClient

c = NexusClient("http://localhost:8090", api_key="...", namespace="default")
c.submit_spec(daily_etl.compile())
run = c.create_run("daily-etl", params={"source": "s3://in"},
                   idempotency_key="daily-2026-09-12")   # sent as a header
for event in c.stream_events(run["id"]):
    print(event)
```

Retrying a failed run re-runs only what did not succeed. Name nodes to retry
just those and their downstream dependents; omit them to retry the whole run:

```python
c.retry_run(run["id"], nodes=["transform"])   # transform + its failed dependents
c.retry_run(run["id"])                        # every node that did not succeed
```

A selected node must be `FAILED`, `UPSTREAM_FAILED` or `CANCELLED`. An unknown
node, a node in another state, or one that would fail again because a failed
upstream is not also selected refuses the whole request (`NexusAPIError` 422 or
`Conflict` 409) and nothing is retried.

`create_run`'s `idempotency_key` is the **one** place a user-chosen key is correct:
it identifies your intent to start a run. Per-task effect keys are always
platform-derived; do not confuse the two.

### Schedules and backfills

Typed methods return the gateway's JSON as `Schedule`, `SchedulePreview` and
`Backfill` TypedDicts:

```python
from datetime import datetime, timezone

c.preview_schedule("30 2 * * *", timezone="Europe/London")      # nothing created
c.create_schedule("nightly-etl", workflow="daily-etl", cron="30 2 * * *",
                  timezone="Europe/London", params={"source": "s3://in"},
                  overlap_policy="skip", catchup_limit=1)
c.update_schedule("nightly-etl", cron="0 3 * * *")   # other fields kept
c.disable_schedule("nightly-etl"); c.enable_schedule("nightly-etl")

bf = c.start_backfill("nightly-etl",
                      start=datetime(2026, 9, 1, tzinfo=timezone.utc),
                      end="2026-09-08T00:00:00Z", max_active=2)
c.get_backfill(bf["id"])["cursor_at"]
c.pause_backfill(bf["id"]); c.resume_backfill(bf["id"])
```

`update_schedule` reads, merges and writes back, because the gateway replaces a
schedule whole; it is not atomic against a concurrent update. Backfill
intervals are half-open, `[start, end)`, and a naive `datetime` is refused. The
backfill actions check the current state first and raise `Conflict` for an
invalid transition, because the server does not (CONTRACT-FINDINGS F119).

## Running a handler as a container command

Executors hand a task its inputs as files in `$NEXUS_INPUT_DIR` and collect
its outputs from `$NEXUS_OUTPUT_DIR`. `nexus.entrypoint` runs a decorated
handler under that contract, so a task's runtime can be your image plus:

```python
@task(task_ref="etl.extract",
      runtime=Runtime(executor="docker", image="registry/etl:1.4.0",
                      command=["python", "-m", "nexus.entrypoint", "--module", "dag"],
                      args=["etl.extract"]))
def extract(ctx: TaskContext, source: str) -> dict: ...
```

Each input file is JSON-decoded into the parameter of the same name; the return
value is written to the declared output (or split by name across several). A
declared `max_bytes` is enforced before anything is written. Exit status 1
means the handler failed, 2 that it never started (bad module, unknown
task_ref, no output directory). `nexusctl init` generates projects wired this
way.

### Bounding a fan-in

Declare `max_bytes` on a map template's output and the gateway proves at
submit that `max_fanout x max_bytes` fits its fan-in limit:

```python
@task(outputs={"result": {"type": "json", "required": True, "max_bytes": 1024}})
```

## Worker entrypoint

Inside a task container, the agent invokes:

```bash
python -m nexus.worker --assignment assignment.json --module mypkg.tasks
```

One handler, one process, then exit. A wedged handler is killed by killing the
process, and an OOM kills the task rather than the agent — which keeps the agent's
lease-renewal loop alive to report the failure honestly.

## Tests

```bash
make sdk-test          # mypy --strict, then pytest with coverage
make sdk-typecheck     # mypy --strict only
```

Validation problems in a `.py` spec map to Python lines. `compiled.locate(path)`
returns the line of the `@task`, or of the workflow-body call for a wiring
problem, and a rejected `submit_spec` lists them in `NexusAPIError.problems`.
