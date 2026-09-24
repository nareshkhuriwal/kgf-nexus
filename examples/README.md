# Nexus examples

Read them in this order.

| Example | What it teaches |
|---|---|
| [`hello_dag/`](hello_dag/) | The basic shape: 3 tasks in a line, resources, timeouts, edges from data flow. |
| [`ten_node_mixed/`](ten_node_mixed/) | The Phase 1 exit-criteria DAG: 10 nodes, fan-out/fan-in, real GPU constraints. |
| [`idempotent_effect/`](idempotent_effect/) | The three idempotency mechanisms, in descending order of strength. |
| [`fan_out_map/`](fan_out_map/) | Dynamic expansion over a runtime collection (mandatory `max_fanout`), fan-in, and memoized children. |

## Setup (once)

```bash
python3 -m venv .venv && ./.venv/bin/pip install -e sdk/python
go build -o ./nexusctl ./cli/cmd/nexusctl
```

Point the CLI at your control plane:

```bash
export NEXUS_ENDPOINT=http://localhost:8090   # the gateway `make up` publishes
export NEXUS_API_KEY=...        # never passed as a flag in shared shells
```

`nexusctl` runs *your* interpreter to compile a `.py` spec, so tell it which one:

```bash
nexusctl workflow compile examples/hello_dag/dag.py --show --python ./.venv/bin/python
```

Or skip the flag by activating the venv first.

## The one command worth knowing

```bash
nexusctl workflow compile <file> --show
```

It prints exactly the document the control plane will parse, and contacts nothing.
ADR-0004 calls this out specifically as the mitigation for *"my Python compiled to
a graph I did not expect"* — when the graph surprises you, this is how you see it
rather than deduce it from a failed run.

## Committed `compiled.yaml` files

Each example commits its compiled spec next to the source. Two reasons: a code
review shows the effect of a decorator change on the actual document, and it
demonstrates that the YAML authoring path produces the byte-identical spec:

```bash
nexusctl workflow compile examples/hello_dag/dag.py --hash-only --python ./.venv/bin/python
nexusctl workflow compile examples/hello_dag/compiled.yaml --hash-only
# identical
```
