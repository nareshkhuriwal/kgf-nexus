# nexusctl import airflow report

- Input: `testdata/upstream/airflow/example_bash_operator.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 10 translated, 2 approximated, 1 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## example_bash_operator -> `example_bash_operator`

Source `testdata/upstream/airflow/example_bash_operator.py:30`. 5 task(s), 4 edge(s). 10 translated, 2 approximated, 1 unsupported. Content address `30f228cc96a8`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `example_bash_operator.py:32` | schedule | companion: nexusctl schedule create example_bash_operator | schedule "0 0 * * *" in UTC |
| translated | `example_bash_operator.py:33` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 (UTC) |
| translated | `example_bash_operator.py:34` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `example_bash_operator.py:35` | dagrun_timeout | run_timeout_ms | dagrun_timeout 3600s |
| translated | `example_bash_operator.py:36` | tags | labels.tags | tags |
| translated | `example_bash_operator.py:37` | param | params.example_key (string) | param example_key |
| translated | `example_bash_operator.py:54` | EmptyOperator | tasks[run_this_last] (runs `true`) | run_this_last: no-op |
| translated | `example_bash_operator.py:59` | BashOperator | tasks[run_after_loop].runtime (bash -c, subprocess executor) | run_after_loop: bash_command |
| unsupported | `example_bash_operator.py:67` | for | placeholders: runme_N | a for loop creates or wires tasks (runme_N); the converter does not unroll loops or evaluate conditions, so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map (dynamic expansion) for a run-time collection |
| translated | `example_bash_operator.py:75` | BashOperator | tasks[also_run_this].runtime (bash -c, subprocess executor) | also_run_this: bash_command |
| approximated | `example_bash_operator.py:77` | Jinja | $NEXUS_RUN_ID | also_run_this: {{ task_instance_key_str }} (dag__task__date) -> dag__task__$NEXUS_RUN_ID |
| approximated | `example_bash_operator.py:83` | skip_on_exit_code |  | this_will_skip: Airflow marks the task SKIPPED on exit code 99 (skip_on_exit_code); Nexus has no skipped state, so that exit code fails the task |
| translated | `example_bash_operator.py:83` | BashOperator | tasks[this_will_skip].runtime (bash -c, subprocess executor) | this_will_skip: bash_command |

### After import

```sh
nexusctl workflow submit example_bash_operator.yaml --dry-run --namespace team-a
nexusctl schedule create example_bash_operator --workflow example_bash_operator --cron '0 0 * * *' --timezone UTC --overlap allow --namespace team-a
```
