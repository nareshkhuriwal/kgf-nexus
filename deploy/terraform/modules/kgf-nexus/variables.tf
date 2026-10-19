# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

variable "namespace" {
  description = "Kubernetes namespace for the release. One Nexus release per namespace."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", var.namespace))
    error_message = "namespace must be a Kubernetes namespace name (RFC 1123 label: lower case, digits and '-', at most 63 characters)."
  }
}

variable "create_namespace" {
  description = "true: create the namespace, labelled for Pod Security (enforce baseline, warn restricted). false: install into a namespace that already exists (an existing cluster whose namespaces a platform team owns); it must allow the baseline profile (the console's nginx starts as root) and is never modified or deleted here."
  type        = bool
  default     = true
}

variable "release_name" {
  description = "Helm release name."
  type        = string
  default     = "kgf-nexus"
}

variable "chart_path" {
  description = "Path to the kgf-nexus Helm chart (a checkout of deploy/helm/kgf-nexus). Leave null when installing the published chart from chart_repository."
  type        = string
  default     = null
}

variable "chart_repository" {
  description = "Repository of the PUBLISHED chart, e.g. oci://ghcr.io/<owner>/charts (the release workflow pushes kgf-nexus there; docs/OPERATIONS.md §22.4). null: install chart_path instead. Exactly one of the two."
  type        = string
  default     = null

  validation {
    condition     = var.chart_repository == null || can(regex("^(oci|https)://[^\\s]+[^/]$", coalesce(var.chart_repository, "x")))
    error_message = "chart_repository must be an oci:// or https:// repository URL without a trailing slash, e.g. oci://ghcr.io/<owner>/charts."
  }
}

variable "chart_version" {
  description = "Exact chart version to install from chart_repository (the chart's Chart.yaml version, e.g. 0.3.0). Required with chart_repository; never a range, so an apply cannot pick up a chart nobody reviewed."
  type        = string
  default     = null

  validation {
    condition     = var.chart_version == null || can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$", coalesce(var.chart_version, "x")))
    error_message = "chart_version must be one exact semantic version (MAJOR.MINOR.PATCH[-PRERELEASE]), not a range."
  }
}

variable "image_tag" {
  description = "Immutable image tag for every Nexus component (a git SHA). Never \"latest\"."
  type        = string

  validation {
    condition     = length(var.image_tag) > 0 && var.image_tag != "latest"
    error_message = "image_tag must be an immutable tag such as a git SHA, not empty or \"latest\"."
  }
}

variable "image_registry" {
  description = "Registry prefix for Nexus images, with trailing slash (\"\" for images loaded into the node)."
  type        = string
  default     = ""
}

variable "image_repository_prefix" {
  description = "Path between the registry and the component name: <image_registry><image_repository_prefix><component>:<tag>. \"kgf-nexus/\" for images built or imported under that path (ACR, a local node); \"<owner>/kgf-nexus/\" for the images the release workflow publishes to ghcr.io."
  type        = string
  default     = "kgf-nexus/"

  validation {
    condition     = var.image_repository_prefix == "" || can(regex("^[a-z0-9]([a-z0-9._/-]*[a-z0-9])?/$", var.image_repository_prefix))
    error_message = "image_repository_prefix must be empty or a lower-case repository path ending in '/', e.g. \"kgf-nexus/\" or \"<owner>/kgf-nexus/\"."
  }
}

variable "image_pull_policy" {
  description = "IfNotPresent for images loaded into a local node; Always is pointless with immutable tags."
  type        = string
  default     = "IfNotPresent"
}

variable "nexus_namespace" {
  description = "The Nexus tenant namespace the agents serve and bootstrap creates."
  type        = string
  default     = "default"
}

variable "nats_credentials_dir" {
  description = "Directory written by `deploy/nats/nexus-nats-auth.sh init --tenant <nexus_namespace>` (NATS NKey identities). Empty: none, and the chart refuses to render an authenticated broker without them."
  type        = string
  default     = ""
}

variable "chart_values" {
  description = "Environment-specific chart values, merged over the chart defaults."
  type        = any
  default     = {}
}

variable "chart_values_overrides" {
  description = "Chart values applied after chart_values (Helm deep-merges them), for per-deployment overrides of what an environment derives from its infrastructure."
  type        = any
  default     = {}
}

variable "postgres_password" {
  description = "Password of an EXTERNAL (managed) database's nexus user, from a cloud platform module. null: generate one for the in-cluster Postgres. Embedded unescaped in a postgres:// URL, so alphanumeric only."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.postgres_password == null || can(regex("^[A-Za-z0-9]{16,}$", coalesce(var.postgres_password, "x")))
    error_message = "postgres_password must be at least 16 alphanumeric characters: it is embedded unescaped in a postgres:// URL."
  }
}

variable "object_store_credentials" {
  description = "Key pair for an EXTERNAL S3-compatible result store (Amazon S3, or GCS through its HMAC interoperability API). null: generate root credentials for the in-cluster MinIO."
  type = object({
    access_key_id     = string
    secret_access_key = string
  })
  default   = null
  sensitive = true
}

variable "postgres_ca_bundle" {
  description = "PEM root CA bundle of an EXTERNAL database, for sslmode=verify-full. Written to the <release>-postgres-ca Secret and mounted by the chart (postgres.caBundle). null: no bundle (the in-cluster Postgres, which serves no TLS)."
  type        = string
  default     = null

  validation {
    condition     = var.postgres_ca_bundle == null || can(regex("-----BEGIN CERTIFICATE-----", coalesce(var.postgres_ca_bundle, "x")))
    error_message = "postgres_ca_bundle must be PEM certificates (-----BEGIN CERTIFICATE-----)."
  }
}

variable "runtime_db_roles" {
  description = "Least-privilege runtime logins (chart postgres.runtimeRoles): a generated password per service login in the <release>-db-logins Secret, created in the database by the chart's db-roles Job as the owner login. The owner password then reaches only the migrate, bootstrap and db-roles Jobs."
  type        = bool
  default     = false
}

variable "runtime_db_password_version" {
  description = "Bump to rotate every runtime login's password: new passwords are generated, the db-roles Job re-runs and the pods roll (chart postgres.runtimeRoles.passwordRevision)."
  type        = number
  default     = 1

  validation {
    condition     = var.runtime_db_password_version >= 1 && floor(var.runtime_db_password_version) == var.runtime_db_password_version
    error_message = "runtime_db_password_version must be a positive integer."
  }
}

variable "generate_object_store_credentials" {
  description = "With object_store_credentials null: true generates root credentials for the in-cluster MinIO; false writes NO S3 key pair, for a store the pods reach with a cloud identity (IRSA, EKS Pod Identity, Azure or GKE workload identity). The identity itself is bound to the chart's ServiceAccounts (chart_values.serviceAccounts.apiGateway / agent / artifactGC), which the environments annotate."
  type        = bool
  default     = true
}

variable "helm_timeout_seconds" {
  description = "How long Helm waits for every workload and Job to become ready."
  type        = number
  default     = 900
}

variable "workspace_worker_namespaces" {
  description = "KGF.Nexus workspace namespaces to give a worker key (chart workspaceWorkers): a development path until KGF issues worker tokens. A namespace must already be provisioned by KGF for its key to be registered."
  type        = list(string)
  default     = []
}
