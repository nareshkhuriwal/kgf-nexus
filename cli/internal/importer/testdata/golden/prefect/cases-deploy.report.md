# nexusctl import prefect report

- Input: `testdata/cases/prefect/deploy` (2 file(s))
- Namespace: `team-a`
- Workflows: 2
- Constructs: 21 translated, 16 approximated, 4 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## Outside any workflow

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `prefect.yaml:16` | prefect.yaml |  | deployment missing: flow nope in other.py was not converted (not in the input) |

## Nightly ETL -> `nightly-etl`

Source `testdata/cases/prefect/deploy/etl_flows.py:36`. 8 task(s), 6 edge(s). 19 translated, 11 approximated, 4 unsupported. Content address `57bccb13a83c`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `etl_flows.py:10` | retry_delay_seconds | retry | list_shards: the delay list [1, 10, 100] becomes exponential backoff from 1s x10, capped at 100s |
| translated | `etl_flows.py:10` | retries | retry.max_attempts=4 | list_shards: retries=3 |
| translated | `etl_flows.py:10` | timeout_seconds | timeouts.start_to_close_ms | list_shards: 300s |
| approximated | `etl_flows.py:11` | python task | tasks[list_shards].runtime | list_shards: runs etl_flows.list_shards through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="etl_flows.list_shards") -- inputs arrive as keyword arguments and the return value is output `result` |
| translated | `etl_flows.py:15` | retries | retry.max_attempts=3 | process: retries=2 |
| translated | `etl_flows.py:15` | exponential_backoff | retry.strategy=exponential | process: exponential_backoff(5) |
| translated | `etl_flows.py:15` | retries | retry.max_attempts=3 | process__1: retries=2 |
| translated | `etl_flows.py:15` | exponential_backoff | retry.strategy=exponential | process__1: exponential_backoff(5) |
| approximated | `etl_flows.py:16` | tags |  | process: task tags (and Prefect's tag-based concurrency limits) are dropped; use concurrency_key/concurrency_limit for a limit |
| approximated | `etl_flows.py:16` | tags |  | process__1: task tags (and Prefect's tag-based concurrency limits) are dropped; use concurrency_key/concurrency_limit for a limit |
| translated | `etl_flows.py:16` | cache_key_fn | cache | process: task_input_hash; Nexus memoizes on task_ref + version + input hash |
| translated | `etl_flows.py:16` | cache_expiration | cache.ttl_ms | process: 3600s |
| translated | `etl_flows.py:16` | cache_key_fn | cache | process__1: task_input_hash; Nexus memoizes on task_ref + version + input hash |
| translated | `etl_flows.py:16` | cache_expiration | cache.ttl_ms | process__1: 3600s |
| approximated | `etl_flows.py:17` | python task | tasks[process].runtime | process: runs etl_flows.process through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="etl_flows.process") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `etl_flows.py:17` | python task | tasks[process__1].runtime | process__1: runs etl_flows.process through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="etl_flows.process") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `etl_flows.py:22` | python task | tasks[warm_up].runtime | warm_up: runs etl_flows.warm_up through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="etl_flows.warm_up") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `etl_flows.py:27` | python task | tasks[combine].runtime | combine: runs etl_flows.combine through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="etl_flows.combine") -- inputs arrive as keyword arguments and the return value is output `result` |
| unsupported | `etl_flows.py:36` | retries |  | flow-level retries re-run the whole flow; Nexus retries tasks, not runs (retry a failed run with `nexusctl run retry`) |
| approximated | `etl_flows.py:36` | @flow | fail_fast | a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks already running finish) |
| translated | `etl_flows.py:36` | timeout_seconds | run_timeout_ms | flow timeout 3600s |
| translated | `etl_flows.py:36` | name | name | Nightly ETL is named nightly-etl (the schema allows lowercase letters, digits, '-', '_' and '.') |
| translated | `etl_flows.py:37` | param | params.source (string) | param source |
| translated | `etl_flows.py:37` | param | params.mode (string) | param mode |
| translated | `etl_flows.py:37` | param | params.limit (int) | param limit |
| approximated | `etl_flows.py:40` | max_fanout | tasks[process].map.max_fanout | process: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `etl_flows.py:40` | wait_for | edges (ordering only) | process: wait_for |
| translated | `etl_flows.py:40` | .map | map.over_node_id=list_shards | process: map over the output of list_shards |
| translated | `etl_flows.py:41` | data dependency | edge process -> combine {counts: result} | combine(counts=process) |
| approximated | `etl_flows.py:42` | .map | map | process__1: mapping over a literal list of 2: a generated task emits the list and process__1 maps over it (max_fanout 2) |
| unsupported | `etl_flows.py:43` | if |  | branches on a task result at run time; Nexus DAGs are static (no conditional tasks). Move the decision into a task, or into the downstream task's code |
| unsupported | `etl_flows.py:43` | if | placeholders: notify | a condition creates or wires tasks (notify); the converter does not unroll loops or evaluate conditions, so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map (dynamic expansion) for a run-time collection |
| unsupported | `etl_flows.py:45` | task | tasks[report] (placeholder) | report: calls flow report as a subflow; Nexus has no nested workflows. The subflow is converted as a workflow of its own: inline its tasks here, or start it from a task with `nexusctl run create` |
| translated | `etl_flows.py:54` | nightly_etl.serve() | companion: nexusctl schedule create nightly-etl | schedule "0 3 * * *" in UTC |

### After import

```sh
nexusctl workflow submit nightly-etl.yaml --dry-run --namespace team-a
nexusctl schedule create nightly-etl --workflow nightly-etl --cron '0 3 * * *' --timezone UTC --param 'mode="incremental"' --namespace team-a
```

## report -> `report`

Source `testdata/cases/prefect/deploy/etl_flows.py:48`. 1 task(s), 0 edge(s). 2 translated, 4 approximated, 0 unsupported. Content address `f7f8c5d429d9`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `etl_flows.py:32` | python task | tasks[notify].runtime | notify: runs etl_flows.notify through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="etl_flows.notify") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `etl_flows.py:48` | @flow | fail_fast | a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks already running finish) |
| translated | `etl_flows.py:49` | param | params.total (int) | param total |
| approximated | `prefect.yaml:10` | work_pool |  | deployment report-hourly: work_pool (Prefect infrastructure) is not part of a Nexus spec; ignored |
| translated | `prefect.yaml:12` | prefect.yaml deployment report-hourly | companion: nexusctl schedule create report | schedule "15 * * * *" in America/New_York |
| approximated | `prefect.yaml:14` | prefect.yaml deployment report-hourly | companion: nexusctl schedule create report-2 | schedule "*/30 * * * *" in UTC (an interval of 30m became cron "*/30 * * * *", which fires on wall-clock boundaries rather than every interval from the start date) |

### After import

```sh
nexusctl workflow submit report.yaml --dry-run --namespace team-a
nexusctl schedule create report --workflow report --cron '15 * * * *' --timezone America/New_York --param total=0 --namespace team-a
nexusctl schedule create report-2 --workflow report --cron '*/30 * * * *' --timezone UTC --disabled --param total=0 --namespace team-a
```
