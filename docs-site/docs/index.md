# KGF Nexus

Nexus is a distributed workflow orchestrator. You describe a DAG of tasks as
a versioned, declarative spec, written in Python or YAML. Nexus decides when
and where each task runs, dispatches it to a fleet of worker agents under a
fenced lease, records every state change durably in PostgreSQL, and moves
results through an object store that the control plane never reads.

It is application-agnostic. The same platform runs ETL, ML training, batch
inference and file processing, because domain logic lives in your task
handlers and the core only knows about graphs, tasks, resources and states.

!!! info "Status"
    Nexus is implemented and under active development. It has **not** passed
    its production acceptance gates and should not be described as
    production-ready. The known gaps are listed in the
    [design README](repo:docs/README.md#current-state).

<div class="grid cards" markdown>

- **[Quickstart](quickstart.md)**: a local stack, a generated project, a run, its logs, and a local run, in about ten minutes.
- **[Concepts](concepts/index.md)**: specs, tasks, DAG semantics, retries and idempotency, leases, resources, fair share, quotas, results, logs, security.
- **[Recipes](recipes/index.md)**: ETL, ML training, batch inference, file fan-out, schedules, secrets, GPUs, Kubernetes, local development.
- **[Migrating](migrate/index.md)**: `nexusctl import airflow|prefect|argo`, with a report of what did not translate.
- **[Operations](operations/index.md)**: deploy, back up, restore, upgrade, monitor.
- **[Reference](reference/index.md)**: REST API (OpenAPI 3.1), `nexusctl`, the Python SDK, the workflow spec.

</div>

## What is different

- **The control plane never runs your code.** The SDK *compiles* your Python
  into a spec on your machine, and the control plane receives only the
  document ([ADR-0004](design/adr/0004-declarative-workflow-spec.md)). A slow
  import or a crash in your module cannot affect scheduling for anyone else.
- **PostgreSQL is the source of truth, including the queue.** Dispatch is one
  ACID transaction covering state, lease, resource reservation and event, so
  scheduler replicas share work without leader election. NATS carries only
  notifications ([ADR-0002](design/adr/0002-postgres-as-metadata-store.md),
  [ADR-0003](design/adr/0003-nats-as-event-bus.md)).
- **At-least-once, stated honestly.** Tasks may run more than once. The
  platform derives a stable idempotency key, and lease fencing rejects reports
  from stale workers ([retries and idempotency](concepts/retries-idempotency.md)).
- **Workers pull, and never hold database credentials**
  ([ADR-0005](design/adr/0005-pull-based-dispatch.md)).
- **Pluggable execution.** Subprocess, Docker and Kubernetes Jobs run behind
  one executor contract ([ADR-0007](design/adr/0007-executor-abstraction.md)).

Section 2 of the [architecture](design/architecture.md#2-what-we-take-from-prior-art-and-what-we-reject)
states what Nexus borrows from Airflow, Prefect, Temporal, Dagster, Argo,
Flyte, Kestra, Celery, Ray and Luigi, and what it deliberately rejects. It
does not claim feature parity with any of them.
