# ADR-0036: Scheduler autoscaling settings applied by the cluster controller

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-10-19
- **Deciders:** platform team (owner decision: saved Scheduler and Worker-pool settings are applied by the in-cluster controller, with Plan/Apply modes, drift display and audit, and stay exportable as Helm values)
- **Refines:** docs/CLUSTER-SCALING.md ("Who scales what"), migration 0018 (the observed-cluster tables)
- **Related:** ADR-0011 (table ownership), ADR-0031 (cluster labels and headroom), ADR-0035 (component releases)

## Context

The console saves four scheduler autoscaling settings per namespace in
`platform_config.scheduler`: minimum and maximum replicas, the CPU utilization
target and the scale-down cooldown. The scheduler Deployment is scaled by a
Kubernetes HorizontalPodAutoscaler on CPU, which the Helm chart (or
`deploy/k8s/cluster-controller/base/scaling.yaml`) renders from its own
values. Nothing connected the two: the settings were saved, the controller
reported the HPA, the console showed the difference as drift, and nothing
ever applied anything. `platform.SchedulerHPA` existed with no caller.

The owner's decision is that saved settings are applied by the in-cluster
cluster controller, with explicit plan and apply modes, drift display and an
audit trail. The constraints are those the controller already lives by for
worker Deployments: one writer per target, nothing changed in plan mode,
nothing changed on the wrong cluster, every write a compare-and-swap, every
action recorded, and no database credential in the cluster.

The HPA is unlike a worker Deployment in one respect: something else already
writes it. Helm (or kubectl, Argo CD, Flux) renders it, and re-applies it on
every upgrade. A controller that silently rewrites what a GitOps tool owns
starts a fight that neither side reports.

## Decision

### 1. What the controller changes, and on which HPA

