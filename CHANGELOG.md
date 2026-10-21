# Changelog

Notable changes to KGF Nexus, newest release first.

`## X.Y.Z` is the release tagged `vX.Y.Z`. Its images are
`ghcr.io/<owner>/kgf-nexus/<image>:vX.Y.Z` and its Helm chart is
`oci://ghcr.io/<owner>/charts/kgf-nexus` version `X.Y.Z`. The release
workflow refuses a tag that does not match `version` in
`deploy/helm/kgf-nexus/Chart.yaml`. When a tag is pushed, the section for its
version becomes the GitHub Release's notes, followed by GitHub's generated
notes (`deploy/scripts/release-notes.sh`). A pre-release tag `vX.Y.Z-rc.N`
uses a `## X.Y.Z-rc.N` section if there is one, and `## X.Y.Z` otherwise.
[docs/OPERATIONS.md](docs/OPERATIONS.md) §22.4 describes the whole release.

Nexus is under active development and is not production-ready. See the status
note in the [README](README.md#kgf-nexus).

## 0.3.0

The first tagged release. It is chart 0.3.0, the eleven images tagged
`v0.3.0` for `linux/amd64` and `linux/arm64`, and the database schema through
migration 0042. The release adds a documented way to install Nexus on an AKS
cluster you already run, and a console that shows that installation live.

### Highlights

- **Install on an existing AKS cluster**
  ([ADR-0035](docs/adr/0035-component-releases-image-contract-existing-aks.md),
  [docs/INSTALL-AKS.md](docs/INSTALL-AKS.md)).
  - **Component-scoped releases.** With `components.controlPlane.enabled=false`,
    a release is a worker pool: agents only, pointed at a control plane
    installed by another release. A pool can be scaled, upgraded and placed
    apart from the control plane.
  - **Placement per component.** Schedulers can be pinned to a system pool,
    and the kubernetes executor's task pods to a GPU pool.
  - **Pod Security `restricted`.** Every pod the chart renders passes it,
    and so do the executor's task pods.
  - **Self-describing images.** Every image is unprivileged and states its
    ports, user and writable paths ([docs/IMAGES.md](docs/IMAGES.md)).
  - **Install tooling.** Terraform for an existing cluster
    (`deploy/terraform/environments/aks-existing`), and a read-only readiness
    check, `deploy/scripts/aks-preflight.sh`.
- **Live infrastructure**
  ([ADR-0037](docs/adr/0037-live-infrastructure-registry-and-placement.md),
  migrations 0041 and 0042).
  - Every control-plane process writes a heartbeat row to a service-instance
    registry.
  - Every attempt reports where it runs: the worker, and for the kubernetes
    executor the pod and the node.
  - `GET .../platform/components` and `GET .../platform/task-runners` list
    both.
- **Scheduler autoscaling, applied by the cluster controller**
  ([ADR-0036](docs/adr/0036-scheduler-autoscaling-applied-by-the-controller.md)).
  - The console's saved scheduler settings (replica bounds, CPU target,
    scale-down window) were saved but never applied. Now the in-cluster
    controller applies them.
  - Plan mode reports the change and writes nothing. Apply mode patches only
    an HPA that carries the controller's ownership annotation, and only its
    four settings. Each patch is a compare-and-swap, and every patch is
    audited.
  - A chart upgrade keeps the controller's values on the HPAs it owns.
- **Cluster & scaling, rebuilt.** The console's Infrastructure page is now a
  live, tabbed operator page.
  - Tabs: an overview of every layer's health, scheduler autoscaling (plan
    against observed), clusters, worker pools and task runners by pod and
    node.
  - An Install on AKS wizard generates the values and commands of
    docs/INSTALL-AKS.md, with preflight and pinned digests.
  - A worker token it mints is never kept in any client cache.
