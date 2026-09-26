# nexusctl import airflow report

- Input: `testdata/upstream/airflow/example_xcomargs.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 2
- Constructs: 11 translated, 7 approximated, 0 unsupported
- Result: specs not written

## example_xcom_args -> `example_xcom_args`

Source `testdata/upstream/airflow/example_xcomargs.py:45`. 2 task(s), 1 edge(s). 5 translated, 3 approximated, 0 unsupported. Content address `0c6764127b36`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `example_xcomargs.py:34` | python task | tasks[generate_value].runtime | generate_value: runs example_xcomargs.generate_value through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_xcomargs.generate_value") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_xcomargs.py:40` | python task | tasks[print_value].runtime | print_value: runs example_xcomargs.print_value through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_xcomargs.print_value") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_xcomargs.py:40` | task context |  | print_value: Airflow injects the task context (ts); Nexus does not -- use the NEXUS_* environment or nexus.TaskContext |
| translated | `example_xcomargs.py:47` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 (UTC) |
| translated | `example_xcomargs.py:48` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `example_xcomargs.py:49` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `example_xcomargs.py:50` | tags | labels.tags | tags |
| translated | `example_xcomargs.py:52` | data dependency | edge generate_value -> print_value {value: result} | print_value(value=generate_value) |

### After import

```sh
nexusctl workflow submit example_xcom_args.yaml --dry-run --namespace team-a
```

## example_xcom_args_with_operators -> `example_xcom_args_with_operators`

Source `testdata/upstream/airflow/example_xcomargs.py:54`. 4 task(s), 3 edge(s). 6 translated, 4 approximated, 0 unsupported. Content address `65c78553299b`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `example_xcomargs.py:40` | python task | tasks[print_value].runtime | print_value: runs example_xcomargs.print_value through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_xcomargs.print_value") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_xcomargs.py:40` | task context |  | print_value: Airflow injects the task context (ts); Nexus does not -- use the NEXUS_* environment or nexus.TaskContext |
| approximated | `example_xcomargs.py:40` | python task | tasks[print_value__1].runtime | print_value__1: runs example_xcomargs.print_value through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_xcomargs.print_value") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_xcomargs.py:40` | task context |  | print_value__1: Airflow injects the task context (ts); Nexus does not -- use the NEXUS_* environment or nexus.TaskContext |
| translated | `example_xcomargs.py:56` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 (UTC) |
| translated | `example_xcomargs.py:57` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `example_xcomargs.py:58` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `example_xcomargs.py:59` | tags | labels.tags | tags |
| translated | `example_xcomargs.py:61` | BashOperator | tasks[c].runtime (bash -c, subprocess executor) | c: bash_command |
| translated | `example_xcomargs.py:62` | BashOperator | tasks[d].runtime (bash -c, subprocess executor) | d: bash_command |

### After import

```sh
nexusctl workflow submit example_xcom_args_with_operators.yaml --dry-run --namespace team-a
```
