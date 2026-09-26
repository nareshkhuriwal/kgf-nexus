# nexusctl import prefect report

- Input: `testdata/upstream/prefect/hello_world.py` (1 file(s))
- Namespace: `team-a`
- Workflows: 1
- Constructs: 1 translated, 2 approximated, 0 unsupported
- Result: specs not written

## hello -> `hello`

Source `testdata/upstream/prefect/hello_world.py:33`. 1 task(s), 0 edge(s). 1 translated, 2 approximated, 0 unsupported. Content address `fa4519f9b7b7`.

| Status | Source | Construct | Nexus | Note |
|---|---|---|---|---|
| approximated | `hello_world.py:33` | @flow | tasks[hello] | flow hello calls no task, so the whole flow body becomes one task (its parameters are the task's inputs) |
| approximated | `hello_world.py:34` | python task | tasks[hello].runtime | hello: runs hello_world.hello through `python -m nexus.entrypoint`; decorate it with @nexus.task(task_ref="hello_world.hello") -- inputs arrive as keyword arguments and the return value is output `result` |
| translated | `hello_world.py:34` | param | params.name (string) | param name |

### After import

```sh
nexusctl workflow submit hello.yaml --dry-run --namespace team-a
```
