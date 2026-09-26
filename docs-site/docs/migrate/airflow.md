# Migrating from Apache Airflow

```bash
nexusctl import airflow dags/ --out-dir nexus/ --report import-report.md
```

`nexusctl` reads your DAG files **statically**: an analyzer embedded in the CLI
parses them with Python's `ast` module, run by your interpreter (`--python`,
default `python3`, 3.8 or later, no Airflow installation needed). It never
imports the file, so module-level code, imports and connections never run.
The flip side is that it only knows what is written down: a task created in a
loop, a value from `Variable.get()`, or anything else computed while Airflow
parses the file is reported as unsupported with its line, never guessed.

See [Migrating to Nexus](index.md) for the output flags, exit codes and the
rule that nothing is written while a construct is unsupported.

## Concept mapping

| Airflow | Nexus | Fidelity |
|---|---|---|
| `with DAG(...)`, `dag = DAG(...)`, `@dag` function | one WorkflowSpec; `dag_id` becomes the name (lowercased to the identifier rules) | translated |
| `description`, `doc_md`, DAG docstring | `description` | translated |
| `tags` | `labels.tags` | translated |
| `params={...}`, `Param(default, type=...)`, `@dag` function arguments | `params` with type and `default_json` | translated; JSON-schema constraints (`minLength`, `enum`...) approximated (not enforced) |
| `BashOperator(bash_command=...)` | task with `bash -c <command>`, subprocess executor | translated |
| `{{ params.x }}` in `bash_command` | input bound to param `x`, read from `$NEXUS_INPUT_DIR` by a shell helper | approximated (string quotes stripped; inside single quotes it does not expand) |
| `{{ dag.dag_id }}`, `{{ task.task_id }}` | literal text | translated |
| `{{ run_id }}`, `{{ task_instance_key_str }}` | `$NEXUS_RUN_ID` | approximated (a UUID) |
| `{{ ds }}`, `{{ ts }}`, `{{ logical_date }}`, `macros.*` | none | **unsupported**: Nexus has no logical date |
| `{% ... %}`, `{{ ti.xcom_pull(...) }}`, `{{ var.* }}`, `{{ conn.* }}` | none | **unsupported** |
| `exit 99` / `skip_on_exit_code` | the task fails | approximated (Nexus has no skipped state) |
| `env=` on BashOperator | `runtime.env` | approximated (added to, not replacing, the environment) |
| `EmptyOperator`, `DummyOperator` | task running `true` | translated |
| `PythonOperator(python_callable=f, op_args, op_kwargs)` | task running `python -m nexus.entrypoint`; literal arguments become `input_literals` | approximated (re-decorate `f` with `@nexus.task`) |
| TaskFlow `@task` | same as above; call arguments become inputs | approximated |
| TaskFlow data passing: `b(a())`, `a()["key"]` | edge with `output_to_input` (`result`, or `key` with `multiple_outputs`) | translated |
| task context parameters (`ti`, `ds`, `**context`) | none | approximated (use `NEXUS_*` environment or `TaskContext`) |
| `KubernetesPodOperator(image, cmds, arguments, env_vars, container_resources)` | kubernetes executor with image, command, args, env and resources (incl. `nvidia.com/gpu`) | translated |
| KPO `secrets`, `volumes`, `affinity`, `tolerations`, `init_containers`, `do_xcom_push`... | none | **unsupported** |
| `DockerOperator(image, command)` | docker executor | translated (string commands approximated via `sh -c`) |
| `>>`, `<<`, lists on one side, `set_upstream`/`set_downstream` | edges | translated |
| `[a, b] >> [c, d]` | none | **unsupported** (Airflow itself raises `TypeError`) |
| `chain(...)` (pairwise between equal lists), `chain_linear`, `cross_downstream` | edges | translated |
| `TaskGroup`, `@task_group` | flattened; ids become `group.task` (nested: `outer.inner.task`); dependencies on a group use its roots/leaves | translated |
| `.expand(x=[literal list])` | a generated `<task>__items` task emits the list, the task is a map over it (`max_fanout` = list length) | approximated |
| `.expand(x=upstream())`, `.partial(...).expand(...)` | map over the upstream output; partial arguments are ordinary inputs | translated (`max_fanout` from `--max-fanout`, reported) |
| a mapped task's result consumed downstream | fan-in: a JSON array of the children's outputs | translated |
| `.expand()` over several arguments, `expand_kwargs`, mapped task groups, classic operator `.partial().expand()` | none | **unsupported** |
| `retries`, `retry_delay` (also in `default_args`) | `retry.max_attempts = retries + 1`, `initial_ms`; fixed backoff | translated |
| `retry_exponential_backoff`, `max_retry_delay` | exponential, multiplier 2, full jitter; `max_ms` | approximated |
| `execution_timeout` | `timeouts.start_to_close_ms` | translated |
| `dagrun_timeout` | `run_timeout_ms` | translated |
| `trigger_rule` `all_success`, `all_done` | same | translated |
| `trigger_rule` `one_success`, `one_failed`, `none_failed`, `all_skipped` | none (in the schema, not yet accepted by the control plane) | **unsupported** |
| other trigger rules (`none_failed_min_one_success`, `always`, ...) | none | **unsupported** |
| `queue` | `queue` (lowercased to the queue rules) | translated |
| `priority_weight` | `priority = 99 + weight` (Nexus default 100) | approximated (`weight_rule` ignored) |
| `max_active_tis_per_dag` | `concurrency_key` + `concurrency_limit` | translated |
| `pool` | none | **unsupported** (slot count lives in Airflow's database; set `concurrency_key`/`concurrency_limit` by hand) |
| `max_active_runs` | `max_active_runs` (and overlap `queue_one` when 1) | translated / approximated |
| `max_active_tasks` | none | **unsupported** |
| `schedule` cron string | companion `nexusctl schedule create --cron` | translated |
| `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly` | the equivalent 5-field cron | translated |
| `timedelta(...)` schedule | cron on wall-clock boundaries when the interval divides a minute/hour/day | approximated |
| `CronTriggerTimetable` | cron and time zone | translated |
| `@once`, `None` | no schedule (`nexusctl run create`) | approximated / translated |
| Dataset/Asset schedules, custom timetables, `@continuous` | none | **unsupported** |
| `start_date` time zone | schedule `--timezone` | translated |
| `catchup=True` | companion `nexusctl backfill start --from <start_date>` | approximated |
| `is_paused_upon_creation=True` | `schedule create --disabled` | translated |
| Sensors (`*Sensor`, `@task.sensor`, any `mode`, `deferrable`) | failing placeholder | **unsupported** |
| `BranchPythonOperator`, `@task.branch`, `ShortCircuitOperator`, `LatestOnlyOperator` | failing placeholder | **unsupported** |
| `TriggerDagRunOperator`, provider operators without a mapping | failing placeholder | **unsupported** |
| `depends_on_past`, `wait_for_downstream`, callbacks, SLAs, email | none | **unsupported** |
| `Variable.get()`, tasks created in `for`/`while`/comprehensions or under `if` | failing placeholder (loops) / nothing | **unsupported** |

## Cannot translate

These have no Nexus equivalent. The report lists each occurrence with its line.

- **Sensors.** Nexus has no sensor primitive. Start the workflow from the
  system that produces the event (`nexusctl run create`), or poll inside a task
  and let its retry policy wait.
- **Branching and short-circuiting.** A Nexus DAG is static: every task runs
  when its trigger rule is met. Move the decision into the downstream tasks, or
  split the workflow.
- **Logical dates** (`{{ ds }}`, data intervals, `macros.ds_add`). Add a workflow
  param and pass it (`nexusctl run create --param ds=2026-09-01`), or compute the
  date inside the task.
- **Variables, Connections and XCom pulls in templates.** Pass values as params,
  or as task inputs through edges.
- **Pools, `max_active_tasks`, `depends_on_past`, callbacks, SLAs.**
- **Dynamic DAG structure**: tasks created in loops, DAGs generated in loops,
  conditional task creation. Write the tasks out, or use a map.
- **Trigger rules other than `all_success` and `all_done`.**

## Worked example

Two DAGs from Airflow's own examples (`tutorial.py` and `example_task_group.py`
from Airflow 3.3.2) in `dags/`:

