# nexusctl import argo report

- Input: `testdata/upstream/argo/scripts-bash.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 3 translated, 1 approximated, 0 unsupported
- Result: specs not written

## scripts-bash -> `scripts-bash`

Source `testdata/upstream/argo/scripts-bash.yaml:8`. 2 task(s), 1 edge(s). 3 translated, 1 approximated, 0 unsupported. Content address `7ff320525471`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `scripts-bash.yaml:17` | script | tasks[generate].runtime (kubernetes executor) | generate: template gen-random-int (image alpine:3.23) |
| translated | `scripts-bash.yaml:17` | outputs.result | runtime.stdout_output | generate: stdout is output result |
| approximated | `scripts-bash.yaml:19` | command | tasks[print].runtime | print: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| translated | `scripts-bash.yaml:19` | container | tasks[print].runtime (kubernetes executor) | print: template print-message (image alpine:3.23) |

### After import

```sh
nexusctl workflow submit scripts-bash.yaml --dry-run --namespace team-a
```
