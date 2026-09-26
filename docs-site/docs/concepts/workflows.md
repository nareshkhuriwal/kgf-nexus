# Workflows and specs

A workflow is a document, the **WorkflowSpec**, not code. You can write it as
YAML or JSON, or generate it with the Python SDK. Either way the control plane
receives the same canonical document, validates it and stores it. The control
plane never imports or runs your Python. This is the largest structural
difference from Airflow ([ADR-0004](../design/adr/0004-declarative-workflow-spec.md)).

```yaml
api_version: nexus.dev/v1
namespace: default
name: hello
params:
  who: {type: string, default_json: '"world"'}
tasks:
  - id: greet
    task_ref: demo.greet
    outputs: {greeting: {type: string}}
    input_params: {who: who}
    inputs: {who: {type: string}}
    runtime:
      executor: subprocess
      command: [sh, -c]
      args: ['printf "hello, %s" "$(cat "$NEXUS_INPUT_DIR/who")"']
      stdout_output: greeting
```

The grammar is the JSON Schema at `schemas/workflow-spec-v1.json`. Every
gateway serves the copy it enforces at `GET /v1/schemas/workflow-spec-v1.json`.
The [workflow spec reference](../reference/workflow-spec.md) summarizes it.

## Versions and content addresses

When you submit a spec, the gateway validates it structurally (the JSON
Schema) and semantically (the graph is acyclic, edges connect declared outputs
to declared inputs of compatible types, map templates are bounded, and so on).
It then **canonicalizes** the spec (sorted keys, sorted tasks and edges, one
enum spelling) and hashes it. The hash is the spec's content address.

- Submitting byte-identical content again is a no-op that returns the existing
  version. CI can submit on every commit.
- Different content under the same name becomes version *n + 1*. Old versions
  are immutable and stay addressable.
- A run **pins** the spec id it was created from. Retries, replays and the
  console show exactly the document that run executed, even after newer
  versions exist.

`nexusctl workflow compile <file> --show` prints the canonical document and its
hash without contacting anything. Use it whenever a graph surprises you. The
SDK and nexusctl compute the same hash as the server; a parity test holds
them together (CONTRACT-FINDINGS F124).

## Workflow-level fields

| Field | Meaning |
| --- | --- |
| `params` | Typed run parameters (`int`, `float`, `string`, `bool`, `json`, `file_ref`) with optional JSON-encoded defaults. They are bound when a run is created. |
| `run_timeout_ms` | Wall-clock budget for the whole run. 0 means unbounded, which is allowed but discouraged. |
| `fail_fast` | Stop dispatching **new** work after the first terminal failure. Running attempts finish. Stopping them is what cancel does. |
| `max_active_runs` | Maximum number of concurrent runs of this workflow. |
| `priority` | Default priority for the workflow's runs. |
| `labels` | Free-form metadata. |

## Runs

A run is one execution of one spec version with bound parameters. The run
state machine (`PENDING → RUNNING → SUCCEEDED | FAILED | CANCELLED | TIMED_OUT`,
with `PAUSING`/`PAUSED` and `CANCELLING` in between) is described in [architecture §5.2](../design/architecture.md#52-workflowrun).

- **Create.** `POST .../runs` or `nexusctl run create NAME --param k=v`. An
  `Idempotency-Key` header (`--idempotency-key`) makes creation safe to retry.
  This is the only place a key you choose is correct; per-task effect keys
  are always derived by the platform.
- **Pause and resume.** A paused run dispatches nothing new. Running attempts
  finish.
- **Cancel.** Stops dispatch and cancels running attempts, which receive
  SIGTERM, or wait if they are
  [uninterruptible](../design/adr/0015-bounded-uninterruptible-tasks.md).
- **Retry.** Re-runs only what did not succeed, either the whole run or
  selected nodes and their dependents (runbook §18).

## Authoring paths

| Path | When to use it |
| --- | --- |
| Python SDK (`@task`, `@workflow`) | Python handlers, and graphs built with loops and helpers. The body is traced once, at compile time, on your machine. |
| YAML/JSON | Shell or container tasks, generated specs, and non-Python teams. |
| `nexusctl import airflow\|prefect\|argo` | Migration. It produces a spec plus a report of what did not translate. See [Migrating](../migrate/index.md). |

All three produce the same document, and none of them is second-class.
