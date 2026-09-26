# Concepts

Nexus runs **workflows**: directed acyclic graphs of **tasks**. The model
fits on one page. Each concept below links to the decision record or runbook
section that explains it in full.

| Concept | One line |
| --- | --- |
| [Workflows and specs](workflows.md) | A workflow is a declarative, versioned, content-addressed JSON/YAML document. The Python SDK compiles to it and the control plane parses it. |
| [Tasks](tasks.md) | A node in the graph: a typed interface (inputs and outputs), a runtime (what to execute and where), and policies for resources, timeouts, retries and caching. |
| [DAG semantics](dag-semantics.md) | Edges carry data. Trigger rules decide whether a node runs, and failure propagates as `UPSTREAM_FAILED`. |
| [Fan-out and fan-in](fan-out-fan-in.md) | `map` expands a template over a collection that exists only at runtime. Downstream nodes receive the children's outputs as one list. |
| [Retries and idempotency](retries-idempotency.md) | Execution is at least once. The platform derives a stable idempotency key, and you make effects idempotent. |
| [Leases and at-least-once](leases.md) | A worker owns an attempt through a fenced lease, so reports from a stale worker are rejected. |
| [Resources and GPUs](resources.md) | CPU, memory, disk, GPU count, kind and memory, and custom countable resources. They are requested per task and reserved atomically at dispatch. |
| [Queues and fair share](queues-fair-share.md) | Workers pull from queues. Priority and aging order work within a namespace, and weighted fair share orders work across namespaces. |
| [Quotas](quotas.md) | Per-namespace limits on concurrent attempts, resources and run volume, with a usage ledger. |
| [Results and artifacts](results-artifacts.md) | Small results are stored inline in Postgres. Large ones go to the result store (S3, Azure Blob or GCS) and never pass through the control plane. |
| [Logs](logs.md) | Durable, per-attempt logs that are searchable, can be followed live, and are bounded. |
| [Security and tenancy](security-tenancy.md) | Tenants, namespaces, roles, scoped and service-account tokens, worker credentials, worker-resolved secrets and the audit trail. |
| [Events, triggers, sensors and lineage](events-triggers.md) | Notifications on run outcomes, signed webhook triggers, sensors on data arrival or upstream success, and declared asset lineage. |
| [Nexus in KGF Cloud](kgf-cloud.md) | Nexus as the `KGF.Nexus` service: KGF context authentication, one namespace per resource group, and what stays in Nexus versus KGF. |

## The shape of the system

```text
 authoring                    control plane (Go)                     execution plane
 ─────────                    ──────────────────                     ───────────────
 dag.py ──compile──►  spec ──► api-gateway ─► workflow-manager       agent ──► subprocess
 spec.yaml ─────────────────►      │          dag-engine             agent ──► docker
 nexusctl / SDK / console          │          scheduler ◄──poll/lease─ agent ──► kubernetes
                                   │          resource-manager
                                   ▼          event-service ─► NATS (hints only)
                             PostgreSQL (source of truth, including the queue)
                                              S3-compatible result store ◄── workers read and write directly
```

The design keeps three invariants. The control plane never executes user code.
Workers never hold database credentials. Task payloads never pass through the
control plane. The [architecture](../design/architecture.md) explains why, and
§2 of it compares Nexus with Airflow, Prefect, Temporal, Dagster, Argo, Flyte
and others.

## What Nexus does not promise

- **Exactly-once execution.** Nexus provides at-least-once execution and the
  tools to make effects idempotent. See [retries and idempotency](retries-idempotency.md).
- **Production readiness.** The project has not passed its production
  acceptance gates. The [design README](repo:docs/README.md) lists the known gaps.
- **Feature parity with other orchestrators.** The
  [migration guides](../migrate/index.md) list what does not translate.
