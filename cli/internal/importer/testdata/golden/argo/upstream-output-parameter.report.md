# nexusctl import argo report

- Input: `testdata/upstream/argo/output-parameter.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 2 translated, 2 approximated, 0 unsupported
- Result: specs not written

## output-parameter -> `output-parameter`

Source `testdata/upstream/argo/output-parameter.yaml:13`. 2 task(s), 1 edge(s). 2 translated, 2 approximated, 0 unsupported. Content address `594bc0aebfc6`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `output-parameter.yaml:22` | command | tasks[generate-parameter].runtime | generate-parameter: output parameter hello-param is copied from /tmp/hello_world.txt to $NEXUS_OUTPUT_DIR after the command |
| translated | `output-parameter.yaml:22` | container | tasks[generate-parameter].runtime (kubernetes executor) | generate-parameter: template hello-world-to-file (image busybox) |
| approximated | `output-parameter.yaml:24` | command | tasks[consume-parameter].runtime | consume-parameter: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| translated | `output-parameter.yaml:24` | container | tasks[consume-parameter].runtime (kubernetes executor) | consume-parameter: template print-message (image busybox) |

### After import

```sh
nexusctl workflow submit output-parameter.yaml --dry-run --namespace team-a
```
