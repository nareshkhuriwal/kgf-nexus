# nexusctl import argo report

- Input: `testdata/upstream/argo/exit-handlers.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 1 translated, 0 approximated, 1 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## exit-handlers -> `exit-handlers`

Source `testdata/upstream/argo/exit-handlers.yaml:9`. 1 task(s), 0 edge(s). 1 translated, 0 approximated, 1 unsupported. Content address `dc795420dfc2`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `exit-handlers.yaml:16` | container | tasks[intentional-fail].runtime (kubernetes executor) | intentional-fail: template intentional-fail (image alpine:3.23) |
| unsupported | `exit-handlers.yaml:17` | onExit |  | exit handler exit-handler runs after the workflow whatever its outcome; Nexus has no exit handlers (watch the run with `nexusctl events --follow`, or add a final task with trigger_rule all_done). The handler's templates were not converted |

### After import

```sh
nexusctl workflow submit exit-handlers.yaml --dry-run --namespace team-a
```
