# nexusctl import airflow report

- Input: `testdata/cases/airflow/runnable_bash.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 12 translated, 2 approximated, 0 unsupported
- Result: specs not written

## runnable_bash -> `runnable_bash`

Source `testdata/cases/airflow/runnable_bash.py:11`. 4 task(s), 3 edge(s). 12 translated, 2 approximated, 0 unsupported. Content address `d6d152da8775`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `runnable_bash.py:13` | schedule | companion: nexusctl schedule create runnable_bash | schedule "30 2 * * *" in UTC |
| translated | `runnable_bash.py:14` | start_date | schedule timezone / backfill --from | start date 2026-01-01T00:00:00 (UTC) |
| translated | `runnable_bash.py:15` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `runnable_bash.py:16` | param | params.greeting (string) | param greeting |
| translated | `runnable_bash.py:17` | retries | retry.max_attempts=2 | default_args: retries=1 |
| translated | `runnable_bash.py:17` | retry_delay | retry.initial_ms | default_args: retry_delay 1s |
| translated | `runnable_bash.py:19` | EmptyOperator | tasks[start] (runs `true`) | start: no-op |
| translated | `runnable_bash.py:20` | TaskGroup | node ids prepare.* | task group prepare flattened |
| approximated | `runnable_bash.py:21` | Jinja params | input_params.greeting | prepare.make: {{ params.greeting }} reads workflow param greeting at run time from $NEXUS_INPUT_DIR (a JSON string's quotes are stripped, escapes are not decoded; inside single quotes it will not expand) |
| translated | `runnable_bash.py:21` | Jinja | literal | prepare.make: {{ task.task_id }} |
| translated | `runnable_bash.py:21` | BashOperator | tasks[prepare.make].runtime (bash -c, subprocess executor) | prepare.make: bash_command |
| approximated | `runnable_bash.py:22` | Jinja params | input_params.greeting | prepare.check: {{ params.greeting }} reads workflow param greeting at run time from $NEXUS_INPUT_DIR (a JSON string's quotes are stripped, escapes are not decoded; inside single quotes it will not expand) |
| translated | `runnable_bash.py:22` | BashOperator | tasks[prepare.check].runtime (bash -c, subprocess executor) | prepare.check: bash_command |
| translated | `runnable_bash.py:24` | BashOperator | tasks[finish].runtime (bash -c, subprocess executor) | finish: bash_command |

### After import

```sh
nexusctl workflow submit runnable_bash.yaml --dry-run --namespace team-a
nexusctl schedule create runnable_bash --workflow runnable_bash --cron '30 2 * * *' --timezone UTC --overlap allow --namespace team-a
```
