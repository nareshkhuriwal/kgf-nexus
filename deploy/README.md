# Nexus MVP deployment (`docker compose up`)

One-command bring-up of the Phase 1 MVP: Postgres, MinIO, a migration job, the
six control-plane services (ADR-0011), two worker agents with **different
capacity**, and the operator console.

Everything here is **local-development only**. The credentials are deliberately
obvious and are marked `DEV ONLY` in `docker-compose.yml`; there is no secret
management in Phase 1.

Other ways in:

- `nexusctl up` wraps this compose stack: it waits for health, registers a
  tenant through the API and prints the console URL and credentials once
  (cli/README.md, "Local quick start").
- Kubernetes directly: the Helm chart in `helm/kgf-nexus`, with a values
  reference and production checklist in docs/OPERATIONS.md §20.
  `helm/validate.sh` lints and schema-checks it.
- Kubernetes through Terraform: [terraform/README.md](terraform/README.md). It
  covers the Helm chart via Terraform on a local cluster, and cloud platforms
  (AKS, EKS, GKE) with managed PostgreSQL, registries and object storage. The
  cloud platforms have been validated offline only (F115).

---

## 1. Bring-up

**First run on a fresh checkout** (needs Docker with Compose v2, and Go):

```bash
git clone <repo> && cd kgf-nexus
make quickstart        # = go run ./cli/cmd/nexusctl up
```

`make quickstart` brings the stack up, waits for health, registers a tenant
through the API, mints the admin key and the agents' worker token through the
API, starts the agents, and prints the console URL and credentials once
(cli/README.md, "Local quick start"). Pass flags with `QUICKSTART_ARGS`, e.g.
`make quickstart QUICKSTART_ARGS="-p my-stack --api-port 18090"`.

**Driving Compose yourself.** The agents' credential, `NEXUS_WORKER_API_KEY`,
has no default: Compose refuses to parse the file while it is unset, and
`make up` stops before Compose with the two options above. Generate a key,
start the stack, then install the key (§2):

```bash
export NEXUS_WORKER_API_KEY=$(make -s worker-key)
make up                # docker compose -f deploy/docker-compose.yml up --build -d
make seed              # installs that worker key and prints a new admin key
```

The compose file lives in deploy/ but the build context is the repo root,
because the Go module and the shared internal/ + gen/ packages live there.
`make down`, `make logs` and `make ps` work with or without the key set.

Then:

| What | Where |
|---|---|
| Operator console | <http://localhost:3000> |
| REST API | <http://localhost:8090/v1/namespaces/default/runs> |
| MinIO console | <http://localhost:9011> (`nexusadmin` / `nexus-dev-password`) |
| Postgres | `postgres://nexus:nexus-dev-password@localhost:55433/nexus` |
| Worker gRPC | `localhost:9091` |

Postgres is published on **55433**, not 5432, so it cannot collide with a local
Postgres or with the 55432 dev database in `CONTRACT.md`.

The services have **no built-in DSN** (docs/OPERATIONS.md §8): compose passes
`DATABASE_URL` to every container, and a service started any other way exits at
startup until you give it one. To run a service from source against this stack:

```bash
export DATABASE_URL='postgres://nexus:nexus-dev-password@localhost:55433/nexus?sslmode=disable'
go run ./services/scheduler
```

### Startup order (and why)

```
postgres ──health──┐
                   ├──► migrate (runs, exits 0) ──► six services ──► agents ──► ui
minio ────health───┴──► minio-init (creates bucket, exits 0)
```

Every service declares:

```yaml
depends_on:
  postgres: { condition: service_healthy }
  migrate:  { condition: service_completed_successfully }
```

The `service_completed_successfully` condition is the important one. Per
**ADR-0011 rule 4**, migrations are applied by a **dedicated job, never by a
service at startup** — six service replicas racing to run `CREATE TABLE` is how
you get a half-migrated schema and six crash loops. Services wait for the
migration job to *exit zero*, so nothing starts against an unmigrated schema.

The migration job is idempotent: it records applied versions in a
`schema_migrations` ledger table, so `docker compose up` twice in a row is a
no-op rather than a failure (the frozen `0001_init.up.sql` contains bare
`CREATE TABLE`s and cannot be re-run on its own).

### Verify

```bash
docker compose -f deploy/docker-compose.yml ps          # all services healthy?
docker compose -f deploy/docker-compose.yml logs migrate # "migrate: done"
curl -s localhost:8090/healthz
```

---

## 2. Seed an API key

`make quickstart` does this for you, through the API. This section is for a
stack started with `make up` and your own `NEXUS_WORKER_API_KEY`.

MVP authn is static API keys, hashed at rest with SHA-256
(`internal/store.HashAPIKey`). Two keys are needed: an **admin** key for humans,
the CLI and the UI, and a **worker** key that the agents already carry via
`NEXUS_WORKER_API_KEY`.

