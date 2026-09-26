# nexusctl import argo report

- Input: `testdata/upstream/argo/loops-sequence.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 5 translated, 7 approximated, 2 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## loops-sequence -> `loops-sequence`

Source `testdata/upstream/argo/loops-sequence.yaml:1`. 9 task(s), 5 edge(s). 5 translated, 7 approximated, 2 unsupported. Content address `1012d212abc0`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `loops-sequence.yaml:1` | independent task | tasks[nexus-import-root] | sequence-param, sequence-format have no dependencies while the rest of the graph is connected; the gateway rejects that as a forgotten edge, so they hang off a no-op root task nexus-import-root |
| translated | `loops-sequence.yaml:11` | parameter | params.count (string) | workflow parameter count |
| approximated | `loops-sequence.yaml:17` | command | tasks[sequence-count].runtime | sequence-count: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| translated | `loops-sequence.yaml:17` | container | tasks[sequence-count].runtime (kubernetes executor) | sequence-count: template echo (image alpine:3.23) |
| translated | `loops-sequence.yaml:17` | steps | edges from every step of the previous group | step group 1: 5 parallel steps |
| approximated | `loops-sequence.yaml:24` | withSequence | map | sequence-count: a sequence of 5 numbers, emitted by a generated task and mapped over (max_fanout 5) |
| approximated | `loops-sequence.yaml:27` | command | tasks[sequence-start-end].runtime | sequence-start-end: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| translated | `loops-sequence.yaml:27` | container | tasks[sequence-start-end].runtime (kubernetes executor) | sequence-start-end: template echo (image alpine:3.23) |
| approximated | `loops-sequence.yaml:34` | withSequence | map | sequence-start-end: a sequence of 6 numbers, emitted by a generated task and mapped over (max_fanout 6) |
| unsupported | `loops-sequence.yaml:45` | withSequence |  | sequence-param: withSequence count is a run-time value ({{workflow.parameters.count}}) |
| approximated | `loops-sequence.yaml:49` | command | tasks[sequence-negative].runtime | sequence-negative: the command runs under sh so run-time values and file copies can be applied (the image needs a shell) |
| translated | `loops-sequence.yaml:49` | container | tasks[sequence-negative].runtime (kubernetes executor) | sequence-negative: template echo (image alpine:3.23) |
| approximated | `loops-sequence.yaml:56` | withSequence | map | sequence-negative: a sequence of 6 numbers, emitted by a generated task and mapped over (max_fanout 6) |
| unsupported | `loops-sequence.yaml:67` | withSequence |  | sequence-format: withSequence format "testuser%02X" (printf formatting) is not translated |

### After import

```sh
nexusctl workflow submit loops-sequence.yaml --dry-run --namespace team-a
```
