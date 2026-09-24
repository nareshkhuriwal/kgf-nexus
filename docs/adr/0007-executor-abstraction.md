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

Phase 1 ships `subprocess` and `docker`. Phase 3 adds `k8s` (shipped as `kubernetes`; see Implementation notes).

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

## Implementation notes: the `kubernetes` executor (2026-09-24)

The Phase 3 executor exists: `agent/internal/executor/kubernetes*.go`. The ADR's decision stands as written. These notes record how the interface maps onto Kubernetes and where the fit is uneven.

**Kind and spelling.** The executor's `Kind()` is `kubernetes`. The agent also advertises `k8s`, which is what the schema, SDK and proto comment called it before the executor existed. Both spellings route to the same executor. The schema and SDK accept both.

**One Job per attempt.** The executor creates one Job per attempt with `backoffLimit: 0` and `podReplacementPolicy: Failed`, so Nexus is the only retry policy. The Job name is a pure function of the attempt id. If Prepare runs again for an attempt, or an agent restarts, the API server answers `AlreadyExists` and the executor **adopts** the existing Job instead of launching a second copy. The cluster itself decides "is this attempt already running", atomically.

**The six methods, on Kubernetes:**

| Method | Mapping |
|---|---|
| `Prepare` | Creates the Job and the inputs Secret, then waits until the task container **starts**. On Kubernetes the node pulls the image after scheduling, so the only way to learn that an image is missing is to submit the pod and watch the kubelet try. Prepare therefore covers "submit, schedule, pull". That keeps a missing image a terminal `NOT_FOUND` from Prepare, as the contract requires. |
| `Start` | Returns the handle and starts following the collector log. It cannot fail. |
| `Wait` | Watches Pod and Job state through one label-scoped informer pair per agent, not per-attempt polling. |
| `Signal` | TERM means delete the pod with `gracePeriodSeconds` equal to the task's grace; the kubelet sends SIGTERM and escalates to SIGKILL. KILL means a 1-second grace. Grace is never 0, because a force delete removes the API object while the process keeps running. |
| `Logs` | Streams `pods/log` of the task container. The log API interleaves stdout and stderr in write order. |
| `Cleanup` | Deletes the pods, the Job (background propagation) and the Secret. Every delete tolerates NotFound. |

**Enforcement fidelity.** Memory is `hard`: a cgroup limit with a kernel OOM kill, reported as reason `OOMKilled`. CPU is `cfs-quota`. GPU is `device-isolation`, through the configured device-plugin resource. Requests are set equal to limits, which puts task pods in the Guaranteed QoS class. Ephemeral storage is enforced by kubelet **eviction** on a sampling interval. That is softer than the other dimensions, and it reports as an eviction. Custom resources are not enforced; they exist only in the Nexus ledger. `start_to_close` becomes `activeDeadlineSeconds`, so it holds even when the agent is dead. The agent keeps its own timer as a backstop.

**Outputs.** The task writes its outputs into a pod-local volume, and they have to leave the pod after the task exits. Every obvious route is closed:

- `pods/exec` would let the agent run code in any pod.
- The termination message is limited to 4 KiB.
- `hostPath` breaks the restricted Pod Security profile.
- A pod-side upload would put result-store credentials into untrusted pods.

A **native sidecar** (the collector) solves this. It shares the output volumes read-only. The kubelet sends it SIGTERM only after the task container has exited, and on that signal it prints each output base64-framed to its own log, followed by a completion marker. A task that exits 0 but whose dump is incomplete is reported as INFRASTRUCTURE, never as a success with missing outputs. This needs Kubernetes 1.29 or later; the startup probe checks the version.

**Restart and fencing.** Each Job is owned by the agent pod (downward API), so deleting the agent pod garbage-collects its Jobs. On boot, the agent checks its WAL for STARTING entries:

- If the entry's Job **finished**, the agent reports the real outcome under the original lease token, instead of reporting INDETERMINATE.
- If the Job is **still running**, the agent kills it. This session cannot renew that lease, because renewal is scoped to the session's worker_id (§5.5), and a retry is about to be dispatched.

After that, the agent reaps any remaining Jobs carrying its owner label.

**Conformance.** The shared suite (`agent/internal/executor/conformance_test.go`; the ADR's `tests/conformance/` path predates it) has a `kubernetes` target and passes against a local Docker Desktop cluster. One subtest adapts rather than asserts identically: the no-orphan test checks that the Job and every pod of the attempt are gone, not that a grandchild pid is gone, because the pid lives on a node. The docker target makes the same substitution one level up.

**Nested scheduling.** Pod phase changes, for example "Pending: Unschedulable: 0/3 nodes are available: insufficient nvidia.com/gpu", go to `ReportProgress`. This is the mitigation this ADR proposed. The scheduler currently records only the heartbeat time from that RPC and drops the message, so the reason does not reach the UI yet. See CONTRACT-FINDINGS F92.
