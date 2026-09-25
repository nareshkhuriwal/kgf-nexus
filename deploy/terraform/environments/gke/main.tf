# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# KGF Nexus on Google Kubernetes Engine: the GCP platform (modules/
# gke-platform), then the Kubernetes pieces that depend on it, then the Nexus
# release (modules/kgf-nexus). See README.md for the two-phase apply.

provider "google" {
  project        = var.project_id
  region         = var.region
  default_labels = var.labels
}

# The caller's own OAuth token (gcloud ADC or a CI workload identity), fresh
# on every run; no static kubeconfig or client certificate.
data "google_client_config" "current" {}

locals {
  # A private control plane is reached through its DNS endpoint, which serves
  # a publicly trusted certificate and authorises every call with IAM.
  kube_host = var.private_endpoint ? "https://${module.platform.cluster_dns_endpoint}" : "https://${module.platform.cluster_endpoint}"
  kube_ca   = var.private_endpoint ? null : base64decode(module.platform.cluster_ca_certificate)
}

provider "kubernetes" {
  host                   = local.kube_host
  cluster_ca_certificate = local.kube_ca
  token                  = data.google_client_config.current.access_token
}

provider "helm" {
  kubernetes = {
    host                   = local.kube_host
    cluster_ca_certificate = local.kube_ca
    token                  = data.google_client_config.current.access_token
  }
}

module "platform" {
  source = "../../modules/gke-platform"

  project_id  = var.project_id
  region      = var.region
  name        = var.name
  labels      = var.labels
  enable_apis = var.enable_apis

  release_channel         = var.release_channel
  private_endpoint        = var.private_endpoint
  master_authorized_cidrs = var.master_authorized_cidrs

  cpu_pool            = var.cpu_pool
  gpu_pool            = var.gpu_pool
  postgres            = var.postgres
  deletion_protection = var.deletion_protection

  nexus_kubernetes_namespace = var.namespace
  postgres_password_version  = var.postgres_password_version
}

# ── In-cluster platform pieces ────────────────────────────────────────────

# Regional persistent disks: synchronously replicated across two zones, so
# NATS JetStream survives a zone loss. Retain: deleting the release never
# deletes the disk.
resource "kubernetes_storage_class_v1" "regional_ssd" {
  metadata {
    name = "nexus-regional-ssd"
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }
  storage_provisioner    = "pd.csi.storage.gke.io"
  reclaim_policy         = "Retain"
  volume_binding_mode    = "WaitForFirstConsumer"
  allow_volume_expansion = true
  parameters = {
    type             = "pd-ssd"
    replication-type = "regional-pd"
  }
}

# GKE installs the NVIDIA driver and device plugin on GPU nodes itself, and
# its admission controller adds the nvidia.com/gpu toleration to pods that
# request GPUs; nothing to install here.

# ── Nexus ─────────────────────────────────────────────────────────────────

module "nexus" {
  source = "../../modules/kgf-nexus"

  namespace       = var.namespace
  nexus_namespace = var.nexus_namespace
  chart_path      = "${path.module}/../../../helm/kgf-nexus"
  image_tag       = var.image_tag
  image_registry  = module.platform.registry

  nats_credentials_dir = var.nats_credentials_dir

  postgres_password = module.platform.postgres_password
  # GCS HMAC key pair, stored in the kgf-nexus credentials Secret.
  object_store_credentials = module.platform.artifact_hmac_credentials

  chart_values = {
    postgres = {
      enabled  = false
      host     = module.platform.postgres_host
      port     = 5432
      user     = module.platform.postgres_user
      database = module.platform.postgres_database
      # ssl_mode ENCRYPTED_ONLY on the instance. Encrypted, server identity
      # not verified: the chart cannot mount the instance CA for verify-full
      # yet (F115). Traffic never leaves the VPC.
      sslmode = "require"
    }

    # GCS through its S3-compatible XML API with HMAC keys ("interoperability").
    # minio-go special-cases storage.googleapis.com (e.g. per-object deletes,
    # since GCS has no multi-object delete).
    minio = {
      enabled  = false
      endpoint = "https://storage.googleapis.com"
      bucket   = module.platform.artifact_bucket
      region   = "auto"
      useTLS   = true
    }

    nats = {
      storage = { storageClass = kubernetes_storage_class_v1.regional_ssd.metadata[0].name }
    }
  }

  chart_values_overrides = var.chart_values_overrides

  depends_on = [module.platform]
}
