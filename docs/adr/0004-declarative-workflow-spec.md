# ADR-0004: Declarative WorkflowSpec, not code-as-DAG in the control plane

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

Workflow definitions must reach the control plane somehow. There are two established approaches.

**Code-as-DAG (Airflow).** Users write Python; the scheduler imports and executes it to discover the graph. Consequences observed in practice: the scheduler's stability becomes a function of user code quality (a slow import or an infinite loop at module level degrades scheduling for everyone); the scheduler needs every user dependency installed; definitions are not portable outside Python; and a run's definition can silently change under it when the file changes mid-flight.

**Declarative spec (Argo, Kestra, Flyte's registration model).** The graph is a serializable document. The control plane parses data, never executes code.

Our constraint that the control plane must never run user code (architecture §3.1, invariant 1) largely settles this, but the authoring ergonomics question remains: a raw YAML-only surface is a real regression for the Python users who are our primary audience.

## Decision

**A JSON/YAML `WorkflowSpec` is the single wire format and the only thing the control plane understands. Authoring surfaces compile to it.**

- The Python SDK's `@workflow` / `@task` decorators run **in the user's process** and emit a `WorkflowSpec`. The SDK is a compiler, not a runtime hook into the scheduler.
- YAML authoring is first-class and produces the identical document, so neither surface is second-class (the Kestra lesson).
- Specs are **immutable and content-addressed** (`sha256` of canonical JSON). Resubmitting a byte-identical spec is a no-op returning the existing version; any change is a new version.
- A run **pins its spec version for its lifetime**. Editing a workflow never mutates a run in flight.
- Validation happens at submission: JSON Schema for structure, then semantic checks — acyclicity, type compatibility across edges (the Flyte typed-interface idea), resource requests within namespace quota, `max_fanout` present on every dynamic node.

## Consequences

**Positive**
- The control plane cannot be destabilized by user code. This is the single largest reliability win in the design.
- The control plane needs no user dependencies installed — a major operational simplification versus Airflow.
- Specs are portable, diffable, reviewable in version control, and inspectable by non-Python tooling.
- Content addressing gives reproducibility: a historical run's exact definition is recoverable forever.
- Errors surface at submit time, not at 3am when the scheduler tries to parse.
- Additional SDKs in any language need only emit valid JSON.

**Negative**
- Genuinely dynamic control flow (arbitrary `while` loops, recursion) cannot be expressed as a static document. Mitigated by dynamic expansion (§5.4) for the overwhelmingly common "fan out over a runtime collection" case; truly arbitrary control flow is deferred to the optional durable-execution mode in Phase 4.
- Conditional edges need an expression language rather than Python. We use a sandboxed, side-effect-free evaluator (CEL or similar) — deliberately less expressive, and never `eval`.
- The SDK must faithfully compile decorators to spec, which is real work and a possible source of surprise ("my Python ran but produced a different graph than I expected"). Mitigated by `nexusctl workflow compile --show`, letting users inspect exactly what the platform will see.

## Alternatives considered

**Code-as-DAG.** Rejected on the reliability and coupling grounds above; it directly violates the plane-separation invariant.

**Code-as-DAG in an isolated parser sidecar.** A real middle ground: user code runs, but in a sandbox separate from the scheduler. Rejected for MVP as strictly more complexity than the declarative path while still giving up portability and reproducibility. Reconsider only if dynamic expansion proves insufficient in practice.

**Full durable-execution model (Temporal).** Maximum expressiveness — arbitrary control flow, because the program *is* the workflow. Rejected as the *core* model: replay determinism is a heavy constraint on user code and a significant learning curve, and the DAG covers the large majority of orchestration needs. Retained as an optional second authoring mode in Phase 4 for users who need it.
