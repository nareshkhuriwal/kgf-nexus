# Migrating from Argo Workflows

```bash
nexusctl import argo workflows/ --out-dir nexus/ --report import-report.md
```

Reads `Workflow`, `WorkflowTemplate`, `ClusterWorkflowTemplate` and
`CronWorkflow` documents (several per file are fine). No interpreter is needed.
Argo templates are **inlined**: a `dag` or `steps` template called from a task
becomes that task's subgraph, with node ids prefixed by the calling task
(`B.A`), the way nested DAGs appear in the Argo UI.

Argo substitutes `{{...}}` parameters into commands as text before a container
starts. Where the value is known at conversion time (a literal argument), the
converter does the same substitution. Where it is only known at run time (a
workflow parameter, an upstream output, a loop item), it becomes a Nexus input,
read from `$NEXUS_INPUT_DIR` by a small shell prelude; that requires a shell in
the image and is reported as approximated.

See [Migrating to Nexus](index.md) for the output flags, exit codes and the
rule that nothing is written while a construct is unsupported.

## Concept mapping

| Argo | Nexus | Fidelity |
|---|---|---|
| `metadata.name` / `generateName` | spec name | translated |
| `spec.entrypoint` | the graph's top level | translated |
| `spec.arguments.parameters` | `params` (string; `value` as default) | translated; `enum` approximated, `valueFrom` unsupported |
| `container` template (`image`, `command`, `args`, `env`, `workingDir`) | kubernetes executor task | translated |
| `script` template (`command` + `source`) | `sh -c`/`bash -c`/`python -c`/`node -e` + source; other interpreters via a temporary file | translated / approximated |
| `resources.requests` / `limits` (`cpu`, `memory`, `ephemeral-storage`, `nvidia.com/gpu`) | `resources.request` / `limit` (millicores, bytes, `gpu_count`) | translated (requests default to limits, as in Kubernetes) |
| other extended resources | `resources.*.custom` | approximated (a worker must advertise it) |
| `podResources` (pod level) | per-task resources where the task declares none | approximated |
| `dag` tasks with `dependencies:` | edges | translated |
| `depends: "A && B.Succeeded"` | edges, `all_success` | translated |
| `depends` where every dependency is `(X.Succeeded \|\| X.Failed ...)` | edges, `trigger_rule: all_done` | approximated |
| other `depends` (`\|\|`, `!`, `.Failed` alone, mixed) | edges kept, `all_success` | **unsupported** |
| `steps` (sequential groups, parallel inner lists) | every step waits for every step of the previous group | translated |
| nested `dag`/`steps` templates | inlined subgraph, ids `task.child` | translated |
| `{{inputs.parameters.x}}` bound to a literal | substituted at conversion | translated |
| `{{workflow.parameters.x}}` | input bound to param `x` | approximated (shell prelude) |
| `{{tasks.X.outputs.result}}` / `{{steps.X.outputs.result}}` | edge carrying `X`'s stdout (`stdout_output: result`) | translated |
| `{{tasks.X.outputs.parameters.p}}` with `valueFrom.path` | the file is copied to `$NEXUS_OUTPUT_DIR/p` after the command; edge carrying `p` | approximated |
| output artifacts with `path` consumed via `from:` | copied into the result store and back to the consumer's `path` | approximated (files, not directories) |
| `raw` input artifacts | written to `path` before the command | translated |
| artifacts from S3/GCS/Git/HTTP..., `arguments.artifacts` | none | **unsupported** (fetch inside the task) |
| `withItems` (literal list) | generated `<task>__items` task emits the list; map, `max_fanout` = length | approximated |
| `withSequence` (literal `count`/`start`/`end`) | as `withItems` | approximated; `format` and run-time bounds unsupported |
| `withParam: "{{tasks.X.outputs.result}}"` | map over `X`'s output | translated (`max_fanout` from `--max-fanout`, reported) |
| `withParam: "{{workflow.parameters.p}}"` | map over the param (declared json) | approximated |
| `{{item.field}}`, loops over a `dag`/`steps` template | failing placeholder | **unsupported** |
| outputs of a looped task used downstream | fan-in: JSON array of the children's outputs | translated |
| `retryStrategy.limit` | `retry.max_attempts = limit + 1` (no limit: 100) | translated / approximated |
| `backoff.duration` / `factor` / `cap` | `initial_ms` / exponential `multiplier` / `max_ms` | translated |
| `backoff.maxDuration` | `timeouts.schedule_to_close_ms` | approximated |
| `retryPolicy` `OnFailure` (default) / `OnError` / `OnTransientError` / `Always` | `retry_on` error classes | approximated / translated |
| `retryStrategy.affinity.nodeAntiAffinity` | `retry_on_different_worker` | translated |
| `retryStrategy.expression` | none | **unsupported** |
| template `activeDeadlineSeconds` | `timeouts.start_to_close_ms` | translated |
| template `timeout` | `timeouts.schedule_to_close_ms` | approximated |
| `spec.activeDeadlineSeconds` | `run_timeout_ms` | translated |
| `failFast: false` | `fail_fast: false` | translated |
| `memoize` | `cache` (keyed on inputs, `maxAge` as TTL) | approximated |
| `synchronization.mutex` | `concurrency_key` with limit 1 | translated |
| `synchronization.semaphore`, `parallelism` | none | **unsupported** |
| `CronWorkflow` `schedule`/`schedules`, `timezone` | companion `nexusctl schedule create` | translated |
| `concurrencyPolicy` `Allow` / `Forbid` | `--overlap allow` / `skip` | translated |
| `concurrencyPolicy: Replace` | none (the suggested schedule uses `skip`) | **unsupported** |
| `suspend: true` (CronWorkflow) | `schedule create --disabled` | translated |
| `startingDeadlineSeconds`, history limits, `ttlStrategy`, `podGC`, metrics | ignored | approximated |
| `when` | the task runs unconditionally in a partial spec | **unsupported** |
| `onExit`, `hooks`, `continueOn` | none | **unsupported** |
| `templateRef`, `workflowTemplateRef`, inline templates | failing placeholder | **unsupported** |
| `resource`, `suspend`, `http`, `plugin`, `containerSet`, `data` templates | failing placeholder | **unsupported** |
| `env.valueFrom` (`secretKeyRef`, `configMapKeyRef`, `fieldRef`) | none | **unsupported** |
| volumes, `serviceAccountName`, `nodeSelector`, `tolerations`, `affinity`, `securityContext`, sidecars, `podSpecPatch` | none | **unsupported** (the Nexus kubernetes executor does not take pod settings from the spec) |
| `{{workflow.name}}`, `{{pod.name}}` | `$NEXUS_RUN_ID`, `$NEXUS_ATTEMPT_ID` | approximated |
| `{{workflow.scheduledTime}}`, `{{workflow.status}}`, `{{=expr}}` | none | **unsupported** |

