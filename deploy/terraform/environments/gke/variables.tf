# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# ── GCP ───────────────────────────────────────────────────────────────────

variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "region" {
  description = "Region for the regional cluster and every regional service."
  type        = string
}

variable "name" {
  description = "Environment name, the prefix of every resource (e.g. kgf-nexus-prod-usc1). At most 24 characters."
  type        = string
}

variable "labels" {
  description = "Labels on every resource that accepts them."
  type        = map(string)
  default     = {}
}

variable "enable_apis" {
  description = "Enable the project APIs the platform needs."
  type        = bool
  default     = true
}

variable "release_channel" {
  description = "GKE release channel."
  type        = string
  default     = "REGULAR"
}

variable "private_endpoint" {
  description = "Control plane on its private IP only; Terraform then reaches it through the IAM-authorised DNS endpoint."
  type        = bool
  default     = false
}

variable "master_authorized_cidrs" {
  description = "CIDRs allowed to reach the control plane's IP endpoint (operators, CI egress)."
  type        = list(string)
  default     = []
}

variable "cpu_pool" {
  description = "CPU node pool; counts are per zone."
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
  description = "Optional GPU node pool; counts are per zone. See F115: the chart cannot yet place agents on it."
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

variable "postgres" {
  description = "Cloud SQL settings (see the module for defaults)."
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
}

variable "deletion_protection" {
  description = "Protect the cluster and database from destroy."
  type        = bool
  default     = true
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
  description = "Immutable image tag (git SHA) pushed to this environment's Artifact Registry."
  type        = string
}

variable "nats_credentials_dir" {
  description = "Directory written on an admin machine by `deploy/nats/nexus-nats-auth.sh init --tenant <nexus_namespace>` (docs/OPERATIONS.md §11.7)."
  type        = string
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
