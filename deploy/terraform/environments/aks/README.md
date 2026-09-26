# KGF Nexus on Azure Kubernetes Service

This root module runs `modules/aks-platform` first, then the in-cluster
pieces, then `modules/kgf-nexus`. It has been **validated offline only**
(`make tf-validate`), not applied. See [../../README.md](../../README.md) for
version choices, the security model and finding F115.

## What it creates

| Layer | Resources |
|---|---|
| Network | VNet with a node subnet, a delegated PostgreSQL subnet (NSG: 5432 from nodes only) and a private-endpoint subnet (NSG). Azure CNI **overlay** with **Cilium** (pods do not use VNet IPs, and NetworkPolicy is enforced). |
| AKS | Standard tier (SLA). Local accounts disabled, with Entra ID and Azure RBAC (`admin_group_object_ids`). OIDC issuer and workload identity enabled. Patch auto-upgrade and NodeImage OS upgrades. Azure Policy, image cleaner, Container Insights (MSI) to Log Analytics, and the Key Vault CSI driver. Optional private API server; otherwise the API server is limited to `api_server_authorized_ip_ranges`. |
| Node pools | `system`: 2-3 × D4as_v5 across zones 1-3, `CriticalAddonsOnly` taint. `cpu`: 2-6 × D4as_v5, which runs Nexus. `gpu` (optional): NC4as_T4_v3, 0-2 nodes, taint `nvidia.com/gpu=present:NoSchedule`, with the driver installed by AKS and the NVIDIA device plugin (0.20.1) installed here. |
| Registry | ACR Premium: admin user off, anonymous pull off, zone redundant, dedicated data endpoints, untagged manifests purged after 7 days. A pre-created kubelet identity holds **AcrPull only**. |
| PostgreSQL | Flexible Server 16 (GP_Standard_D2ds_v5), private access only. TLS is required, connections are logged, and logs go to Log Analytics. Zone-redundant HA is **on** by default. PITR retention is 14 days (7-35). The password is write-only on the server and stored in Key Vault. |
| Artifacts | StorageV2 account on ZRS: no shared keys, public access disabled, TLS 1.2, infrastructure encryption, versioning, 30-day soft delete, a private endpoint, and blob audit logs to Log Analytics. **Not used by Nexus yet**, see below. |
| Key Vault | RBAC authorization, purge protection, 90-day soft delete, firewall with default deny, a private endpoint, and audit logs. Holds `nexus-postgres-password`. |
| Protection | `CanNotDelete` locks on the database, storage account and vault (`delete_locks_enabled`). |
| In cluster | StorageClass `nexus-premium-ssd` (`Premium_ZRS`, Retain, WaitForFirstConsumer, expandable), then the Nexus release. |

### The artifact store on AKS (read this)

Nexus's result store speaks only S3, and Azure Blob has no S3 endpoint. **On
AKS, Nexus uses its in-cluster MinIO**, on a `minio_storage_size` Premium SSD
volume (200 GiB by default). That volume is the whole artifact store, so you
must size it, monitor it and back it up (`make backup-artifacts`,
docs/OPERATIONS.md §7). The Blob account, the container and the workload
identity are provisioned for the native Blob backend, which **does not exist
yet**. `azure_blob_native_backend` is reserved for it and refuses `true`
(F115).

## Prerequisites

- Terraform 1.11 or later, Azure CLI, `kubelogin`, and Helm is **not** needed
  (the helm provider embeds it).
- A signed-in identity with Owner, or with Contributor plus User Access
  Administrator, on the subscription. The module creates role assignments.
- The NATS identities, generated on an admin machine:
  `NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh init --tenant default`
  (docs/OPERATIONS.md §11.7). The chart refuses to run without them.
- Images pushed to the ACR created here, as
  `<registry>/kgf-nexus/<component>:<git-sha>`. Run the release pipeline
  against this ACR, or use `az acr import` / `docker push`.
- For a **private cluster** (`private_cluster_enabled = true`), Terraform,
  kubectl and CI must run from inside the VNet or a peered network (jump host,
  self-hosted agent, Bastion).
- `key_vault_allowed_ip_ranges` must include the IP Terraform runs from, or
  Terraform must run inside the VNet. Otherwise it cannot write the database
  secret.

## Apply

```sh
cp terraform.tfvars.example terraform.tfvars      # fill in
cp backend.hcl.example backend.hcl                # fill in
terraform init -backend-config=backend.hcl
terraform apply -target=module.platform           # first time only: cloud resources
# push images to $(terraform output -raw registry)
terraform apply                                   # StorageClass, GPU plugin, Nexus
```

To destroy, first set `delete_locks_enabled = false` in the platform call and
apply. Deletion protection is deliberate.

## Outputs

| Output | Meaning |
|---|---|
| `kubeconfig_command` | `az aks get-credentials ... && kubelogin convert-kubeconfig -l azurecli` |
| `registry` | ACR login server |
| `database` | Host, port, database, user, `sslmode=verify-full` (pinned CA bundle in `kgf-nexus-postgres-ca`), the runtime logins Secret, the **Key Vault secret ID** of the password, and the Kubernetes Secret key. The password itself is never printed. |
| `object_store` | What stores artifacts today (MinIO) and the unused Blob target |
| `key_vault_uri`, `namespace`, `console_access` | As named |
| `admin_api_key` | Sensitive: `terraform output -raw admin_api_key` |

## Cost note (indicative, list prices, Central India, 730 h/month, before tax)

| Item | Approx. USD/month |
|---|---|
| AKS Standard tier (uptime SLA) | ~73 ($0.10/cluster-hour) |
| System pool, 2 × D4as_v5 ($0.111/h, retail API 2026-09-25) | ~162 |
| CPU pool, 2 × D4as_v5 (min) | ~162, up to ~486 at 6 nodes |
| PostgreSQL GP_Standard_D2ds_v5 + 64 GiB, **zone-redundant HA** | ~250-300 (about half without HA) |
| ACR Premium | ~50 ($1.667/day) |
| Premium SSD: node OS disks, MinIO 200 GiB, NATS 5 GiB (ZRS) | ~60-90 |
| Log Analytics ingestion (Container Insights + audit) | Usage-based, often the surprise. Budget ~$2.3-3/GB. |
| Load balancer, public IP, private endpoints (2), egress | ~20-40 |
| GPU pool (NC4as_T4_v3), when scaled up | ~$0.5-0.6/h per node, $0 at min 0 |
| **Baseline total** | **roughly $850-1,000 per month** |

This is far above the USD 50/month development budget recorded in
`deploy/AZURE-PROVISIONING.md`. For a short test, use `sku_tier = "Free"`,
`high_availability = false`, `acr_sku = "Standard"` (a platform-module input),
and the smallest pools
Microsoft allows (the system pool still needs 2 × 4 vCPU). Stop the cluster
afterwards (`az aks stop`). Disks, the public IP and the database keep
billing while the cluster is stopped. Check prices with the Azure pricing
calculator before applying.
