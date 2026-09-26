# Nexus service controls — evidence for KGF Cloud's SOC 2 program

**Status:** self-assessment of the Nexus service, 2026-09-26, against branch
`feat/secrets-audit-airgap` (ADR-0025). **Not an attestation.**

## What this document is, and is not

Nexus is a **service of KGF Cloud** (the `KGF.Nexus` provider behind KGF's
gateway), not a standalone platform. SOC 2 attests an *organization's*
controls; the organization here is KGF Cloud, and its compliance program owns
the system description, the risk assessment and the control matrix. This
document lists the **controls the Nexus service implements**, where each is
documented, and the evidence KGF Cloud's program can collect for it. It does
not claim that Nexus, or KGF Cloud, is SOC 2 compliant, certified or
attested; no auditor has reviewed it.

**Organization-level controls belong to KGF Cloud**, and Nexus does not
restate them: governance and oversight (CC1), communication and policies
(CC2), risk assessment (CC3), monitoring of controls (CC4), control
selection and policy deployment (CC5), workforce identity, joiner/mover/leaver
and **access reviews** (CC6.2-CC6.3 at the organization level), physical
security (CC6.4), incident response program (CC7.3-CC7.5 at the organization
level), change approval and segregation of duties (CC8.1 process), vendor and
business-partner management (CC9.2), business continuity planning (A1),
privacy notices, consent and data-subject requests (P1-P8). Where Nexus
produces an input to one of those, the row says so.

Status legend: **Implemented** (mechanism in the code, with a test or drill
record in the repository), **Partial** (limitation named in the row),
**Gap** (nothing yet; listed at the end), **KGF** (the control is KGF
Cloud's; Nexus's contribution noted).

## Service boundary

| Concern | KGF Cloud | Nexus service |
|---|---|---|
| People authenticate | KGF gateway (Entra ID, `docs/kgf-identity.md` in kgf-cloud) | Accepts KGF's signed `X-KGF-Context` for **attribution** of audit rows (verified, OPERATIONS §25.2); authorizes with its own credentials (namespace-scoped API keys, scoped tokens, sessions, RBAC) |
| Secrets | Platform Key Vault per environment, workload identity, CSI projection | Workers resolve task `SecretRef`s per tenant from Key Vault (`akv://`, KGF vault convention), Vault, mounted files or an allow-listed environment (OPERATIONS §25.1) |
| Audit | `audit_events`, per-project export | Hash-chained, exportable per-namespace trail with a KGF-shaped export and signed sink (OPERATIONS §25.2); no KGF ingestion API exists yet |
| Infrastructure | AKS stamp, PostgreSQL flexible server, networking, Key Vault | Helm chart, migrations, service configuration, worker agents |

## Controls the service implements

### Logical access (CC6)

