# Nexus examples

Read them in this order.

| Example | What it teaches |
|---|---|
| [`hello_dag/`](hello_dag/) | The basic shape: 3 tasks in a line, resources, timeouts, edges from data flow. |
| [`ten_node_mixed/`](ten_node_mixed/) | The Phase 1 exit-criteria DAG: 10 nodes, fan-out/fan-in, real GPU constraints. |
| [`idempotent_effect/`](idempotent_effect/) | The three idempotency mechanisms, in descending order of strength. |
| [`fan_out_map/`](fan_out_map/) | Dynamic expansion over a runtime collection (mandatory `max_fanout`), fan-in, and memoized children. |

## Workload suite

Four realistic workflows that check their own results and FAIL the run if the
check fails. They are plain `sh` + `awk`, so they run on the stock agent image.

| Example | What it does | What it proves |
|---|---|---|
| [`csv_sales_pipeline/`](csv_sales_pipeline/) | 16 regions × (generate CSV → validate → aggregate) → report; 320k rows by default | A 49-node DAG; MB-sized files through the result store (MinIO); every row either aggregated or rejected, or the run fails |
| [`ml_training_pipeline/`](ml_training_pipeline/) | Synthetic regression data, a 24-trial hyperparameter search (real gradient descent), best-model selection, an R² quality gate | Runtime fan-out with shared blob inputs, memoization (a re-run reuses every trial), a gate that can fail the run |
| [`gpu_scheduling/`](gpu_scheduling/) | 8 GPU jobs (1×A100, 2×A100, A10) contending for 2 A100s + 1 A10 | Queueing under GPU contention; an audit fails the run on oversubscription, a wrong GPU model, or two concurrent jobs sharing a device |
| [`fanout_scale/`](fanout_scale/) | Fan out to N tiny tasks (500 by default) and sum them | Dispatch throughput with hundreds queued; the sum must equal the closed form exactly |

Run all four on the Kubernetes dev environment:

```bash
make k8s-dev-up      # once: deploy the platform (deploy/terraform/environments/dev)
make k8s-dev-test    # examples/run-suite.sh -- prints each verdict, exits non-zero on any failure
SCALE=large make k8s-dev-test    # 1.6M CSV rows, 50k-row training set, 1,500-way fan-out
SUITE="gpu" make k8s-dev-test    # one workload
```

On a laptop the GPUs are **simulated** (the dev agents advertise capacity and
are labelled `simulated=true`): `gpu_scheduling` tests the scheduler's GPU
accounting, queueing and device assignment, not CUDA.

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
