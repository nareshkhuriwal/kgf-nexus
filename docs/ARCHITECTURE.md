# Nexus — Distributed Workflow Orchestration Platform

**Status:** Implemented and under active development. This began as the Phase 0 design proposal; the control-plane services, worker agent, CLI, Python SDK, UI and schema migrations now exist in this repository. Where this document describes something not yet built, it says so. Where it disagrees with the code, the code and [CONTRACT-FINDINGS.md](../CONTRACT-FINDINGS.md) are authoritative and this document is the bug. It is **not** a claim of production readiness (see [README.md](README.md#current-state)).
**Audience:** platform engineers, operators, and reviewers.
**Scope of this document:** system architecture, component boundaries, data model, state machines, execution semantics, and a phased delivery plan.

---

## 1. Purpose and non-goals

### 1.1 What Nexus is

Nexus is a **general-purpose, reusable orchestration platform**. It accepts a declarative description of a graph of work (a DAG of tasks), decides when and where each task should run, dispatches it to a pool of worker agents, tracks every state transition durably, and makes the results queryable.

It is deliberately *not* coupled to any particular application domain. The same platform must be usable for ETL pipelines, ML training jobs, batch reporting, infrastructure automation, or long-running business processes. Domain knowledge lives in **task handlers** written by users against the SDK; Nexus itself only understands *graphs, tasks, resources, and states*.

### 1.2 Design goals

| Goal | Meaning in practice |
|---|---|
| **G1. Reusability** | Zero domain types in the core. A workflow is data; a task is a typed contract. Nothing in the control plane imports application code. |
| **G2. Clean plane separation** | The control plane never executes user code. The execution plane never writes to the metadata store. The boundary is a small, versioned API. |
| **G3. Resource awareness** | Scheduling respects declared CPU / memory / GPU requirements against real advertised worker capacity. A task that cannot fit does not get dispatched. |
| **G4. Correctness under failure** | Every state transition is durable and monotonic. Worker death, control-plane restart, and network partition are normal events, not exceptions. |
| **G5. Operational honesty** | At-least-once delivery with mandated idempotency. We state the guarantee we can actually keep, rather than claiming exactly-once. |
| **G6. Boring dependencies** | Postgres for state. NATS for messaging. No bespoke storage engine, no bespoke consensus, no bespoke queue. |

### 1.3 Explicit non-goals

- **Not exactly-once execution.** See §6. We provide at-least-once plus idempotency primitives. Any vendor claiming exactly-once side effects over a network is describing at-least-once with deduplication, and we prefer to say so.
- **Not a Ray replacement.** Nexus orchestrates coarse-grained tasks (seconds to hours), not fine-grained actor-to-actor messaging or shared distributed memory. If you need a distributed object store and tight task fan-out at millisecond latency, that is a different tool. Nexus may *dispatch a task that itself uses* such a framework, but we take no dependency on one. (See [ADR-0009](adr/0009-no-ray-dependency.md).)
- **Not a database or broker.** We reinvent neither. (See [ADR-0002](adr/0002-postgres-as-metadata-store.md), [ADR-0003](adr/0003-nats-as-event-bus.md).)
- **Not a data-asset catalog** in the MVP. Dagster's asset-centric model is attractive and §11.3 discusses layering it on later, but the MVP core is task-centric.
- **Not a general compute autoscaler.** The resource manager tracks and allocates known capacity; provisioning new capacity is delegated to a pluggable provider, deferred past MVP.

---

## 2. What we take from prior art, and what we reject

This section exists because "learn from them, don't copy blindly" requires being explicit about which specific decision we are borrowing and why.

### 2.1 Per-system assessment

**Airflow** — the reference point for scheduler-centric DAG orchestration.
- *Adopt:* the operator/executor split; the idea of a scheduler loop that reconciles DAG state against a database; backfill and catchup as first-class concepts; rich UI around DAG runs and task instances.
- *Reject:* DAGs defined by executing arbitrary Python at parse time in the scheduler process. This makes the scheduler's stability a function of user code quality, causes parse-time coupling, and makes the DAG definition non-portable. Nexus separates **authoring** (SDK, user process) from **submission** (a validated JSON/YAML `WorkflowSpec` sent over the API). See [ADR-0004](adr/0004-declarative-workflow-spec.md).
- *Reject:* the historical single-scheduler bottleneck and the "one row per task instance, polled in a tight loop" pattern. We use event-driven dispatch with a reconciliation loop as a safety net, not as the primary path.

**Prefect** — the reference point for developer ergonomics.
- *Adopt:* the hybrid model where the control plane orchestrates but never sees user code or data; the notion that a task's *result* and a task's *metadata* are stored separately; clean Pythonic decorators.
- *Adopt:* dynamic task generation at runtime (mapping over a collection whose size is unknown until an upstream task finishes). Nexus supports this via **dynamic node expansion** (§5.4), which many pure-static-DAG systems cannot express.
- *Reject:* implicit global state in the client (context-variable-driven flow discovery), which makes behaviour hard to reason about across process boundaries.

**Temporal** — the reference point for durable execution.
- *Adopt:* the event-sourced history as the source of truth for a workflow's progress; the hard separation between deterministic orchestration logic and non-deterministic activity execution; task queues as the unit of worker routing; heartbeating for long activities with server-side timeout detection; the four-timeout model (schedule-to-start, start-to-close, schedule-to-close, heartbeat), which is genuinely better thought out than most competitors' single "timeout" field. We adopt it wholesale in §4.3.
- *Reject:* replay-based determinism as the *only* programming model. It is powerful but it imposes a steep constraint on user code (no non-deterministic calls, versioning hazards on code change). Nexus's core model is a DAG of idempotent tasks, which is a weaker but far more approachable abstraction. We keep the event-sourced *history* without requiring user code to be replayable.

**Dagster** — the reference point for typed, testable data pipelines.
- *Adopt:* declarative resource/config binding, separated from business logic; strong typing at task boundaries; the insight that observability should be structured (structured events, not log scraping).
- *Defer:* software-defined assets as the primary abstraction. See §11.3.

**Argo Workflows** — the reference point for Kubernetes-native execution.
- *Adopt:* the container as the universal task unit; the workflow spec as a declarative, serializable document; the sidecar/init-container pattern for input staging and output capture.
- *Reject:* encoding workflow state inside a single Kubernetes CRD object. etcd is not a workflow database; large workflows hit object size limits and generate heavy watch traffic. Our state lives in Postgres. Kubernetes, when used, is one *executor* among several, not the system of record. See [ADR-0007](adr/0007-executor-abstraction.md).

**Flyte** — the reference point for strongly typed, versioned, resource-aware ML orchestration.
- *Adopt:* immutable, content-addressed task and workflow versions; a typed interface per task (declared input/output schemas) enabling static validation of the DAG before execution; resource requests/limits as a first-class part of the task spec; memoization keyed on (task version + input hash), which is exactly the right primitive for idempotency and caching.
- *Reject:* mandatory Kubernetes coupling for the MVP. We keep the typed-interface and content-addressing ideas but make the execution substrate pluggable.

**Kestra** — the reference point for declarative-first authoring.
- *Adopt:* YAML as a first-class authoring surface, not an afterthought, so that non-Python users can define workflows; a plugin taxonomy for tasks and triggers.
- *Reject:* nothing significant; the main divergence is that our YAML and our SDK compile to the *same* `WorkflowSpec`, so neither is second-class.

**Celery** — the reference point for task queues.
- *Adopt:* the worker pool model, prefetch limits, and the hard lesson that `acks_late` plus idempotent tasks is the only safe configuration for work you care about.
- *Reject:* using the broker as the state store. Celery has no real notion of a DAG, no durable workflow-level state machine, and its result backend is an afterthought. Queue position is not workflow state. In Nexus the broker carries *notifications*; Postgres holds *truth*. See [ADR-0003](adr/0003-nats-as-event-bus.md).

**Ray** — the reference point for distributed Python compute.
- *Adopt:* conceptually, the value of resource-typed scheduling (`num_cpus`, `num_gpus`, custom resources) — a clean model we mirror in our `ResourceRequest`.
- *Reject:* as a dependency. Explicitly out of scope per [ADR-0009](adr/0009-no-ray-dependency.md).

**Luigi** — the reference point for dependency resolution via targets.
- *Adopt:* the `Target`/completeness idea — a task is skippable if its output already exists. This is the ancestor of Flyte's memoization and informs our cache-key design.
- *Reject:* the pull-based, worker-resolves-dependencies model with no central scheduler state. It does not scale operationally and gives no global view.

### 2.2 Synthesis

The resulting shape: **Flyte's typed, versioned, resource-aware task spec; Temporal's event-sourced history and timeout model; Prefect's control-plane/user-code separation; Airflow's operational surface area (backfill, UI, catchup); Celery's hard-won lessons about ack timing.** State in Postgres, notifications over NATS, execution behind a pluggable executor interface.

---

## 3. System architecture

### 3.1 Plane separation

The central structural decision is that the system is split into two planes with a narrow, versioned contract between them.

```
┌───────────────────────────────────────────────────────────────────────────────┐
│                             CLIENTS                                           │
│   CLI (nexusctl)      SDK (python)      React UI      external HTTP callers   │
└───────────────────────────────┬───────────────────────────────────────────────┘
                                │  HTTPS / gRPC  (authn: session, API key, scoped token)
                                ▼
╔═══════════════════════════════════════════════════════════════════════════════╗
║                            CONTROL PLANE                                      ║
║                  (stateless services + shared durable state)                  ║
║                                                                               ║
║   ┌──────────────────┐                                                        ║
║   │  API Gateway     │  authn/authz, rate limit, request validation,          ║
║   │                  │  spec compilation, versioning, read APIs               ║
║   └────────┬─────────┘                                                        ║
║            │                                                                  ║
║   ┌────────▼─────────┐   ┌──────────────────┐   ┌──────────────────┐          ║
║   │ Workflow Manager │   │   DAG Engine     │   │    Scheduler     │          ║
║   │                  │   │                  │   │                  │          ║
║   │ run lifecycle,   │◄─►│ dependency       │◄─►│ queue → worker   │          ║
║   │ cancel/pause,    │   │ resolution,      │   │ matching,        │          ║
║   │ retry policy,    │   │ readiness calc,  │   │ priority,        │          ║
║   │ timers, backfill │   │ dynamic expand   │   │ preemption       │          ║
║   └────────┬─────────┘   └────────┬─────────┘   └────────┬─────────┘          ║
║            │                      │                      │                    ║
║            │             ┌────────▼──────────┐  ┌────────▼─────────┐          ║
║            │             │ Resource Manager  │  │ Worker Registry  │          ║
║            │             │                   │  │                  │          ║
║            │             │ capacity ledger,  │  │ registration,    │          ║
║            │             │ reservations,     │◄─┤ heartbeat,       │          ║
║            │             │ quota enforcement │  │ liveness, drain  │          ║
║            │             └───────────────────┘  └──────────────────┘          ║
║            │                                                                  ║
║   ┌────────▼──────────────────────────────────────────────────────┐           ║
║   │                    Metadata Store (Postgres)                  │           ║
║   │  specs · runs · task_attempts · events · workers · leases     │           ║
║   │  reservations · schedules · idempotency_keys                  │           ║
║   └───────────────────────────────────────────────────────────────┘           ║
║                                                                               ║
║   ┌───────────────────────────────────────────────────────────────┐           ║
║   │                     Event Bus (NATS + JetStream)              │           ║
║   │  task.dispatch.<queue> · run.events · worker.heartbeat        │           ║
║   └───────────────────────────────────────────────────────────────┘           ║
╚═══════════════════════════════════════════════════════════════════════════════╝
            │  outbound: dispatch notifications        ▲  inbound: status reports
            │  (NATS subject per task queue)          │  (gRPC to API Gateway)
            ▼                                         │
╔═══════════════════════════════════════════════════════════════════════════════╗
║                           EXECUTION PLANE                                     ║
║                                                                               ║
║   ┌─────────────────────┐  ┌─────────────────────┐  ┌────────────────────┐   ║
║   │   Worker Agent      │  │   Worker Agent      │  │   Worker Agent     │   ║
║   │  queue=default      │  │  queue=gpu          │  │  queue=default     │   ║
║   │  16 CPU / 64Gi      │  │  8 CPU/128Gi/2 A100 │  │  16 CPU / 64Gi     │   ║
║   │                     │  │                     │  │                    │   ║
║   │  ┌───────────────┐  │  │  ┌───────────────┐  │  │  ┌──────────────┐  │   ║
║   │  │ lease mgr     │  │  │  │ lease mgr     │  │  │  │ lease mgr    │  │   ║
║   │  │ heartbeat     │  │  │  │ heartbeat     │  │  │  │ heartbeat    │  │   ║
║   │  │ local sched   │  │  │  │ local sched   │  │  │  │ local sched  │  │   ║
║   │  └───────┬───────┘  │  │  └───────┬───────┘  │  │  └──────┬───────┘  │   ║
║   │  ┌───────▼───────┐  │  │  ┌───────▼───────┐  │  │  ┌──────▼───────┐  │   ║
║   │  │   Executor    │  │  │  │   Executor    │  │  │  │   Executor   │  │   ║
║   │  │ subprocess /  │  │  │  │ subprocess /  │  │  │  │ subprocess / │  │   ║
║   │  │ docker / k8s  │  │  │  │ docker / k8s  │  │  │  │ docker / k8s │  │   ║
║   │  └───────────────┘  │  │  └───────────────┘  │  │  └──────────────┘  │   ║
║   └─────────────────────┘  └─────────────────────┘  └────────────────────┘   ║
╚═══════════════════════════════════════════════════════════════════════════════╝
            │
            ▼  task inputs/outputs (never through the control plane)
   ┌─────────────────────────────────────────┐
   │  Result Store (S3 / MinIO)              │
   │  content-addressed blobs + small inline │
   └─────────────────────────────────────────┘
```

**The invariants that make this a real boundary, not a diagram:**

1. **No user code in the control plane.** The control plane parses `WorkflowSpec` JSON. It never imports, executes, or `eval`s anything the user wrote. This is the single biggest divergence from Airflow and the reason the scheduler can be made genuinely reliable.
2. **No control-plane database credentials in the execution plane.** Workers hold an API token. They reach Postgres only through the API Gateway's narrow gRPC surface. A compromised worker cannot corrupt the run history of workflows it never touched.
3. **Task payloads do not transit the control plane.** Large inputs and outputs go to the result store; the control plane moves *references*. This keeps the metadata store small and prevents a single large task from degrading the whole system. (See [ADR-0008](adr/0008-result-store-separation.md).)
4. **The control plane is stateless.** All durable state is in Postgres or NATS JetStream. Any control-plane service can be killed and restarted. Horizontal scaling is adding replicas.

### 3.2 Control plane components

#### API Gateway
The only ingress. Responsibilities:
- **Authentication:** three credential kinds, all presented as `Authorization: Bearer` and resolved in `services/api-gateway/authn`:
  - **Human sessions.** Email + password login (argon2id verifier in `users.password_hash`) issues a short-lived, revocable session token (`sessions`, migration 0007; email verification and reset, 0017). Sessions are refused on the worker protocol.
  - **API keys** (sha256-hashed at rest, `api_keys`) for workers, the CLI and other machines.
  - **Scoped tokens** (`scoped_tokens`): an API-key-shaped credential with an expiry and an explicit action list that *intersects* the role's grants and never extends them.

  **OIDC and SAML single sign-on are not implemented; they are planned.** Migration 0007 records this as a deliberate decision (it needs an identity provider no deployment has yet) and leaves `users.password_hash` nullable so an OIDC-backed user can later be added as a new auth kind rather than a redesign. No identity-provider integration, group mapping or SCIM exists today ([RBAC-IMPLEMENTATION.md](RBAC-IMPLEMENTATION.md)).
- **Authorization:** RBAC over `(principal, namespace, action)`. Namespaces are the tenancy and quota boundary.
- **Validation and compilation:** accepts a `WorkflowSpec` in JSON or YAML, validates it structurally (schema), semantically (acyclicity, type compatibility across edges, resource requests within namespace quota), then content-addresses it. Submitting a byte-identical spec is a no-op returning the existing version.
- **Two API surfaces:** a REST/JSON API for humans, CLI, and UI; a gRPC API for the high-frequency worker RPCs (`Register`, `Heartbeat`, `PollTask`, `ReportTaskResult`, `ExtendLease`). Splitting these matters: worker traffic is chatty and latency-sensitive, and it must not contend with UI queries.

Stateless. Scales horizontally behind any L7 load balancer.

#### Workflow Manager
Owns the lifecycle of a `WorkflowRun`. Responsibilities:
- Creating runs from a trigger (manual, schedule, event, or API call) with idempotency-key deduplication.
- Enforcing run-level policies: overall run timeout, concurrency limits (`max_active_runs` per workflow), failure policy (`fail_fast` vs. `continue_on_failure`).
- Handling operator interventions: `cancel`, `pause`, `resume`, and `retry` — of the whole run or of selected nodes and their dependents (§5.2, Operator retry).
- **Timer management.** A durable timer table drives all deadline-based work: schedule-to-start expiry, start-to-close expiry, heartbeat expiry, retry backoff. A single reconciler polls `timers WHERE fire_at <= now()` with `FOR UPDATE SKIP LOCKED`. Using the database as the timer store rather than in-memory timers means a control-plane restart loses nothing.
- Backfill: generating a bounded set of runs over a historical interval with its own concurrency cap, so a backfill cannot starve live traffic.

#### DAG Engine
Pure dependency logic, deliberately kept free of I/O beyond its own transaction. Responsibilities:
- Loading the compiled DAG for a run and computing the **ready frontier**: the set of nodes whose every upstream dependency is in a satisfying terminal state.
- Applying **trigger rules** per node (`all_success` default, `all_done`, `one_success`, `one_failed`, `none_failed`, `all_skipped`) — Airflow's taxonomy here is well-designed and worth adopting directly.
- Propagating `SKIPPED` downstream when a branch is not taken, and deciding when a run is terminal.
- **Dynamic expansion** (§5.4): materializing N child nodes when an upstream task returns a collection whose cardinality was unknown at submission time.
- Evaluating conditional edges (`when` expressions over upstream outputs, using a restricted, side-effect-free expression language — CEL or a similar sandboxed evaluator, never `eval`).

The engine is a function: `(dag_spec, current_attempt_states) → set of transitions`. Making it pure means it is exhaustively unit-testable and can be property-tested against invariants (no node ever runs twice, no node runs before its dependencies, every run reaches a terminal state).

#### Scheduler
Decides *which* ready task goes to *which* worker, and when. Responsibilities:
- Maintaining per-queue ordered views of `PENDING` task attempts, ordered by `(priority DESC, enqueued_at ASC)`.
- **Resource-aware matching:** for each candidate attempt, ask the Resource Manager whether any worker in the target queue can satisfy the `ResourceRequest`. If not, the task stays pending and a structured reason is recorded (`waiting_for_resources: gpu`) so the UI can explain the delay rather than showing an unexplained stall.
- **Fair sharing:** weighted round-robin across namespaces within a queue, so one namespace flooding the queue cannot starve others.
- **Bin-packing policy** (pluggable): `best_fit` by default to reduce fragmentation, `spread` available for resilience-sensitive workloads.
- Emitting a dispatch notification to `task.dispatch.<queue>` and creating a **lease** record.
- **Head-of-line blocking avoidance:** a large task that cannot fit is not allowed to block smaller tasks indefinitely. The scheduler looks ahead a bounded window, but an aging term in the priority function guarantees the blocked large task eventually wins. Without this, a pure priority queue starves big GPU jobs forever — a real and common failure in naive resource schedulers.

Multiple scheduler replicas run concurrently; correctness comes from the database (row locks and the atomic claim in §5.3), not from leader election. A scheduler replica dying mid-cycle loses nothing because the claim is transactional.

#### Resource Manager
The capacity ledger. Responsibilities:
- Tracking each worker's **advertised capacity** (from registration) and **committed capacity** (sum of active reservations).
- Issuing and releasing **reservations** atomically with lease creation, so capacity accounting can never drift from actual dispatch.
- Enforcing namespace **quotas** (max concurrent CPU / memory / GPU per namespace) as an admission check before dispatch.
- Reconciling: on worker heartbeat, the worker reports its actual running task IDs. Any reservation the control plane holds for that worker that the worker does not acknowledge is a leak, and is released. This closes the gap where a worker restarts and forgets its work but the ledger still believes the capacity is committed. Periodic reconciliation of a ledger against ground truth is the only way to keep distributed accounting honest.

Resources are modelled as a **typed vector**, not scalars: `{cpu_millis, memory_bytes, gpu: {count, kind}, disk_bytes, custom: {name: qty}}`. Custom resources let users express non-obvious constraints (`license_token: 1`, `db_connection: 5`) without a code change — Ray's custom-resource model, borrowed.

#### Metadata Store (Postgres)
Single source of truth. Holds specs, runs, attempts, the event log, worker registry, leases, reservations, schedules, and idempotency keys. §4 gives the model.

#### Event Bus (NATS + JetStream)
Two distinct roles:
1. **Dispatch notification** (`task.dispatch.<queue>`): a hint that work exists, enabling sub-second latency instead of waiting for a poll interval. It is *only* a hint. Losing the message costs latency, never correctness, because the reconciliation loop will find the pending attempt anyway. This is what makes the system robust to bus failure.
2. **Event fan-out** (`run.events.<namespace>.<run_id>`): durable structured event stream for the UI's live updates, notifications, external subscribers, and the audit trail. JetStream gives replay for late subscribers.

Critically, **the bus is never the source of truth**. This is the Celery lesson from §2.1.

**As implemented** (ADR-0003, migration 0016). Nothing publishes to NATS from inside or after a business transaction. Each announced fact -- a `run_events` row (including `AttemptDispatched` inside the §5.3 claim), a new `PENDING` attempt, a run state transition -- writes an `outbox` row *in the same SQL statement*, so the announcement exists if and only if the fact committed. The event-service runs the relay: it claims outbox rows `FOR UPDATE SKIP LOCKED`, publishes them to JetStream with a `Nats-Msg-Id` derived from the row (so a re-send after a crash is dropped as a duplicate), and marks them published only after the broker acks. The relay is deliberately *not* in the scheduler: the one component allowed to be down is kept off the one path that must not stall.

Subjects: `nexus.task.dispatch.<namespace>.<queue>` (hint; stream `NEXUS_DISPATCH`, in memory, 2-minute max age), `nexus.run.events.<namespace>.<run_id>` and `nexus.run.state.<namespace>.<run_id>` (projection; stream `NEXUS_RUNS`, file, 7 days). Workers consume hints through plain core subscriptions. A hint carries no work: it only wakes the agent's poller between long polls, and the claim remains the Postgres CAS, so duplicate, stale and reordered hints are harmless. The relay also re-announces every queue that holds dispatchable `PENDING` work every 10s, which covers transitions no outbox row describes (a resume, freed capacity) and re-wakes the fleet after any outage. An empty `NEXUS_NATS_URL` disables all of it and the system runs exactly as the poll-only Phase 1 did.

### 3.3 Execution plane: the worker agent

The worker agent is deliberately small. Its entire job is: register, prove liveness, obtain leased work, execute it under limits, report the outcome durably. Everything clever lives in the control plane.

```
┌───────────────────────────── WORKER AGENT ──────────────────────────────┐
│                                                                          │
│  ┌────────────────┐   Register(capacity, queues, labels, agent_version)  │
│  │  Registration  │ ──────────────────────────────────────────────────►  │
│  │                │ ◄────── worker_id, session_token, config ──────────  │
│  └────────────────┘                                                      │
│                                                                          │
│  ┌────────────────┐   Heartbeat(worker_id, running_task_ids, util)       │
│  │   Heartbeat    │ ──── every 5s, independent goroutine/thread ──────►  │
│  │     Loop       │ ◄──── ack + control directives (drain/cancel) ─────  │
│  └────────────────┘                                                      │
│                                                                          │
│  ┌────────────────┐   subscribe task.dispatch.<queue>  (NATS)            │
│  │   Task Puller  │ ◄─────────── notification ────────────────────────   │
│  │                │   PollTask(worker_id, free_capacity)   (gRPC)        │
│  │  notification- │ ──────────────────────────────────────────────────►  │
│  │  driven pull,  │ ◄──── TaskAttempt + lease_token + deadline ────────  │
│  │  poll fallback │                                                      │
│  └───────┬────────┘   pull, never push — see ADR-0005                    │
│          │                                                               │
│  ┌───────▼────────┐                                                      │
│  │ Local Admission│  re-check free capacity; reject if over-committed    │
│  └───────┬────────┘                                                      │
│          │                                                               │
│  ┌───────▼─────────────────────────────────────────────────┐             │
│  │                     Executor (pluggable)                │             │
│  │                                                          │             │
│  │   subprocess │ docker │ kubernetes │ (future: firecracker)│             │
│  │                                                          │             │
│  │   • stage inputs from result store                       │             │
│  │   • apply cgroup/container CPU+memory+GPU limits         │             │
│  │   • enforce start_to_close deadline (SIGTERM → SIGKILL)  │             │
│  │   • stream logs, capture structured events               │             │
│  │   • persist output to result store, return reference     │             │
│  └───────┬──────────────────────────────────────────────────┘             │
│          │                                                               │
│  ┌───────▼────────┐   ExtendLease(lease_token)  while task runs           │
│  │  Lease Keeper  │ ──────────────────────────────────────────────────►  │
│  └───────┬────────┘                                                      │
│          │                                                               │
│  ┌───────▼────────┐   ReportTaskResult(attempt_id, lease_token,          │
│  │    Reporter    │                    outcome, output_ref, metrics)     │
│  │                │ ──── retried with backoff until durably acked ────►  │
│  │  local WAL for │                                                      │
│  │  crash safety  │   see §6.3                                           │
│  └────────────────┘                                                      │
└──────────────────────────────────────────────────────────────────────────┘
```

Design choices worth calling out:

**Pull, not push.** The control plane never opens a connection to a worker. Workers initiate everything. This means workers can live behind NAT, in another VPC, or on a laptop; it removes the need for the control plane to track worker network reachability; and it makes backpressure automatic — a saturated worker simply stops polling. The NATS notification makes pull latency comparable to push without giving up these properties. ([ADR-0005](adr/0005-pull-based-dispatch.md).)

**Heartbeat is separate from task execution.** A blocked or slow task must not delay the heartbeat, or the control plane will wrongly declare the worker dead and duplicate the work. Heartbeat runs on its own thread with its own connection.

**Two levels of heartbeat.** Worker-level liveness (agent is alive) and optional task-level progress (this specific long-running task is making progress, reported by the SDK via `context.heartbeat()`). A worker can be alive while one task is wedged; distinguishing these lets us time out the task without killing the worker.

**Local write-ahead log.** Before a worker starts a task it records the intent locally; after the task finishes it records the outcome locally *before* reporting. If the agent crashes between task completion and successful report, it replays the outcome on restart instead of silently losing a completed result. This converts an expensive re-execution into a cheap re-report. §6.3.

**Durable task logs.** The executor's stdout and stderr are tagged, numbered and
sealed into NDJSON chunks that the agent writes directly to the result store
under `<namespace>/logs/<attempt_id>/`; only chunk *metadata* crosses to the
control plane, through the lease-fenced `ReportLogs` RPC, into a gateway-owned
index (`attempt_logs`, `attempt_log_chunks`). Writes into the shipper never
block the workload; a per-attempt cap keeps the head and the tail and records
what it omitted; the log is finalized and compacted before the result is
reported, while the lease is still valid. The gateway serves tail, pages,
bounded search and a live follow from index plus range reads, and artifact-gc
treats the index as a reference source. [ADR-0020](adr/0020-durable-task-logs.md),
OPERATIONS §19.

### 3.4 Monorepo layout

```
kgf-nexus/
├── docs/
│   ├── ARCHITECTURE.md              ← this document
│   ├── adr/                         ← decision records 0001-0010
│   └── diagrams/
├── proto/                           ← gRPC + protobuf: the plane contract
│   └── nexus/v1/
│       ├── worker.proto             ← Register/Heartbeat/Poll/Report/ExtendLease
│       ├── task.proto               ← TaskSpec, ResourceRequest, outcomes
│       └── workflow.proto           ← WorkflowSpec, run APIs
├── schemas/
│   └── workflow-spec-v1.json        ← JSON Schema; the authoring contract
├── services/                        ← CONTROL PLANE
│   ├── api-gateway/
│   ├── workflow-manager/
│   ├── dag-engine/                  ← library + service wrapper
│   ├── scheduler/
│   ├── resource-manager/
│   └── migrations/                  ← versioned SQL, no ORM auto-migrate
├── agent/                           ← EXECUTION PLANE
│   ├── cmd/nexus-agent/
│   └── internal/
│       ├── lease/ heartbeat/ wal/
│       └── executor/                ← subprocess | docker | k8s
├── sdk/
│   └── python/                      ← @task, @workflow, TaskContext, client
├── cli/                             ← nexusctl
├── ui/                              ← React + Vite + TanStack Query
├── deploy/
│   ├── docker-compose.yml           ← MVP one-command bring-up
│   └── helm/                        ← post-MVP
└── tests/
    ├── conformance/                 ← executor contract suite
    └── chaos/                       ← failure-injection scenarios
```

Language split: **Go** for the control-plane services and the agent (static binary, good concurrency story, trivial to ship an agent with no runtime dependency). **Python** for the first SDK (the user base for orchestration is Python-heavy). **TypeScript/React** for the UI. The `proto/` directory is the contract; all three generate from it, so the plane boundary is enforced by codegen rather than by discipline. ([ADR-0001](adr/0001-monorepo-and-language-split.md).)

---

## 4. Data model

### 4.1 Entity relationships

```
  WorkflowSpec (immutable, content-addressed)
    id, namespace, name, version, spec_hash, spec_json, created_at
        │
        │ 1:N
        ▼
  Schedule ──────────► WorkflowRun ◄────────── Trigger (manual|event|api|backfill)
    cron, tz,            id, spec_id, namespace, state, priority,
    catchup,             idempotency_key, trigger, params,
    next_fire_at         started_at, ended_at, deadline_at
                              │
                              │ 1:N
                              ▼
                         TaskNode (from spec; logical node in the DAG)
                           run_id, node_id, task_ref, state,
                           trigger_rule, parent_node_id (dynamic expansion)
                              │
                              │ 1:N  (one per retry / restart)
                              ▼
                         TaskAttempt  ◄──── the unit of execution
                           id, run_id, node_id, attempt_number, state,
                           queue, priority, resource_request,
                           idempotency_key, cache_key,
                           worker_id, lease_id,
                           scheduled_at, started_at, ended_at,
                           output_ref, error, exit_code, metrics
                              │
                    ┌─────────┼─────────┬──────────────┐
                    ▼         ▼         ▼              ▼
                 Lease   Reservation  Event[]      ResultRef
                                                   (→ result store)

  Worker (registry)                    Timer (durable deadline queue)
    id, namespace, queues[],             id, kind, subject_id,
    capacity, committed,                 fire_at, payload
    labels, state, agent_version,
    last_heartbeat_at, session_token_hash
```

### 4.2 Core schema (illustrative DDL)

Presented as SQL because the constraints *are* the design. The guarantees below are enforced by the database, not by application convention.

```sql
-- ─── Specs: immutable and content-addressed ───────────────────────────────
CREATE TABLE workflow_specs (
    id            UUID PRIMARY KEY,
    namespace     TEXT NOT NULL,
    name          TEXT NOT NULL,
    version       INT  NOT NULL,
    spec_hash     TEXT NOT NULL,            -- sha256 of canonical JSON
    spec_json     JSONB NOT NULL,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, name, version),
    UNIQUE (namespace, name, spec_hash)     -- resubmit identical spec = no-op
);
-- Specs are never UPDATEd. A change is a new version. This makes a run's
-- definition reproducible forever, which is the whole point of versioning.

-- ─── Runs ─────────────────────────────────────────────────────────────────
CREATE TYPE run_state AS ENUM (
    'PENDING','RUNNING','PAUSING','PAUSED',
    'SUCCEEDED','FAILED','CANCELLING','CANCELLED','TIMED_OUT'
);

CREATE TABLE workflow_runs (
    id               UUID PRIMARY KEY,
    spec_id          UUID NOT NULL REFERENCES workflow_specs(id),
    namespace        TEXT NOT NULL,
    state            run_state NOT NULL DEFAULT 'PENDING',
    priority         INT  NOT NULL DEFAULT 100,
    idempotency_key  TEXT,
    trigger_kind     TEXT NOT NULL,
    trigger_detail   JSONB,
    params           JSONB NOT NULL DEFAULT '{}',
    logical_date     TIMESTAMPTZ,           -- for schedules and backfill
    deadline_at      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    ended_at         TIMESTAMPTZ,
    state_version    BIGINT NOT NULL DEFAULT 0,   -- optimistic concurrency
    UNIQUE (namespace, idempotency_key)
);
-- The UNIQUE on idempotency_key is how "submit the same run twice" becomes
-- safe at the outermost layer: a duplicate INSERT conflicts and we return the
-- existing run rather than starting a second one.

CREATE INDEX ON workflow_runs (namespace, state)
    WHERE state IN ('PENDING','RUNNING','PAUSING','CANCELLING');
-- Partial index: the reconciler only ever scans non-terminal runs, and that
-- set stays small even when the history table grows to billions of rows.

-- ─── Nodes ────────────────────────────────────────────────────────────────
CREATE TYPE node_state AS ENUM (
    'PENDING','READY','RUNNING','SUCCEEDED','FAILED',
    'SKIPPED','UPSTREAM_FAILED','CANCELLED'
);

CREATE TABLE task_nodes (
    run_id          UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    node_id         TEXT NOT NULL,          -- stable id from the spec
    task_ref        TEXT NOT NULL,          -- handler identifier
    state           node_state NOT NULL DEFAULT 'PENDING',
    trigger_rule    TEXT NOT NULL DEFAULT 'all_success',
    max_attempts    INT  NOT NULL DEFAULT 3,
    attempts_used   INT  NOT NULL DEFAULT 0,
    parent_node_id  TEXT,                   -- non-null for dynamic children
    map_index       INT,                    -- non-null for dynamic children
    PRIMARY KEY (run_id, node_id)
);

-- ─── Attempts: the execution unit ─────────────────────────────────────────
CREATE TYPE attempt_state AS ENUM (
    'PENDING','DISPATCHED','RUNNING','SUCCEEDED','FAILED',
    'TIMED_OUT','LOST','CANCELLED'
);

CREATE TABLE task_attempts (
    id                UUID PRIMARY KEY,
    run_id            UUID NOT NULL,
    node_id           TEXT NOT NULL,
    attempt_number    INT  NOT NULL,
    state             attempt_state NOT NULL DEFAULT 'PENDING',
    queue             TEXT NOT NULL DEFAULT 'default',
    priority          INT  NOT NULL DEFAULT 100,
    resource_request  JSONB NOT NULL,
    idempotency_key   TEXT NOT NULL,        -- stable across retries, see §6.2
    cache_key         TEXT,                 -- for memoization, see §6.4
    worker_id         UUID REFERENCES workers(id),
    lease_id          UUID,
    input_refs        JSONB,
    output_ref        JSONB,
    error             JSONB,                -- {type, message, retryable, stack_ref}
    metrics           JSONB,                -- peak_memory, cpu_seconds, ...
    scheduled_at      TIMESTAMPTZ,
    dispatched_at     TIMESTAMPTZ,
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    state_version     BIGINT NOT NULL DEFAULT 0,
    FOREIGN KEY (run_id, node_id) REFERENCES task_nodes(run_id, node_id)
        ON DELETE CASCADE,
    UNIQUE (run_id, node_id, attempt_number)
);

-- The dispatch hot path. Partial index keeps it tiny regardless of history size.
CREATE INDEX idx_attempts_dispatchable
    ON task_attempts (queue, priority DESC, scheduled_at ASC)
    WHERE state = 'PENDING';

-- ─── Workers ──────────────────────────────────────────────────────────────
CREATE TYPE worker_state AS ENUM ('ACTIVE','DRAINING','QUARANTINED','DEAD');

CREATE TABLE workers (
    id                  UUID PRIMARY KEY,
    namespace           TEXT NOT NULL,
    hostname            TEXT NOT NULL,
    queues              TEXT[] NOT NULL,
    labels              JSONB NOT NULL DEFAULT '{}',
    capacity            JSONB NOT NULL,     -- advertised ResourceVector
    committed           JSONB NOT NULL DEFAULT '{}',  -- sum of reservations
    state               worker_state NOT NULL DEFAULT 'ACTIVE',
    agent_version       TEXT NOT NULL,
    session_token_hash  TEXT NOT NULL,
    registered_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON workers (last_heartbeat_at) WHERE state = 'ACTIVE';

-- ─── Leases: the safety mechanism for at-least-once ───────────────────────
CREATE TABLE leases (
    id           UUID PRIMARY KEY,
    attempt_id   UUID NOT NULL UNIQUE REFERENCES task_attempts(id)
                                       ON DELETE CASCADE,
    worker_id    UUID NOT NULL REFERENCES workers(id),
    token        TEXT NOT NULL,       -- opaque; must be presented on report
    expires_at   TIMESTAMPTZ NOT NULL,
    renewed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    epoch        INT  NOT NULL DEFAULT 0
);
CREATE INDEX ON leases (expires_at);
-- UNIQUE(attempt_id) means at most one worker legitimately holds an attempt at
-- a time. A stale worker's report is rejected on token mismatch. This is
-- fencing: it does not prevent a zombie worker from *running* the task (nothing
-- can, over a network), but it prevents its result from corrupting our state.

-- ─── Reservations: the capacity ledger ────────────────────────────────────
CREATE TABLE reservations (
    id           UUID PRIMARY KEY,
    worker_id    UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    attempt_id   UUID NOT NULL UNIQUE REFERENCES task_attempts(id)
                                       ON DELETE CASCADE,
    resources    JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Created in the SAME transaction as the lease, so the ledger cannot drift
-- from dispatch reality. Reconciled against worker heartbeats (§3.2).

-- ─── Events: append-only history ──────────────────────────────────────────
CREATE TABLE run_events (
    id          BIGSERIAL PRIMARY KEY,
    run_id      UUID NOT NULL,
    seq         INT  NOT NULL,
    node_id     TEXT,
    attempt_id  UUID,
    kind        TEXT NOT NULL,   -- RunCreated, AttemptDispatched, ...
    payload     JSONB NOT NULL,
    actor       TEXT NOT NULL,   -- scheduler | worker:<id> | user:<sub>
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);
-- Append-only, never UPDATEd or DELETEd within retention. This is the audit
-- log, the UI timeline source, and the debugging record. The current-state
-- tables above are a materialized projection of this log; on any disagreement
-- the log wins, and state can be rebuilt from it.

-- ─── Timers: durable deadlines ────────────────────────────────────────────
CREATE TABLE timers (
    id          UUID PRIMARY KEY,
    kind        TEXT NOT NULL,   -- schedule_to_start | start_to_close |
                                 -- heartbeat | retry_backoff | run_deadline
    subject_id  UUID NOT NULL,
    fire_at     TIMESTAMPTZ NOT NULL,
    payload     JSONB NOT NULL DEFAULT '{}',
    claimed_by  TEXT,
    claimed_at  TIMESTAMPTZ
);
CREATE INDEX ON timers (fire_at) WHERE claimed_by IS NULL;
-- Every deadline in the system is a row here. Control-plane restart loses no
-- timers, which is the failure mode of in-memory timer wheels.

-- ─── Idempotency for external submissions ─────────────────────────────────
CREATE TABLE idempotency_keys (
    namespace    TEXT NOT NULL,
    key          TEXT NOT NULL,
    request_hash TEXT NOT NULL,
    response     JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, key)
);
-- Replayed submit with same key + same body → cached response.
-- Same key + different body → 409. This catches client bugs instead of
-- silently doing the wrong thing.
```

### 4.3 Task specification

The task spec is the contract between the author and the platform, and the most important type in the system. Shown as the authoring form; the wire form is protobuf.

```yaml
# ── Identity ───────────────────────────────────────────────────────────────
id: train_model                  # unique within the workflow
task_ref: ml.train:v3            # handler identity, resolved by the worker
version: 3                       # bump ⇒ new cache namespace

# ── Typed interface (enables static DAG validation, Flyte-style) ──────────
inputs:
  dataset:     {type: file_ref, required: true}
  epochs:      {type: int, default: 10}
  hyperparams: {type: json, required: false}
outputs:
  model:   {type: file_ref}
  metrics: {type: json}

# ── Resource requirements ─────────────────────────────────────────────────
resources:
  cpu:    {request: "2000m", limit: "4000m"}   # millicores
  memory: {request: "8Gi",   limit: "16Gi"}    # request = scheduling,
  gpu:                                          # limit  = enforcement
    count: 1
    kind:  "nvidia-a100"        # optional; omit for any GPU
    memory: "40Gi"              # optional per-device VRAM floor
  disk:   {request: "50Gi"}     # ephemeral scratch
  custom:
    license_token: 1            # arbitrary countable constraints

# ── Placement ─────────────────────────────────────────────────────────────
queue: gpu                      # routing: worker subscribes to queues
node_selector:                  # hard constraints on worker labels
  zone: us-east-1a
tolerations: ["spot"]
affinity:
  prefer_same_worker_as: preprocess_data   # data-locality hint, soft

# ── Timeouts: Temporal's four-timeout model ───────────────────────────────
timeouts:
  schedule_to_start: 30m   # queued too long ⇒ fail fast, alert on capacity
  start_to_close:    4h    # single attempt wall clock
  schedule_to_close: 6h    # total across ALL retries; the real SLA
  heartbeat:         5m    # no progress signal ⇒ presume stuck, retry
# Why four and not one: "it timed out" is ambiguous and operationally useless.
# Distinguishing "never got capacity" (schedule_to_start) from "ran too long"
# (start_to_close) from "went silent" (heartbeat) tells you whether to fix the
# cluster, the code, or the network. Collapsing them loses that signal.

# ── Retries ───────────────────────────────────────────────────────────────
retry:
  max_attempts: 4
  backoff:
    strategy: exponential      # fixed | linear | exponential
    initial:  10s
    max:      10m
    multiplier: 2.0
    jitter:   full             # full jitter; prevents retry thundering herd
  retry_on:                    # allowlist of error classes
    - transient
    - timeout
    - worker_lost
  no_retry_on:                 # always terminal, do not waste capacity
    - validation_error
    - permission_denied
  retry_on_different_worker: true   # avoid a poisoned host

# ── Execution semantics ───────────────────────────────────────────────────
semantics:
  idempotency: required        # required | best_effort | none
  idempotency_key_from:        # deterministic key derivation
    - run_id
    - node_id
    - map_index
  cache:
    enabled: true
    key_from: [task_ref, version, inputs_hash]
    ttl: 7d
# idempotency: required is the DEFAULT for the platform. A task that declares
# `none` is accepted but flagged in the UI and excluded from automatic retry
# of ambiguous outcomes (§6.5), because we cannot safely re-run it.

# ── Runtime ───────────────────────────────────────────────────────────────
runtime:
  executor: docker             # subprocess | docker | kubernetes (alias: k8s)
  image: registry/ml-train:3.1.0@sha256:abc...   # digest-pinned
  command: ["python", "-m", "train"]
  env:
    LOG_LEVEL: info
  secrets:                     # injected by worker from its provider;
    - name: S3_CREDENTIALS     # never stored in the metadata store
      ref: vault://kv/ml/s3

# ── Concurrency control ───────────────────────────────────────────────────
concurrency:
  key: "model-training"        # named semaphore
  limit: 3                     # ≤3 attempts with this key run globally
```

**On `request` vs `limit`:** `request` is what the scheduler subtracts from worker capacity; `limit` is what the executor enforces via cgroups/container limits. Allowing `limit > request` permits opportunistic bursting and better utilization; setting them equal gives predictable, guaranteed performance. Kubernetes got this distinction right and it is worth adopting verbatim.

---

## 5. State machines

### 5.1 TaskAttempt

The most important state machine in the system. Everything about correctness under failure is visible here.

```
                            ┌─────────┐
                            │ PENDING │  created by DAG engine; node is ready
                            └────┬────┘
                                 │
              ┌──────────────────┼──────────────────────────┐
              │                  │                          │
    scheduler claims:   schedule_to_start                 run cancelled
    lease+reservation      expires                             │
    in one txn                │                                │
              │               ▼                                ▼
              │         ┌───────────┐                    ┌───────────┐
              │         │ TIMED_OUT │                    │ CANCELLED │
              │         └───────────┘                    └───────────┘
              ▼
      ┌────────────┐
      │ DISPATCHED │  lease held; worker notified; not yet confirmed started
      └─────┬──────┘
            │
    ┌───────┼──────────────────────────┐
    │       │                          │
 worker  lease expires             cancel
 reports  before start           requested
 started  (worker never
    │      picked it up)              │
    │       │                          │
    │       ▼                          ▼
    │  ┌──────┐                  ┌───────────┐
    │  │ LOST │──► retry?        │ CANCELLED │
    │  └──────┘                  └───────────┘
    ▼
 ┌─────────┐
 │ RUNNING │  worker confirmed start; renewing lease; heartbeating
 └────┬────┘
      │
      ├──── worker reports success (valid lease token) ──────► ┌───────────┐
      │                                                        │ SUCCEEDED │
      │                                                        └───────────┘
      ├──── worker reports failure (valid lease token) ──────► ┌────────┐
      │                                                        │ FAILED │
      │                                                        └────────┘
      ├──── start_to_close timer fires ──► worker sent SIGTERM ┌───────────┐
      │                                    then SIGKILL ─────► │ TIMED_OUT │
      │                                                        └───────────┘
      ├──── heartbeat timer fires (task went silent) ────────► ┌───────────┐
      │                                                        │ TIMED_OUT │
      │                                                        └───────────┘
      ├──── lease expires (worker died / partitioned) ───────► ┌──────┐
      │                                                        │ LOST │
      │                                                        └──────┘
      └──── cancel requested ──► SIGTERM, grace, SIGKILL ────► ┌───────────┐
                                                               │ CANCELLED │
                                                               └───────────┘

TERMINAL: SUCCEEDED, FAILED, TIMED_OUT, LOST, CANCELLED
          ── no transition out of a terminal state, ever ──

RETRY: FAILED | TIMED_OUT | LOST, when
         attempts_used < max_attempts
         AND error class ∈ retry_on
         AND schedule_to_close not exceeded
       ⇒ a NEW TaskAttempt row (attempt_number + 1) in PENDING,
         after retry_backoff timer fires.
       The failed attempt keeps its terminal state forever. We never mutate
       history; a retry is a new attempt. This is what makes the event log a
       truthful record and makes "show me every attempt" trivially answerable.
```

**Why `LOST` is separate from `FAILED`.** This distinction is the crux of at-least-once and the thing naive designs get wrong. `FAILED` means *we know the task ran and did not succeed*. `LOST` means *we do not know whether it ran, whether it is still running, or whether it had side effects*. These demand different handling:

- `FAILED` → safe to retry per policy; the previous attempt is known-complete-and-unsuccessful.
- `LOST` → retrying may **double-execute**. This is legal only because we mandate idempotency. For a task declaring `idempotency: none`, we refuse to auto-retry a `LOST` attempt and surface it for human decision instead. Pretending otherwise would be the dishonest choice.

A zombie worker — alive, partitioned, still running the task whose lease we expired — cannot be prevented over a network. Lease fencing means its eventual report is *rejected* (stale token), so it cannot corrupt state. Its *side effects* are handled by idempotency, which is why idempotency is a platform requirement rather than a suggestion.

### 5.2 WorkflowRun

```
                    ┌─────────┐
                    │ PENDING │
                    └────┬────┘
                         │ DAG engine computes initial frontier
                         ▼
      ┌──────────► ┌─────────┐ ◄──────────┐
      │            │ RUNNING │            │
      │            └────┬────┘            │
      │                 │                 │
      │  resume    ┌────┼─────┬───────────┼──────────┐
      │            │    │     │           │          │
      │         pause  all  a node     cancel     run
      │         req   nodes failed      req     deadline
      │            │  terminal (fail_fast)        │
      │            │    │     │           │          │
      │            ▼    │     │           ▼          ▼
      │       ┌─────────┐│     │    ┌────────────┐ ┌───────────┐
      └───────│ PAUSING ││     │    │ CANCELLING │ │ TIMED_OUT │
              └────┬────┘│     │    └──────┬─────┘ └───────────┘
                   │     │     │           │
        in-flight  │     │     │  in-flight attempts
        attempts   │     │     │  cancelled, workers
        drain      │     │     │  acked
                   ▼     │     │           ▼
              ┌────────┐ │     │     ┌───────────┐
              │ PAUSED │ │     │     │ CANCELLED │
              └────────┘ │     │     └───────────┘
                         │     │
            ┌────────────┘     └──────────┐
            ▼                             ▼
    ┌───────────┐                    ┌────────┐
    │ SUCCEEDED │                    │ FAILED │
    └───────────┘                    └────────┘

SUCCEEDED: every node terminal AND no node in {FAILED, UPSTREAM_FAILED, TIMED_OUT}
FAILED:    at least one node terminally failed after retries exhausted
PAUSING:   no NEW attempts dispatched; in-flight ones run to completion.
           Pausing does not kill work — that is cancel. Conflating them
           makes pause useless for its main job (holding a pipeline while
           you investigate without losing hours of compute).
RETRY:     FAILED | TIMED_OUT ──operator retry──► RUNNING (see below)
```

#### Operator retry

`POST /v1/namespaces/{ns}/runs/{id}/retry` with `{"nodes": [...]}` (`nexusctl run retry <id> --node X`, SDK `retry_run(id, nodes=[...])`, the console's "Retry this task") retries a `FAILED` or `TIMED_OUT` run in place. An empty body retries the whole run. As implemented (CONTRACT-FINDINGS F100; `internal/dag/retry.go` is the specification in code):

- **What is reset.** Each named node must be `FAILED`, `UPSTREAM_FAILED` or `CANCELLED`. It and every downstream node that did not succeed (`FAILED`, `UPSTREAM_FAILED`, `CANCELLED`, `SKIPPED`) return to `PENDING`; the ordinary reconcile then decides what runs, so a retried graph is evaluated by the same trigger rules as the first time. `SUCCEEDED` nodes are never reset, upstream or downstream; upstream work is never touched. A whole-run retry selects every node that did not succeed, `SKIPPED` included (fail_fast skips independent branches). An expanded map template goes back to `RUNNING`, not `PENDING` — its expansion record exists and must not be re-expanded — and a selected child resets its template but not its siblings. A template whose expansion was refused is not retryable: the same upstream output would be refused again.
- **All or nothing.** The request is refused, and nothing is reset, if any named node is unknown (422), is in another state (409), would fail again at once because a failed or cancelled upstream is not also named (409, naming the node to add), or cannot run because the workflow is `fail_fast` and another failure is left standing (409). The gateway checks the selection first so the operator gets a precise answer; the dag-engine re-checks it in its own transaction, and applies the resets as compare-and-sets that roll back together if any node moved in between.
- **Attempts.** History is immutable (invariant 9): earlier attempts are not touched and stay in the run view. `task_nodes.retry_floor` (migration 0026) records the highest attempt number the retry superseded; the engine derives node state only from attempts above it. The next attempt is `attempt_number = MAX + 1` with the **same** idempotency key as every earlier try (invariant 1, ADR-0006), and the node's retry budget (`max_attempts`, backoff) starts afresh from the floor.
- **Concurrency.** The workflow-manager holds the run row (`FOR NO KEY UPDATE`) across the engine's reset, renews the run deadline, moves the run to `RUNNING` and commits, then reconciles. A second retry waits and is refused (the run is no longer failed); a cancel waits and then cancels the retried run; a stale terminal report — a reconcile that concluded `FAILED` just before the reset — waits and is then refused, because `OnNodesTerminal` counts the nodes under the same lock and finds them no longer terminal. If the engine call fails without saying whether it committed, the run is committed `RUNNING` and reconciled anyway: either the reset happened and the retry proceeds, or every node is still terminal and the reconcile restores the run's outcome. The API answers 503 in that case, meaning "re-read the run", not "it failed".
- **Audit.** The gateway writes a `run.retry` audit record (success or failure, with the nodes); the run's history gets `RunStateChanged`, a `RunRetriedFromFailed` event (`mode`, `requested_nodes`, `selected`, `reset_nodes`, actor) and one `NodeStateChanged` (`reason: operator_retry`) per reset node.

### 5.3 Dispatch: the atomic claim

Everything about multi-scheduler correctness reduces to this one transaction. It is the heart of the design, so it is specified precisely.

```
-- READ COMMITTED, deliberately, NOT REPEATABLE READ.
-- Verified empirically: under REPEATABLE READ a locking read that reaches a row
-- another transaction has already COMMITTED an update to does not skip it -- it
-- aborts the whole transaction with SQLSTATE 40001 (could not serialize access
-- due to concurrent update). SKIP LOCKED only skips rows *currently locked*; it
-- does nothing for rows already committed by someone else. With N scheduler
-- replicas on one queue those aborts are constant and every claim degrades into
-- a retry loop.
-- Nothing is lost by using READ COMMITTED: the no-double-dispatch argument rests
-- on the row locks, the state='PENDING' CAS guard, and UNIQUE(attempt_id) on
-- leases -- none of which depend on snapshot isolation.
BEGIN ISOLATION LEVEL READ COMMITTED;

  -- 1. Claim a candidate attempt. SKIP LOCKED is what allows N scheduler
  --    replicas to work the same queue without coordination or contention.
  SELECT * FROM task_attempts
   WHERE state = 'PENDING' AND queue = $queue
   ORDER BY priority DESC, scheduled_at ASC
   LIMIT  1
   FOR UPDATE SKIP LOCKED;

  -- 2. Find a worker that fits, locking the row so two schedulers cannot
  --    both spend the same capacity.
  SELECT * FROM workers
   WHERE state = 'ACTIVE'
     AND $queue = ANY(queues)
     AND last_heartbeat_at > now() - $liveness_window
     AND fits(capacity, committed, $resource_request)
     AND labels @> $node_selector
   ORDER BY best_fit_score(capacity, committed, $resource_request)
   LIMIT  1
   FOR UPDATE SKIP LOCKED;

  -- If no worker fits: record the reason, leave PENDING, COMMIT, move on.
  -- The attempt is NOT failed. It waits, and the UI can say why.

  -- 3. Namespace quota admission.
  IF quota_would_exceed($namespace, $resource_request) THEN
      record_reason('quota_exceeded'); COMMIT; RETURN;
  END IF;

  -- 4. Commit capacity, lease, state, event, and timers atomically.
  INSERT INTO reservations (worker_id, attempt_id, resources) ...;
  UPDATE workers SET committed = committed + $resource_request WHERE id = ...;
  INSERT INTO leases (attempt_id, worker_id, token, expires_at)
       VALUES (..., gen_token(), now() + $lease_ttl);
  UPDATE task_attempts
     SET state='DISPATCHED', worker_id=..., lease_id=...,
         dispatched_at=now(), state_version = state_version + 1
   WHERE id = $attempt_id AND state = 'PENDING';     -- CAS guard
  INSERT INTO run_events (...) VALUES (..., 'AttemptDispatched', ...);
  INSERT INTO timers (kind,'schedule_to_start'|'start_to_close', ...);

COMMIT;

-- 5. AFTER commit, publish a NATS notification. Deliberately outside the
--    transaction: the bus is a hint (§3.2). If publish fails, the worker's
--    next poll or the reconciler picks the attempt up. Latency degrades;
--    correctness does not. Publishing *inside* would require distributed
--    transactions across Postgres and NATS, which is the wrong trade.
--
--    As implemented: step 4's run_event INSERT also writes an outbox row in
--    the same statement (no extra round trip; isolation and locking
--    unchanged), and the event-service relay performs this step from the
--    outbox after commit. That closes the gap a post-commit publish by the
--    claimer leaves open: a crash between COMMIT and publish.
```

Properties this gives us:
- **No double dispatch.** `UNIQUE(attempt_id)` on `leases` plus the CAS guard on `state = 'PENDING'`.
- **No capacity over-commit.** `FOR UPDATE` on the worker row serializes capacity spending.
- **No accounting drift.** Reservation and lease are created in one transaction; heartbeat reconciliation catches the residual case of worker amnesia.
- **No leader election needed.** Correctness comes from the database. Scheduler replicas are interchangeable and disposable.

### 5.4 Dynamic expansion

Static DAGs cannot express "process each file in whatever the previous step produced" when the count is unknown at submission time. Prefect and Airflow's dynamic task mapping solve this; we adopt the pattern with explicit bounds.

```
Spec declares:                      At runtime:

  ┌──────────┐                       ┌──────────┐
  │  list    │                       │  list    │ → returns 4 items
  │  files   │                       │  files   │
  └────┬─────┘                       └────┬─────┘
       │ map_over: files                  │  DAG engine materializes
       ▼                                  ├─────┬─────┬─────┐
  ┌──────────┐                            ▼     ▼     ▼     ▼
  │ process  │  (template node,        ┌────┐┌────┐┌────┐┌────┐
  │  (×N)    │   never executes)       │p[0]││p[1]││p[2]││p[3]│
  └────┬─────┘                         └──┬─┘└──┬─┘└──┬─┘└──┬─┘
       │ reduce                            └─────┴──┬──┴─────┘
       ▼                                            ▼
  ┌──────────┐                                 ┌──────────┐
  │ summarize│                                 │ summarize│
  └──────────┘                                 └──────────┘
```

Rules that keep this from becoming a footgun:
- `max_fanout` is **mandatory** in the spec. An upstream task returning 10 million items must fail the expansion explicitly, not silently try to create 10 million rows and take down the database.
- Children are real `task_nodes` with `parent_node_id` and `map_index`, so they get full per-item state, retries, and observability. They are not opaque sub-items.
- `map_index` participates in the idempotency key, so retrying child 7 re-runs exactly child 7's work.
- The reduce node's trigger rule decides tolerance for partial failure (`all_success` vs. `none_failed` vs. a `min_success_ratio` threshold).
- Expansion is recorded as an event, so the materialized DAG for a historical run is reconstructible.

**As implemented** (migration 0020, `internal/dag/expand.go`, `services/dag-engine/expansion.go`):

- **Spec.** A task with `map: {over_node_id, over_output, item_input, max_fanout}` is a *template*. Submit-time validation requires `max_fanout > 0` and at most the operator ceiling (default 10,000), `over_node_id` to be a **direct** upstream (an edge must exist), `over_output` to be declared `json`, `item_input` to be a declared input bound by no edge or literal, and every downstream input fed by the template to be `json`. Authored node ids may not contain `[`/`]`, which are reserved for child ids `template[i]`.
- **The collection must be inline** (≤ the result-store inline threshold). The control plane counts and slices it; invariant 6 forbids it from fetching a blob, so a blob collection fails the template with `collection_not_inline`. Map over references (object keys), not over data.
- **Atomic, at most once.** When the template's trigger fires, the engine CASes the template `PENDING → RUNNING` (or `SUCCEEDED` for an empty collection, `FAILED` for a refused one), inserts a `task_expansions` record keyed `(run_id, template_node_id)`, and inserts every child `task_nodes` row and inherited `task_edges` row — **one transaction**. A crash leaves nothing; the retry re-derives byte-identical children from the same immutable upstream output. Concurrent reconcilers and duplicate completion reports lose the CAS (or the record's primary key) and roll back entirely.
- **Refusal is explicit.** Over `max_fanout`, not an array, missing, or not inline: the template becomes `FAILED` with the reason on the record and an `ExpansionFailed` event; nothing is materialized; downstream propagates as for any failure.
- **Children** inherit the template's spec (minus `map`), trigger rule, retry budget and incoming edges; the element is written onto the child's spec as an `input_literals` entry. Their idempotency key is `sha256(run_id ‖ template_node_id ‖ map_index ‖ version)`.
- **Fan-in.** Downstream nodes depend on the template, never on children, so a 10,000-item fan-in has one predecessor. The template's state is the aggregate of its children (any failed → `FAILED`; else any cancelled → `CANCELLED`; else any skipped → `SKIPPED`; else `SUCCEEDED`), folded in the same reconcile pass in which the last child settles. A downstream input bound to a template output receives an inline JSON array, one element per child in `map_index` order: the child's inline JSON value, `{"inline_base64": …}` for non-JSON inline bytes, `{"result_ref": …}` for a blob, `null` for a child that did not succeed.
- **The fan-in is bounded.** The array is assembled in the control plane, so it is capped at `NEXUS_DAG_MAX_FANIN_BYTES` (default 16 MiB, allowed 64 KiB–256 MiB; the JSON payload before base64). Children's outputs are never loaded with the rest of the run; they are streamed from Postgres in `map_index` order and the size is checked **before each element is appended**, so an over-limit fan-in stops reading at the element that would cross the limit and never holds more than the limit. The consuming node is then `FAILED` without an attempt, in one transaction with a `fan_in_refusals` row (`reason = fan_in_too_large`, the limit, how many children fit, how to raise it or restructure) and a `FanInRejected` event; the run view shows it as "fan-in too large". Not retried: the same children produce the same array. At submit time, a template output declaring `max_bytes` is rejected (`map_fan_in_too_large`) when `max_fanout × max_bytes` could exceed the limit (the gateway checks against the default; the engine re-checks against its configured limit at materialize).
- **Pause / cancel / fail_fast** withhold expansion exactly as they withhold the frontier. Cancel settles unexpanded templates and pending children `CANCELLED`; pause leaves them `PENDING` to be dispatched on resume (an expanded template does not count as in flight).
- **Not implemented:** `min_success_ratio` (use `all_done` / `none_failed` on the fan-in); nested maps inside one template; expanding over a blob collection; re-expanding a template on retry (an expanded template is retried through its children; a refused expansion is not retryable — §5.2, Operator retry).

### 5.5 Worker lifecycle

```
        Register(capacity, queues, labels)
                    │
                    ▼
              ┌──────────┐
              │  ACTIVE  │ ◄──── heartbeat every 5s
              └────┬─────┘
                   │
   ┌───────────────┼────────────────┬──────────────────┐
   │               │                │                  │
 drain req    missed HB >        repeated task     graceful
 (deploy,     liveness_window    crashes /         shutdown
  scale-in)   (default 30s)      OOMs
   │               │                │                  │
   ▼               ▼                ▼                  ▼
┌──────────┐  ┌────────┐    ┌──────────────┐    ┌────────┐
│ DRAINING │  │  DEAD  │    │ QUARANTINED  │    │  DEAD  │
└────┬─────┘  └───┬────┘    └──────┬───────┘    └────────┘
     │            │                │
 no new tasks;    │         no new tasks;
 finish current;  │         operator or auto-
 then DEAD        │         recovery re-ACTIVEs
                  │
                  ▼
     • all reservations released → capacity returns to the pool
     • all held leases expire → attempts → LOST → retry per policy
     • event emitted for the audit trail and alerting

Recovery: a DEAD worker that comes back re-registers with a NEW worker_id.
It never reclaims old leases. Identity is per-session, not per-host, so a
restarted agent cannot resurrect stale leases — a genuinely nasty class of bug
in systems that key worker identity on hostname.

QUARANTINED exists because the worst failure mode in any task queue is the
poison host: a worker with a broken driver or bad disk that accepts tasks,
fails them instantly, and is immediately handed more — chewing through the
retry budget of every task in the queue in seconds. Circuit-breaking the
worker, not just the task, is the fix.
```

**Quarantine, as implemented** (migration 0021, `internal/store/rm_quarantine.go`, `services/resource-manager/internal/server/quarantine.go`). *Definition:* a QUARANTINED worker receives **no new dispatches** — the claim admits only `ACTIVE` workers — while work it already holds finishes; it keeps heartbeating (and is swept `DEAD` if it stops). It is entered:

- **automatically**, by a resource-manager detector, when in the last 10 minutes a worker has ≥ 3 infrastructure-class failures (`ERROR_CLASS_INFRASTRUCTURE`, `ERROR_CLASS_WORKER_LOST`, or `LOST` while it stayed registered) spanning ≥ 2 distinct `task_ref`s and ≥ 50% of its terminal attempts. The distinct-task rule is what makes the verdict worker-correlated rather than a poison *task*. Application, validation, timeout and OOM failures never count. Never more than 50% of a namespace's fleet at once, and never its last ACTIVE worker — a shared-dependency outage makes every worker look poisoned, and quarantining them all would outlast the outage. Duration 15 min, doubling per relapse of the same host within 24 h, capped at 24 h;
- **manually**, by an operator (`POST …/workers/{id}/quarantine {reason, duration_ms}`, permission `workers.quarantine`); `duration_ms: 0` is indefinite;
- **by inheritance**: a new session from a host with an open episode starts QUARANTINED, because worker identity is per session and a restart must not launder a poison host back into the pool.

It ends on expiry, or by `POST …/workers/{id}/release`, which closes every open episode for the host. Every episode is a `worker_quarantines` row (trigger, reason, evidence, actor, expiry, release); operator actions are also in `audit_log`. A quarantined worker is no longer sent `DRAIN`, which the agent treats as permanent. All thresholds are `NEXUS_QUARANTINE_*` settings (OPERATIONS §14).

---

## 6. Execution semantics: at-least-once + idempotency

### 6.1 The guarantee, stated honestly

**Nexus guarantees at-least-once execution of every task attempt, and requires task handlers to be idempotent.**

We do not claim exactly-once. Consider a worker that finishes a task, commits its side effect, and dies before its report is acknowledged. The control plane cannot distinguish this from a worker that died *before* the side effect. Its only options are to retry (risking double execution) or not to (risking a stalled workflow). No amount of engineering removes this choice; it is a consequence of the network, not a limitation of implementation. Systems advertising exactly-once are providing at-least-once delivery plus deduplication at the effect boundary — which is exactly what we provide, described accurately.

What we *do* guarantee:
1. A task that is dispatched will run **at least once**, unless cancelled before start.
2. **At most one attempt is authorized** at any instant (lease fencing). Concurrent duplicate *execution* is possible only in the zombie-worker window; concurrent *state corruption* is not.
3. A task with a correct idempotency key produces **exactly one logical effect**, however many times it physically executes.
4. **Every state transition is durable before it is observable.** No transition is acknowledged from memory.

### 6.2 Idempotency key derivation

The platform computes the key deterministically — it never asks users to invent one, because hand-rolled keys are where this goes wrong in practice.

```
idempotency_key = sha256(
    run_id        ‖    # unique per workflow execution
    node_id       ‖    # unique per DAG node
    map_index     ‖    # dynamic expansion child index (or "-")
    task_version       # bumping version = intentionally new logical effect
)
```

Note the deliberate exclusion of `attempt_number`: **all retries of the same logical work share one key.** That is the entire point — the key identifies *the work*, not *the try*. Including the attempt number would make every retry a fresh effect and defeat deduplication.

The key is handed to the handler as `context.idempotency_key`, and the SDK provides the primitives that make using it easy:

```python
@task(idempotency="required")
def charge_customer(ctx: TaskContext, order_id: str, amount: int):
    # Guard: skip if this logical effect already landed.
    if ctx.effect_recorded("charge"):
        return ctx.previous_result("charge")

    # Pass the key to the downstream system's own dedup mechanism.
    receipt = payments.charge(
        order_id=order_id, amount=amount,
        idempotency_key=ctx.idempotency_key,   # Stripe-style dedup
    )
    ctx.record_effect("charge", receipt)       # durable, via result store
    return receipt
```

Three cooperating mechanisms, in descending order of preference:
1. **Downstream-native dedup** — pass the key to a system that already supports it (Stripe, most modern APIs). Strongest.
2. **Conditional writes** — `INSERT ... ON CONFLICT DO NOTHING` keyed on the idempotency key; atomic in the target store.
3. **Effect journal** (`ctx.record_effect`) — a platform-provided record of "this effect landed", for systems with no native support. Weakest, because the write-then-crash window still exists, but it narrows the window from the whole task to a single write.

The SDK documents this ordering explicitly. Being clear about which guarantee a user actually has is more valuable than a comfortable abstraction that hides it.

### 6.3 The worker write-ahead log

```
Worker receives attempt
   │
   ├─► WAL: {attempt_id, lease_token, state: STARTING, ts}   ── fsync
   │
   ├─► report Started → control plane
   │
   ├─► execute task
   │
   ├─► WAL: {attempt_id, state: COMPLETED, outcome, output_ref}  ── fsync
   │        ▲
   │        └── written BEFORE reporting. If the agent dies here, the
   │            outcome survives and is replayed on restart.
   │
   ├─► report Result → control plane  (retry w/ backoff until acked)
   │
   └─► WAL: delete entry   (only after durable ack)

On agent restart:
   • scan WAL
   • entries in COMPLETED  → re-report (idempotent server-side on attempt_id)
   • entries in STARTING   → cannot know the outcome. Report
                             `indeterminate`; control plane treats the
                             attempt as LOST and applies §6.5.
```

This does not eliminate the fundamental uncertainty, but it removes the *common* case: agent restarts (deploys, OOM of the agent process itself, node reboots) no longer waste completed work. Only genuinely ambiguous crashes reach the indeterminate path.

### 6.4 Memoization

Distinct from idempotency, and worth separating: idempotency makes re-execution *safe*; memoization makes it *unnecessary*.

```
cache_key = sha256(task_ref ‖ task_version ‖ canonical_hash(inputs))
```

On dispatch, if a `SUCCEEDED` attempt with the same `cache_key` exists within TTL, the scheduler short-circuits: the new attempt goes straight to `SUCCEEDED` with the cached `output_ref`, and emits a `CacheHit` event. This is Flyte's memoization and Luigi's target-completeness idea, and it is what makes retrying a 50-node pipeline after one late failure cheap instead of catastrophic. Opt-in per task, because it is only valid for genuinely pure tasks.

**As implemented** (migration 0021, `services/dag-engine/expansion.go` `memoFor`, `internal/store/dagengine_memo.go`), with two deliberate departures from the paragraph above:

- **The key is namespaced:** `cache_key = sha256(namespace ‖ sha256(task_ref ‖ version ‖ inputs_digest))`, using the §6.2 construction (`\x1f`-separated fields). A cached `output_ref` points into its tenant's result-store prefix, so a cross-tenant hit would be both unusable and a disclosure. `inputs_digest` hashes each resolved input's **content** (inline bytes, or the blob's content address), so encodings of one value cannot miss.
- **The DAG engine, not the scheduler, short-circuits** — at attempt *creation*. A hit is inserted directly as a `SUCCEEDED` attempt carrying the source's `output_ref` and `cache_hit_of = <source attempt>`; it is never `PENDING`, so no scheduler can race to dispatch it and the §5.3 claim pays nothing for memoization. The engine re-reconciles in the same call so downstream work proceeds (nothing will ever report on a memoized attempt). A `CacheHit` event is emitted and the console marks the node *cached*.
- **Eligibility:** only `SUCCEEDED`, executed (`cache_hit_of IS NULL` — a hit never restamps the TTL), ended within `cache.ttl_ms` (default 7 days) of control-plane time, after the newest matching operator invalidation, same tenant. Never `idempotency: none` (rejected at submit, and never memoized at runtime for specs stored earlier); never a failed/timed-out/lost/cancelled result.
- **Concurrency:** identical runs that miss concurrently both execute (the task is declared pure); later runs hit the newest source. No single-flight — documented, not implemented.
- **Invalidation:** `POST /v1/namespaces/{ns}/cache/invalidations {task_ref[, task_version] | cache_key, reason}` appends a cutoff to `memo_invalidations`; it deletes nothing.

### 6.5 Handling indeterminate outcomes

The interesting policy question. When an attempt is `LOST`:

| Task declares | Policy |
|---|---|
| `idempotency: required` | Auto-retry. Safe by contract; this is the default and the recommended setting. |
| `idempotency: best_effort` | Auto-retry, but emit a `PossibleDuplicateExecution` warning event, surfaced in the UI and available to alerting. |
| `idempotency: none` | **Do not auto-retry.** Move to `FAILED` with `reason=indeterminate_outcome` and require explicit human action (`nexusctl attempt retry --force`). |

Refusing to silently retry non-idempotent work is the right default. A platform that quietly double-charges a customer because retrying was more convenient has chosen the wrong failure mode.

---

## 7. Key flows

### 7.1 Submit and execute

```
 SDK/CLI        API GW       WF Mgr      DAG Eng     Sched      RM      Worker
   │              │            │            │          │         │         │
   │─submit spec─►│            │            │          │         │         │
   │              │─validate,  │            │          │         │         │
   │              │ hash, store│            │          │         │         │
   │◄─spec_id,ver─│            │            │          │         │         │
   │              │            │            │          │         │         │
   │─create run──►│───────────►│            │          │         │         │
   │  (+idem key) │            │─dedup on   │          │         │         │
   │              │            │ idem key   │          │         │         │
   │              │            │──compute──►│          │         │         │
   │              │            │  frontier  │          │         │         │
   │              │            │            │─insert   │         │         │
   │              │            │            │ PENDING  │         │         │
   │              │            │            │ attempts │         │         │
   │◄──run_id─────│◄───────────│            │          │         │         │
   │              │            │            │          │         │         │
   │              │            │            │  ◄──poll─│         │         │
   │              │            │            │          │─fits?──►│         │
   │              │            │            │          │◄─worker─│         │
   │              │            │            │          │         │         │
   │              │            │   ┌────────┴──────────┴───┐     │         │
   │              │            │   │ ATOMIC TXN (§5.3):    │     │         │
   │              │            │   │ reservation + lease + │     │         │
   │              │            │   │ DISPATCHED + event +  │     │         │
   │              │            │   │ timers                │     │         │
   │              │            │   └────────┬──────────────┘     │         │
   │              │            │            │          │─NATS notify──────►│
   │              │            │            │          │         │         │
   │              │◄────────────── PollTask(free_capacity) ────────────────│
   │              │─── attempt + lease_token + deadline ──────────────────►│
   │              │            │            │          │         │         │
   │              │◄────────── ReportStarted ──────────────────────────────│
   │              │            │                                 │   [stage inputs
   │              │◄────────── ExtendLease (periodic) ────────────────────│    from result
   │              │◄────────── Heartbeat (every 5s) ──────────────────────│    store; run;
   │              │            │                                 │    write output]
   │              │◄────────── ReportResult(output_ref) ──────────────────│
   │              │─release reservation, close lease, SUCCEEDED  │         │
   │              │─────────────────────────► DAG Eng: recompute frontier  │
   │              │            │            │  → next attempts PENDING     │
   │              │─publish run.events (UI live update)          │         │
```

### 7.2 Worker death mid-task

```
 t+0     Worker RUNNING attempt A. Lease expires_at = t+30s. HB every 5s.
 t+3     ExtendLease → expires_at = t+33s.       [normal operation]
 t+7     ── WORKER PROCESS KILLED (OOM / node loss / network partition) ──
 t+33    Lease expiry timer fires.
           • attempt A: RUNNING → LOST
           • reservation released; worker.committed decremented
           • event: AttemptLost{reason: lease_expired}
 t+35    Worker liveness window (30s since last HB) exceeded.
           • worker: ACTIVE → DEAD
           • all its remaining leases expired the same way
           • event: WorkerDead
 t+35    Retry evaluation for A:
           idempotency=required, attempts_used=1 < max_attempts=3,
           schedule_to_close not exceeded, worker_lost ∈ retry_on
           ⇒ new attempt A' (attempt_number=2) PENDING,
             retry_on_different_worker=true → excludes the dead worker
 t+36    Scheduler dispatches A' to a healthy worker.
 t+40    A' runs. Its idempotency_key EQUALS A's (attempt_number excluded
         from derivation), so any side effect A already committed is
         deduplicated rather than repeated. This is the whole design working.

 ── Zombie case: if the original worker was merely partitioned, not dead,
    it may still be executing A and may eventually try to report.
    Its lease token is stale ⇒ report REJECTED, state uncontaminated.
    Its side effects are deduplicated by the shared idempotency key.
    Both halves of the defence are necessary; neither alone suffices.
```

### 7.3 Resource contention

```
 Queue `gpu`: one worker, 4 GPUs, 3 free.
 PENDING: T1(priority 200, 4 GPU)   T2(priority 100, 1 GPU)

 Cycle 1: T1 is highest priority but needs 4 > 3 free.
          → stays PENDING, reason='waiting_for_resources: gpu (need 4, free 3)'
          Look-ahead window permits T2 → dispatched (1 GPU, 2 free).
          Pure head-of-line blocking would idle 3 GPUs waiting for T1.

 Cycle 2..n: T1 still blocked. Its effective priority accrues an aging term:
          effective = priority + age_seconds × aging_rate
          Once it exceeds the look-ahead threshold, the scheduler STOPS
          dispatching smaller tasks to this worker and reserves capacity as
          it frees. Without aging, a steady stream of small tasks starves the
          large one forever — the classic failure of naive bin-packing
          schedulers, and the reason this term is in the design from day one.

 Cycle m: enough GPUs freed → T1 dispatched.
```

---

## 8. Interfaces

### 8.1 Worker gRPC (the plane contract)

```protobuf
service WorkerService {
  rpc Register       (RegisterRequest)     returns (RegisterResponse);
  rpc Heartbeat      (HeartbeatRequest)    returns (HeartbeatResponse);
  rpc PollTask       (PollTaskRequest)     returns (PollTaskResponse);
  rpc ReportStarted  (ReportStartedRequest)returns (Ack);
  rpc ExtendLease    (ExtendLeaseRequest)  returns (ExtendLeaseResponse);
  rpc ReportResult   (ReportResultRequest) returns (Ack);
  rpc ReportProgress (ProgressRequest)     returns (Ack);   // task heartbeat + executor status (F96)
  rpc ReportLogs     (ReportLogsRequest)   returns (Ack);   // log chunk index (ADR-0020)
}

message HeartbeatRequest {
  string worker_id = 1;
  repeated string running_attempt_ids = 2;  // ground truth for reconciliation
  ResourceVector free_capacity = 3;
  Utilization util = 4;
}
message HeartbeatResponse {
  repeated Directive directives = 1;  // DRAIN | CANCEL_ATTEMPT | UPDATE_CONFIG
  int32 next_heartbeat_ms = 2;        // server-controlled backoff
}
```

`running_attempt_ids` on every heartbeat is what makes the capacity ledger self-correcting (§3.2): the control plane compares its reservations against what the worker says it is actually doing and releases the difference. Server-controlled heartbeat interval lets the control plane shed load under pressure instead of being overwhelmed by a large fleet.

### 8.2 Public REST API (sketch)

```
POST   /v1/namespaces/{ns}/specs                 submit/validate spec
GET    /v1/namespaces/{ns}/specs/{name}/versions
POST   /v1/namespaces/{ns}/runs                  create run (Idempotency-Key hdr)
GET    /v1/namespaces/{ns}/runs?state=&spec=     list
GET    /v1/namespaces/{ns}/runs/{id}             detail + node states
GET    /v1/namespaces/{ns}/runs/{id}/events      history (SSE for live tail)
POST   /v1/namespaces/{ns}/runs/{id}/cancel
POST   /v1/namespaces/{ns}/runs/{id}/pause | /resume
POST   /v1/namespaces/{ns}/runs/{id}/retry       retry; body {"nodes":[...]} selects nodes
GET    /v1/namespaces/{ns}/attempts/{id}/logs    tail | page | search (?q=) | SSE follow | text (ADR-0020)
GET    /v1/namespaces/{ns}/workers               fleet view
GET    /v1/namespaces/{ns}/workers/{id}          detail + the tasks running on it
POST   /v1/namespaces/{ns}/workers/{id}/drain
GET    /v1/namespaces/{ns}/capacity              advertised vs committed
```

### 8.3 SDK sketch

```python
from nexus import task, workflow, TaskContext, Resources, Retry

@task(
    resources=Resources(cpu="2", memory="8Gi"),
    timeouts={"start_to_close": "1h", "heartbeat": "5m"},
    retry=Retry(max_attempts=3, backoff="exponential", jitter="full"),
    idempotency="required",
    cache=True,
)
def extract(ctx: TaskContext, source: str) -> FileRef:
    ctx.heartbeat("connecting")          # progress signal for long tasks
    ctx.log.info("extracting", source=source)
    return ctx.store.put(fetch(source))  # result store, not the control plane

@workflow(name="daily-etl", schedule="0 2 * * *", timezone="UTC")
def daily_etl(ctx, sources: list[str]):
    raw = extract.map(sources, max_fanout=100)   # dynamic expansion
    clean = transform.map(raw)
    return load(clean.reduce())
```

The decorator does **not** execute at DAG-build time in the control plane. It compiles to a `WorkflowSpec` document in the user's process, which is then submitted. That is the Airflow lesson from §2.1, made structural.

---

## 9. Cross-cutting concerns

**Multi-tenancy.** Namespace is the boundary for RBAC, quota, queue access, and result-store prefixes. Enforced at the API Gateway and again in every query — never by client-side filtering.

**Secrets.** Never stored in the metadata store. The spec holds a *reference* (`vault://...`); the worker resolves it at execution time through its own configured provider. The control plane never sees secret values, so a metadata-store compromise does not leak credentials.

**Observability.** Structured JSON logs with `trace_id` / `run_id` / `attempt_id` on every line. OpenTelemetry traces spanning submit → dispatch → execute → report, so a slow pipeline can be diagnosed end-to-end. Prometheus metrics: queue depth by queue and namespace, dispatch latency, `schedule_to_start` distribution (the single best indicator of capacity shortfall), attempt outcomes by state, worker utilization, lease-expiry rate (the best indicator of fleet instability).

**Retention.** Terminal runs older than the retention window are archived to object storage and pruned from Postgres. Partition `run_events` and `task_attempts` by month so pruning is a partition drop rather than a mass `DELETE`. Designing this in from the start avoids the operational cliff every orchestrator hits at scale.

**Security.** mTLS between control-plane services; TLS plus token auth for workers; per-worker session tokens rotated on re-registration; sandboxed, side-effect-free expression evaluation for conditional edges (never `eval`); container isolation with dropped capabilities and non-root by default; full audit trail in `run_events` with the acting principal on every transition.

**Versioning.** The gRPC/proto contract is versioned (`nexus.v1`) with N-1 agent compatibility, so the control plane can be upgraded before the fleet. `WorkflowSpec` carries an explicit `api_version`. A run pins its spec version for its lifetime, so a mid-flight spec change never alters a running workflow.

---

## 10. Failure modes and responses

| Failure | Detection | Response | Guarantee preserved |
|---|---|---|---|
| Worker process dies | Missed heartbeat > 30s | Worker → `DEAD`; leases expire; attempts → `LOST`; retry per policy | At-least-once |
| Worker partitioned (zombie) | Lease expiry | Attempts → `LOST`, retried; zombie's later report rejected on stale token | No state corruption; idempotency handles effects |
| Task exceeds `start_to_close` | Durable timer | SIGTERM, grace period, SIGKILL; → `TIMED_OUT`; retry per policy | Bounded resource use |
| Task silently wedged | Heartbeat timeout | → `TIMED_OUT`; retry on a different worker | Progress |
| Scheduler replica dies | Peer/orchestrator restart | Stateless; uncommitted claim rolls back; another replica proceeds | No lost or double dispatch |
| Postgres failover | Connection error | Services retry with backoff; no ack from memory; in-flight tasks keep running and report on recovery | Durability |
| NATS unavailable | Publish/subscribe error | Fall back to polling; the outbox backlog waits in Postgres and drains, de-duplicated, when the broker returns; the relay's periodic hint re-wakes the fleet | Correctness (latency degrades) |
| Result store unavailable | Put/get error | Attempt fails as `transient`; retried | No silent data loss |
| Poison task (crashes every worker) | Repeated identical failures | Exhaust `max_attempts` → `FAILED`; escalate to quarantine only if worker-correlated | Fleet stays healthy |
| Poison worker (fails every task) | Failure rate per worker | → `QUARANTINED`; stops receiving work | Retry budgets not burned |
| Runaway dynamic expansion | `max_fanout` exceeded | Expansion fails explicitly: template `FAILED`, `task_expansions.reason = max_fanout_exceeded`, nothing materialized | Control plane stability |
| Crash mid-expansion | — | The expansion is one transaction; nothing survives, and the next reconcile re-expands identically | No partial fan-out |
| Shared dependency down (every worker reports infrastructure errors) | Detector fleet cap reached | At most 50% of a namespace quarantined; the rest keep serving; logged as a probable shared-dependency fault | Platform stays up |
| Clock skew between nodes | — | All deadlines computed from control-plane time; worker clocks never authoritative | Correct timeouts |
| Duplicate run submission | `UNIQUE(namespace, idempotency_key)` | Return the existing run | Exactly one run per key |

---

## 11. Phased plan

### Phase 0 — Design (this deliverable)
Architecture document, ADRs 0001–0010, data model, state machines, phased plan. **No code.**
*Exit criteria:* reviewers agree on plane boundary, execution semantics, and the task spec. Open questions in §12 resolved or explicitly deferred.

### Phase 1 — MVP (the requested build target)
**Goal:** a DAG of containerized tasks runs end-to-end on a resource-aware scheduler, with heartbeats, retries, a result store, and one-command `docker compose up`.

In scope:
- `proto/` contract and `WorkflowSpec` JSON Schema.
- Postgres schema and migrations (§4.2) — Postgres as the *only* store for MVP; even the event bus is optional.
- API Gateway: authn via static API keys, spec submit/validate, run create/get/cancel, worker gRPC.
- DAG engine: static DAGs, `all_success` and `all_done` trigger rules, `SKIPPED` propagation, terminal-state detection. Pure and property-tested.
- Scheduler: single-queue, priority-ordered, resource-aware matching with the §5.3 atomic claim; look-ahead plus aging.
- Resource manager: CPU / memory / GPU-count ledger; reservations; heartbeat reconciliation.
- Worker agent: register, 5s heartbeat, poll (poll-only in Phase 1; optional NATS dispatch hints since added, see §3.2 and ADR-0003), lease renewal, subprocess **and** Docker executors, local WAL, result reporting with retry.
- Retries: exponential backoff with full jitter; `retry_on` classification; `LOST` handling per §6.5.
- Result store: MinIO, content-addressed, small results inline in Postgres.
- CLI: `nexusctl workflow submit | run create | run get | run cancel | worker ls | logs`.
- UI: run list, DAG view with live node states, attempt detail, log tail, worker fleet view. Read-only plus cancel.
- Docker Compose: Postgres, MinIO, control plane, 2 workers, UI.
- Tests: DAG-engine property tests; a chaos suite that kills workers mid-task and asserts exactly-one-logical-effect.

Explicitly deferred: dynamic expansion, cron schedules, backfill, NATS, k8s executor, multi-tenancy beyond a single namespace, RBAC, memoization, quarantine. (Since delivered: NATS as an optional transactional-outbox relay, ADR-0003; the k8s executor, see Phase 3; dynamic expansion, memoization and worker quarantine, see §5.4, §6.4 and §5.5.)

*Exit criteria:* a 10-node DAG with mixed CPU/GPU tasks completes correctly; killing a worker mid-run yields exactly one logical effect per task; two schedulers never double-dispatch under load.

### Phase 2 — Production hardening
NATS JetStream for sub-second dispatch and UI live updates. Cron schedules with timezone and catchup. Backfill with its own concurrency cap. Full trigger-rule set and conditional edges (sandboxed CEL). Memoization. Worker quarantine. Namespaces, RBAC, quotas. OpenTelemetry traces and the full Prometheus metric set. Retention and partition pruning. Multi-replica control plane with rolling upgrade and N-1 agent compatibility.

### Phase 3 — Scale and expressiveness
Dynamic expansion with `max_fanout`. Sub-workflows and workflow-of-workflows. Event-driven triggers (webhook, bus, sensor tasks). Kubernetes executor via the `Executor` interface (**delivered**: `agent/internal/executor/kubernetes*.go`, one Job per attempt, conformance-tested against a local cluster; see ADR-0007 "Implementation notes" and CONTRACT-FINDINGS F96 for what it does not yet do). Fair-share and preemption. Typed interface validation and lineage. Additional SDKs (TypeScript, Go) generated from `proto/`.

### Phase 4 — Platform maturity
Asset/lineage layer over the task core (the Dagster idea, added without rewriting the core — the reason the task layer is kept clean). Pluggable capacity providers for autoscaling. Multi-region control plane. Durable-execution programming model as an *optional* second authoring mode for users who need Temporal-style semantics.

---

## 12. Open questions for review

1. **Control-plane language.** Go is proposed (static agent binary, strong concurrency). Rust would give a smaller agent footprint at the cost of team velocity. Decide before Phase 1.
2. **gRPC vs. HTTP/2 + JSON for worker traffic.** gRPC is proposed; it costs proxy complexity in some environments. Is a JSON fallback needed for restricted networks?
3. **Cancellation semantics for non-idempotent tasks.** SIGTERM grace period is configurable; should a task be able to declare itself *uninterruptible* and, if so, what bounds it?
4. **Result-store size threshold.** Where is the inline-vs-blob boundary? Proposed 64 KiB inline; wants validation against real payload distributions.
5. **Priority model.** Single integer plus aging, or explicit weighted fair-share queues per namespace from the start? Retrofitting fair-share is unpleasant, so this deserves a decision in Phase 0.
6. **Postgres-as-queue ceiling.** `SKIP LOCKED` comfortably handles thousands of dispatches/second, which covers the target envelope. At what measured throughput do we need a dedicated dispatch path, and what is the migration story?

**Resolutions (Proposed — pending owner sign-off).** Each question is closed by an ADR carrying the evidence; the questions above are kept as asked.

| # | Resolution | ADR |
|---|---|---|
| 1 | Go stays for control plane and agent. Stripped static agent: 16.6 MB (linux/amd64), ~19 ms exec-to-exit, 17 MB RSS floor; a Rust agent could save ~10 MB at most, for a second codegen and conformance stack. | [ADR-0013](adr/0013-go-control-plane-and-agent.md) |
| 2 | gRPC only. All seven worker RPCs are unary, so the proxy requirements are short (HTTP/2 end to end, trailers, ≥ 60 s timeout, ≥ 4 MiB bodies, no buffering). No JSON fallback until a deployment needs HTTP/1.1-only egress; then Connect's JSON protocol. | [ADR-0014](adr/0014-worker-transport-grpc-without-json-fallback.md) |
| 3 | Yes, bounded: `cancellation.uninterruptible` sends no SIGTERM, waits min(declared ≤ 1 h, agent ceiling default 10 m), then SIGKILL. Does not extend `start_to_close`. Implemented and tested. | [ADR-0015](adr/0015-bounded-uninterruptible-tasks.md) |
| 4 | 64 KiB stays the default (measured: inline beats a MinIO round trip up to between 64 and 256 KiB; every shipped result is < 1 KiB), configurable within [1 B, 1 MiB], applied consistently. | [ADR-0016](adr/0016-inline-result-threshold.md) |
| 5 | Weighted fair share across namespaces (in-flight ÷ weight) ahead of priority + aging within a namespace, inside the unchanged claim. Implemented (migration 0022) and tested for starvation-freedom, weights, aging and no double dispatch. | [ADR-0017](adr/0017-weighted-fair-share-across-namespaces.md) |
| 6 | The assumption above does **not** hold for the current claim: its cost is linear in the queue's PENDING backlog (laptop: ~1.1k/s at 1k backlog with 16 replicas; ~40–200/s at 10k; single digits to ~20/s at 100k). Fix the scan in SQL first; build a dedicated dispatch path only if claim p99 > 250 ms, backlog > 50k, or demand > 70 % of measured capacity persists afterwards. | [ADR-0018](adr/0018-postgres-dispatch-ceiling.md) |

---

## 13. Document index

| Document | Subject |
|---|---|
| [ADR-0001](adr/0001-monorepo-and-language-split.md) | Monorepo layout and language split |
| [ADR-0002](adr/0002-postgres-as-metadata-store.md) | Postgres as metadata store and dispatch queue |
| [ADR-0003](adr/0003-nats-as-event-bus.md) | NATS as event bus, never as source of truth |
| [ADR-0004](adr/0004-declarative-workflow-spec.md) | Declarative spec over code-as-DAG |
| [ADR-0005](adr/0005-pull-based-dispatch.md) | Pull-based dispatch with push notification |
| [ADR-0006](adr/0006-at-least-once-with-idempotency.md) | At-least-once plus mandated idempotency |
| [ADR-0007](adr/0007-executor-abstraction.md) | Pluggable executor abstraction |
| [ADR-0008](adr/0008-result-store-separation.md) | Result store separate from metadata store |
| [ADR-0009](adr/0009-no-ray-dependency.md) | No Ray dependency |
| [ADR-0010](adr/0010-lease-based-task-ownership.md) | Lease-based task ownership and fencing |
| [ADR-0011](adr/0011-microservice-decomposition.md) | Microservice decomposition with a shared database schema |
| [ADR-0013](adr/0013-go-control-plane-and-agent.md) | Go remains the control-plane and agent language (§12 Q1) |
| [ADR-0014](adr/0014-worker-transport-grpc-without-json-fallback.md) | Worker transport: gRPC, proxy requirements, no JSON fallback yet (§12 Q2) |
| [ADR-0015](adr/0015-bounded-uninterruptible-tasks.md) | Bounded `uninterruptible` tasks (§12 Q3) |
| [ADR-0016](adr/0016-inline-result-threshold.md) | Inline-vs-blob result threshold (§12 Q4) |
| [ADR-0017](adr/0017-weighted-fair-share-across-namespaces.md) | Weighted fair share across namespaces (§12 Q5) |
| [ADR-0018](adr/0018-postgres-dispatch-ceiling.md) | Postgres dispatch ceiling, trigger and migration path (§12 Q6) |
