# Nexus compared with Airflow, Prefect, Argo Workflows and Flyte

*Sources accessed 2026-09-26.* Every statement about another project cites that
project's official documentation or repository (§5). Where the documentation
was silent or unclear, the cell says **not documented** rather than guessing.
Statements about Nexus cite this repository. They describe `main` at
`82b9ec8` unless they are marked *branch* (unmerged; see [ROADMAP.md](ROADMAP.md)).

**Read this first.**

- Nexus is young and **not production-ready** ([docs/README.md](README.md#current-state)).
  The other four are mature projects with large user bases and managed
  offerings. This page compares **semantics and design**, not maturity. On
  maturity, the other four win. §3 says where.
- **There is no like-for-like performance comparison.** None of the four
  publishes official throughput benchmarks (§4). Nexus has only laptop
  measurements of its own dispatch path ([BENCHMARKS.md](BENCHMARKS.md)). This
  page therefore makes no performance claim relative to any of them.
- Feature parity with these projects is not claimed. Nexus borrows specific
  ideas from each ([ARCHITECTURE.md §2](ARCHITECTURE.md#2-what-we-take-from-prior-art-and-what-we-reject)).

---

## 1. At a glance

| | Nexus | Apache Airflow | Prefect | Argo Workflows | Flyte |
|---|---|---|---|---|---|
| Version checked | 0.1.0, pre-release, no tags ([COMPATIBILITY.md §1](COMPATIBILITY.md)) | 3.3.2 (2026-09-17) [A1] | 3.8.6 (2026-09-14) [P1] | v4.1.4 (2026-09-18) [G1] | Flyte 2 GA; backend v2.0.50, SDK 2.10.1 [F1] |
| Licence | Apache-2.0 | Apache-2.0 [A2] | Apache-2.0 [P2] | Apache-2.0, CNCF graduated [G2] | Apache-2.0, LF AI & Data graduated [F2] |
| Authoring | Python SDK or YAML, compiled to a declarative JSON `WorkflowSpec`. The control plane never runs user code (ADR-0004). | Python DAG files parsed by the scheduler; top-level code runs on every parse [A3] | Plain Python `@flow`/`@task`; control flow at runtime [P3] | Kubernetes CRD (YAML); Hera Python SDK [G3] | Pure Python; tasks call tasks, with async fan-out [F3] |
| Kubernetes required | No: subprocess, Docker or Kubernetes executors (ADR-0007) | No [A4] | No [P4] | **Yes** [G3] | **Yes** [F4] |
| State store | PostgreSQL only, which is also the dispatch queue (ADR-0002) | PostgreSQL or MySQL [A5] | SQLite or PostgreSQL; HA needs PostgreSQL + Redis [P5] | etcd (CRD status); PostgreSQL/MySQL for offload and archive [G4] | PostgreSQL + object store [F5] |
| Worker connectivity | Agents pull over outbound gRPC and hold no DB credentials (ADR-0005, ARCHITECTURE §3.1) | Airflow 3 workers talk to the API server, not the DB; Edge workers need only HTTPS [A4] | Pull work pools (worker on your infra), push (serverless) or managed [P4] | Controller creates pods in-cluster [G3] | Executor reconciles task pods in-cluster [F4] |
| Managed offering | none (planned as a KGF Cloud service, design only) | Astronomer, MWAA, Cloud Composer and others [A12] | Prefect Cloud [P12] | Akuity, Pipekit [G12] | Union.ai [F12] |

## 2. Execution semantics

| | Nexus | Airflow | Prefect | Argo Workflows | Flyte |
|---|---|---|---|---|---|
| Delivery guarantee (as documented) | At-least-once, stated explicitly; exactly-once side effects are disclaimed (ADR-0006) | Retries re-run tasks; tasks "should produce the same outcome on every re-run" [A6] | Retries; transactions run "at most one time" per cache key, with rollback hooks [P6] | `retryStrategy` re-runs the step [G6] | Retries run in a new pod; "task logic should be idempotent" [F6] |
| Idempotency key | **Platform-derived**, identical across retries, passed to the task as `NEXUS_IDEMPOTENCY_KEY` (ADR-0006) | Not documented; left to the user [A6] | User-supplied run `idempotency_key`; cache keys for task results [P6] | Not documented | Cache key (inputs + version) serves memoization; not documented as an effect key |
| Stale-worker protection | Leases with fencing tokens: a report from a superseded attempt is rejected (ADR-0010) | Heartbeat timeout: task failed or retried [A6] | Heartbeats + automation mark runs Crashed; managed in Cloud, user-configured when self-hosted [P6] | Kubernetes pod lifecycle | System retries for infrastructure failures, separate budget [F6] |
| Unknown outcome vs failure | Distinct `LOST` state, separate from `FAILED` (invariant 2, ARCHITECTURE §5.1) | Failed or retry [A6] | `CRASHED` state for infrastructure issues [P6] | `OnError` / `OnTransientError` retry policies [G6] | System vs user retries [F6] |
| Timeout model | Four timeouts: schedule-to-start, start-to-close, schedule-to-close, heartbeat (ARCHITECTURE §4.3) | Execution timeout, heartbeat timeout [A6] | Timeouts on flows and tasks [P6] | not checked for this page | `max_queued_time`, `max_runtime`, `deadline` [F6] |
| Dynamic fan-out | Dynamic expansion with `max_fanout` and a submit-time fan-in bound (ARCHITECTURE §5.4) | `expand()`; `max_map_length` default 1024 [A3] | `.map()`, `.submit()` [P3] | `withParam` over runtime JSON [G3] | `flyte.map`, `asyncio.gather` [F3] |
| Memoization / caching | Keyed on task version + input hash (ARCHITECTURE §6.4) | Not documented natively [A8] | Cache policies; needs result persistence [P8] | Per-step, in ConfigMaps, 1 MB limit [G8] | Versioned cache, off by default [F8] |

## 3. Scheduling, tenancy and operations

| | Nexus | Airflow | Prefect | Argo Workflows | Flyte |
|---|---|---|---|---|---|
| Cron + timezone | Yes (`internal/cron`) | Yes, DST-aware for cron [A7] | Cron, interval, RRule with timezone [P7] | CronWorkflow with timezone, concurrency policy [G7] | `flyte.Cron` with timezone [F7] |
| Backfill | Yes, with its own concurrency; F119 guards on a branch | Scheduler-managed backfills with reprocess modes [A7] | Not documented [P7] | No native backfill; documented pattern [G7] | No native backfill; user task pattern [F7] |
| Event triggers | *Branch* (ADR-0028: webhooks, sensors, lineage) | Asset-aware scheduling, AssetWatcher [A7] | Events, automations; webhooks in Cloud [P7] | Argo Events, 20+ sources [G7] | Webhook and artifact triggers "coming" [F7] |
| Tenancy model | Tenant = namespace; `tenant_id` on every owned table; logical isolation, no RLS ([MULTITENANCY.md](MULTITENANCY.md)) | Multi-Team is experimental and "not designed for complete tenant separation" [A9] | Workspaces, RBAC, SSO in Prefect Cloud, not OSS [P9] | Kubernetes namespaces + RBAC, SSO→service accounts [G9] | Organization / project / domain [F9] |
| Quotas and concurrency | Per-namespace quotas enforced exactly in the claim; weighted fair share across namespaces (ADR-0021, ADR-0017) | Pools and slots; per-task limits [A9] | Global and tag-based concurrency limits [P9] | Parallelism per controller, namespace, workflow and template; semaphores [G9] | Concurrency controls; Flyte 1 documents K8s ResourceQuota [F9] |
| Resource-aware placement | CPU, memory, GPU count and custom resources matched against advertised worker capacity (design goal G3, ARCHITECTURE §1.2); fractional GPU on a *branch* (ADR-0029) | Via executor, e.g. K8s `pod_override` [A10] | Via infrastructure job variables [P10] | Kubernetes requests, DRA in v4.1 [G10] | `Resources(gpu="A10G:1")` [F10] |
| Usage metering | Append-only per-attempt ledger with an export event (ADR-0021) | Not documented | Not documented in OSS | Not documented | Not documented |
| Observability | Prometheus metrics (ADR-0019); OpenTelemetry on a *branch* | StatsD or OpenTelemetry [A11] | UI; OTel not documented [P11] | Prometheus/OTel metrics; tracing "not finished" [G11] | OTel plugin [F11] |
| Backup / PITR | Documented and drilled on a laptop (OPERATIONS §4.6) | Database-level (the operator's own) | Database-level | etcd + archive DB | Database + object store |

### Where the others are stronger

This is not a complete list. It covers what an evaluator should weigh first.

- **Maturity and adoption.** Airflow (~47k GitHub stars), Prefect (~24k), Argo
  (~17k, CNCF graduated) and Flyte (~7.6k, LF AI & Data graduated) have years
  of production use [A2, P2, G2, F2]. Nexus has no production deployments and
  no releases.
- **Integrations.** Airflow has "more than 80" community providers [A14].
  Prefect has integration packages [P14]. Argo pairs with Argo Events, CD and
  Rollouts [G14]. Flyte has connectors [F4]. Nexus has an executor interface
  and no integration library.
- **Managed services.** All four can be bought as a hosted product today. Nexus
  is planned as the `KGF.Nexus` service of KGF Cloud. That is a design, not a
  running service ([KGF-INTEGRATION.md](KGF-INTEGRATION.md)).
- **Authoring ergonomics.** Prefect and Flyte 2 run plain Python with runtime
  control flow. Nexus deliberately compiles to a static spec plus dynamic
  expansion. That is safer for the control plane, and less flexible for authors.
- **Event-driven scheduling.** Airflow assets, Prefect automations and Argo
  Events exist and are documented. Nexus's equivalent is on a branch.
- **Kubernetes-native operation.** For teams that live in Kubernetes, Argo
  and Flyte reuse the cluster's scheduler, quotas and RBAC directly. Nexus runs
  its own scheduler and capacity ledger, even when using the Kubernetes executor.
- **SDKs.** Airflow 3.3 adds Java and Go Task SDKs [A1]. Nexus has only a Python SDK.
- **Horizontal scale evidence.** Nexus's dispatch path has a measured,
  backlog-dependent ceiling on one PostgreSQL (ADR-0018). Whether it matches
  the others at scale is **unknown**, because no comparable measurement exists.

### Where Nexus's design differs on purpose

- A platform-derived idempotency key, lease fencing and a distinct `LOST`
  state, all stated in the contract. The chaos suite checks them (exactly one
  logical effect after SIGKILL, [BENCHMARKS.md §4](BENCHMARKS.md#4-chaos-suite)).
- No user code in the control plane, and no database credentials on workers.
- Outbound-only workers without Kubernetes, which suits edge and mixed fleets.
  (Airflow's Edge executor is the closest analogue [A4].)
- Namespace quotas that stay exact with many scheduler replicas, plus a
  billing-grade usage ledger.

---

## 4. Benchmarks

**No like-for-like benchmark exists, so none is shown.** Official
documentation publishes:

| Project | Official throughput figure | Source |
|---|---|---|
| Airflow | None. The scheduler "scales almost linearly with several instances". | [A13] |
| Prefect | None found | — |
| Argo Workflows | None. Tuning defaults only: controller QPS 20 and burst 30; pod creation 10/s, burst 25. These are **limits, not measurements**. | [G13] |
| Flyte | None in the docs. A vendor blog says "a single process can drive thousands of concurrent tasks", which is not a benchmark. | [F13] |
| Nexus | Claim throughput and latency of its own dispatch transaction on a laptop, with the method and hardware recorded | [BENCHMARKS.md](BENCHMARKS.md) |

A fair comparison would run one workload shape on the same hardware for each
system, measured end to end from submit to completion, with each project's
recommended production configuration. That has not been done. Nexus's numbers
measure one internal transaction, not end-to-end workflow latency, so they
**cannot** be compared with any figure the other projects might publish.

---

## 5. Sources

All accessed 2026-09-26.

**Airflow**
- [A1] Release notes: https://airflow.apache.org/docs/apache-airflow/stable/release_notes.html
- [A2] Repository: https://github.com/apache/airflow
- [A3] Best practices: https://airflow.apache.org/docs/apache-airflow/stable/best-practices.html. Dynamic task mapping: https://airflow.apache.org/docs/apache-airflow/stable/authoring-and-scheduling/dynamic-task-mapping.html
- [A4] Executors: https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/executor/index.html. Edge: https://airflow.apache.org/docs/apache-airflow-providers-edge3/stable/index.html. Upgrading to Airflow 3: https://airflow.apache.org/docs/apache-airflow/stable/installation/upgrading_to_airflow3.html
- [A5] Database: https://airflow.apache.org/docs/apache-airflow/stable/howto/set-up-database.html
- [A6] Tasks: https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/tasks.html, and [A3] best practices
- [A7] Timezones: https://airflow.apache.org/docs/apache-airflow/stable/authoring-and-scheduling/timezone.html. Backfill: https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/backfill.html. Assets: https://airflow.apache.org/docs/apache-airflow/stable/authoring-and-scheduling/asset-scheduling.html
- [A8] No native task-output memoization found in the pages above
- [A9] Multi-Team: https://airflow.apache.org/docs/apache-airflow/stable/core-concepts/multi-team.html. Pools: https://airflow.apache.org/docs/apache-airflow/stable/administration-and-deployment/pools.html
- [A10] Kubernetes executor: https://airflow.apache.org/docs/apache-airflow-providers-cncf-kubernetes/stable/kubernetes_executor.html
- [A11] Metrics: https://airflow.apache.org/docs/apache-airflow/stable/administration-and-deployment/logging-monitoring/metrics.html
- [A12] Ecosystem: https://airflow.apache.org/ecosystem/
- [A13] Scheduler: https://airflow.apache.org/docs/apache-airflow/stable/administration-and-deployment/scheduler.html
- [A14] Providers: https://airflow.apache.org/docs/apache-airflow-providers/

**Prefect**
- [P1] Releases: https://github.com/PrefectHQ/prefect/releases
- [P2] Repository: https://github.com/PrefectHQ/prefect
- [P3] Flows and tasks: https://docs.prefect.io/v3/concepts/flows, https://docs.prefect.io/v3/concepts/tasks
- [P4] Work pools: https://docs.prefect.io/v3/concepts/work-pools
- [P5] Server: https://docs.prefect.io/v3/concepts/server. Self-hosted: https://docs.prefect.io/v3/advanced/self-hosted
- [P6] Retries: https://docs.prefect.io/v3/how-to-guides/workflows/retries. States: https://docs.prefect.io/v3/concepts/states. Zombie flows: https://docs.prefect.io/v3/advanced/detect-zombie-flows. Transactions: https://docs.prefect.io/v3/advanced/transactions
- [P7] Schedules: https://docs.prefect.io/v3/concepts/schedules. Automations: https://docs.prefect.io/v3/concepts/automations. Webhooks: https://docs.prefect.io/v3/concepts/webhooks. No backfill feature found.
- [P8] Caching: https://docs.prefect.io/v3/concepts/caching
- [P9] Global concurrency limits: https://docs.prefect.io/v3/concepts/global-concurrency-limits. Tag-based limits: https://docs.prefect.io/v3/concepts/tag-based-concurrency-limits. Server features: [P5]
- [P10] Work pools [P4]
- [P11] No documented OpenTelemetry feature found
- [P12] Pricing: https://www.prefect.io/pricing
- [P14] Docs index: https://docs.prefect.io/llms.txt

**Argo Workflows**
- [G1] Releases: https://github.com/argoproj/argo-workflows/releases. New features: https://argo-workflows.readthedocs.io/en/latest/new-features/
- [G2] Repository: https://github.com/argoproj/argo-workflows
- [G3] Docs home: https://argo-workflows.readthedocs.io/en/latest/. Loops: https://argo-workflows.readthedocs.io/en/latest/walk-through/loops/
- [G4] Offloading: https://argo-workflows.readthedocs.io/en/latest/offloading-large-workflows/. Archive: https://argo-workflows.readthedocs.io/en/latest/workflow-archive/
- [G6] Retries: https://argo-workflows.readthedocs.io/en/latest/retries/
- [G7] Cron workflows: https://argo-workflows.readthedocs.io/en/latest/cron-workflows/. Cron backfill: https://argo-workflows.readthedocs.io/en/latest/cron-backfill/. Argo Events: https://argoproj.github.io/argo-events/
- [G8] Memoization: https://argo-workflows.readthedocs.io/en/latest/memoization/
- [G9] SSO: https://argo-workflows.readthedocs.io/en/latest/argo-server-sso/. Parallelism: https://argo-workflows.readthedocs.io/en/latest/parallelism/. Synchronization: https://argo-workflows.readthedocs.io/en/latest/synchronization/
- [G10] New features [G1]
- [G11] Metrics: https://argo-workflows.readthedocs.io/en/latest/metrics/. Tracing: https://argo-workflows.readthedocs.io/en/latest/tracing/
- [G12] Akuity: https://akuity.io/argo-enterprise. Pipekit: https://pipekit.io/
- [G13] Scaling: https://argo-workflows.readthedocs.io/en/latest/scaling/
- [G14] The Argo project: https://argoproj.github.io/

**Flyte**
- [F1] Releases: https://github.com/flyteorg/flyte/releases, https://github.com/flyteorg/flyte-sdk/releases. GA announcement (vendor blog): https://www.union.ai/blog-post/flyte-2-is-generally-available-the-durable-open-source-ai-runtime
- [F2] Repository: https://github.com/flyteorg/flyte
- [F3] Get started: https://www.union.ai/docs/v2/flyte/user-guide/get-started. Map: https://www.union.ai/docs/v2/flyte/user-guide/tasks/task-programming/map
- [F4] Deployment: https://www.union.ai/docs/v2/flyte/oss-deployment. Overview: https://www.union.ai/docs/v2/flyte/oss-deployment/overview. Connectors: https://www.union.ai/docs/v2/flyte/api-reference/flyte-sdk/flyte.connectors
- [F5] Where data lives: https://www.union.ai/docs/v2/flyte/user-guide/get-started/core-concepts/where-data-lives
- [F6] Retries and timeouts: https://www.union.ai/docs/v2/flyte/user-guide/tasks/task-configuration/retries-and-timeouts. Interruptible: https://www.union.ai/docs/v2/flyte/user-guide/tasks/task-configuration/interruptible-tasks-and-queues
- [F7] Triggers: https://www.union.ai/docs/v2/flyte/user-guide/triggers. Backfill: a user-task pattern in the migration guide (exact page not pinned)
- [F8] Caching: https://www.union.ai/docs/v2/flyte/user-guide/tasks/task-configuration/caching
- [F9] Projects and domains: https://www.union.ai/docs/v2/flyte/user-guide/get-started/core-concepts/projects-and-domains. Flyte 1 quotas: https://docs.flyte.org/en/latest/deployment/configuration/customizable_resources.html. Quotas for Flyte 2 OSS are not documented.
- [F10] User guide, resources: https://www.union.ai/docs/v2/flyte/user-guide/
- [F11] OpenTelemetry: https://www.union.ai/docs/v2/flyte/integrations/opentelemetry
- [F12] Union.ai: https://www.union.ai/
- [F13] GA blog [F1]

Figures that are counts (stars, providers) move daily and are approximate as of
the access date.