```console
$ nexusctl import airflow dags/ --out-dir nexus/ --report import-report.md
Imported 2 Airflow workflow(s) from dags/: 26 translated, 1 approximated, 3 unsupported.

example_task_group -> example_task_group (9 tasks, 13 edges)
  16 translated, 0 approximated, 0 unsupported

tutorial -> tutorial (3 tasks, 2 edges)
  10 translated, 1 approximated, 3 unsupported
  UNSUPPORTED   tutorial.py:118  Jinja     templated: Jinja statements ({% ... %}) are rendered by Airflow before the command runs; Nexus runs the command verbatim. Rewrite them as shell
  UNSUPPORTED   tutorial.py:118  Jinja     templated: {{ ds }} is the run's LOGICAL date, which Nexus does not have: a scheduled run fires at the cron time and a backfill passes no per-tick date. Add a workflow param and pass it (`nexusctl run create --param ...`), or compute the date in the task
  UNSUPPORTED   tutorial.py:118  Jinja     templated: {{ macros.ds_add(ds, 7) }} is the run's LOGICAL date, which Nexus does not have: a scheduled run fires at the cron time and a backfill passes no per-tick date. Add a workflow param and pass it (`nexusctl run create --param ...`), or compute the date in the task
  approximated  tutorial.py:65   schedule  schedule "0 0 * * *" in UTC (an interval of 1d became cron "0 0 * * *", which fires on wall-clock boundaries rather than every interval from the start date)
  schedule: nexusctl schedule create tutorial --workflow tutorial --cron '0 0 * * *' --timezone UTC --overlap allow --namespace default

Nothing written: unsupported constructs remain (re-run with --allow-partial to write partial specs).
Full report: import-report.md
nexusctl: 3 construct(s) could not be converted; nothing was written.
  Review them in the report, then re-run with --allow-partial to write the partial spec(s), in which each one is a task that fails when run
$ echo $?
1
```

