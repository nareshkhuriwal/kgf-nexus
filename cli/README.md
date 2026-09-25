# nexusctl

Command line interface for Nexus. Go, standard library only plus `gopkg.in/yaml.v3`.

## Build

```bash
go build -o ./nexusctl ./cli/cmd/nexusctl
```

## Configure

Resolution order, highest wins:

1. flags — `--endpoint`, `--namespace`, `--api-key`, `--context`
2. environment — `NEXUS_ENDPOINT`, `NEXUS_API_KEY`, `NEXUS_NAMESPACE`, `NEXUS_CONTEXT`, `NEXUS_CONFIG`
3. `~/.nexus/config.yaml` (or `--config`)
4. built-in defaults (`http://localhost:8090`, the host port `make up` publishes; namespace `default`)

Flags beat the environment beats the file — otherwise a stale config file would
silently override an explicit `--endpoint`, and you would spend an afternoon
wondering why you were talking to the wrong cluster.

```yaml
# ~/.nexus/config.yaml
current-context: staging
namespace: default
contexts:
  staging:
    endpoint: http://nexus.staging.internal:8080
    api_key: nx_...
  prod:
    endpoint: https://nexus.prod.internal
    namespace: prod
```

```bash
nexusctl config view              # shows where each value came from
```

**API key material is never printed** — not by `config view`, not in `--output
json`, not in an error, not in a log. `config view` reports only whether a key is
set and its length, which is enough to diagnose an auth failure without putting a
key on a screen that may be shared or recorded.

## Commands

```
up [flags]                           Start a local stack with Docker and seed a
                                     tenant (see "Local quick start")
status [-o json]                     Per-service state and health of that stack
down [--volumes]                     Stop it; --volumes also DELETES its data

workflow submit <file> [--dry-run]   Submit a spec (JSON, YAML, or .py)
workflow compile <file> [--show]     Print exactly the spec the server will see
workflow versions <name>             List registered spec versions

run create <name> [--param K=V]      Start a run
           [--idempotency-key KEY] [--wait]
run get <run-id>                     Detail with per-node states
run list [--state S] [--spec N]      List runs
run cancel <run-id>                  Cancel
run retry <run-id> [--node ID ...]   Retry a failed run, whole or from the
                                     named nodes and their dependents

worker ls                            Fleet with capacity and heartbeat age
capacity                             Advertised vs committed
logs <attempt-id> [--follow]         Stream an attempt's logs
events <run-id> [--follow]           Event history, or live tail over SSE
config view                          Resolved configuration
version
```

Global: `-o, --output table|json|yaml`. Table is the default and is formatted for
humans — its columns may change, so **scripts should use `--output json`**.

## Local quick start

Requires Docker with the Compose v2 plugin, and a checkout of this repository
(`up` runs `deploy/docker-compose.yml`, found by searching upward from the
current directory, or `--compose-file`).

```bash
go build -o ./nexusctl ./cli/cmd/nexusctl
./nexusctl up
```

`up` checks Docker, brings the stack up detached (building images that do not
exist yet), waits until every service is healthy and the gateway answers
`/readyz`, and on the **first** run seeds a tenant through the product's own API:

1. `POST /v1/auth/register` creates a tenant and its administrator — the same
   self-registration a new customer uses in the console.
2. Sign-in requires a verified address, and the compose stack delivers all mail
   to its bundled Mailpit, so `up` reads the verification link from Mailpit's API
   and posts it to `/v1/auth/email/verify`.
3. `POST /v1/auth/login`, then `POST /v1/namespaces/{ns}/tokens` mints an admin
   API key (role `admin`, scopes `read`/`write`/`admin_write`, 90-day expiry);
   the session is logged out.
