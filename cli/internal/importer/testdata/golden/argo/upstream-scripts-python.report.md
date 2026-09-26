# nexusctl import argo report

- Input: `testdata/upstream/argo/scripts-python.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 3 translated, 1 approximated, 0 unsupported
- Result: specs not written

## scripts-python -> `scripts-python`

Source `testdata/upstream/argo/scripts-python.yaml:8`. 2 task(s), 1 edge(s). 3 translated, 1 approximated, 0 unsupported. Content address `8b3dc8d4b19b`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `scripts-python.yaml:17` | script | tasks[generate].runtime (kubernetes executor) | generate: template gen-random-int (image python:alpine3.23) |
| translated | `scripts-python.yaml:17` | outputs.result | runtime.stdout_output | generate: stdout is output result |
| approximated | `scripts-python.yaml:19` | command | tasks[print].runtime | print: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| translated | `scripts-python.yaml:19` | container | tasks[print].runtime (kubernetes executor) | print: template print-message (image alpine:3.23) |

### After import

```sh
nexusctl workflow submit scripts-python.yaml --dry-run --namespace team-a
```
