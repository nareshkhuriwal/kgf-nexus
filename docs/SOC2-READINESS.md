# Nexus — SOC 2 readiness self-assessment

**Status:** readiness self-assessment, 2026-09-25, against the repository at
branch `feat/secrets-audit-airgap` (ADR-0025 in flight; see below).
**Audience:** an organization that operates a Nexus deployment and is preparing
for a SOC 2 examination, and the platform team deciding what to build next.
**Framework:** AICPA *2017 Trust Services Criteria for Security, Availability,
Processing Integrity, Confidentiality, and Privacy* (with revised points of
focus, 2022).

> **Read this first.**
>
> - This is a **readiness self-assessment, not an attestation.** No auditor has
>   examined anything in it. It has not been reviewed by a CPA firm.
> - **Nexus is NOT SOC 2 compliant, certified, or attested,** and nothing here
>   should be quoted as saying otherwise. `docs/README.md` states that the
>   project has not passed its own production acceptance gates.
> - **SOC 2 attests an organization's controls**, not a product. The subject of
>   a SOC 2 report is the service organization that *operates* a system — for
>   Nexus, whoever runs the deployment (the "operator"). A product can only
>   *support* controls: provide mechanisms, configuration, logs and evidence
>   that the operator's controls rely on. Most CC1–CC5 criteria are about
>   people, governance and process and cannot be met by software at all.
> - Every "Nexus capability" below is grounded in a file in this repository.
>   Where a document describes a design that the code does not implement, this
>   assessment says so and follows the code (`docs/README.md`: "When a document
>   here disagrees with the code, treat the document as the bug").

**ADR-0025 caveat.** Worker-side secret resolution, the paginated/exportable,
hash-chained audit trail and the air-gapped install bundle are specified in
[ADR-0025](adr/0025-secret-resolution-audit-export-airgap.md) (status
*Proposed — pending owner sign-off*) and are being implemented on sibling
branches for merge into this one; their runbooks are docs/OPERATIONS.md §25 and
`docs/AIRGAP.md`. Rows that depend on them say "(ADR-0025)". Until that merge
lands and its tests pass, treat those rows as one status lower. Nothing here
claims more than the ADR text.

---

## 1. Status legend

| Status | Meaning |
|---|---|
| **Met-by-product** | Nexus provides a mechanism that, correctly configured, satisfies the product-side part of the criterion, and there is repository evidence (test, drill record, config) for it. The operator still has to configure it, run it, and keep the evidence. |
| **Partial** | A mechanism exists but has a documented limitation, is off by default, is unverified in a production-like environment, or covers only part of the criterion. The gap is named in the row. |
| **Gap** | The product has nothing for the product-side part of the criterion, or what exists contradicts it. Listed in §17. |
| **Customer-responsibility** | An organizational control outside the product (governance, HR, policy, physical security, vendor contracts). Nexus may supply inputs; the operator owns the control. |

Abbreviations: "OPERATIONS §N" = [docs/OPERATIONS.md](OPERATIONS.md) section N; **CF** =
[CONTRACT-FINDINGS.md](../CONTRACT-FINDINGS.md) (findings `F<n>`, with their
FIXED/OPEN status); **ARCH** = [docs/ARCHITECTURE.md](ARCHITECTURE.md).
"Operator" = the organization running Nexus.

---

## 2. Shared responsibility

| Layer | Who | What they own for SOC 2 purposes |
|---|---|---|
| Nexus product (code, chart, compose, Terraform modules, docs) | Khuriwal Group platform team | The mechanisms in this document; secure defaults; honest documentation of limits (CF); fixing product gaps (§17). Also a **vendor** to the operator (CC9.2). |
| Deployment and operation | Operator | Choosing and enforcing configuration (TLS, rate limits, DB roles, retention, alerting); backups and drills in *their* environment; user administration and access reviews; change approval; incident response; evidence retention. |
| Infrastructure | Operator and its cloud provider | Physical security, host hardening, disk/database/object-store encryption at rest, network edge (WAF/DDoS), DR regions, managed-database PITR. The Terraform modules (`deploy/terraform/`) set some of these; the provider's own SOC report covers the rest. |
| Workflow content | Operator's workflow authors | What data flows through tasks (including personal data), idempotency of handlers (ARCH §6), which secrets are referenced, container images run by tasks. |
| Organization | Operator management | CC1–CC5 governance, HR, risk assessment, policies, vendor management, privacy notices. |

Items that are always the operator's regardless of product changes: CC1.x,
CC2.3, CC3.x, CC5.1, CC6.4, CC7.3, CC9.x (program), P1–P3, P6–P8.

---

## 3. CC1 — Control environment

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC1.1 Integrity and ethical values | None (organizational). | — | Operator's code of conduct, acknowledgements. | Customer-responsibility |
| CC1.2 Board oversight / independence | None (organizational). | — | Board/committee minutes. | Customer-responsibility |
| CC1.3 Structures, reporting lines, authorities | Supports separation of authority inside the tool: built-in roles Administrator, Viewer, Workflow Operator, Workflow Developer, Infrastructure Operator, Auditor, plus custom roles; an administrator cannot change their own bindings or delegate grants they do not hold. | `docs/RBAC-IMPLEMENTATION.md` (Administration); `docs/RBAC-DESIGN.md` (Built-in roles; Escalation and lockout safeguards) | Role matrix export from `/admin/roles`; PostgreSQL integration tests listed in RBAC-IMPLEMENTATION (Validation). | Customer-responsibility |
| CC1.4 Competence, hiring, training | None. HR onboarding, background checks and security training are outside the product. | — | HR records, training completion. | Customer-responsibility |
| CC1.5 Accountability | Every administrative action is attributable: `audit_log` records actor id/kind/label, action, target, outcome (`success`/`denied`/`error`) and client IP. | `services/migrations/0007_identity.up.sql` (audit_log); OPERATIONS §25 (ADR-0025) | Audit export (Appendix A.1). | Customer-responsibility |

## 4. CC2 — Communication and information

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC2.1 Relevant, quality information for internal control | Prometheus metrics from every process with cardinality rules; structured JSON logs (`slog.NewJSONHandler` in every service and the agent); append-only `run_events`; `audit_log`; durable task logs. | OPERATIONS §17 (§17.3 cardinality, §17.4 catalogue); OPERATIONS §19; [ADR-0019](adr/0019-metrics-exposition.md); [ADR-0020](adr/0020-durable-task-logs.md) | `/metrics` scrape; audit export; log retrieval via `GET /v1/namespaces/{ns}/attempts/{id}/logs`. | Partial — tracing is described in ARCH §9 (OpenTelemetry) but no OpenTelemetry dependency exists in `go.mod`; no correlated trace evidence. |
| CC2.2 Internal communication of objectives and responsibilities | A complete system description input: architecture, ADRs with rejected alternatives, runbooks, shared-responsibility statements, a findings log that records defects honestly. | ARCH; `docs/adr/`; docs/OPERATIONS.md; CF | These documents, versioned in git. | Partial — documentation exists; communicating it to staff is the operator's. |
| CC2.3 External communication (customers, users, regulators) | Console shows honest states for email delivery (`queued`/`mail_disabled`/`not_queued`). No `SECURITY.md`, vulnerability-disclosure policy or security contact exists in the repository. | `docs/MULTITENANCY.md` (Registration) | — | Customer-responsibility (and a product Gap for the vendor: no disclosure policy, §17 #16) |

## 5. CC3 — Risk assessment

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC3.1 Objectives specified | Product states its own availability/recovery facts (measured RTO/RPO, what is not covered) that the operator can use when setting objectives. | OPERATIONS §4, §5; `docs/drills/` | Drill JSON reports. | Customer-responsibility |
| CC3.2 Risk identification and analysis | Inputs only: ADR "Consequences" sections, ARCH §10 failure matrix, CF open findings with severity (F109 partly open, F115, F119, F125 open). | ARCH §10; CF | Findings list with status. | Customer-responsibility |
| CC3.3 Fraud risk | None. | — | — | Customer-responsibility |
| CC3.4 Changes that affect internal control | ADRs document architectural changes; COMPATIBILITY policy classifies breaking changes. | `docs/adr/README.md`; `docs/COMPATIBILITY.md` §2–3 | ADR history in git. | Customer-responsibility |

## 6. CC4 — Monitoring activities

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC4.1 Ongoing and separate evaluations | Automated evaluations run on every change: build/vet/race tests, Postgres+S3 integration (skips fail the job, F85), NATS integration, examples + chaos suite, browser e2e, SDK matrix, Helm lint + digest check, Terraform validate/lint, previous-release skew test, doc-reference check. Separate evaluations: restore and PITR drills with JSON reports; `pitr-verify.sh` as a probe. | `.github/workflows/ci.yml`; `.azure-pipelines/pr-build-validation.yml`; OPERATIONS §4.5–4.6; `docs/COMPATIBILITY.md` §7 | CI run history and uploaded artifacts (retention 14 days in `ci.yml`); `docs/drills/2026-09-25-*.json`. | Partial — drills were run on a laptop (OPERATIONS §4.6 "LAPTOP measurements, not production claims"); CI artifact retention (14 days) is shorter than a typical audit period, so the operator must archive results. |
| CC4.2 Deficiencies evaluated and communicated | CF is a running deficiency log: each finding has severity, root cause, fix, verification and FIXED/OPEN status. | CF | CF git history. | Partial — no ticketing/SLA for remediation; ownership and timelines are the operator's (or vendor's) process. |

## 7. CC5 — Control activities

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC5.1 Control activities selected to mitigate risk | None (organizational selection). | — | Operator's control matrix. | Customer-responsibility |
| CC5.2 General controls over technology | Additive-migration gate (`internal/migrations`) with reviewed allow-list; migrations applied by a dedicated job only; explicit DSN required (no fallback, F103); digest-pinned third-party images; Helm values schema refuses unsafe values (e.g. `latest` tag, unauthenticated NATS). | OPERATIONS §6.2, §8, §20.8, §22 | `go test ./internal/migrations/`; `deploy/helm/validate.sh` output; `make check-doc-refs`. | Partial — secure configuration is not the default everywhere (TLS opt-in, `postgres.sslmode: disable` in `values.yaml`, rate limiting off); see CC6.6/CC6.7. |
| CC5.3 Policies and procedures deployed | Runbooks exist for backup/restore, PITR, migrations/upgrade, TLS rotation, NATS credential rotation, cluster controller, quotas. No security policies (acceptable use, access control policy, IR policy) ship with the product. | OPERATIONS §2–§22 | The runbooks. | Partial |

## 8. CC6 — Logical and physical access controls

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| **CC6.1** Logical access security software, infrastructure and architectures | **Authentication:** email + password sessions (argon2id, 12-character minimum, 12 h session TTL `authn.SessionTTL`), sha256-hashed API keys, expiring scoped tokens that intersect (never extend) role grants; sessions refused on the worker protocol. **Authorization:** namespace-scoped module RBAC checked on every request; unknown permissions/routes deny. **Tenancy:** one tenant = one namespace, `tenant_id` FKs and composite FKs on 24 tables (migration 0014). **Service identity:** mTLS between services (opt-in); NATS NKey identities with per-tenant subscribe-only agents. **Database:** per-service Postgres roles with write grants (migration 0005, F70). **Secrets (ADR-0025):** secret values resolved on the worker per tenant, never stored in the metadata store. | ARCH §3.2 (API Gateway); `docs/RBAC-IMPLEMENTATION.md`; `docs/MULTITENANCY.md`; `docs/TLS.md` §2, §4; OPERATIONS §11.7; CF F70, F101; OPERATIONS §25 (ADR-0025) | `services/api-gateway/authn/*_test.go` (incl. `ns_cross_test.go`); tenancy integration test (MULTITENANCY.md, Isolation); CF F101 reproduction; `\dp` output of grants (A.6). | Partial — **no MFA and no SSO** (OIDC/SAML "planned but not implemented", `docs/README.md`); **per-service DB roles are created but not used by compose or Helm** (both connect every service as one `nexus` user, so the grant matrix is not enforced in shipped deployments; OPERATIONS §5 notes the single role); no PostgreSQL RLS or tenant-specific keys (MULTITENANCY.md). |
| **CC6.2** User registration, authorization and deprovisioning | Admin-created users with an atomic account + role assignment; self-registration creates a tenant owner; email verification required by default; disabling a user revokes all its sessions in the same transaction; password reset revokes sessions; last active admin cannot be disabled; every step audited (`user.create`, `user.disable`, `user.roles.assign`, `user.sessions_revoked`, `token.create`, `token.revoke`, …). | `docs/RBAC-IMPLEMENTATION.md`; `docs/MULTITENANCY.md` (Email verification and password recovery) | Audit export filtered by `action=user.*`/`token.*`; `internal/store` identity integration tests. | Partial — no SCIM or IdP-driven deprovisioning; scoped tokens and API keys created by a user are **not** revoked when that user is disabled or resets a password (MULTITENANCY.md, Reset); nothing expires unverified registrations. |
| **CC6.3** Role-based access, least privilege, segregation of duties | Six built-in roles and custom roles; delegation bounded by the grantor's own authority; role changes take effect on the next request (read from PostgreSQL per request); raw event payloads redacted without `tasks.results.view`, logs require `tasks.logs.view`; worker agents bound to one namespace; cluster controller RBAC verified with `kubectl auth can-i`. | `docs/RBAC-IMPLEMENTATION.md`; `docs/RBAC-DESIGN.md`; OPERATIONS §20.8 (Controller RBAC row) | RBAC integration tests; `kubectl auth can-i` transcript (OPERATIONS §20.8). | Partial — machine tokens keep coarse role/scope intersection (no service-account bindings or per-token ceilings); several mutations share `manage`/`update`/`configure` grants; **no access-review report** (effective permissions per principal) and the users API has no cursor pagination (UI shows up to 500 accounts). Periodic access review itself is the operator's. |
| **CC6.4** Physical access | None. Data centers and hosts are the operator's or its cloud provider's. | — | Provider SOC 2 report. | Customer-responsibility |
| **CC6.5** Disposal / discontinued protection of assets and data | Artifact GC reclaims orphaned objects (report by default; delete needs two explicit flags); opt-in time-based **log** retention (`artifact-gc -log-retention`); usage ledger exported then deleted manually; audit retention with legal hold and chain checkpoints (ADR-0025). | OPERATIONS §7.7, §19.2, §21.6, §25 | `artifact-gc` summaries (Pushgateway metrics `nexus_artifact_gc_*`). | Partial — **run deletion is a soft delete**: `DELETE /v1/namespaces/{ns}/runs/{id}` inserts into `run_deletions` and "preserv[es] execution and deduplication records" (`services/api-gateway/rest/delete_run.go`); there is no purge of runs, attempts, `run_events`, parameters or referenced outputs. The retention/partition-drop design in ARCH §9 is not implemented. Media sanitization is the provider's. |
| **CC6.6** Boundary protection against threats from outside | Gateway is the only ingress; three-bucket rate limiter (per address, auth failures, per credential) that cannot be bypassed with random headers (F99); `/metrics` never on the public port; agent metrics bind loopback by default; NATS has no anonymous user; Terraform: no public database endpoints, API-server allow-lists refusing `0.0.0.0/0`, NetworkPolicy-enforcing CNIs; Kubernetes task pods get a default-deny NetworkPolicy; docker tasks default to `--network none`. | OPERATIONS §16, §17.1, §11.7, §13; `deploy/terraform/README.md` (Security defaults); `deploy/k8s/executor-policies.yaml` | `make tf-lint` (checkov/trivy) output; 429 counts `nexus_http_requests_total{code="429"}`; `helm template` showing NetworkPolicy. | Partial — rate limiting is **off unless `NEXUS_RATE_LIMIT_RPS` is set**, per-replica and in memory, "not a DDoS defence" (OPERATIONS §16.6); the chart ships a NetworkPolicy for NATS only, not for control-plane services; WAF/edge protection is the operator's. |
| **CC6.7** Restriction of transmission, movement and removal of data | TLS/mTLS on every gRPC hop with hot reload and `NEXUS_REQUIRE_TLS` to make plaintext fatal; NATS TLS; Terraform-managed databases require TLS; email tokens in URL fragments (not logged by proxies); secret values never in specs, WAL, results, events or agent logs, redacted from shipped task logs (ADR-0025); air-gapped install bundle with `SHA256SUMS` and optional cosign signature (ADR-0025). | `docs/TLS.md` §2, §4.4; OPERATIONS §9, §11.9, §25; `docs/AIRGAP.md`; `deploy/terraform/README.md` | `openssl s_client` serial check (OPERATIONS §9.1); `nexus_tls_*` metrics; startup log lines. | Partial — TLS is **opt-in** and plaintext by default in `make up`; REST ingress TLS is terminated by the operator's load balancer; Helm default `postgres.sslmode: disable` (production checklist OPERATIONS §20.9 says `verify-full`); no certificate revocation (TLS.md §5.6); log redaction is exact-match per line (ADR-0025 Negative consequences). |
| **CC6.8** Prevent/detect unauthorized or malicious software | Third-party images pinned by digest; S3 image provenance verified in CI (`make verify-s3-image`, GitHub/Sigstore attestation); release images pushed and deployed by digest with a CI check that every rendered image resolves to its digest (F112); Kubernetes task pods non-root, no privilege escalation, all capabilities dropped; control-plane pods with read-only root filesystems; worker quarantine for poison workers. | OPERATIONS §22, §22.1, §13, §14.3; CF F111, F112; `deploy/helm/kgf-nexus/templates/*.yaml` securityContext | CI logs of `verify-s3-image` and `helm-image-digests.py`; rendered manifests. | Partial — **no vulnerability scanning** of Go modules, npm packages, Python packages or built images in either CI system (`npm ci --no-audit`; trivy runs on Terraform config only); **no SBOM** for Nexus images; release images are not signed; the docker executor adds no `--cap-drop`, `--security-opt no-new-privileges` or `--user` (`agent/internal/executor/docker.go`); subprocess executor is development-grade (CF F28). |

## 9. CC7 — System operations

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| **CC7.1** Detect configuration changes and new vulnerabilities | Migration-shape gate; Helm values schema + `validate.sh` with 15 refused bad-values cases; Terraform tflint/checkov/trivy with every accepted finding justified in `.checkov.yaml`/`.trivyignore`; `pitr-verify.sh` detects broken archiving; cert-expiry metrics; `nexus_build_info` shows running versions; `make check-refs` finds dangling artifact references. | OPERATIONS §6.2, §20.8, §4.3, §9.3, §7.8, §17.4; `deploy/terraform/README.md` | Gate/lint outputs; `pitr-verify.sh` JSON last line. | Partial — no dependency/image CVE detection (see CC6.8); `check-refs` and backup checks run only when invoked (OPERATIONS §7.6, §5). |
| **CC7.2** Monitor components for anomalies | Metrics catalogue with "alert on" guidance; audit rows for denied outcomes and failed logins; auth-failure throttling; cert-expiry `PrometheusRule` (`deploy/k8s/cert-alerts.yaml`); ServiceMonitor/PodMonitor in the chart; `nexus_audit_write_failures_total` and HMAC-signed webhook SIEM sink delivering the audit chain at least once (ADR-0025). | OPERATIONS §17, §9.3, §16.6, §25 | Prometheus targets; alert rule files; SIEM receipt of signed batches. | Partial — the **only shipped alert rules are for certificate expiry**; everything else in §17.4 is a suggested expression the operator must write; no limiter-specific metrics (OPERATIONS §16.6); no tracing. |
| **CC7.3** Evaluate security events | Inputs: audit export in NDJSON/CSV (formula-injection neutralised), cursor pagination, `outcome` filter, SIEM sink (ADR-0025). Today, before ADR-0025: `GET /v1/namespaces/{ns}/audit` without a cursor. | OPERATIONS §25; ADR-0025 §2 | Exported audit files; SIEM triage records. | Customer-responsibility (triage process) |
| **CC7.4** Respond to security incidents | Component runbooks usable during an incident: CA key rotation after compromise (OPERATIONS §9.4), NATS credential rotation (OPERATIONS §11.8), revoke all sessions of a user, disable user, quarantine worker, restore/PITR. Fail-closed audit for security-sensitive actions so credential minting cannot go unaudited (ADR-0025). | OPERATIONS §9.4, §11.8, §14.3, §25; `docs/TLS.md` §5.6 | Runbook execution records. | Gap — no security incident-response runbook (triage, containment, forensics, notification) exists in the repository; the org-level IR plan is the operator's. |
| **CC7.5** Recover from incidents | Logical backup of both stores in the correct order; pgBackRest continuous archiving with encrypted repository and point-in-time restore; timed restore and PITR drills with assertions (exact row counts, every `output_ref` resolves, artifact re-hash, post-target data absent); fleet reconciliation after restore. Measured on a laptop: PITR RTO 20.6–32.3 s, RPO 33.8–44.7 s; logical RTO 39–45 s. | OPERATIONS §2, §3, §4 (§4.2–§4.7), §7 | `docs/drills/2026-09-25-pitr-drill-54k.json`, `…-pitr-drill-420k.json`, `…-restore-drill-rto-48k.json`, `…-restore-drill-rto-409k.json`. | Partial — Helm/Kubernetes PITR "rendered, not exercised" (OPERATIONS §4.4); logical dump and artifact mirror unencrypted and not copied off-host; nothing alerts if the logical backup cron stops (OPERATIONS §5); drills on laptop hardware only. |

## 10. CC8 — Change management

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| **CC8.1** Authorize, design, develop, test, approve and implement changes | ADRs for design decisions; two CI systems (GitHub Actions `ci.yml`, Azure `pr-build-validation.yml` with a QualityGate stage); the Azure release pipeline starts only after validation succeeds on the same commit and publishes a digest map; additive-only migrations enforced by test and by the previous-release skew test; SPDX check (`make license-check`); frozen contract files (CONTRACT.md); upgrade procedure "back up first, migrate, then roll services". | `docs/adr/`; `.github/workflows/ci.yml`; `.azure-pipelines/*.yml`; OPERATIONS §6.2–§6.4; `docs/COMPATIBILITY.md` §6–§7 | Git history; PR records; CI run logs; `make compat-smoke` result (COMPATIBILITY.md §7.4, incl. negative control). | Partial — PR approvals and branch protection are repository settings, not in the repo (no `CODEOWNERS`); the release pipeline's deploy stages are written but **off** and environment approval checks are a to-do in `kgf-nexus-release.yml`; no release tags or changelog (`docs/COMPATIBILITY.md` §9 item 4); `ci.yml` defines the job id `e2e` twice (browser e2e and examples/chaos), so one definition may be rejected or shadowed — verify both run. Emergency-change procedure is the operator's. |

## 11. CC9 — Risk mitigation

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC9.1 Mitigate business-disruption risk | HA inputs: stateless services scaled horizontally, schedulers share the queue with no leader election, PDBs/HPAs/topology spread in the chart, NATS loss costs latency not correctness, backups/PITR. | ARCH §10; OPERATIONS §20.5, §20.9, §11.4 | Chaos suite (`make chaos`); CF F80, F82. | Customer-responsibility (business continuity program, insurance) |
| CC9.2 Vendor and business-partner risk | Inputs: license inventory and compatibility assessment; `make license-report`; third-party image selection with documented rationale and provenance checks (Silo vs MinIO); toolchain end-of-life tracking (Node). | `docs/THIRD-PARTY-LICENSES.md`; OPERATIONS §22, §22.1, §22.3 | `make license-report` output; `make verify-s3-image`. | Customer-responsibility — the operator's vendor program must also assess Nexus itself. Product-side weaknesses: no SBOM, no vulnerability disclosure policy, `postgres:16-alpine` in `values.yaml` is a tag, not a digest. |

## 12. A1 — Availability

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| **A1.1** Capacity maintained, monitored, evaluated | Backlog and `schedule_to_start` metrics (≤ 50 named queues); namespace quotas enforced at dispatch with `quota_blocked` metrics; weighted fair share; HPAs; cluster controller (plan mode by default); dispatch benchmarks; sizing tables for logs. | OPERATIONS §17.4, §21, §21.7, §12, §15, §19.3; [ADR-0017](adr/0017-weighted-fair-share-across-namespaces.md), [ADR-0018](adr/0018-postgres-dispatch-ceiling.md) | Metrics history; `make bench-dispatch` output. | Partial — no shipped capacity alert rules; HPA scale-out under load not verified (OPERATIONS §20.8). |
| **A1.2** Environmental protections, backups, recovery infrastructure | pgBackRest repository encrypted client-side (`aes-256-cbc`), separate bucket and principal from artifacts; managed-Postgres PITR path; Terraform: deletion protection, management locks, versioned buckets, 3-AZ VPC on EKS. | OPERATIONS §4.1, §4.2; `deploy/terraform/README.md` | `pitr-verify.sh`; Terraform plans. | Partial — no cross-region DR design; off-host copy of logical backups is not automated; environmental safeguards are the provider's. |
| **A1.3** Recovery plan procedures tested | Two scripted, destructive drills with phase tables, assertions and negative controls, run at two dataset sizes; `pitr-verify.sh` negative control. | OPERATIONS §4.5, §4.6, §7.4 | `docs/drills/*.json`. | Partial — laptop only; Kubernetes restore never executed; no drill against a real remote object store; recurring drill schedule (OPERATIONS §2.3 recommends monthly) is the operator's. |

## 13. C1 — Confidentiality

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| **C1.1** Identify and maintain confidential information | Tenant isolation enforced in every query and by composite FKs; secrets referenced, not stored, resolved per tenant on the worker with allow-lists (ADR-0025); credentials hashed at rest (API keys, scoped tokens, sessions, email tokens); audit `detail` must not contain secret material; task payloads never transit the control plane; content access (logs, results) is a separate permission. Encryption at rest of the database and object store is delegated to infrastructure (Terraform: platform- or customer-managed keys). | `docs/MULTITENANCY.md`; ARCH §9; OPERATIONS §25; `deploy/terraform/README.md` (Security defaults) | Tenancy integration tests; CF F101; schema showing `*_hash` columns. | Partial — no data classification or field-level encryption; logical dumps contain `api_keys` rows and every workflow parameter and are **unencrypted** (OPERATIONS §5); secrets reach worker hosts (memory, env-file, per-attempt Secret) and `env` injection is visible in `/proc` (ADR-0025); `aws-sm://`/`gcp-sm://` not implemented. |
| **C1.2** Dispose of confidential information | Log retention (opt-in), artifact GC for orphans, audit retention with legal hold (ADR-0025). | OPERATIONS §19.2, §7.7, §25 | GC summaries; retention settings. | Gap — confidential run data (parameters, attempt records, events, referenced outputs) cannot be purged through the product; run delete is soft (see CC6.5). |

## 14. PI1 — Processing integrity

| Control | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| **PI1.1** Processing objectives and specifications defined | Declarative, typed `WorkflowSpec` with JSON Schema, explicit `api_version`, versioned proto `nexus.v1`; runs pin their spec version. | ARCH §4.3, §9 (Versioning); `schemas/`; `docs/COMPATIBILITY.md` §3, §5 | Schema files; spec content address (`spec_hash`). | Met-by-product |
| **PI1.2** Input controls | Structural and semantic validation (acyclicity, type compatibility, fan-in bounds); unknown fields rejected on spec schema; content-addressed specs; `dry_run` no longer registers (F118); duplicate run submission returns the existing run (`UNIQUE(namespace, idempotency_key)`); client and server compute the same content address (F124). | ARCH §3.2, §10; CF F117, F118, F124; `docs/COMPATIBILITY.md` §3.3 | Gateway spec tests; golden content-address test. | Met-by-product |
| **PI1.3** Processing is complete, accurate, timely | Dispatch as one ACID transaction; lease fencing rejects stale reports; LOST distinct from FAILED; stable idempotency key across retries; four-timeout model with control-plane time; retries create new attempt rows; stalled-run detection (F84); examples + chaos suite in CI (no double dispatch under three schedulers). | CONTRACT.md (invariants); ARCH §5, §6, §10; CF F80, F82, F84, F104 | `make chaos` output; `TestCancelIsNotSkippedWhileMarkStartedHoldsTheAttempt` and related tests. | Partial — execution is at-least-once, not exactly-once (ARCH §6.1): correctness of external effects depends on handler idempotency; open findings F119 (backfill cancel is not final server-side) and F109 (cancel fan-out not re-issued). |
| **PI1.4** Outputs complete, accurate, distributed only to intended parties | Outputs stored inline or as content-addressed immutable objects; restore drills re-hash samples; `check-refs` finds rows whose object is missing; outputs readable only with `tasks.results.view` in the owning namespace. | ARCH §6; OPERATIONS §7.4, §7.8; [ADR-0008](adr/0008-result-store-separation.md) | Drill reports; `make check-refs` exit status. | Partial — CF F125 (OPEN): the Go agent does not enforce a declared output `max_bytes`; `check-refs` runs on demand only and does not cover the log index (OPERATIONS §19.2). |
| **PI1.5** Inputs and outputs stored completely, accurately, timely | Postgres is the single system of record; `run_events` append-only by invariant; attempts immutable once terminal; backups keep both stores consistent directionally (orphans, never dangling refs). | CONTRACT.md invariants 8–9; OPERATIONS §1, §7.2 | Migration 0005 grants; drill assertions. | Partial — the append-only guarantee is enforced by grants only when services use the per-service roles, which shipped deployments do not (CC6.1). |

## 15. P — Privacy (brief)

Nexus is application-agnostic: it does not know whether workflow parameters,
outputs or logs contain personal data. Personal data it stores about its *own*
users: email, display name, password verifier, and session `client_ip` /
`user_agent`; audit rows carry actor labels and client IPs.

| Criterion | Nexus capability | Documentation | Evidence | Status |
|---|---|---|---|---|
| P1 Notice | None. | — | Operator's privacy notice. | Customer-responsibility |
| P2 Choice and consent | None. | — | — | Customer-responsibility |
| P3 Collection | Registration collects email, name, password only. | `docs/MULTITENANCY.md` | — | Customer-responsibility |
| P4 Use, retention, disposal | Log retention opt-in; audit retention (ADR-0025). | OPERATIONS §19.2, §25 | — | Gap — no retention/erasure for run data or user accounts (accounts are disabled, not erased). |
| P5 Access (data-subject requests) | Audit filter by `actor_id` (ADR-0025 export). | OPERATIONS §25 | — | Gap — no per-subject search/export/erasure across run inputs, outputs and logs. |
| P6 Disclosure to third parties | Audit SIEM sink is operator-configured (ADR-0025). | OPERATIONS §25 | — | Customer-responsibility |
| P7 Quality | Users can change their own address (re-verification enforced by trigger). | `docs/MULTITENANCY.md` | — | Customer-responsibility |
| P8 Monitoring and enforcement | None. | — | — | Customer-responsibility |

---

## 16. Summary by status

| Group | Met-by-product | Partial | Gap | Customer-responsibility |
|---|---:|---:|---:|---:|
| CC1 | 0 | 0 | 0 | 5 |
| CC2 | 0 | 2 | 0 | 1 |
| CC3 | 0 | 0 | 0 | 4 |
| CC4 | 0 | 2 | 0 | 0 |
| CC5 | 0 | 2 | 0 | 1 |
| CC6 | 0 | 7 | 0 | 1 |
| CC7 | 0 | 3 | 1 | 1 |
| CC8 | 0 | 1 | 0 | 0 |
| CC9 | 0 | 0 | 0 | 2 |
| A1 | 0 | 3 | 0 | 0 |
| C1 | 0 | 1 | 1 | 0 |
| PI1 | 2 | 3 | 0 | 0 |
| P | 0 | 0 | 2 | 6 |
| **Total (51)** | **2** | **24** | **4** | **21** |

The low "Met-by-product" count is expected and not the headline: SOC 2 Security
criteria are organizational, and most product mechanisms land at "Partial"
because a default, an unexercised path or an unwired piece keeps them from
being evidence on their own. The Gap rows and the product-side gaps inside
Partial rows are in §17.

---

## 17. Prioritized gap list

Effort: S ≈ days, M ≈ 1–3 weeks, L ≈ a quarter. Owners are suggested roles.

| # | Pri | Gap | Criteria | Suggested owner | Effort |
|---|---|---|---|---|---|
| 1 | P0 | **Per-service Postgres roles are not used by compose or Helm.** Every service connects as one `nexus` user, so `audit_log` INSERT-only and `run_events` append-only grants (migrations 0005, 0007) are not enforced in shipped deployments. Wire a login role per service (chart + compose + Terraform), and document how an auditor verifies it (A.6). ADR-0025's hash chain detects tampering but does not prevent it. | CC6.1, CC7.2, PI1.5 | Platform / deploy | M |
| 2 | P0 | **No MFA and no SSO** (OIDC/SAML/SCIM planned, not implemented). Auditors expect MFA for administrative access; today only IdP-fronting at the edge could supply it. | CC6.1, CC6.2 | Identity | L |
| 3 | P0 | **No vulnerability scanning or SBOM** for Go, npm, Python dependencies or built images; `npm ci --no-audit`; release images unsigned. Add `govulncheck`, `npm audit`/OSV, `pip-audit`, image scanning, SBOM generation and signing to both CI systems. | CC6.8, CC7.1, CC9.2 | Build / release | M |
| 4 | P0 | **Insecure defaults:** TLS opt-in (plaintext in `make up`), `postgres.sslmode: disable` in Helm `values.yaml`, rate limiting off unless configured. Make production mode refuse these (the chart already refuses unauthenticated NATS). | CC6.6, CC6.7, CC5.2 | Platform | M |
| 5 | P0 | **Run data cannot be disposed of.** Run delete is soft; no retention for runs/attempts/events/parameters/outputs; ARCH §9's retention design is not built. Needed for C1.2, P4, and any customer contract with a deletion clause. | CC6.5, C1.2, P4, P5 | Control plane | L |
| 6 | P1 | **No security incident-response runbook** (triage, containment steps per component, evidence preservation using audit export + legal hold, credential mass-revocation). Component runbooks exist and can be linked. | CC7.4 | Security + docs | S |
| 7 | P1 | **Alerting ships only for cert expiry.** Ship a `PrometheusRule` for backup/archive failures (`pitr-verify`), stale GC, queue age, quota blocking, audit write failures, auth-failure/429 spikes, controller leadership. | CC7.2, A1.1 | Observability | M |
| 8 | P1 | **Logical backups and artifact mirror are unencrypted and local**; nothing alerts if the logical backup job stops. | CC7.5, A1.2, C1.1 | Platform | S–M |
| 9 | P1 | **Access-review tooling:** no report of effective permissions per user/token, no pagination on users API (500 cap in UI); API keys/scoped tokens survive the disabling of the user who created them. | CC6.2, CC6.3 | Identity | M |
| 10 | P1 | **Change-management evidence:** no `CODEOWNERS`; release deploy stages off and environment approvals not configured; no release tags/changelog; duplicate `e2e` job id in `ci.yml`; CI artifacts kept 14 days. | CC8.1, CC4.1 | Release engineering | S |
| 11 | P1 | **Recovery not exercised in a production-like environment:** Helm PITR restore never run; no remote object-store repository drill; no cross-region DR. | CC7.5, A1.2, A1.3 | Platform / operator | M |
| 12 | P1 | **Open correctness findings** F125 (output `max_bytes` not enforced by agent), F119 (backfill state has no from-state guard), F109 (cancel fan-out not re-issued), F115 (cloud identities for the result store). | PI1.3, PI1.4 | Control plane | S–M each |
| 13 | P2 | **Tracing** (OpenTelemetry) described in ARCH §9, absent from the code. | CC2.1, CC7.2 | Observability | M |
| 14 | P2 | **Docker executor hardening:** add `--cap-drop ALL`, `no-new-privileges`, non-root `--user`; document that subprocess is development-grade (CF F28). | CC6.8 | Agent | S |
| 15 | P2 | **Session controls:** fixed 12 h absolute TTL, no idle timeout, not configurable; auth-failure throttle is per replica and in memory (not a lockout). | CC6.1 | Identity | S |
| 16 | P2 | **Vendor-facing security posture:** no `SECURITY.md` / disclosure policy; no third-party penetration test on record; `postgres:16-alpine` referenced by tag. | CC2.3, CC9.2 | Product security | S (policy), M (pentest) |
| 17 | P2 | **Privacy tooling:** no DSAR search/export/erasure; no guidance for keeping personal data out of workflow parameters and logs. | P4, P5 | Product | M |
| 18 | P2 | **Secret providers:** `aws-sm://` and `gcp-sm://` not implemented (ADR-0025); no certificate revocation (TLS.md §5.6). | CC6.1, CC6.7 | Agent / platform | M |

Operator-side items that no product change closes: governance (CC1), risk
assessment (CC3), policies, HR onboarding/offboarding and background checks,
security awareness training, periodic access reviews, vendor management,
physical security, business continuity, and retention of evidence for the
audit period.

---

## Appendix A — How to produce evidence

Run these in the operator's environment; drill targets are **destructive** and
belong on an isolated stack (OPERATIONS §4.5). Keep the outputs, with timestamps, for
the audit period.

### A.1 Audit trail

```sh
# Today: recent audit rows for one namespace (no cursor before ADR-0025).
curl -sf -H "Authorization: Bearer $NEXUS_API_KEY" \
  "$NEXUS_ENDPOINT/v1/namespaces/$NS/audit?limit=100"

# ADR-0025 (see OPERATIONS §25 for the exact route and flags):
#   cursor pagination:  ...&cursor=<next_cursor>&since=...&until=...
#   full export:        the audit export route with format=ndjson|csv and
#                       since/until/action/outcome/actor_id filters
#   verification:       nexusctl audit verify   (against the API, or offline
#                       against an exported NDJSON file)
```

Evidence to keep: the export file, the verifier's output, the chain checkpoint
after any purge, legal-hold records, and SIEM receipts of signed webhook
batches. The chain proves retained rows were not altered relative to each
other; it does not stop a database superuser rewriting an un-exported tail
(ADR-0025), which is why an external copy matters.

### A.2 Backup and recovery

```sh
make backup-all BACKUP_DIR=/var/backups/nexus      # both stores, correct order (OPERATIONS §7.1)
make pitr-verify                                    # archiving + backup age + repo verify; JSON last line
make restore-drill RTO=1 DRILL_REPORT=restore.json  # DESTRUCTIVE; timed full recovery (OPERATIONS §4.5)
make pitr-drill PITR_REPORT=pitr.json               # DESTRUCTIVE; restore to T and to latest; RTO/RPO
make check-refs                                     # rows whose object is missing; nonzero on any
```

Reference results: `docs/drills/2026-09-25-*.json` (laptop; OPERATIONS §4.6).

### A.3 Change management and build integrity

```sh
go test ./internal/migrations/                      # additive-migration gate (OPERATIONS §6.2)
make compat-smoke                                   # previous release vs this schema (COMPATIBILITY.md §7)
make check-doc-refs                                 # runbook references resolve
deploy/helm/validate.sh                             # chart lint + kubeconform + bad-values cases
make tf-validate && make tf-lint                    # Terraform offline validate; tflint, checkov, trivy
make verify-s3-image                                # provenance of the pinned S3 image (needs gh)
make license-check && make license-report           # SPDX headers; per-module licence inventory
```

Plus: PR history with approvals (repository settings), CI run logs, and the
release pipeline's published image-digest map.

### A.4 Access control

```sh
go test ./services/api-gateway/authn/...            # RBAC and cross-namespace unit tests
NEXUS_TEST_DATABASE_URL=... go test -tags integration ./internal/store/... ./services/api-gateway/...
```

Plus: role matrix and user-role assignments from **Govern → Roles &
permissions**, audit rows for `user.*`, `token.*`, `role.*`, `session.login`
(including `denied`), and the rate-limit configuration line logged at gateway
start-up (OPERATIONS §16).

### A.5 Transport security and monitoring

```sh
openssl s_client -connect <host>:9090 -servername scheduler </dev/null 2>/dev/null \
  | openssl x509 -noout -serial -enddate            # what a service presents (OPERATIONS §9.1)
deploy/nexus-certs.sh check <dir>                   # nonzero within 7 days of expiry
curl -s localhost:19100/metrics | grep nexus_build_info   # running version per process (OPERATIONS §17.2)
```

Plus: the deployed `NEXUS_REQUIRE_TLS` setting, loaded alert rules
(`deploy/k8s/cert-alerts.yaml` and the operator's own), and Prometheus
target health.

### A.6 Database role enforcement (gap #1)

```sql
\dp audit_log                      -- grants: nexus_api_gateway should have INSERT only
\dp run_events
SELECT usename, application_name, count(*) FROM pg_stat_activity GROUP BY 1, 2;
-- If every service shows the same owner user, the grants above are not in force.
```

### A.7 Supply chain and installation (ADR-0025)

`make airgap-bundle` produces `dist/nexus-airgap-<version>.tar.gz` with
digest-pinned images, `manifest.json`, `SHA256SUMS` and, when cosign is
available, a signature; `deploy/airgap/load.sh` verifies checksums (and the
signature if present), pushes to a private registry and renders Helm values by
registry-reported digest. Keep the manifest, checksum verification output and
rendered values as installation evidence (`docs/AIRGAP.md`).

---

## Appendix B — Claims checked and not verifiable from the repository

- Whether GitHub branch protection, required reviews or Azure DevOps branch
  policies are enabled: repository settings, not files.
- Whether the duplicated `e2e` job id in `.github/workflows/ci.yml` causes
  GitHub Actions to reject the workflow or run only one of the two jobs: not
  executed here.
- ADR-0025 behaviour: taken from the ADR text only; the implementation is on
  sibling branches and was not tested for this assessment.
- The Azure release pipeline's deploy stages and the AKS environment: the
  pipeline states they are off and the cluster is stopped.
