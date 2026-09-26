# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# ── AWS ───────────────────────────────────────────────────────────────────

variable "region" {
  description = "AWS region."
  type        = string
}

variable "allowed_account_ids" {
  description = "The only AWS account(s) this environment may touch; the provider refuses any other credentials."
  type        = list(string)
}

variable "name" {
  description = "Environment name, the prefix of every resource (e.g. kgf-nexus-prod-use1)."
  type        = string
}

variable "tags" {
  description = "Tags on every AWS resource (applied as provider default_tags too)."
  type        = map(string)
  default     = {}
}

variable "vpc_cidr" {
  description = "VPC /16."
  type        = string
  default     = "10.70.0.0/16"
}

variable "nat_gateway_per_az" {
  description = "One NAT gateway per AZ (production) or a single shared one (cheaper, not AZ-resilient)."
  type        = bool
  default     = true
}

variable "kubernetes_version" {
  description = "EKS Kubernetes minor version."
  type        = string
  default     = "1.36"
}

variable "endpoint_public_access" {
  description = "Public API endpoint restricted to endpoint_public_access_cidrs; false = private only."
  type        = bool
  default     = true
}

variable "endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach the public API endpoint."
  type        = list(string)
  default     = []
}

variable "admin_principal_arns" {
  description = "IAM principals granted cluster admin via access entries."
  type        = list(string)
  default     = []
}

variable "cpu_node_group" {
  description = "CPU managed node group."
  type = object({
    instance_types = list(string)
    min_size       = number
    max_size       = number
    desired_size   = number
    disk_size_gb   = number
  })
  default = {
    instance_types = ["m7i.xlarge"]
    min_size       = 3
    max_size       = 6
    desired_size   = 3
    disk_size_gb   = 100
  }
}

variable "gpu_node_group" {
  description = "Optional GPU managed node group (tainted). See F115: the chart cannot yet place agents on it."
  type = object({
    enabled        = bool
    instance_types = list(string)
    min_size       = number
    max_size       = number
    desired_size   = number
    disk_size_gb   = number
  })
  default = {
    enabled        = false
    instance_types = ["g6.xlarge"]
    min_size       = 0
    max_size       = 2
    desired_size   = 0
    disk_size_gb   = 200
  }
}

variable "container_insights" {
  description = "Install the CloudWatch Observability add-on (Container Insights)."
  type        = bool
  default     = false
}

variable "postgres" {
  description = "RDS for PostgreSQL settings (see the module for defaults)."
  type = object({
    engine_version        = string
    instance_class        = string
    allocated_storage_gb  = number
    max_storage_gb        = number
    multi_az              = bool
    backup_retention_days = number
    deletion_protection   = bool
  })
  default = {
    engine_version        = "16"
    instance_class        = "db.m7g.large"
    allocated_storage_gb  = 50
    max_storage_gb        = 500
    multi_az              = true
    backup_retention_days = 14
    deletion_protection   = true
  }
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
  description = "Immutable image tag (git SHA) pushed to this environment's ECR repositories."
  type        = string
}

variable "nats_credentials_dir" {
  description = "Directory written on an admin machine by `deploy/nats/nexus-nats-auth.sh init --tenant <nexus_namespace>` (docs/OPERATIONS.md §11.7)."
  type        = string
}

variable "nvidia_device_plugin_version" {
  description = "NVIDIA k8s-device-plugin Helm chart version, installed only with the GPU node group."
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
  description = "Services connect as their own least-privilege logins, created by the chart's db-roles Job; the managed server's admin login then reaches only the migrate, bootstrap and db-roles Jobs (docs/OPERATIONS.md §20.9)."
  type        = bool
  default     = true
}

variable "runtime_db_password_version" {
  description = "Bump to rotate every runtime login's password in one apply (the db-roles Job re-runs and the pods roll)."
  type        = number
  default     = 1
}

variable "gpu_agent" {
  description = "The Nexus agent placed on the GPU pool when it is enabled: what it advertises (gpu_count devices of gpu_kind, and CPU/memory) and requests from the device plugin. Size it inside one GPU node (default: g6.xlarge: 4 vCPU, 16 GiB, one L4)."
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
    gpu_kind       = "nvidia-l4"
    gpu_memory_gib = 24
    cpu_millis     = 3000
    memory_gib     = 10
    disk_gib       = 150
  }

  validation {
    condition     = var.gpu_agent.gpu_count >= 1 && var.gpu_agent.replicas >= 0
    error_message = "gpu_agent.gpu_count must be at least 1 (a GPU agent requests its devices) and replicas non-negative."
  }
}
