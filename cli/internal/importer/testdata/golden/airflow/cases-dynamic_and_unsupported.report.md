# nexusctl import airflow report

- Input: `testdata/cases/airflow/dynamic_and_unsupported.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 10 translated, 10 approximated, 7 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## Outside any workflow

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| unsupported | `dynamic_and_unsupported.py:14` | Variable.get |  | Airflow Variables are read from the metadata database; Nexus has no variable store. Pass the value as a workflow param, or read it inside the task |

## dynamic_and_unsupported -> `dynamic_and_unsupported`

Source `testdata/cases/airflow/dynamic_and_unsupported.py:21`. 13 task(s), 11 edge(s). 10 translated, 10 approximated, 6 unsupported. Content address `ce619253ca5d`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `dynamic_and_unsupported.py:21` | independent task | tasks[nexus-import-root] | pair have no dependencies while the rest of the graph is connected; the gateway rejects that as a forgotten edge, so they hang off a no-op root task nexus-import-root |
| approximated | `dynamic_and_unsupported.py:21` | schedule | companion: nexusctl schedule create dynamic_and_unsupported | schedule "0 */6 * * *" in UTC (an interval of 6h became cron "0 */6 * * *", which fires on wall-clock boundaries rather than every interval from the start date) |
| translated | `dynamic_and_unsupported.py:21` | start_date | schedule timezone / backfill --from | start date 2026-01-01T00:00:00 |
| translated | `dynamic_and_unsupported.py:21` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `dynamic_and_unsupported.py:21` | dagrun_timeout | run_timeout_ms | dagrun_timeout 7200s |
| translated | `dynamic_and_unsupported.py:22` | param | params.source (string) | param source |
| translated | `dynamic_and_unsupported.py:22` | param | params.batch (int) | param batch |
| translated | `dynamic_and_unsupported.py:25` | retries | retry.max_attempts=4 | list_files: retries=3 |
| approximated | `dynamic_and_unsupported.py:26` | python task | tasks[list_files].runtime | list_files: runs dynamic_and_unsupported.list_files through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="dynamic_and_unsupported.list_files") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `dynamic_and_unsupported.py:30` | python task | tasks[process].runtime | process: runs dynamic_and_unsupported.process through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="dynamic_and_unsupported.process") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `dynamic_and_unsupported.py:34` | python task | tasks[total].runtime | total: runs dynamic_and_unsupported.total through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="dynamic_and_unsupported.total") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `dynamic_and_unsupported.py:38` | python task | tasks[square].runtime | square: runs dynamic_and_unsupported.square through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="dynamic_and_unsupported.square") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `dynamic_and_unsupported.py:42` | python task | tasks[pair].runtime | pair: runs dynamic_and_unsupported.pair through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="dynamic_and_unsupported.pair") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `dynamic_and_unsupported.py:46` | max_fanout | tasks[process].map.max_fanout | process: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `dynamic_and_unsupported.py:46` | expand | map.over_node_id=list_files | process: map over the output of list_files |
| translated | `dynamic_and_unsupported.py:47` | data dependency | edge process -> total {counts: result} | total(counts=process) |
| approximated | `dynamic_and_unsupported.py:48` | expand | map | square: mapping over a literal list of 3: a generated task emits the list and square maps over it (max_fanout 3) |
| unsupported | `dynamic_and_unsupported.py:49` | expand |  | pair: expand() over 2 arguments maps over their cross product; a Nexus map expands one input |
| unsupported | `dynamic_and_unsupported.py:51` | FileSensor | tasks[wait_for_file] (placeholder) | wait_for_file: FileSensor (mode=reschedule, poke_interval=60s) waits for an external condition; Nexus has no sensor primitive. Start the workflow from the system that produces the event (`nexusctl run create`), or poll inside a task with a retry policy |
| unsupported | `dynamic_and_unsupported.py:52` | BranchPythonOperator | tasks[branch] (placeholder) | branch: BranchPythonOperator chooses downstream branches at run time; Nexus DAGs have no branching or skipping. Split the workflow, or move the decision into the downstream tasks |
| approximated | `dynamic_and_unsupported.py:58` | pod settings |  | train: namespace are managed by the Nexus kubernetes executor; ignored |
| translated | `dynamic_and_unsupported.py:58` | KubernetesPodOperator | tasks[train].runtime (kubernetes executor) | train: image, cmds, arguments |
| translated | `dynamic_and_unsupported.py:58` | resources | tasks[train].resources | train: resource requests/limits |
| unsupported | `dynamic_and_unsupported.py:71` | for | placeholders: shard_N | a for loop creates or wires tasks (shard_N); the converter does not unroll loops or evaluate conditions, so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map (dynamic expansion) for a run-time collection |
| unsupported | `dynamic_and_unsupported.py:75` | task | tasks[echo_each] (placeholder) | echo_each: mapping a classic operator (BashOperator.partial().expand()) is not translated; rewrite it as a TaskFlow task, which maps to a Nexus map |
| unsupported | `dynamic_and_unsupported.py:77` | task | tasks[pick] (placeholder) | pick: @task.branch chooses downstream branches at run time; Nexus DAGs have no branching or skipping |

### After import

```sh
nexusctl workflow submit dynamic_and_unsupported.yaml --dry-run --namespace team-a
nexusctl schedule create dynamic_and_unsupported --workflow dynamic_and_unsupported --cron '0 */6 * * *' --timezone UTC --overlap allow --namespace team-a
```