Each cycle the controller lists the HPAs in the control-plane namespace (it
always has) and takes as **scheduler HPAs** those whose `scaleTargetRef` is an
observed scheduler Deployment (`app.kubernetes.io/part-of=kgf-nexus,
app.kubernetes.io/component=scheduler`). On those it manages exactly four
fields: `spec.minReplicas`, `spec.maxReplicas`, the CPU `Utilization` metric's
`averageUtilization`, and `spec.behavior.scaleDown.stabilizationWindowSeconds`
(Kubernetes' 300 when unset counts as 300). Never the scheduler Deployment,
never its replica count (the HPA controller's), never another metric, policy
or label.

**Opt-in per HPA.** The controller writes an HPA only when it carries the
annotation `nexus.khuriwalgroup.com/autoscaling-owner: cluster-controller`
(`platform.HPAOwnerAnnotation`). The kustomize apply overlay adds it to the
scheduler HPA (`apply/scheduler-hpa-owner-patch.yaml`); the chart adds it to
its scheduler HPAs when the controller runs in apply mode with
`clusterController.apply.schedulerAutoscaling` (the default). An HPA without
it is reported -- the difference shows as drift, with the reason -- and never
written. This is how GitOps- and Helm-managed HPAs coexist with the
controller: whoever renders the HPA decides, per HPA, who owns its settings.

**Settings never saved are not applied.** Until a namespace saves its platform
config (revision 0), the "desired" values are the defaults, which nobody
chose; the controller reports `not_configured` and changes nothing.

### 2. Plan and apply

The cluster's effective mode is the one worker scaling already uses: apply
only when the registry says apply with the expected kube-system UID **and**
the process runs with `NEXUS_CONTROLLER_ALLOW_APPLY` **and** the observed UID
matches. In plan mode the controller records one `hpa_plan` event per
distinct intended change ("would set min 2→3, max 6→10 ... (platform config
revision 4)") and writes nothing. In apply mode it patches.

### 3. Safety of a patch

- **UID-pinned, owner-pinned, CAS.** The patch is a JSON patch (RFC 6902)
  whose `test` operations require the HPA's UID, the ownership annotation and,
  when the CPU target changes, the CPU metric at the same index; and whose
  `replace /metadata/resourceVersion` makes it a compare-and-swap (the API
  server answers 409 when the object changed since it was read). Only the
  fields that change are in the patch.
- **Conflict retry.** A 409 is retried twice on a fresh read of the same
  object -- the HPA controller writes status every sync, which makes the first
  read stale often enough to matter. A different UID on the re-read (the HPA
  was replaced) is refused; the replacement is planned from scratch next
  cycle.
- **Bounded change.** A patch moves each replica bound by at most a factor of
  two (and at least one): lowering max from 20 to 2 takes four patches (10, 5,
  3, 2). The CPU target and the window are set outright; they change how the
  HPA decides, and its behavior policies bound what follows. Patches to one
  HPA are at least `NEXUS_CONTROLLER_SCHEDULER_PATCH_INTERVAL` (1 minute)
  apart, measured from the last patch time persisted in the resource manager,
  so a restart or a leader change does not reset it. Together these bound how
  fast a saved setting resizes the scheduler fleet, and how often the
  controller rewrites an HPA another writer keeps reverting.
- **RBAC.** Patch on the scheduler HPAs, by name, in the control-plane
  namespace, bound only by the apply overlay
  (`apply/rbac-apply.yaml`) or the chart's apply-mode RBAC. The base install
  stays read-only.

### 4. Audit, state and the console

The controller reports each cycle's reconcile through a new resource-manager
RPC, `RecordSchedulerAutoscaling`. The resource manager writes, in one
transaction, an `hpa_plan` / `hpa_update` / `hpa_refused` row in
`cluster_scaling_events` and the controller's status (result, reason, error,
reconciled config revision) in `cluster_scaling_state`, both under the target
`hpa/<name>`. A `/` cannot occur in a Deployment name, so the HPA's rows never
collide with a worker Deployment's, and **no migration is needed**. The
resource manager refuses HPAs outside the cluster's control-plane namespace
and ignores a status reconciled against an older config revision. A refused
patch is also a reconcile error and fails the cycle.

The assignment (`GetAssignment`, new field `scheduler_autoscaling`) carries the
saved settings, their config revision and the persisted last-patch time per
HPA. The gateway's cluster view gains `scheduler_autoscaling`: desired,
observed (the HPA as last reported, after any patch in that cycle), and a
state -- `in_sync`, `plan_only` (plan mode, differs), `applying` (apply mode,
and the controller has not yet reconciled this revision or is moving in
bounded steps), `drift` (it reconciled this revision and cannot converge;
`last_error` says why), `not_observed` -- with `last_applied_at`.

### 5. Coexisting with Helm and GitOps

- **Chart-managed HPA, owned by the controller** (apply mode, the default):
  a `helm upgrade` re-renders the four fields from chart values; the
  controller restores the saved settings within one patch interval and
  records it. To avoid even that revert, export the saved settings as the
  scheduler's Helm values (`schedulers[].autoscaling`), so a re-render is a
  no-op, or have the chart render the live values of an owned HPA.
- **GitOps-managed HPA** (Argo CD, Flux with self-heal): leave the annotation
  off. The console shows drift with the reason; the operator changes the
  values in Git. Annotating such an HPA would make two controllers fight; the
  patch interval bounds the fight to one write a minute and the event trail
  shows it, but the fix is to pick one owner.

## Consequences

**Positive.** Saved scheduler settings take effect without a redeploy, with
the same plan/apply gates, cluster-identity check and audit trail as worker
scaling. Every write is attributable (`fieldManager: nexus-cluster-controller`,
an event row with before and after) and minimal (four fields, UID-pinned,
CAS). An HPA nobody opted in is never touched. No schema change.

**Negative.** A chart- or kubectl-managed owned HPA is reverted by every
re-apply until the controller's next patch: up to a minute at the chart's
values, and a large revert is undone in bounded steps (several minutes). Two
writers can still fight on an HPA someone wrongly annotates; the controller
bounds and reports it but cannot prevent it. `cluster_scaling_state` and
`cluster_scaling_events` now hold two kinds of target, told apart by the
`hpa/` prefix rather than by a column. Settings apply per namespace to every
scheduler HPA in the control-plane namespace: per-queue schedulers cannot
have different saved settings.

**Neutral.** The base (plan) install's RBAC is unchanged. A resource manager
that predates this sends no settings, and the controller then leaves HPAs
alone; a controller that predates it is shown as drift ("upgrade the cluster
controller").

## Alternatives considered

- **Let the chart render the saved settings.** The chart cannot read the
  database, and making it would put a credential in the deploy path. Exporting
  settings as Helm values remains available as the GitOps path.
- **Manage every scheduler HPA, no opt-in.** Silently fights every GitOps
  tool and every `helm upgrade`; rejected, it is the failure this ADR exists
  to avoid.
- **Server-side apply with a field manager instead of a JSON patch.**
  `spec.metrics` is an atomic list: applying the CPU target through SSA would
  take ownership of, and replace, every metric. A JSON patch touches one
  element, and its test operations give the UID and ownership checks for free.
- **Scale the scheduler Deployment directly.** Two writers (the HPA and the
  controller) on one replica count; rejected by the one-writer rule.
- **A new table (migration) for HPA state.** Cleaner typing, but a new
  ownership grant and a schema change for state that fits 0018's tables
  exactly; the `hpa/` target prefix keeps it additive and migration-free.
- **Apply the whole difference in one patch.** Lowering max from 20 to 2 would
  remove eighteen schedulers on the HPA's next sync; bounded steps cost a few
  minutes and keep dispatch capacity from collapsing on a mistyped value.
