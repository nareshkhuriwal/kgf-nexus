# nexusctl import argo report

- Input: `testdata/upstream/argo/retry-backoff.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 3 translated, 2 approximated, 0 unsupported
- Result: specs not written

## retry-backoff -> `retry-backoff`

Source `testdata/upstream/argo/retry-backoff.yaml:2`. 1 task(s), 0 edge(s). 3 translated, 2 approximated, 0 unsupported. Content address `8076fb3ac7be`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `retry-backoff.yaml:9` | container | tasks[retry-backoff].runtime (kubernetes executor) | retry-backoff: template retry-backoff (image python:alpine3.23) |
| approximated | `retry-backoff.yaml:13` | retryPolicy | retry.retry_on | retry-backoff: OnFailure (Argo's default) -> retry on application, timeout and oom errors |
| translated | `retry-backoff.yaml:13` | retryStrategy.limit | retry.max_attempts=11 | retry-backoff: limit 10 |
| translated | `retry-backoff.yaml:15` | retryStrategy.backoff | retry | retry-backoff: backoff exponential |
| approximated | `retry-backoff.yaml:17` | backoff.maxDuration | timeouts.schedule_to_close_ms | retry-backoff: maxDuration 1m stops retrying after that long; it becomes the task's total budget across attempts |

### After import

```sh
nexusctl workflow submit retry-backoff.yaml --dry-run --namespace team-a
```
