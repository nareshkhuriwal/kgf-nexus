# ADR-0009: No Ray dependency

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

Ray is a mature distributed-execution framework: a distributed scheduler, a shared-memory object store with zero-copy local reads, actors, and a resource model (`num_cpus`, `num_gpus`, custom resources). Ray Core plus Ray Workflows could plausibly supply much of the execution plane, and building on it would save real implementation effort.

It is worth being precise about why we are not doing that, because "the requirements said not to" is a reason to comply, not a reason that explains itself.

Ray solves a different problem. It is designed for **fine-grained, in-process, Python-native compute**: millions of short tasks, actor-to-actor messaging at millisecond latency, and a shared object store for passing large Python objects without serialization cost. Nexus orchestrates **coarse-grained, heterogeneous, polyglot work**: tasks lasting seconds to hours, running as containers, written in any language, where a few hundred milliseconds of scheduling overhead is irrelevant.

Adopting Ray for our workload would mean taking on its operational model without needing its core advantages.

## Decision

**Nexus takes no dependency on Ray.** The execution plane is our own worker agent (architecture §3.3) with pluggable executors (ADR-0007).

What we *do* borrow, conceptually and without a dependency: Ray's **typed resource vector** — the insight that scheduling should be expressed as multi-dimensional resource requests including user-defined custom resources (`license_token: 1`), rather than as CPU/memory scalars. Our `ResourceRequest` mirrors this directly.

Ray remains entirely usable **inside** a Nexus task: a task may run a container that starts a Ray job, and Nexus orchestrates it as one coarse unit of work. That composition is the correct relationship between the two systems, and we should document it as a supported pattern.

## Consequences

**Positive**
- **No Python coupling in the execution plane.** Ray is Python-centric; our agent is a Go binary that runs containers of any language. Polyglot support is a core requirement (G1), and Ray would compromise it.
- **No competing scheduler.** Ray has its own resource-aware scheduler. Running it under ours means two schedulers making placement decisions over the same capacity — the nested-scheduling problem of ADR-0007's `k8s` executor, but worse, because Ray's scheduler is less transparent about *why* a task is pending.
- **No cluster-lifecycle coupling.** A Ray cluster is a stateful, gang-scheduled entity with a head node whose loss is significant. Our worker fleet is a set of independent, disposable agents that can come and go individually — a much better fit for at-least-once semantics and for workers behind NAT.
- **Failure semantics stay ours.** Ray's fault tolerance (lineage-based object reconstruction) is powerful but is a different model from lease-fenced at-least-once attempts (ADR-0006/0010). Layering ours on top of theirs would produce two interacting recovery mechanisms and an untestable failure matrix.
- Dependency surface stays small: Postgres, NATS, object storage. All operationally boring.

**Negative**
- We implement our own worker agent, resource ledger, and heartbeat/lease machinery — genuinely the largest single chunk of engineering in Phase 1. Ray would have supplied an approximation of it.
- We do not get Ray's shared-memory object store, so passing large intermediate data between tasks goes through the result store (ADR-0008) with serialization cost. For workloads that pass multi-gigabyte objects between many short tasks, Ray would be materially faster. **We are explicit that such workloads are not our target**, and that the right answer there is to run Ray inside a single Nexus task.
- No fine-grained (sub-second) task support. Our scheduling overhead is tens to hundreds of milliseconds per task, which makes sub-second tasks inefficient. Acceptable and stated as a non-goal (§1.3).

## Alternatives considered

**Ray as the execution plane.** Rejected for the reasons above: Python coupling, competing scheduler, stateful cluster lifecycle, and a fault-tolerance model that conflicts with ours.

**Ray as one executor implementation (`runtime.executor: ray`).** The most defensible version of adopting Ray — it fits ADR-0007's abstraction and stays optional. Rejected for now on scope, not on principle: the `Executor` interface makes it addable later without core changes, and there is no MVP requirement it satisfies. Worth revisiting if a concrete Ray-shaped workload appears.

**Dask, Spark, or similar as the execution plane.** Rejected on the same reasoning. These are data-parallel compute engines, not general orchestrators; the correct relationship is to orchestrate them as tasks, not to build on them.
