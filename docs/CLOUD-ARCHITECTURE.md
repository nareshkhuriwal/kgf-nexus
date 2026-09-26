# Nexus Cloud — architecture (design only)

**Status:** Design proposal, recorded as [ADR-0033](adr/0033-nexus-cloud-hosted-control-plane.md)
(*Proposed — pending owner sign-off*). **Nothing in this document is built or
deployed.** No hosted service exists, and no account, region or price has been
created. Where the design depends on something the repository already has, it
names the file, ADR or finding. Where it depends on something missing, it says
so and lists it under [§14 Gaps](#14-gaps-between-todays-code-and-this-design).
Decisions that belong to the owner are marked **OPEN-n** and collected in
[§15](#15-open-decisions-for-the-owner).

This document makes no production-readiness claim. It also sets no availability
figure as a promise: the SLO numbers in §9 are proposed targets to be validated
before anything is sold against them.

---

## 1. What Nexus Cloud is

Nexus Cloud is a **hosted, multi-tenant control plane with customer-run
workers**. The vendor runs the API gateway, workflow manager, DAG engine,
scheduler, resource manager, event service, the metadata store (PostgreSQL) and
the console. Customers run the worker agents on their own machines, VMs or
Kubernetes clusters, and task code and task data stay there.

This is the shape the architecture already has. It is not a new split:

| Existing invariant (ARCHITECTURE.md §3.1) | What it gives a hosted service |
|---|---|
| 1. No user code in the control plane | The vendor never runs customer code. The hosted plane parses `WorkflowSpec` JSON and nothing else. |
| 2. No database credentials in the execution plane | A compromised customer worker can reach only the gateway's narrow gRPC surface, authenticated as its own tenant. |
| 3. Task payloads do not transit the control plane | Large inputs and outputs go agent to object store. The customer can own that store (§5.3). |
| Pull-based dispatch (ADR-0005) | Agents dial out. The customer opens no inbound port, and there is no vendor-to-customer network path. |

The exceptions to "the data stays with the customer" matter for compliance and
are listed in §5.3: inline results up to the inline threshold (64 KiB by
default, ADR-0016) are stored in the metadata store, workflow parameters and
spec literals are stored there, and the console reads task logs and artifact
previews through the gateway.

### 1.1 Non-goals

- **Vendor-run compute.** Managed workers (vendor-hosted execution) are a
  possible later product. They are not part of this design, because they bring
  hostile-code isolation (sandboxing, per-tenant VMs) that the platform does not
  have. The production brief says so directly: a local Docker socket is not
  hostile multi-tenant isolation.
- **Exactly-once.** The hosted service keeps ADR-0006's at-least-once contract.
- **A different product from the open-source one.** The control plane that runs
  in the cloud is the same code as the Apache-2.0 release, plus the
  enterprise and cloud-operations modules in §12.

---

## 2. Topology

```
 Customer network (any cloud, on-prem, laptop)          Vendor: Nexus Cloud, region R
 ┌───────────────────────────────────────────┐          ┌───────────────────────────────────────────────┐
 │  nexus-agent (N)  ── gRPC/TLS, outbound ──┼──443────►│  Edge: L7 LB / WAF / DDoS protection           │
 │      │  mTLS client cert (tenant CA)      │          │     │                                          │
 │      │  or nxtok_ worker token            │          │     ▼                                          │
 │      │                                    │          │  Cell k (one of several per region)            │
 │      │  ── NATS/TLS, outbound, optional ──┼──443/   ►│   api-gateway ×n   scheduler ×n   wfm, dag,     │
 │      │     NKey agent-<ns>, subscribe-only│   7422   │   resource-mgr, event-service, console         │
 │      ▼                                    │          │   PostgreSQL (HA, PITR)   NATS JetStream        │
 │  customer object store (BYO bucket)       │          │   vendor object store (logs/artifacts, if not   │
 │  customer secrets (Vault, Key Vault)      │          │   BYO)                                          │
 └───────────────────────────────────────────┘          │                                                │
                                                        │  Global (not per cell): signup, cell router,   │
 Browser / CLI / SDK ──── HTTPS 443 ───────────────────►│  billing export, status page, audit archive    │
                                                        └───────────────────────────────────────────────┘
```

- **Outbound only.** The agent opens every connection: gRPC to the gateway for
  register, heartbeat, poll, report and logs (ADR-0014 lists the proxy
  requirements: HTTP/2 end to end, trailers, ≥ 60 s timeouts, ≥ 4 MiB bodies),
  and optionally NATS for dispatch hints. Losing NATS costs latency only,
  never correctness (ADR-0003), so a customer whose egress allows only 443
  runs without it.
- **Cells.** A cell is one complete, independent copy of the control plane:
  its own PostgreSQL cluster, NATS cluster and service replicas. A tenant lives
  in exactly one cell. §4 explains why cells, and why not now.
- **Global services** are few and stateless with respect to workflows: signup,
  tenant-to-cell routing, billing export, the status page and the audit
  archive. A global outage prevents signups and slows billing. It does not stop
  running workflows.

---

## 3. Agent identity and connectivity

### 3.1 What exists

| Mechanism | Where | What it proves |
|---|---|---|
| Worker credential: `nxtok_` scoped token with role `worker` and scope `worker_rpc`, or a legacy API key | F114; `services/api-gateway/workergrpc` | The caller belongs to namespace N and may call the worker RPCs. Tokens expire and can be revoked. |
| mTLS client certificate, key generated on the agent host, CSR signed by the CA holder | TLS.md §5.5, `deploy/nexus-certs.sh` | The caller holds a key the CA signed. **It authorizes nothing** (TLS.md §5.6): the certificate's CN is not checked against the tenant. |
| NATS NKey, one identity per tenant, subscribe-only on `nexus.task.dispatch.<ns>.>` | OPERATIONS §11.7 | The caller may receive dispatch hints for its own namespace and publish nothing. |
| Per-session worker identity, lease fencing | ADR-0010 | A stale or duplicated agent cannot complete work it no longer owns. |

### 3.2 What the hosted service requires

1. **One CA per tenant.** Each tenant gets its own intermediate CA, issued by
   an offline vendor root and held in a KMS/HSM. The gateway maps the
   presenting certificate's issuing CA to a tenant and **refuses a worker
   credential whose namespace does not match the certificate's tenant**. This
   closes TLS.md §5.6 for the worker listener. Until then mTLS is transport
   security only, and the bearer token is the whole identity.
2. **Short-lived leaves.** 30 days or less, renewed automatically by the agent
   through an authenticated `RenewCertificate` call. That call does not exist
   yet. Today enrolment is a manual CSR round trip. Revocation is by expiry plus
   a gateway-side deny list of certificate serials. There is still no CRL or
   OCSP (TLS.md §5.6).
3. **Enrolment.** The console mints a one-time enrolment token (15-minute TTL,
   single use, bound to the tenant and a pool). The agent generates its key
   locally, sends the CSR with the token, and receives its leaf plus the tenant
   bundle. The private key never leaves the host.
4. **NATS.** Per-tenant NKey identities as today. With many tenants, the static
   server config (a list of public keys) stops scaling somewhere in the
   thousands of identities. That limit has not been measured. The alternative
   the repo already anticipates is decentralized JWT accounts (`.creds` is
   supported client-side, OPERATIONS §11.7). **OPEN-5.**
5. **Connection limits per tenant.** The gateway's rate limits are per replica
   and in memory (OPERATIONS §16.6). A hosted service needs a shared limiter:
   per-tenant agent connection caps, poll rate and register rate. See §8.

---

## 4. Tenant isolation

### 4.1 What isolation exists today

It is **application-enforced logical isolation on shared infrastructure**
(MULTITENANCY.md):

- `tenant_id` is a non-null, indexed foreign key on all 24 customer-owned
  tables. Composite foreign keys stop cross-tenant links (spec/run,
  worker/attempt, worker/lease and others). Triggers reject mismatched or
  changed ownership (migration 0014).
- Every REST route authorizes the path namespace against the principal (F101).
  Resource IDs do not bypass that check.
- Scheduling is tenant-scoped in both claim directions. Weighted fair share
  (ADR-0017) and namespace quotas (ADR-0021) bound a tenant's share of the
  shared dispatch path.
- **Not present:** PostgreSQL row-level security. Internal service roles are
  trusted and can read every tenant. There are no per-tenant encryption keys,
  and signup abuse controls are incomplete (MULTITENANCY.md, "Deployment").

### 4.2 Options

| | A. Shared control plane, logical isolation (today) | B. Cells: N shared control planes, tenants partitioned | C. Dedicated control plane per tenant |
|---|---|---|---|
| Blast radius of a DB outage or bad migration | every tenant | one cell | one tenant |
| Blast radius of an isolation bug (missing `tenant_id` filter) | every tenant | the tenants in one cell | none across tenants |
| Noisy neighbour on dispatch (ADR-0018: claim cost grows with backlog on the push path; the pull path is windowed) | whole fleet | one cell | none |
| Cost per small tenant | lowest | low (amortized over a cell) | highest: one PostgreSQL HA pair, NATS, services each |
| Operational load | 1 deployment | N identical deployments plus a router | one deployment per customer. Needs full automation. |
| Per-region residency | one region per deployment | cells are placed in regions | wherever the customer asks |
| Upgrade | one rollout, one risk | cell-by-cell canary. The first cell is the canary. | per tenant, schedulable by the customer |
| Fits | beta, free tier | standard paid tiers | enterprise, regulated, and "bring your own cloud account" |

**Proposed:** B for the general service, with C offered as an enterprise tier.
A is B with one cell, so the service can launch with one cell and split without
a redesign, provided the cell router exists from day one. Cells are the unit of
deployment, upgrade, backup and capacity. The cell size (tenants or attempts
per cell) must come from a measurement on production-class hardware using the
ADR-0018 harness, not from the laptop numbers in [BENCHMARKS.md](BENCHMARKS.md).
**OPEN-1.**

Option C is today's Helm chart plus Terraform (`deploy/helm`,
`deploy/terraform`) run by the vendor into a dedicated account. It needs the
F115 cloud gaps closed first: keyless storage credentials, a CA-verified
database connection, and a non-admin runtime database role.

### 4.3 Defence in depth for B

Logical isolation is only as strong as the rarest missed filter. In a shared
cell, add:

1. **PostgreSQL RLS as a backstop, not the primary control.** Add policies on
   `tenant_id` keyed by a per-transaction `SET LOCAL nexus.tenant_id`, applied
   to the gateway's read paths first. Scheduler claims legitimately span
   tenants on the push path, so that role stays outside RLS, and that exception
   is documented as ADR-0011 rules are. This needs its own ADR and a
   measurement of claim latency. It is not free.
2. **A cross-tenant canary.** Two synthetic tenants in every cell try each
   other's resources through every public route on a schedule, and any success
   pages the on-call engineer.
3. **Per-tenant encryption of sensitive columns.** The encrypted columns are
   inline results, parameters, and the sealed secrets in ADR-0028 migration
   0034. They use envelope keys per tenant in the cloud KMS. Enterprise tenants
   can bring their own key (BYOK), which also enables cryptographic erasure on
   offboarding. This is not built.

### 4.4 Threat model (STRIDE, hosted service)

Assets: tenant workflow metadata, inline results and parameters, logs,
credentials (worker tokens, certificates, NKeys, SSO configuration), the usage
ledger (billing truth), and availability.

| # | Threat | Actor | Today | Required for Cloud |
|---|---|---|---|---|
| T1 | **Spoofing:** stolen worker token used from elsewhere | external | Token expiry and revocation (F114); lease fencing limits damage to in-flight work | Bind tokens to the tenant CA certificate (§3.2). Alert on a new source ASN for a pool. |
| T2 | **Spoofing:** agent of tenant A presents a valid certificate and B's token | malicious tenant | Certificate identity is not checked (TLS.md §5.6) | Match the certificate tenant to the credential namespace. **Blocking.** |
| T3 | **Tampering:** worker reports a result for another tenant's attempt | malicious tenant | Leases carry tenant and worker. Composite FKs stop cross-tenant worker/attempt links (0014). | Keep. Add a fuzzing test of the worker RPCs with foreign IDs. |
| T4 | **Information disclosure:** missing tenant filter in a new query | vendor bug | Namespace authorization on every route (F101), and integration tests for cross-tenant denial | RLS backstop (§4.3), cross-tenant canary, cells to bound the blast radius |
| T5 | **Information disclosure:** vendor operator reads tenant data | insider | Internal roles are trusted. The audit log exists; ADR-0025 (on a branch) proposes a hash-chained export. | Just-in-time, ticketed, audited break-glass access. Per-tenant keys for sensitive columns. No standing production database access. |
| T6 | **Denial of service:** one tenant floods runs or dispatch | tenant (malicious or careless) | Quotas enforced in the claim and at run creation (ADR-0021); fair share (ADR-0017); per-replica rate limits | Plan-default quotas that are never unlimited (§7). A shared rate limiter. Per-tenant connection caps. Cells. |
| T7 | **Denial of service:** signup flood creates thousands of free tenants | bot | One registration per email, email verification, per-IP limits in memory per replica (MULTITENANCY.md) | §8 controls: CAPTCHA/proof-of-work, disposable-domain block, verified email before any worker token, card for paid tiers, and global limits. |
| T8 | **Elevation:** tenant admin raises its own quota or fair-share weight | tenant | Weight needs a platform operator (ADR-0017). Quotas can be locked by an operator (ADR-0021). | Every Cloud tenant's quotas are `locked: true`, set by plan. Only billing can raise them. |
| T9 | **Repudiation:** a tenant disputes a bill | tenant | Append-only ledger with UPDATE refused by trigger, one row per dispatched attempt, deduplicated on `attempt_id` (ADR-0021) | Export the ledger to an immutable archive (object lock) per billing period, and give the tenant a self-serve usage export. |
| T10 | **Supply chain:** compromised image or dependency | external | Digest pins, provenance verification for the S3 image (OPERATIONS §22, `make verify-s3-image`) | Sign every Nexus image. Record SBOMs. Admission policy in the cells that only admits signed digests. |
| T11 | **Webhook / SSRF:** notification or webhook targets internal vendor addresses | tenant | ADR-0028 (branch) | Egress through a proxy that denies private and metadata ranges. |

---

## 5. Data residency and regions

### 5.1 Model

- A **region** is a set of cells in one cloud region. A tenant chooses its region
  at signup, and the choice is immutable without a migration (§5.4).
- **Region-pinned data:** everything in the cell's PostgreSQL (specs, runs,
  attempts, events, inline results, parameters, audit, usage ledger), the
  cell's NATS streams, logs and artifacts in the vendor object store, and
  backups (§10).
