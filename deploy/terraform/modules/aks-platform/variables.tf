# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# ── Identity and placement ────────────────────────────────────────────────

variable "name" {
  description = "Short environment name, used as the prefix of every resource (e.g. kgf-nexus-prod-cin). Lowercase letters, digits and hyphens."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,30}[a-z0-9]$", var.name))
    error_message = "name must be 4-32 characters of lowercase letters, digits and hyphens, starting with a letter."
  }
}

variable "location" {
  description = "Azure region (e.g. centralindia). It must offer availability zones for the zonal settings below."
  type        = string
}

variable "tags" {
  description = "Tags applied to every resource that accepts them (cost centre, owner, environment)."
  type        = map(string)
  default     = {}
}

variable "zones" {
  description = "Availability zones for node pools and the database standby."
  type        = list(string)
  default     = ["1", "2", "3"]
}

# ── Network ───────────────────────────────────────────────────────────────

variable "vnet_cidr" {
  description = "Address space of the VNet. Pods do not consume it (Azure CNI overlay)."
  type        = string
  default     = "10.60.0.0/16"
}

variable "subnet_cidrs" {
  description = "Subnets: AKS nodes, the delegated PostgreSQL subnet, and private endpoints (Key Vault, Blob)."
  type = object({
    nodes             = string
    postgres          = string
    private_endpoints = string
  })
  default = {
    nodes             = "10.60.0.0/20"
    postgres          = "10.60.16.0/24"
    private_endpoints = "10.60.17.0/24"
  }
}

variable "pod_cidr" {
  description = "Overlay pod CIDR. Must not overlap the VNet, peered networks or service_cidr."
  type        = string
  default     = "192.168.0.0/16"
}

variable "service_cidr" {
  description = "Kubernetes Service CIDR. Must not overlap the VNet or pod_cidr."
  type        = string
  default     = "172.16.0.0/16"
}

variable "dns_service_ip" {
  description = "ClusterIP of CoreDNS, inside service_cidr."
  type        = string
  default     = "172.16.0.10"
}

# ── Cluster ───────────────────────────────────────────────────────────────

variable "kubernetes_version" {
  description = "AKS Kubernetes minor version (patches follow automatic_upgrade_channel). 1.35 is the version this subscription validated on 2026-09-13 (deploy/AZURE-PROVISIONING.md)."
  type        = string
  default     = "1.35"
}

variable "sku_tier" {
  description = "AKS control-plane tier. Standard carries the uptime SLA and is the production choice; Free has no SLA."
  type        = string
  default     = "Standard"

  validation {
    condition     = contains(["Free", "Standard", "Premium"], var.sku_tier)
    error_message = "sku_tier must be Free, Standard or Premium."
  }
}

variable "private_cluster_enabled" {
  description = "API server reachable only from the VNet (and peered networks). Terraform, kubectl and CI must then run from inside the network (a jump host, a self-hosted agent, or Bastion)."
  type        = bool
  default     = false
}

variable "api_server_authorized_ip_ranges" {
  description = "CIDRs allowed to reach a PUBLIC API server. Ignored for a private cluster. An empty list on a public cluster means the whole internet; the validation refuses that."
  type        = list(string)
  default     = []

  validation {
    condition     = var.private_cluster_enabled || length(var.api_server_authorized_ip_ranges) > 0
    error_message = "A public API server needs api_server_authorized_ip_ranges (e.g. the CI egress and operator CIDRs); set private_cluster_enabled = true otherwise."
  }
}

variable "admin_group_object_ids" {
  description = "Microsoft Entra group object IDs granted cluster-admin through Azure RBAC. Local accounts are disabled, so this is how people get in."
  type        = list(string)
}

variable "system_pool" {
  description = "System node pool: cluster add-ons only (CriticalAddonsOnly taint). Microsoft requires at least 2 nodes of 4 vCPUs, and no B-series."
  type = object({
    vm_size   = string
    min_count = number
    max_count = number
  })
  default = {
    vm_size   = "Standard_D4as_v5"
    min_count = 2
    max_count = 3
  }

  validation {
    condition     = var.system_pool.min_count >= 2 && var.system_pool.max_count >= var.system_pool.min_count
    error_message = "system_pool needs min_count >= 2 (Microsoft's production guidance) and max_count >= min_count."
  }
}

variable "cpu_pool" {
  description = "User node pool for the Nexus control plane, agents, NATS and (on AKS today) MinIO."
  type = object({
    vm_size   = string
    min_count = number
    max_count = number
  })
  default = {
    vm_size   = "Standard_D4as_v5"
    min_count = 2
    max_count = 6
  }
}

