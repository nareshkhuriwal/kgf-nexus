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
