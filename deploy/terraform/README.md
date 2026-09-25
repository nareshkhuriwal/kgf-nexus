# KGF Nexus on Kubernetes with Terraform

Everything under this directory installs KGF Nexus with the Helm chart in
`deploy/helm/kgf-nexus`. The `kgf-nexus` module installs the release into a
cluster that already exists. The three cloud platform modules create that
cluster and the managed services around it.

```
modules/
  kgf-nexus/      namespace, generated credentials Secret, Helm release (any cluster)
  aks-platform/   Azure: VNet, AKS, ACR, PostgreSQL Flexible Server, Blob, Key Vault, Log Analytics
  eks-platform/   AWS:   VPC (3 AZ), EKS, ECR, RDS PostgreSQL, S3, KMS, CloudWatch
  gke-platform/   GCP:   VPC, regional GKE, Artifact Registry, Cloud SQL, GCS, Secret Manager
environments/
  dev/            Docker Desktop / kind (existing; make k8s-dev-up)
  aks/ eks/ gke/  platform module + in-cluster pieces + kgf-nexus, one root each
```

Each cloud environment has its own README with the apply procedure, the
outputs and a cost note: [aks](environments/aks/README.md),
[eks](environments/eks/README.md) and [gke](environments/gke/README.md).

**Status: validated offline only.** Nothing here has been applied to any
cloud from this repository. `make tf-validate` checks formatting, runs
`init -backend=false` and `validate`, and runs `terraform test`. The tests
**plan** with mocked providers: they check that the example tfvars pass every
variable validation, that the modules connect correctly, and that the security
defaults hold. They do not show that a cloud API accepts the plan.
Finding **F115** in `CONTRACT-FINDINGS.md` lists what still stands between
these modules and a verified production deployment.

## The object store: the one thing that differs per cloud

The chart's result store speaks **only the S3 API**. It uses minio-go with
static credentials (`credentials.NewStaticV4` in
`agent/internal/resultstore`, `services/api-gateway/logs` and
`internal/artifactgc`). Here is what that means on each cloud today:

| Cloud | Artifact store Nexus uses today | How | Status |
|---|---|---|---|
| AWS | **Amazon S3**, native | IAM-user key pair limited to one bucket. An explicit deny blocks every request that does not come through the VPC's S3 gateway endpoint. | Works now. The IRSA / Pod Identity role is also provisioned, but it stays unused until the chart and client can use it (F115). |
| GCP | **GCS through its S3 interoperability (XML) API** | HMAC key of a service account that has `roles/storage.objectUser` on one bucket. It is stored in the kgf-nexus credentials Secret. | Works with minio-go, which special-cases `storage.googleapis.com` (for example, it deletes objects one at a time because GCS has no multi-object delete). Large multipart uploads and the GC sweep have **not** been exercised against GCS (F115). |
| Azure | **In-cluster MinIO on a Premium SSD PVC** | The chart's own MinIO (`minio.enabled=true`) on the `nexus-premium-ssd` StorageClass. | Works, but you operate it: capacity, backups (`make backup-artifacts`) and availability are yours. Azure Blob has **no S3 endpoint**. |

