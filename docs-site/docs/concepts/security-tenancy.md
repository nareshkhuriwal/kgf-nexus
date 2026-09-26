# Security and tenancy

!!! warning "Read the known gaps first"
    Nexus has not passed its production acceptance gates. OIDC/SAML single
    sign-on and SCIM are not implemented. Inter-service TLS is supported but
    opt-in. Tenancy is enforced by the application on shared infrastructure.
    The [design README](repo:docs/README.md) keeps the current list.

## Tenants and namespaces

Self-registration (`POST /v1/auth/register`, or `nexusctl up` on a laptop)
creates a **tenant** with one **namespace**, whose name is the tenant's UUID,
and an owner with the protected Administrator role. Every customer-owned
table carries a non-null `tenant_id` with foreign keys that prevent
cross-tenant links, and scheduling filters by tenant in both directions
([Multi-tenancy](../design/multitenancy.md)).

This is **logical** isolation on shared infrastructure. There are no dedicated
databases, no PostgreSQL row-level security for the trusted service roles, and
no per-tenant encryption keys. It is not a hostile multi-tenant sandbox. In
particular, a local Docker-socket agent is effectively root on its host.

## Who can do what

Each REST route has one authorization decision: a coarse action (`read`,
`write`, `admin_write`) and one or more **module permissions**, such as
`runs.create`, `workflows.publish` or `workers.drain`. The
[API reference](../reference/api/index.md) lists them per operation as
`x-nexus-permissions`. The built-in roles are:

| Role | Can |
| --- | --- |
| Viewer | see overview, workflows, runs, tasks, schedules, backfills, workers and quotas |
| Workflow Operator | Viewer, plus create, cancel, pause, resume and retry runs; read logs and results; manage schedules and backfills |
| Workflow Developer | Operator, plus publish workflows, delete schedules and invalidate the cache |
| Infrastructure Operator | workers (drain, quarantine), infrastructure configuration, quotas, usage |
| Auditor | audit log, users, roles, tokens, quotas, usage (read) |
| Administrator | everything in the namespace |

Custom roles are built from the same permission catalog
(`PUT /v1/namespaces/{ns}/roles/{name}`). The target model and what is
implemented today are described in [RBAC design](../design/rbac-design.md)
and [RBAC implementation](../design/rbac-implementation.md).

## Credentials

| Kind | Prefix | For |
| --- | --- | --- |
| Session | `nxsess_` | people, created by `POST /v1/auth/login`. Unverified email addresses cannot sign in. |
| Scoped token | `nxtok_` | automation. It has a role, scopes (`read`, `write`, `admin_write`, `worker_rpc`) and an expiry, and can be revoked. |
| Worker credential | role `worker` | agents only. REST refuses it, and it authenticates the worker gRPC protocol. |

The gateway stores only hashes. Authentication failures are rate limited per
address, and the response never says *why* a credential was refused
([runbook §16](../operations/runbook.md#16-gateway-rate-limiting)).
`nexusctl` never prints key material, not even in `config view`.

**Workers never hold database credentials.** A worker can report only on
attempts it holds leases for, and the lease fencing rejects reports on
anything else.

!!! danger "Subprocess tasks inherit the agent's credentials"
    The `subprocess` executor passes the **agent's whole environment** to the
    task (`agent/internal/executor/common.go`, `buildEnv`). On the compose stack
    that includes `NEXUS_API_KEY` (the worker credential) and the result store
    keys (`NEXUS_S3_ACCESS_KEY_ID`/`NEXUS_S3_SECRET_ACCESS_KEY`,
    `RESULT_STORE_*`). This was verified with a task that listed variable
    names. Any code that runs as a subprocess task can therefore act as the
    worker, and can reach the result store with the agent's keys. The
    `docker` and `kubernetes` executors do **not** inherit the agent's
    environment. Run untrusted or multi-tenant workloads only on `docker` or
    `kubernetes` pools, and treat `subprocess` pools as trusted.

## Secrets

A spec may reference secrets, never contain them:

```yaml
runtime:
  secrets:
    - {name: DB_PASSWORD, ref: "vault://kv/etl/db"}
```

The schema accepts only a provider URI, so a pasted credential fails
validation.

!!! danger "Secret references are not resolved yet"
    **No worker resolves `runtime.secrets` today** (`agent/internal/worker/runner.go`).
    A declared reference is stored and shown, and has **no effect** on the
    task's environment. Until a resolver exists, provide credentials through
    the executor's own mechanism, as described in the
    [secrets recipe](../recipes/secrets.md).

## Transport

TLS and mTLS between services, and TLS to NATS, are supported and opt-in.
Plaintext is fatal only with `NEXUS_REQUIRE_TLS` set ([TLS](../operations/tls.md)).
The public REST port and the metrics listener are separate, and `/metrics` is
never served on the public port.

## Audit

User, role, token, quota and infrastructure changes write audit rows
(`GET /v1/namespaces/{ns}/audit`, permission `audit.view`). Audit rows name
token ids, never tokens.
