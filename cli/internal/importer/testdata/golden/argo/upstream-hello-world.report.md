# nexusctl import argo report

- Input: `testdata/upstream/argo/hello-world.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 1 translated, 0 approximated, 0 unsupported
- Result: specs not written

## hello-world -> `hello-world`

Source `testdata/upstream/argo/hello-world.yaml:1`. 1 task(s), 0 edge(s). 1 translated, 0 approximated, 0 unsupported. Content address `d5200c681c52`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `hello-world.yaml:11` | container | tasks[hello-world].runtime (kubernetes executor) | hello-world: template hello-world (image busybox) |

### After import

```sh
nexusctl workflow submit hello-world.yaml --dry-run --namespace team-a
```
