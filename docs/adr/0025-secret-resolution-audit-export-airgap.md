# ADR-0025: Worker-side secret resolution, a tamper-evident exportable audit trail, and air-gapped installation

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0007 (executors), ADR-0008 (result store), ADR-0011 (table ownership, additive migrations), ADR-0020 (durable task logs and the redaction hook)

## Context

Three production gaps share one theme: evidence and trust boundaries that a
security review asks about first.

1. **Secrets are declared but never resolved.** `RuntimeSpec.secrets` carries
   `SecretRef{name, ref}` from the SDK through the gateway to the agent, and the
   agent ignores it. A task that needs a credential today must receive it as a
   literal in `runtime.env` — stored in `workflow_specs`, returned by the spec
   API, shown in the console — which is precisely the leak the SecretRef schema
   exists to prevent. ADR-0020's log redactor (`logship.Redactor`, fed by
   `Runner.SecretValues`) is wired but has no input: nothing produces values.
2. **The audit trail is not exportable, verifiable or reliable.**
   `GET /audit` returns at most 1000 rows with no cursor; there is no export,
   no SIEM delivery, no retention policy, no legal hold, and nothing would
   reveal an edited or deleted row. `auditAttempt` logs and swallows a failed
   audit write, so a credential can be minted with no audit record at all.
3. **Installation assumes internet egress.** Images are pulled from public
   registries at install time; a regulated customer with no egress cannot
   install without reverse-engineering the image list.

Constraints: workers hold no database credentials; secret values never enter
the metadata store; migrations are additive (ADR-0011 rule 5); the agent is
bound to exactly one namespace (`--namespace`), and a namespace is exactly one
tenant (MULTITENANCY.md).

**Nexus is a service of KGF Cloud** (kgf-cloud: the `KGF.Nexus` provider
behind KGF's gateway), not a standalone platform. Organization-level concerns
-- identity, access reviews, vendor management, the compliance program --
belong to KGF Cloud. Two KGF conventions bind this ADR: KGF's gateway signs an
Ed25519 **context token** (`X-KGF-Context`, kgf-cloud `pkg/tenantctx`) naming
the organization, subscription, resource group, principal and request id of
every request it forwards; and KGF keeps secrets in one **platform Key Vault**
per environment (`kgf-platform-kv-<env>`, RBAC), reached through **workload
identity** and projected by the Key Vault CSI driver, with service-prefixed
kebab-case secret names. KGF's own audit is `audit_events` (organization_id,
id, action, resource_id, subscription_id, resource_group, created_at), written
by its services and exported per project; KGF has **no endpoint that ingests
another service's audit events**.

## Decision

### 1. Secrets are resolved by the agent, per tenant, at attempt start

**Resolver.** `agent/internal/secrets` defines

```go
type Provider interface {
    Scheme() string
    Resolve(ctx context.Context, ref Ref) ([]byte, error)
}
```

and a `Resolver` that parses each `SecretRef.ref`, dispatches by scheme, and
returns `[]Resolved{Name, Value, Inject}`. Providers:

| Scheme | Ref grammar | Source | Notes |
|---|---|---|---|
| `env://` | `env://NAME` | the agent's own environment | NAME must match the tenant's allow-list (glob). Default allow-list is **empty**: an agent's environment holds its own control-plane credential, and `env://NEXUS_API_KEY` must not be expressible. |
| `file://` | `file:///abs/path` | a mounted secret file (Kubernetes Secret volume, CSI driver, systemd credential) | Path must be absolute, is `filepath.Clean`ed, symlinks resolved, and must stay under an allow-listed directory prefix after resolution. Size-capped (1 MiB). |
| `vault://` | `vault://<mount>/<path>[?version=N][#<field>]` | HashiCorp Vault KV v2 over its HTTP API | Auth: `kubernetes`, `approle`, `token` (tokens and secret-ids read from files, never inline config). Vault Enterprise namespaces (`X-Vault-Namespace`). TLS with CA file, server name, optional client cert; `insecure_skip_verify` refused unless the address is loopback. Mount+path must match the tenant's allow-listed prefixes. No field ⇒ the secret's `data` map as JSON. KV has no lease to renew; the agent re-authenticates when its token expires or is rejected. |
| `akv://` | `akv://<vault-name>/<secret-name>[/<version>]`, or `akv:///<secret-name>` for the tenant's `default_vault` | Azure Key Vault via the official Azure SDK (`azidentity`, `azsecrets`) | Auth: workload identity (KGF's convention: the stamp's federated identity), managed identity, or client credentials (secret or certificate read from a file). The vault name must be allow-listed for the tenant; the vault URL is derived (`https://<name>.vault.azure.net`) unless configured (sovereign clouds). On KGF Cloud `default_vault` is the platform vault, and `allow_secret_prefixes` (`nexus-<tenant>-`) is the tenant fence inside that shared vault. |

