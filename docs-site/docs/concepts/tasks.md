# Tasks

A task is one node of the graph. It has a **typed interface**, a **runtime**
and a set of **policies**.

## Interface: inputs and outputs

`inputs` and `outputs` map names to `ValueSpec`s: a `type` (`int`, `float`,
`string`, `bool`, `json` or `file_ref`), whether the value is `required`, a
JSON-encoded `default_json`, and an optional `max_bytes` bound. The gateway
type-checks every edge against both ends when you submit, so a wiring mistake
is caught before anything runs.

An input is bound from exactly one source. When more than one is present,
this order applies:

1. an **edge** from an upstream output (`edges[].output_to_input`, keyed by the
   *downstream input*);
2. a **run parameter** (`input_params: {input: param}`);
3. a **literal** (`input_literals`, JSON-encoded) or the input's default.

## Runtime: what runs, and where

| `runtime.executor` | Runs the task as | Isolation |
| --- | --- | --- |
| `subprocess` | A process on the agent's host or in its container, in its own process group | None beyond the agent's. Limits cannot be enforced; see [resources](resources.md). |
| `docker` | A sibling container started through the Docker CLI, with `--network none` | cgroup memory and CPU limits, and GPU device isolation |
| `kubernetes` (alias `k8s`) | A Kubernetes Job in the agent's namespace, with a collector sidecar | Pod security `restricted`, default-deny network policy, and requests and limits ([recipe](../recipes/kubernetes-executor.md)) |

An agent advertises an executor only after it has verified that it can use
it. The scheduler treats the executor as a placement constraint, so a
`docker` task is never sent to an agent that cannot start containers.

Every executor gives the task the same file contract:

| Variable | Meaning |
| --- | --- |
| `NEXUS_INPUT_DIR` | One file per input, named after the input. JSON-typed values are JSON text. |
| `NEXUS_OUTPUT_DIR` | Write one file per declared output, named after the output. |
| `NEXUS_OUTPUT_FILE`, `NEXUS_OUTPUT_NAMES` | The legacy single-output file (reported as `result`), and the declared output names. |
| `NEXUS_IDEMPOTENCY_KEY` | The platform-derived key, stable across retries. See [idempotency](retries-idempotency.md). |
| `NEXUS_DEADLINE_UNIX_MS` | When `start_to_close` expires, in control-plane time. |
| `NEXUS_INLINE_THRESHOLD_BYTES` | The size above which results go to the object store. |
| `NEXUS_WORK_DIR` | The attempt's scratch directory. |

`runtime.stdout_output` names a declared output that receives the task's
stdout. A file with the same name in `$NEXUS_OUTPUT_DIR` takes precedence.

Python handlers run through `python -m nexus.entrypoint --module <module> <task_ref>`,
which maps input files to parameters and return values to outputs. The
`nexusctl init` templates are set up this way.

## Policies

| Field | What it controls | Read more |
| --- | --- | --- |
| `resources.request` / `limit` | What the scheduler reserves, and what the executor enforces | [Resources and GPUs](resources.md) |
| `queue`, `priority`, `node_selector`, `prefer_same_worker_as` | Routing. The node selector is a hard constraint; `prefer_same_worker_as` is a soft locality hint and never a correctness constraint. | [Queues and fair share](queues-fair-share.md) |
| `timeouts` | `schedule_to_start`, `start_to_close`, `schedule_to_close` and `heartbeat` (Temporal's four-timeout model). 0 means not enforced. | [Retries](retries-idempotency.md#timeouts) |
| `retry` | Attempts, backoff, jitter, and which error classes are retried | [Retries](retries-idempotency.md) |
| `idempotency` | `required` (default), `best_effort` or `none` | [Idempotency](retries-idempotency.md#idempotency) |
| `cache` | Memoization keyed on `(task_ref, version, inputs)` | [Results](results-artifacts.md#memoization) |
| `version` | Part of the idempotency and cache keys. Bump it when the task's effect changes. | |
| `concurrency_key` / `concurrency_limit` | A global semaphore across all runs, for example "at most 2 concurrent writers to this table" | |
| `cancellation.uninterruptible` | No SIGTERM on cancel. The task may finish within a bound, then it is killed. | [ADR-0015](../design/adr/0015-bounded-uninterruptible-tasks.md) |
| `map` | Makes the task a template expanded at runtime | [Fan-out and fan-in](fan-out-fan-in.md) |
| `trigger_rule` | When the node may run, given its upstreams' outcomes | [DAG semantics](dag-semantics.md) |

## Attempts

Each execution of a task is an **attempt**, and attempts are immutable
history. Attempt states are `PENDING → DISPATCHED → RUNNING`, ending in
`SUCCEEDED`, `FAILED`, `TIMED_OUT`, `LOST` (the worker's lease expired) or
`CANCELLED`. `LOST` is kept distinct because a lost attempt may or may not
have produced its effect ([architecture §6.5](../design/architecture.md#65-handling-indeterminate-outcomes)).
A node's state (`PENDING`, `READY`, `RUNNING`, `SUCCEEDED`, `FAILED`,
`SKIPPED`, `UPSTREAM_FAILED`, `CANCELLED`) is derived from its attempts.
