# ADR-0031: Multi-cluster placement as claim-time eligibility and scoring

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0005 (pull dispatch), ADR-0017/0021 (the claim), docs/CLUSTER-SCALING.md
- **Related:** ADR-0029 (placement score), ADR-0030 (capacity type)

## Context

Migration 0018 registers Kubernetes clusters (`clusters`, desired state, owned
by the gateway) and records what each cluster's controller observes
(`cluster_status`, `cluster_observations`, owned by the resource manager). The
controller already reads the worker namespace's `ResourceQuota`s to bound its
own scale-out (`QuotaHeadroom`). Agents in different clusters register as
ordinary workers of their namespace; nothing records which cluster a worker is
in, and placement cannot express "run in eu-west", "only where the data is",
or "never on the expensive tier".

The requirement is to place work across several clusters, each with its own
agents and queues, honouring data locality, cost and each cluster's quota
headroom -- without giving up the pull model: workers anywhere, behind NAT,
dialling out, with backpressure for free (ADR-0005).

## Decision

### 1. Cluster membership and labels

- An agent names its cluster with the well-known label **`nexus.io/cluster`**
  (`NEXUS_CLUSTER_NAME`; the Helm chart sets it). The claim resolves it to
  `clusters.name` **within the worker's tenant** -- a worker cannot borrow
  another tenant's cluster labels by naming it.
- The cluster-controller publishes **cluster labels** with every report:
  operator-declared ones (`NEXUS_CLUSTER_LABELS`, e.g.
  `nexus.io/cost-tier=2,nexus.io/data-locality=s3-eu-west-1|lake-eu`) and
  observed ones: `nexus.io/region` (the most common
  `topology.kubernetes.io/region` of its Nodes) and `nexus.io/provider` (from
  the Nodes' `spec.providerID` scheme: `aws`, `azure`, `gce`, ...). They are
  stored in `cluster_status.labels` (migration 0035; resource-manager owned,
  like the rest of the observed state).
- A worker's **effective labels** for placement are its own labels overlaid
  with its cluster's labels. Cluster labels win on the `nexus.io/` keys the
  controller publishes, because the controller observes them and the agent
  merely claims them.

Well-known keys: `nexus.io/cluster`, `nexus.io/region`, `nexus.io/provider`,
`nexus.io/cost-tier` (integer, 0 = cheapest), `nexus.io/data-locality`
(`|`-separated tags), `nexus.io/capacity-type` (ADR-0030).

### 2. Task placement constraints and preferences

`PlacementSpec` (TaskSpec field 60) carries, besides ADR-0030's fields:

| Field | Kind | Semantics |
|---|---|---|
| `required_labels` | hard | effective labels must contain every pair (a superset of `node_selector` that also sees cluster labels) |
| `max_cost_tier` | hard | effective `nexus.io/cost-tier` ≤ this (0 = no limit; a worker with no tier is tier 0) |
| `preferred_data_locality` | soft | score bonus when the worker's `nexus.io/data-locality` tags intersect this list |
| `preferred_labels` | soft | score bonus per matching pair |

### 3. Per-cluster quota headroom

The controller also publishes, per task namespace it is configured to watch
(`NEXUS_CLUSTER_TASK_NAMESPACES`, default the bound worker namespaces), the
remaining room under that namespace's `ResourceQuota`s: CPU, memory,
`requests.nvidia.com/gpu` (and any configured MIG/shared resource names) and
pods, with the time it was observed (`cluster_status.headroom`,
`headroom_observed_at`).

The claim applies it to attempts that will create pods in that namespace -- the
kubernetes executor on a worker labelled `nexus.io/k8s-task-namespace` (set by
the agent from `NEXUS_K8S_NAMESPACE`). Such an attempt is **not eligible** on
that worker when the observation is fresh (younger than
`NEXUS_CLUSTER_HEADROOM_TTL`, default 2 min) and its request does not fit in
`headroom − Σ requests reserved on that cluster's workers since the
observation`. The subtraction accounts for dispatches the quota controller has
not seen yet. A stale or missing observation gates nothing.

This is a routing guard, not the enforcement point: the API server's
ResourceQuota admission is. Two replicas may both see the last unit of
headroom; the loser's pod is refused by the API server and the kubernetes
executor reports it (ADR-0007 progress), exactly as without this guard. What
the guard buys is that a cluster known to be full is not chosen while another
has room.

### 4. Pull model kept: eligibility plus scoring at claim time

There is no global placement component. Hard constraints are **eligibility**:
in the pull path they are SQL pre-filter clauses (so an attempt a worker can
never run cannot block its queue), headroom is a per-candidate skip, and in the
push path all of them filter the locked worker set. Preferences are the
ADR-0029 **score**: the push path picks the best worker; the pull path defers a
candidate to a strictly better live worker for at most `NEXUS_PLACEMENT_WAIT`.

Queues still route: a cluster can serve its own queues, and a task can target
one. Placement constraints make one queue span clusters when that is what the
author wants.

### 5. Why not a push scheduler

A central multi-cluster scheduler (Kueue's MultiKueue, Karmada, Armada) sees
the whole fleet and can compute better global placements, including gang
placement across clusters. It would need a network path *to* every cluster
(or a per-cluster agent that pulls from it -- which is what our workers already
are), a leader, and its own state. The deciding facts for Nexus: every
decision it would make still has to end in the §5.3 transaction on one worker
row; our pull claim already has the candidate row, the worker row and the
tenant quota lock in hand; and the placement quality lost by deciding
per-poll is bounded by the delay-scheduling wait. Rejected until a requirement
(gang scheduling across clusters) forces it.

## Consequences

**Positive** — data locality, cost ceilings and per-cluster headroom are
expressible and enforced at the only point that is exact (the claim);
clusters are added by running a controller and labelled agents; no new service.

**Negative** — headroom is only as fresh as the controller's report interval,
and the guard is advisory (the API server enforces). Cluster labels need a
running controller; without one only the agent's own labels count. The push
path resolves cluster labels per candidate worker (one join). A worker whose
`nexus.io/cluster` names no cluster of its tenant simply has no cluster labels.

## Alternatives considered

1. **Copy cluster labels into worker labels at registration.** No join at claim
   time, but labels would go stale when the controller's observation changes,
   and the agent would be trusted with cluster facts. Rejected.
2. **One queue per cluster, routing by queue only.** Works today; it pushes
   placement into every author's code and cannot express "any EU cluster" or a
   cost ceiling. Kept as a pattern, not the mechanism.
3. **A central push scheduler.** See §5.