`aws-sm://` and `gcp-sm://` are **not** implemented in this change; they fit
the same interface and are listed as a gap rather than stubbed.

**Per-tenant configuration.** `NEXUS_SECRETS_CONFIG` points at a YAML file
with a `tenants:` map keyed by namespace. The agent loads **only its own
namespace's entry** at start-up and discards the rest, so an agent can never
authenticate to another tenant's Vault role or Key Vault, even if an operator
shares one file across fleets. A missing entry means no providers: any secret
reference then fails as `PERMISSION_DENIED`. Credentials in the file are
always *paths* to files (token file, AppRole secret-id file, client-secret
file); an inline credential field does not exist.

**Declared injection.** `SecretRef` gains one additive field, `inject`
(`"env"` default, or `"file"`), in the proto (field 3), the JSON Schema and
the SDK. `env` sets the variable `name` in the workload. `file` writes the
value to `$NEXUS_SECRETS_DIR/<name>` (directory 0700, file 0400) and never
into the environment. Per executor:

- **subprocess:** env appended after the task env and before platform vars;
  files in a per-attempt directory removed by Cleanup.
- **docker:** env through `--env-file` written 0600 beside (never inside) the
  bind-mounted work directory and removed as soon as `docker run` has read it
  (never on the `docker run` argv, where `ps` shows it); a multi-line value
  cannot be an env-file entry and is refused with "use inject: file";
  files bind-mounted read-only at `/run/nexus/secrets`. Values remain
  visible to anyone with access to the Docker socket (`docker inspect`), who is
  root on that host anyway — documented, not hidden.
- **kubernetes:** values go into the per-attempt Kubernetes Secret the
  executor already creates (owned by the Job, garbage-collected with it) and
  are referenced by `valueFrom.secretKeyRef` / a Secret volume. **Never** a
  literal `env.value` in the pod spec, which any `get pods` reader and etcd
  backups would see.

Platform variables still override task variables; a secret may not use a
`NEXUS_*` name (rejected as VALIDATION), nor a name also set in `runtime.env`.
Host-side secret files live under `NEXUS_SECRETS_DIR_ROOT` when set (the Helm
chart mounts a memory-backed emptyDir there), so they never reach a disk.

**Where values never go.** Values live in agent memory for the attempt and in
the substrate-specific injection above. They are not placed in: the WAL
(`wal.Entry` has no field for them), the result store, `ReportResult` /
`ReportProgress` / events, agent log lines (`Resolved` implements
`slog.LogValuer` and `fmt.Formatter` as `[REDACTED]`), or panics/crash dumps
(errors carry the ref and name, never a value; the resolver never wraps a
provider response body into an error). Resolved values are passed to
`Runner.SecretValues` so the ADR-0020 redactor replaces them in shipped task
logs, and the runner's own `streamLogs` copy into the agent log uses the same
redactor.

**Failure classes.** A resolution failure fails the attempt **before** the
workload starts, with `TaskError.type = "SecretResolutionFailed"` and a class
chosen by cause:

| Cause | Class | Retryable |
|---|---|---|
| malformed ref, unknown scheme, `NEXUS_*` name, missing field in a secret | VALIDATION | no |
| scheme not configured for this tenant, path/name not allow-listed, provider answered 401/403 | PERMISSION_DENIED | no |
| provider answered 404 / secret or version absent | NOT_FOUND | no |
| provider unreachable, timeout, 429, 5xx, Vault sealed/standby | TRANSIENT | yes |

### 2. The audit trail is paginated, exportable, delivered, retained and hash-chained

- **Cursor pagination.** `GET /audit` gains `cursor` / `next_cursor`: an
  opaque keyset cursor, stable under concurrent inserts, with `since`/`until`
  filters. The existing response shape is a superset (additive).
