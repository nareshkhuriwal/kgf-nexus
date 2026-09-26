# Secrets

## The rule

**A credential never goes into a spec, a parameter, a URL or a log.** Specs
are stored, versioned, shown in the console, and returned by the API to
anyone with `workflows.view`. Run parameters are visible in the same way.

The spec grammar enforces this: `runtime.secrets` accepts **references
only**, as provider URIs, and the schema rejects a bare value.

## Declaring a secret

```yaml
runtime:
  secrets:
    - {name: DB_PASSWORD, ref: "vault://kv/team-a/db#password"}           # an environment variable
    - {name: tls.key,     ref: "akv:///nexus-team-a-tls-key", inject: file} # a file
```

| `inject` | The workload sees |
| --- | --- |
| `env` (default) | the variable `name` |
| `file` | the file `$NEXUS_SECRETS_DIR/<name>` (mode 0400), and nothing in its environment |

**Prefer `file`.** Environment variables are visible to root on the same host
(`docker inspect`, `/proc/<pid>/environ`); a file is not inherited by every
child process and does not end up in a crash dump of the environment.

From the Python SDK, declare them on the task's runtime:

```python
import os
from nexus import Runtime, SecretRef, task

@task(runtime=Runtime(secrets=[
    SecretRef("DB_PASSWORD", "vault://kv/team-a/db#password"),
    SecretRef("tls.key", "akv:///nexus-team-a-tls-key", inject="file"),
]))
def load(ctx, rows: list) -> dict:
    password = os.environ["DB_PASSWORD"]
    ctx.log.info("connecting", host=HOST)       # never the password
    ...
```

## How it resolves

**The worker agent resolves every declared reference** when the attempt
starts, before the workload is prepared, with the secret providers configured
for **its own namespace only**; the control plane never sees a value. A task
gets every secret it declared or does not start: a reference that cannot be
resolved fails the attempt with `SecretResolutionFailed`, classified so the
retry policy can decide (`VALIDATION` for a malformed reference,
`PERMISSION_DENIED` when the provider or path is not allowed for the tenant,
`NOT_FOUND`, or a retryable `TRANSIENT` when the backend is unreachable).

| Scheme | Reference | Backend |
| --- | --- | --- |
| `env://` | `env://NAME` | the agent's environment, allow-listed per tenant; the agent's own credentials are never resolvable |
| `file://` | `file:///abs/path` | a mounted secret file (a Kubernetes Secret volume, the Key Vault CSI driver), inside the tenant's allowed directories |
| `vault://` | `vault://<mount>/<path>[?version=N][#field]` | HashiCorp Vault KV v2 (Kubernetes, AppRole or token auth) |
| `akv://` | `akv://<vault>/<secret>[/<version>]`, `akv:///<secret>` | Azure Key Vault (workload identity, managed identity, or a client credential) -- inside KGF Cloud, the platform Key Vault |

The operator configures the providers per namespace in the agent's secrets
file (`NEXUS_SECRETS_CONFIG`; Helm `agentDefaults.secrets`), with allow-lists
for paths, vaults and secret-name prefixes. The format, the failure classes
and a manual Key Vault test are in
[runbook §25.1](../operations/runbook.md#251-worker-side-secret-resolution).

## Where values go, and where they do not

- **subprocess:** env-injected values in the task's environment, file-injected
  ones under `NEXUS_SECRETS_DIR` on a memory-backed directory, removed when
  the attempt ends.
- **docker:** a 0600 `--env-file` removed right after `docker run`, files
  bind-mounted read-only at `/run/nexus/secrets`.
- **kubernetes:** a per-attempt Secret, referenced by `secretKeyRef` and a
  projected volume, never a literal in the pod spec.

Values never enter the agent's write-ahead log, reports, events or its own
log lines. Shipped task logs, progress notes and error messages pass a
redactor that replaces every resolved value with `[REDACTED:<name>]`
([logs](../concepts/logs.md#secrets-in-logs)). A subprocess task's
environment is built from an allow-list, so the agent's own credentials never
reach it ([security](../concepts/security-tenancy.md#what-a-task-can-see-of-the-agent)).

`nexus.local` and `nexusctl run --local` run on your machine and do **not**
resolve references: export the values a task needs under the names it
declares. `nexusctl import` reports source secrets for you to declare as
references.

## Checklist

- Pool separation: a queue whose workers hold production secret-provider
  access should serve only the workloads that need it. `node_selector` and
  queues are the routing tools ([queues](../concepts/queues-fair-share.md)).
- Never pass a secret as `--param`. Parameters are stored with the run and
  shown in `run get`.
- Do not print environment variables in tasks. Logs are durable and
  searchable; the redactor is a backstop, not a licence.
- For API clients, prefer short-lived scoped or service-account tokens
  (`POST /v1/namespaces/{ns}/tokens` with an expiry and a permission ceiling)
  over long-lived keys, and keep them in `NEXUS_API_KEY`, never in a
  command-line flag.
