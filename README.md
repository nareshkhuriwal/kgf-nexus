# KGF Nexus

A distributed workflow orchestrator. You describe a DAG of tasks as a
versioned, declarative spec, in Python or YAML. Nexus decides when and where
each task runs, dispatches it to a fleet of worker agents under a fenced
lease, records every state change in PostgreSQL, and moves results through
an object store that the control plane never reads.

It is application-agnostic. ETL, ML training, batch inference and file
processing all run on the same platform, because domain logic lives in your
task handlers. The core only knows about graphs, tasks, resources and states.

> **Status: under active development, not production-ready.** Nexus has not
> passed its production acceptance gates. The known gaps include OIDC/SAML
> single sign-on (not implemented), opt-in inter-service TLS, secret
> references that workers do not yet resolve, and GPU execution not yet
> qualified on real hardware. The current list is in
> [docs/README.md](docs/README.md#current-state), and open defects are in
> [CONTRACT-FINDINGS.md](CONTRACT-FINDINGS.md).

## Key capabilities

- **Declarative, versioned specs.** The Python SDK *compiles* your workflow on
  your machine. The control plane receives only a canonical,
  content-addressed document and never imports or runs your code
  ([ADR-0004](docs/adr/0004-declarative-workflow-spec.md)). YAML specs are
  first-class, and `nexusctl workflow compile --show` prints exactly what the
  server will parse.
- **Correctness under failure.** Dispatch is one PostgreSQL transaction
  covering state, lease, resource reservation and event. Leases are fenced, so
  stale workers cannot overwrite results. Workers keep a write-ahead log. `LOST`
  is kept distinct from `FAILED`. Execution is **at least once**, with a
  platform-derived idempotency key that is stable across retries
  ([ADR-0006](docs/adr/0006-at-least-once-with-idempotency.md),
  [ADR-0010](docs/adr/0010-lease-based-task-ownership.md)).
- **Dynamic fan-out and fan-in.** A `map` expands over a collection only known
  at runtime. `max_fanout` is mandatory, the fan-in size is bounded, and every
  child is a real node with its own retries and logs.
- **Resource-aware scheduling.** CPU, memory, disk, GPU count, kind and memory,
  and custom countable resources are reserved atomically. Aging prevents
  starvation. There is weighted fair share across namespaces
  ([ADR-0017](docs/adr/0017-weighted-fair-share-across-namespaces.md)), plus
  namespace quotas and usage metering
  ([ADR-0021](docs/adr/0021-namespace-quotas-and-usage-metering.md)).
- **Retries, timeouts and memoization.** Error-class-aware retry policies,
  Temporal's four-timeout model, and memoization keyed on task version and
  input content.
- **Schedules and backfills.** Durable, time-zone-aware cron with explicit
  daylight-saving rules, overlap policies, bounded catch-up, and resumable
  backfills.
- **Executors.** Subprocess, Docker, and Kubernetes Jobs (Pod Security
  restricted), behind one file contract
  ([ADR-0007](docs/adr/0007-executor-abstraction.md)).
- **Operator surface.** A React console (run DAGs, attempts, durable
  searchable logs, schedules, workers, quotas, RBAC), the `nexusctl` CLI, a
  typed Python SDK and client, and a REST API described by
  [OpenAPI 3.1](api/openapi.yaml).
- **Local development.** `nexusctl run --local` runs a workflow on your
  machine with the same scheduling decisions as the DAG engine. `nexusctl
  replay` re-runs a failed attempt locally with its original inputs
  ([ADR-0022](docs/adr/0022-local-runner-and-replay.md)).
- **Migration tooling.** `nexusctl import airflow|prefect|argo` converts
  pipelines statically, without executing your code. Each import comes with a
  report of what translated, what was approximated, and what did not
  translate.
- **Operations.** Backup and restore, and PITR with pgBackRest, each with a
  recorded drill. Additive migrations with rolling-upgrade rules. Prometheus
  metrics. Optional NATS JetStream through a transactional outbox. A Helm
  chart and Terraform for AKS, EKS and GKE ([docs/OPERATIONS.md](docs/OPERATIONS.md)).

## Quickstart

Requirements: Docker with Compose v2, Go 1.25+, Python 3.11+.

```bash
go build -o ./nexusctl ./cli/cmd/nexusctl && export PATH="$PWD:$PATH"
nexusctl up                                  # local stack; prints the console URL and credentials once
nexusctl init etl orders-etl && cd orders-etl
python3 -m venv .venv && . .venv/bin/activate
pip install -e ../sdk/python -r requirements.txt pytest && pytest -q
docker build --build-context nexus-sdk=../sdk/python -t orders-etl:0.1.0 .
nexusctl workflow submit dag.py
nexusctl run create orders-etl --param source=/app/data/orders.csv --param target_db=/tmp/warehouse.db --wait
nexusctl run get <run-id>                    # then: nexusctl logs <attempt-id>
nexusctl run --local dag.py --host-executor --param source=data/orders.csv --param target_db=warehouse.db
```

The [quickstart](docs-site/docs/quickstart.md) walks through each step with
real output, including the console at <http://localhost:3000>.

## A workflow

```python
from nexus import Resources, Retry, TaskContext, task, workflow

@task(resources=Resources(cpu="1", memory="2Gi"), retry=Retry(max_attempts=4))
def extract(ctx: TaskContext, source: str) -> list[str]:
    return list_keys(source)

@task(cache=True)
def process(ctx: TaskContext, key: str) -> dict: ...

@task(trigger_rule="all_done")
def report(ctx: TaskContext, results: list) -> dict: ...

@workflow(name="files", params={"source": {"type": "string"}})
def files(source: str):
    return report(results=process.map(extract(source), item="key", max_fanout=1000))
```

## Quick start

Needs Docker (Compose v2) and Go. From a fresh checkout:

```bash
make quickstart    # = go run ./cli/cmd/nexusctl up
```

It starts the local stack, registers a tenant, mints an admin key and the
agents' worker token through the API, and prints the console URL and
credentials once. `go run ./cli/cmd/nexusctl down --volumes` removes the stack
and its local record again. `make up` is the path
for driving Compose with your own keys (deploy/README.md).

## Documentation

The documentation site lives in [`docs-site/`](docs-site/README.md). Build it
with `make docs`, a strict build with a link check, and preview it with
`make docs-serve`. It contains:

| Section | What it covers |
| --- | --- |
| [Quickstart](docs-site/docs/quickstart.md) | ten minutes from checkout to a run |
| [Concepts](docs-site/docs/concepts/index.md) | specs, tasks, DAG semantics, fan-out, retries and idempotency, leases, resources, fair share, quotas, results, logs, security |
| [Recipes](docs-site/docs/recipes/index.md) | ETL, ML training, batch inference, file fan-out, schedules, secrets, GPUs, Kubernetes, local development |
| [Migrating](docs-site/docs/migrate/index.md) | from Airflow, Prefect and Argo Workflows, with concept maps and what cannot be translated |
| Reference | REST API ([api/openapi.yaml](api/openapi.yaml), also served at `GET /v1/openapi.yaml`), `nexusctl`, the Python SDK and the spec schema, each generated from source |

The design and operations documents are rendered in the site and live next to
the code:

| Document | What it covers |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | the design, including §2 on what Nexus takes from Airflow, Prefect, Temporal, Dagster, Argo, Flyte and others, and what it rejects |
| [docs/adr/](docs/adr/README.md) | decision records, each with the alternatives and why they were rejected |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | the runbook: backup, restore, upgrades, metrics, Helm, quotas |
| [docs/TLS.md](docs/TLS.md), [docs/MULTITENANCY.md](docs/MULTITENANCY.md) | transport security, tenancy |
| [cli/README.md](cli/README.md), [sdk/python/README.md](sdk/python/README.md), [ui/README.md](ui/README.md) | the CLI, the SDK and the console |
| [examples/](examples/README.md) | runnable workflows, including self-checking workloads |

Performance figures, such as the dispatch ceiling
([ADR-0018](docs/adr/0018-postgres-dispatch-ceiling.md)) and the restore and
PITR drills (OPERATIONS §3–§4), are laptop measurements with their hardware
stated. They are not production capacity claims. Nexus does not claim feature
parity with Airflow, Prefect, Argo or any other orchestrator.

## Repository layout

```text
services/     control plane: api-gateway, workflow-manager, dag-engine, scheduler, resource-manager, event-service
agent/        the worker agent and its executors
cli/          nexusctl
sdk/python/   the Python SDK (authoring, client, local runner)
ui/           the operator console
api/          OpenAPI 3.1 description of the REST API
schemas/      the workflow spec JSON Schema
deploy/       Compose, Helm, Terraform, Kubernetes manifests, backup and restore scripts
docs/         architecture, ADRs, operations
docs-site/    the documentation site
examples/     runnable workflows
```

## Development

```bash
make help          # every target
make build test    # Go build, then Go, UI and SDK tests
make up            # the compose stack (or: nexusctl up)
make docs          # the documentation site
```

## Evaluating Nexus

These four pages are for anyone deciding whether Nexus fits. They are kept
separate from the rest of the documentation above.

- [docs/BENCHMARKS.md](docs/BENCHMARKS.md): measured dispatch throughput and
  latency, inline-vs-blob results, chaos and restore drills. The method and
  hardware are recorded, and every figure is a laptop figure, not a production
  capacity figure.
- [docs/COMPARISON.md](docs/COMPARISON.md): Nexus next to Airflow, Prefect,
  Argo Workflows and Flyte, with sources. It says where each is stronger.
- [docs/ROADMAP.md](docs/ROADMAP.md): what has shipped, what is in progress,
  and what is planned.
- [docs/KGF-INTEGRATION.md](docs/KGF-INTEGRATION.md): how Nexus ships as the
  `KGF.Nexus` service of KGF Cloud. This is a design; it has not been built
  ([ADR-0033](docs/adr/0033-nexus-as-kgf-nexus-service.md)).

Nexus has not passed its production acceptance gates and is not described as
production-ready.

## License

Copyright 2026 Khuriwal Group. Licensed under the
[Apache License, Version 2.0](LICENSE). Attribution notices for bundled
third-party software are in [NOTICE](NOTICE), and a per-dependency inventory
is in [docs/THIRD-PARTY-LICENSES.md](docs/THIRD-PARTY-LICENSES.md). All
dependencies are under permissive licences. Source files carry
`SPDX-License-Identifier: Apache-2.0`.

Contributions are accepted under the terms of the Apache License 2.0
(section 5).
