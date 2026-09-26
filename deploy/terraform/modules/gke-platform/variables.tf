# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

variable "name" {
  description = "Short environment name, the prefix of every resource (e.g. kgf-nexus-prod-usc1)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,22}[a-z0-9]$", var.name))
    error_message = "name must be 4-24 characters of lowercase letters, digits and hyphens (service-account IDs derived from it are limited to 30)."
  }
}

variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "Region for the regional cluster, Cloud SQL, Artifact Registry and the bucket."
  type        = string
}

variable "labels" {
  description = "Labels on every resource that accepts them (cost centre, owner, environment). Lowercase keys and values."
  type        = map(string)
  default     = {}
}

variable "enable_apis" {
  description = "Enable the project APIs this module needs. Turn off where a platform team manages project services."
  type        = bool
  default     = true
}

# ── Network ───────────────────────────────────────────────────────────────

variable "subnet_cidr" {
  description = "Primary range of the node subnet."
  type        = string
  default     = "10.80.0.0/20"
}

variable "pods_cidr" {
  description = "Secondary range for pods (VPC-native)."
  type        = string
  default     = "10.84.0.0/14"
}

variable "services_cidr" {
  description = "Secondary range for Services."
  type        = string
  default     = "10.88.0.0/20"
}

variable "private_services_cidr_prefix" {
  description = "Prefix length of the range reserved for private services access (Cloud SQL private IP)."
  type        = number
  default     = 20
}

# ── Cluster ───────────────────────────────────────────────────────────────

variable "release_channel" {
  description = "GKE release channel; versions and patches follow it. REGULAR is the production default."
  type        = string
  default     = "REGULAR"

  validation {
    condition     = contains(["RAPID", "REGULAR", "STABLE", "EXTENDED"], var.release_channel)
    error_message = "release_channel must be RAPID, REGULAR, STABLE or EXTENDED."
  }
}

variable "private_endpoint" {
  description = "Control plane reachable only on its private IP (plus the IAM-authorised DNS endpoint when dns_endpoint_external is true)."
  type        = bool
  default     = false
}

variable "master_authorized_cidrs" {
  description = "CIDRs allowed to reach the control plane's IP endpoint. Refused empty for a public endpoint."
  type        = list(string)
  default     = []

  validation {
    condition = var.private_endpoint || (
      length(var.master_authorized_cidrs) > 0 && !contains(var.master_authorized_cidrs, "0.0.0.0/0")
    )
    error_message = "A public control-plane endpoint needs master_authorized_cidrs, and not 0.0.0.0/0; or set private_endpoint = true."
  }
}

variable "dns_endpoint_external" {
  description = "Allow the control plane's DNS-based endpoint from outside the VPC. Every request there is authorised by IAM, which makes it the recommended way for CI to reach a private cluster."
  type        = bool
  default     = true
}

variable "cpu_pool" {
  description = "Node pool for Nexus and its dependencies. Counts are PER ZONE (a regional pool spans three zones)."
  type = object({
    machine_type = string
    min_per_zone = number
    max_per_zone = number
    disk_size_gb = number
  })
  default = {
    machine_type = "n2d-standard-4"
    min_per_zone = 1
    max_per_zone = 3
    disk_size_gb = 100
  }
}

variable "gpu_pool" {
  description = "Optional GPU node pool; GKE installs the driver and taints the nodes nvidia.com/gpu. Counts are per zone; min 0 costs nothing until scaled."
  type = object({
    enabled           = bool
    machine_type      = string
    accelerator_type  = string
    accelerator_count = number
    min_per_zone      = number
    max_per_zone      = number
    disk_size_gb      = number
    zones             = list(string)
  })
  default = {
    enabled           = false
    machine_type      = "g2-standard-8"
    accelerator_type  = "nvidia-l4"
    accelerator_count = 1
    min_per_zone      = 0
    max_per_zone      = 1
    disk_size_gb      = 200
    zones             = []
  }
}

variable "deletion_protection" {
  description = "Refuse to destroy the cluster and database until this is set false and applied."
  type        = bool
  default     = true
}

# ── Database ──────────────────────────────────────────────────────────────

variable "postgres" {
  description = "Cloud SQL for PostgreSQL. POSTGRES_16 matches the chart's image and the backup scripts' pg_dump."
  type = object({
    database_version   = string
    tier               = string
    disk_size_gb       = number
    high_availability  = bool
    backup_retention   = number
    log_retention_days = number
  })
  default = {
    database_version   = "POSTGRES_16"
    tier               = "db-custom-2-7680"
    disk_size_gb       = 50
    high_availability  = true
    backup_retention   = 14
    log_retention_days = 7
  }

  validation {
    condition     = var.postgres.log_retention_days >= 1 && var.postgres.log_retention_days <= 7
    error_message = "postgres.log_retention_days is the PITR window: 1 to 7 days on the Enterprise edition."
  }
}

# ── Artifacts ─────────────────────────────────────────────────────────────

variable "nexus_kubernetes_namespace" {
  description = "Kubernetes namespace of the Nexus release (Workload Identity binding for the future native path)."
  type        = string
  default     = "nexus"
}

variable "nexus_service_account" {
  description = "Kubernetes ServiceAccount bound to the artifact service account through Workload Identity. The environments pass the chart's shared result-store account (nexus-artifacts), given to api-gateway, the agents and artifact-gc."
  type        = string
  default     = "kgf-nexus"
}

variable "artifact_noncurrent_version_days" {
  description = "Days a deleted artifact version stays recoverable."
  type        = number
  default     = 30
}

variable "postgres_password_version" {
  description = "Bump to rotate the database password: a new one is generated and written to the server, the secret store and (through the environment) the kgf-nexus credentials Secret in one apply. Restart the Nexus pods afterwards."
  type        = number
  default     = 1
}