- **Global data (minimized):** tenant ID, cell and region mapping, the owner's
  email for login routing, billing account ID and billing aggregates. Signup
  collects nothing else. Where the owner's email must also be pinned, **OPEN-3**
  covers region-local login routing by subdomain (`eu.nexus.cloud` style).
- **Customer-pinned data:** task inputs and outputs above the inline threshold,
  and task execution, on the customer's workers and in the customer's bucket.

### 5.2 Initial regions

**OPEN-3.** A minimal proposal: one EU region and one US region, on one cloud
provider, because Terraform modules exist for AKS, EKS and GKE but none has
been applied (F115 item 6).

### 5.3 What leaves the customer's network

This is the honest list for a data-protection review:

| Data | Where it goes | Customer control |
|---|---|---|
| Workflow specs (task names, images, commands, literal inputs) | metadata store | Authors must not put secrets in specs. Secret references resolve on the worker (ADR-0025, branch). |
| Run parameters | metadata store | Same. Per-tenant column encryption (§4.3). |
| Inline results ≤ threshold (default 64 KiB) | metadata store | **Needs a per-tenant threshold.** Today the gateway has one global value (`NEXUS_INLINE_THRESHOLD_BYTES`, ADR-0016). A tenant that wants no result bytes hosted needs threshold = 1 byte, meaning every result goes to its bucket. The [1 B, 1 MiB] range already allows this. Making it per tenant is new work. |
| Task logs | the result store under `<namespace>/logs/…` (ADR-0020), read by the gateway for the console | With BYO bucket, logs stay in the customer's bucket, but the gateway then needs read credentials to it (§5.3.1). |
| Error messages, exit codes, measured metrics | metadata store (`task_attempts`, `usage_ledger`) | Error text can carry data. A redaction hook in the agent is a gap. |

