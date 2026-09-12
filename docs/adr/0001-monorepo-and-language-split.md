# ADR-0001: Monorepo with Go control plane, Python SDK, React UI

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team
- **Supersedes:** —

## Context

Nexus comprises five deliverables that must stay contract-compatible: control-plane services, a worker agent, an SDK, a CLI, and a UI. The plane boundary (§3.1 of the architecture) is only real if it is mechanically enforced, and a contract split across repositories drifts.

Constraints:
- The worker agent must be deployable onto arbitrary hosts with no runtime prerequisite. A Python agent requires a matching interpreter and a virtualenv on every host; that is a real operational tax.
- The SDK's users are overwhelmingly Python developers. An orchestration platform whose first SDK is not Python will not be adopted by the teams who need it.
- The control plane is I/O-bound and highly concurrent: thousands of heartbeats, lease renewals, and poll requests per second.

## Decision

**A single monorepo.** The `proto/` directory holds the protobuf/gRPC contract; all components generate their bindings from it in CI, so a breaking change to the plane boundary fails the build rather than surfacing in production.

**Language split:**
- **Go** — control-plane services and worker agent. Static single-file binary for the agent (no runtime dependency), mature gRPC support, goroutine concurrency that matches the workload, fast compile/test cycle.
- **Python** — first SDK. Meets users where they are.
- **TypeScript + React** — UI, with types generated from the same proto/OpenAPI definitions.
- **SQL** — versioned migration files. No ORM auto-migration; schema changes are reviewed artifacts.

## Consequences

**Positive**
- One commit can change the contract and every consumer atomically; no cross-repo version matrix.
- Codegen enforces the plane boundary mechanically instead of by code review.
- Agent ships as a single binary; no interpreter management on worker hosts.
- Go's concurrency model fits the control plane without an async framework.

**Negative**
- Two languages in one repo means two toolchains and two CI paths.
- Go is more verbose than Python for the DAG-engine logic. Accepted: that code is small, pure, and heavily tested — verbosity there is cheap.
- Monorepo CI needs path-based selective builds to stay fast. Standard, solved tooling.

**Neutral**
- Additional SDKs (TypeScript, Go, Java) become straightforward later, since they generate from the same proto.

## Alternatives considered

**All-Python (Airflow/Prefect-style).** Faster initial velocity and one language. Rejected: the agent inherits interpreter deployment pain, and the high-concurrency control plane pushes toward asyncio complexity that Go handles natively.

**All-Rust.** Best agent footprint and strongest safety guarantees. Rejected for Phase 1 on team velocity; Go is materially faster to write and hire for, and the control plane is I/O-bound rather than CPU-bound, so Rust's performance edge is not where our bottleneck is. Revisit if the agent's resource ceiling becomes a constraint.

**Polyrepo.** Rejected: the plane contract is the most important interface in the system and must be atomically versionable.
