# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

variable "name" {
  description = "Short environment name, the prefix of every resource (e.g. kgf-nexus-prod-use1)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,30}[a-z0-9]$", var.name))
    error_message = "name must be 4-32 characters of lowercase letters, digits and hyphens, starting with a letter."
  }
}

variable "tags" {
  description = "Tags on every resource (cost centre, owner, environment)."
  type        = map(string)
  default     = {}
}

# ── Network ───────────────────────────────────────────────────────────────

variable "vpc_cidr" {
  description = "VPC CIDR, a /16. Carved into 3 public /22, 3 private /19 and 3 database /24 subnets, one of each per AZ."
  type        = string
  default     = "10.70.0.0/16"

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0)) && endswith(var.vpc_cidr, "/16")
    error_message = "vpc_cidr must be a valid /16 (the subnet layout is computed from it)."
  }
}

variable "nat_gateway_per_az" {
  description = "One NAT gateway per AZ (survives an AZ loss; ~3x the NAT cost) instead of a single shared one."
  type        = bool
  default     = true
}

# ── Cluster ───────────────────────────────────────────────────────────────

variable "kubernetes_version" {
  description = "EKS Kubernetes minor version. 1.36 is the newest in standard support (until 2027-08-02)."
  type        = string
  default     = "1.36"
}

variable "endpoint_public_access" {
  description = "Expose the API server publicly (restricted to endpoint_public_access_cidrs). false = private endpoint only; Terraform and kubectl must then run inside the VPC."
  type        = bool
  default     = true
}

variable "endpoint_public_access_cidrs" {
  description = "CIDRs allowed to reach a public API endpoint. Refused empty when the endpoint is public."
  type        = list(string)
  default     = []

  validation {
    condition = !var.endpoint_public_access || (
      length(var.endpoint_public_access_cidrs) > 0 && !contains(var.endpoint_public_access_cidrs, "0.0.0.0/0")
    )
    error_message = "A public endpoint needs endpoint_public_access_cidrs, and not 0.0.0.0/0; or set endpoint_public_access = false."
  }
}

variable "admin_principal_arns" {
  description = "IAM role/user ARNs granted cluster admin through EKS access entries (e.g. an SSO admin role)."
  type        = list(string)
  default     = []
}

variable "cluster_creator_admin" {
  description = "Grant the identity running Terraform cluster admin, which the environment root needs to install the chart. Turn off once admin_principal_arns covers it."
  type        = bool
  default     = true
}

variable "cpu_node_group" {
  description = "Managed node group for Nexus and its dependencies."
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
  description = "Optional GPU managed node group (AL2023 NVIDIA AMI), tainted nvidia.com/gpu=present:NoSchedule. min_size 0 costs nothing until scaled."
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

variable "log_retention_days" {
  description = "CloudWatch retention for control-plane, flow and database logs. A year: audit logs are what an incident review needs."
  type        = number
  default     = 365
}

variable "container_insights" {
  description = "Install the amazon-cloudwatch-observability add-on (Container Insights: node/pod metrics and container logs to CloudWatch). Billed per metric and GB ingested."
  type        = bool
  default     = false
}

# ── Database ──────────────────────────────────────────────────────────────

variable "postgres" {
  description = "RDS for PostgreSQL. Major 16 matches the chart's image and the backup scripts' pg_dump."
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

  validation {
    condition     = var.postgres.backup_retention_days >= 1 && var.postgres.backup_retention_days <= 35
    error_message = "postgres.backup_retention_days is the PITR window: 1 to 35 days (0 would disable backups and PITR)."
  }
}

# ── Artifacts ─────────────────────────────────────────────────────────────

variable "nexus_kubernetes_namespace" {
  description = "Kubernetes namespace of the Nexus release; the agents' IAM role trusts only its ServiceAccount."
  type        = string
  default     = "nexus"
}

variable "nexus_service_account" {
  description = "ServiceAccount the IRSA / Pod Identity role trusts. The chart does not create or use one yet (F115)."
  type        = string
  default     = "kgf-nexus"
}

variable "artifact_static_credentials" {
  description = "Create an IAM user + access key for the result store, usable ONLY through this VPC's S3 endpoint. Required today: the Nexus S3 client takes static keys only (F115). Set false once the chart and client use the IRSA / Pod Identity role."
  type        = bool
  default     = true
}

variable "artifact_noncurrent_version_days" {
  description = "Days a superseded or deleted artifact version is kept (recovery window for accidental deletes)."
  type        = number
  default     = 30
}

variable "ecr_repositories" {
  description = "Image repositories under kgf-nexus/ (the chart's repositoryPrefix): every component the chart runs."
  type        = list(string)
  default     = ["api-gateway", "workflow-manager", "dag-engine", "scheduler", "resource-manager", "event-service", "agent", "migrate", "ui"]
}

variable "postgres_password_version" {
  description = "Bump to rotate the database password: a new one is generated and written to the server, the secret store and (through the environment) the kgf-nexus credentials Secret in one apply. Restart the Nexus pods afterwards."
  type        = number
  default     = 1
}

variable "cluster_deletion_protection" {
  description = "EKS deletion protection: the cluster cannot be deleted until this is set false and applied."
  type        = bool
  default     = true
}
