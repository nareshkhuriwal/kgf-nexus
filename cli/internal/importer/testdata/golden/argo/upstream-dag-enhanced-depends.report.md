# nexusctl import argo report

- Input: `testdata/upstream/argo/dag-enhanced-depends.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 10 translated, 0 approximated, 3 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## dag-diamond -> `dag-diamond`

Source `testdata/upstream/argo/dag-enhanced-depends.yaml:1`. 7 task(s), 10 edge(s). 10 translated, 0 approximated, 3 unsupported. Content address `18bc4edb1387`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `dag-enhanced-depends.yaml:11` | container | tasks[A].runtime (kubernetes executor) | A: template pass (image busybox) |
| translated | `dag-enhanced-depends.yaml:13` | container | tasks[B].runtime (kubernetes executor) | B: template pass (image busybox) |
| translated | `dag-enhanced-depends.yaml:14` | depends | edges | B: depends "A": all must succeed |
| translated | `dag-enhanced-depends.yaml:16` | container | tasks[C].runtime (kubernetes executor) | C: template fail (image busybox) |
| translated | `dag-enhanced-depends.yaml:17` | depends | edges | C: depends "A": all must succeed |
| translated | `dag-enhanced-depends.yaml:19` | container | tasks[should-execute-1].runtime (kubernetes executor) | should-execute-1: template pass (image busybox) |
| unsupported | `dag-enhanced-depends.yaml:20` | depends | edges | should-execute-1: depends "A && (C.Succeeded \|\| C.Failed)" needs per-dependency conditions (\|\|, !, .Failed, or a mix of must-succeed and may-fail dependencies); Nexus has one trigger rule per task (all_success or all_done). The edges were kept with all_success |
| translated | `dag-enhanced-depends.yaml:22` | container | tasks[should-execute-2].runtime (kubernetes executor) | should-execute-2: template pass (image busybox) |
| unsupported | `dag-enhanced-depends.yaml:23` | depends | edges | should-execute-2: depends "B \|\| C" needs per-dependency conditions (\|\|, !, .Failed, or a mix of must-succeed and may-fail dependencies); Nexus has one trigger rule per task (all_success or all_done). The edges were kept with all_success |
| translated | `dag-enhanced-depends.yaml:25` | container | tasks[should-not-execute].runtime (kubernetes executor) | should-not-execute: template pass (image busybox) |
| translated | `dag-enhanced-depends.yaml:26` | depends | edges | should-not-execute: depends "B && C": all must succeed |
| translated | `dag-enhanced-depends.yaml:28` | container | tasks[should-execute-3].runtime (kubernetes executor) | should-execute-3: template pass (image busybox) |
| unsupported | `dag-enhanced-depends.yaml:29` | depends | edges | should-execute-3: depends "should-execute-2.Succeeded \|\| should-not-execute" needs per-dependency conditions (\|\|, !, .Failed, or a mix of must-succeed and may-fail dependencies); Nexus has one trigger rule per task (all_success or all_done). The edges were kept with all_success |

### After import

```sh
nexusctl workflow submit dag-diamond.yaml --dry-run --namespace team-a
```
