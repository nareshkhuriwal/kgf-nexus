# nexusctl import argo report

- Input: `testdata/upstream/argo/steps.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 4 translated, 0 approximated, 0 unsupported
- Result: specs not written

## steps -> `steps`

Source `testdata/upstream/argo/steps.yaml:3`. 3 task(s), 2 edge(s). 4 translated, 0 approximated, 0 unsupported. Content address `91bd7676dc69`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `steps.yaml:12` | container | tasks[hello1].runtime (kubernetes executor) | hello1: template print-message (image busybox) |
| translated | `steps.yaml:16` | container | tasks[hello2a].runtime (kubernetes executor) | hello2a: template print-message (image busybox) |
| translated | `steps.yaml:16` | steps | edges from every step of the previous group | step group 2: 2 parallel steps |
| translated | `steps.yaml:20` | container | tasks[hello2b].runtime (kubernetes executor) | hello2b: template print-message (image busybox) |

### After import

```sh
nexusctl workflow submit steps.yaml --dry-run --namespace team-a
```
