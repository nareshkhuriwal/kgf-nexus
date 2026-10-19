# Operations

The operational documents live next to the code they describe and are
rendered here unchanged, so there is one copy of each:

| Page | Source | Covers |
| --- | --- | --- |
| [Runbook](runbook.md) | `docs/OPERATIONS.md` | backup, restore, PITR, migrations and upgrades, artifacts, email, NATS, the cluster controller, the Kubernetes executor, rate limiting, metrics, durable logs, Helm, quotas, pinned images |
| [Install on an existing AKS cluster](install-aks.md) | `docs/INSTALL-AKS.md` | the full install on AKS you already run: Flexible Server, Blob through workload identity, worker pools, the cluster controller; Helm or Terraform |
| [Container images](images.md) | `docs/IMAGES.md` | what each Nexus image listens on, needs and writes, for any runtime |
| [Deploying](deploy.md) | `deploy/README.md` | the Docker Compose stack, development against production configuration |
| [TLS](tls.md) | `docs/TLS.md` | inter-service TLS/mTLS and how to roll it out |
| [Terraform](terraform/index.md) | `deploy/terraform/` | AKS, EKS and GKE environments, and an existing AKS cluster |
| [Azure provisioning](azure-provisioning.md) | `deploy/AZURE-PROVISIONING.md` | Azure specifics |
| [Tracing and storage backends](tracing-storage.md) | ADR-0026, ADR-0027 | OpenTelemetry tracing; the S3, Azure Blob and GCS result stores |
| [Air-gapped installation](airgap.md) | `docs/AIRGAP.md` | a digest-pinned bundle and mirroring it into a private registry |

## Find the runbook section for a task

| I need to… | Runbook section |
| --- | --- |
| install on Kubernetes with Helm | [§20](runbook.md#20-installing-with-helm), then the [production checklist, §20.9](runbook.md#209-production-checklist) |
| install on an AKS cluster I already run | [Install on an existing AKS cluster](install-aks.md), then [§20.16](runbook.md#2016-existing-clusters-and-worker-pool-releases) for worker-pool releases |
| back up and restore the database | [§2](runbook.md#2-backup) and [§3](runbook.md#3-restore) |
| set up point-in-time recovery | [§4](runbook.md#4-rpo-rto-and-point-in-time-recovery) |
| back up the result store | [§7](runbook.md#7-artifacts-backing-up-the-result-store) |
| apply a schema migration or upgrade | [§6](runbook.md#6-schema-migrations-and-upgrades) |
| turn on TLS, rotate certificates | [§9](runbook.md#9-tls-certificates-rotation-runbook-and-expiry-alerts), [TLS](tls.md) |
| configure SMTP for verification and password reset | [§10](runbook.md#10-email-smtp-delivery-for-verification-and-password-recovery) |
| run NATS JetStream, rotate its credentials | [§11](runbook.md#11-the-event-bus-nats-jetstream) |
| run agents with the Kubernetes executor | [§13](runbook.md#13-kubernetes-executor) |
| tune dispatch at large backlogs | [§15](runbook.md#15-dispatch-at-large-backlogs-the-windowed-pull-claim) |
| size and tune rate limits | [§16](runbook.md#16-gateway-rate-limiting) |
| scrape metrics, set alerts | [§17](runbook.md#17-metrics) |
| retry a failed run as an operator | [§18](runbook.md#18-operator-retry-of-a-failed-run) |
| size log retention | [§19](runbook.md#19-durable-task-logs-retention-and-sizing) |
| set namespace quotas, export usage | [§21](runbook.md#21-namespace-quotas-and-usage-metering) |
| bump a pinned third-party image | [§22](runbook.md#22-third-party-images-and-toolchains-what-is-pinned-and-why) |
| know where cost management went | [§23](runbook.md#23-cost-management-moved-to-kgf-billing) |
| run Nexus inside KGF Cloud | [§24](runbook.md#24-running-nexus-inside-kgf-cloud-context-authentication-tenancy-mapping-and-service-accounts) |
| resolve task secrets, export the audit trail, install air-gapped | [§25](runbook.md#25-task-secrets-the-audit-trail-and-air-gapped-installation) |
| turn on tracing, choose a result-store backend | [§26](runbook.md#26-tracing-and-result-store-backends) |
| configure notifications, triggers and sensors | [§27](runbook.md#27-notifications-webhook-triggers-sensors-and-asset-lineage) |
| run fractional GPUs, spot capacity, multi-cluster placement | [§28](runbook.md#28-fractional-gpus-spot-capacity-model-caches-and-multi-cluster-placement) |
| meter AI usage, share provider rate limits, checkpoint | [§29](runbook.md#29-ai-workloads-usage-units-shared-rate-limits-and-checkpoints) |

## What is measured and what is not

Backup, restore and PITR drills have been run, and their results are recorded
in the runbook (§3.2, §4.6) and in `docs/drills/`. They are **laptop
measurements**, not production RPO/RTO commitments. Dispatch-throughput
figures are in [ADR-0018](../design/adr/0018-postgres-dispatch-ceiling.md) and
the current laptop curves in [Benchmarks](../design/benchmarks.md), with the
same caveat. The runbook's [§5](runbook.md#5-what-is-not-covered) lists
what backups do not cover.

## Development versus production

The compose stack (`nexusctl up`, `make up`) is for development. Its agents
mount the Docker socket, which is effectively root on the host. Its GPUs are
simulated, and its mail goes to a local catcher. Production deployment is
Helm on Kubernetes, with the Kubernetes executor, TLS, real SMTP and real
capacity. It is covered by §20 and the production checklist.