| Criterion | Nexus control | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC6.1 Access is restricted | Every route passes one enforcement point (`guard`): authentication, namespace authorization, and a permission per route (`rbac_routes.go`, a route without a mapping is refused). Credentials are hashed at rest; sessions expire; scoped tokens intersect with their role. | `docs/RBAC-IMPLEMENTATION.md`, `docs/RBAC-DESIGN.md`, `docs/MULTITENANCY.md` | `go test ./services/api-gateway/...` (route-permission and RBAC integration tests) | Implemented |
| CC6.1 Tenant isolation | Tenant ownership enforced by the database (0014/0015 triggers, composite FKs); scheduling filters by tenant; one namespace per KGF customer tenant. Application-enforced, not RLS. | `docs/MULTITENANCY.md` | Integration tests listed there | Partial (no RLS for service roles) |
| CC6.1 Secrets never in metadata | Specs hold `SecretRef`s only (schema rejects literals); values are resolved by the worker, per tenant, from that tenant's providers only, and never written to the WAL, result store, events or reports; log lines, progress notes and errors are redacted. | ADR-0025; OPERATIONS §25.1 | `make test-vault`; `TestSecretsAreRedactedFromShippedTaskLogs`; `TestK8sSecretsAreReferencedNeverLiteral` | Implemented (Key Vault verified with a fake only; manual procedure in §25.1) |
| CC6.1 Worker credentials | Workers hold no database credentials; the subprocess executor no longer hands tasks the agent's own credentials (F138); agent memory is non-dumpable on Linux when secret providers are configured. | ADR-0011, ADR-0025; CONTRACT-FINDINGS F138 | `TestSubprocessInjectsSecretsAndScrubsAgentCredentials` | Implemented |
| CC6.2 / CC6.3 Provisioning, changes, removal | Namespace administrators create, disable, re-role users and mint/revoke tokens; every grant commits with its audit row (fail closed); revocations are never blocked. Access **reviews** are KGF's. | ADR-0025 §2; OPERATIONS §25.2 | `nexusctl audit export --action user.create` etc.; `TestFailedAuditBlocksAndRollsBackSecurityGrants` | Implemented (review process: KGF) |
| CC6.6 External threats at the boundary | The gateway is the only ingress; per-address, auth-failure and per-credential rate limits; audit sinks refuse internal destinations (SSRF) at configuration and dial time. | OPERATIONS §16, §25.2 | `TestURLPolicyRefusesInternalDestinations`; rate-limit tests | Partial (rate limiting is opt-in and per replica) |
| CC6.7 Data in transit | TLS / mTLS between every gRPC hop, hot-reloaded certificates, expiry alerts; Vault and Key Vault over TLS only (plain HTTP refused off loopback); sink delivery HTTPS only by default. | `docs/TLS.md`; OPERATIONS §9 | `make up-tls`, `make certs-check` | Partial (TLS is configuration, off in dev compose) |
| CC6.8 Unauthorized software | Every third-party image pinned by digest; first-party images built from the tree; the air-gap bundle carries digests, checksums and (with cosign) a signature, and `load.sh` refuses unlisted files. | OPERATIONS §22, §25.3; `docs/AIRGAP.md` | `make airgap-bundle`, `make airgap-test`; `manifest.json` | Partial (no image vulnerability scan; see gaps) |

### System operations and change (CC7, CC8)

| Criterion | Nexus control | Documentation | Evidence | Status |
|---|---|---|---|---|
| CC7.1 Configuration and vulnerability monitoring | Migration safety checks (`internal/migrations`), Helm schema and render validation (`deploy/helm/validate.sh`), Terraform lint (tflint/checkov/trivy over IaC), license inventory. | OPERATIONS §6, §20 | CI runs; `make license-check` | Partial (no dependency/image CVE scanning) |
| CC7.2 Anomaly detection | Metrics on every service, including `nexus_audit_write_failures_total`, sink delivery failures, rate-limit rejections; documented alerts. | OPERATIONS §17, §25.2 | Prometheus scrape; alert rules the operator installs | Partial (alert rules are documentation, KGF deploys them) |
| CC7.2 Audit trail | Per-namespace hash chain over every audited action with KGF organization, principal and request id when a verified KGF context is present; append-only for every role (grant + triggers); keyset listing; NDJSON/CSV/KGF export; signed at-least-once sink; retention with legal hold; offline verifier. | ADR-0025; OPERATIONS §25.2 | `nexusctl audit export` + `nexusctl audit verify --file` (exit 0 / 8); `TestAuditTamperingIsDetected`; `TestAuditChainIsUnforkedUnderConcurrentReplicas` | Implemented (KGF-side ingestion: gap) |
| CC7.3-CC7.5 Incident evaluation, response, recovery | Supplies evidence: audit export and verification, attempt logs, run events, legal hold to freeze evidence. The response program is KGF's. | OPERATIONS §19, §25.2 | Exports; `POST /audit/legal-hold` | KGF |
| CC8.1 Change management | Additive-only migrations with automated hazard checks, rolling-upgrade compatibility suite, ADRs for design changes, findings register. Approval workflow and branch protection are KGF's. | ADR-0011; `services/migrations/README.md`; OPERATIONS §6; `CONTRACT-FINDINGS.md` | `go test ./internal/migrations`; `make compat-smoke` | Implemented (approval: KGF) |

### Availability, confidentiality, processing integrity

