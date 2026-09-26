# nexusctl import argo report

- Input: `testdata/cases/argo/depends-and-unsupported.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 30 translated, 21 approximated, 9 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## Depends-And-Unsupported -> `depends-and-unsupported`

Source `testdata/cases/argo/depends-and-unsupported.yaml:3`. 13 task(s), 15 edge(s). 30 translated, 21 approximated, 9 unsupported. Content address `eec91153d65e`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `depends-and-unsupported.yaml:3` | name | name | Depends-And-Unsupported is named depends-and-unsupported (the schema allows lowercase letters, digits, '-', '_' and '.') |
| unsupported | `depends-and-unsupported.yaml:9` | onExit |  | exit handler cleanup runs after the workflow whatever its outcome; Nexus has no exit handlers (watch the run with `nexusctl events --follow`, or add a final task with trigger_rule all_done). The handler's templates were not converted |
| unsupported | `depends-and-unsupported.yaml:10` | parallelism |  | parallelism caps a run's concurrent pods; Nexus has no per-run cap (use concurrency_key/concurrency_limit on contending tasks) |
| translated | `depends-and-unsupported.yaml:13` | parameter | params.env (string) | workflow parameter env |
| translated | `depends-and-unsupported.yaml:15` | parameter | params.shards (string) | workflow parameter shards |
| approximated | `depends-and-unsupported.yaml:21` | command | tasks[A].runtime | A: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `depends-and-unsupported.yaml:21` | command | tasks[A].runtime | A: {{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID |
| translated | `depends-and-unsupported.yaml:21` | container | tasks[A].runtime (kubernetes executor) | A: template work (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:21` | resources | tasks[A].resources | A: resource requests/limits |
| approximated | `depends-and-unsupported.yaml:23` | command | tasks[B].runtime | B: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `depends-and-unsupported.yaml:23` | command | tasks[B].runtime | B: {{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID |
| translated | `depends-and-unsupported.yaml:23` | container | tasks[B].runtime (kubernetes executor) | B: template work (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:23` | resources | tasks[B].resources | B: resource requests/limits |
| translated | `depends-and-unsupported.yaml:24` | depends | edges | B: depends "A.Succeeded": all must succeed |
| translated | `depends-and-unsupported.yaml:26` | container | tasks[C].runtime (kubernetes executor) | C: template flaky (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:27` | depends | edges | C: depends "A": all must succeed |
| approximated | `depends-and-unsupported.yaml:29` | command | tasks[all-done].runtime | all-done: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `depends-and-unsupported.yaml:29` | command | tasks[all-done].runtime | all-done: {{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID |
| translated | `depends-and-unsupported.yaml:29` | container | tasks[all-done].runtime (kubernetes executor) | all-done: template work (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:29` | resources | tasks[all-done].resources | all-done: resource requests/limits |
| approximated | `depends-and-unsupported.yaml:30` | depends | edges | all-done: depends "(B.Succeeded \|\| B.Failed) && (C.Succeeded \|\| C.Failed \|\| C.Errored)": runs once every dependency finished, however it ended (trigger_rule all_done; it also fires after a cancelled or skipped upstream) |
| approximated | `depends-and-unsupported.yaml:32` | command | tasks[either].runtime | either: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `depends-and-unsupported.yaml:32` | command | tasks[either].runtime | either: {{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID |
| translated | `depends-and-unsupported.yaml:32` | container | tasks[either].runtime (kubernetes executor) | either: template work (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:32` | resources | tasks[either].resources | either: resource requests/limits |
| unsupported | `depends-and-unsupported.yaml:33` | depends | edges | either: depends "B \|\| C" needs per-dependency conditions (\|\|, !, .Failed, or a mix of must-succeed and may-fail dependencies); Nexus has one trigger rule per task (all_success or all_done). The edges were kept with all_success |
| approximated | `depends-and-unsupported.yaml:35` | command | tasks[not-c].runtime | not-c: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `depends-and-unsupported.yaml:35` | command | tasks[not-c].runtime | not-c: {{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID |
| translated | `depends-and-unsupported.yaml:35` | container | tasks[not-c].runtime (kubernetes executor) | not-c: template work (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:35` | resources | tasks[not-c].resources | not-c: resource requests/limits |
| unsupported | `depends-and-unsupported.yaml:36` | depends | edges | not-c: depends "A && !C.Skipped" needs per-dependency conditions (\|\|, !, .Failed, or a mix of must-succeed and may-fail dependencies); Nexus has one trigger rule per task (all_success or all_done). The edges were kept with all_success |
| approximated | `depends-and-unsupported.yaml:38` | command | tasks[gated].runtime | gated: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `depends-and-unsupported.yaml:38` | command | tasks[gated].runtime | gated: {{workflow.name}}/{{pod.name}} become $NEXUS_RUN_ID/$NEXUS_ATTEMPT_ID |
| translated | `depends-and-unsupported.yaml:38` | container | tasks[gated].runtime (kubernetes executor) | gated: template work (image alpine:3.23) |
| translated | `depends-and-unsupported.yaml:38` | resources | tasks[gated].resources | gated: resource requests/limits |
| unsupported | `depends-and-unsupported.yaml:41` | when | tasks[gated] | gated: `when: {{workflow.parameters.env}} == prod` runs the task only if a condition holds at run time; Nexus DAGs are static, so in the partial spec it runs unconditionally. Move the condition into the task |
| unsupported | `depends-and-unsupported.yaml:42` | template | tasks[remote] (placeholder) | remote: templateRef shared-templates/publish runs a template stored in the cluster; convert that WorkflowTemplate and inline the template here |
| unsupported | `depends-and-unsupported.yaml:47` | template | tasks[approve] (placeholder) | approve: a suspend template pauses the workflow for approval or a duration; Nexus has `nexusctl run pause`/`resume` but no in-DAG suspension |
| approximated | `depends-and-unsupported.yaml:50` | script source | tasks[per-shard].runtime | per-shard: run-time {{...}} values are substituted into the Python source by sh, textually, as Argo does; reading them from $NEXUS_INPUT_DIR in the script is more robust |
| approximated | `depends-and-unsupported.yaml:50` | command | tasks[per-shard].runtime | per-shard: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| approximated | `depends-and-unsupported.yaml:50` | max_fanout | tasks[per-shard].map.max_fanout | per-shard: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `depends-and-unsupported.yaml:50` | script | tasks[per-shard].runtime (kubernetes executor) | per-shard: template shard-work (image python:3.12-alpine) |
| approximated | `depends-and-unsupported.yaml:57` | withParam | map | per-shard: map over param shards, now declared json (pass a JSON list) |
| unsupported | `depends-and-unsupported.yaml:58` | template reference |  | fields: {{item.name}} reads a field of the loop item; a Nexus map hands the whole item to the task, which must read the field itself |
| translated | `depends-and-unsupported.yaml:58` | script | tasks[fields].runtime (kubernetes executor) | fields: template shard-work (image python:3.12-alpine) |
| approximated | `depends-and-unsupported.yaml:66` | withItems | map | fields: a loop over 2 literal items: a generated task emits them and fields maps over them (max_fanout 2) |
| translated | `depends-and-unsupported.yaml:75` | activeDeadlineSeconds | timeouts.start_to_close_ms | A: 600s per attempt |
| translated | `depends-and-unsupported.yaml:75` | activeDeadlineSeconds | timeouts.start_to_close_ms | B: 600s per attempt |
| translated | `depends-and-unsupported.yaml:75` | activeDeadlineSeconds | timeouts.start_to_close_ms | all-done: 600s per attempt |
| translated | `depends-and-unsupported.yaml:75` | activeDeadlineSeconds | timeouts.start_to_close_ms | either: 600s per attempt |
| translated | `depends-and-unsupported.yaml:75` | activeDeadlineSeconds | timeouts.start_to_close_ms | not-c: 600s per attempt |
| translated | `depends-and-unsupported.yaml:75` | activeDeadlineSeconds | timeouts.start_to_close_ms | gated: 600s per attempt |
| translated | `depends-and-unsupported.yaml:78` | retryStrategy.limit | retry.max_attempts=5 | C: limit 4 |
| approximated | `depends-and-unsupported.yaml:79` | retryPolicy | retry.retry_on | C: OnError -> retry on infrastructure, worker_lost and transient errors |
| approximated | `depends-and-unsupported.yaml:80` | backoff.maxDuration | timeouts.schedule_to_close_ms | C: maxDuration 5m stops retrying after that long; it becomes the task's total budget across attempts |
| translated | `depends-and-unsupported.yaml:80` | retryStrategy.backoff | retry | C: backoff exponential |
| translated | `depends-and-unsupported.yaml:82` | nodeAntiAffinity | retry.retry_on_different_worker | C: retry on a different node |
| approximated | `depends-and-unsupported.yaml:84` | memoize | cache | C: memoization keyed by Nexus on task_ref + version + input hash, not on the template's key expression |
| translated | `depends-and-unsupported.yaml:89` | synchronization.mutex | concurrency_key + concurrency_limit 1 | C: mutex |
| unsupported | `depends-and-unsupported.yaml:95` | env |  | C: env TOKEN comes from a Kubernetes Secret (secretKeyRef); Nexus SecretRefs are not resolved by workers yet, so provide it through the worker's environment |

### After import

```sh
nexusctl workflow submit depends-and-unsupported.yaml --dry-run --namespace team-a
```
