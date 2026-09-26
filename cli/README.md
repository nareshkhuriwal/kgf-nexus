# nexusctl

Command line interface for Nexus. Go, standard library only plus `gopkg.in/yaml.v3`.
(The templates test also imports the gateway's spec package to validate what
`init` generates; that is test-only and not linked into the binary.)

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

init <template> [dir] [--name N] [--gpu] [--force]   New project from a template
init --list                                          List templates

workflow submit <file> [--dry-run] [--watch]   Submit a spec (JSON, YAML, or .py)
workflow compile <file> [--show]     Print exactly the spec the server will see
workflow versions <name>             List registered spec versions
workflow delete <name> [--yes]       Delete a workflow and all its versions

run create <name> [--param K=V]      Start a run
           [--idempotency-key KEY] [--wait]
run get <run-id>                     Detail with per-node states
run list [--state S] [--spec N]      List runs
run cancel <run-id>                  Cancel
run retry <run-id> [--node ID ...]   Retry a failed run, whole or from the
                                     named nodes and their dependents
run pause <run-id>                   Stop new dispatch; running attempts finish
run resume <run-id>                  Resume a paused run
run --local <file> [--param K=V]     Run a workflow on this machine, no cluster
replay <attempt-id> [--local]        Fetch a failed attempt's inputs; re-run it here

schedule list [--limit N]            Schedules with cron, zone, next fire
schedule get <name> [--count N]      Detail with the next N fire times
schedule create <name> --workflow W --cron EXPR [--timezone TZ] [--param K=V]
           [--spec-version N] [--overlap skip|allow|queue_one] [--catchup N]
           [--priority N] [--disabled]
schedule update <name> [same flags] [--unset-param K]   Change only what is given
schedule delete <name> [--yes]       Delete (and its tick history)
schedule enable|disable <name>
schedule preview <cron> [--timezone TZ] [--count N]     Nothing is created

backfill start <schedule> --from T --to T [--max-active N] [--max-runs N]
backfill list <schedule>             A schedule's backfills
backfill get <id>                    Progress: cursor, runs created
backfill pause|resume <id>
backfill cancel <id> [--yes]

