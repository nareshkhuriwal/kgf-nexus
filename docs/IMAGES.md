# Nexus container images: the runtime contract

What each first-party image is, what it listens on, what it needs, and what it
writes. It holds for any runtime: `docker run`, Docker Compose, the Helm
chart, or your own Kubernetes manifests. Decision record:
[ADR-0035](adr/0035-component-releases-image-contract-existing-aks.md).
Installing on Kubernetes: [OPERATIONS.md §20](OPERATIONS.md#20-installing-with-helm)
and, on an existing AKS cluster, [INSTALL-AKS.md](INSTALL-AKS.md).

## 1. Names, tags, platforms

```
ghcr.io/<owner>/kgf-nexus/<component>:<tag>
```

| Component | Built from | What it is |
|---|---|---|
| `api-gateway` | `deploy/Dockerfile.service` | REST API, worker gRPC endpoint, authentication |
| `workflow-manager` | `deploy/Dockerfile.service` | workflows, runs, timers, schedules, sensors |
| `dag-engine` | `deploy/Dockerfile.service` | run reconciliation, task readiness |
| `scheduler` | `deploy/Dockerfile.service` | claims attempts for one queue |
| `resource-manager` | `deploy/Dockerfile.service` | worker registry, liveness, capacity |
| `event-service` | `deploy/Dockerfile.service` | outbox relay, run-event streams, notifications |
| `agent` | `deploy/Dockerfile.service` | the worker: registers, claims, runs task attempts |
| `artifact-gc` | `deploy/Dockerfile.service` | one result-store sweep, then exit |
| `cluster-controller` | `deploy/Dockerfile.service` | reports a Kubernetes cluster, applies saved scaling |
| `migrate` | `deploy/Dockerfile.migrate` | schema migrations (`psql`) and per-service logins (`nexus-db-roles`) |
| `ui` | `deploy/Dockerfile.ui` | the console: static bundle behind nginx, `/v1` proxied to the gateway |

- **Tags.** A release is `vX.Y.Z`; every push to `main` is also tagged with
  its commit (12 hex digits) and with `main`, which moves: never deploy it.
  There is no `latest`, and the chart refuses one. The release workflow
  (`.github/workflows/release.yml`, OPERATIONS §22.4) publishes a digest map;
  pin `<image>@sha256:...` from it.
- **Platforms.** `linux/amd64` and `linux/arm64`.
- **Provenance.** `gh attestation verify oci://ghcr.io/<owner>/kgf-nexus/<image>@sha256:<digest> --repo <owner>/kgf-nexus`.
- **Labels.** Every image carries `org.opencontainers.image.title`
  (`kgf-nexus/<component>`), `.description`, `.source`, `.version`,
  `.revision`, `.licenses` (`Apache-2.0`), `.vendor` and `.documentation`:
  `docker image inspect --format '{{json .Config.Labels}}' <image>`.

`Dockerfile.service` builds all nine Go images. The `SERVICE` build argument
picks both the main package and the runtime stage (`image-<SERVICE>`) that
becomes the image, so each image declares its own ports:

```sh
docker build -f deploy/Dockerfile.service \
  --build-arg SERVICE=scheduler --build-arg SERVICE_PATH=services/scheduler \
  --build-arg VERSION=v0.1.0 -t kgf-nexus/scheduler:v0.1.0 .
```

## 2. What every image shares

| | Go images | `migrate` | `ui` |
|---|---|---|---|
| Base | `alpine:3.21` + ca-certificates, tzdata | `postgres:16-alpine` | `nginxinc/nginx-unprivileged:1.30.5-alpine3.24`, pinned by digest |
| Entrypoint | `/usr/local/bin/nexus-service` (one static binary) | `/usr/local/bin/migrate.sh` | the nginx image's `/docker-entrypoint.sh` |
| User | `10001:10001` (`nexus`) | `10001:10001` (`nexus`) | `101:101` (`nginx`) |
| Stop signal | `SIGTERM`: stop taking work, drain, exit | `SIGTERM` | `SIGQUIT`: nginx's graceful stop |
| Root filesystem | read-only works (§5) | read-only works | read-only with two writable paths (§5) |
| Capabilities | none needed | none | none (it listens on 8080, not 80) |

Every container passes Kubernetes Pod Security **restricted** when run with
`runAsNonRoot: true`, a `RuntimeDefault` seccomp profile, `allowPrivilegeEscalation: false`
and `capabilities: { drop: [ALL] }`. The users are numeric, so
`runAsNonRoot` can verify them. Logs are JSON lines: on stdout for
api-gateway, workflow-manager and event-service; on stderr for the others.
artifact-gc writes text to stderr.

## 3. Ports and probes

`EXPOSE` lists each binary's **default** listeners, which is what a bare
`docker run` serves. The chart and compose move most of them to one
convention: HTTP on 8080, gRPC on 9090, and the worker gRPC endpoint on
9091. Set the variable in the last column to do the same.

| Image | Listener | Binary default | Chart and compose | Variable |
|---|---|---|---|---|
| api-gateway | REST, `/healthz`, `/readyz` | `:8080` | `:8080` | `NEXUS_REST_ADDR` |
| | worker gRPC (agents dial it) | `:9090` | `:9091` | `NEXUS_GRPC_ADDR` |
| | ops: `/metrics`, `/healthz`, `/readyz` | `:8088` | `:8088` (`metrics.gatewayPort`) | `NEXUS_METRICS_ADDR` (`off` disables) |
| workflow-manager | gRPC | `:9091` | `:9090` | `NEXUS_WFM_GRPC_ADDR` |
| | `/healthz`, `/readyz`, `/metrics` | `:8081` | `:8080` | `NEXUS_WFM_HEALTH_ADDR` |
| dag-engine | gRPC | `:8092` | `:9090` | `NEXUS_GRPC_ADDR` |
| | `/healthz`, `/readyz`, `/metrics` | `:8082` | `:8080` | `NEXUS_HTTP_ADDR` |
| scheduler | gRPC | `:9090` | `:9090` | `NEXUS_GRPC_ADDR` (or `GRPC_ADDR`) |
| | `/healthz`, `/readyz`, `/metrics` | `:8081` | `:8080` | `NEXUS_HTTP_ADDR` |
| resource-manager | gRPC (and gRPC health) | `:50052` | `:9090` | `RM_GRPC_ADDR` |
| | `/healthz`, `/readyz`, `/metrics` | `:8082` | `:8080` | `RM_HTTP_ADDR` |
| event-service | HTTP only: run-event streams, `/healthz`, `/readyz`, `/metrics` | `:8085` | `:8080` | `HTTP_ADDR` |
| agent | ops: `/metrics`, `/healthz`, `/readyz` | `127.0.0.1:8089` (loopback) | `:8089` (`metrics.agentPort`) | `NEXUS_METRICS_ADDR` |
| | task API for kubernetes-executor task pods (TLS) | off | `:9443` | `NEXUS_K8S_TASK_API_LISTEN` / `_ADVERTISE` |
| artifact-gc | none (one sweep, then exit) | off | off | `ARTIFACT_GC_METRICS_ADDR` serves `/metrics` during the sweep |
| cluster-controller | `/healthz`, `/readyz`, `/metrics` | `:8090` | `:8090` | `CONTROLLER_HTTP_ADDR` |
| migrate | none (a Job). The `5432` EXPOSE and the data `VOLUME` come from the postgres base image | | | |
| ui | the console, `/v1` proxy, `/healthz` | `:8080` | `:8080` (Service port `ui.service.port`, 80) | fixed |

Probes:

- **`/healthz`**: the process is alive. Use it for liveness and startup.
- **`/readyz`**: the process can serve. Each one checks something real:
  - the control-plane services ping Postgres;
  - dag-engine also waits until its gRPC server is up;
  - the scheduler also reports its claim loop's last error;
  - the agent returns 503 until it has registered with the control plane;
  - on the cluster controller, the standby is always ready, and the leader
    turns unready after four intervals without a successful cycle.
- Do **not** put a liveness probe on agent registration. A control-plane
  outage should be waited out, not restarted through.
- The agent's default ops address is loopback, so a kubelet or `docker`
  health check cannot reach it until `NEXUS_METRICS_ADDR=:8089` is set.

## 4. Environment

### 4.1 Runtime identity (every Nexus process)

| Variable | Kubernetes source | Unset |
|---|---|---|
| `NEXUS_POD_NAME` | downward API `metadata.name` | the hostname |
| `NEXUS_POD_NAMESPACE` | `metadata.namespace` | empty |
| `NEXUS_NODE_NAME` | `spec.nodeName` | empty |
| `NEXUS_CLUSTER_NAME` | chart `cluster.name`. For the agents, `claimPlacement.clusterName` overrides it. For the cluster controller, it is `clusterController.clusterName`, and that binary refuses to start without it. | empty |

Each process reports these with its service-instance row, so the console's
Infrastructure page can place every instance on a pod and node (ADR-0035).
The agent also uses `NEXUS_NODE_NAME` for spot-eviction and capacity-type
discovery, and turns `NEXUS_CLUSTER_NAME` into a claim-placement label
(ADR-0031). Under `docker run`, set `--hostname` to a stable name: it then
becomes the instance name.

### 4.2 Control-plane services

| Variable | Required | Meaning |
|---|---|---|
| `DATABASE_URL` (also read as `NEXUS_DATABASE_URL`, which wins, then `NEXUS_POSTGRES_DSN` and `NEXUS_DSN`) | **yes**: exit 1 without it | `postgres://<login>:<password>@<host>:5432/<db>?sslmode=verify-full&sslrootcert=<file>`. The password must be URL-safe. |
| `NEXUS_DB_MAX_CONNS`, `NEXUS_DB_MIN_CONNS` | no | pool size per process: 16/2 by default; api-gateway and workflow-manager use 20; artifact-gc uses 4 |
| `NEXUS_WORKFLOW_MANAGER_ADDR`, `NEXUS_DAG_ENGINE_ADDR`, `NEXUS_SCHEDULER_ADDR`, `NEXUS_RESOURCE_MANAGER_ADDR`, `NEXUS_EVENT_SERVICE_ADDR` | yes, for the peers each one calls (below) | `host:port` of the peer's gRPC listener. `NEXUS_SCHEDULER_ADDR` is a comma-separated list: one entry per scheduler Deployment. Unset peers only log a warning, and the features that need them fail. |
| `NEXUS_QUEUE` | scheduler | the queue it drains (default `default`) |
| `NEXUS_LOG_LEVEL` | no | `debug`, `info`, `warn` or `error`. resource-manager reads `RM_LOG_LEVEL` instead; event-service always logs at info. |
| `NEXUS_TLS_CERT_FILE` / `_KEY_FILE`, `NEXUS_TLS_CLIENT_CERT_FILE` / `_KEY_FILE`, `NEXUS_TLS_CA_FILE`, `NEXUS_TLS_REQUIRE_CLIENT_CERT`, `NEXUS_REQUIRE_TLS`, `NEXUS_TLS_SERVER_NAME`, `NEXUS_TLS_RELOAD_INTERVAL` | no | gRPC TLS and mTLS ([TLS.md](TLS.md)). Unset = plaintext. `NEXUS_REQUIRE_TLS=1` makes plaintext fatal. |
| `NEXUS_RESULT_STORE_*`, `NEXUS_S3_*`, `NEXUS_AZURE_*`, `NEXUS_GCS_*` | api-gateway; workflow-manager for sensors | the result store (§4.4) |
| `NEXUS_NATS_*` | event-service | the event bus. `NEXUS_NATS_URL` empty means no bus (ADR-0003). |
| `OTEL_*` | no | tracing. Off unless an exporter or endpoint is set ([OPERATIONS §26](OPERATIONS.md#26-tracing-and-result-store-backends)). |

Who calls whom, and so which peer addresses each service needs:

| Service | Peer addresses it needs |
|---|---|
| api-gateway | workflow-manager, dag-engine, every scheduler, resource-manager |
| workflow-manager | dag-engine, the schedulers, event-service |
| dag-engine | workflow-manager. Without it, runs stay PENDING. |

workflow-manager reaches event-service over **HTTP**. It rewrites a `:9090`
address, or an address with no port, to `:8080`. The binary default of
event-service is `:8085`, so an event-service run outside the chart or
compose needs `HTTP_ADDR=:8080`.

The api-gateway routes every worker poll to a scheduler from
`NEXUS_SCHEDULER_ADDR`. A scheduler that is missing from that list never
answers a claim.

The chart and compose also set the older spellings next to these, such as
`HTTP_ADDR`, `GRPC_ADDR` and `SCHEDULER_ADDR`, because each binary reads only
its own (§3 names the listener variable each one reads).

### 4.3 Agent

| Variable | Default | Notes |
|---|---|---|
| `NEXUS_CONTROL_PLANE` | `localhost:50051` | the api-gateway's worker gRPC, `host:port` or a gRPC target URI. The chart: `agentDefaults.controlPlane.address`. |
| `NEXUS_API_KEY` | none: **required** in practice | a worker key (`nxs_worker_...`) of the namespace below. A rejected key makes the agent exit. |
| `NEXUS_NAMESPACE`, `NEXUS_QUEUES`, `NEXUS_LABELS` | `default`, `default`, empty | tenant, queues (comma-separated), placement labels |
| `NEXUS_CPU_MILLIS`, `NEXUS_MEMORY_BYTES`, `NEXUS_DISK_BYTES` | the CPU count, **2 GiB (not detected)**, statfs of the work dir | advertised capacity: set them |
| `NEXUS_GPU_COUNT`, `NEXUS_GPU_KIND`, `NEXUS_GPU_MEMORY_BYTES`, `NEXUS_GPU_DISCOVERY=nvidia-smi`, `NEXUS_GPU_MIG`, `NEXUS_GPU_SHARED` | 0 / empty | GPU capacity (ADR-0029) |
| `NEXUS_WAL_DIR` | `/var/lib/nexus-agent/wal` | must be writable; a volume, so an agent restart keeps it |
| `NEXUS_WORK_DIR` | `$TMPDIR/nexus-agent` | task working directories |
| `NEXUS_SECRETS_DIR_ROOT` | `<work dir>.secrets` | resolved secret files: memory-backed in the chart |
| `NEXUS_METRICS_ADDR` | `127.0.0.1:8089` | set `:8089` to probe or scrape it |
| `NEXUS_DRAIN_GRACE` | `5m` | keep it below the runtime's stop timeout (the chart's `terminationGracePeriodSeconds`) |
| `NEXUS_NATS_URL` (+ `NEXUS_NATS_NKEY_SEED_FILE`, `NEXUS_NATS_TLS*`) | empty = long-poll | dispatch hints. The chart: `agentDefaults.nats.url`. |
| `NEXUS_TLS_CA_FILE`, `NEXUS_TLS_CLIENT_CERT_FILE` / `_KEY_FILE`, `NEXUS_TLS_SERVER_NAME`, `NEXUS_REQUIRE_TLS` | plaintext | TLS to the control plane. A CA file alone gives server-verified TLS; add the client pair for mTLS. `NEXUS_REQUIRE_TLS` also refuses a plaintext broker. |
| `NEXUS_RESULT_STORE_ENDPOINT`, `NEXUS_RESULT_STORE_BACKEND`, `NEXUS_S3_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY`, `NEXUS_AZURE_*`, `NEXUS_GCS_*` | from the gateway | §4.4 |
| `NEXUS_K8S_NAMESPACE` (+ `NEXUS_K8S_*`) | unset = no kubernetes executor | Setting it, with the executor's API permissions, enables the kubernetes executor ([OPERATIONS §13](OPERATIONS.md#13-kubernetes-executor)). |
| `NEXUS_EXECUTORS` | unset = discover | the executors the agent builds and advertises, comma-separated: `subprocess`, `docker`, `kubernetes` (`k8s` is read as kubernetes). Below. |
| `NEXUS_DEFAULT_EXECUTOR` | `subprocess` when allowed, else the first of `NEXUS_EXECUTORS` | the executor for a task that names none. Below. |
| `NEXUS_LOG_LEVEL`, `NEXUS_LOG_FORMAT` | `info`, `json` | |

**Which executors an agent offers.** With `NEXUS_EXECUTORS` unset the agent
**discovers** them, as it always has:

- **subprocess** is always available.
- **docker** is available when `docker info` succeeds, which needs a Docker
  socket mounted into the agent. That socket is root-equivalent on the host.
- **kubernetes** is available with `NEXUS_K8S_NAMESPACE` set and its startup
  probe passing ([OPERATIONS §13](OPERATIONS.md#13-kubernetes-executor)).

An unusable docker or kubernetes is then a warning, and the agent serves the
rest. With `NEXUS_EXECUTORS` set, the list is **exact**: the agent builds and
advertises only those executors, and refuses to start when one of them cannot
be built, with the reason (an unknown name, kubernetes without
`NEXUS_K8S_NAMESPACE` or with a failing RBAC probe, docker without a reachable
daemon). A configured kubernetes executor the list leaves out is not built.
`NEXUS_EXECUTORS=kubernetes` is a worker on which every task is its own pod and
nothing runs inside the agent's container.

**A task that names no executor** (`runtime.executor`) runs on the claiming
agent's **default executor**: `NEXUS_DEFAULT_EXECUTOR`, which must be one of
`NEXUS_EXECUTORS`, else `subprocess` when allowed, else the first listed. The
agent advertises it as the worker label `nexus.io/default-executor` (it owns
that label; a `NEXUS_LABELS` value that disagrees is refused), and the
scheduler gives such a task only to a worker whose default can run it: a docker
or kubernetes default needs the task's `runtime.image`. A task without an image
on a fleet whose defaults all need one waits with
`waiting_for_worker: the task names no runtime.executor and no runtime.image ...`.
A task that names an executor is placed only on a worker that advertises it.
Agents older than these settings carry no label and default to subprocess. The
console shows a worker's executors and its `nexus.io/default-executor` label;
`nexus_agent_executor_advertised` and `nexus_agent_executor_default` export the
same per executor.

The chart renders both variables from `agents[].executors` (default
`subprocess`) and `agents[].defaultExecutor` (default: the first executor);
`kubernetes` there also renders the `NEXUS_K8S_*` variables, the executor's
Role and the task-API policy. It refuses `docker`, because a chart pod has no
Docker daemon to reach and the agent would not start. Compose leaves
`NEXUS_EXECUTORS` unset, so its agents discover docker through the mounted
socket. The heartbeat interval is not an agent setting: the gateway dictates
it in the Register response (ADR-0005).

### 4.4 Result store (api-gateway, agents, artifact-gc, workflow-manager sensors)

`NEXUS_RESULT_STORE_BACKEND` (`s3`, `azblob` or `gcs`) and
`NEXUS_RESULT_STORE_BUCKET` / `_REGION` / `_ENDPOINT` select the store. Then:

| Backend | Credential variables |
|---|---|
| S3 | `NEXUS_S3_CREDENTIALS` (`auto`, `static`, `web-identity`, `pod-identity`, `iam`, `env` or `anonymous`), `NEXUS_S3_ACCESS_KEY_ID` / `_SECRET_ACCESS_KEY`, `NEXUS_S3_USE_TLS` |
| Azure Blob | `NEXUS_AZURE_STORAGE_ACCOUNT`, `NEXUS_AZURE_STORAGE_AUTH` (`workload-identity`, `managed-identity`, `shared-key` or `auto`), `NEXUS_AZURE_CLIENT_ID`, `NEXUS_AZURE_STORAGE_KEY` |
| GCS | `NEXUS_GCS_AUTH` |

Each agent takes the bucket and region from the gateway at registration, and
the endpoint unless it sets `NEXUS_RESULT_STORE_ENDPOINT` itself. Set it for
an agent outside the cluster when the gateway advertises an in-cluster name
such as `http://minio:9000`.

Two defaults matter outside the chart:

- The api-gateway's S3 endpoint defaults to `http://localhost:9000`.
- artifact-gc falls back to MinIO's development key pair when no key is set.
  Under S3 `auto` it therefore signs with static keys, never a workload
  identity. Set `NEXUS_S3_CREDENTIALS=web-identity` (or `pod-identity`, or
  `iam`) explicitly for keyless S3.

### 4.5 migrate, cluster-controller, ui

- **migrate.**
  - `DATABASE_URL` (**required**), as the owner login.
  - `MIGRATIONS_DIR` (the image sets `/migrations`).
  - `NEXUS_MIGRATE_WAIT_SECONDS` (default 60).
  - `NEXUS_MIGRATE_CREATE_DATABASE=true` creates a missing database.
  - `NEXUS_DB_ROLE_PREFIX`.
  - The same image runs `nexus-db-roles` (`--entrypoint nexus-db-roles`).
    It takes `NEXUS_DB_ROLES` (a JSON spec), `NEXUS_DB_ROLE_PREFIX`,
    `NEXUS_DB_REVOKE_PUBLIC_CONNECT`, and one password variable per login.
- **cluster-controller.**
  - `NEXUS_CLUSTER_NAME` (**required**).
  - `NEXUS_RM_ADDR`, default `resource-manager:50052`; the chart sets
    `resource-manager:9090`.
  - `POD_NAME` / `POD_NAMESPACE`: the Lease identity and namespace.
  - `NEXUS_CONTROLLER_ALLOW_APPLY`, `NEXUS_CONTROLLER_INTERVAL`,
    `NEXUS_JOB_LABEL_SELECTOR`, `CONTROLLER_LOG_LEVEL`.
  - In-cluster API credentials, or `KUBECONFIG`.
- **ui.**
  - `NEXUS_GATEWAY_UPSTREAM`, default `api-gateway:8080`.
  - `NEXUS_DNS_RESOLVER`, default `127.0.0.11`, Docker's embedded DNS. nginx
    needs an IP and applies no search domains. On Kubernetes, use the cluster
    DNS IP and the gateway's FQDN, as the chart does.

## 5. Writable paths and volumes

| Image | Writes to | Provide |
|---|---|---|
| Control-plane services, artifact-gc, cluster-controller | nothing | `--read-only` as is. The chart still mounts an emptyDir on `/tmp`. |
| agent | WAL and its log spool (`NEXUS_WAL_DIR`), work dir, secrets dir, `/tmp/nexus-task` (task-API sockets), the artifact cache (`NEXUS_ARTIFACT_CACHE_DIR`). Subprocess tasks also write wherever they write, inside the container. | A volume on the WAL dir; `/tmp`, or volumes for the work and secrets dirs. The image creates `/var/lib/nexus` and `/var/lib/nexus-agent/wal` owned by 10001, so a named volume mounted there is writable. |
| migrate | nothing | `--read-only` as is |
| ui | `/etc/nginx/conf.d` (the config rendered from the template at start), `/tmp` (pid, temp files) | two tmpfs or emptyDir mounts |

The chart runs every container with `readOnlyRootFilesystem: true` except the
agents and the Jobs. Agents keep a writable root because subprocess tasks run
inside them. The Jobs use psql, which needs nothing extra. Read-only roots
there are possible but not set. deploy/k8s/agent-deployment.yaml, an agent
that runs only the kubernetes executor, runs read-only.

## 6. Secrets each image needs

| Image | Secret material |
|---|---|
| Control-plane services | the database password (inside `DATABASE_URL`); a CA bundle for `verify-full`; TLS leaves under mTLS; api-gateway and event-service also `NEXUS_SECRET_KEY` (sealed channel and trigger secrets, optional), api-gateway `NEXUS_AUDIT_SINK_KEY` (optional); event-service its NATS seed |
| api-gateway, artifact-gc, agents (result store) | S3 keys, an Azure account key, or none with a workload identity |
| agent | a worker API key; its NATS seed (`NEXUS_NATS_NKEY_SEED_FILE`) under NKey auth; a CA bundle (and a client leaf under mTLS) for the control plane |
| migrate | the owner's database password; for `nexus-db-roles`, each runtime login's password |
| cluster-controller | none of Nexus's; its ServiceAccount token, and a client leaf under mTLS |
| ui | none |

Pass secrets as files or Kubernetes `secretKeyRef`s, not as literal `-e`
flags in shell history. The chart takes them only from Secrets you create
(`existingSecret` and the keys in values.yaml).

## 7. Running with plain `docker run`

### 7.1 A scheduler for an extra queue, against an existing database

```sh
docker run -d --name nexus-scheduler-batch --hostname nexus-scheduler-batch-1 \
  --restart unless-stopped --read-only --cap-drop ALL --security-opt no-new-privileges \
  -v "$PWD/azure-postgres-roots.pem:/etc/nexus/postgres-ca/ca.crt:ro" \
  --env-file scheduler.env \
  -e NEXUS_QUEUE=batch \
  -e NEXUS_HTTP_ADDR=:8080 -e NEXUS_GRPC_ADDR=:9090 \
  -e NEXUS_LOG_LEVEL=info -e NEXUS_CLUSTER_NAME=onprem-dc1 \
  -p 8080:8080 -p 9090:9090 \
  ghcr.io/<owner>/kgf-nexus/scheduler:<tag>

curl -fsS http://localhost:8080/readyz
```

`scheduler.env` holds
`DATABASE_URL=postgres://nexus_scheduler_login:<password>@<server>.postgres.database.azure.com:5432/nexus?sslmode=verify-full&sslrootcert=/etc/nexus/postgres-ca/ca.crt`.
Then add `<host>:9090` to `NEXUS_SCHEDULER_ADDR` on the api-gateway and the
workflow-manager. Under mTLS, also give the scheduler
`NEXUS_TLS_CERT_FILE` / `_KEY_FILE` / `NEXUS_TLS_CA_FILE` and
`NEXUS_TLS_REQUIRE_CLIENT_CERT=1`.

### 7.2 A worker agent connecting to a control plane

```sh
docker volume create nexus-agent-wal
docker run -d --name nexus-agent-cpu --hostname nexus-agent-cpu-1 \
  --restart unless-stopped --stop-timeout 360 \
  --read-only --tmpfs /tmp:rw,size=8g --cap-drop ALL --security-opt no-new-privileges \
  -v nexus-agent-wal:/var/lib/nexus-agent/wal \
  -v "$PWD/nexus-ca.crt:/etc/nexus/tls/ca.crt:ro" \
  --env-file agent.env \
  -e NEXUS_CONTROL_PLANE=workers.nexus.example.com:9091 \
  -e NEXUS_TLS_CA_FILE=/etc/nexus/tls/ca.crt \
  -e NEXUS_NAMESPACE=default -e NEXUS_QUEUES=default \
  -e NEXUS_LABELS=zone=dc1,accelerator=none \
  -e NEXUS_CPU_MILLIS=8000 -e NEXUS_MEMORY_BYTES=17179869184 \
  -e NEXUS_METRICS_ADDR=:8089 -p 127.0.0.1:8089:8089 \
  -e NEXUS_CLUSTER_NAME=onprem-dc1 \
  ghcr.io/<owner>/kgf-nexus/agent:<tag>

curl -fsS http://localhost:8089/readyz    # 200 once registered
```

- `agent.env` holds `NEXUS_API_KEY=nxs_worker_...` and the result-store
  credentials, e.g. `NEXUS_S3_CREDENTIALS=static`,
  `NEXUS_S3_ACCESS_KEY_ID=...`, `NEXUS_S3_SECRET_ACCESS_KEY=...`, plus
  `NEXUS_RESULT_STORE_ENDPOINT=https://...` when the gateway advertises an
  in-cluster name.
- `--stop-timeout` must exceed `NEXUS_DRAIN_GRACE` (5m): `docker stop` sends
  SIGTERM, and the agent drains before it exits.
- Behind a load balancer whose certificate does not name the dialed host, add
  `NEXUS_TLS_SERVER_NAME`.
- With NATS, add `NEXUS_NATS_URL=nats://...` and mount the seed file at
  `NEXUS_NATS_NKEY_SEED_FILE`.

### 7.3 The same on Kubernetes

| `docker run` | Kubernetes (the chart renders all of it) |
|---|---|
| `--read-only`, `--cap-drop ALL`, `--security-opt no-new-privileges`, image `USER` | `securityContext`: `readOnlyRootFilesystem`, `capabilities.drop: [ALL]`, `allowPrivilegeEscalation: false`, `runAsNonRoot` + `runAsUser`; pod `seccompProfile: RuntimeDefault` |
| `--hostname` | the pod name, plus `NEXUS_POD_NAME` / `_NAMESPACE` / `NEXUS_NODE_NAME` from the downward API (§4.1) |
| `--env-file` with credentials | env `valueFrom.secretKeyRef` (`existingSecret`) |
| `-v ca.crt:...:ro` | a Secret or ConfigMap volume (`postgres.caBundle`, `agentDefaults.controlPlane.tls.caSecretName`, `tls.*`) |
| `-v nexus-agent-wal:...`, `--tmpfs /tmp` | emptyDir volumes (the WAL survives container restarts, not pod deletion) |
| `-p` | `containerPort` + Service; agents have none |
| `curl /readyz` | `readinessProbe` (and `startupProbe` / `livenessProbe` on `/healthz` for the services) |
| `--stop-timeout` | `terminationGracePeriodSeconds` (`agentDefaults.terminationGracePeriodSeconds`, 90, over `drainGrace`, 60s) |

The chart equivalent of §7.2 is a worker-pool release
(`components.controlPlane.enabled=false`, `agentDefaults.controlPlane.address`,
`agentDefaults.controlPlane.tls`). See
`deploy/helm/kgf-nexus/examples/aks-worker-pool.yaml` and
[OPERATIONS §20.16](OPERATIONS.md#2016-existing-clusters-and-worker-pool-releases).
