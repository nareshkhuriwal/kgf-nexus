# KGF Nexus on Google Kubernetes Engine

This root module runs `modules/gke-platform` first, then the in-cluster
pieces, then `modules/kgf-nexus`. It has been **validated offline only**
(`make tf-validate`), not applied. See [../../README.md](../../README.md) for
version choices, the security model and finding F115.

## What it creates

| Layer | Resources |
|---|---|
| APIs | compute, container, sqladmin, servicenetworking, artifactregistry, secretmanager, iam, logging, monitoring (`enable_apis`) |
| Network | A custom-mode VPC with a node subnet and secondary ranges for pods and Services (**VPC-native**). Private Google access and subnet flow logs are on. Cloud Router and Cloud NAT provide egress for private nodes. Private services access (VPC peering range) carries Cloud SQL traffic. |
| GKE | **Regional** cluster (control plane and nodes in three zones) on the REGULAR release channel. **Private nodes.** The control-plane IP endpoint is limited to `master_authorized_cidrs` (`0.0.0.0/0` is refused) or made private, and the IAM-authorised DNS endpoint serves automation. **Workload Identity**, **Dataplane V2** (enforced NetworkPolicy), shielded nodes, no client certificate, no legacy ABAC, the PD CSI driver, system, workload and control-plane logging, Managed Prometheus, basic security posture, weekend maintenance windows, and deletion protection. |
| Node pools | `cpu`: n2d-standard-4, 1-3 per zone. `gpu` (optional): g2-standard-8 + 1 × L4, 0-1 per zone, with the driver and device plugin installed by GKE and taint `nvidia.com/gpu=present:NO_SCHEDULE`. Both pools run COS, pd-balanced disks, `GKE_METADATA` (pods never see the node identity), secure boot, legacy metadata endpoints off, and a dedicated node service account (logs, metrics, Artifact Registry read only). |
| Registry | Artifact Registry Docker repository `kgf-nexus` with **immutable tags** and cleanup of untagged images after 14 days. |
| Cloud SQL | PostgreSQL 16, Enterprise edition, db-custom-2-7680, 50 GB PD-SSD with autoresize. **Regional HA on** by default. **No public IP**: private IP only, `ENCRYPTED_ONLY`. Backups keep 14 copies, **PITR** keeps 7 days of logs, and query insights and connection/lock/checkpoint logging are on. Deletion protection is on. The `nexus` built-in user has a write-only password. The password is also in Secret Manager. |
| Artifacts | GCS bucket: uniform access, public access prevention enforced, versioning, 7-day soft delete, and lifecycle rules (abort multipart after 7 days, delete noncurrent after 30). |
| Artifact identity | Service account `<name>-art` with `roles/storage.objectUser` on the bucket only, bound through **Workload Identity** to `nexus/kgf-nexus` (default). An **HMAC key** is created only with `artifact_identity = "hmac"`. |
| In cluster | StorageClass `nexus-regional-ssd` (pd-ssd, **regional PD** replicated across two zones, Retain), then the Nexus release. |

### The artifact store on GKE

By default (`artifact_identity = "workload-identity"`) Nexus uses **GCS
natively** through its `gcs` backend (`cloud.google.com/go/storage`,
ADR-0027): the kgf-nexus ServiceAccount carries
`iam.gke.io/gcp-service-account`, the chart sets `NEXUS_GCS_AUTH=auto`, and
tokens come from the GKE metadata server. No key exists in the cluster.

`artifact_identity = "hmac"` is the fallback: GCS through its S3
interoperability API with an HMAC key pair in the credentials Secret, as
before. Both paths pass the conformance suite against fake-gcs-server; neither
has been run against real GCS yet (F115). Rotating the HMAC key is described
in the top-level README.

## Prerequisites

- Terraform 1.11 or later, the gcloud CLI, and `gke-gcloud-auth-plugin` for
  kubectl. Terraform uses the caller's OAuth token (Application Default
  Credentials, or a CI workload identity).
- The caller needs `roles/owner`, or the equivalent set of Compute Network
  Admin, Kubernetes Engine Admin, Cloud SQL Admin, Service Account Admin,
  Project IAM Admin, Storage Admin, Artifact Registry Admin, Secret Manager
  Admin and Service Usage Admin.
- NATS identities: `NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh init --tenant default`.
- Images pushed as `<registry>kgf-nexus/<component>:<git-sha>` (for example
  `us-central1-docker.pkg.dev/<project>/kgf-nexus/api-gateway:<sha>`).
- The GPU pool needs `gpu_pool.zones` set to zones that actually offer the
  accelerator.

## Apply

```sh
cp terraform.tfvars.example terraform.tfvars      # fill in
cp backend.hcl.example backend.hcl                # fill in
terraform init -backend-config=backend.hcl
terraform apply -target=module.platform           # first time only: cloud resources
# push images to $(terraform output -raw registry)kgf-nexus/...
terraform apply                                   # StorageClass, Nexus
```

To destroy, set `deletion_protection = false` and apply. The bucket does not
`force_destroy`.

## Outputs

| Output | Meaning |
|---|---|
| `kubeconfig_command` | `gcloud container clusters get-credentials ... --region ...` (adds `--dns-endpoint` for a private control plane) |
| `registry` | `<region>-docker.pkg.dev/<project>/` (images under `kgf-nexus/`) |
| `database` | Private IP, port, database, user, `sslmode=require`, the instance connection name, the **Secret Manager secret** holding the password, and the Kubernetes Secret key. The password itself is never printed. |
| `object_store` | Endpoint, bucket, service account, and where the HMAC key lives |
| `namespace`, `console_access` | As named |
| `admin_api_key` | Sensitive: `terraform output -raw admin_api_key` |

## Cost note (indicative, on-demand list prices, us-central1, 730 h/month)

| Item | Approx. USD/month |
|---|---|
| GKE cluster management fee (regional, Standard) | ~73 ($0.10/h). The free-tier credit covers only zonal and Autopilot clusters. |
| cpu pool: 3 zones × 1 n2d-standard-4 (~$0.17/h) | ~370 at the minimum, up to ~1,100 at 3 per zone |
| Cloud SQL db-custom-2-7680, **regional HA**, 50 GB SSD | ~230 (about half zonal) |
| Cloud NAT gateway + data | ~35 + $0.045/GB |
| Regional PD-SSD for NATS; pd-balanced boot disks (3 × 100 GB) | ~40 |
| GCS, Artifact Registry, Secret Manager, Cloud Logging over the free allotment | Usage-based |
| GPU pool (g2-standard-8 + L4, ~$0.85/h), when scaled up | $0 at min 0 |
| **Baseline total** | **roughly $750-850 per month** |

Committed-use discounts cut the compute lines substantially. Check prices
with the Google Cloud pricing calculator.