variable "gpu_pool" {
  description = "Optional GPU user pool, tainted nvidia.com/gpu=present:NoSchedule so only pods that tolerate it land there. min_count 0 lets the autoscaler park it at no compute cost."
  type = object({
    enabled   = bool
    vm_size   = string
    min_count = number
    max_count = number
  })
  default = {
    enabled   = false
    vm_size   = "Standard_NC4as_T4_v3"
    min_count = 0
    max_count = 2
  }
}

variable "host_encryption_enabled" {
  description = "Encrypt node temp disks and OS/data disk caches at the host. Needs the EncryptionAtHost feature registered on the subscription (az feature register --namespace Microsoft.Compute --name EncryptionAtHost). Managed disks are encrypted at rest either way."
  type        = bool
  default     = false
}

# ── Registry ──────────────────────────────────────────────────────────────

variable "acr_sku" {
  description = "Azure Container Registry SKU. Premium adds zone redundancy, retention of untagged manifests and network rules."
  type        = string
  default     = "Premium"

  validation {
    condition     = contains(["Basic", "Standard", "Premium"], var.acr_sku)
    error_message = "acr_sku must be Basic, Standard or Premium."
  }
}

variable "acr_public_network_access_enabled" {
  description = "Keep the registry reachable from the internet (authenticated). CI pushes with Entra ID; turn this off only when pushes come from inside the VNet through a private endpoint."
  type        = bool
  default     = true
}

# ── PostgreSQL Flexible Server ────────────────────────────────────────────

variable "postgres" {
  description = "Azure Database for PostgreSQL Flexible Server. Major 16 matches the chart's own image and the pg_dump the backup scripts run."
  type = object({
    version               = string
    sku_name              = string
    storage_mb            = number
    storage_tier          = string
    backup_retention_days = number
    geo_redundant_backup  = bool
    high_availability     = bool
  })
  default = {
    version               = "16"
    sku_name              = "GP_Standard_D2ds_v5"
    storage_mb            = 65536
    storage_tier          = "P6"
    backup_retention_days = 14
    geo_redundant_backup  = false
    high_availability     = true
  }

  validation {
    condition     = var.postgres.backup_retention_days >= 7 && var.postgres.backup_retention_days <= 35
    error_message = "postgres.backup_retention_days is the point-in-time-restore window: 7 to 35 days."
  }
}

# ── Artifacts (Blob) ──────────────────────────────────────────────────────

variable "storage_replication_type" {
  description = "Replication of the artifact storage account. ZRS survives a zone loss; GZRS a region loss too."
  type        = string
  default     = "ZRS"
}

variable "blob_soft_delete_days" {
  description = "Days a deleted blob or container stays recoverable."
  type        = number
  default     = 30
}

variable "blob_noncurrent_version_days" {
  description = "Days a noncurrent (overwritten or deleted) blob version is kept before the lifecycle policy removes it. Matches the S3 and GCS noncurrent expiry."
  type        = number
  default     = 30
  validation {
    condition     = var.blob_noncurrent_version_days >= 1
    error_message = "blob_noncurrent_version_days must be at least 1."
  }
}

# ── Key Vault ─────────────────────────────────────────────────────────────

variable "key_vault_allowed_ip_ranges" {
  description = "Public CIDRs allowed through the Key Vault firewall (where Terraform runs). In-cluster access goes through the private endpoint."
  type        = list(string)
  default     = []
}

variable "delete_locks_enabled" {
  description = "CanNotDelete locks on the database, storage account and Key Vault. Set false and apply before a deliberate destroy."
  type        = bool
  default     = true
}

variable "log_retention_days" {
  description = "Log Analytics retention for cluster and database logs."
  type        = number
  default     = 30
}

variable "nexus_kubernetes_namespace" {
  description = "Kubernetes namespace the Nexus release will run in; the workload-identity federation is scoped to it."
  type        = string
  default     = "nexus"
}

variable "nexus_service_account" {
  description = "Kubernetes ServiceAccount the workload identity federates: the one the result-store pods run as (modules/kgf-nexus creates it)."
  type        = string
  default     = "kgf-nexus"
}

variable "postgres_password_version" {
  description = "Bump to rotate the database password: a new one is generated and written to the server, the secret store and (through the environment) the kgf-nexus credentials Secret in one apply. Restart the Nexus pods afterwards."
  type        = number
  default     = 1
}
