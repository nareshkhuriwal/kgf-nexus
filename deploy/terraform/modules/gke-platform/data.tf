# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Stateful services: Cloud SQL, the database secret, the artifact bucket and
# the identity that writes to it.

# ── Cloud SQL for PostgreSQL (private IP) ─────────────────────────────────

# Alphanumeric: the chart embeds it unescaped in a postgres:// URL.
resource "random_password" "postgres" {
  length  = 40
  special = false
  # A new version generates a new password and pushes it everywhere at once.
  keepers = { version = var.postgres_password_version }
}

resource "google_sql_database_instance" "this" {
  project             = var.project_id
  name                = "${var.name}-pg-${random_string.suffix.result}"
  region              = var.region
  database_version    = var.postgres.database_version
  deletion_protection = var.deletion_protection

  settings {
    tier              = var.postgres.tier
    edition           = "ENTERPRISE"
    availability_type = var.postgres.high_availability ? "REGIONAL" : "ZONAL"
    disk_type         = "PD_SSD"
    disk_size         = var.postgres.disk_size_gb
    disk_autoresize   = true
    user_labels       = local.labels

    deletion_protection_enabled = var.deletion_protection

    # No public IP at all; TLS required on the private one (sslmode=require
    # works without client certificates).
    ip_configuration {
      ipv4_enabled                                  = false
      private_network                               = google_compute_network.this.id
      enable_private_path_for_google_cloud_services = true
      ssl_mode                                      = "ENCRYPTED_ONLY"
    }

    # Automated backups + write-ahead logs = point-in-time recovery (the
    # managed-database path, docs/OPERATIONS.md §4).
    backup_configuration {
      enabled                        = true
      start_time                     = "02:00"
      point_in_time_recovery_enabled = true
      transaction_log_retention_days = var.postgres.log_retention_days
      backup_retention_settings {
        retained_backups = var.postgres.backup_retention
        retention_unit   = "COUNT"
      }
    }

    maintenance_window {
      day          = 7
      hour         = 3
      update_track = "stable"
    }

    insights_config {
      query_insights_enabled  = true
      record_application_tags = false
      record_client_address   = false
    }

    database_flags {
      name  = "log_connections"
      value = "on"
    }
    database_flags {
      name  = "log_disconnections"
      value = "on"
    }
    database_flags {
      name  = "log_min_duration_statement"
      value = "1000"
    }
    database_flags {
      name  = "log_lock_waits"
      value = "on"
    }
    database_flags {
      name  = "log_checkpoints"
      value = "on"
    }
  }

  depends_on = [google_service_networking_connection.private_services]
}

resource "google_sql_database" "nexus" {
  project  = var.project_id
  name     = "nexus"
  instance = google_sql_database_instance.this.name
}

# A built-in user is a member of cloudsqlsuperuser, which has CREATEROLE: the
# migrations create the per-service NOLOGIN roles.
resource "google_sql_user" "nexus" {
  project             = var.project_id
  name                = "nexus"
  instance            = google_sql_database_instance.this.name
  type                = "BUILT_IN"
  password_wo         = random_password.postgres.result
  password_wo_version = var.postgres_password_version
}

resource "google_secret_manager_secret" "postgres_password" {
  project   = var.project_id
  secret_id = "${var.name}-postgres-password"
  labels    = local.labels

  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }

  depends_on = [google_project_service.this]
}

resource "google_secret_manager_secret_version" "postgres_password" {
  secret                 = google_secret_manager_secret.postgres_password.id
  secret_data_wo         = random_password.postgres.result
  secret_data_wo_version = var.postgres_password_version
}

# ── Artifact bucket (GCS through the S3 interoperability API) ─────────────

resource "google_storage_bucket" "artifacts" {
  project                     = var.project_id
  name                        = "${var.name}-results-${random_string.suffix.result}"
  location                    = upper(var.region)
  storage_class               = "STANDARD"
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  labels                      = local.labels

  versioning {
    enabled = true
  }

  soft_delete_policy {
    retention_duration_seconds = 604800
  }

  lifecycle_rule {
    condition {
      age = 7
    }
    action {
      type = "AbortIncompleteMultipartUpload"
    }
  }

  # Objects are immutable and content-addressed; a noncurrent version exists
  # only after a delete, so this is the undelete window.
  lifecycle_rule {
    condition {
      days_since_noncurrent_time = var.artifact_noncurrent_version_days
      with_state                 = "ARCHIVED"
    }
    action {
      type = "Delete"
    }
  }
}

# The identity the result store acts as: object read/write on this bucket and
# nothing else in the project.
resource "google_service_account" "artifacts" {
  project      = var.project_id
  account_id   = "${var.name}-art"
  display_name = "${var.name} Nexus result store"
}

resource "google_storage_bucket_iam_member" "artifacts" {
  bucket = google_storage_bucket.artifacts.name
  role   = "roles/storage.objectUser"
  member = google_service_account.artifacts.member
}

# OPT-IN fallback (artifact_hmac_key, default false): an HMAC key for the
# service account, for the s3 backend against GCS's XML API in S3
# interoperability mode. The environment root writes it into the kgf-nexus
# credentials Secret. Rotate by creating a second key, rolling the Secret,
# then setting the old key INACTIVE and deleting it. Unlike the default below
# it is a long-lived secret; prefer Workload Identity.
resource "google_storage_hmac_key" "artifacts" {
  count                 = var.artifact_hmac_key ? 1 : 0
  project               = var.project_id
  service_account_email = google_service_account.artifacts.email
}

# Existing state (created before the count) keeps its key instead of being
# destroyed and recreated under a new address.
moved {
  from = google_storage_hmac_key.artifacts
  to   = google_storage_hmac_key.artifacts[0]
}

# The default: the native gcs backend acts as this service account through
# GKE Workload Identity -- the kgf-nexus ServiceAccount, annotated
# iam.gke.io/gcp-service-account, gets short-lived tokens from the metadata
# server. No key exists anywhere.
resource "google_service_account_iam_member" "artifacts_workload_identity" {
  service_account_id = google_service_account.artifacts.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.nexus_kubernetes_namespace}/${var.nexus_service_account}]"

  depends_on = [google_container_cluster.this]
}
