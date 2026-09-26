# nexusctl import argo report

- Input: `testdata/upstream/argo/artifact-passing.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 2 translated, 2 approximated, 0 unsupported
- Result: specs not written

## artifact-passing -> `artifact-passing`

Source `testdata/upstream/argo/artifact-passing.yaml:3`. 2 task(s), 1 edge(s). 2 translated, 2 approximated, 0 unsupported. Content address `2fbb03c2389f`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `artifact-passing.yaml:12` | command | tasks[generate-artifact].runtime | generate-artifact: output artifact hello-art (/tmp/hello_world.txt) is stored as a Nexus output (files only) |
| translated | `artifact-passing.yaml:12` | container | tasks[generate-artifact].runtime (kubernetes executor) | generate-artifact: template hello-world-to-file (image busybox) |
| translated | `artifact-passing.yaml:14` | container | tasks[consume-artifact].runtime (kubernetes executor) | consume-artifact: template print-message-from-file (image busybox) |
| approximated | `artifact-passing.yaml:34` | input artifact | edge generate-artifact -> consume-artifact | consume-artifact: artifact message travels through the Nexus result store as a task output and is copied to /tmp/message before the command (files only, not directories; large files should be passed by reference) |

### After import

```sh
nexusctl workflow submit artifact-passing.yaml --dry-run --namespace team-a
```
