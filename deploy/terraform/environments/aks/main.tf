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
  nexus_service_account      = local.artifact_service_account
  postgres_password_version  = var.postgres_password_version
}

locals {
  # The native Blob backend (azblob over workload identity), or the in-cluster
  # MinIO when false.
  blob = var.azure_blob_native_backend
}

# ── In-cluster platform pieces ────────────────────────────────────────────

# Premium SSD for every stateful volume the chart creates (NATS JetStream and,
# with azure_blob_native_backend = false, MinIO). Retain: deleting the release
# never deletes the disk.
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

  # The chart's default affinity requires node-feature-discovery labels
  # (feature.node.kubernetes.io/pci-10de.present, nvidia.com/gpu.present) that
  # AKS GPU nodes do not carry, and a nodeSelector only narrows it further: the
  # DaemonSet would schedule nowhere and nvidia.com/gpu would never be
  # advertised. Helm replaces lists, so this affinity REPLACES that default.
  values = [yamlencode({
    nodeSelector = { "nexus.khuriwalgroup.com/pool" = "gpu" }
    affinity = {
      nodeAffinity = {
        requiredDuringSchedulingIgnoredDuringExecution = {
          nodeSelectorTerms = [{
            matchExpressions = [{ key = "nexus.khuriwalgroup.com/pool", operator = "In", values = ["gpu"] }]
          }]
        }
      }
    }
    tolerations = [{ key = "nvidia.com/gpu", operator = "Exists", effect = "NoSchedule" }]
  })]
}

# ── Result-store identity, database CA, agent pools ──────────────────────

locals {
  gib = 1073741824

  # The ONE ServiceAccount the platform's object-store identity trusts
  # (module.platform nexus_service_account). The chart gives it to exactly
  # the components that touch the result store: api-gateway (task logs), the
  # agents and artifact-gc. Every other component keeps its own account.
  artifact_service_account = "nexus-artifacts"
  artifact_identity = {
    name = local.artifact_service_account
    # Azure Workload Identity: the federated credential on this identity
    # names system:serviceaccount:<namespace>:nexus-artifacts. The chart
    # creates the account with this annotation, and adds the pod label
    # azure.workload.identity/use to the result-store pods itself under
    # resultStore.backend=azblob + auth=workload-identity. Annotated with or
    # without Blob, so flipping azure_blob_native_backend never renames it.
    annotations = { "azure.workload.identity/client-id" = module.platform.nexus_workload_identity_client_id }
  }

  cpu_agent = {
    name         = "agent-cpu"
    replicas     = 1
    queues       = "default"
    labels       = "zone=cluster,accelerator=none"
    executors    = "subprocess"
    cpuMillis    = 2000
    memoryBytes  = 4 * local.gib
    gpuCount     = 0
    diskBytes    = 10 * local.gib
    nodeSelector = { "nexus.khuriwalgroup.com/pool" = "cpu" }
    resources    = { requests = { cpu = "250m", memory = "256Mi" }, limits = { memory = "4Gi" } }
  }

  # On the tainted GPU pool: the chart requests gpuCount nvidia.com/gpu from
  # the device plugin (advertised == allocated) and tolerates the
  # nvidia.com/gpu NoSchedule taint; the nodeSelector keeps it off CPU nodes.
  gpu_agent = {
    name           = "agent-gpu"
    replicas       = var.gpu_agent.replicas
    queues         = "gpu"
    labels         = "zone=cluster,accelerator=nvidia"
    executors      = "subprocess"
    cpuMillis      = var.gpu_agent.cpu_millis
    memoryBytes    = var.gpu_agent.memory_gib * local.gib
    gpuCount       = var.gpu_agent.gpu_count
    gpuKind        = var.gpu_agent.gpu_kind
    gpuMemoryBytes = var.gpu_agent.gpu_memory_gib * local.gib
    diskBytes      = var.gpu_agent.disk_gib * local.gib
    nodeSelector   = { "nexus.khuriwalgroup.com/pool" = "gpu" }
    resources = {
      requests = { cpu = "500m", memory = "1Gi" }
      limits   = { memory = "22Gi" }
    }
  }

  agents = concat([local.cpu_agent], slice([local.gpu_agent], 0, var.gpu_pool.enabled ? 1 : 0))
}

