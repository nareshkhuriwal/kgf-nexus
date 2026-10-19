# ADR-0037: Live infrastructure — the service-instance registry and attempt placement

- **Status:** Proposed — pending owner sign-off (implemented: migrations 0041, 0042)
- **Date:** 2026-10-19
- **Deciders:** owner, platform team
- **Refines:** ADR-0005 (pull-based workers), ADR-0007 (executors), ADR-0010
  (lease fencing), ADR-0011 (table ownership, rolling deploys), ADR-0017
  (platform operators)
- **Operations:** OPERATIONS §31. **Migrations:** 0041, 0042.

## Context

The console's Infrastructure page is to show, live, every process that makes
up an installation on an existing AKS cluster: each scheduler replica, each
control-plane instance, each worker agent and each running task with its pod
and node. Before this decision the platform could answer only part of that:

- **Workers** are rows in `workers` (ADR-0005): host name, capacity, heartbeat.
- **Control-plane processes** were visible to nobody but whoever could reach
  each one's `/readyz`. `GET /v1/health/fleet` reports the gateway's own gRPC
  connectivity to one address per service -- a load-balanced address, behind
  which a dead replica looks exactly like a live one.
- **Where a task runs** was known only as "on worker W". For the kubernetes
  executor the workload is a separate pod that the cluster schedules onto any
  node (ADR-0007), so "which pod do I `kubectl logs`, and on which node" had
  no answer short of searching the cluster by the attempt label.

What the code establishes:

- Each service already computes its readiness in its health handler, each in
  its own way: a draining flag, a database ping, the scheduler's loop error,
  the KGF context keys.
- Every attempt RPC is fenced on the lease token in constant time
  (`workergrpc.fence`), and `leases.id` and `leases.epoch` identify the exact
  lease that passed.
- Terminal transitions lock the attempt row `FOR UPDATE ... SKIP LOCKED`; a
  side table that never locks the attempt is the established way to store
  report-path data about it (0024 `attempt_progress`, 0036 `attempt_usage`).
- Service pools are deliberately small and an installation's Postgres
  connection budget is shared (`NEXUS_DB_MAX_CONNS`).

## Decision

### 1. Runtime identity

Every process reads where it runs from four variables the Helm chart sets from
the downward API and the release: `NEXUS_POD_NAME`, `NEXUS_POD_NAMESPACE`,
`NEXUS_NODE_NAME`, `NEXUS_CLUSTER_NAME` (`internal/instances/identity`, no
dependency beyond the standard library, shared by the agent). Values are
trimmed, stripped of control characters and bounded at 253 bytes. Outside
Kubernetes they are empty; the registry then falls back to the host name for
the instance, and the agent reports no pod.

### 2. The service-instance registry (migration 0041)

