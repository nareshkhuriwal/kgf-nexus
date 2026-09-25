# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# KGF Nexus on Amazon EKS: the AWS platform (modules/eks-platform), then the
# Kubernetes pieces that depend on it, then the Nexus release (modules/
# kgf-nexus). See README.md for the two-phase apply.

provider "aws" {
  region              = var.region
  allowed_account_ids = var.allowed_account_ids

  default_tags {
    tags = var.tags
  }
}

# Short-lived tokens from the AWS CLI on every call; no static kubeconfig.
locals {
  kube_exec = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    args        = ["eks", "get-token", "--cluster-name", module.platform.cluster_name, "--region", var.region]
  }
}

provider "kubernetes" {
  host                   = module.platform.cluster_endpoint
  cluster_ca_certificate = base64decode(module.platform.cluster_certificate_authority_data)
  exec {
    api_version = local.kube_exec.api_version
    command     = local.kube_exec.command
    args        = local.kube_exec.args
  }
}

provider "helm" {
  kubernetes = {
    host                   = module.platform.cluster_endpoint
    cluster_ca_certificate = base64decode(module.platform.cluster_certificate_authority_data)
    exec                   = local.kube_exec
  }
}

module "platform" {
  source = "../../modules/eks-platform"

  name = var.name
  tags = var.tags

  vpc_cidr           = var.vpc_cidr
  nat_gateway_per_az = var.nat_gateway_per_az

  kubernetes_version           = var.kubernetes_version
  endpoint_public_access       = var.endpoint_public_access
  endpoint_public_access_cidrs = var.endpoint_public_access_cidrs
  admin_principal_arns         = var.admin_principal_arns

  cpu_node_group     = var.cpu_node_group
  gpu_node_group     = var.gpu_node_group
  container_insights = var.container_insights
  postgres           = var.postgres

  nexus_kubernetes_namespace = var.namespace
  postgres_password_version  = var.postgres_password_version
}

# ── In-cluster platform pieces ────────────────────────────────────────────

# Encrypted gp3 with this environment's KMS key, and the cluster default:
# recent EKS clusters ship no default StorageClass, which would leave the
# chart's volumes Pending.
resource "kubernetes_storage_class_v1" "gp3" {
  metadata {
    name = "nexus-gp3"
    annotations = {
      "storageclass.kubernetes.io/is-default-class" = "true"
    }
    labels = {
      "app.kubernetes.io/part-of"    = "kgf-nexus"
      "app.kubernetes.io/managed-by" = "terraform"
    }
  }
  storage_provisioner    = "ebs.csi.aws.com"
  reclaim_policy         = "Retain"
  volume_binding_mode    = "WaitForFirstConsumer"
  allow_volume_expansion = true
  parameters = {
    type      = "gp3"
    encrypted = "true"
    kmsKeyId  = module.platform.kms_key_arn
  }
}

# The AL2023 NVIDIA AMI carries the driver and container toolkit, not the
# device plugin that advertises nvidia.com/gpu.
resource "helm_release" "nvidia_device_plugin" {
  count = var.gpu_node_group.enabled ? 1 : 0

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

# A plan-time error, not a check warning: without a key pair the kgf-nexus
# module would fall back to generating MinIO root credentials, which S3 would
# reject at the first upload.
resource "terraform_data" "artifact_credentials_guard" {
  lifecycle {
    precondition {
      condition     = module.platform.artifact_credentials != null
      error_message = "No result-store credentials: the Nexus S3 client needs a static key pair today (F115). Keep artifact_static_credentials = true in the platform module."
    }
  }
}

module "nexus" {
  source = "../../modules/kgf-nexus"

  namespace       = var.namespace
  nexus_namespace = var.nexus_namespace
  chart_path      = "${path.module}/../../../helm/kgf-nexus"
  image_tag       = var.image_tag
  image_registry  = "${module.platform.registry}/"

  nats_credentials_dir = var.nats_credentials_dir

  postgres_password        = module.platform.postgres_password
  object_store_credentials = module.platform.artifact_credentials

  chart_values = {
    postgres = {
      enabled  = false
      host     = module.platform.postgres_host
      port     = module.platform.postgres_port
      user     = module.platform.postgres_user
      database = module.platform.postgres_database
      # rds.force_ssl=1 refuses anything else. Encrypted, server identity not
      # verified: the chart cannot mount the RDS CA bundle for verify-full yet
      # (F115). Traffic never leaves the VPC.
      sslmode = "require"
    }

    # Amazon S3, natively: the result store's own protocol.
    minio = {
      enabled  = false
      endpoint = module.platform.artifact_s3_endpoint
      bucket   = module.platform.artifact_bucket
      region   = module.platform.region
      useTLS   = true
    }

    nats = {
      storage = { storageClass = kubernetes_storage_class_v1.gp3.metadata[0].name }
    }
  }

  chart_values_overrides = var.chart_values_overrides

  depends_on = [module.platform]
}