Azure Blob is **not** used by Nexus today. The storage account, the private
container, the private endpoint and a workload identity with `Storage Blob
Data Contributor` on that container are all provisioned. They wait for a
native Blob backend, which does not exist yet. The `azure_blob_native_backend`
variable is reserved for that backend, and its validation refuses `true`.
Two other routes exist, and neither is wired: an S3-to-Blob gateway such as
s3proxy (MinIO's own gateway mode was removed in 2022), or the native backend.
Both are tracked in F115.

## Version choices

Each version was checked against the official registry on 2026-09-25
(`registry.terraform.io/v1/providers/...` and `/v1/modules/...`). Providers
are pinned with `~>` in `versions.tf`. The **exact** builds are pinned in
each environment's committed `.terraform.lock.hcl`, with hashes for
linux/darwin on amd64/arm64.

| Component | Version | Source / reason |
|---|---|---|
| Terraform | `>= 1.11, < 2.0`; CI pins 1.14.3 | 1.11 adds write-only arguments (`*_wo`), which keep DB passwords out of resource state. |
| hashicorp/azurerm | 5.7.0 (`~> 5.7`) | Current major (5.0 shipped 2026-07-30). |
| hashicorp/aws | 6.66.0 (`~> 6.66`) | Current. The EKS module needs >= 6.59. |
| hashicorp/google | 8.4.0 (`~> 8.4`) | Current major. |
| hashicorp/kubernetes | 3.2.1 (`~> 3.2`) | Current. The kgf-nexus module accepts `>= 2.38, < 4.0`, so the dev environment stays on its locked 2.38. |
| hashicorp/helm | 3.3.0 (`~> 3.3`) | Current. |
| hashicorp/random | 3.9.1 (`~> 3.9`) | Current. |
| terraform-aws-modules/vpc | **6.7.3** (exact) | The de facto standard AWS VPC module: subnet tiers, NAT per AZ, flow logs, ELB subnet tags. |
| terraform-aws-modules/eks | **21.26.0** (exact) | The most widely used EKS module: access entries, add-ons with Pod Identity, IRSA, managed node groups. |
| Azure/aks | **not used** | Its newest release (11.7.0, 2026-05-11) requires `azurerm < 5.0`. The 4.x line stopped at 4.81.0 (2026-07-14), so using the module would tie the whole stack to a provider line that no longer gets releases. `aks-platform` uses native `azurerm_kubernetes_cluster` resources instead, which is about as much code as configuring the module. The Microsoft AVM module (`avm-res-containerservice-managedcluster` 0.8.3) was also rejected: it is pre-1.0 and uses azapi only. |
| terraform-google-modules/kubernetes-engine | **not used** | 45.0.0 requires `google < 8`. Native resources are used for the same reason as on Azure. |
| NVIDIA k8s-device-plugin chart | 0.20.1 | Latest release. AKS and EKS install the GPU driver but not the plugin; GKE installs both. |
| tflint | 0.64.0; rulesets terraform 0.15.0, azurerm 0.32.0, aws 0.49.0, google 0.40.0 | Latest releases. |
| checkov | 3.3.19 | Latest release. |
| trivy | 0.74.0 | Latest release. |

Kubernetes versions: EKS defaults to **1.36**, the newest version in standard
support (until 2027-08-02). AKS defaults to **1.35**, which this subscription
validated on 2026-09-13 (`deploy/AZURE-PROVISIONING.md`). GKE follows the
**REGULAR** release channel.

## Security defaults, and where they are enforced

- **No public database endpoint anywhere.** Azure uses a delegated subnet with
  `public_network_access_enabled = false`. RDS has `publicly_accessible =
  false`, sits in isolated subnets with no internet route, and accepts
  connections only from the node security groups. Cloud SQL has no IPv4
  address and uses a private IP over private services access. TLS is required
  on all three (`require_secure_transport`, `rds.force_ssl=1`,
  `ENCRYPTED_ONLY`), and Nexus connects with `sslmode=require`.
- **Encryption at rest everywhere.** Azure uses platform-managed keys, with
  infrastructure double encryption on the storage account. AWS uses one
  customer-managed KMS key, rotated yearly, for RDS, S3, ECR, Secrets Manager,
  CloudWatch Logs and the gp3 StorageClass; EKS Secrets use the module's own
  KMS key. GCP uses Google-managed keys. CMK on Azure and GCP is a documented
  choice, not an oversight (see `.checkov.yaml`).
- **Least-privilege identities.** The AKS kubelet identity is pre-created and
  has only `AcrPull`. EKS nodes use IMDSv2 with a hop limit of 1, so pods
  cannot borrow the node role. GKE nodes run as a dedicated service account
  with log, metric and registry read roles, never as the Compute default
  account. Every artifact identity can reach exactly one bucket or container.
  The deployer gets `Key Vault Secrets Officer` on the one vault.
- **Enforced NetworkPolicy.** Azure CNI overlay with Cilium, the VPC CNI with
  `enableNetworkPolicy`, and GKE Dataplane V2. The chart's NATS policy is only
  real if the CNI enforces it.
- **Guarded control plane.** Each module either makes the API server private
  or requires an allow-list, and a validation refuses an empty list or
  `0.0.0.0/0`. AKS has local accounts disabled and uses Entra ID with Azure
  RBAC. EKS uses access entries (API mode). GKE uses private nodes.
- **Protection from deletion.** RDS and Cloud SQL use `deletion_protection`,
  and the GKE cluster uses `deletion_protection`. The Azure database, storage
  account and vault get `CanNotDelete` management locks. Key Vault has purge
  protection. The EKS cluster has deletion protection. Buckets are versioned
  and do not `force_destroy`.
- **Tags and labels** on every resource: `app.kubernetes.io/part-of`,
  `managed-by`, `nexus-environment`, plus your own `tags`/`labels`.

`make tf-lint` runs tflint, checkov and trivy. Every accepted finding sits in
`.tflint.hcl`, `.checkov.yaml` or `.trivyignore` with its reason.

## The two-phase apply

Each environment configures its `kubernetes` and `helm` providers from the
cluster it creates. Terraform cannot configure a provider from a resource
that does not exist yet, so **the first apply is split**:

```sh
terraform init -backend-config=backend.hcl
terraform apply -target=module.platform     # cloud resources only
terraform apply                             # StorageClass, device plugin, Nexus
```

Every later apply is a single `terraform apply`. HashiCorp recommends separate
states for cluster and workloads. This layout keeps one state per environment
for simplicity. If you split it, the platform outputs are the interface.

## Rotating credentials

- **Database password.** Increase `postgres_password_version` by one and run
  apply. A new password is generated, and the same apply writes it to the
  server, the secret store (Key Vault, Secrets Manager or Secret Manager) and
  the kgf-nexus credentials Secret. Then restart the Nexus Deployments, which
  read the Secret only at start. Connections fail between those two steps, so
  do this in a maintenance window.
- **S3 key (EKS).** `-replace=module.platform.aws_iam_access_key.artifacts[0]`.
- **GCS HMAC key (GKE).** Create the new key, roll the Secret, then deactivate
  and delete the old key. A plain `-replace` does it in one step, with a short
  window of failed uploads while pods restart.

## Running the checks

```sh
make tf-validate    # offline: fmt, init -backend=false, validate, mocked-plan tests
make tf-lint        # tflint + checkov + trivy in pinned containers (Docker)
```

These checks need `terraform` 1.11 or later, or Docker, in which case the
pinned `hashicorp/terraform` image is used. They also need network access to
the Terraform registry for providers and modules. Set `TF_PLUGIN_CACHE_DIR`
to reuse downloads. CI runs both targets: the `terraform` job in
`.github/workflows/ci.yml` and the `Terraform` stage in
`.azure-pipelines/pr-build-validation.yml`.