worker ls                            Fleet with capacity and heartbeat age
worker drain <id> [--yes]            Finish current work, take nothing new, exit
worker quarantine <id> --reason R [--for 6h] [--yes]   Out of placement
worker release <id> [--reason R]     Back into placement
capacity                             Advertised vs committed
logs <attempt-id> [--follow]         Stream an attempt's logs
events <run-id> [--follow]           Event history, or live tail over SSE
cache invalidate --task-ref R [--task-version N] | --cache-key K  [--yes]
config view                          Resolved configuration
completion bash|zsh|fish             Shell completion script
version
```

Global: `-o, --output table|json|yaml`. Table is the default and is formatted for
humans — its columns may change, so **scripts should use `--output json`**.
Every command above accepts `-o json`; streaming output (`events --follow`,
`workflow submit --watch`) is JSON Lines.

## Destructive commands ask first

`workflow delete`, `schedule delete`, `backfill cancel`, `worker drain`,
`worker quarantine` and `cache invalidate` print what they are about to do and
ask `Continue? [y/N]` on the terminal. `--yes` skips the prompt. With no
terminal (a script, CI, a pipe) and no `--yes` they **refuse** with exit code 3
and change nothing: a script that forgot `--yes` must fail loudly rather than
delete things because nobody was there to say no. The prompt is written to
stderr, so `-o json` output stays parseable.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Any other failure: a server fault (5xx), a failed `run create --wait`, an invalid spec or cron expression, a local file error |
| 2 | Usage: unknown command or flag, wrong operands, invalid flag value. Nothing was sent |
| 3 | Not confirmed: a destructive command was declined, or had no terminal and no `--yes` |
| 4 | Not found (HTTP 404): no such run, schedule, backfill, worker or workflow in the namespace |
| 5 | Conflict (HTTP 409, or a refused state transition): wrong state, name taken, still referenced |
| 6 | Authentication or authorization (HTTP 401/403) |
| 7 | Control plane unreachable, or answered 502/503/504 |
| 130 | Interrupted (Ctrl-C) |

So `nexusctl schedule get nightly >/dev/null 2>&1 || nexusctl schedule create ...`
can tell "does not exist" (4) from an outage (7). Codes are only ever added,
never renumbered.

## `init`: start a project

```bash
nexusctl init --list
nexusctl init etl orders-etl            # extract -> validate -> transform -> load
nexusctl init ml-training tuner         # grid via .map, select best, quality gate
nexusctl init batch-inference scoring --gpu
nexusctl init file-fanout files --namespace data
```

Each template is a runnable project: `dag.py` with resources, retries,
timeouts and caching set deliberately, `tests/` for the task functions,
`requirements.txt`, a `Dockerfile` whose tasks run through
`python -m nexus.entrypoint`, and a README. The workflow name defaults to the
directory name (made schema-valid) and the namespace to the resolved
`--namespace`. `init` writes only into an empty or new directory unless
`--force`, and even then touches only the template's own files. Templates are
embedded in the binary, so it works offline.

## `workflow submit --watch`

```bash
nexusctl workflow submit dag.py --watch            # submit on every save
nexusctl workflow submit dag.py --watch --dry-run  # validate on every save
```

Re-submits whenever the file (and, for a `.py` spec, any `.py` beside it)
changes, printing the validation result and the new version and content hash.
Saves within 300 ms are debounced into one submit; an edit that does not change
the compiled spec (a comment) is reported and not re-sent; an invalid save is
reported and watching continues. Ctrl-C stops it with exit code 0.

It **polls** (twice a second, hashing content) instead of using fsnotify: no
new dependency in a binary that is otherwise stdlib plus yaml.v3; it works on
Docker Desktop and VM bind mounts and on NFS/SMB, where inotify events are
delayed or never arrive; and it handles editors that save by renaming a temp
file over the original without re-arming a watch. The cost is at most half a
second of latency and hashing a few small files.

## Shell completion

```bash
source <(nexusctl completion bash)                  # bash, current shell
nexusctl completion zsh > "${fpath[1]}/_nexusctl"   # zsh (compinit enabled)
nexusctl completion fish > ~/.config/fish/completions/nexusctl.fish
```

The scripts call a hidden `nexusctl __complete` that walks the real command
tree, so commands, verbs, flags (per command), `-o` formats, `--overlap`
policies, `--state` values and `init` templates always match the binary. Other
operands fall back to file names.

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
4. With the same session, `POST /v1/namespaces/{ns}/tokens` also mints the
   agents' **worker token** (role `worker`, scope `worker_rpc`, the default
   90-day expiry), which the worker protocol accepts since F114 was fixed. Like
   the admin key, it is audited, listed and revocable in the console. `up`
   then points the agents and the NATS identity job at the tenant's namespace
   with a generated override file, reloads the broker, starts the agents, and
   waits until they register.

`up` writes nothing to the database directly. (Before F114 it inserted a
locally generated key's hash into `api_keys`, the only credential the worker
protocol then accepted.) A direct database bootstrap remains only where a
credential is needed before any API can be asked: the Helm chart's bootstrap
job (docs/OPERATIONS.md §20). `make seed` is the manual equivalent for a stack
you started with `make up` and your own `NEXUS_WORKER_API_KEY`.

When the worker token has expired, or expires within a week, `up` says so
before starting anything; an expired token otherwise looks like agents that
never register. Recover with `nexusctl down --volumes && nexusctl up`, or mint
a new worker token and restart the agents with it.

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

## Local runs and replay (ADR-0022)

```bash
nexusctl run --local examples/fan_out_map/compiled.yaml --param source=demo
nexusctl run --local dag.py --param source=s3://in --backoff-scale 0 -o json
nexusctl replay 3f2a9c1e-... --local --module mypkg.tasks
```

`run --local` executes the workflow with the Python SDK's local runner, in
**your** interpreter (`--python`). The decisions follow the platform's DAG
engine: trigger rules, propagation, `fail_fast`, map expansion and fan-in,
retries with backoff, timeouts, and memoization. Shell tasks get the agent's
NEXUS_* environment and Python handlers run through `python -m nexus.worker`.
It prints a per-node summary and exits 1 when the run fails. There are no
leases, heartbeats, queues, resources or GPU placement, and no
docker/kubernetes executors unless you pass `--host-executor`.

`replay` downloads `GET .../runs/{run}/attempts/{id}/assignment` (the TaskSpec
and resolved inputs; never the lease token or a secret value) to
`--out/assignment.json`. With `--local` it re-runs the attempt with its
original inputs and idempotency key. Blob inputs need `--input NAME=PATH`.

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

A 401 names `NEXUS_API_KEY`; a 422 on a spec lists each validation problem and
suggests `--dry-run`; a 409 on run creation explains the idempotency key (on
other routes, the state conflict); a refused workflow delete lists the runs
and schedules still using it; an unreachable endpoint names the endpoint it
tried. Locally detectable problems fail before any request is made.

Spec problems, whether found locally or returned by the gateway, are reported
where you would fix them. For YAML or JSON that is `file:line:col`. For a `.py`
spec it is the Python line of the `@task` or of the workflow-body call:

```
nexusctl: the control plane rejected pipeline.yaml:
  - pipeline.yaml:14:12: properties/queue/pattern: ... (/tasks/2/queue)
  - pipeline.yaml:22:22: stdout_output names "reprot", which is not declared ... (/tasks/load/runtime/stdout_output)
```

## Tests

```bash
go test ./cli/...
```

Covers arg parsing (including flags before and after the operand), output
formatting, config precedence, canonicalization parity with the Python SDK, error
message content, and that key material never reaches any output stream. Every
operator command is tested against an `httptest` fake gateway, including its
error paths, exit codes and confirmation prompts.

`TestEveryTemplateCompilesAndValidates` compiles each `init` template with the
SDK in a real interpreter, validates it against `schemas/workflow-spec-v1.json`
and the gateway's own submit pipeline, and runs the template's tests. It uses
`python3` with `sdk/python` on `PYTHONPATH`, or `NEXUS_TEST_PYTHON`; set that in
CI so a missing interpreter fails instead of skipping.

The `up`/`down`/`status` tests drive the real orchestration against a scripted,
stateful fake of the docker CLI and an `httptest` fake of the gateway and Mailpit
(`cli/internal/quickstart`): first-run seeding, idempotent re-runs, resume after
`down`, `down --volumes`, every error class, and that each secret is printed
exactly once. They never touch a Docker daemon or `~/.nexus`.
