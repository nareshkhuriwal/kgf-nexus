# nexusctl import airflow report

- Input: `testdata/upstream/airflow/example_kubernetes.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 7 translated, 4 approximated, 13 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## example_kubernetes_operator -> `example_kubernetes_operator`

Source `testdata/upstream/airflow/example_kubernetes.py:106`. 5 task(s), 3 edge(s). 7 translated, 4 approximated, 13 unsupported. Content address `a6168de477cf`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `example_kubernetes.py:106` | independent task | tasks[nexus-import-root] | task, task-two have no dependencies while the rest of the graph is connected; the gateway rejects that as a forgotten edge, so they hang off a no-op root task nexus-import-root |
| translated | `example_kubernetes.py:108` | schedule |  | no schedule: runs start on demand (`nexusctl run create`) |
| translated | `example_kubernetes.py:109` | start_date | schedule timezone / backfill --from | start date 2021-01-01T00:00:00 |
| translated | `example_kubernetes.py:110` | tags | labels.tags | tags |
| unsupported | `example_kubernetes.py:112` | secrets |  | task: secrets is not translated by the Nexus kubernetes executor; Nexus SecretRefs are not resolved by workers yet, so provide the value through the worker's environment |
| unsupported | `example_kubernetes.py:112` | volumes |  | task: volumes is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | volume_mounts |  | task: volume_mounts is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | env_from |  | task: env_from is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | affinity |  | task: affinity is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | tolerations |  | task: tolerations is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | init_containers |  | task: init_containers is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | ports |  | task: ports is not translated by the Nexus kubernetes executor |
| unsupported | `example_kubernetes.py:112` | priority_class_name |  | task: priority_class_name is not translated by the Nexus kubernetes executor |
| approximated | `example_kubernetes.py:112` | pod settings |  | task: namespace, name, labels, on_finish_action are managed by the Nexus kubernetes executor; ignored |
| translated | `example_kubernetes.py:112` | KubernetesPodOperator | tasks[task].runtime (kubernetes executor) | task: image, cmds, arguments |
| unsupported | `example_kubernetes.py:134` | image_pull_secrets |  | task-two: image_pull_secrets is not translated by the Nexus kubernetes executor; configure NEXUS_K8S_IMAGE_PULL_SECRETS on the agent |
| approximated | `example_kubernetes.py:134` | pod settings |  | task-two: namespace, name, labels, in_cluster, get_logs, on_finish_action are managed by the Nexus kubernetes executor; ignored |
| translated | `example_kubernetes.py:134` | KubernetesPodOperator | tasks[task-two].runtime (kubernetes executor) | task-two: image, cmds, arguments |
| unsupported | `example_kubernetes.py:150` | do_xcom_push |  | write-xcom: do_xcom_push is not translated by the Nexus kubernetes executor; write the value to $NEXUS_OUTPUT_DIR/result instead of /airflow/xcom/return.json |
| approximated | `example_kubernetes.py:150` | pod settings |  | write-xcom: namespace, name, in_cluster, get_logs, on_finish_action are managed by the Nexus kubernetes executor; ignored |
| translated | `example_kubernetes.py:150` | KubernetesPodOperator | tasks[write-xcom].runtime (kubernetes executor) | write-xcom: image, cmds, arguments |
| translated | `example_kubernetes.py:162` | BashOperator | tasks[pod_task_xcom_result].runtime (bash -c, subprocess executor) | pod_task_xcom_result: bash_command |
| unsupported | `example_kubernetes.py:163` | Jinja |  | pod_task_xcom_result: {{ task_instance.xcom_pull('write-xcom')[0] }} pulls an XCom in a template; pass the upstream output as a task input instead |
| unsupported | `example_kubernetes.py:174` | dependency |  | a dependency operand is not statically resolvable (a call to list()); the edge was not created |

### After import

```sh
nexusctl workflow submit example_kubernetes_operator.yaml --dry-run --namespace team-a
```