```bash
make seed    # host psql if installed, else the stack's own postgres container
```

It creates the `default` tenant row if missing, installs the SHA-256 of the
exported `NEXUS_WORKER_API_KEY` (role `worker`) and of a newly generated admin
key (or `NEXUS_DEV_API_KEY`, if set), and prints both plaintexts once:

```
  admin  key (console, CLI):  nxs_admin_<64 hex>
  worker key (agents):        nxs_worker_<64 hex>
```

Use the admin key:

```bash
export NEXUS_API_KEY=nxs_admin_...
curl -s -H "Authorization: Bearer $NEXUS_API_KEY" \
  localhost:8090/v1/namespaces/default/runs | jq
```

Once you hold an admin key, further credentials, including rotated worker
tokens (role `worker`, scope `worker_rpc`), are better minted through
`POST /v1/namespaces/default/tokens` or the console: those are audited, expire
and can be revoked.

The **worker** role deliberately cannot read the REST surface. A worker
credential lives on every execution host — the least trustworthy place in the
system — so a compromised host must not be able to enumerate other namespaces'
runs.

If you change the worker key, export the new value and run both `make up`
(to restart the agents with it) and `make seed` (to install it), or the agents
will fail to register.

---

## 3. Submit the example DAG

The examples live in `examples/` (owned by the CLI/SDK workstream) as compiled
`WorkflowSpec` documents — the SDK is a compiler that emits the spec in the
user's process, and the control plane only ever parses data (ADR-0004).

```bash
go build -o ./nexusctl ./cli/cmd/nexusctl

export NEXUS_API_KEY=<the admin key>
export NEXUS_ADDR=http://localhost:8090

# Start here: 3 tasks in a line.
./nexusctl workflow submit -f examples/hello_dag/compiled.yaml --namespace default
./nexusctl run create --spec hello-dag --namespace default

# The Phase 1 exit-criteria DAG: 10 nodes, fan-out/fan-in, mixed CPU/GPU.
./nexusctl workflow submit -f examples/ten_node_mixed/compiled.yaml --namespace default
./nexusctl run create --spec ten-node-mixed --namespace default

./nexusctl run get <run-id>
./nexusctl worker ls
```

or, without the CLI:

```bash
curl -s -X POST localhost:8090/v1/namespaces/default/specs \
  -H "Authorization: Bearer $NEXUS_API_KEY" \
  -H 'Content-Type: application/yaml' \
  --data-binary @examples/ten_node_mixed/compiled.yaml

curl -s -X POST localhost:8090/v1/namespaces/default/runs \
  -H "Authorization: Bearer $NEXUS_API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: my-first-run' \
  -d '{"spec_name":"ten-node-mixed","params":{}}'
```

Then open <http://localhost:3000> and watch the DAG fill in. Spec names come from
each example's `compiled.yaml`; check `examples/README.md` for the exact CLI
invocations, which that workstream owns.

### Exercising the resource-aware scheduler

The two agents advertise **deliberately different** capacity, because the Phase 1
exit criterion is a *mixed CPU/GPU* DAG and two identical workers would let a
broken resource matcher pass:

| Agent | Queues | CPU | Memory | GPU |
|---|---|---|---|---|
| `agent-cpu` | `default` | 8 cores | 16 GiB | none |
| `agent-gpu` | `default`, `gpu` | 4 cores | 32 GiB | 2 × `nvidia-a100` |

Advertised capacity is a **ledger entry, not a hardware probe**, so GPU
scheduling is exercisable on a machine with no GPU. A task requesting
`gpu_count: 1` can only be placed on `agent-gpu`; request `gpu_count: 3` and it
stays `PENDING` with
`pending_reason = "waiting_for_resources: gpu (need 3, free 2)"`, which the
console surfaces on the attempt — that is the difference between an unexplained
stall and an explained one.

Scale a dimension:

```bash
NEXUS_GPU_KIND=nvidia-h100 docker compose -f deploy/docker-compose.yml up -d agent-gpu
docker compose -f deploy/docker-compose.yml up -d --scale scheduler=3   # no leader election needed (§5.3)
```

---

## 4. Configuration

Everything is env-var driven with working defaults. Put overrides in a `.env`
file next to `docker-compose.yml`.

