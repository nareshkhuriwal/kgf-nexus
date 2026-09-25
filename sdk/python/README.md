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
python3 -m pytest sdk/python
```
