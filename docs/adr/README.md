# Architecture decision records

Records for the Nexus distributed workflow orchestration platform. All are
**Proposed** as of 2026-09-12 and document the Phase 0 design; none implies
implementation approval. See [ARCHITECTURE.md](../ARCHITECTURE.md) for the
full system design, data model, state machines, and phased plan.

| ADR | Decision | Key trade-off accepted |
| --- | --- | --- |
| [0001](0001-monorepo-and-language-split.md) | Monorepo; Go control plane + agent, Python SDK, React UI | Two toolchains in one repo, for a mechanically-enforced plane contract |
| [0002](0002-postgres-as-metadata-store.md) | Postgres as sole source of truth, including the dispatch queue | A throughput ceiling, for atomic dispatch without distributed transactions |
| [0003](0003-nats-as-event-bus.md) | NATS + JetStream for notification and fan-out only | A second system, for sub-second dispatch without making the bus authoritative |
| [0004](0004-declarative-workflow-spec.md) | Declarative `WorkflowSpec`; SDK compiles to it, never runs in the control plane | No arbitrary control flow, for a control plane user code cannot destabilize |
| [0005](0005-pull-based-dispatch.md) | Workers pull; directives ride on heartbeat responses | Poll-interval latency in degraded mode, for topology freedom and free backpressure |
| [0006](0006-at-least-once-with-idempotency.md) | At-least-once execution with platform-derived idempotency keys | Users must write idempotent handlers, for unattended progress through failure |
| [0007](0007-executor-abstraction.md) | Executor is an interface inside the agent (subprocess / docker / k8s) | Duplicated per-executor effort, for a substrate-agnostic control plane |
| [0008](0008-result-store-separation.md) | Task payloads in an S3-compatible store; metadata holds references | A second failure domain, for control-plane performance decoupled from payload size |
| [0009](0009-no-ray-dependency.md) | No Ray dependency; borrow its resource-vector model only | We build the agent ourselves, for polyglot support and one scheduler |
| [0010](0010-lease-based-task-ownership.md) | Time-bounded leases with fencing tokens | Renewal overhead and TTL tuning, for no stuck attempts and no state corruption |
| [0011](0011-microservice-decomposition.md) | Six deployable control-plane services, shared schema with table ownership | Shared-schema coupling, for independent deploy without losing atomic dispatch |
| [0012](0012-module-permissions-and-role-bindings.md) | Namespace-scoped roles and module permissions | Migration and UI rollout cost, for least-privilege administration |
| [0013](0013-go-control-plane-and-agent.md) | Go stays the control-plane and agent language (§12 Q1) | A larger agent binary than Rust, for one toolchain and team velocity |
| [0014](0014-worker-transport-grpc-without-json-fallback.md) | Worker transport stays gRPC; proxy requirements documented, no JSON fallback yet (§12 Q2) | HTTP/1.1-only networks unsupported, for one auth and fencing path |
| [0015](0015-bounded-uninterruptible-tasks.md) | Tasks may declare `uninterruptible`, bounded by min(spec, platform ceiling) (§12 Q3) | Cancel is no longer prompt for those tasks, for no half-applied side effects |
| [0016](0016-inline-result-threshold.md) | Inline-vs-blob threshold stays 64 KiB, configurable within [1 B, 1 MiB] (§12 Q4) | Payload bytes in Postgres, for no object-store round trip on small results |
| [0017](0017-weighted-fair-share-across-namespaces.md) | Weighted fair share across namespaces; priority + aging within one (§12 Q5) | An extra index walk per claim, for no cross-tenant starvation |
| [0018](0018-postgres-dispatch-ceiling.md) | Postgres stays the dispatch queue; measured ceiling, trigger and migration path (§12 Q6) | A known, backlog-dependent ceiling, for keeping atomic dispatch |
| [0019](0019-metrics-exposition.md) | Metrics exposition via client_golang (pinned) behind the in-house instrument API | A new dependency tree, for spec-compliant exposition and runtime/process collectors |
| [0020](0020-durable-task-logs.md) | Task logs: agents write chunks to the result store, index them through a lease-fenced RPC; gateway-owned index | Seconds of live-tail latency and gateway read credentials, for no log bytes through the control plane |
| [0021](0021-namespace-quotas-and-usage-metering.md) | Namespace quotas enforced in the claim under a per-tenant advisory lock; append-only, scheduler-owned usage ledger written with each terminal transition | Serialized dispatch for quota-bound namespaces and one lookup per claim, for exact quotas and complete metering |
| [0022](0022-local-runner-and-replay.md) | Local runner (`nexus.local`, `nexusctl run --local`) and attempt replay reuse the DAG semantics, held to Go by shared fixtures | A second implementation of the decision logic kept aligned by tests, for a seconds-long local loop |
| [0024](0024-nexus-inside-kgf-cloud.md) | Nexus inside KGF Cloud: verify the gateway-minted Ed25519 `X-KGF-Context`, map each KGF organization/subscription/resource group to one Nexus tenant namespace provisioned on first use, map KGF roles and `KGF.Nexus/*` actions onto the Nexus catalog; `NEXUS_AUTH_MODE=standalone\|kgf\|both` | Nexus trusts KGF's signing key and flat role for humans, for one sign-in and one audit trail across KGF |
| [0025](0025-secret-resolution-audit-export-airgap.md) | Agent-side per-tenant secret resolution (env, file, Vault KV v2, Azure Key Vault) with declared env/file injection; paginated, exported, sink-delivered, hash-chained audit with retention and legal hold, fail-closed for security actions; digest-pinned air-gap bundle | Secret values exist on worker hosts, and security administration blocks while the audit store is down, in exchange for no credentials in the metadata store and a verifiable audit trail |
| [0026](0026-opentelemetry-tracing.md) | OpenTelemetry tracing (pinned SDK, standard OTEL_* config, off by default, parent-based ratio); W3C context carried over HTTP, gRPC, the outbox, the attempt row and into task processes | A dependency tree and one additive migration, for one trace per run and trace ids on log lines |
| [0027](0027-result-store-backends.md) | One result-store interface with native S3 (credential chain), Azure Blob and GCS backends, held to one conformance suite | Two cloud SDK trees and emulator-only verification until each cloud is applied, for keyless native storage on every cloud |
| [0028](0028-event-triggers-notifications-sensors-lineage.md) | Notifications, signed webhook triggers, sensors and declared asset lineage, all driven from the transactional outbox with `SKIP LOCKED` claims and unique event keys | At-least-once delivery to channels without a dedup primitive and a shared secret key, for no new infrastructure and no leader election |
| [0029](0029-gpu-sharing-and-packing.md) | MIG profiles and shared (MPS/time-sliced) GPUs as first-class, per-device-subtracted resources; a placement score in the claim (push: best worker; pull: bounded delay scheduling) | Larger resource vectors and up to a few seconds' deferral, for exact fractional-GPU accounting and packing without a new lock |
| [0030](0030-spot-capacity-and-model-cache.md) | Spot capacity type as a label, task capacity preference, pluggable eviction sources that checkpoint-signal and report PREEMPTED against a separate retry budget; node-local LRU artifact cache with integrity checks and soft cache affinity | Dependence on the eviction notice arriving, for spot use that does not burn task budgets |
| [0031](0031-multi-cluster-placement.md) | Multi-cluster placement as claim-time eligibility (required labels, cost ceiling, per-cluster quota headroom) plus scoring (data locality); no push scheduler | Per-poll rather than global placement, for keeping the pull model |
| [0032](0032-ai-workloads-usage-rate-limits-checkpoints.md) | A per-attempt agent socket (the workload API) for usage units recorded into the ledger's `custom` map, lease-fenced shared token-bucket permits outside the claim, forward-only checkpoint pointers, and uncharged RATE_LIMITED retries | A permit round trip per provider call and a self-reported usage record, for fleet-wide provider limits, token metering and mid-task resume |
| [0033](0033-nexus-as-kgf-nexus-service.md) | Nexus ships as the KGF.Nexus service of KGF Cloud: a KGF provider front over the Nexus engine, `X-KGF-Context` authentication, namespace per KGF resource group (owner decision), KGF-set quotas enforced in the claim, usage and audit exported to KGF, portal extension; workers stay Nexus gRPC agents | Row-level security, database-role and KGF ingest-contract gaps to close, for one platform around the engine instead of two |
| [0034](0034-kgf-nexus-workspaces.md) | KGF.Nexus workspaces: an optional `res` claim naming one workspace, assertable only by KGF provider fronts (`NEXUS_KGF_FRONT_ISSUERS`); one namespace per workspace (`kgf-nexus/v2` derivation, `kgf_workspaces`), provisioned and deleted explicitly by the front's service context, never lazily; delete revokes and retains | A deleted workspace's name cannot be reused, and keys are not bound to issuers, for final deletes and several isolated workspaces per resource group |
| [0036](0036-scheduler-autoscaling-applied-by-the-controller.md) | The cluster controller applies the saved scheduler settings (min/max replicas, CPU target, scale-down window) to scheduler HPAs that opt in with `nexus.khuriwalgroup.com/autoscaling-owner`, in plan or apply mode: UID-pinned, compare-and-swap JSON patches of only those four fields, bounded steps at most one a minute, audited as `hpa_*` events under the `hpa/<name>` target; no migration | A chart- or kubectl-managed owned HPA is reverted by each re-apply until the next patch, and two kinds of target share 0018's tables, for saved settings that take effect without a redeploy while GitOps-owned HPAs are left alone |

ADRs 0013–0018 close the six open questions of ARCHITECTURE.md §12 and are
**Proposed — pending owner sign-off**.

## Conventions

Each record states context, the decision, consequences (positive, negative,
neutral), and alternatives considered **with the reason for rejection**. A
record that lists no negative consequences is incomplete.

Supersede an accepted decision with a new record rather than rewriting
history. While everything is still Proposed, edit in place and let version
control carry the revisions.

## Reading order

For the execution-semantics core, read **0006** then **0010** together — they
are two halves of one mechanism (idempotency handles duplicate side effects;
fencing handles stale state). For the structural core, read **0002** then
**0004**.