| Criterion | Nexus control | Documentation | Evidence | Status |
|---|---|---|---|---|
| A1.2 Backup and recovery | Backups of both stores in order, PITR with pgBackRest, timed restore drills with recorded RTO/RPO. | OPERATIONS §2-§4, §7 | `docs/drills/*.json`; `make restore-drill`, `make pitr-drill` | Implemented (drills on a laptop, not the KGF environment) |
| A1.2 Capacity | Namespace quotas enforced in the claim; usage ledger; advertised vs committed capacity. | ADR-0021; OPERATIONS §21 | Quota integration tests | Implemented |
| A1.3 Recovery testing | Restore and PITR drills are scripted and timed. Scheduling them is KGF's. | OPERATIONS §4 | Drill JSON records | KGF (Nexus supplies the drill) |
| C1.1 Confidential information | Secrets resolved on workers only; sink signing secrets sealed (AES-256-GCM, bound to the namespace) and shown once; attempt results visible only with `tasks.results.view`. Encryption at rest of PostgreSQL and object storage is the infrastructure's (KGF stamp). | ADR-0025; OPERATIONS §25 | `TestKeyringSealsPerNamespace`; RBAC tests | Partial (at-rest encryption: KGF) |
| C1.2 Disposal | Audit retention through `audit_purge()` only, respecting legal hold and sink delivery, with checkpoints; durable task logs have retention; result-store orphans are swept. | OPERATIONS §19, §25.2, §7 | `TestAuditRetentionLegalHoldAndCheckpoints`; `make artifact-gc` | Partial (no retention policy for run history) |
| PI1.x Processing integrity | Validated, content-addressed specs; atomic dispatch; lease fencing; at-least-once execution with stable idempotency keys; distinct LOST outcomes; immutable attempt history. | ARCHITECTURE §5-§6; ADR-0006, ADR-0010 | Chaos and integration suites | Implemented (effects on external systems are at-least-once by design) |

## Evidence KGF Cloud's program can collect

| Evidence | How |
|---|---|
| Audit trail, verifiable | `nexusctl audit export --file audit-<ns>-<date>.ndjson`, then `nexusctl audit verify --file ...`; store both outside the database |
| Audit trail for KGF tooling | `GET /v1/namespaces/{ns}/audit/export?format=kgf`, or a sink with `format: kgf` |
| Access changes | export filtered by `action=user.*`, `token.*`, `user.roles.assign`, `role.*` |
| Audit write health | `nexus_audit_write_failures_total`, `nexus_audit_sink_delivery_failures_total` |
| Recovery | `make restore-drill`, `make pitr-drill` JSON reports |
| Software provenance | air-gap `manifest.json` + `SHA256SUMS` (+ cosign bundle); OPERATIONS §22 pin table |
| Secret handling | `make test-vault`; `go test ./agent/...` |

## Gaps (service level)

1. **No KGF-side audit ingestion.** Nexus exports KGF-shaped events and can
   push them, but KGF Cloud has no collector or ingestion API; until KGF adds
   one, Nexus events do not appear in KGF's activity log.
2. **Nexus identities are not KGF identities.** Nexus authorizes with its own
   users, API keys and tokens; KGF's context is used for attribution only.
   Federating Nexus sign-in with KGF identity is future work.
3. **No dependency or image vulnerability scanning** (govulncheck, image CVE
   scanning) and no SBOM in CI; only IaC is scanned.
4. **Key Vault resolution** is tested against a fake; the real-vault
   procedure (OPERATIONS §25.1) has not been run. Vault Kubernetes auth is
   tested against a protocol fake.
5. **Retention for run history** (runs, attempts, events) has no policy;
   audit and task logs do.
6. **Audit rows written before migration 0033** are outside the chain
   (reported as pre-chain); the console's audit page does not yet page or
   export (API and CLI do).
7. Open findings: F115 (cloud identities for the result store), F119, F125
   (CONTRACT-FINDINGS.md).
8. The air-gap bundle builds the Docker host's platform by default; a
   multi-platform bundle (`PLATFORMS=`) needs buildx emulation and was not
   exercised here.
