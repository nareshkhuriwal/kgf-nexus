# nexusctl import argo report

- Input: `testdata/upstream/argo/cron-workflow.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 2 translated, 3 approximated, 2 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## hello-world -> `hello-world`

Source `testdata/upstream/argo/cron-workflow.yaml:1`. 1 task(s), 0 edge(s). 2 translated, 3 approximated, 2 unsupported. Content address `859f240870be`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `cron-workflow.yaml:7` | CronWorkflow | companion: nexusctl schedule create hello-world | schedule "* * * * *" in America/Los_Angeles |
| approximated | `cron-workflow.yaml:9` | startingDeadlineSeconds |  | startingDeadlineSeconds is managed by the Nexus scheduler; ignored |
| unsupported | `cron-workflow.yaml:10` | concurrencyPolicy |  | concurrencyPolicy Replace cancels the running workflow when the next one is due; Nexus schedules skip, allow or queue one (the suggested schedule uses the default, skip) |
| approximated | `cron-workflow.yaml:11` | successfulJobsHistoryLimit |  | successfulJobsHistoryLimit is managed by the Nexus scheduler; ignored |
| approximated | `cron-workflow.yaml:12` | failedJobsHistoryLimit |  | failedJobsHistoryLimit is managed by the Nexus scheduler; ignored |
| translated | `cron-workflow.yaml:15` | container | tasks[hello-world-with-time].runtime (kubernetes executor) | hello-world-with-time: template hello-world-with-time (image busybox) |
| unsupported | `cron-workflow.yaml:21` | template reference |  | hello-world-with-time: {{workflow.scheduledTime}} is the run's scheduled/creation time, which Nexus does not pass to tasks; add a workflow param or compute it in the task |

### After import

```sh
nexusctl workflow submit hello-world.yaml --dry-run --namespace team-a
nexusctl schedule create hello-world --workflow hello-world --cron '* * * * *' --timezone America/Los_Angeles --namespace team-a
```
