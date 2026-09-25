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
#
# Generated only when the caller supplies none: a managed database (RDS, Azure
# Flexible Server, Cloud SQL) is created by a cloud platform module, which owns
# its password and passes it in through var.postgres_password.
resource "random_password" "postgres" {
  count   = var.postgres_password == null ? 1 : 0
  length  = 32
  special = false
}

# Root credentials for the in-cluster MinIO, unless the caller supplies a key
# pair for an external S3-compatible store (Amazon S3, GCS HMAC interop).
resource "random_password" "minio" {
  count   = var.object_store_credentials == null ? 1 : 0
  length  = 32
  special = false
}

# These two gained `count`; existing state (the dev environment) moves to
# index 0 instead of being regenerated, which would rotate live passwords.
moved {
  from = random_password.postgres
  to   = random_password.postgres[0]
}

moved {
  from = random_password.minio
  to   = random_password.minio[0]
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

  postgres_password = (
    var.postgres_password != null ? var.postgres_password : random_password.postgres[0].result
  )
  object_store_access_key = (
    var.object_store_credentials != null ? var.object_store_credentials.access_key_id : "nexus"
  )
  object_store_secret_key = (
    var.object_store_credentials != null ? var.object_store_credentials.secret_access_key : random_password.minio[0].result
  )

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
    "postgres-password" = local.postgres_password
    # The chart's key names predate external stores: they hold the S3 key pair
    # whether the store is the in-cluster MinIO or not.
    "minio-root-user"     = local.object_store_access_key
    "minio-root-password" = local.object_store_secret_key
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
    # Last, so an operator's per-deployment overrides win over the values a
    # cloud environment derives from its infrastructure. Helm deep-merges.
    yamlencode(var.chart_values_overrides),
  ]
}
