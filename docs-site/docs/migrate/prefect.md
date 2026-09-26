# Migrating from Prefect

```bash
nexusctl import prefect flows/ --out-dir nexus/ --report import-report.md
```

Point it at a flow file or a directory. In a directory, `prefect.yaml` is read
too, and each deployment's schedules are attached to the flow its
`entrypoint` names.

Flow files are read **statically**: an analyzer embedded in `nexusctl` parses
them with Python's `ast` module, run by your interpreter (`--python`, default
`python3`, 3.8 or later, Prefect not required). It never imports or executes
them. The flow body is *read*, not traced: task calls, `.submit()`, `.map()`,
`wait_for=` and the futures passed between tasks become the graph. Anything the
body decides at run time (a loop, an `if` on a task result) is reported as
unsupported with its line.

See [Migrating to Nexus](index.md) for the output flags, exit codes and the
rule that nothing is written while a construct is unsupported.

## Concept mapping

| Prefect | Nexus | Fidelity |
|---|---|---|
| `@flow` / `@flow(name=...)` | one WorkflowSpec; `name` (or the function name) becomes the spec name | translated |
| flow parameters with type hints and defaults | `params` (`str` string, `int`, `float`, `bool`, anything else json); no default means required | translated |
| flow docstring / `description` | `description` | translated |
| `@flow(timeout_seconds=)` | `run_timeout_ms` | translated |
| a raising task fails the flow | `fail_fast: true` | approximated (tasks already running finish) |
| `@flow(retries=)` | none | **unsupported** (Nexus retries tasks; retry a run with `nexusctl run retry`) |
| a flow that calls no task | one task running the whole flow function | approximated |
| `@task` function | task running `python -m nexus.entrypoint --module <file> <file>.<function>` | approximated (re-decorate with `@nexus.task`) |
| `task(x)`, `task.submit(x)`, `future.result()` | edge from the task that produced `x`, `output_to_input` `{param: result}` | translated |
| literal arguments / flow parameters passed to a task | `input_literals` / `input_params` | translated |
| `wait_for=[a, b]` | ordering edges | translated |
| `task.map(upstream_result, unmapped(x))` | map over the upstream output; `unmapped` values are ordinary inputs | translated (`max_fanout` from `--max-fanout`, reported) |
| `task.map([literal list])` | a generated `<task>__items` task emits the list; map over it | approximated |
| results of a mapped task passed downstream | fan-in: JSON array of the children's results | translated |
| `.map()` over several iterables (zip) | none | **unsupported** |
| `task.with_options(...)` | the overridden options | translated |
| `retries` | `retry.max_attempts = retries + 1` | translated |
| `retry_delay_seconds=N` | fixed backoff | translated |
| `retry_delay_seconds=[a, b, ...]` | exponential from `a` with multiplier `b/a`, capped at the largest | approximated |
| `retry_delay_seconds=exponential_backoff(backoff_factor=f)` | exponential, initial `f` s, multiplier 2 | translated |
| `retry_jitter_factor` | full jitter | approximated |
| `timeout_seconds` | `timeouts.start_to_close_ms` | translated |
| `cache_key_fn=task_input_hash`, `cache_policy=INPUTS` | `cache.enabled` (Nexus keys on task_ref + version + input hash) | translated |
| `cache_expiration` | `cache.ttl_ms` | translated |
| `TASK_SOURCE`, custom key functions/policies | cache enabled | approximated (bump the task `version` when code changes) |
| `DEFAULT`/`RUN_ID` cache policies | no cache | approximated |
| `tags` (and tag concurrency limits) | none | approximated (use `concurrency_key`/`concurrency_limit`) |
| `flow.serve(cron=)`, `flow.deploy(cron=)`, `to_deployment(cron=)` | companion `nexusctl schedule create`, with `parameters=` as `--param` | translated |
| `interval=` / `Interval(...)` | cron on wall-clock boundaries when the interval divides evenly | approximated |
| `Cron("...", timezone=)` / `schedules=[...]` | one companion schedule each | translated |
| `prefect.yaml` deployments: `schedules` (`cron`, `interval`, `timezone`, `active`) | companion schedules (`active: false` gives `--disabled`) | translated / approximated |
| `rrule` schedules | none | **unsupported** |
| work pools, images, `build`/`push`/`pull` steps | none | approximated (infrastructure, not part of a spec) |
| subflow call (a `@flow` called from a flow) | failing placeholder; the subflow is converted as its own workflow | **unsupported** |
| `if` / `for` / `while` in the flow body that creates tasks | failing placeholders | **unsupported** |
| `print`/logging in the flow body | not run | approximated (task logs are kept per attempt) |
| state hooks (`on_failure`, `on_completion`...), `retry_condition_fn`, `return_state=True` | none | **unsupported** |
| `async` tasks | the entrypoint calls handlers synchronously | approximated |

## Cannot translate

- **Control flow on run-time values**: `if total.result() > limit:`,
  `for x in some_task():`, `while` loops. A Nexus DAG is static. Move the
  decision into a task, or use a map for "one task per element".
