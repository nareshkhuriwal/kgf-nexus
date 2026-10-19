# ADR-0035: Component-scoped releases, the image contract, and installing on an existing AKS cluster

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-10-19
- **Deciders:** owner, platform team
- **Refines:** ADR-0011 (six deployable services), ADR-0013 (one agent binary),
  ADR-0031 (cluster placement labels), docs/CLUSTER-SCALING.md
- **Related:** ADR-0024/0033 (Nexus inside KGF Cloud), ADR-0027 (Blob through
  workload identity), docs/IMAGES.md, docs/INSTALL-AKS.md, docs/OPERATIONS.md §20

## Context

The owner's goal is to install Nexus on an Azure AKS cluster that already
exists, with standard container images. That means the control plane,
schedulers and worker pools on AKS, Azure Database for PostgreSQL Flexible
Server and Blob Storage through workload identity. The console should then
show every scheduler replica, control-plane instance, worker agent and task
runner live, by pod and node.

Four things stood in the way.

- **One release was all or nothing.** The chart rendered the whole platform
  in every release: the six services, the schedulers, the console, the Jobs
  and the in-cluster dependencies. The agents dialed a hard-coded
  `api-gateway:9091` and `nats://nats:4222`. A GPU pool therefore could not
  be scaled, upgraded or placed apart from the control plane. A second release
  would have installed a second control plane. Agents in another namespace
  could not reach the first one, because a bare Service name resolves only in
  its own namespace.
- **No process said where it ran.** The console could list workers, but not
  which pod or node a scheduler replica, a gateway instance or an agent was
  on. Only the agents got the node name, and only for spot eviction.
- **The images were described by their consumers.**
  - The ports, users, probes and writable paths lived in the chart and the
    compose file, not in the images.
  - The binaries' default ports differ from the chart's convention. dag-engine
    defaults to 8092/8082, resource-manager to 50052/8082, and event-service
    to HTTP on 8085.
  - The console ran nginx as root on port 80. That forced the namespace down
    to Pod Security "baseline", and the in-cluster postgres did the same.
- **Placement was global.** `placement.*` applied to every Deployment alike.
  So the schedulers could not be pinned to a system pool, and the kubernetes
  executor's task pods could not be pinned to a GPU pool. The executor
  already read `NEXUS_K8S_NODE_SELECTOR` and `NEXUS_K8S_TOLERATIONS`, but
  the chart never set them.

## Decision

1. **A release declares the components it runs.**
   `components.controlPlane.enabled` defaults to true. Set to false, the
   release is a **worker pool**: its agents, their ServiceAccount, the
   executor Role and the task-API policy, and the cluster controller if
   enabled. Nothing else is rendered.
   - The agents dial `agentDefaults.controlPlane.address`, by default the old
     `api-gateway:9091`. A pool in another namespace uses the FQDN.
   - `agentDefaults.controlPlane.tls` sets the CA (`caSecretName`) and the
     name to verify (`serverName`) for an endpoint the release's own mTLS does
     not cover.
   - The broker is `agentDefaults.nats.url`. An empty URL means the agents
     long-poll.
   - The chart refuses a pool with no agents or no `existingSecret`, a pool
     whose mTLS would come from a self-signed CA the gateway does not trust,
     and pool NATS TLS without client material.

   The install shape for a pool is fixed:
   `helm upgrade --install <release> <chart> -n <ns> --set components.controlPlane.enabled=false --set existingSecret=<secret> -f <pool-values.yaml>`.
   Each pool release should go in its own namespace. The chart's names stay
   fixed per namespace (Services, the `nexus-agent` ServiceAccount, the agent
   Deployments), as they always were.

2. **Every Nexus process reports its runtime identity.** The chart sets four
   variables on every control-plane Deployment, every scheduler, the agents,
   artifact-gc and the cluster controller:

   | Variable | Source |
   |---|---|
   | `NEXUS_POD_NAME` | downward API |
   | `NEXUS_POD_NAMESPACE` | downward API |
   | `NEXUS_NODE_NAME` | downward API |
   | `NEXUS_CLUSTER_NAME` | `cluster.name`, the registered cluster. For the agents, `claimPlacement.clusterName` overrides it. |

   One template helper renders them for every caller. The binaries fall back
   to the hostname and an empty cluster when the variables are unset, so
   `docker run` and compose need nothing.

3. **The image is the contract** (docs/IMAGES.md). Each image:
   - carries OCI labels;
   - declares its binary's default listeners with `EXPOSE`. `Dockerfile.service`
     selects one runtime stage per component from the `SERVICE` argument it
     already took, so no caller changes;
   - runs as a numeric non-root user and declares its stop signal;
   - writes only to paths a volume provides.

   The console moves to NGINX's unprivileged image (uid 101, port 8080),
   pinned by digest. The chart's in-cluster postgres starts as uid 70. Every
   pod the chart renders then passes Pod Security **restricted**, and
   `deploy/helm/validate.sh` checks every render against it.

4. **Placement follows the workload.**
   - `schedulers[]` take `nodeSelector`, `tolerations` and `affinity`, which
     replace `placement.*`, plus a `topologySpread` merged over the global one.
   - `agentDefaults.kubernetes.taskNodeSelector` and `.taskTolerations` place
     the task pods.
   - Zone-then-node topology spread, PodDisruptionBudgets and
     `placement.priorityClassName` keep their defaults.

