# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

variable "namespace" {
  description = "Kubernetes namespace for the release. One Nexus release per namespace."
  type        = string
}

variable "release_name" {
  description = "Helm release name."
  type        = string
  default     = "kgf-nexus"
}

variable "chart_path" {
  description = "Path to the kgf-nexus Helm chart."
  type        = string
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

variable "helm_timeout_seconds" {
  description = "How long Helm waits for every workload and Job to become ready."
  type        = number
  default     = 900
}
