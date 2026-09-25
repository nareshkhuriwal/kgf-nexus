# kubernetes_executor

Two tasks, `produce` then `summarize`, each run as a **Kubernetes Job** by the
`kubernetes` executor (ADR-0007). What it shows:

- `runtime.executor: kubernetes` routes the task only to workers that
  advertise it. An agent advertises it only when `NEXUS_K8S_NAMESPACE` is set
  **and** its startup probe passes: cluster reachable, server ≥ 1.29, and the
  RBAC verbs the executor needs.
- `resources.request` becomes pod `requests == limits`. Memory is a cgroup
  limit (going over it gives `ERROR_CLASS_OOM`, which is retryable). CPU is a
  CFS quota.
- `start_to_close` becomes the Job's `activeDeadlineSeconds`. The cluster
  enforces it even if the agent dies.
- An upstream output reaches the next task as a file under `$NEXUS_INPUT_DIR`,
  staged through a Job-owned, immutable Secret. It never passes through the
  control plane.
- The pod runs with the restricted Pod Security settings: uid 65532, a
  read-only root, all capabilities dropped and no service-account token. A task
  can write only under `/nexus` and `/tmp`.

The file is called `spec.yaml`, not `compiled.yaml`, on purpose. The chaos
suite (`tests/chaos/examples_test.go`) runs every `examples/*/compiled.yaml`
against the docker-compose fleet, and that fleet has no kubernetes worker.
There, this example would stay PENDING and fail the suite for a reason that has
nothing to do with the example.

## Run it

You need a control plane (for example `make up`) and an agent with the
kubernetes executor enabled. Either of these gives you that agent:

**Out of cluster, against Docker Desktop's Kubernetes** (for development):

```bash
kubectl config use-context docker-desktop
kubectl apply -f deploy/k8s/executor-namespace.yaml     # namespace nexus-exec, PSA restricted
NEXUS_CONTROL_PLANE=localhost:9091 NEXUS_API_KEY=$NEXUS_WORKER_API_KEY \
NEXUS_K8S_NAMESPACE=nexus-exec NEXUS_K8S_CONTEXT=docker-desktop \
NEXUS_WAL_DIR=/tmp/nexus-k8s-wal NEXUS_LOG_FORMAT=text \
  go run ./agent/cmd/nexus-agent
# log line to look for: registered ... executors=[subprocess docker kubernetes k8s]
```

**In cluster:** apply `deploy/k8s/executor-namespace.yaml`,
`agent-rbac.yaml`, `executor-policies.yaml` and `agent-deployment.yaml`, in that
order. Set the agent image and `NEXUS_CONTROL_PLANE` first. See
[docs/OPERATIONS.md](../../docs/OPERATIONS.md#13-kubernetes-executor).

Then submit the example and start a run:

```bash
nexusctl workflow submit -f examples/kubernetes_executor/spec.yaml
nexusctl run create kubernetes-executor-demo
kubectl -n nexus-exec get jobs,pods -l app.kubernetes.io/managed-by=nexus-agent -w
```

Each attempt appears as a Job named `nexus-<attempt_id>`, labelled with its
run, node, attempt and worker. When the attempt finishes, the agent deletes the
Job. `summarize` produces `{"input":[1,2,3,4,5],"idempotency_key":"…"}`.
