# nexusctl import airflow report

- Input: `testdata/upstream/airflow/tutorial_taskflow_api.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 6 translated, 5 approximated, 0 unsupported
- Result: specs not written

## tutorial_taskflow_api -> `tutorial_taskflow_api`

Source `testdata/upstream/airflow/tutorial_taskflow_api.py:32`. 3 task(s), 2 edge(s). 6 translated, 5 approximated, 0 unsupported. Content address `a87d7be5cc32`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `tutorial_taskflow_api.py:33` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `tutorial_taskflow_api.py:34` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 (UTC) |
| translated | `tutorial_taskflow_api.py:35` | catchup | schedule | catchup=False: only new ticks run, as with a Nexus schedule |
| translated | `tutorial_taskflow_api.py:36` | tags | labels.tags | tags |
| approximated | `tutorial_taskflow_api.py:51` | python task | tasks[extract].runtime | extract: runs tutorial_taskflow_api.extract through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="tutorial_taskflow_api.extract") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `tutorial_taskflow_api.py:66` | multiple_outputs | outputs | transform: multiple_outputs: each key read downstream is a separate Nexus output |
| approximated | `tutorial_taskflow_api.py:67` | python task | tasks[transform].runtime | transform: runs tutorial_taskflow_api.transform through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="tutorial_taskflow_api.transform") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `tutorial_taskflow_api.py:84` | python task | tasks[load].runtime | load: runs tutorial_taskflow_api.load through `python -m nexus.entrypoint`; it is defined inside the DAG/flow function, so move it to module level where it is importable; decorate it with @nexus.task(task_ref="tutorial_taskflow_api.load") -- inputs arrive as keyword arguments and the return value is output `result` |
| translated | `tutorial_taskflow_api.py:97` | data dependency | edge extract -> transform {order_data_dict: result} | transform(order_data_dict=extract) |
| approximated | `tutorial_taskflow_api.py:98` | output key | outputs.total_order_value | load: transform['total_order_value'] is declared as a separate output total_order_value of transform; a Nexus handler with several outputs returns a mapping holding each of them |
| translated | `tutorial_taskflow_api.py:98` | data dependency | edge transform -> load {total_order_value: total_order_value} | load(total_order_value=transform) |

### After import

```sh
nexusctl workflow submit tutorial_taskflow_api.yaml --dry-run --namespace team-a
```
