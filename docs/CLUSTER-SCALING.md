# Cluster and scaling

A Nexus namespace is the unit of policy. A **registered cluster** is the unit of
Kubernetes observation and, when explicitly enabled, of worker scaling. The
console never infers a cluster from a task profile's cloud label: a cluster
exists for Nexus only once it is registered and its controller reports.

## Who scales what

Exactly one writer per target. Two controllers writing one replica count fight,
and the loser's drains are wasted.

| Target | Scaled by | Signal |
|---|---|---|
| Scheduler Deployment | Kubernetes HPA (`deploy/k8s/cluster-controller/base/scaling.yaml`, or the chart's) | CPU utilization |
| That HPA's min/max replicas, CPU target, scale-down window | Nexus cluster controller, only on an HPA that opts in ([ADR-0036](adr/0036-scheduler-autoscaling-applied-by-the-controller.md)) | the namespace's saved scheduler settings |
| Worker Deployments bound to a cluster | Nexus cluster controller | queued demand (`platform.Plan`) |
| Nodes | AKS cluster autoscaler (or EKS managed node groups) | unschedulable pods |

Nexus does **not** build a node autoscaler. Worker pods that do not fit make the
cluster autoscaler add nodes; nodes emptied by worker scale-in are removed by it.
Task pod requests (the profile's CPU/memory/GPU) are separate from node VM
capacity, and the controller reports Nodes for context only. Utilization is the
wrong signal for a work queue (see `internal/platform/planner.go`), which is why
workers scale on demand and only the scheduler -- whose load really is CPU --
uses the HPA.

## Model

Migration `0018_cluster_registry` keeps desired and observed state in different
tables with different owners (ADR-0011):

| Table | Owner (writes) | Holds |
|---|---|---|
| `clusters` | api-gateway | name, provider, provider resource ID, expected kube-system UID, control-plane namespace, mode (`plan`/`apply`), `desired_revision` |
| `cluster_namespace_bindings` | api-gateway | worker Deployment ↔ Nexus pool, with a registry ceiling, scale-down step and cooldowns |
| `cluster_status` | resource-manager | the controller's last self-report: `observed_revision`, observed UID, Kubernetes version, leader identity, effective mode and why, last error |
| `cluster_observations` | resource-manager | latest snapshot of scheduler/worker Deployments, Nodes, task Jobs and the HPA, with `generation`, `observed_generation`, `observed_at` |
| `cluster_scaling_state` | resource-manager | the controller's memory that must survive restarts: last scale time/direction, idle-since, last decision; for a scheduler HPA (target `hpa/<name>`), its last patch time and reconcile status |
| `cluster_reconcile_errors` | resource-manager | recent failures (pruned to 500 per cluster) |
| `cluster_scaling_events` | resource-manager, INSERT-only | every decision that changed and every action taken or withheld, scheduler HPA changes (`hpa_plan`, `hpa_update`, `hpa_refused`) included |

**Why these owners.** Registry rows are operator intent, like `platform_config`,
so the gateway owns them. Observed rows describe the fleet, which the resource
manager already owns (`workers`); it is also the service that drains workers. The
controller has **no database credential**: it reads its assignment and writes
everything through the resource manager's `ClusterControllerService`
(`services/resource-manager/proto/nexus/rm/v1/cluster.proto`). The resource
manager reads `platform_config`, the registry and the scheduler's `leases`, all
reads ADR-0011 permits; 0018 grants it `SELECT` on `platform_config`.

**Tenancy.** A cluster belongs to the namespace that registered it. A binding's
namespace must equal the cluster's (a composite foreign key), one pool is scaled
by at most one Deployment, and one Deployment has at most one binding. Sharing a
physical cluster between tenants needs platform-operator authorization that does
not exist yet.

**Revisions.** Every registry change bumps `desired_revision`. The controller
reports `observed_revision = desired_revision` only after a cycle in which every
binding reconciled and every kind was observed. The console shows both; "saved"
is never presented as "applied".

## The controller (`services/cluster-controller`)

Leader-elected through a Kubernetes Lease (`nexus-cluster-controller-<cluster>`);
the standby does nothing but wait. Every 15 s the leader:

1. **Observes** worker Deployments (by name, from the bindings), scheduler
   Deployments (`app.kubernetes.io/component=scheduler` in the control-plane
   namespace), HPAs there, Nodes, and task Jobs (the kubernetes executor's
   label `nexus.dev/attempt`, attempt and run ids from its `nexus.dev/attempt-id`
   and `nexus.dev/run-id` annotations, newest 500) in the task namespaces
   (`NEXUS_CLUSTER_TASK_NAMESPACES`: where the executor runs its Jobs; default
   the bound worker namespaces). A kind whose list fails keeps its previous
   rows and is not marked complete -- "no Nodes" and "could not list Nodes" never
   look the same.
2. **Plans** each binding with `platform.Plan` on demand from the resource
   manager, counting drained pods as not-capacity. Idle time is persisted, so a
   restart does not reset it. It also compares each scheduler HPA with the
   namespace's saved scheduler settings (below).
3. **Applies**, only in apply mode:
   - *Scale-out*: bounded by the pool max, the registry ceiling (the smaller
     wins), the scale-up step, a scale-up cooldown and the worker namespace's
     `ResourceQuota` headroom. A quota on a resource the pod template does not
     declare, or whose usage is not yet calculated, means zero headroom.
   - *Scale-in*, never directly: (a) after the pool has been idle for its
     cooldown and the scale-down cooldown has passed, the controller asks the
     resource manager to **drain** one batch (default one pod) -- ACTIVE workers
     registered from that pod go DRAINING, which stops dispatch to them; (b) in a
     later cycle, it asks for a **fresh** removability check -- no ACTIVE or
     QUARANTINED worker, no lease, no reservation, counting DEAD rows too; (c)
     only then does it set `controller.kubernetes.io/pod-deletion-cost` on that pod
     and lower the replica count, with a `resourceVersion` precondition.
     Scale-in is held unless every pod is scheduled, Running and Ready with no
     rollout in progress, because only then is deletion cost the deciding rank
     for the ReplicaSet.
   - *Scheduler HPA settings*: see "Scheduler autoscaling" below.
4. **Reports** the snapshot, effective mode and any error.

The pod ↔ worker join is the agent's hostname, which in a pod is the pod name.
Worker Deployments must not override it (`base/worker-deployment.example.yaml`).
The join is scoped to the cluster: a worker whose agent names another cluster
(`NEXUS_CLUSTER_NAME`, the `nexus.io/cluster` label) never matches, so two
clusters registered by one namespace cannot see or drain each other's
same-named pods. A worker naming no cluster still matches; set
`NEXUS_CLUSTER_NAME` on agents (the chart does) once a namespace has more
than one cluster.

### Scheduler autoscaling ([ADR-0036](adr/0036-scheduler-autoscaling-applied-by-the-controller.md))

The console saves four scheduler settings per namespace: min and max replicas,
the CPU target and the scale-down cooldown. The controller compares them with
every HPA in the control-plane namespace whose scale target is a scheduler
Deployment:

- **Plan mode** records one `hpa_plan` event per distinct intended change and
  writes nothing.
- **Apply mode** patches only an HPA annotated
  `nexus.khuriwalgroup.com/autoscaling-owner: cluster-controller`, and only
  `minReplicas`, `maxReplicas`, the CPU utilization target and
  `behavior.scaleDown.stabilizationWindowSeconds`. The JSON patch pins the
  HPA's UID and the annotation, and carries the `resourceVersion` it was
  computed from (a Conflict is retried twice on a fresh read). Replica bounds
  move at most a factor of two per patch, and patches to one HPA are at least a
  minute apart (`NEXUS_CONTROLLER_SCHEDULER_PATCH_INTERVAL`; the last patch time
  is persisted). Each patch is an `hpa_update` event with before and after.
- **Left alone**, and shown as drift with the reason: an HPA without the
  annotation (Helm- or GitOps-owned; change its values there), an HPA without
  a CPU utilization metric, and settings never saved (`config_revision` 0).
- **Refused** patches (RBAC, a writer that changes the HPA under every
  attempt, a replaced object) are `hpa_refused` events and reconcile errors,
  and fail the cycle.

The console's cluster view reports `scheduler_autoscaling`: desired, observed,
`state` (`in_sync`, `plan_only`, `applying`, `drift`, `not_observed`),
`last_applied_at` and `last_error`, per HPA and for the worst one. A
`helm upgrade` or `kubectl apply` resets an owned HPA's four fields to the
rendered values until the next patch; exporting the saved settings as the
scheduler's Helm values (`schedulers[].autoscaling`) makes the re-render a
no-op.

### Why draining is enough to protect running work

The scheduler's dispatch claim selects `workers WHERE state='ACTIVE' ... FOR
UPDATE`. The drain's `UPDATE workers SET state='DRAINING'` therefore waits for any
claim holding the row, and every later claim skips it. Once the drain commits,
every lease that will ever exist on those workers already exists and is visible to
the removability check. DRAINING has no transition back to ACTIVE.

### Apply is three independent opt-ins

1. The registry entry is in `apply` mode **with** the expected kube-system UID
   (the API refuses apply without it).
2. The controller process runs with `NEXUS_CONTROLLER_ALLOW_APPLY=true`.
3. The apply Roles (`deploy/k8s/cluster-controller/apply/rbac-apply.yaml`:
   `deployments/scale` update and `pods` patch in the worker namespace, and
   patch on the scheduler HPA by name in the control-plane namespace) are
   bound.

The controller also requires the observed kube-system UID to match (1). The
resource manager re-checks mode, UID and binding inside the drain transaction, so
a controller in the wrong mode or pointed at the wrong cluster cannot drain.

### Residual risks (read before enabling apply)

- **Registration after drain.** An agent that restarts inside a drained pod
  registers a new ACTIVE row. The pre-removal check catches it (the pod is then
  not removable and is drained again), but there is a window of one API round trip
  between the final check and the ReplicaSet acting. The agent's own SIGTERM drain
  (`NEXUS_DRAIN_GRACE`, with `terminationGracePeriodSeconds` at least that) is the
  backstop.
- **Readiness flips in the same window.** If a busy pod turns NotReady between the
  stability check and the ReplicaSet acting, the ReplicaSet prefers it over the
  marked pod. Same backstop. The drained pod is removed in a later cycle.
- **Leader handover.** A demoted leader may act for up to `RenewDeadline` (10 s)
  after losing the Lease; every replica write carries a `resourceVersion`
  precondition, so it cannot overwrite a newer change.
- **Scale-in only from idle.** `platform.Plan` scales in only a fully idle pool
  (no pending, no running tasks in its queue); a partially busy pool is never
  shrunk. Deliberately conservative.
- **Cost.** No provider cost estimate is produced. The Azure development budget
  remains $50; budget alerts are not hard caps. Set registry ceilings accordingly.

## Operator flow

1. Define pod profiles and worker pools (Cluster & scaling → Task runtime /
   Worker pools) and save.
2. Register the cluster (Live cluster → Register a cluster). It starts in plan.
3. Install `kubectl apply -k deploy/k8s/cluster-controller/base` with
   `NEXUS_CLUSTER_NAME` set to the registered name -- or, for a Helm install,
   `clusterController.enabled=true` with `clusterController.clusterName`
   (docs/OPERATIONS.md §20.6, same RBAC); confirm the Live cluster tab
   shows a recent report, the observed UID and the scheduler HPA.
4. Bind worker Deployments; watch plan-only events for a representative period.
5. To apply: enter the kube-system UID the controller reports, confirm apply,
   and install `deploy/k8s/cluster-controller/apply`. See docs/OPERATIONS.md §12.

## Placement facts for multi-cluster dispatch (ADR-0031)

Besides observing and scaling, the controller publishes two facts the
scheduler's claim reads (OPERATIONS §28.5):

- **Cluster labels**: `NEXUS_CLUSTER_LABELS` (operator-declared, e.g. cost tier
  and data locality) over the observed `nexus.io/region` (most common
  `topology.kubernetes.io/region` of the Nodes) and `nexus.io/provider` (the
  Nodes' `spec.providerID` scheme). Published only from a complete Node list.
- **Quota headroom**: per task namespace (`NEXUS_CLUSTER_TASK_NAMESPACES`,
  default the bound worker namespaces), `min(hard - used)` over its
  ResourceQuotas for CPU, memory, GPUs (`NEXUS_CLUSTER_GPU_RESOURCES`), pods and
  other extended resources; `-1` where unconstrained. A namespace whose quota
  usage is not computed yet is omitted, and nothing is published if any list
  failed, so a partial view never replaces a complete one.

Both are stored in `cluster_status` (migration 0035) and resolved by the claim
through the worker's `nexus.io/cluster` label within its own tenant. They are
read-only observations: the bound namespaces' Role already lists
ResourceQuotas, a task namespace that is not a worker namespace gets a
read-only Role for Jobs and ResourceQuotas, and `observeNodes` already lists
Nodes. The console's cluster view shows both (`labels`, `headroom`,
`headroom_observed_at`).

## What is verified, and what is not

Verified: unit tests with the client-go fake clientset (scale-out, cooldown,
plan mode, apply gates, drain-before-removal, lease protection, readiness hold,
bounds, quota, Kubernetes and resource-manager failures, Conflict, restart,
per-kind observation, task Jobs in their task namespace, leader election;
scheduler HPAs in plan and apply mode, bounded steps and the persisted patch
interval, Conflict retry on a fresh read, a replaced or unowned HPA left
alone, an RBAC refusal recorded as a reconcile error); Postgres integration
tests running as the per-service roles (the cluster-scoped pod join, the
scheduler autoscaling record and view); and an end-to-end test on a local
Docker Desktop cluster (real ReplicaSet, pod-deletion-cost, RBAC via
ServiceAccount impersonation, Lease failover between two replicas, the
resource-manager binary).

Not verified: AKS, a real agent image in a worker Deployment, the cluster
autoscaler interaction, metrics-server-driven HPA behaviour, a scheduler HPA
patch against a real API server (the 409 on a stale resourceVersion is
emulated, as the API server documents it), sustained operation, and TLS
between controller and resource manager. No cloud resource was changed.
