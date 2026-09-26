# nexusctl import airflow report

- Input: `testdata/upstream/airflow/example_task_group.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 16 translated, 0 approximated, 0 unsupported
- Result: specs not written

## example_task_group -> `example_task_group`

Source `testdata/upstream/airflow/example_task_group.py:29`. 9 task(s), 13 edge(s). 16 translated, 0 approximated, 0 unsupported. Content address `551c16fddf78`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `example_task_group.py:31` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `example_task_group.py:32` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 (UTC) |
| translated | `example_task_group.py:33` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `example_task_group.py:34` | tags | labels.tags | tags |
| translated | `example_task_group.py:36` | EmptyOperator | tasks[start] (runs `true`) | start: no-op |
| translated | `example_task_group.py:39` | TaskGroup | node ids section_1.* | task group section_1 flattened |
| translated | `example_task_group.py:40` | EmptyOperator | tasks[section_1.task_1] (runs `true`) | section_1.task_1: no-op |
| translated | `example_task_group.py:41` | BashOperator | tasks[section_1.task_2].runtime (bash -c, subprocess executor) | section_1.task_2: bash_command |
| translated | `example_task_group.py:42` | EmptyOperator | tasks[section_1.task_3] (runs `true`) | section_1.task_3: no-op |
| translated | `example_task_group.py:48` | TaskGroup | node ids section_2.* | task group section_2 flattened |
| translated | `example_task_group.py:49` | EmptyOperator | tasks[section_2.task_1] (runs `true`) | section_2.task_1: no-op |
| translated | `example_task_group.py:52` | TaskGroup | node ids section_2.inner_section_2.* | task group section_2.inner_section_2 flattened |
| translated | `example_task_group.py:53` | BashOperator | tasks[section_2.inner_section_2.task_2].runtime (bash -c, subprocess executor) | section_2.inner_section_2.task_2: bash_command |
| translated | `example_task_group.py:54` | EmptyOperator | tasks[section_2.inner_section_2.task_3] (runs `true`) | section_2.inner_section_2.task_3: no-op |
| translated | `example_task_group.py:55` | EmptyOperator | tasks[section_2.inner_section_2.task_4] (runs `true`) | section_2.inner_section_2.task_4: no-op |
| translated | `example_task_group.py:62` | EmptyOperator | tasks[end] (runs `true`) | end: no-op |

### After import

```sh
nexusctl workflow submit example_task_group.yaml --dry-run --namespace team-a
```
