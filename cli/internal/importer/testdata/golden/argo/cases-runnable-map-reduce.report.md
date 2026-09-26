# nexusctl import argo report

- Input: `testdata/cases/argo/runnable-map-reduce.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 10 translated, 5 approximated, 0 unsupported
- Result: specs not written

## runnable-map-reduce -> `runnable-map-reduce`

Source `testdata/cases/argo/runnable-map-reduce.yaml:5`. 4 task(s), 4 edge(s). 10 translated, 5 approximated, 0 unsupported. Content address `48d74aa22ade`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `runnable-map-reduce.yaml:13` | parameter | params.prefix (string) | workflow parameter prefix |
| translated | `runnable-map-reduce.yaml:18` | script | tasks[generate].runtime (kubernetes executor) | generate: template gen-words (image alpine:3.23) |
| translated | `runnable-map-reduce.yaml:18` | steps | edges from every step of the previous group | step group 1: 2 parallel steps |
| translated | `runnable-map-reduce.yaml:18` | outputs.result | runtime.stdout_output | generate: stdout is output result |
| approximated | `runnable-map-reduce.yaml:20` | command | tasks[stamp].runtime | stamp: output parameter stamp is copied from /tmp/nexus-import-stamp.txt to $NEXUS_OUTPUT_DIR after the command |
| translated | `runnable-map-reduce.yaml:20` | container | tasks[stamp].runtime (kubernetes executor) | stamp: template write-stamp (image alpine:3.23) |
| approximated | `runnable-map-reduce.yaml:22` | command | tasks[shout].runtime | shout: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `runnable-map-reduce.yaml:22` | max_fanout | tasks[shout].map.max_fanout | shout: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `runnable-map-reduce.yaml:22` | container | tasks[shout].runtime (kubernetes executor) | shout: template upper (image alpine:3.23) |
| translated | `runnable-map-reduce.yaml:22` | outputs.result | runtime.stdout_output | shout: stdout is output result |
| translated | `runnable-map-reduce.yaml:28` | withParam | map.over_node_id=generate | shout: map over the output of generate |
| approximated | `runnable-map-reduce.yaml:29` | command | tasks[collect].runtime | collect: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| translated | `runnable-map-reduce.yaml:29` | container | tasks[collect].runtime (kubernetes executor) | collect: template join (image alpine:3.23) |
| approximated | `runnable-map-reduce.yaml:62` | retryPolicy | retry.retry_on | shout: OnFailure (Argo's default) -> retry on application, timeout and oom errors |
| translated | `runnable-map-reduce.yaml:62` | retryStrategy.limit | retry.max_attempts=3 | shout: limit 2 |

### After import

```sh
nexusctl workflow submit runnable-map-reduce.yaml --dry-run --namespace team-a
```
