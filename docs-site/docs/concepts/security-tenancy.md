# Security and tenancy

!!! warning "Read the known gaps first"
    Nexus has not passed its production acceptance gates. Inside KGF Cloud,
    people sign in through KGF and Nexus verifies KGF's signed request
    context; Nexus has no single sign-on of its own (no OIDC, SAML or SCIM).
    Inter-service TLS is supported but opt-in. Tenancy is enforced by the
    application on shared infrastructure. The [design README](repo:docs/README.md)
    keeps the current list.

## Tenants and namespaces

A **tenant** owns exactly one **namespace**. Every customer-owned table
carries a non-null `tenant_id` with foreign keys that prevent cross-tenant
links, and scheduling filters by tenant in both directions
([Multi-tenancy](../design/multitenancy.md)). Namespaces come into existence
in one of two ways:

- **Standalone:** self-registration (`POST /v1/auth/register`, or
  `nexusctl up` on a laptop) creates a tenant with one namespace, whose name
  is the tenant's UUID, and an owner with the protected Administrator role.
- **Inside KGF Cloud** (`NEXUS_AUTH_MODE=kgf`): one namespace per KGF
  **(organization, subscription, resource group)**, derived from the triple
  and provisioned on the first request for it
  ([KGF Cloud](kgf-cloud.md), [ADR-0024](../design/adr/0024-nexus-inside-kgf-cloud.md)).

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
| Viewer | see overview, workflows, runs, tasks, schedules, backfills, workers, quotas, notifications, triggers, sensors and assets |
| Workflow Operator | Viewer, plus create, cancel, pause, resume and retry runs; read logs and results; manage schedules and backfills |
| Workflow Developer | Operator, plus publish workflows, delete schedules, invalidate the cache, and manage triggers, sensors and notifications |
| Infrastructure Operator | workers (drain, quarantine), infrastructure configuration, quotas, usage |
| Auditor | audit log, users, roles, tokens, service accounts, quotas, usage (read) |
| Administrator | everything in the namespace |

Custom roles are built from the same permission catalog
(`PUT /v1/namespaces/{ns}/roles/{name}`). The target model and what is
implemented today are described in [RBAC design](../design/rbac-design.md)
and [RBAC implementation](../design/rbac-implementation.md).

In KGF mode a person's KGF role (operator or viewer) is mapped onto these
permissions through KGF's built-in Contributor and Reader action sets, and
Nexus re-derives the decision rather than trusting the caller
([runbook §24.5](../operations/runbook.md#245-authorization)). A KGF role never
grants user administration (KGF owns people) or `audit.manage`.

## Credentials

| Kind | Prefix | For |
| --- | --- | --- |
| KGF context | `X-KGF-Context` | people and KGF services inside KGF Cloud: an Ed25519 token the KGF gateway signs per request, verified for signature, audience (`kgf-nexus`), lifetime, issuer and replay ([runbook §24](../operations/runbook.md#24-running-nexus-inside-kgf-cloud-context-authentication-tenancy-mapping-and-service-accounts)) |
| Session | `nxsess_` | people in standalone mode, created by `POST /v1/auth/login`. Unverified email addresses cannot sign in. Refused in `NEXUS_AUTH_MODE=kgf`. |
| Scoped token | `nxtok_` | automation. It has a role, scopes (`read`, `write`, `admin_write`, `worker_rpc`), an optional permission ceiling and an expiry, and can be revoked. |
| Service-account token | `nxtok_` | agents and CI: a token of a service account, holding the account's current roles intersected with the token's ceiling. Disabling the account stops its tokens at once. |
| Worker credential | role `worker` | agents only. REST refuses it, and it authenticates the worker gRPC protocol. |

A request carrying both an `X-KGF-Context` and an `Authorization` header is
refused (`400 ambiguous_credentials`) rather than resolved. The gateway stores
only hashes. Authentication failures are rate limited per address, and the
response never says *why* a credential was refused
([runbook §16](../operations/runbook.md#16-gateway-rate-limiting)).
`nexusctl` never prints key material, not even in `config view`.

**Workers never hold database credentials.** A worker can report only on
attempts it holds leases for, and the lease fencing rejects reports on
anything else.

### What a task can see of the agent

A subprocess task's environment is built from an **allow-list**, never
inherited wholesale (F151): a base set (`PATH`, `HOME`, locale, `TZ`,
`TMPDIR`, user), the task's own `runtime.env`, its env-injected secrets, and
the task contract the SDK relies on -- the `NEXUS_*` attempt variables,
`NEXUS_INLINE_THRESHOLD_BYTES`, the attempt's `TRACEPARENT`/`TRACESTATE` and
its workload API (`NEXUS_TASK_API`, `NEXUS_TASK_API_TOKEN`). The agent's own
credentials -- its worker key, result-store keys and cloud identities, NATS
seed, Vault and Azure credentials, mTLS key, trace-exporter headers -- never
pass, even when an operator's passthrough pattern
(`NEXUS_SUBPROCESS_ENV_PASSTHROUGH`) matches them. `docker` and `kubernetes`
tasks start from an empty environment. The subprocess executor still runs
tasks as the agent's user, so keep untrusted or multi-tenant workloads on
`docker` or `kubernetes` pools
([runbook §25.1](../operations/runbook.md#251-worker-side-secret-resolution)).

## Secrets

A spec references secrets, never contains them, and says how each value
reaches the workload:

```yaml
runtime:
  secrets:
    - {name: DB_PASSWORD, ref: "vault://kv/etl/db#password"}          # env (default)
    - {name: tls.key,     ref: "akv:///nexus-team-a-tls-key", inject: file}
```

The schema accepts only a provider URI, so a pasted credential fails
validation. **Workers resolve the references** at attempt start, with the
providers configured for their own namespace only (`env://`, `file://`,
`vault://` for Vault KV v2, `akv://` for Azure Key Vault); the control plane
never sees a value. A task gets every declared secret or does not start, and
the values are redacted from shipped logs, progress notes and error messages.
See the [secrets recipe](../recipes/secrets.md) and
[runbook §25.1](../operations/runbook.md#251-worker-side-secret-resolution).

## Transport

TLS and mTLS between services, and TLS to NATS, are supported and opt-in.
Plaintext is fatal only with `NEXUS_REQUIRE_TLS` set ([TLS](../operations/tls.md)).
The public REST port and the metrics listener are separate, and `/metrics` is
never served on the public port. Services connect to PostgreSQL with
`verify-full` against a pinned CA bundle and as their own least-privilege
logins in the Helm chart ([runbook §20.11-§20.12](../operations/runbook.md#2011-database-tls-verify-full-and-the-ca-bundle)).

## Audit

User, role, token, service-account, quota, trigger and infrastructure changes
write audit rows (`GET /v1/namespaces/{ns}/audit`, permission `audit.view`).
Audit rows name token ids, never tokens. The trail is **hash-chained per
namespace**, so an export can be verified offline (`nexusctl audit verify`),
and security grants **fail closed**: a credential is not minted if its audit
row cannot be written. It can be exported as NDJSON, CSV or KGF's audit-event
shape and delivered continuously to signed webhook sinks, with retention and
legal hold (`audit.manage`). Rows written for a KGF-authenticated request
record the KGF organization, subscription, resource group, principal and
request id ([runbook §25.2](../operations/runbook.md#252-the-audit-trail-pagination-export-kgf-cloud-sinks-retention-hash-chain)).
