# nexusctl import airflow report

- Input: `testdata/upstream/airflow/example_simplest_dag.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 1 translated, 2 approximated, 0 unsupported
- Result: specs not written

## example_simplest_dag -> `example_simplest_dag`

Source `testdata/upstream/airflow/example_simplest_dag.py:25`. 1 task(s), 0 edge(s). 1 translated, 2 approximated, 0 unsupported. Content address `74e8c48e1e0e`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `example_simplest_dag.py:25` | schedule |  | no schedule argument: Airflow 2 defaults to daily, Airflow 3 to none; no schedule was suggested |
| translated | `example_simplest_dag.py:25` | tags | labels.tags | tags |
| approximated | `example_simplest_dag.py:28` | python task | tasks[my_task].runtime | my_task: runs example_simplest_dag.my_task through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="example_simplest_dag.my_task") -- inputs arrive as keyword arguments and the return value is output `result` |

### After import

```sh
nexusctl workflow submit example_simplest_dag.yaml --dry-run --namespace team-a
```