- **Export.** `GET /audit/export?format=ndjson|csv|kgf&since=&until=&action=&outcome=&actor_id=`
  streams rows in chain order from one snapshot, with no row cap and bounded
  memory. CSV is RFC 4180 with formula-injection neutralisation (a cell
  starting with `= + - @` is prefixed with `'`, as KGF's own export does).
- **KGF Cloud compatibility.** When a request carries an `X-KGF-Context` that
  verifies against KGF's configured public keys (same rules as kgf-cloud's
  `tenantctx.Verifier`: EdDSA, kid, lifetime, audience `nexus`, only a
  user-issuer may assert a person), the audit row records the KGF
  organization, subscription, resource group, principal, on-behalf-of and
  request id -- all covered by the hash chain. The token is used for
  **attribution only**; Nexus's own credentials still authorize, and an
  unverifiable token contributes nothing. `format=kgf` (export and sinks)
  emits KGF's audit event shape (organization_id, id, action, resource_id,
  subscription_id, resource_group, created_at) with the Nexus facts and the
  chain position alongside; the field mapping is in OPERATIONS §25.2.
- **Sinks.** Because KGF has no ingestion endpoint, Nexus keeps a generic,
  standalone webhook sink (format `nexus` or `kgf`) configured per namespace
  by an admin; syslog and OTLP are not implemented. Delivery is **at-least-once from a durable
  per-sink cursor** over the append-only chain — the audit table itself is the
  outbox, so no second copy of each row is written and a sink added later can
  be back-filled from any position still retained. Webhook bodies are
  NDJSON batches signed with HMAC-SHA256 over `timestamp + "." + body`
  (`X-Nexus-Signature: t=<unix>,v1=<hex>`), retried with capped exponential
  backoff. The signing secret is generated by the gateway, returned once, and
  stored AES-256-GCM-sealed (bound to the namespace) under the gateway's
  `NEXUS_AUDIT_SINK_KEY`; it is never returned again. Destinations are HTTPS
  and may not resolve to internal addresses unless allow-listed (checked at
  dial time). Exactly one gateway replica delivers a given sink at a time
  (advisory lock).
- **Retention and legal hold.** Per-namespace `retention_days` (default: keep
  forever) and a `legal_hold` flag with reason and actor. Purging runs through
  a `SECURITY DEFINER` function, not a grant: the gateway role stays
  INSERT-only on `audit_log`. The function refuses while a legal hold is set,
  never purges rows that an enabled sink has not yet delivered, and records a
  **chain checkpoint** (last purged sequence and hash) so verification of the
  remainder still starts from a known anchor.
- **Hash chain.** Each namespace has its own chain: `chain_seq`,
  `prev_hash`, `row_hash = sha256(canonical_v1(row))`, where canonical_v1 is a
  JSON array of every column including the KGF attribution and, last,
  `prev_hash`. The
  append takes a row lock on the namespace's chain head in the same
  transaction as the insert, so concurrent gateway replicas cannot fork the
  chain. The canonical encoding is defined in code and in OPERATIONS §25 so
  an external verifier can be written independently. `nexusctl audit verify`
  re-computes the chain from the API or from an exported NDJSON file,
  offline. The chain proves that retained rows were not altered, inserted or
  removed *relative to each other*; it does not stop a database superuser
  from rewriting the whole tail. Anchoring is external: the SIEM sink and
  exports carry `row_hash`, so a rewritten tail disagrees with the copy
  already delivered.
- **Fail-closed where it matters.** Actions are classed. **Security grants**
  (authentication success, users created/enabled/updated, password resets,
  tokens minted, role assignment, tenancy, platform/cluster/quota
  configuration, and every audit-configuration change: retention, legal hold,
  sinks) write their audit row in the same transaction as the change and
  **fail the request (503 `audit_unavailable`) with the change rolled back if
  the audit write fails.** **Security revocations** (logout, token revoke, user
  disable, sessions revoked, role delete) are **never** blocked -- refusing to
  revoke because the audit store is down would keep a credential alive -- and
  a failed write is logged as `AUDIT RECORD LOST` and counted. Denied
  attempts remain denied if the audit write fails.
  **Operational** actions (run retry/delete, schedules, quarantine, cluster
  and platform configuration that already audit transactionally keep doing
  so) log and count the failure (`nexus_audit_write_failures_total`) rather
  than blocking an operator mid-incident. The class of each action is a table
  in code, reviewed like the RBAC matrix.

