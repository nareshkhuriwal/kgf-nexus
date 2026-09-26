# nexusctl import argo report

- Input: `testdata/upstream/argo/pod-resources.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 2 translated, 1 approximated, 0 unsupported
- Result: specs not written

## pod-resources -> `pod-resources`

Source `testdata/upstream/argo/pod-resources.yaml:9`. 1 task(s), 0 edge(s). 2 translated, 1 approximated, 0 unsupported. Content address `ea42266b4aad`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `pod-resources.yaml:14` | container | tasks[hello-world].runtime (kubernetes executor) | hello-world: template hello-world (image busybox) |
| translated | `pod-resources.yaml:14` | resources | tasks[hello-world].resources | hello-world: resource requests/limits |
| approximated | `pod-resources.yaml:17` | podResources | resources | pod-level resources are applied to every task that declares none of its own (Nexus reserves per task) |

### After import

```sh
nexusctl workflow submit pod-resources.yaml --dry-run --namespace team-a
```
