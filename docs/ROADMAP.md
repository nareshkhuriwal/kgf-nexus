# Nexus roadmap

*As of 2026-09-26, `main` at `82b9ec8`.* This roadmap is built from
[ARCHITECTURE.md §11](ARCHITECTURE.md#11-phased-plan) (the phased plan), the
[ADRs](adr/README.md) and the open items in
[CONTRACT-FINDINGS.md](../CONTRACT-FINDINGS.md). It is a statement of
direction, not a commitment to dates. Nexus is delivered as the `KGF.Nexus`
service of KGF Cloud ([ADR-0033](adr/0033-nexus-as-kgf-nexus-service.md)). **Nexus is not production-ready.** It has
not passed its production acceptance gates ([docs/README.md](README.md#current-state)).
"Shipped" below means *merged to `main` with tests*, not *proven in production*.

Status words:

- **Shipped:** on `main`, with the tests and documentation named.
- **In progress:** work exists on a named branch that is **not merged**. It may
  change or be dropped in review, and every ADR on these branches is
  *Proposed — pending owner sign-off*.
- **Planned:** in the phased plan or an ADR, with no branch yet.

---

## Shipped

### Core orchestration (Phase 1, ARCHITECTURE.md §11)

| Capability | Reference |
|---|---|
| Declarative `WorkflowSpec` (JSON Schema), compiled from the Python SDK or written as YAML. The control plane never runs user code. | ADR-0004 |
| Six control-plane services on one PostgreSQL schema with table ownership: gateway, workflow manager, DAG engine, scheduler, resource manager, event service | ADR-0011 |
| Atomic dispatch claim: attempt CAS, lease, capacity reservation and event in one transaction. `SKIP LOCKED` across replicas, no leader election. | ADR-0002, ARCHITECTURE §5.3 |
| At-least-once execution with platform-derived idempotency keys and lease fencing. `LOST` distinct from `FAILED`. | ADR-0006, ADR-0010 |
| Pull-based Go worker agent with subprocess, Docker and Kubernetes executors; local write-ahead log; bounded uninterruptible tasks | ADR-0005, ADR-0007, ADR-0015, F96 |
| Resource-aware placement (CPU, memory, GPU count, custom resources) | ARCHITECTURE §7.3 |
| Retries with backoff and `retry_on`; operator retry of a failed run from selected nodes | OPERATIONS §18, F100 |
| Result store (S3-compatible, content-addressed) with a measured 64 KiB inline threshold; artifact backup and garbage collection | ADR-0008, ADR-0016, OPERATIONS §7 |
| Phase 1 exit criteria met: 10-node mixed DAG, exactly-one-logical-effect under worker SIGKILL, no double dispatch across scheduler replicas | CONTRACT-FINDINGS F80, F82, F83; [BENCHMARKS.md §4](BENCHMARKS.md#4-chaos-suite) |

### Phase 2–3 items already delivered

| Capability | Reference |
|---|---|
| NATS JetStream through a transactional outbox (a hint bus; losing it costs latency only), with per-tenant NKey authorization | ADR-0003, F94, OPERATIONS §11 |
| Cron schedules with timezones, catch-up, and backfills with their own concurrency | `internal/cron`, `services/workflow-manager/schedules` |
| Dynamic expansion (`max_fanout`), memoization, worker quarantine | ARCHITECTURE §5.4, §6.4, §5.5, F97 |
| Weighted fair share across namespaces; priority + aging within one | ADR-0017 |
| Windowed pull claim: claim cost no longer grows with backlog on the pull path | ADR-0018 Stage 1, OPERATIONS §15 |
| Namespace quotas enforced exactly in the claim; append-only usage ledger with a `nexus.usage.recorded` export | ADR-0021, OPERATIONS §21 |
| Customer tenancy: registration, one namespace per tenant, `tenant_id` on every owned table, email verification and password recovery | MULTITENANCY.md |
| Namespace-scoped module RBAC, API keys, scoped tokens (including worker tokens) | ADR-0012, F114, RBAC-IMPLEMENTATION.md |
| Durable task logs through the result store | ADR-0020, OPERATIONS §19 |
| Prometheus metrics for every service and the agent | ADR-0019, OPERATIONS §17 |
| Inter-service TLS/mTLS with hot reload and agent enrolment | TLS.md |
| Gateway rate limiting | OPERATIONS §16 |
| Backup, restore and point-in-time recovery (pgBackRest), with drills recorded | OPERATIONS §2–§4, `docs/drills/` |
| Helm chart (HPA, PDBs, cluster controller, artifact GC CronJob) and Terraform for AKS/EKS/GKE. **Terraform is validated offline and has never been applied** (F115). | OPERATIONS §20, CLUSTER-SCALING.md |
| Local runner and attempt replay (`nexusctl run --local`) | ADR-0022 |
| Operator console: overview, workflow catalog, run explorer with DAG, attempts and logs, schedules and backfills, workers, administration | `ui/` |
| Compatibility policy and a previous-release schema skew test | COMPATIBILITY.md |

---

## In progress (branches, not merged)

| Theme | Branch | Proposal | Scope, as proposed |
|---|---|---|---|
| **SSO and RBAC** | `feat/sso-and-rbac` | ADR-0024 | Tenant OIDC (code + PKCE) and SAML 2.0, IdP group-to-role sync, service accounts, token ceilings, granular identity permissions |
| **Cost management** | `feat/cost-management` | ADR-0023 | Effective-dated price books, cost rated from the usage ledger, budgets enforced at the quota points |
| **Secrets, audit and air-gap** | `feat/secrets-audit-airgap` | ADR-0025 | Agent-side secret resolution (env, file, Vault KV v2, Azure Key Vault), hash-chained exportable audit with retention and legal hold, digest-pinned air-gap bundle |
| **OpenTelemetry and storage backends** | `feat/otel-and-storage-backends` | ADR-0026/0027 (reserved) | Result-store interface with S3, Azure Blob and GCS backends, and a tracing package. Addresses F115 item 1. |
| **Events, triggers and notifications** | `feat/events-triggers-notifications` | ADR-0028 | Notification channels, signed inbound webhook triggers, sensors, and declared asset lineage, driven from the outbox |
| **Docs site and converters** | `feat/docs-site-and-converters` | — | OpenAPI 3.1 description served at `/v1/openapi.yaml` and checked against the routes; template fixes (F144); documentation site |
| **GPU and multi-cluster scheduling** | `feat/gpu-and-multicluster-scheduling` | ADR-0029, ADR-0030, ADR-0031 | Fractional GPUs (MIG, MPS or time-sliced) with packing; spot capacity with eviction handling and a model cache; multi-cluster placement as claim-time eligibility and scoring |
| **AI workload pack** | `feat/ai-workload-pack` | ADR-0032 (reserved) | AI and LLM workload support, with token usage as ledger custom units |

Fix branches in the same wave: `fix/control-plane-open-bugs` (F104 residual,
F109, F119, F125), `fix/f115-chart-db-hardening` (database `verify-full`,
per-service runtime logins, ServiceAccounts and GPU agents in the chart) and
`fix/tooling-and-ci-gaps`.

---

## Open findings (CONTRACT-FINDINGS.md) on `main`

| Finding | Summary | Being addressed on |
|---|---|---|
| F109 (partly open) | A run cancel could skip attempts being started or dispatched. The remainder is still open. | `fix/control-plane-open-bugs` |
| F115 | Cloud platforms: S3-only result store with static keys; the chart lacks cloud identities and GPU pools; nothing applied to a real cloud | `feat/otel-and-storage-backends`, `fix/f115-chart-db-hardening`. A real cloud apply is still planned. |
| F119 | Backfill pause, resume and cancel have no from-state guard | `fix/control-plane-open-bugs` |
| F125 | The agent does not enforce a declared output `max_bytes` | `fix/control-plane-open-bugs` |

The early contract-review findings (F1–F60) that carry no status marker were not
re-audited for this roadmap.

---

## Planned

From ARCHITECTURE.md §11 phases 2–4, the ADRs' "not yet" sections and the
production brief. Order is indicative.

**Production acceptance (before any "production-ready" claim)**
- First real cloud deployment: apply per cloud in a scratch account, run a
  >64 MiB fan-out/fan-in artifact, the GC sweep and a managed-database PITR
  restore (F115 item 6).
- Dispatch capacity measured on production-class hardware with the ADR-0018
  harness. Every figure in [BENCHMARKS.md](BENCHMARKS.md) is a laptop figure.
- N-1 agent against an N control plane tested with real binaries
  (COMPATIBILITY.md §4.3), and an agent capability list.
- GPU execution qualified on real hardware.

**Scale and expressiveness (Phase 3)**
- ADR-0018 Stage 2 (batched claims) and a windowed push path. Stage 3 (a
  dedicated dispatch path) only if the ADR's trigger fires.
- Sub-workflows and workflow-of-workflows.
- Conditional edges (sandboxed CEL) and the full trigger-rule set.
- Typed interface validation and lineage beyond ADR-0028's first layer.
- Preemption.
- TypeScript and Go SDKs generated from `proto/`.

**Platform maturity (Phase 4)**
- Asset layer over the task core.
- Pluggable capacity providers for autoscaling (the cluster controller is the
  start, CLUSTER-SCALING.md).
- Multi-region control plane.
- Optional durable-execution authoring mode.

**Shipping as a KGF Cloud service (design only)**

Nexus ships as the `KGF.Nexus` service of KGF Cloud, not as a separate
hosted product ([ADR-0033](adr/0033-nexus-as-kgf-nexus-service.md),
[KGF-INTEGRATION.md](KGF-INTEGRATION.md)). The merge plan is in
KGF-INTEGRATION.md §13 and is carried out service by service:

1. `X-KGF-Context` authentication in the Nexus gateway. Another branch is
   implementing it; it is not merged.
2. The subscription-to-namespace scope mapping.
3. Database posture: per-service runtime logins, then a row-level security
   decision.
4. The engine deployed in the KGF platform cluster.
5. The KGF provider front replacing the simulated `kgf-cloud:services/nexus`.
6. Quotas set in KGF.
7. Usage exported to kgf-billing.
8. Audit exported to KGF.
9. The portal extension.
10. Acceptance.
11. Removal of the features KGF owns.

Items 6–8 need contracts that KGF Cloud does not have yet.

Consequence for the branches above: SSO/RBAC, cost management and audit
sinks duplicate KGF identity, billing and audit export. Whether they merge
at all is an open owner decision (KGF-INTEGRATION.md OPEN-10).

**Operational gaps already documented**
- A shared, not per-replica, rate limiter (OPERATIONS §16.6).
- Certificate revocation and certificate-identity authorization (TLS.md §5.6).
- PostgreSQL row-level security as a backstop (MULTITENANCY.md).
- Tenant suspension, ownership transfer and multi-namespace membership
  (MULTITENANCY.md).
- Metering of result-store artifacts (ADR-0021).
