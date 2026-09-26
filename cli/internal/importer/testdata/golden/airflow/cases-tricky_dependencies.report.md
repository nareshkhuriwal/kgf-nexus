# nexusctl import airflow report

- Input: `testdata/cases/airflow/tricky_dependencies.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 33 translated, 8 approximated, 5 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## Tricky_Dependencies -> `tricky_dependencies`

Source `testdata/cases/airflow/tricky_dependencies.py:22`. 11 task(s), 16 edge(s). 33 translated, 8 approximated, 5 unsupported. Content address `c23d2e7d78d6`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `tricky_dependencies.py:22` | name | name | Tricky_Dependencies is named tricky_dependencies (the schema allows lowercase letters, digits, '-', '_' and '.') |
| approximated | `tricky_dependencies.py:24` | overlap | schedule --overlap queue_one | max_active_runs=1 queues later runs in Airflow; overlap queue_one keeps at most one waiting |
| translated | `tricky_dependencies.py:24` | schedule | companion: nexusctl schedule create tricky_dependencies | schedule "0 0 * * *" in Europe/Amsterdam (@daily is "0 0 * * *") |
| translated | `tricky_dependencies.py:25` | start_date | schedule timezone / backfill --from | start date 2026-01-01T00:00:00 (Europe/Amsterdam) |
| approximated | `tricky_dependencies.py:26` | catchup | backfill start | catchup=True: Airflow creates a run for every interval since start_date; a Nexus schedule does not, so a backfill command is suggested instead |
| translated | `tricky_dependencies.py:27` | max_active_runs | max_active_runs | max_active_runs=1 |
| approximated | `tricky_dependencies.py:28` | retry_exponential_backoff | retry.strategy=exponential | default_args: exponential backoff with multiplier 2 and full jitter; Airflow's formula differs in detail |
| translated | `tricky_dependencies.py:28` | retries | retry.max_attempts=3 | default_args: retries=2 |
| translated | `tricky_dependencies.py:28` | retry_delay | retry.initial_ms | default_args: retry_delay 30s |
| translated | `tricky_dependencies.py:28` | max_retry_delay | retry.max_ms | default_args: max_retry_delay 600s |
| translated | `tricky_dependencies.py:28` | execution_timeout | timeouts.start_to_close_ms | default_args: execution_timeout 900s |
| approximated | `tricky_dependencies.py:29` | Param |  | param greeting: its JSON-schema constraints (minLength) are not enforced by Nexus params |
| translated | `tricky_dependencies.py:29` | param | params.greeting (string) | param greeting |
| translated | `tricky_dependencies.py:29` | param | params.limit (int) | param limit |
| translated | `tricky_dependencies.py:30` | tags | labels.tags | tags |
| translated | `tricky_dependencies.py:32` | EmptyOperator | tasks[a] (runs `true`) | a: no-op |
| translated | `tricky_dependencies.py:33` | EmptyOperator | tasks[b] (runs `true`) | b: no-op |
| translated | `tricky_dependencies.py:34` | EmptyOperator | tasks[c] (runs `true`) | c: no-op |
| translated | `tricky_dependencies.py:35` | EmptyOperator | tasks[d] (runs `true`) | d: no-op |
| translated | `tricky_dependencies.py:36` | EmptyOperator | tasks[e] (runs `true`) | e: no-op |
| translated | `tricky_dependencies.py:37` | EmptyOperator | tasks[f] (runs `true`) | f: no-op |
| unsupported | `tricky_dependencies.py:40` | >> |  | a list on both sides of >> (or <<) is rejected by Airflow itself (TypeError); use cross_downstream() for all pairs or chain() for pairwise dependencies |
| translated | `tricky_dependencies.py:43` | chain | edges | chain |
| unsupported | `tricky_dependencies.py:46` | chain |  | chain() between lists of different lengths (2 and 3) is rejected by Airflow |
| translated | `tricky_dependencies.py:46` | chain | edges | chain |
| translated | `tricky_dependencies.py:49` | cross_downstream | edges | cross_downstream |
| unsupported | `tricky_dependencies.py:56` | pool |  | greet: Airflow pool 'db_pool': its slot count lives in the Airflow database, not the DAG file. The Nexus equivalent is a global semaphore, concurrency_key + concurrency_limit; set both by hand |
| approximated | `tricky_dependencies.py:56` | priority_weight | priority | greet: priority_weight 5 -> priority 104 (Nexus default 100; weight_rule is not applied) |
| approximated | `tricky_dependencies.py:56` | queue | tasks[greet].queue | greet: queue High-Memory is high-memory in Nexus (queue name rules) |
| translated | `tricky_dependencies.py:56` | BashOperator | tasks[greet].runtime (bash -c, subprocess executor) | greet: bash_command |
| translated | `tricky_dependencies.py:56` | queue | queue | greet: queue High-Memory (a worker must subscribe to it) |
| translated | `tricky_dependencies.py:56` | max_active_tis_per_dag | concurrency_key + concurrency_limit | greet: at most 2 concurrent instances across runs |
| translated | `tricky_dependencies.py:56` | trigger_rule | trigger_rule | greet: trigger_rule all_done |
| approximated | `tricky_dependencies.py:58` | Jinja params | input_params.greeting | greet: {{ params.greeting }} reads workflow param greeting at run time from $NEXUS_INPUT_DIR (a JSON string's quotes are stripped, escapes are not decoded; inside single quotes it will not expand) |
| translated | `tricky_dependencies.py:58` | Jinja | literal Tricky_Dependencies | greet: {{ dag.dag_id }} |
| translated | `tricky_dependencies.py:58` | Jinja | literal | greet: {{ task.task_id }} |
| unsupported | `tricky_dependencies.py:65` | trigger_rule |  | report: trigger_rule one_success is in the Nexus schema, but the control plane accepts only all_success and all_done in this release; the task keeps all_success |
| translated | `tricky_dependencies.py:65` | BashOperator | tasks[report].runtime (bash -c, subprocess executor) | report: bash_command |
| translated | `tricky_dependencies.py:65` | retries | retry.max_attempts=1 | report: retries=0 |
| unsupported | `tricky_dependencies.py:67` | Jinja |  | report: {{ ds }} is the run's LOGICAL date, which Nexus does not have: a scheduled run fires at the cron time and a backfill passes no per-tick date. Add a workflow param and pass it (`nexusctl run create --param ...`), or compute the date in the task |
| approximated | `tricky_dependencies.py:67` | Jinja | $NEXUS_RUN_ID | report: {{ run_id }} -> $NEXUS_RUN_ID (a UUID, not Airflow's run id) |
| translated | `tricky_dependencies.py:72` | TaskGroup | node ids outer.* | task group outer flattened |
| translated | `tricky_dependencies.py:73` | EmptyOperator | tasks[outer.first] (runs `true`) | outer.first: no-op |
| translated | `tricky_dependencies.py:74` | TaskGroup | node ids outer.inner.* | task group outer.inner flattened |
| translated | `tricky_dependencies.py:75` | BashOperator | tasks[outer.inner.work].runtime (bash -c, subprocess executor) | outer.inner.work: bash_command |
| translated | `tricky_dependencies.py:76` | EmptyOperator | tasks[outer.inner.done] (runs `true`) | outer.inner.done: no-op |

### After import

```sh
nexusctl workflow submit tricky_dependencies.yaml --dry-run --namespace team-a
nexusctl schedule create tricky_dependencies --workflow tricky_dependencies --cron '0 0 * * *' --timezone Europe/Amsterdam --overlap queue_one --namespace team-a
nexusctl backfill start tricky_dependencies --from 2026-01-01 --to "$(date -u +%Y-%m-%d)" --namespace team-a
```
