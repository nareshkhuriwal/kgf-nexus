# Kubernetes executor

An agent can run each attempt as a **Kubernetes Job** in its own namespace
([ADR-0007](../design/adr/0007-executor-abstraction.md)). Kubernetes is one
executor among several, not the system of record: run state stays in
PostgreSQL, so large workflows do not strain etcd.

## Use it from a task

```yaml
runtime:
  executor: kubernetes            # alias: k8s
  image: registry.example.com/etl@sha256:...
  command: [python, -m, nexus.entrypoint, --module, dag]
  args: [etl.extract]
resources:
  request: {cpu_millis: 1000, memory_bytes: 2147483648}
timeouts: {start_to_close_ms: 3600000}
```

- The task is routed only to agents that **advertise** `kubernetes`. An agent
  advertises it only when `NEXUS_K8S_NAMESPACE` is set **and** its startup
  probe passes: the cluster is reachable, the server is 1.29 or later, and the
  agent holds the RBAC verbs the executor uses.
- `resources.request` becomes pod `requests == limits`. Memory over the limit
  is `oom`, which is retryable.
- `start_to_close` becomes the Job's `activeDeadlineSeconds`, so the cluster
  enforces it even if the agent dies.
- Inputs are staged through a Job-owned, immutable Secret and appear under
  `$NEXUS_INPUT_DIR`. Outputs are collected by a sidecar. Payloads never pass
  through the control plane.
- Pods run under Pod Security **restricted**: uid 65532, read-only root, all
  capabilities dropped, and no service-account token. A task may write only
  under `/nexus` and `/tmp`.

## Deploy an agent

The manifests are in [`deploy/k8s`](repo:deploy/k8s/), and the procedure is in
[runbook §13.1](../operations/runbook.md#131-deploying):

```bash
kubectl apply -f deploy/k8s/executor-namespace.yaml   # nexus-exec, PSA restricted
kubectl apply -f deploy/k8s/agent-rbac.yaml           # SA + Role; no exec, no secret reads
kubectl apply -f deploy/k8s/executor-policies.yaml    # default-deny NetworkPolicy + LimitRange
kubectl -n nexus-exec create secret generic nexus-agent-credentials \
    --from-literal=api-key="$NEXUS_WORKER_API_KEY"
kubectl apply -f deploy/k8s/agent-deployment.yaml     # pin the image digest first
```

Or install the whole platform with Helm ([runbook §20](../operations/runbook.md#20-installing-with-helm)).
Configuration variables are listed in [runbook §13.2](../operations/runbook.md#132-configuration),
and failure behaviour in [§13.3](../operations/runbook.md#133-what-happens-to-attempts-when-things-fail).

## Advertise honest capacity

Two schedulers are involved: Nexus places the attempt on the agent, then
Kubernetes places the pod. Set `NEXUS_CPU_MILLIS`, `NEXUS_MEMORY_BYTES` and
`NEXUS_DISK_BYTES` to what the namespace can really run, which is its
ResourceQuota or node pool size. Each attempt pod also carries a 50m/32Mi
collector sidecar. If you over-advertise, pods sit `Pending` until
`NEXUS_K8S_START_TIMEOUT` (default 10 minutes), then fail as retryable
`infrastructure`.

## Try it

[`examples/kubernetes_executor`](repo:examples/kubernetes_executor/) runs a
two-task spec against Docker Desktop's Kubernetes, with an out-of-cluster
agent. `nexusctl import argo` produces `executor: kubernetes` tasks from Argo
container templates ([Argo guide](../migrate/argo.md)).
