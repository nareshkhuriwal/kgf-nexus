# nexusctl import prefect report

- Input: `testdata/upstream/prefect/per_worker_task_concurrency.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 3 translated, 5 approximated, 0 unsupported
- Result: specs not written

## process_image -> `process_image`

Source `testdata/upstream/prefect/per_worker_task_concurrency.py:104`. 3 task(s), 2 edge(s). 3 translated, 5 approximated, 0 unsupported. Content address `a50b473e401c`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `per_worker_task_concurrency.py:58` | python task | tasks[download_image].runtime | download_image: runs per_worker_task_concurrency.download_image through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="per_worker_task_concurrency.download_image") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `per_worker_task_concurrency.py:67` | python task | tasks[save_results].runtime | save_results: runs per_worker_task_concurrency.save_results through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="per_worker_task_concurrency.save_results") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `per_worker_task_concurrency.py:82` | python task | tasks[run_ml_model].runtime | run_ml_model: runs per_worker_task_concurrency.run_ml_model through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="per_worker_task_concurrency.run_ml_model") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `per_worker_task_concurrency.py:104` | @flow | fail_fast | a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks already running finish) |
| translated | `per_worker_task_concurrency.py:105` | param | params.image_id (int) | param image_id |
| approximated | `per_worker_task_concurrency.py:114` | logging |  | a log call in the flow body does not run in Nexus (there is no flow-body process); task logs are captured per attempt |
| translated | `per_worker_task_concurrency.py:117` | data dependency | edge download_image -> run_ml_model {data: result} | run_ml_model(data=download_image) |
| translated | `per_worker_task_concurrency.py:118` | data dependency | edge run_ml_model -> save_results {data: result} | save_results(data=run_ml_model) |

### After import

```sh
nexusctl workflow submit process_image.yaml --dry-run --namespace team-a
```
