# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# One KGF Nexus environment: namespace, generated credentials, and the Helm
# release. Environment-specific choices come in through var.chart_values.
#
# Credentials are GENERATED here and only here. Nothing is typed, committed or
# shared between environments. They do live in Terraform state (marked
# sensitive), so state must be stored as carefully as the secrets themselves:
# local and gitignored for dev, an encrypted remote backend anywhere else.

resource "kubernetes_namespace_v1" "this" {
  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
      # Everything Nexus runs as non-root with no capabilities; Postgres and
      # the console's nginx still start as root, hence baseline, not restricted.
      "pod-security.kubernetes.io/enforce" = "baseline"
      "pod-security.kubernetes.io/warn"    = "restricted"
    }
  }
}

# Alphanumeric only: the password is embedded in a postgres:// URL, where a
# special character would need escaping and silently break the connection.
resource "random_password" "postgres" {
  length  = 32
  special = false
}

resource "random_password" "minio" {
  length  = 32
  special = false
}

# API keys in the platform's own format (nxs_<role>_<64 hex>), with the same
# 256 bits of entropy `make seed` uses.
resource "random_id" "worker_key" {
  byte_length = 32
}

resource "random_id" "admin_key" {
  byte_length = 32
}

locals {
  worker_api_key = "nxs_worker_${random_id.worker_key.hex}"
  admin_api_key  = "nxs_admin_${random_id.admin_key.hex}"
  secret_name    = "${var.release_name}-credentials"

  # NATS identities, from a directory deploy/nats/nexus-nats-auth.sh wrote
  # (Terraform cannot mint NKeys itself). Seeds join the credentials Secret;
  # public keys become chart values. Empty dir = no identities, and the chart
  # then refuses to render unless nats.auth.mode=none is set explicitly.
  nats_dir   = var.nats_credentials_dir
  nats_agent = "agent-${var.nexus_namespace}"
  nats_seeds = local.nats_dir == "" ? {} : {
    "nats-event-service-nkey" = trimspace(file("${local.nats_dir}/event-service/user.nk"))
    "nats-agent-nkey"         = trimspace(file("${local.nats_dir}/${local.nats_agent}/user.nk"))
  }
  nats_auth_values = local.nats_dir == "" ? {} : {
    nats = {
      auth = {
        eventService = { publicKeys = [trimspace(file("${local.nats_dir}/event-service/user.pub"))] }
        monitor      = { publicKeys = [trimspace(file("${local.nats_dir}/monitor/user.pub"))] }
        tenants = [{
          namespace  = var.nexus_namespace
          publicKeys = [trimspace(file("${local.nats_dir}/${local.nats_agent}/user.pub"))]
        }]
      }
    }
  }
}

resource "kubernetes_secret_v1" "credentials" {
  metadata {
    name      = local.secret_name
    namespace = kubernetes_namespace_v1.this.metadata[0].name
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }

  data = merge({
    "postgres-password"   = random_password.postgres.result
    "minio-root-user"     = "nexus"
    "minio-root-password" = random_password.minio.result
    "worker-api-key"      = local.worker_api_key
    "admin-api-key"       = local.admin_api_key
  }, local.nats_seeds)
}

resource "helm_release" "nexus" {
  name      = var.release_name
  namespace = kubernetes_namespace_v1.this.metadata[0].name
  chart     = var.chart_path

  # Wait for every Deployment AND the migrate/bootstrap Jobs, so a successful
  # apply means a working platform, not merely submitted manifests.
  wait          = true
  wait_for_jobs = true
  timeout       = var.helm_timeout_seconds
  # Keep a failed release in place for diagnosis rather than rolling it back
  # out from under the person debugging it.
  atomic = false

  values = [
    yamlencode({
      existingSecret = kubernetes_secret_v1.credentials.metadata[0].name
      nexusNamespace = var.nexus_namespace
      image = {
        registry         = var.image_registry
        repositoryPrefix = "kgf-nexus/"
        tag              = var.image_tag
        pullPolicy       = var.image_pull_policy
      }
    }),
    yamlencode(local.nats_auth_values),
    yamlencode(var.chart_values),
  ]
}
