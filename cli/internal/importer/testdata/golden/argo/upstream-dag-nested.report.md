# nexusctl import argo report

- Input: `testdata/upstream/argo/dag-nested.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 31 translated, 0 approximated, 0 unsupported
- Result: specs not written

## dag-nested -> `dag-nested`

Source `testdata/upstream/argo/dag-nested.yaml:1`. 16 task(s), 20 edge(s). 31 translated, 0 approximated, 0 unsupported. Content address `cec006928617`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `dag-nested.yaml:25` | depends | edges | B: depends "A": all must succeed |
| translated | `dag-nested.yaml:30` | depends | edges | C: depends "A": all must succeed |
| translated | `dag-nested.yaml:35` | depends | edges | D: depends "B && C": all must succeed |
| translated | `dag-nested.yaml:45` | container | tasks[A.A].runtime (kubernetes executor) | A.A: template echo (image busybox) |
| translated | `dag-nested.yaml:45` | container | tasks[B.A].runtime (kubernetes executor) | B.A: template echo (image busybox) |
| translated | `dag-nested.yaml:45` | container | tasks[C.A].runtime (kubernetes executor) | C.A: template echo (image busybox) |
| translated | `dag-nested.yaml:45` | container | tasks[D.A].runtime (kubernetes executor) | D.A: template echo (image busybox) |
| translated | `dag-nested.yaml:49` | container | tasks[A.B].runtime (kubernetes executor) | A.B: template echo (image busybox) |
| translated | `dag-nested.yaml:49` | container | tasks[B.B].runtime (kubernetes executor) | B.B: template echo (image busybox) |
| translated | `dag-nested.yaml:49` | container | tasks[C.B].runtime (kubernetes executor) | C.B: template echo (image busybox) |
| translated | `dag-nested.yaml:49` | container | tasks[D.B].runtime (kubernetes executor) | D.B: template echo (image busybox) |
| translated | `dag-nested.yaml:50` | depends | edges | A.B: depends "A": all must succeed |
| translated | `dag-nested.yaml:50` | depends | edges | B.B: depends "A": all must succeed |
| translated | `dag-nested.yaml:50` | depends | edges | C.B: depends "A": all must succeed |
| translated | `dag-nested.yaml:50` | depends | edges | D.B: depends "A": all must succeed |
| translated | `dag-nested.yaml:54` | container | tasks[A.C].runtime (kubernetes executor) | A.C: template echo (image busybox) |
| translated | `dag-nested.yaml:54` | container | tasks[B.C].runtime (kubernetes executor) | B.C: template echo (image busybox) |
| translated | `dag-nested.yaml:54` | container | tasks[C.C].runtime (kubernetes executor) | C.C: template echo (image busybox) |
| translated | `dag-nested.yaml:54` | container | tasks[D.C].runtime (kubernetes executor) | D.C: template echo (image busybox) |
| translated | `dag-nested.yaml:55` | depends | edges | A.C: depends "A": all must succeed |
| translated | `dag-nested.yaml:55` | depends | edges | B.C: depends "A": all must succeed |
| translated | `dag-nested.yaml:55` | depends | edges | C.C: depends "A": all must succeed |
| translated | `dag-nested.yaml:55` | depends | edges | D.C: depends "A": all must succeed |
| translated | `dag-nested.yaml:59` | container | tasks[A.D].runtime (kubernetes executor) | A.D: template echo (image busybox) |
| translated | `dag-nested.yaml:59` | container | tasks[B.D].runtime (kubernetes executor) | B.D: template echo (image busybox) |
| translated | `dag-nested.yaml:59` | container | tasks[C.D].runtime (kubernetes executor) | C.D: template echo (image busybox) |
| translated | `dag-nested.yaml:59` | container | tasks[D.D].runtime (kubernetes executor) | D.D: template echo (image busybox) |
| translated | `dag-nested.yaml:60` | depends | edges | A.D: depends "B && C": all must succeed |
| translated | `dag-nested.yaml:60` | depends | edges | B.D: depends "B && C": all must succeed |
| translated | `dag-nested.yaml:60` | depends | edges | C.D: depends "B && C": all must succeed |
| translated | `dag-nested.yaml:60` | depends | edges | D.D: depends "B && C": all must succeed |

### After import

```sh
nexusctl workflow submit dag-nested.yaml --dry-run --namespace team-a
```
