# Migrating to Nexus

`nexusctl import` converts pipelines written for another orchestrator into
Nexus WorkflowSpecs, and tells you, construct by construct, how faithful the
conversion is.

| From | Command | Reads | Guide |
|---|---|---|---|
| Apache Airflow 2.x / 3.x | `nexusctl import airflow <file-or-dir>` | DAG files (`.py`) | [Airflow](airflow.md) |
| Prefect 2.x / 3.x | `nexusctl import prefect <file-or-dir>` | flow files (`.py`) and `prefect.yaml` | [Prefect](prefect.md) |
| Argo Workflows 3.x / 4.x | `nexusctl import argo <file-or-dir>` | `Workflow`, `WorkflowTemplate`, `ClusterWorkflowTemplate`, `CronWorkflow` YAML | [Argo](argo.md) |

The full flag reference is in the [CLI reference](../reference/cli.md); the
Nexus model the specs target is described under [Concepts](../concepts/index.md).

## How the import works

1. **Parse without running.** Airflow and Prefect files are parsed with
   Python's `ast` module by an analyzer embedded in `nexusctl`, run with your
   interpreter (`--python`, default `python3`; any Python 3.8+ with nothing
   installed). It never imports or executes your file, its imports, or its
   module-level code. Argo YAML is read directly, with line numbers.
2. **Convert** every DAG / flow / workflow found into one WorkflowSpec.
3. **Validate** each generated spec against the WorkflowSpec schema and the
   control plane's semantic rules. A spec that fails is a converter bug: it is
   reported as one, and nothing is written.
4. **Report** every construct the converter met, with its `file:line`:

| Status | Meaning |
|---|---|
| translated | Nexus behaves the same way. |
| approximated | Converted, but behaviour differs in a way the report states. Read these. |
| unsupported | Not converted. The spec is incomplete until you act on it. |

## The safety rule

If **any** construct is unsupported, `nexusctl import` writes **no spec** and
exits with code 1. The report is still written, because it is what tells you
what to fix. A CI job that runs `import` and then `workflow submit` therefore
cannot submit a workflow that silently lost a sensor, a branch or a template.

`--allow-partial` writes the specs anyway. Each unsupported construct that
stood for a task becomes a **placeholder task that fails when run**
(`task_ref: nexus-import/unsupported`, with the reason in its log), and the file
starts with a `# PARTIAL` comment. Replace every placeholder before relying on
the workflow.

| Exit code | When |
|---|---|
| 0 | Specs written (all translated/approximated, or `--allow-partial`) |
| 1 | Unsupported constructs without `--allow-partial`; no workflow found; a generated spec failed validation (converter bug); the analyzer could not run |
| 2 | Usage: bad flags, missing path, `--out` with more than one workflow |

## Outputs

```bash
nexusctl import airflow dags/ --out-dir nexus/ --report import-report.md
```

- `--out FILE` writes one spec; use it when the input holds exactly one
  workflow (a spec file holds one document).
- `--out-dir DIR` writes `<workflow-name>.yaml` per workflow.
- With neither, specs are printed on stdout, separated by `---` (for reading;
  `workflow submit` takes one document per file).
- `--report FILE.md` writes the full Markdown report (every finding, including
  translated ones); `--report FILE.json` writes it as JSON.
- `-o json` / `-o yaml` prints the report, with each written spec inline, on
  stdout instead of the human summary on stderr.
- `--namespace NS` sets the namespace written into the specs.
- `--max-fanout N` bounds maps the source leaves unbounded (default 1024,
  Airflow's `max_map_length`).

## What every migration needs afterwards

- **Submit with `--dry-run` first**: `nexusctl workflow submit nexus/x.yaml --dry-run`
  gives the control plane's own verdict without storing anything.
- **Schedules are separate objects.** A Nexus schedule is never part of a spec.
  The report prints the `nexusctl schedule create ...` command for each source
  schedule (cron, time zone, overlap policy), and a `nexusctl backfill start ...`
  command where the source would have caught up on missed intervals. Run them
  after the workflow is submitted.
- **Secrets.** Secret references are not converted. Provide credentials through
  the worker's environment or your image; do not paste values into a spec.
- **Code.** Python tasks run through `python -m nexus.entrypoint`. Re-decorate
  each function with `@nexus.task(task_ref="<module>.<function>")` in an
  importable module, exactly as the report names it. Inputs arrive as keyword
  arguments; the return value is the task's `result` output.
- **Idempotency.** Nexus executes tasks **at least once**: a task can run again
  after a worker is lost, and every retry re-runs it. Review each task that has
  side effects (writes, payments, notifications) and make it safe to repeat,
  using the idempotency key Nexus gives every attempt (`$NEXUS_IDEMPOTENCY_KEY`,
  or `ctx.idempotency_key` in Python).
- **Retries.** Airflow, Prefect and Argo all default to *no* retry; Nexus
  defaults to three attempts. Converted tasks therefore carry an explicit
  `retry.max_attempts` (1 when the source had no retries). Raise it deliberately,
  after the idempotency review.
