# nexusctl import airflow report

- Input: `testdata/upstream/airflow/tutorial.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 10 translated, 1 approximated, 3 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## tutorial -> `tutorial`

Source `testdata/upstream/airflow/tutorial.py:41`. 3 task(s), 2 edge(s). 10 translated, 1 approximated, 3 unsupported. Content address `076c1a5c12ab`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `tutorial.py:46` | retries | retry.max_attempts=2 | default_args: retries=1 |
| translated | `tutorial.py:46` | retry_delay | retry.initial_ms | default_args: retry_delay 300s |
| translated | `tutorial.py:64` | description | description | description |
| approximated | `tutorial.py:65` | schedule | companion: nexusctl schedule create tutorial | schedule "0 0 * * *" in UTC (an interval of 1d became cron "0 0 * * *", which fires on wall-clock boundaries rather than every interval from the start date) |
| translated | `tutorial.py:66` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 |
| translated | `tutorial.py:67` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `tutorial.py:68` | tags | labels.tags | tags |
| translated | `tutorial.py:74` | BashOperator | tasks[print_date].runtime (bash -c, subprocess executor) | print_date: bash_command |
| translated | `tutorial.py:79` | BashOperator | tasks[sleep].runtime (bash -c, subprocess executor) | sleep: bash_command |
| translated | `tutorial.py:79` | retries | retry.max_attempts=4 | sleep: retries=3 |
| translated | `tutorial.py:115` | BashOperator | tasks[templated].runtime (bash -c, subprocess executor) | templated: bash_command |
| unsupported | `tutorial.py:118` | Jinja |  | templated: Jinja statements ({% ... %}) are rendered by Airflow before the command runs; Nexus runs the command verbatim. Rewrite them as shell |
| unsupported | `tutorial.py:118` | Jinja |  | templated: {{ ds }} is the run's LOGICAL date, which Nexus does not have: a scheduled run fires at the cron time and a backfill passes no per-tick date. Add a workflow param and pass it (`nexusctl run create --param ...`), or compute the date in the task |
| unsupported | `tutorial.py:118` | Jinja |  | templated: {{ macros.ds_add(ds, 7) }} is the run's LOGICAL date, which Nexus does not have: a scheduled run fires at the cron time and a backfill passes no per-tick date. Add a workflow param and pass it (`nexusctl run create --param ...`), or compute the date in the task |

### After import

```sh
nexusctl workflow submit tutorial.yaml --dry-run --namespace team-a
nexusctl schedule create tutorial --workflow tutorial --cron '0 0 * * *' --timezone UTC --overlap allow --namespace team-a
```
