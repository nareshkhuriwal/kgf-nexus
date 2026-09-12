# ADR-0007: Pluggable executor abstraction in the worker agent

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

Tasks need somewhere to actually run, and the right substrate depends entirely on context:

- Local development and tests want a subprocess: fast, no image build, debuggable.
- Most production work wants a container: dependency isolation, reproducibility, enforceable resource limits.
- Kubernetes shops want each task as a Pod, so the cluster's own scheduler, autoscaler, and quotas apply.
- Untrusted multi-tenant code eventually wants stronger isolation (microVMs).

Committing to one substrate is how orchestrators end up unusable outside their assumed environment. Argo requires Kubernetes; Celery effectively requires the worker's own process.

The important question is *where* the abstraction lives. If executor choice is a control-plane concern, the control plane must understand Kubernetes APIs, container runtimes, and their failure modes — exactly the coupling plane separation exists to prevent.

## Decision

**The executor is an interface implemented inside the worker agent. The control plane knows nothing about execution substrates.**

```go
type Executor interface {
    Prepare(ctx context.Context, spec TaskSpec) (Prepared, error)
    Start(ctx context.Context, p Prepared) (Handle, error)
    Wait(ctx context.Context, h Handle) (Outcome, error)
    Signal(ctx context.Context, h Handle, sig Signal) error   // TERM then KILL
    Logs(ctx context.Context, h Handle) (io.ReadCloser, error)
    Cleanup(ctx context.Context, h Handle) error
}
```

The `runtime.executor` field in the task spec selects an implementation. A worker advertises which executors it supports at registration; the scheduler treats that as a placement constraint, exactly like a label — so executor selection is *routing*, not control-plane logic.

Phase 1 ships `subprocess` and `docker`. Phase 3 adds `k8s`.

**A conformance test suite** (`tests/conformance/`) defines the contract every executor must satisfy: resource limits are actually enforced; SIGTERM is delivered and the grace period honoured before SIGKILL; exit codes map correctly to error classes; logs stream; cleanup is idempotent; a killed executor process leaves no orphaned workload. New executors must pass it. This is what keeps the abstraction honest rather than nominal.

## Consequences

**Positive**
- The control plane stays substrate-agnostic; adding Kubernetes support changes no control-plane code.
- Local development uses the same control plane as production with a different executor — dramatically better developer experience than mocking out an orchestrator.
- Kubernetes becomes *one option*, not a prerequisite. Nexus runs on bare VMs, on a laptop, or on a cluster.
- The conformance suite makes "we support executor X" a testable claim.

**Negative**
- Each executor must independently implement limits, signals, and log capture. Real duplicated effort, and a real risk of subtle behavioural divergence — which is exactly why the conformance suite is part of this decision and not an afterthought.
- Resource enforcement fidelity differs by executor: `subprocess` cannot enforce memory limits as strictly as a container. Documented explicitly; `subprocess` is labelled development-grade.
- Nested scheduling with the `k8s` executor: our scheduler places the task on a worker, then Kubernetes places the Pod. Two schedulers can disagree, and diagnosing "why is my task pending" spans both. Mitigated by having the k8s executor report Pod-level pending reasons back through `ReportProgress` so the UI shows the real cause.

**Neutral**
- The interface will need widening for streaming inputs and GPU device plugins. The six methods are a starting point, not a frozen contract.

## Alternatives considered

**Control-plane executors (Airflow's model).** Executor choice as a deployment-wide control-plane setting. Rejected: it couples the control plane to every substrate, makes per-task executor choice impossible, and violates plane separation.

**Kubernetes-only (Argo/Flyte's model).** Simplest single implementation, and inherits a genuinely excellent scheduler and autoscaler. Rejected: it makes Kubernetes mandatory, which forecloses bare-VM, edge, and laptop deployment, and makes local development painful. We keep the option via the `k8s` executor without the mandate.

**Process-only (Celery's model).** Rejected: no dependency isolation, no real resource enforcement, and every task shares the worker's environment.
