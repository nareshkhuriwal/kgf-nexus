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