#### 5.3.1 Bring-your-own bucket

`RegisterResponse.result_store` sends endpoint, bucket, region and threshold to
the agent. **Credentials are not sent.** The agent has its own. So a customer
bucket works for writing today. What does not work:

1. The result store configuration is one global value per gateway. Cloud needs it
   per tenant, and ideally per pool.
2. The gateway reads logs and artifact previews with its own credential.
   Proposed: the tenant grants a narrowly scoped, read-only role on
   `<namespace>/logs/*` to a vendor principal (cross-account role, workload
   identity federation). Or the console reads through short-lived presigned URLs
   that **the agent** issues. The second keeps vendor credentials out of the
   customer's account, but it works only while an agent is online. **OPEN-4.**
3. Artifact GC (`internal/artifactgc`) and the orphan sweep need list and delete
   access. With BYO, GC runs as an optional agent-side job in the customer's
   network, or is the customer's responsibility.

### 5.4 Moving a tenant between cells or regions

Moving a tenant means a logical export of that tenant's rows (every table has
`tenant_id`), an import into the target cell, cut-over of the router with the
tenant paused, and agents redialling the new endpoint. The agents'
configuration points at a stable per-tenant hostname, so a move needs no
customer change. There is no tooling for this today, and it is not required at
launch. Cross-region moves are a residency change and need the customer's
written instruction.