- **Loops that call tasks**, even over literal ranges: write the tasks out or
  use `.map()` (which converts to a Nexus map).
- **Subflows.** Nexus has no nested workflows. Inline the subflow's tasks, or
  start it from a task with `nexusctl run create`.
- **Flow-level retries**, state hooks, `retry_condition_fn`, `rrule` schedules,
  `.map()` over several iterables.

## Worked example

Prefect's own `per_worker_task_concurrency.py` example (Prefect 3.8.6) in `flows/`:

```console
$ nexusctl import prefect flows/ --out-dir nexus/
Imported 1 Prefect workflow(s) from flows/: 3 translated, 5 approximated, 0 unsupported.

process_image -> process_image (3 tasks, 2 edges) -> nexus/process_image.yaml
  3 translated, 5 approximated, 0 unsupported
  approximated  per_worker_task_concurrency.py:58   python task  download_image: runs per_worker_task_concurrency.download_image through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="per_worker_task_concurrency.download_image") -- inputs arrive as keyword arguments and the return value is output `result`
  approximated  per_worker_task_concurrency.py:67   python task  save_results: runs per_worker_task_concurrency.save_results through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="per_worker_task_concurrency.save_results") -- inputs arrive as keyword arguments and the return value is output `result`
  approximated  per_worker_task_concurrency.py:82   python task  run_ml_model: runs per_worker_task_concurrency.run_ml_model through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="per_worker_task_concurrency.run_ml_model") -- inputs arrive as keyword arguments and the return value is output `result`
  approximated  per_worker_task_concurrency.py:104  @flow        a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks already running finish)
  approximated  per_worker_task_concurrency.py:114  logging      a log call in the flow body does not run in Nexus (there is no flow-body process); task logs are captured per attempt

Specs written.
$ cat nexus/process_image.yaml
# Generated by `nexusctl import prefect` from flows/per_worker_task_concurrency.py:104 (process_image).
# 3 translated, 5 approximated, 0 unsupported. Read the import report before submitting.
api_version: nexus.dev/v1
namespace: default
name: process_image
description: |-
  Process an image through the ML pipeline.

  When multiple instances run concurrently on the same worker:
  - download and save tasks overlap freely
  - run_ml_model tasks are limited by the per-worker GPU concurrency limit
labels:
  imported-from: prefect
  source-id: process_image
params:
  image_id:
    type: int
    default_json: "1"
fail_fast: true
tasks:
  - id: download_image
    task_ref: per_worker_task_concurrency.download_image
    inputs:
      image_id:
        type: json
        required: true
    outputs:
      result:
        type: json
    input_params:
      image_id: image_id
    retry:
      max_attempts: 1
    runtime:
      executor: subprocess
      command:
        - python3
        - -m
        - nexus.entrypoint
        - --module
        - per_worker_task_concurrency
      args:
        - per_worker_task_concurrency.download_image
  - id: run_ml_model
    task_ref: per_worker_task_concurrency.run_ml_model
    inputs:
      data:
        type: json
        required: true
    outputs:
      result:
        type: json
    retry:
      max_attempts: 1
    runtime:
      executor: subprocess
      command:
        - python3
        - -m
        - nexus.entrypoint
        - --module
        - per_worker_task_concurrency
      args:
        - per_worker_task_concurrency.run_ml_model
  - id: save_results
    task_ref: per_worker_task_concurrency.save_results
    inputs:
      data:
        type: json
        required: true
    outputs:
      result:
        type: json
    retry:
      max_attempts: 1
    runtime:
      executor: subprocess
      command:
        - python3
        - -m
        - nexus.entrypoint
        - --module
        - per_worker_task_concurrency
      args:
        - per_worker_task_concurrency.save_results
edges:
  - from_node_id: download_image
    to_node_id: run_ml_model
    output_to_input:
      data: result
  - from_node_id: run_ml_model
    to_node_id: save_results
    output_to_input:
      data: result
```

The data flow `image = download_image(image_id)` → `run_ml_model(image)` →
`save_results(predictions)` became two edges carrying `result`; `image_id` is a
workflow param bound to `download_image`'s input. The example's per-worker GPU
limit is a Prefect global concurrency limit taken inside the task; in Nexus,
declare it on the task instead (`concurrency_key`/`concurrency_limit`, or a GPU
in `resources`).

The only code change is the decorator:

```python
from nexus import task

@task(task_ref="per_worker_task_concurrency.run_ml_model")
def run_ml_model(data: dict) -> dict:
    ...
```

## After the import

1. Fix every **unsupported** row of the report and read every **approximated** one.
2. Re-decorate each task function with `@nexus.task(task_ref=...)` in an
   importable module; flow-body code other than task calls does not run in
   Nexus, so move anything that matters into a task.
3. `nexusctl workflow submit nexus/<name>.yaml --dry-run`, then submit, then run
   the companion `nexusctl schedule create ...` commands from the report.
4. Replace Prefect blocks, secrets and variables with worker environment or
   image configuration.
5. Review idempotency: Nexus runs tasks at least once. See
   [Migrating to Nexus](index.md#what-every-migration-needs-afterwards).