`service_instances` holds one row per control-plane process, keyed by
(component, cluster, pod namespace, instance). Each api-gateway,
workflow-manager, dag-engine, scheduler, resource-manager and event-service
process runs one heartbeat (`internal/instances`) that upserts its own row
immediately and every 10 seconds, with its version, process start, readiness
and a bounded JSON detail. `last_seen_at` is the **database** clock. An
instance is **stale** when its row is older than 30 seconds (three beats,
judged against the database's own `now()`); rows unseen for 24 hours are
pruned by the processes themselves, each at most every 10 minutes.

**Readiness is the process's own.** The heartbeat asks the service's `/readyz`
handler in-process (`instances.ProbeHandler`) and records ready for a 2xx,
with the handler's own reason otherwise. Calling the handler rather than
re-deriving its checks means the registry and the load balancer can never
disagree, and a check added to one is in the other.

**Graceful shutdown** stops the heartbeat where the service fails its
readiness: one last row with `ready = false` and `detail.shutting_down`, then
nothing, so the instance turns stale 30 seconds later and is pruned after a
day. `instances.Start` returns a stop function that waits for that write;
services defer it after their pool's `Close`.

**Off the hot path.** One statement per beat on the service's existing pool
under a 2-second deadline, no second pool (connections are budgeted per
installation), and never an error to anyone: a failed beat is counted in
`nexus_instance_heartbeat_failures_total{op}` and logged once per outage. A
process whose pool is saturated or whose database is unreachable for 30 seconds
shows as stale, which is exactly when an operator should look at it.

**One writer per row, not per table.** Every service role gets SELECT, INSERT,
UPDATE and DELETE on the table, through `nexus_role()`. ADR-0011's
one-owner-per-table rule exists to protect authoritative state; this table has
none -- nothing reads it to schedule, fence or authorize -- so a forged row can
mislead a dashboard and nothing else, and a service able to forge one already
writes tables that matter far more. Row-level security keyed on the login was
considered and rejected (Alternatives).

**Detail (§3).** Every row carries `readiness`, `pid`,
`heartbeat_interval_seconds`, `db_pool` (max, total, acquired, idle,
empty_acquires_total) and, on the last write, `shutting_down`. Components add
keys read from counters they already keep, never from a query: the scheduler
its `queue`, claims, claim errors, dispatches and losses since start and since
the previous beat, and its queue's PENDING backlog from its existing sampler;
the api-gateway whether it offers placement reporting. The detail is capped at
16 KiB by a CHECK; an oversized one is replaced by a marker rather than failing
the beat.

### 3. Attempt placement (migration 0042)

The worker API gains `ReportPlacement(attempt, lease token, AttemptPlacement)`
and `RegisterResponse.placement_reporting`. The placement is the executor,
the registered cluster, and the Kubernetes namespace, pod and node:

| Executor | Pod and node reported |
|---|---|
| kubernetes | the attempt's own pod: its name and namespace when it exists, and `spec.nodeName` once it is bound, from the executor's informer snapshots (`TaskSpec.OnPlacement`) |
| subprocess | the agent's own pod, namespace and node: the workload runs beside the agent |
| docker | the agent's own pod and node; a docker agent outside Kubernetes reports the container's short id as the pod, the handle an operator can `docker inspect` on the worker host |

The agent sends a report whenever the placement changes, latest first, from
one goroutine per attempt that never blocks the executor; a transport failure
is retried three times, a refusal is final, and Unimplemented turns reporting
off for the session.

The gateway stores it in `attempt_runtime`, its own table (like the log index,
placement is metadata about an attempt and changes nothing in its lifecycle,
so it needs neither the scheduler's transaction nor a lock on the attempt
row). The write is **fenced in the statement**: it goes through the exact
lease the constant-time token check found (`leases.id`), only while that lease
is unexpired by the database clock, and never over a row from a newer lease
epoch. The row references the run (cascade) and inherits its tenant from the
attempt (0014's trigger). The gateway offers the RPC only when it finds the
table at startup, so a gateway rolled out before 0042 tells agents not to
report.

### 4. What a namespace sees

Two routes, both guarded by `infrastructure.view` like the platform routes
beside them:

- `GET /v1/namespaces/{ns}/platform/task-runners` lists the namespace's
  in-flight attempts with their placement and counts by executor and cluster.
  This is the namespace's own data -- its runs, on its own workers -- scoped
  through the run exactly as the attempt reads are, and nothing is masked.
- `GET /v1/namespaces/{ns}/platform/components` lists the registry. **The
  control plane belongs to the installation, not to the namespace.** On a
  shared installation (KGF Cloud, ADR-0033) a tenant's infrastructure viewer
  must see whether the platform is healthy, not the provider's pod names,
  nodes, cluster names or the whole platform's dispatch rates. So by default:
  - a **platform operator** (ADR-0017, `platform_operators`) sees everything;
  - every other caller sees **component health only**: each instance's state,
    version, start and last-seen times and readiness reason, under an opaque
    label `<component>-<12 hex>` (a SHA-256 of its full identity: stable
    across polls and gateway replicas, distinct per instance, revealing no
    name), with pod namespace, node and cluster empty and the detail cut to
    `readiness`, `shutting_down` and `heartbeat_interval_seconds`.

  A single-organisation installation -- the common case of Nexus on one's own
  AKS cluster, where whoever administers the namespace also runs the cluster
  -- sets `NEXUS_COMPONENT_TOPOLOGY=infrastructure-viewers` on the gateway to
  show the full view to every `infrastructure.view` holder. The response says
  which view the caller got (`topology_visible`). A malformed value stops the
  gateway: guessing would either publish the topology or hide it.

## Consequences

**Positive.**

- Every control-plane replica is visible with its own readiness, version and
  placement, and a dead replica is distinguishable from a live one behind the
  same service address.
- Every running task names its pod and node; a pod stuck unscheduled shows its
  pod with no node, beside the executor's progress line.
- Old agents, old gateways and databases without the migrations keep working:
  the fields are simply empty.
- No new pool, no query on any hot path, and no lock on the attempt row.

**Negative.**

- Thirty writes a minute per replica of the six services, and an extra
  readiness evaluation every 10 seconds (the scheduler's includes its PENDING
  count query, as its `/readyz` does).
- A process's clock sets `started_at`; a badly skewed host shows an odd start
  time (staleness is unaffected: it uses the database clock).
- Any service role can overwrite any registry row. Acceptable for a table with
  no authority (§2), but a compromised service could make the console lie.
- A gracefully stopped instance stays listed, stale, for up to a day; the
  console must present stale rows as history, not as an outage.
- Self-hosted operators must either enrol as platform operators or set
  `NEXUS_COMPONENT_TOPOLOGY` to see pods and nodes of the control plane.
- An attempt that finishes before its first report lands records no placement;
  the report is advisory by design.

**Neutral.**

- The agent's placement is self-reported, like its capacity; it is fenced
  against a stale lease but not verified against the cluster.

## Alternatives considered

- **A separate one-connection pool for the heartbeat.** Independent of a
  saturated service pool, but one more connection per replica against a
  budgeted server, and it would report a replica whose real work is starved
  as healthy. Rejected.
- **Probing each replica's `/readyz` from the gateway.** The gateway would need
  every pod's address (Kubernetes API access the gateway does not have and
  should not need), would not work in compose, and would put a fan-out of HTTP
  calls on a console poll. Rejected.
- **Deleting the row on graceful shutdown.** Simpler display, but loses the
  difference between "stopped an hour ago" and "never existed", which is what
  an operator investigating a deploy needs. Rejected in favour of not-ready,
  then stale, then pruned.
- **Row-level security so each service writes only its component's rows.**
  Needs the login-to-component mapping in policies that must follow
  `NEXUS_DB_ROLE_PREFIX` renames, for a table nothing trusts. Rejected; the
  grant is documented as the exception.
- **Placement as fields on ReportStarted or ReportProgress.** ReportStarted is
  sent before a kubernetes pod exists; ReportProgress is forwarded to the
  scheduler, which would have to write a second table in its progress
  transaction. A dedicated, idempotent, latest-wins RPC is simpler for both
  sides. Rejected.
- **Columns on `task_attempts`.** The attempt row is the one every terminal
  transition locks with `SKIP LOCKED` (0024's reasoning). Rejected.
- **Showing the full registry to every `infrastructure.view` holder.** Right
  for a single organisation, wrong for a shared installation where tenants
  hold that permission in their own namespaces; hence the opt-in instead of
  the default.