---

## 6. Metering and billing hooks

Built entirely on ADR-0021. **No billing is built. This section specifies only
the hook.**

1. **Source of truth:** `usage_ledger`. It has one append-only row per
   dispatched attempt, written in the same transaction as the terminal
   transition. It cannot be double-counted (`UNIQUE(attempt_id)`), and it
   survives run deletion. Quantities: `reserved_seconds`, `cpu_core_seconds`,
   `memory_gib_seconds`, `gpu_seconds`, `wall_seconds`, and measured metrics
   where a worker reported them. AI token units are planned as custom ledger
   keys `tokens:<provider>/<model>:input|output` on the `feat/ai-workload-pack`
   branch.
2. **Transport:** two at-least-once paths, both deduplicated on `attempt_id`
   (OPERATIONS §21.5):
   - **push:** the outbox relays to `nexus.usage.recorded.<namespace>` in stream
     `NEXUS_USAGE` (30 days, file-backed);
   - **pull:** `internal/metering` `Exporter` / `Pump` with a persisted cursor
     and a 30 s commit-lag guard.
3. **Cloud billing pipeline (to build):** a per-cell *usage forwarder*
   (`Pump` + `Exporter`) writes to a regional, append-only billing store keyed
   by `(cell, ledger_id)` and deduplicated on `attempt_id`. A daily job rates
   usage with the effective-dated price books from ADR-0023 (on
   `feat/cost-management`) and pushes **aggregates only** (tenant, period,
   meter, quantity) to the billing provider. Raw ledger rows never leave the
   region.
4. **What is billed. OPEN-2.** The ledger supports three options:
   (a) **orchestration events**: dispatched attempts. This is cheap to explain,
   and it does not charge for compute the vendor does not run. (b) **reserved
   resource-seconds**: this charges for the customer's own compute, which is
   hard to justify when the customer pays its own cloud bill. (c) **a platform
   fee plus attempt tiers.** Since workers are customer-run, (a) or (c) is the
   natural fit. (b) is reported for cost management, not billed.
5. **Reconciliation:** `nexus_scheduler_usage_ledger_writes_total` should track
   `nexus_scheduler_attempts_dispatched_total` (OPERATIONS §21.7). A billing
   period closes only when the forwarder's cursor has passed every ledger row
   with `ended_at` inside it, plus the lag guard. Ledger export files are kept
   under object lock for the invoice-dispute window.