## Cannot translate

- **Conditions** (`when`, enhanced `depends` with `||`, `!` or `.Failed`).
  Nexus DAGs are static with one trigger rule per task (`all_success` or
  `all_done`). Move the condition into the task.
- **Exit handlers and lifecycle hooks** (`onExit`, `hooks`). Watch the run
  (`nexusctl events --follow`), or add a final task with `trigger_rule: all_done`.
- **Resources Argo manages for you**: `resource` templates, `suspend`, HTTP
  templates, external artifact repositories, volumes and pod settings, secrets
  from Kubernetes Secrets.
- **Templates stored in the cluster** (`templateRef`, `workflowTemplateRef`):
  convert that `WorkflowTemplate`'s YAML and inline the template.
- **Expressions** (`{{= ...}}`) and run-time parts of loops (`{{item.field}}`,
  `withSequence` with a parameter).

## Worked example

Argo's own `loops-param-result.yaml` and `cron-workflow.yaml` (Argo Workflows
v4.1.4) in `workflows/`:

```console
$ nexusctl import argo workflows/ --out-dir nexus/ --allow-partial
Imported 2 Argo Workflows workflow(s) from workflows/: 6 translated, 5 approximated, 2 unsupported.

hello-world -> hello-world (1 tasks, 0 edges) -> nexus/hello-world.yaml
  2 translated, 3 approximated, 2 unsupported
  UNSUPPORTED   cron-workflow.yaml:10  concurrencyPolicy           concurrencyPolicy Replace cancels the running workflow when the next one is due; Nexus schedules skip, allow or queue one (the suggested schedule uses the default, skip)
  UNSUPPORTED   cron-workflow.yaml:21  template reference          hello-world-with-time: {{workflow.scheduledTime}} is the run's scheduled/creation time, which Nexus does not pass to tasks; add a workflow param or compute it in the task
  approximated  cron-workflow.yaml:9   startingDeadlineSeconds     startingDeadlineSeconds is managed by the Nexus scheduler; ignored
  approximated  cron-workflow.yaml:11  successfulJobsHistoryLimit  successfulJobsHistoryLimit is managed by the Nexus scheduler; ignored
  approximated  cron-workflow.yaml:12  failedJobsHistoryLimit      failedJobsHistoryLimit is managed by the Nexus scheduler; ignored
  schedule: nexusctl schedule create hello-world --workflow hello-world --cron '* * * * *' --timezone America/Los_Angeles --namespace default

loops-param-result -> loops-param-result (2 tasks, 1 edges) -> nexus/loops-param-result.yaml
  4 translated, 2 approximated, 0 unsupported
  approximated  loops-param-result.yaml:16  command     sleep: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand)
  approximated  loops-param-result.yaml:16  max_fanout  sleep: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly

PARTIAL specs written (--allow-partial): unsupported constructs are failing placeholder tasks.
$ cat nexus/loops-param-result.yaml
# Generated by `nexusctl import argo` from workflows/loops-param-result.yaml:3 (loops-param-result).
# 4 translated, 2 approximated, 0 unsupported. Read the import report before submitting.
api_version: nexus.dev/v1
namespace: default
name: loops-param-result
labels:
  imported-from: argo
  source-id: loops-param-result
tasks:
  - id: generate
    task_ref: argo/loops-param-result/generate
    outputs:
      result:
        type: json
    retry:
      max_attempts: 1
    runtime:
      executor: kubernetes
      image: python:alpine3.23
      command:
        - python
        - -c
      args:
        - |
          import json
          import sys
          json.dump([i for i in range(20, 31)], sys.stdout)
      stdout_output: result
  - id: sleep
    task_ref: argo/loops-param-result/sleep
    inputs:
      seconds:
        type: json
        required: true
    map:
      over_node_id: generate
      over_output: result
      item_input: seconds
      max_fanout: 1024
    retry:
      max_attempts: 1
    runtime:
      executor: kubernetes
      image: alpine:3.23
      command:
        - sh
        - -c
      args:
        - |-
          nexus_in() { v=$(cat "$NEXUS_INPUT_DIR/$1"); case $v in \"*\") v=${v#\"}; v=${v%\"} ;; esac; printf '%s' "$v"; }
          echo sleeping for $(nexus_in seconds) seconds; sleep $(nexus_in seconds); echo done
edges:
  - from_node_id: generate
    to_node_id: sleep
```

The generator script's stdout (`outputs.result`) is the `result` output; the
`withParam` loop is a Nexus map over it, and each child receives one element in
its `seconds` input. Set `max_fanout` to the real bound of your collection.

Converted Argo specs can be tried on a laptop before a cluster: `nexusctl run
--local nexus/loops-param-result.yaml --host-executor` runs each container's
command on the host (not in its image).

## After the import

1. Fix every **unsupported** row and read every **approximated** one.
2. Container tasks use the `kubernetes` executor. Pin images by digest, and move
   pod settings the executor does not take from the spec (service accounts,
   volumes, node selection) into the agent's configuration.
3. Provide secrets through the worker environment; `valueFrom.secretKeyRef` is
   not converted.
4. `nexusctl workflow submit nexus/<name>.yaml --dry-run`, then submit, then run
   the companion `nexusctl schedule create ...` commands from the report.
5. Review idempotency: Nexus runs tasks at least once. See
   [Migrating to Nexus](index.md#what-every-migration-needs-afterwards).
