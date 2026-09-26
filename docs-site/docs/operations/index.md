# Operations

The operational documents live next to the code they describe and are
rendered here unchanged, so there is one copy of each:

| Page | Source | Covers |
| --- | --- | --- |
| [Runbook](runbook.md) | `docs/OPERATIONS.md` | backup, restore, PITR, migrations and upgrades, artifacts, email, NATS, the cluster controller, the Kubernetes executor, rate limiting, metrics, durable logs, Helm, quotas, pinned images |
| [Deploying](deploy.md) | `deploy/README.md` | the Docker Compose stack, development against production configuration |
| [TLS](tls.md) | `docs/TLS.md` | inter-service TLS/mTLS and how to roll it out |
| [Terraform](terraform/index.md) | `deploy/terraform/` | AKS, EKS and GKE environments |
| [Azure provisioning](azure-provisioning.md) | `deploy/AZURE-PROVISIONING.md` | Azure specifics |

## Find the runbook section for a task

| I need to… | Runbook section |
| --- | --- |
| install on Kubernetes with Helm | [§20](runbook.md#20-installing-with-helm), then the [production checklist, §20.9](runbook.md#209-production-checklist) |
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

## What is measured and what is not

Backup, restore and PITR drills have been run, and their results are recorded
in the runbook (§3.2, §4.6) and in `docs/drills/`. They are **laptop
measurements**, not production RPO/RTO commitments. Dispatch-throughput
figures are in [ADR-0018](../design/adr/0018-postgres-dispatch-ceiling.md), with
the same caveat. The runbook's [§5](runbook.md#5-what-is-not-covered) lists
what backups do not cover.

## Development versus production

The compose stack (`nexusctl up`, `make up`) is for development. Its agents
mount the Docker socket, which is effectively root on the host. Its GPUs are
simulated, and its mail goes to a local catcher. Production deployment is
Helm on Kubernetes, with the Kubernetes executor, TLS, real SMTP and real
capacity. It is covered by §20 and the production checklist.
