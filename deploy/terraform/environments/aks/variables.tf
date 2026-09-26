# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# ── Azure ─────────────────────────────────────────────────────────────────

variable "subscription_id" {
  description = "Azure subscription to deploy into. Explicit, so a stale `az account set` cannot redirect an apply."
  type        = string
}

variable "name" {
  description = "Environment name, the prefix of every resource (e.g. kgf-nexus-prod-cin)."
  type        = string
}

variable "location" {
  description = "Azure region."
  type        = string
}

variable "tags" {
  description = "Tags on every Azure resource."
  type        = map(string)
  default     = {}
}

variable "admin_group_object_ids" {
  description = "Entra ID groups granted cluster admin (Azure RBAC)."
  type        = list(string)
}

variable "private_cluster_enabled" {
  description = "Private API server. Terraform must then run from inside the VNet."
  type        = bool
  default     = false
}

variable "api_server_authorized_ip_ranges" {
  description = "CIDRs allowed to reach a public API server (operators, CI egress)."
  type        = list(string)
  default     = []
}

variable "key_vault_allowed_ip_ranges" {
  description = "Public CIDRs allowed through the Key Vault firewall (where Terraform runs)."
  type        = list(string)
  default     = []
}

variable "kubernetes_version" {
  description = "AKS Kubernetes minor version."
  type        = string
  default     = "1.35"
}

variable "sku_tier" {
  description = "AKS tier: Standard (SLA) for production, Free for a throwaway test."
  type        = string
  default     = "Standard"
}

variable "system_pool" {
  description = "System node pool (see the module for defaults)."
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
}

variable "cpu_pool" {
  description = "User node pool for Nexus."
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
  description = "Optional GPU pool (tainted). See F115: the chart cannot yet place agents on it."
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

variable "postgres" {
  description = "PostgreSQL Flexible Server settings (see the module for defaults)."
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
}

variable "premium_disk_sku" {
  description = "Disk SKU of the nexus-premium-ssd StorageClass. Premium_ZRS survives a zone loss (the disk re-attaches in another zone); Premium_LRS where ZRS disks are not offered."
  type        = string
  default     = "Premium_ZRS"

  validation {
    condition     = contains(["Premium_LRS", "Premium_ZRS", "PremiumV2_LRS"], var.premium_disk_sku)
    error_message = "premium_disk_sku must be Premium_LRS, Premium_ZRS or PremiumV2_LRS."
  }
}

# ── Object store ──────────────────────────────────────────────────────────

variable "azure_blob_native_backend" {
  description = <<-EOT
    true: Nexus stores results and logs in the provisioned Blob container
    through the native azblob backend (resultStore.backend=azblob), keyless:
    AKS workload identity federates the nexus-artifacts ServiceAccount to the
    user-assigned identity that holds Storage Blob Data Contributor on that one
    container. The in-cluster MinIO is not deployed and the credentials Secret
    holds no S3 key pair.
    false: the in-cluster MinIO on a Premium SSD PVC, as before; you then own
    its capacity, backups and availability.
    Switching an environment that already holds results in MinIO does NOT
    migrate them: copy the bucket into the container first (rclone, azcopy),
    or keep false.
  EOT
  type        = bool
  # Default true. The Blob account is zone-redundant, versioned, soft-deleted,
  # private-endpoint-only and audited, and needs no key; a single MinIO pod on
  # one PVC is none of those. No AKS environment has been applied yet (F115),
  # so there is no MinIO data this default could strand.
  default = true
}

variable "minio_storage_size" {
  description = "Size of the in-cluster MinIO volume (Premium SSD), used ONLY with azure_blob_native_backend = false. It is then the whole artifact store: plan capacity here."
  type        = string
  default     = "200Gi"
}

# ── Nexus ─────────────────────────────────────────────────────────────────

variable "namespace" {
  description = "Kubernetes namespace of the Nexus release."
  type        = string
  default     = "nexus"
}

variable "nexus_namespace" {
  description = "Nexus tenant namespace the agents serve and bootstrap creates."
  type        = string
  default     = "default"
}

variable "image_tag" {
  description = "Immutable image tag (git SHA) pushed to this environment's ACR."
  type        = string
}

variable "nats_credentials_dir" {
  description = "Directory written on an admin machine by `deploy/nats/nexus-nats-auth.sh init --tenant <nexus_namespace>` (docs/OPERATIONS.md §11.7). Required: the chart refuses an unauthenticated broker."
  type        = string
}

variable "nvidia_device_plugin_version" {
  description = "NVIDIA k8s-device-plugin Helm chart version, installed only with the GPU pool (AKS installs the driver, not the plugin)."
  type        = string
  default     = "0.20.1"
}

variable "chart_values_overrides" {
  description = "Chart values deep-merged last (agents, replicas, gateway, SMTP, NATS TLS...)."
  type        = any
  default     = {}
}

variable "postgres_password_version" {
  description = "Bump to rotate the database password: a new one is generated and written to the server, the secret store and (through the environment) the kgf-nexus credentials Secret in one apply. Restart the Nexus pods afterwards."
  type        = number
  default     = 1
}

# ── Database hardening and agent pools (F115) ─────────────────────────────

variable "runtime_db_roles" {
  description = "Services connect as their own least-privilege logins, created by the chart's db-roles Job; the managed server's admin login then reaches only the migrate, bootstrap and db-roles Jobs (docs/OPERATIONS.md §20.12)."
  type        = bool
  default     = true
}

variable "runtime_db_password_version" {
  description = "Bump to rotate every runtime login's password in one apply (the db-roles Job re-runs and the pods roll)."
  type        = number
  default     = 1
}

variable "gpu_agent" {
  description = "The Nexus agent placed on the GPU pool when it is enabled: what it advertises (gpu_count devices of gpu_kind, and CPU/memory) and requests from the device plugin. Size it inside one GPU node (default: Standard_NC4as_T4_v3: 4 vCPU, 28 GiB, one T4)."
  type = object({
    replicas       = number
    gpu_count      = number
    gpu_kind       = string
    gpu_memory_gib = number
    cpu_millis     = number
    memory_gib     = number
    disk_gib       = number
  })
  default = {
    replicas       = 1
    gpu_count      = 1
    gpu_kind       = "nvidia-t4"
    gpu_memory_gib = 16
    cpu_millis     = 3000
    memory_gib     = 20
    disk_gib       = 100
  }

  validation {
    condition     = var.gpu_agent.gpu_count >= 1 && var.gpu_agent.replicas >= 0
    error_message = "gpu_agent.gpu_count must be at least 1 (a GPU agent requests its devices) and replicas non-negative."
  }
}