6. **Known limits carried from ADR-0021:** usage is attributed to the UTC day
   an attempt **ended**, which matters for month-boundary invoices. Storage is
   soft and only log bytes are measured; artifacts are not metered. LOST and
   cancelled attempts have no measured metrics.

---

## 7. Plans and usage limits

Built on ADR-0021 quotas. **OPEN-2** sets the numbers. The dimensions are
fixed by what is enforced today:

| Dimension (`platform_config.quotas`) | Enforced at | Free | Team | Enterprise |
|---|---|---|---|---|
| `max_inflight_attempts` | claim (exact under concurrency) | small | medium | contract |
| `max_active_runs` | run creation, 429 | small | medium | contract |
| `max_runs_per_hour` | run creation, 429 with `Retry-After` | small | medium | contract |
| `max_cpu_millis` / `max_memory_bytes` / `max_gpu_count` / `max_custom` | claim | unset* | unset* | contract |
| `storage_soft_limit_bytes` | reported only | yes | yes | contract |
| workers, users, retention days, SSO, audit export | **not enforced by quotas today** | | | |

\* Workers are customer-owned, so capping the customer's own CPU is a
cost-management feature, not a plan limit.

Rules for the hosted service:

- **Never unlimited.** Every Cloud namespace is created with plan quotas and
  `locked: true`. ADR-0021's "absent means unlimited" default is right for
  self-hosting and wrong for a shared cell (T6, T8).
- **Plan changes are writes by the platform operator's billing service**
  through the existing `PUT /v1/namespaces/{ns}/quotas` with `quotas.manage`
  and a platform-operator principal. That call is audited as `quota.update`.
- **Downgrade** tightens quotas. In-flight work keeps running, and new claims
  obey the new limit (quota-blocked attempts stay PENDING with a reason and
  spend no retry budget).
- **Missing limits:** number of workers, users, schedules and specs, retention,
  and log bytes per day. Each needs an enforcement point before a plan can
  promise it.
