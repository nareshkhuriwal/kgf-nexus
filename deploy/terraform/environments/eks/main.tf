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

  nexus_kubernetes_namespace  = var.namespace
  nexus_service_account       = local.nexus_service_account
  artifact_static_credentials = var.artifact_identity == "static"
  postgres_password_version   = var.postgres_password_version
}

locals {
  # One name on both ends: the role trust and the Pod Identity association
  # name system:serviceaccount:<ns>:<it>, and modules/kgf-nexus creates it.
  nexus_service_account = "kgf-nexus"
  # The chart's resultStore.s3.credentials for each artifact_identity: an
  # explicit mode, never auto, so a stray key or a missing webhook is an error
  # at startup rather than a silent fall-through to another identity.
  s3_credentials = {
    pod-identity = "pod-identity"
    irsa         = "web-identity"
    static       = "static"
  }[var.artifact_identity]
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

# A plan-time error, not a check warning: artifact_identity = "static" with no
# key pair would start every result-store pod against a Secret without keys.
resource "terraform_data" "artifact_credentials_guard" {
  lifecycle {
    precondition {
      condition     = var.artifact_identity != "static" || module.platform.artifact_credentials != null
      error_message = "artifact_identity = \"static\" needs the platform's IAM-user key pair (artifact_static_credentials), and it produced none."
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

  postgres_password = module.platform.postgres_password
  # A key pair only for the static fallback. Otherwise the Secret holds none:
  # the chart would prefer a key over the pod's identity.
  object_store_credentials          = var.artifact_identity == "static" ? module.platform.artifact_credentials : null
  generate_object_store_credentials = false

  # Created in every mode so switching never renames it. IRSA needs the role
  # annotation; Pod Identity's association already names this ServiceAccount.
  service_account = {
    name = local.nexus_service_account
    annotations = var.artifact_identity == "irsa" ? tomap({
      "eks.amazonaws.com/role-arn" = module.platform.artifact_role_arn
    }) : tomap({})
  }

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
    resultStore = {
      backend = "s3"
      s3      = { credentials = local.s3_credentials }
    }

    nats = {
      storage = { storageClass = kubernetes_storage_class_v1.gp3.metadata[0].name }
    }
  }

  chart_values_overrides = var.chart_values_overrides

  depends_on = [module.platform]
}
