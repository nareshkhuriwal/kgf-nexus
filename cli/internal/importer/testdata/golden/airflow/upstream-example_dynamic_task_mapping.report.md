# nexusctl import airflow report

- Input: `testdata/upstream/airflow/example_dynamic_task_mapping.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 3
- Constructs: 14 translated, 8 approximated, 1 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## example_dynamic_task_mapping -> `example_dynamic_task_mapping`

Source `testdata/upstream/airflow/example_dynamic_task_mapping.py:27`. 3 task(s), 2 edge(s). 4 translated, 3 approximated, 0 unsupported. Content address `0460e4f812f0`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `example_dynamic_task_mapping.py:28` | start_date | schedule timezone / backfill --from | start date 2022-03-04T00:00:00 |
| translated | `example_dynamic_task_mapping.py:28` | tags | labels.tags | tags |
| translated | `example_dynamic_task_mapping.py:28` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| approximated | `example_dynamic_task_mapping.py:32` | python task | tasks[add_one].runtime | add_one: runs example_dynamic_task_mapping.add_one through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_dynamic_task_mapping.add_one") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_dynamic_task_mapping.py:36` | python task | tasks[sum_it].runtime | sum_it: runs example_dynamic_task_mapping.sum_it through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_dynamic_task_mapping.sum_it") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_dynamic_task_mapping.py:40` | expand | map | add_one: mapping over a literal list of 3: a generated task emits the list and add_one maps over it (max_fanout 3) |
| translated | `example_dynamic_task_mapping.py:41` | data dependency | edge add_one -> sum_it {values: result} | sum_it(values=add_one) |

### After import

```sh
nexusctl workflow submit example_dynamic_task_mapping.yaml --dry-run --namespace team-a
```

## example_task_mapping_second_order -> `example_task_mapping_second_order`

Source `testdata/upstream/airflow/example_dynamic_task_mapping.py:43`. 3 task(s), 2 edge(s). 6 translated, 5 approximated, 0 unsupported. Content address `cfb9832bf525`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `example_dynamic_task_mapping.py:45` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `example_dynamic_task_mapping.py:46` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `example_dynamic_task_mapping.py:47` | start_date | schedule timezone / backfill --from | start date 2022-03-04T00:00:00 |
| translated | `example_dynamic_task_mapping.py:48` | tags | labels.tags | tags |
| approximated | `example_dynamic_task_mapping.py:52` | python task | tasks[get_nums].runtime | get_nums: runs example_dynamic_task_mapping.get_nums through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_dynamic_task_mapping.get_nums") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_dynamic_task_mapping.py:56` | python task | tasks[times_2].runtime | times_2: runs example_dynamic_task_mapping.times_2 through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_dynamic_task_mapping.times_2") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_dynamic_task_mapping.py:60` | python task | tasks[add_10].runtime | add_10: runs example_dynamic_task_mapping.add_10 through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="example_dynamic_task_mapping.add_10") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `example_dynamic_task_mapping.py:64` | max_fanout | tasks[times_2].map.max_fanout | times_2: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `example_dynamic_task_mapping.py:64` | expand | map.over_node_id=get_nums | times_2: map over the output of get_nums |
| approximated | `example_dynamic_task_mapping.py:65` | max_fanout | tasks[add_10].map.max_fanout | add_10: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `example_dynamic_task_mapping.py:65` | expand | map.over_node_id=times_2 | add_10: map over the output of times_2 |

### After import

```sh
nexusctl workflow submit example_task_mapping_second_order.yaml --dry-run --namespace team-a
```

## example_task_group_mapping -> `example_task_group_mapping`

Source `testdata/upstream/airflow/example_dynamic_task_mapping.py:67`. 1 task(s), 0 edge(s). 4 translated, 0 approximated, 1 unsupported. Content address `db3db1d11ea0`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `example_dynamic_task_mapping.py:69` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `example_dynamic_task_mapping.py:70` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `example_dynamic_task_mapping.py:71` | start_date | schedule timezone / backfill --from | start date 2022-03-04T00:00:00 |
| translated | `example_dynamic_task_mapping.py:72` | tags | labels.tags | tags |
| unsupported | `example_dynamic_task_mapping.py:87` | task | tasks[op] (placeholder) | op: a mapped task group (op.expand) expands a whole subgraph per element; a Nexus map expands a single task |

### After import

```sh
nexusctl workflow submit example_task_group_mapping.yaml --dry-run --namespace team-a
```