- **Cost of quotas:** a quota-bound namespace's dispatches are serialized across
  scheduler replicas. The laptop measurement is in [BENCHMARKS.md](BENCHMARKS.md#quota-bound-cells).
  That caps one namespace's dispatch rate. It is acceptable for Free and Team,
  and it is a sizing input for Enterprise-on-shared-cell.

---

## 8. Signup-abuse controls

Today: one self-service registration per normalized email, email verification
required before sign-in, per-IP and per-address limits held in memory per
replica, and an hourly token-issuance cap held in the database
(MULTITENANCY.md). None of these is enough for a public free tier.

Required before public signup:

1. **Global, shared rate limits** (not per replica) on registration, login,
   verification-mail sends and password resets, keyed by IP, /24 or /48, ASN and
   email domain. Held in a shared store or at the edge (OPERATIONS §16.6
   already says this is not provided).
2. **Bot friction** on the registration form: a privacy-preserving CAPTCHA or
   proof-of-work, applied adaptively.
3. **Disposable-domain and known-abuse lists** for email, updated regularly.
4. **Staged capability.** An unverified account can do nothing (already true).
   A verified Free tenant gets small, locked quotas. Minting a worker token or
   enrolling an agent certificate needs a verified email. Features that send
   outbound traffic (notifications, webhooks, ADR-0028) need either a payment
   method or tenant age above N days.
5. **Card or identity check** for paid tiers, done by the payment provider.
   No card data touches Nexus.
6. **Expire abandoned registrations.** Unverified accounts older than N days
   are deleted, which releases the email (a known limit in MULTITENANCY.md).
7. **Abuse signals and actions:** runs per hour near the cap for days, agent
   registrations from many ASNs, and outbound webhook failure rates. Actions
   are suspension (a tenant `locked` state, which needs the "customer
   suspension" lifecycle work listed as missing in MULTITENANCY.md), quota
   tightening and review.

---

## 9. SLOs and error budgets

**These are proposed targets, not measured results and not promises.** None has
been observed on a hosted deployment, because none exists. They are chosen so
that every SLI can be computed from metrics the services already export
(OPERATIONS §17.4).

| SLO | SLI (source) | Proposed target (28-day window) |
|---|---|---|
| API availability | `nexus_http_requests_total` at the edge: non-5xx ÷ all, excluding 429 | 99.9 % (Team), 99.95 % (Enterprise, per-tenant cell) |
| Worker plane availability | successful worker RPCs ÷ all (register, heartbeat, poll, report) | 99.9 % |
| Dispatch latency | `nexus_scheduler_claim_duration_seconds` p99 per cell | < 250 ms. This is ADR-0018's trigger for a dedicated dispatch path, so exhausting this budget is itself the signal to build it. |
| Queue freshness | `nexus_scheduler_oldest_pending_age_seconds` for attempts **not** quota- or capacity-blocked | p99 < 30 s. This needs a label that excludes blocked attempts, which is a gap. |
| Durability of acknowledged state | acknowledged run and attempt transitions lost in a restore | RPO ≤ 5 min (§10). The objective is 0 loss outside a disaster. |
| Usage metering completeness | ledger rows ÷ dispatched attempts that ended | 100 % per billing period. This is a correctness invariant, not a budget. |

**Error budget policy (proposed).** When a cell has spent more than 50 % of its
budget, feature rollouts to that cell pause. When it has spent 100 %, only
reliability changes ship to it until the 28-day window recovers. Budgets are
per cell, so a bad cell does not freeze the fleet. Tenant-attributable errors
are excluded from the SLI but tracked: 429s from quota, 4xx from invalid specs,
and failures of the customer's own workers.

**What the vendor cannot promise:** task success, task latency and worker
availability. Those depend on customer-run workers. The SLA must say so.

---

## 10. Backup and disaster recovery

Built on what has been drilled (OPERATIONS §2–§7; results in §4.6 and
`docs/drills/`).

| Layer | Mechanism | Drilled? |
|---|---|---|
| Cell PostgreSQL | Managed PostgreSQL with provider PITR (OPERATIONS §4: pgBackRest cannot run against a managed server), or self-managed with pgBackRest 2.59.1 continuous archiving | pgBackRest PITR drilled on a laptop: RTO 20.6–32.3 s and RPO 33.8–44.7 s for a primary-loss scenario, on 127–914 MiB databases (OPERATIONS §4.6). Managed-provider PITR is **not** drilled. |
| Logical safety net | `backup-all.sh` (pg_dump + result-store mirror) daily per cell, to another region | Drilled on a laptop (OPERATIONS §4.6, logical path: RTO 39–45 s) |
| Vendor object store (logs, artifacts for non-BYO tenants) | Provider versioning + cross-region replication + object lock on billing exports | Not drilled on a cloud provider (F115 item 6) |
| NATS JetStream | Not backed up. It is a hint bus; PostgreSQL is authoritative, and the outbox replays (ADR-0003). `NEXUS_USAGE` is re-derivable from the ledger. | By design |
| Global services | Tenant-to-cell mapping replicated across regions. Rebuildable from the cells' `tenants` tables. | Not built |

**Targets (proposed).** Cell RPO ≤ 5 min and RTO ≤ 1 h for loss of a zone or
database. Region loss: RPO ≤ 15 min and RTO ≤ 4 h into a paired region, only
for tenants whose residency permits the paired region. Otherwise the region is
restored in place. The laptop drills show database restore is not the long
pole at those sizes. Artifact bytes and service start-up are (OPERATIONS §4.6).
A production-size database is "a different regime entirely" (same section).
These targets must be validated by a drill in the real environment before they
appear in any contract.

**Correctness after restore.** A restore re-creates attempts that already ran,
so tasks will re-execute (OPERATIONS §4.7, ADR-0006). A Cloud restore must
notify affected tenants with the restore point, because their non-idempotent
handlers will duplicate side effects.

**Drills.** Quarterly per cell: a PITR restore into a scratch cell, with the
existing `pitr-drill.sh` assertions (exact row counts to the target, every
`output_ref` resolves, and a gap-free prefix for RPO) adapted to the managed
provider. Annually: a region failover game day.

---

## 11. Operational runbooks (outline)

Each becomes a page under `docs/runbooks/cloud/` when the service is built. The
existing OPERATIONS sections they build on are named.

1. **Cell provisioning and decommissioning.** Terraform plus Helm (§20), the
   migrate job, NATS authorization render (§11.7), certificate authority per
   tenant, then synthetic tenants and the canary.
2. **Tenant lifecycle.** Signup, verification, plan assignment (locked quotas),
   suspension, reinstatement, export, and deletion with cryptographic erasure
   (§4.3). The last three are not built.
3. **Rolling upgrade across cells.** Canary cell first, then expand-contract
   migrations (§6, COMPATIBILITY.md §6), then N-1 agent skew
   (COMPATIBILITY.md §4). Error budget gates each step.
4. **Dispatch degradation.** Claim p99 above 250 ms, backlog above 50k
   (ADR-0018, OPERATIONS §15). Identify the tenant with `pending_attempts` by
   queue, tighten quotas, then move the tenant to its own cell.
5. **Database incidents.** Failover, PITR (§4), connection-pool exhaustion
   (`nexus_db_pool_*`), long transactions, and bloat on `task_attempts`.
6. **Event bus outage.** Expected behaviour (§11.4), relay backlog, and
   credential rotation (§11.8).
7. **Certificate and credential compromise.** Tenant CA rotation (TLS.md §5.3),
   serial deny list, NKey rotation (§11.8), and mass revocation of worker
   tokens.
8. **Abuse response.** Signup flood, crypto-mining signals from outbound
   webhooks, and suspension (§8).
9. **Billing close.** Forwarder cursor check, reconciliation (§6.5), dispute
   handling from the object-locked export.
10. **Break-glass access.** Request, approval, time limit, session recording and
    audit review (T5).
11. **Restore drill** and **region failover** (§10).
12. **Customer-facing incident communication.** Status page, per-tenant
    notification, and post-incident report.

---

## 12. Open core and enterprise tiers

### 12.1 What the licence says today

Every file in the repository is Apache-2.0 (LICENSE, NOTICE, SPDX headers on
source files). Contributions are accepted under Apache-2.0 section 5, inbound
equals outbound, **with no CLA** (README "Contributing"). Dependencies are
permissive (THIRD-PARTY-LICENSES.md). The one AGPL component is the Silo S3
server image used for development and tests. It is a separate program
reached over the network, not linked.

Consequences that cannot be designed away:

- **Released Apache-2.0 code stays Apache-2.0 for anyone who has it.** Moving a
  feature that is already in the public tree into a proprietary tier does not
  take it back. Anyone may fork the last Apache version and maintain it.
  Features on unmerged branches (SSO/RBAC ADR-0024, cost management ADR-0023,
  secrets/audit/air-gap ADR-0025, GPU and multi-cluster ADR-0029–0031) are
  still undecided. **Their licence must be decided before they merge to a
  public branch** (**OPEN-6**).
- **The vendor can ship proprietary code built on Apache-2.0 code**, including
  third-party contributions, because Apache-2.0 permits it (section 4, keeping
  notices). A CLA is **not** needed to build a proprietary tier on top. A CLA
  or copyright assignment **is** needed if the vendor ever wants to relicense
  the core itself, for example to a source-available licence. Without one,
  every outside contributor must agree. **OPEN-7.**
- **Trademark** is separate from copyright. "Nexus" / "KGF Nexus" branding for the
  hosted service needs a trademark policy regardless of the code licence.
  (Whether the name is registrable is also open.)
- **AGPL Silo in Cloud:** the service must not ship a modified Silo, and should
  use the provider's object store in production anyway. Legal review is
  needed before Silo appears in any vendor-operated production path.

### 12.2 Proposed split

The principle: **anything a single team needs to run Nexus correctly and safely
on its own infrastructure stays Apache-2.0. What a large organization needs to
govern many teams, and what only the vendor can provide, is enterprise.**
Correctness and security fixes are never enterprise-only.

| Stays Apache-2.0 (core) | Enterprise (proposed) | Cloud-only (operational, not shipped) |
|---|---|---|
| Control plane, agent, CLI, SDK, console | SSO: OIDC/SAML, IdP group mapping, SCIM (ADR-0024 branch) **OPEN-6** | Cell router, signup, abuse controls |
| Scheduling semantics, fair share, quotas, usage ledger + export hook | Cost management: price books, rating, budgets (ADR-0023 branch) | Billing forwarder and billing integration |
| Namespace RBAC (ADR-0012), API keys, scoped tokens, mTLS | Multi-cluster placement (ADR-0031), and federation of several control planes | Per-tenant CA issuance and enrolment service |
| Durable logs, metrics, OpenTelemetry | Audit export sinks with hash chain, legal hold and retention policies (ADR-0025 branch) | Status page, SLO reporting |
| Backup, PITR, restore drills, Helm, Terraform | Air-gapped bundle and long-term-support releases | Break-glass tooling |
| Executors (subprocess, docker, kubernetes), GPU accounting | BYOK / per-tenant encryption (§4.3) | |
| Secret *references* resolved on the worker (env, file) | Enterprise secret backends (Vault, Key Vault) **OPEN-6** | |
| | Support SLAs (a contract, not code) | |

A reasonable alternative is to keep **SSO in core**: many buyers treat it as
baseline security ("SSO tax" criticism). The trade-off is revenue against
adoption. **OPEN-6.**

### 12.3 How the repository would separate them

Today the tree is one Go module (`go.mod` at the root) with `services/`,
`internal/`, `agent/`, `cli/`, `sdk/`, `ui/`, `deploy/`. The proposed
separation is designed so that **the core builds, tests and runs with no
enterprise code present**:

1. **Interfaces in core, implementations out.** Each enterprise feature enters
   through an interface that already has, or gets, an Apache-2.0 default. Examples:
   `IdentityProvider` (default: email/password + tokens), `CostRater` (default:
   none), `AuditSink` (default: database table), `PlacementScorer`
   (default: none), `SecretResolver` (default: env/file). Core must not
   import `ee/`.
2. **`ee/` directory with its own LICENSE** (proprietary or source-available,
   **OPEN-7**) and its own `go.mod`, which `require`s the core module. The
   enterprise binaries (`ee/cmd/api-gateway` etc.) are thin `main` packages
   that register enterprise implementations and call the same `Run` function
   the core `main` uses. A second module makes the licence boundary visible to
   tooling. A build tag (`//go:build enterprise`) is the lighter alternative,
   but it mixes licences in one module and is easy to import by accident.
   **Recommendation: a separate module.**
3. **Migrations.** Enterprise tables go in a separate migration stream
   (`ee/migrations`, its own `schema_migrations` namespace) applied after core.
   Core migrations never reference enterprise tables. This keeps ADR-0011
   rule 4 and COMPATIBILITY.md §6 intact for core users.
4. **UI.** Enterprise console pages are a separate package loaded behind a
   capability flag the gateway advertises. The core console shows no
   disabled upsell controls. The production brief forbids unwired controls.
5. **Separate repository or directory?** A private repository for `ee/` keeps
   the public repository pure Apache-2.0 and simple for contributors. A public
   source-available `ee/` directory lets customers audit what they run. **OPEN-7.**
6. **CI.** The public pipeline builds and tests core alone. A private pipeline
   builds core + ee and runs both suites, so a core change that breaks an ee
   implementation is caught before release.

Nothing in this section has been implemented. The branches named in §12.2 were
built directly in the core tree, so if the owner makes any of them enterprise,
moving them behind interfaces is real refactoring work, and it should happen
before they merge.

---

## 13. Compliance scope

**No certification is held, and none is claimed.** The in-scope boundary for a
first audit would be the vendor-operated control plane, global services and
their supporting processes, not customer workers.

| Framework | Relevance | Readiness from what exists | Largest gaps |
|---|---|---|---|
| SOC 2 Type II (Security, Availability, Confidentiality) | Expected by most B2B buyers | Audit log, RBAC, TLS/mTLS, backup and restore drills, change control through PRs, pinned dependencies | Formal policies, access reviews, vendor management, incident-response evidence, a 3–12 month observation window, break-glass tooling |
| ISO/IEC 27001 | Common in EU procurement | Same technical base | ISMS, risk register, Statement of Applicability |
| GDPR (processor role) | EU tenants; the control plane holds user emails and whatever customers put in specs, parameters and logs | Region pinning (§5), data minimization in global services, deletion is possible per tenant | DPA and sub-processor list, deletion and export tooling, records of processing |
| HIPAA | Only if a customer puts PHI in hosted data | BYO bucket + inline threshold of 1 byte keeps results out of the hosted plane (§5.3), but logs, parameters and errors may still carry PHI | BAA, and per-tenant encryption. Recommended: out of scope for the shared tier and offered only on dedicated control planes. |
| PCI DSS | Card data | None needed: the payment provider handles cards (§8) | Keep card data entirely out of scope |
| FedRAMP / IL | Government | Not planned | |

**OPEN-8:** which framework comes first, and for which tier.

---

## 14. Gaps between today's code and this design

In rough dependency order. None is started unless a branch is named.

1. Certificate-to-tenant binding on the worker listener (T2, TLS.md §5.6). **Blocking** for shared cells.
2. Shared rate limiter plus signup abuse controls (§8, OPERATIONS §16.6, MULTITENANCY.md).
3. Plan-default locked quotas at tenant creation. Limits for workers, users, schedules and retention (§7).
4. Per-tenant result store configuration and inline threshold (§5.3). BYO-bucket read path for logs (OPEN-4).
5. Cell router and stable per-tenant endpoints (§2, §5.4).
6. Tenant suspension, export and deletion lifecycle (MULTITENANCY.md "separate lifecycle work").
7. Usage forwarder, billing store and rating (§6). Rating is proposed on `feat/cost-management`.
8. RLS backstop and per-tenant column encryption (§4.3). Each needs an ADR.
9. F115: keyless storage credentials, CA-verified database TLS, non-admin runtime role, and a first real cloud apply.
10. Certificate renewal RPC and enrolment tokens (§3.2).
11. Queue-freshness SLI that excludes blocked attempts (§9).
12. Image signing and admission policy (T10).
13. Production-class measurement of cell capacity with the ADR-0018 harness (§4.2).

---

## 15. Open decisions for the owner

| # | Decision | Proposed default | Blocks |
|---|---|---|---|
| OPEN-1 | Isolation model per tier, and cell size | Cells (B) for Free/Team; dedicated control plane (C) for Enterprise; size from a production-class benchmark | Everything in §2 and §4 |
| OPEN-2 | What is billed, and the plan limits | Platform fee + dispatched-attempt tiers; reserved resource-seconds reported, not billed | §6, §7 |
| OPEN-3 | First regions and cloud provider; whether login routing is region-local | One EU and one US region on one provider; region-local subdomains | §5 |
| OPEN-4 | BYO-bucket log access: vendor cross-account read role, or agent-issued presigned URLs | Presigned URLs from the agent, with vendor-hosted logs as the default for tenants without BYO | §5.3.1 |
| OPEN-5 | NATS identity at scale: static NKeys or decentralized JWT accounts | JWT accounts per cell once static tenants exceed a measured limit | §3.2 |
| OPEN-6 | Which features are enterprise, especially SSO and secret backends, **decided before those branches merge** | Table in §12.2, with SSO an explicit choice | §12 |
| OPEN-7 | Enterprise licence (proprietary, BSL, ELv2), repository layout (private repo vs public `ee/`), and whether to introduce a CLA | Separate `ee/` Go module in a private repo, proprietary licence; a CLA only if relicensing the core is ever wanted | §12.1, §12.3 |
| OPEN-8 | First compliance framework and target date | SOC 2 Type II for the shared tier | §13 |
| OPEN-9 | Whether managed (vendor-run) workers are ever in scope | No. Revisit only with a sandboxing design. | §1.1 |
| OPEN-10 | SLO targets and the SLA's exclusions | §9 table as the starting proposal | §9 |