4. The agents need a worker credential, which the API cannot currently issue in
   a form they accept (the worker gRPC interceptor authenticates only legacy
   `api_keys`, not scoped tokens). So, exactly as `make seed` does, `up`
   generates a worker key locally and inserts only its SHA-256 hash into
   `api_keys` (role `worker`, the new tenant's namespace) through
   `docker compose exec` into the stack's Postgres — no host `psql` needed. It
   then points the agents and the NATS identity job at the tenant's namespace
   with a generated override file, reloads the broker, starts the agents, and
   waits until they register.

It then prints the console URL, the tenant ID, the sign-in email and password,
and the admin API key **once**. The password is stored nowhere. The admin key is
also written to the nexusctl config (`--config` / `NEXUS_CONFIG` /
`~/.nexus/config.yaml`, mode 0600) as a context named after the compose project
(`--context-name`, or `--no-context` to skip); it becomes the current context
only if the file had none, so an existing setup is never switched silently.

Running `up` again on a running, seeded stack changes nothing and prints no
secret — it says the stack is already up and how to reset. After `down`
(without `--volumes`), `up` starts it again with the same tenant. To start over:

```bash
nexusctl down --volumes && nexusctl up
```

`down --volumes` deletes the database, stored results and NATS identities, the
local stack record, and the context `up` wrote. It prints a warning; there is
no undo.

### Local state

`up` keeps one record per compose project under `~/.nexus/quickstart/<project>/`
(override with `--state-dir` or `NEXUS_STATE_DIR`; directory 0700, files 0600):
the compose files and ports used, the tenant ID and namespace, the IDs of the
created credentials, and the generated agent override. It also holds the
**worker** key, because the compose file requires `NEXUS_WORKER_API_KEY` on every
`docker compose up`; that key has role `worker`, is refused by every REST route,
and is never printed. The record is discarded automatically if the database
volume it describes has been deleted or recreated behind nexusctl's back.

### Options

```
--compose-file PATH      Base compose file
-f, --file PATH          Extra override file (repeatable), e.g. to change images
-p, --project NAME       Compose project name (default "nexus"); several stacks
                         can run side by side under different names and ports
--api-port, --ui-port, --grpc-port, --postgres-port, --minio-port,
--minio-console-port, --mailpit-port, --nats-port N
                         Host ports (NEXUS_API_PORT etc.); remembered for later runs
--build | --no-build     Rebuild every image | never build (default: build missing)
--timeout 5m             Health wait, after containers start (builds not counted)
--tenant-name, --admin-email, --context-name, --no-context
--down [--volumes]       Same as `nexusctl down`
```

The compose file also publishes some fixed loopback ports (service metrics on
127.0.0.1:191xx) that no variable moves. To run beside another stack that holds
them, pass an override with `ports: !reset []` for those services (Compose
2.24+). Errors name the cause: Docker missing or its daemon down, Compose v2
missing, a host port in use (with the flag that moves it), an image that cannot
be pulled, a failing service (with the tail of its log), a health timeout (with
the services still waiting), or the seeding step that failed.

## `workflow compile --show`

The command worth knowing:

```bash
nexusctl workflow compile dag.py --show
```

It prints the canonical document and its content address, and contacts nothing.
ADR-0004 names this specifically as the mitigation for *"my Python compiled to a
graph I did not expect"*. When the graph surprises you, this shows you what the
control plane will actually parse.

For a `.py` file, nexusctl shells out to **your** interpreter (`--python`, default
`python3`). It does not embed a Python runtime and does not parse Python: the
platform's guarantee is that only your own process executes your code, and the CLI
keeps that true.

## Error messages

An orchestrator CLI that prints a bare `500` is useless. Every non-2xx carries the
server's own message, the request id for correlating with control-plane logs, and —
where the cause is knowable — a concrete next step:

```
$ nexusctl run get run-xyz
nexusctl: GET /v1/namespaces/prod/runs/run-xyz failed: HTTP 500: scheduler panic
  request id: req-abc123
  This is a control-plane fault. Quote the request id above when reporting it.
```

A 401 names `NEXUS_API_KEY`; a 422 suggests `--dry-run`; a 409 explains the
idempotency key; an unreachable endpoint names the endpoint it tried. Locally
detectable spec problems fail before any request is made.

## Tests

```bash
go test ./cli/...
```

Covers arg parsing (including flags before and after the operand), output
formatting, config precedence, canonicalization parity with the Python SDK, error
message content, and that key material never reaches any output stream.

The `up`/`down`/`status` tests drive the real orchestration against a scripted,
stateful fake of the docker CLI and an `httptest` fake of the gateway and Mailpit
(`cli/internal/quickstart`): first-run seeding, idempotent re-runs, resume after
`down`, `down --volumes`, every error class, and that each secret is printed
exactly once. They never touch a Docker daemon or `~/.nexus`.
