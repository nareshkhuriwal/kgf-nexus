# nexusctl import argo report

- Input: `testdata/upstream/argo/conditionals.yaml` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 5 translated, 0 approximated, 3 unsupported
- Result: nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs)

## conditional -> `conditional`

Source `testdata/upstream/argo/conditionals.yaml:10`. 3 task(s), 0 edge(s). 5 translated, 0 approximated, 3 unsupported. Content address `647745f0e3d1`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| translated | `conditionals.yaml:18` | parameter | params.should-print (string) | workflow parameter should-print |
| translated | `conditionals.yaml:27` | container | tasks[print-hello-govaluate].runtime (kubernetes executor) | print-hello-govaluate: template argosay (image argoproj/argosay:v1) |
| translated | `conditionals.yaml:27` | steps | edges from every step of the previous group | step group 1: 3 parallel steps |
| unsupported | `conditionals.yaml:29` | when | tasks[print-hello-govaluate] | print-hello-govaluate: `when: {{inputs.parameters.should-print}} == true` runs the step only if a condition holds at run time; Nexus DAGs are static, so in the partial spec it runs unconditionally. Move the condition into the task |
| translated | `conditionals.yaml:30` | container | tasks[print-hello-expr].runtime (kubernetes executor) | print-hello-expr: template argosay (image argoproj/argosay:v1) |
| unsupported | `conditionals.yaml:32` | when | tasks[print-hello-expr] | print-hello-expr: `when: {{= inputs.parameters["should-print"] == 'true'}}` runs the step only if a condition holds at run time; Nexus DAGs are static, so in the partial spec it runs unconditionally. Move the condition into the task |
| translated | `conditionals.yaml:33` | container | tasks[print-hello-expr-json].runtime (kubernetes executor) | print-hello-expr-json: template argosay (image argoproj/argosay:v1) |
| unsupported | `conditionals.yaml:35` | when | tasks[print-hello-expr-json] | print-hello-expr-json: `when: {{=jsonpath(workflow.parameters.json, '$[0].value') == 'true'}}` runs the step only if a condition holds at run time; Nexus DAGs are static, so in the partial spec it runs unconditionally. Move the condition into the task |

### After import

```sh
nexusctl workflow submit conditional.yaml --dry-run --namespace team-a
```
