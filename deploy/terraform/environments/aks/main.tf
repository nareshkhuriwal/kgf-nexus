# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# KGF Nexus on Azure Kubernetes Service: the Azure platform (modules/
# aks-platform), then the Kubernetes pieces that depend on it, then the Nexus
# release (modules/kgf-nexus). See README.md for the two-phase apply.

provider "azurerm" {
  subscription_id = var.subscription_id
  # Data-plane calls (the Blob container) authenticate with Entra ID: the
  # artifact storage account has shared keys disabled.
  storage_use_azuread = true

  features {
    key_vault {
      # Purge protection is on; never let a destroy try to purge.
      purge_soft_delete_on_destroy = false
    }
    resource_group {
      prevent_deletion_if_contains_resources = true
    }
  }
}

# Local accounts are disabled, so every Kubernetes call carries an Entra ID
# token from kubelogin (the Azure CLI login of whoever runs Terraform, or a
# workload/managed identity in CI: set AAD_LOGIN_METHOD / kubelogin flags).
# 6dae42f8-... is the fixed application ID of the AKS AAD server.
locals {
  kube_exec = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "kubelogin"
    args        = ["get-token", "--login", "azurecli", "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630"]
  }
}

provider "kubernetes" {
  host                   = module.platform.kube_host
  cluster_ca_certificate = base64decode(module.platform.kube_cluster_ca_certificate)
  exec {
    api_version = local.kube_exec.api_version
    command     = local.kube_exec.command
    args        = local.kube_exec.args
  }
}

provider "helm" {
  kubernetes = {
    host                   = module.platform.kube_host
    cluster_ca_certificate = base64decode(module.platform.kube_cluster_ca_certificate)
    exec                   = local.kube_exec
  }
}

module "platform" {
  source = "../../modules/aks-platform"

  name     = var.name
  location = var.location
  tags     = var.tags

  kubernetes_version              = var.kubernetes_version
  sku_tier                        = var.sku_tier
  private_cluster_enabled         = var.private_cluster_enabled
  api_server_authorized_ip_ranges = var.api_server_authorized_ip_ranges
  admin_group_object_ids          = var.admin_group_object_ids
  key_vault_allowed_ip_ranges     = var.key_vault_allowed_ip_ranges

  system_pool = var.system_pool
  cpu_pool    = var.cpu_pool
  gpu_pool    = var.gpu_pool
  postgres    = var.postgres

  nexus_kubernetes_namespace = var.namespace
  postgres_password_version  = var.postgres_password_version
}

# ── In-cluster platform pieces ────────────────────────────────────────────

# Premium SSD for every stateful volume the chart creates (NATS JetStream and,
# on AKS today, MinIO). Retain: deleting the release never deletes the disk.
resource "kubernetes_storage_class_v1" "premium_ssd" {
  metadata {
    name = "nexus-premium-ssd"
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }
  storage_provisioner    = "disk.csi.azure.com"
  reclaim_policy         = "Retain"
  volume_binding_mode    = "WaitForFirstConsumer"
  allow_volume_expansion = true
  parameters = {
    skuName     = var.premium_disk_sku
    cachingMode = var.premium_disk_sku == "PremiumV2_LRS" ? "None" : "ReadOnly"
  }
}

# AKS installs the NVIDIA driver on GPU nodes but not the device plugin that
# advertises nvidia.com/gpu to the scheduler.
resource "helm_release" "nvidia_device_plugin" {
  count = var.gpu_pool.enabled ? 1 : 0

  name             = "nvidia-device-plugin"
  namespace        = "nvidia-device-plugin"
  create_namespace = true
  repository       = "https://nvidia.github.io/k8s-device-plugin"
  chart            = "nvidia-device-plugin"
  version          = var.nvidia_device_plugin_version
  wait             = true

  values = [yamlencode({
    nodeSelector = { "nexus.khuriwalgroup.com/pool" = "gpu" }
    tolerations  = [{ key = "nvidia.com/gpu", operator = "Exists", effect = "NoSchedule" }]
  })]
}

# ── Nexus ─────────────────────────────────────────────────────────────────

module "nexus" {
  source = "../../modules/kgf-nexus"

  namespace       = var.namespace
  nexus_namespace = var.nexus_namespace
  chart_path      = "${path.module}/../../../helm/kgf-nexus"
  image_tag       = var.image_tag
  image_registry  = "${module.platform.registry_login_server}/"

  nats_credentials_dir = var.nats_credentials_dir

  # The managed server's password; MinIO keeps generated root credentials.
  postgres_password = module.platform.postgres_password

  chart_values = {
    postgres = {
      enabled  = false
      host     = module.platform.postgres_host
      port     = 5432
      user     = module.platform.postgres_user
      database = module.platform.postgres_database
      # Encrypted, server identity not verified: the chart cannot mount a CA
      # bundle for verify-full yet (F115). Traffic never leaves the VNet.
      sslmode = "require"
    }

    # No S3 endpoint exists for Azure Blob: the result store is the in-cluster
    # MinIO on Premium SSD until the native Blob backend lands (F115).
    minio = {
      enabled = true
      storage = { size = var.minio_storage_size, storageClass = kubernetes_storage_class_v1.premium_ssd.metadata[0].name }
    }

    nats = {
      storage = { storageClass = kubernetes_storage_class_v1.premium_ssd.metadata[0].name }
    }
  }

  chart_values_overrides = var.chart_values_overrides

  depends_on = [module.platform]
}