### 3. Air-gapped installation is a bundle plus a mirror script

`make airgap-bundle` writes `dist/nexus-airgap-<version>.tar.gz` containing:
every image the chart and compose reference, resolved to and saved by
**digest** (skopeo/oras when present, `docker save` otherwise); the packaged
Helm chart; `nexusctl` and `nexus-agent` binaries for linux/amd64 and
linux/arm64 (and darwin for the CLI); the SDK wheel; `schemas/`; and
`manifest.json` + `SHA256SUMS` over every file, signed with `cosign sign-blob`
when cosign is available (unsigned bundles say so in the manifest).
`deploy/airgap/load.sh` verifies checksums (and the signature if present),
pushes every image to a private registry, records the digests the registry
reports, and renders a Helm values file that points every image at the
private registry **by digest**.

## Consequences

**Positive** — task credentials leave the metadata store entirely; the
redactor finally has input; a tenant's agent cannot reach another tenant's
secret backend by configuration error; the audit trail can be handed to an
auditor with a verification procedure, streamed to a SIEM, retained on
policy, frozen under legal hold; credential minting can no longer happen
unaudited; installation needs no egress.

**Negative** — secret *values* now exist on worker hosts (memory, the
per-attempt Kubernetes Secret, the docker env-file and secret files for the
attempt's lifetime); the work root should be on tmpfs for file-injected
secrets and this is documented, not enforced. Env injection is visible to
same-host root and in `/proc/<pid>/environ` for the task's UID; `file` is the
recommended mode. Redaction is per-line and exact-match (ADR-0020 limits):
an encoded or split secret is not caught. Security-sensitive administration
now fails when the audit store is down. Per-namespace chain appends serialize
on one row lock per namespace (audit volume is low; measured in tests, not
assumed). The azure SDK adds dependencies to the agent binary. A superuser
can still rewrite an un-exported tail. `docker save`/`load` may change
manifest digests on push, which is why the load script records registry
digests rather than trusting bundle digests for the rendered values.

**Neutral** — the proto, schema and SDK change is one optional field whose
default preserves every stored spec's content address. The export and sink
formats are versioned (`"v": 1`).

## Alternatives considered

1. **Resolve secrets in the control plane and send values in the
   assignment.** Rejected: values would transit and possibly persist in the
   control plane, the one place the SecretRef contract promises they never
   reach, and every control-plane replica would need every tenant's secret
   backend credentials.
2. **Vault Agent / CSI driver only (no in-agent Vault client).** Supported —
   that is what `file://` is for — but not sufficient alone: subprocess and
   docker executors on VMs have no CSI, and the SDK authoring model names a
   Vault path directly.
3. **The official `hashicorp/vault/api` client.** Rejected for now: a large
   dependency tree for four HTTP calls (login ×3, KV read); the HTTP API is
   stable and versioned. Revisit if dynamic secrets with leases are added.
4. **Encode injection mode in the URI (`?as=file`).** Rejected: it mixes
   workload wiring into a provider address and every provider would need to
   strip it; an explicit field is validated by the schema.
5. **Hash chain computed by a trigger over `detail::text`.** Rejected: an
   offline verifier cannot reproduce PostgreSQL's jsonb text form from an
   export without re-implementing its numeric normalisation. The gateway
   computes the hash over a canonical encoding of PostgreSQL-normalised
   values, in the same transaction, under the head lock.
6. **Copy every audit row into the NATS outbox (0016).** Rejected: a second
   copy of every row, a broker hop the SIEM does not need, and no back-fill
   for a sink added later; a per-sink cursor over the append-only table gives
   the same at-least-once guarantee.
7. **Fail closed for every audited action.** Rejected: a database hiccup would
   block run retries and schedule edits during the incident an operator is
   trying to fix, with no security benefit.
8. **Write Nexus audit rows into KGF Cloud's `audit_events` directly.**
   Rejected: KGF exposes no ingestion API, its table is owned by KGF services
   under tenant RLS, and a second writer would couple Nexus to KGF's schema
   and credentials. A KGF-shaped export and sink let a KGF-side collector own
   the write.
9. **Trust KGF headers without verification.** Rejected: any caller could
   then claim any KGF organization or principal in the audit trail.
