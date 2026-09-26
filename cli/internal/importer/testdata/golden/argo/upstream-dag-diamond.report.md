# nexusctl import argo report

- Input: `testdata/upstream/argo/dag-diamond.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 7 translated, 0 approximated, 0 unsupported
- Result: specs not written

## dag-diamond -> `dag-diamond`

Source `testdata/upstream/argo/dag-diamond.yaml:8`. 4 task(s), 4 edge(s). 7 translated, 0 approximated, 0 unsupported. Content address `ffcb553b5ca2`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `dag-diamond.yaml:18` | container | tasks[A].runtime (kubernetes executor) | A: template echo (image busybox) |
| translated | `dag-diamond.yaml:22` | container | tasks[B].runtime (kubernetes executor) | B: template echo (image busybox) |
| translated | `dag-diamond.yaml:23` | depends | edges | B: depends "A": all must succeed |
| translated | `dag-diamond.yaml:27` | container | tasks[C].runtime (kubernetes executor) | C: template echo (image busybox) |
| translated | `dag-diamond.yaml:28` | depends | edges | C: depends "A": all must succeed |
| translated | `dag-diamond.yaml:32` | container | tasks[D].runtime (kubernetes executor) | D: template echo (image busybox) |
| translated | `dag-diamond.yaml:33` | depends | edges | D: depends "B && C": all must succeed |

### After import

```sh
nexusctl workflow submit dag-diamond.yaml --dry-run --namespace team-a
```
