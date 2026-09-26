# nexusctl import argo report

- Input: `testdata/upstream/argo/loops.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 1 translated, 2 approximated, 0 unsupported
- Result: specs not written

## loops -> `loops`

Source `testdata/upstream/argo/loops.yaml:1`. 2 task(s), 1 edge(s). 1 translated, 2 approximated, 0 unsupported. Content address `76014c92e189`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `loops.yaml:10` | command | tasks[print-message-loop].runtime | print-message-loop: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| translated | `loops.yaml:10` | container | tasks[print-message-loop].runtime (kubernetes executor) | print-message-loop: template print-message (image busybox) |
| approximated | `loops.yaml:17` | withItems | map | print-message-loop: a loop over 2 literal items: a generated task emits them and print-message-loop maps over them (max_fanout 2) |

### After import

```sh
nexusctl workflow submit loops.yaml --dry-run --namespace team-a
```
