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
