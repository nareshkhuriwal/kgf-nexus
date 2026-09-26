# nexusctl import argo report

- Input: `testdata/upstream/argo/global-parameters.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 2 translated, 1 approximated, 0 unsupported
- Result: specs not written

## global-parameters -> `global-parameters`

Source `testdata/upstream/argo/global-parameters.yaml:1`. 1 task(s), 0 edge(s). 2 translated, 1 approximated, 0 unsupported. Content address `3c268c2d6453`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `global-parameters.yaml:6` | command | tasks[print-message].runtime | print-message: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| translated | `global-parameters.yaml:6` | container | tasks[print-message].runtime (kubernetes executor) | print-message: template print-message (image busybox) |
| translated | `global-parameters.yaml:12` | parameter | params.message (string) | workflow parameter message |

### After import

```sh
nexusctl workflow submit global-parameters.yaml --dry-run --namespace team-a
```