- **Agent executors you can name.**
  - `NEXUS_EXECUTORS` is the exact set of executors an agent offers.
    `NEXUS_DEFAULT_EXECUTOR` runs a task that names none.
  - The chart's `agents[].executors` and `defaultExecutor` set both.
  - A kubernetes-default worker claims only the unnamed tasks the kubernetes
    executor can run.
- **KGF.Nexus workspaces**
  ([ADR-0034](docs/adr/0034-kgf-nexus-workspaces.md), migration 0040). A
  workspace that KGF Cloud provisions gets its own namespace. Only KGF's
  provider front may name a workspace, and a workspace is never created on
  first use.
- **GitHub-only CI and releases to GHCR.**
  - CI is `.github/workflows/ci.yml`; the Azure DevOps pipelines moved to KGF
    Cloud.
  - Only after every check passes on a commit is anything published. Each
    push to `main` and each release tag publishes the eleven images to GHCR
    by digest, with build-provenance attestations, then the digest map and
    its Helm values, then the chart. For a tag, a GitHub Release follows.
  - The documentation site is published to GitHub Pages.

### Upgrade notes

- Apply migrations 0040 to 0042 with the migrate job before you roll the
  services (ADR-0011 rule 4). All three only add tables.
- **Agents that ran tasks as subprocesses by accident.** Before this release,
  an agent with `executors: kubernetes` still offered subprocess, and ran a
  task that named no executor inside its own pod. It now runs such a task as
  a pod, or leaves a task with no image waiting. To keep the old behaviour,
  add `subprocess` to its executors (OPERATIONS §20.14).
- The console image is nginx-unprivileged on port 8080. The Service port is
  still `ui.service.port`.
- The in-cluster postgres runs as uid 70, not root. An existing volume is
  already owned by uid 70.
- The chart no longer renders `NEXUS_HEARTBEAT_INTERVAL_MS`, which no agent
  read; the control plane sets the heartbeat interval. The
  `agentDefaults.heartbeatIntervalMs` value is still accepted and ignored.
- **The bootstrap's static worker key is now optional.**
  `bootstrap.registerWorkerKey: false` registers no static key, for
  installations whose agents use worker tokens minted by the console.

### Fixes

- **A lost reconcile call no longer strands a run.** Run advancement used to
  depend on one best-effort RPC; when that call was lost, the run stayed
  RUNNING forever (F160).
- **A kubernetes worker pool ran tasks inside its agent pod.** A task that
  named no executor ran there, with no resource isolation and off the pool's
  GPU nodes, and `NEXUS_EXECUTORS` was read by nothing (F161).
- **A drain in one cluster could drain another's workers.** When two
  clusters registered by one namespace had pods with the same name, each saw
  the other's workers. The pod-to-worker join is now scoped to the cluster.
- **Bootstrap Job failed on numeric-looking keys.** A workspace worker's
  secret key such as `123` or `true` rendered unquoted and failed Kubernetes
  validation; every key is now quoted.
- **The NVIDIA device plugin scheduled on no node.** In the AKS and EKS
  environments it now runs on the GPU pool.
- **pgcrypto on Azure Flexible Server.** The AKS platform module
  allow-lists the extension, so the first migration can create it.
- **Faster arm64 and amd64 builds.** Images cross-compile on the builder's
  platform rather than running the compiler under emulation.
- **Attestations and private repositories.** Provenance attestations are
  produced only when the repository is public. GitHub signs a private
  repository's attestations only on GitHub Enterprise Cloud, so a private
  release no longer fails after publishing.

### Known limitations

- Nexus has not passed its production acceptance gates.
- **Not yet built:** OIDC/SAML single sign-on, and workers resolving secret
  references.
- Inter-service TLS is opt-in.
- GPU execution is not yet qualified on real hardware.
- ADR-0034 to ADR-0037 are implemented and await the owner's sign-off.
- The current list is in [docs/README.md](docs/README.md#current-state), and
  open defects are in [CONTRACT-FINDINGS.md](CONTRACT-FINDINGS.md).
