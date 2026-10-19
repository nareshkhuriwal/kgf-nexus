# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# One KGF Nexus environment: namespace (unless it already exists), generated
# credentials, and the Helm release. Environment-specific choices come in
# through var.chart_values.
#
# Credentials are GENERATED here and only here. Nothing is typed, committed or
# shared between environments. They do live in Terraform state (marked
# sensitive), so state must be stored as carefully as the secrets themselves:
# local and gitignored for dev, an encrypted remote backend anywhere else.

# Created here by default. On a cluster whose namespaces a platform team owns
# (create_namespace = false) the release goes into the existing one, which this
# module neither labels, changes nor deletes.
resource "kubernetes_namespace_v1" "this" {
  count = var.create_namespace ? 1 : 0

  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
      # Every pod of chart 0.3.0 passes restricted (OPERATIONS §20.17); the
      # default stays baseline so an existing environment is not tightened by
      # an apply, and warn=restricted reports anything that would not pass.
      "pod-security.kubernetes.io/enforce" = var.pod_security_enforce
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
# pair for an external S3-compatible store (Amazon S3, GCS HMAC interop) or
# the store is reached with a cloud identity and needs no key at all.
resource "random_password" "minio" {
  count   = var.object_store_credentials == null && var.generate_object_store_credentials ? 1 : 0
  length  = 32
  special = false
}

# These gained `count`; existing state (the dev environment) moves to index 0
# instead of being regenerated, which would rotate live passwords -- or, for
# the namespace, delete and recreate it with everything in it.
moved {
  from = kubernetes_namespace_v1.this
  to   = kubernetes_namespace_v1.this[0]
}

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

# One worker key per KGF.Nexus workspace namespace this environment runs
# agents for (var.workspace_worker_namespaces).
resource "random_id" "workspace_worker_key" {
  for_each    = toset(var.workspace_worker_namespaces)
  byte_length = 32
}

locals {
  # Every namespaced object below goes here: the namespace this module
  # created, or the existing one it was given.
  namespace = var.create_namespace ? kubernetes_namespace_v1.this[0].metadata[0].name : var.namespace

  worker_api_key = "nxs_worker_${random_id.worker_key.hex}"
  workspace_worker_keys = {
    for ns, key in random_id.workspace_worker_key : "worker-api-key-${ns}" => "nxs_worker_${key.hex}"
  }
  admin_api_key = "nxs_admin_${random_id.admin_key.hex}"
  secret_name   = "${var.release_name}-credentials"

  postgres_password = (
    var.postgres_password != null ? var.postgres_password : random_password.postgres[0].result
  )
  # The S3 key pair for the Secret: the caller's, the generated MinIO root
  # pair, or NONE. None matters: the chart's default credential mode (auto)
  # prefers a key pair over the pod's cloud identity, so a placeholder pair
  # written "just in case" would shadow IRSA / Pod Identity with keys that S3
  # rejects.
  object_store_keys = (
    var.object_store_credentials != null ? {
      "minio-root-user"     = var.object_store_credentials.access_key_id
      "minio-root-password" = var.object_store_credentials.secret_access_key
    } :
    var.generate_object_store_credentials ? {
      "minio-root-user"     = "nexus"
      "minio-root-password" = random_password.minio[0].result
    } : {}
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
  # Each KGF.Nexus workspace namespace's agents have their own identity
  # (nexus-nats-auth.sh add-tenant NS), seeded as nats-agent-nkey-<ns>.
  workspace_nats_seeds = local.nats_dir == "" ? {} : {
    for ns in var.workspace_worker_namespaces :
    "nats-agent-nkey-${ns}" => trimspace(file("${local.nats_dir}/agent-${ns}/user.nk"))
  }
  nats_auth_values = local.nats_dir == "" ? {} : {
    nats = {
      auth = {
        eventService = { publicKeys = [trimspace(file("${local.nats_dir}/event-service/user.pub"))] }
        monitor      = { publicKeys = [trimspace(file("${local.nats_dir}/monitor/user.pub"))] }
        tenants = concat([{
          namespace  = var.nexus_namespace
          publicKeys = [trimspace(file("${local.nats_dir}/${local.nats_agent}/user.pub"))]
          }], [
          for ns in var.workspace_worker_namespaces : {
            namespace  = ns
            publicKeys = [trimspace(file("${local.nats_dir}/agent-${ns}/user.pub"))]
          }
        ])
      }
    }
  }
}

resource "kubernetes_secret_v1" "credentials" {
  metadata {
    name      = local.secret_name
    namespace = local.namespace
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }

  # The chart's key names (minio-root-*) predate external stores: they hold
  # the S3 key pair whether the store is the in-cluster MinIO or not.
  data = merge({
    "postgres-password" = local.postgres_password
    "worker-api-key"    = local.worker_api_key
    "admin-api-key"     = local.admin_api_key
  }, local.object_store_keys, local.nats_seeds, local.workspace_nats_seeds, local.workspace_worker_keys)
}

# ── Database: server verification and runtime logins ────────────────────

# The managed database's root CAs, for sslmode=verify-full (chart
# postgres.caBundle). Public certificates, but a Secret rather than a
# ConfigMap so that replacing the trust anchors needs Secret write access.
resource "kubernetes_secret_v1" "postgres_ca" {
  count = var.postgres_ca_bundle == null ? 0 : 1

  metadata {
    name      = "${var.release_name}-postgres-ca"
    namespace = local.namespace
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }
  data = { "ca.crt" = var.postgres_ca_bundle }
}

# One login per service, never the owner (docs/OPERATIONS.md §20.12). The chart's
# db-roles Job creates them inside the cluster, because Terraform cannot reach
# a managed server's private endpoint to run SQL. Alphanumeric: embedded in a
# postgres:// URL, and nexus-db-roles accepts only URL-safe passwords.
locals {
  runtime_db_logins = var.runtime_db_roles ? toset([
    "api-gateway", "workflow-manager", "dag-engine", "scheduler",
    "resource-manager", "event-service", "artifact-gc",
  ]) : toset([])
}

resource "random_password" "runtime_db" {
  for_each = local.runtime_db_logins
  length   = 32
  special  = false
  keepers  = { version = var.runtime_db_password_version }
}

resource "kubernetes_secret_v1" "db_logins" {
  count = var.runtime_db_roles ? 1 : 0

  metadata {
    name      = "${var.release_name}-db-logins"
    namespace = local.namespace
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }
  data = { for k, p in random_password.runtime_db : "postgres-password-${k}" => p.result }
}

locals {
  database_values = var.postgres_ca_bundle == null ? {} : {
    postgres = {
      sslmode  = "verify-full"
      caBundle = { secretName = kubernetes_secret_v1.postgres_ca[0].metadata[0].name, key = "ca.crt" }
    }
  }
  runtime_role_values = var.runtime_db_roles ? {
    postgres = {
      runtimeRoles = {
        enabled          = true
        existingSecret   = kubernetes_secret_v1.db_logins[0].metadata[0].name
        passwordRevision = var.runtime_db_password_version
      }
    }
  } : {}
}

resource "helm_release" "nexus" {
  name      = var.release_name
  namespace = local.namespace
  # A local checkout of the chart, or the published chart at one exact
  # version (oci://ghcr.io/<owner>/charts, kgf-nexus, <version>).
  chart      = var.chart_repository == null ? var.chart_path : "kgf-nexus"
  repository = var.chart_repository
  version    = var.chart_repository == null ? null : var.chart_version

  # Wait for every Deployment AND the migrate/bootstrap Jobs, so a successful
  # apply means a working platform, not merely submitted manifests.
  wait          = true
  wait_for_jobs = true
  timeout       = var.helm_timeout_seconds
  # Keep a failed release in place for diagnosis rather than rolling it back
  # out from under the person debugging it.
  atomic = false

  lifecycle {
    precondition {
      condition     = (var.chart_path == null) != (var.chart_repository == null)
      error_message = "Set exactly one of chart_path (a chart checkout) and chart_repository (the published chart)."
    }
    precondition {
      condition     = var.chart_repository == null || var.chart_version != null
      error_message = "chart_repository needs chart_version: one exact chart version, never \"whatever is newest\"."
    }
  }

  values = [
    yamlencode({
      existingSecret = kubernetes_secret_v1.credentials.metadata[0].name
      nexusNamespace = var.nexus_namespace
      workspaceWorkers = [
        for ns in var.workspace_worker_namespaces : { namespace = ns, secretKey = "worker-api-key-${ns}" }
      ]
      image = {
        registry         = var.image_registry
        repositoryPrefix = var.image_repository_prefix
        tag              = var.image_tag
        pullPolicy       = var.image_pull_policy
      }
    }),
    yamlencode(local.nats_auth_values),
    yamlencode(var.chart_values),
    # After the environment's values: the CA bundle and runtime logins this
    # module created must be the ones the chart mounts.
    yamlencode(local.database_values),
    yamlencode(local.runtime_role_values),
    # Last, so an operator's per-deployment overrides win over the values a
    # cloud environment derives from its infrastructure. Helm deep-merges.
    yamlencode(var.chart_values_overrides),
  ]
}
