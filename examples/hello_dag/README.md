# hello_dag — a 3-task linear DAG

The smallest useful Nexus workflow: `extract → transform → load`. Start here.

## Run it

```bash
# 1. Install the SDK (once, from the repo root). A venv keeps it out of system
#    site-packages.
python3 -m venv .venv && ./.venv/bin/pip install -e sdk/python

# 2. See exactly what the control plane will receive. This is local -- it
#    contacts nothing.
nexusctl workflow compile examples/hello_dag/dag.py --show \
  --python ./.venv/bin/python

# 3. Validate without registering anything.
nexusctl workflow submit examples/hello_dag/dag.py --dry-run \
  --python ./.venv/bin/python

# 4. Register the spec.
nexusctl workflow submit examples/hello_dag/dag.py \
  --python ./.venv/bin/python

# 5. Start a run and follow it.
nexusctl run create hello-dag --param source=s3://example/input.csv
nexusctl events <run-id> --follow
nexusctl run get <run-id>
```

The YAML path is equivalent, and produces the identical document:

```bash
nexusctl workflow submit examples/hello_dag/compiled.yaml
```

Without nexusctl, the Python file prints its own spec:

```bash
./.venv/bin/python examples/hello_dag/dag.py
```

## Files

| File | What it is |
|---|---|
| `dag.py` | The workflow. Read the module docstring first. |
| `compiled.yaml` | The compiled WorkflowSpec. Committed so a diff shows up in review. |

## What to notice

**The decorators run in your process, not the server's.** Importing `dag.py`
compiles a JSON document; the control plane parses that document and never
imports your module (ADR-0004). So the control plane needs none of your
dependencies, and a slow import here cannot degrade scheduling for anyone.

**Edges come from data flow.** `transform(extract(source))` creates the edge.
There is no `set_downstream` step because the data dependency *is* the dependency.
The parameter name on the downstream task becomes the input name in the edge's
`output_to_input` map, which is what lets the server type-check it at submit time.

**`source` is a run parameter, not a literal.** It is bound when a run is created.
Baking the value in would make the spec hash depend on whoever compiled it, and
content addressing would stop meaning anything.

**Compiling twice is byte-identical.** The document is canonically ordered, so
resubmitting an unchanged workflow is a no-op instead of a new version:

```bash
nexusctl workflow compile examples/hello_dag/dag.py --hash-only --python ./.venv/bin/python
nexusctl workflow compile examples/hello_dag/dag.py --hash-only --python ./.venv/bin/python
# same hash both times
```

## Next

- `examples/ten_node_mixed/` — fan-out/fan-in with real GPU constraints.
- `examples/idempotent_effect/` — how to make a side-effecting task safe to retry.
