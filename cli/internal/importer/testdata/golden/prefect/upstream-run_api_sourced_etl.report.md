# nexusctl import prefect report

- Input: `testdata/upstream/prefect/run_api_sourced_etl.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 5 translated, 3 approximated, 2 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## devto_etl -> `devto_etl`

Source `testdata/upstream/prefect/run_api_sourced_etl.py:125`. 3 task(s), 2 edge(s). 5 translated, 3 approximated, 2 unsupported. Content address `72330341c974`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `run_api_sourced_etl.py:89` | python task | tasks[to_dataframe].runtime | to_dataframe: runs run_api_sourced_etl.to_dataframe through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="run_api_sourced_etl.to_dataframe") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `run_api_sourced_etl.py:114` | python task | tasks[save_csv].runtime | save_csv: runs run_api_sourced_etl.save_csv through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="run_api_sourced_etl.save_csv") -- inputs arrive as keyword arguments and the return value is output `result` |
| approximated | `run_api_sourced_etl.py:125` | @flow | fail_fast | a task that raises fails the Prefect flow; fail_fast=true stops new work the same way (tasks already running finish) |
| translated | `run_api_sourced_etl.py:126` | param | params.api_base (string) | param api_base |
| translated | `run_api_sourced_etl.py:126` | param | params.pages (int) | param pages |
| translated | `run_api_sourced_etl.py:126` | param | params.per_page (int) | param per_page |
| translated | `run_api_sourced_etl.py:126` | param | params.output_file (string) | param output_file |
| unsupported | `run_api_sourced_etl.py:131` | for | placeholders: fetch_page | a for loop creates or wires tasks (fetch_page); the converter does not unroll loops or evaluate conditions, so each is a failing placeholder. Write the tasks out explicitly, or use a Nexus map (dynamic expansion) for a run-time collection |
| unsupported | `run_api_sourced_etl.py:135` | argument |  | to_dataframe: argument raw_articles is a collection of task outputs; a Nexus input binds exactly one upstream output. Ordering edges were kept |
| translated | `run_api_sourced_etl.py:138` | data dependency | edge to_dataframe -> save_csv {df: result} | save_csv(df=to_dataframe) |

### After import

```sh
nexusctl workflow submit devto_etl.yaml --dry-run --namespace team-a
```
