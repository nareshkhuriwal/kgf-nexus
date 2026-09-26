# nexusctl import argo report

- Input: `testdata/upstream/argo/loops-param-result.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 4 translated, 2 approximated, 0 unsupported
- Result: specs not written

## loops-param-result -> `loops-param-result`

Source `testdata/upstream/argo/loops-param-result.yaml:3`. 2 task(s), 1 edge(s). 4 translated, 2 approximated, 0 unsupported. Content address `e27daff15d23`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `loops-param-result.yaml:14` | script | tasks[generate].runtime (kubernetes executor) | generate: template gen-number-list (image python:alpine3.23) |
| translated | `loops-param-result.yaml:14` | outputs.result | runtime.stdout_output | generate: stdout is output result |
| approximated | `loops-param-result.yaml:16` | command | tasks[sleep].runtime | sleep: run-time values are substituted into the shell script from $NEXUS_INPUT_DIR (a placeholder inside single quotes will not expand) |
| approximated | `loops-param-result.yaml:16` | max_fanout | tasks[sleep].map.max_fanout | sleep: the source bounds the collection nowhere; max_fanout 1024 was chosen (--max-fanout). A larger collection fails the map explicitly |
| translated | `loops-param-result.yaml:16` | container | tasks[sleep].runtime (kubernetes executor) | sleep: template sleep-n-sec (image alpine:3.23) |
| translated | `loops-param-result.yaml:22` | withParam | map.over_node_id=generate | sleep: map over the output of generate |

### After import

```sh
nexusctl workflow submit loops-param-result.yaml --dry-run --namespace team-a
```