5. **The control plane may be fenced.** `networkPolicy.enabled` (off by
   default) renders an ingress default-deny for the control-plane pods, then
   one allow per real path:
   - the front door (the gateway's REST and worker gRPC, and the console)
     from `ingressFrom`, or from anywhere when it is empty;
   - service to service;
   - the database from its clients, and MinIO from its clients;
   - `/metrics` from `metricsFrom`.

   Egress is not restricted.

6. **An existing AKS cluster is installed from two example files.** They are
   validated as their `ci/` copies:
   - `examples/aks-existing-cluster.yaml`, the full install: Flexible Server
     over `verify-full` with runtime logins, Blob through workload identity,
     GHCR images, schedulers on the system pool, kubernetes-executor agents
     for a CPU pool and an optional GPU pool, the controller in plan mode, and
     NetworkPolicy;
   - `examples/aks-worker-pool.yaml`, a GPU pool as its own release.

   docs/INSTALL-AKS.md is the step-by-step guide. The chart is 0.3.0, and the
   release workflow publishes it as `oci://ghcr.io/<owner>/charts/kgf-nexus`.

## Consequences

**Positive**

- A pool scales, upgrades and moves on its own release cadence. It can live
  in another namespace or, through the gateway's external endpoint, in
  another cluster. Its blast radius is its own namespace.
- The console can place every instance on a pod and node with no Kubernetes
  API access from the services themselves.
- A namespace can enforce `pod-security.kubernetes.io/enforce=restricted` for
  the whole release, including the agents' task pods, which were already
  restricted.
- Anyone can run an image from its own metadata and docs/IMAGES.md. Nothing
  has to be reverse-engineered from the chart.
- With the chart, nothing changes for an existing values file except the
  intended renders:
  - the identity variables;
  - `RM_LOG_LEVEL`, so `logLevel` finally reaches resource-manager;
  - the console's port, probes and security context;
  - postgres's security context.

**Negative**

- **Two releases must agree on several settings, and nothing checks them
  together.** A pool's NKey must be in the control plane's
  `nats.auth.tenants`, and the control plane must admit the pool's namespace
  in `nats.networkPolicy.extraFrom`. Its worker key must belong to the right
  Nexus namespace, and its federated credential must name the pool's
  ServiceAccount. Each mistake surfaces at runtime as an agent that does not
  register, or does not get dispatch hints. The examples and
  OPERATIONS §20.16 list them.
- **The console's port changed.** Anything that addressed container port 80
  breaks: `nexusctl up` looked the console up on port 80 and needs the same
  one-line change. The Service port stays 80.
- **EXPOSE documents the binaries' defaults, not the chart's convention.** An
  image therefore shows 8092 for dag-engine while the chart runs it on 9090.
  The alternative was changing the binaries' defaults. That is a behaviour
  change for every compose and hand-run deployment, and it is left to its
  own decision.
- **A NetworkPolicy with `metricsFrom` unset blocks external scraping.**
  That is deliberate, and it is the first thing an operator enabling the
  policy will hit.

**Neutral**

- The release's chart version rose to 0.3.0 and appears in every object's
  `helm.sh/chart` label. The Job-name hash includes it, so the migrate and
  bootstrap Jobs re-run once on upgrade. Both are idempotent.
- The migrate image keeps the postgres base's inherited `EXPOSE 5432` and
  data `VOLUME`, which an image cannot remove. Nothing listens there.

## Alternatives considered

- **Separate charts for the control plane and the agents.** Rejected:
  - two charts would version and drift independently;
  - every shared helper (result store, identity, TLS, NATS client) would be
    duplicated;
  - a full install would become two installs in a fixed order.

  One chart with a component switch keeps one set of values, guards and
  examples.
- **Per-component switches** (`components.schedulers.enabled`,
  `components.ui.enabled`, …). Rejected for now. The split that operators
  need is control plane against workers. Finer switches would multiply the
  states validate.sh must cover, and `ui.enabled` already exists. The
  `components` block leaves room for them.
- **Agents looking up the control plane by DNS search domains or an
  ExternalName Service.** Rejected:
  - a cross-namespace FQDN is explicit;
  - it works the same from another cluster with the external address;
  - an ExternalName Service would hide TLS server-name mismatches.
- **Having the services read their pod and node from the Kubernetes API.**
  Rejected. The control-plane services hold no API token, and should not
  (OPERATIONS §20.13). The downward API gives the same facts with no
  permissions.
- **Keeping nginx on port 80 as non-root through the
  `net.ipv4.ip_unprivileged_port_start` sysctl.** It is allowed under
  restricted, but every runtime has to set it: Docker does by default,
  Kubernetes does not. Port 8080 needs nothing, and it is the unprivileged
  image's own convention.
- **An egress default-deny as well.** Rejected. The services reach endpoints
  the chart cannot enumerate: a managed database, Blob or S3, Entra ID for
  workload identity, an OTLP collector, SMTP, and webhook targets chosen at
  runtime. A wrong egress policy fails closed in ways that look like an
  outage of those services.