`example_task_group` converts completely; `tutorial`'s `templated` task uses a
Jinja loop and the logical date. With `--allow-partial` both specs are written,
and `templated` keeps its command verbatim, which fails when run:

```console
$ nexusctl import airflow dags/ --out-dir nexus/ --report import-report.md --allow-partial
...
PARTIAL specs written (--allow-partial): unsupported constructs are failing placeholder tasks.
Full report: import-report.md
$ cat nexus/tutorial.yaml
# Generated by `nexusctl import airflow` from dags/tutorial.py:41 (tutorial).
# 10 translated, 1 approximated, 3 unsupported. Read the import report before submitting.
# PARTIAL: tasks with task_ref nexus-import/unsupported stand in for constructs that were
# not converted, and FAIL when run. Replace them before relying on this workflow.
api_version: nexus.dev/v1
namespace: default
name: tutorial
description: This is a documentation placed anywhere
labels:
  imported-from: airflow
  source-id: tutorial
  tags: example
tasks:
  - id: print_date
    task_ref: airflow/tutorial/print_date
    retry:
      max_attempts: 2
      strategy: fixed
      initial_ms: 300000
      jitter: none
    runtime:
      executor: subprocess
      command:
        - bash
        - -c
      args:
        - date
  - id: sleep
    task_ref: airflow/tutorial/sleep
    retry:
      max_attempts: 4
      strategy: fixed
      initial_ms: 300000
      jitter: none
    runtime:
      executor: subprocess
      command:
        - bash
        - -c
      args:
        - sleep 5
  - id: templated
    task_ref: airflow/tutorial/templated
    retry:
      max_attempts: 2
      strategy: fixed
      initial_ms: 300000
      jitter: none
    runtime:
      executor: subprocess
      command:
        - bash
        - -c
      args:
        - "\n{% for i in range(5) %}\n    echo \"{{ ds }}\"\n    echo \"{{ macros.ds_add(ds, 7)}}\"\n{% endfor %}\n"
edges:
  - from_node_id: print_date
    to_node_id: sleep
  - from_node_id: print_date
    to_node_id: templated
```

`default_args` retries (1 retry after 5 minutes) became `max_attempts: 2` with a
fixed 300 s backoff, and `t2`'s own `retries=3` became `max_attempts: 4`. To
finish `templated`, add a `ds` param and rewrite the loop in shell:

```yaml
params:
  ds: {type: string, required: true}
...
  - id: templated
    inputs:
      ds: {type: json, required: true}
    input_params: {ds: ds}
    runtime:
      command: [bash, -c]
      args:
        - |
          ds=$(tr -d '"' < "$NEXUS_INPUT_DIR/ds")
          for i in 1 2 3 4 5; do echo "$ds"; date -d "$ds + 7 days" +%F; done  # GNU date
```

## After the import

1. Read every **approximated** row of the report; fix every **unsupported** one.
2. `nexusctl workflow submit nexus/<name>.yaml --dry-run`, then submit.
3. Run the companion `nexusctl schedule create ...` (and `backfill start ...`)
   commands from the report. Airflow runs a cron DAG at the *end* of its data
   interval in Airflow 2 (at the cron time in Airflow 3 by default); a Nexus
   schedule fires at the cron time.
4. Port Python callables: move each TaskFlow function to module level if it was
   defined inside the `@dag` function, and decorate it with
   `@nexus.task(task_ref="<module>.<function>")` as the report names it. Build
   an image or worker environment that contains the module.
5. Replace Connections, Variables and secrets with worker environment or image
   configuration.
6. Review idempotency: every retry, and every lost-worker recovery, runs a task
   again. See [Migrating to Nexus](index.md#what-every-migration-needs-afterwards).