# The Azure Database for PostgreSQL root CA bundle, vendored with its upstream source and SHA-256 pinned
# (deploy/terraform/ca-bundles/refresh.sh). Refused if the file no longer
# matches SHA256SUMS, so a hand edit cannot change the trust anchors unseen.
locals {
  ca_bundle_dir = "${path.module}/../../ca-bundles"
  ca_bundle_pins = {
    for l in compact(split("\n", file("${local.ca_bundle_dir}/SHA256SUMS"))) :
    element(split("  ", l), 1) => element(split("  ", l), 0)
  }
  postgres_ca_bundle_file = "azure-postgres-roots.pem"
  postgres_ca_bundle      = file("${local.ca_bundle_dir}/${local.postgres_ca_bundle_file}")
}

resource "terraform_data" "postgres_ca_bundle_pin" {
  lifecycle {
    precondition {
      condition     = sha256(local.postgres_ca_bundle) == lookup(local.ca_bundle_pins, local.postgres_ca_bundle_file, "")
      error_message = "ca-bundles/${local.postgres_ca_bundle_file} does not match its pin in ca-bundles/SHA256SUMS. Regenerate it with ca-bundles/refresh.sh --write (which verifies the upstream source) instead of editing it."
    }
  }
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

  # The managed server's password. MinIO (only without the Blob backend)
  # keeps generated root credentials; with Blob the Secret holds no S3 key.
  postgres_password                 = module.platform.postgres_password
  generate_object_store_credentials = !local.blob

  postgres_ca_bundle          = local.postgres_ca_bundle
  runtime_db_roles            = var.runtime_db_roles
  runtime_db_password_version = var.runtime_db_password_version

  chart_values = {
    postgres = {
      enabled  = false
      host     = module.platform.postgres_host
      port     = 5432
      user     = module.platform.postgres_user
      database = module.platform.postgres_database
      # verify-full against the managed server's CA (the kgf-nexus module
      # mounts postgres_ca_bundle and sets it); the services connect as
      # their own runtime logins, not the admin (runtime_db_roles).
      sslmode = "verify-full"
    }

    # The result store: the provisioned Blob container through workload
    # identity, or (azure_blob_native_backend = false) the in-cluster MinIO on
    # Premium SSD. The values keep one shape either way -- only leaves change
    # -- so the two cases cannot drift apart structurally. minio.bucket is the
    # Blob CONTAINER under backend azblob. The chart adds the pod label
    # azure.workload.identity/use itself; the Blob account is reached over its
    # private endpoint, whose private DNS zone answers the public name.
    minio = {
      enabled = !local.blob
      bucket  = local.blob ? module.platform.artifact_container : "nexus-results"
      storage = { size = var.minio_storage_size, storageClass = kubernetes_storage_class_v1.premium_ssd.metadata[0].name }
    }
    resultStore = {
      backend = local.blob ? "azblob" : "s3"
      azure = {
        account = local.blob ? module.platform.artifact_storage_account : ""
        auth    = local.blob ? "workload-identity" : "auto"
      }
    }

    nats = {
      storage = { storageClass = kubernetes_storage_class_v1.premium_ssd.metadata[0].name }
    }

    serviceAccounts = {
      apiGateway      = local.artifact_identity
      workflowManager = local.artifact_identity
      agent           = local.artifact_identity
      artifactGC      = local.artifact_identity
    }

    agents = local.agents
  }

  chart_values_overrides = var.chart_values_overrides

  depends_on = [module.platform]
}
