# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

variable "kube_context" {
  description = "kubeconfig context of the local cluster."
  type        = string
  default     = "docker-desktop"

  # The dev environment runs simulated hardware and dev-only services
  # (Mailpit). Pointing it at a real cluster by mistake must be impossible, not
  # merely unlikely.
  validation {
    condition     = var.kube_context == "docker-desktop" || startswith(var.kube_context, "kind-")
    error_message = "The dev environment only targets a local cluster: docker-desktop or kind-*."
  }
}

variable "kubeconfig_path" {
  description = "Path to the kubeconfig."
  type        = string
  default     = "~/.kube/config"
}

variable "image_tag" {
  description = "Image tag printed by deploy/scripts/k8s-dev-images.sh (make k8s-dev-images)."
  type        = string
}

variable "namespace" {
  description = "Kubernetes namespace for the dev environment."
  type        = string
  default     = "nexus-dev"
}

# Hostnames served on port 80 through the Gateway API. `.localhost` names are
# reserved (RFC 6761) and resolve to this machine in browsers and on macOS with
# no DNS record and no /etc/hosts edit, and cannot collide with a real domain.
variable "hostnames" {
  description = "Hostnames for the console, API and Mailpit, served on http://<name>."
  type = object({
    console = string
    api     = string
    mail    = string
  })
  default = {
    console = "nexus.localhost"
    api     = "api.nexus.localhost"
    mail    = "mail.nexus.localhost"
  }
}

variable "envoy_gateway_version" {
  description = "Envoy Gateway Helm chart version (the Gateway API implementation)."
  type        = string
  default     = "1.9.1"
}

variable "nats_credentials_dir" {
  description = "NATS NKey identities written by deploy/nats/nexus-nats-auth.sh (make k8s-dev-up does it). Seeds end up in Terraform state, like every other generated credential here."
  type        = string
  default     = "../../../../.dev-nats"
}

# Host ports for Docker Desktop's localhost LoadBalancer. Chosen to avoid the
# compose stack's (3000, 8090, 9091, 8025) so both can run side by side.
variable "ports" {
  description = "localhost ports for the console, REST API, worker gRPC and Mailpit."
  type = object({
    ui      = number
    api     = number
    grpc    = number
    mailpit = number
  })
  default = {
    ui      = 3100
    api     = 8190
    grpc    = 9191
    mailpit = 8125
  }
}

# The HOST machine's PostgreSQL (make k8s-dev-up runs
# deploy/scripts/dev-host-postgres.sh to create the role and database). Pods
# reach the workstation at host.docker.internal; no Postgres runs in-cluster.
variable "postgres" {
  description = "External (host) PostgreSQL the dev platform uses instead of an in-cluster StatefulSet."
  type = object({
    host     = optional(string, "host.docker.internal")
    port     = optional(number, 5432)
    user     = optional(string, "nexus_dev")
    database = optional(string, "nexus_dev")
    sslmode  = optional(string, "disable")
  })
  default = {}
}

variable "postgres_password" {
  description = "Password of var.postgres.user, from .dev-postgres/password (TF_VAR_postgres_password)."
  type        = string
  sensitive   = true
}

variable "kgf" {
  description = "KGF Cloud integration (the chart's kgf block). Standalone by default. To run this engine behind a local KGF platform, set authMode = \"kgf\", contextKeysUrl to kgf-gateway's and the KGF.Nexus front's /.well-known/kgf-context.json (comma-separated), audience = \"kgf-nexus,kgf-nexus-engine\" and frontIssuers = \"kgf-nexus\" (docs/OPERATIONS.md §24.11)."
  type = object({
    authMode          = string
    contextKeysUrl    = optional(string, "")
    contextPublicKeys = optional(string, "")
    audience          = optional(string, "kgf-nexus")
    userIssuers       = optional(string, "kgf-gateway")
    frontIssuers      = optional(string, "")
    servicePrincipals = optional(string, "")
  })
  default = {
    authMode = "standalone"
  }

  validation {
    condition     = contains(["standalone", "kgf", "both"], var.kgf.authMode)
    error_message = "kgf.authMode must be standalone, kgf or both (the chart's values.schema.json)."
  }
}

variable "workspace_worker_namespaces" {
  description = "KGF.Nexus workspace namespaces (kgf-...) to run local agents for. Needs authMode kgf and namespaces KGF has provisioned. Kept out of git in local.tfvars here, which make k8s-dev-up passes when it exists."
  type        = list(string)
  default     = []

  # The derived KGF.Nexus namespace (OPERATIONS §24.11), and what make
  # k8s-dev-up matches to create each one's NATS identity.
  validation {
    condition     = alltrue([for ns in var.workspace_worker_namespaces : can(regex("^kgf-[0-9a-f]{32}$", ns))])
    error_message = "Each workspace namespace is kgf- followed by 32 lowercase hex characters."
  }
}