| Variable | Default | Purpose |
|---|---|---|
| `NEXUS_UI_PORT` | `3000` | Console port |
| `NEXUS_API_PORT` | `8080` | Gateway REST port |
| `NEXUS_GRPC_PORT` | `9091` | Gateway worker-gRPC port |
| `NEXUS_POSTGRES_PORT` | `55433` | Published Postgres port |
| `NEXUS_MINIO_PORT` / `_CONSOLE_PORT` | `9000` / `9001` | MinIO |
| `NEXUS_NAMESPACE` | `default` | Single namespace for the MVP |
| `NEXUS_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `NEXUS_RESULT_BUCKET` | `nexus-results` | MinIO bucket (auto-created) |
| `NEXUS_WORKER_API_KEY` | none: required (`make -s worker-key`) | Agent credential; `make quickstart` supplies it |
| `NEXUS_DEV_API_KEY` | generated | Admin credential `make seed` installs |
| `NEXUS_GPU_KIND` | `nvidia-a100` | Advertised GPU model on `agent-gpu` |
| `MINIO_ROOT_USER` / `_PASSWORD` | `nexusadmin` / `nexus-dev-password` | **DEV ONLY** |

### Images

| File | Builds |
|---|---|
| `Dockerfile.service` | All six Go services **and** the agent, parameterised by `SERVICE` / `SERVICE_PATH` |
| `Dockerfile.migrate` | The migration job (`postgres:16-alpine` + `migrate.sh`) |
| `Dockerfile.ui` | The console (`node:24` LTS build → `nginx:1.27` runtime) |

`Dockerfile.service` is one parameterised file rather than seven near-identical
ones, because the build differs only in the main-package path and seven copies
drift the moment someone bumps a Go version in six of them. It is multi-stage:
the `golang` builder never ships, and the final `alpine` image carries one static
`CGO_ENABLED=0` binary running as uid 10001.

`SERVICE_PATH` is passed explicitly for every service. The six services do not
all put `main` in the same place (some use `cmd/<name>/`), and an implicit
convention that half the repo violates is worse than none. If a main package
moves, change that one line:

```yaml
args:
  SERVICE: api-gateway
  SERVICE_PATH: services/api-gateway/cmd/api-gateway
```

`nginx.conf` proxies `/v1` to `api-gateway:8080` so the browser sees one origin
and the gateway needs no CORS configuration. It disables `proxy_buffering` on
that location — without it SSE buffers indefinitely and the console silently
stops updating, which looks exactly like a stalled run.

---

## 5. Troubleshooting

**`dependency failed to start: container nexus-migrate-1 exited (1)`**
The migration job could not apply the schema; nothing else will start, by design.
`docker compose logs migrate` shows the failing statement. Usual causes: a
hand-modified `0001_init.up.sql` (it is frozen), or a volume from an older schema
— `docker compose down -v` and retry.

**Services restart-loop with `no Go files in /src/services/<name>`**
That service's `main` package does not exist yet. Phase 1 is built concurrently;
build only what exists:
```bash
docker compose -f deploy/docker-compose.yml up postgres minio minio-init migrate ui
```
Fix the path with `SERVICE_PATH` once the service lands.

**Console shows "Cannot reach the Nexus API"**
The gateway is not answering. `docker compose ps api-gateway`, then
`docker compose logs -f api-gateway`. This is the *intended* behaviour for a dead
backend — a clear message, never a blank screen.

**Console loads but every panel says "Not found (HTTP 404)"**
The gateway is up but that endpoint is not implemented yet. Each panel degrades
independently, so the rest of the console keeps working.

**No workers in the fleet view**
`docker compose logs -f agent-cpu agent-gpu`. Almost always the API key: the
agents present `NEXUS_WORKER_API_KEY`, and it must be a live credential with
role `worker` in the agents' namespace. With `make up`, run `make seed` with the
same key exported. With `make quickstart`, `nexusctl up` warns when its worker
token has expired.

**Tasks stay `PENDING` forever**
Check the attempt's `pending_reason` in the console — it is populated precisely
so this is not a mystery. `waiting_for_resources: gpu (...)` means no worker
advertises enough of that dimension; raise `AGENT_CAPACITY_GPU_COUNT` on
`agent-gpu` or lower the task's request. If there is no reason at all, the
scheduler is not running: `docker compose logs -f scheduler`.

**Attempts go `LOST` under normal operation**
Leases are expiring, meaning heartbeats are not arriving in time. On a loaded
laptop raise `LEASE_TTL_MS` on the scheduler. Note that `LOST` is *not* `FAILED`:
it means we do not know whether the work ran (invariant 2), and whether it is
auto-retried depends on the task's `idempotency` mode.

**Port already in use**
Override the port: `NEXUS_UI_PORT=3001 docker compose -f deploy/docker-compose.yml up`.

**`mc ready local` healthcheck never passes**
Older MinIO images lack `mc ready`. Pin a newer release or substitute
`curl -f http://localhost:9010/minio/health/live`.

**Docker socket permission denied on an agent**
The agents bind-mount `/var/run/docker.sock` for the docker executor, which is
effectively root on the host — acceptable on a laptop, never in production
(ADR-0007's k8s executor is the answer there). If you only need the subprocess
executor, drop the mount and set `AGENT_EXECUTORS: subprocess`.

**Reset everything**
```bash
make down                     # also drops run history
make up && make seed          # with NEXUS_WORKER_API_KEY exported
# or, for a quickstart stack:  nexusctl down --volumes && nexusctl up
```
