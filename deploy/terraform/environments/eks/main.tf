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
  nexus_service_account       = local.artifact_service_account
  artifact_static_credentials = var.artifact_identity == "static"
  postgres_password_version   = var.postgres_password_version
}

locals {
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
    # One name on both ends: the role trust and the Pod Identity association
    # name system:serviceaccount:<ns>:nexus-artifacts, and the chart creates
    # the account. IRSA also needs the role annotation (the chart refuses
    # web-identity without it); Pod Identity's association already names this
    # ServiceAccount, and an unused IRSA annotation would make the webhook
    # inject a second, competing identity. Created in every mode, so switching
    # artifact_identity never renames it.
    annotations = var.artifact_identity == "irsa" ? tomap({
      "eks.amazonaws.com/role-arn"               = module.platform.artifact_role_arn
      "eks.amazonaws.com/sts-regional-endpoints" = "true"
    }) : tomap({})
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
      limits   = { memory = "12Gi" }
    }
  }

  agents = concat([local.cpu_agent], slice([local.gpu_agent], 0, var.gpu_node_group.enabled ? 1 : 0))
}

# The RDS CA bundle, vendored with its upstream source and SHA-256 pinned
# (deploy/terraform/ca-bundles/refresh.sh). Refused if the file no longer
# matches SHA256SUMS, so a hand edit cannot change the trust anchors unseen.
locals {
  ca_bundle_dir = "${path.module}/../../ca-bundles"
  ca_bundle_pins = {
    for l in compact(split("\n", file("${local.ca_bundle_dir}/SHA256SUMS"))) :
    element(split("  ", l), 1) => element(split("  ", l), 0)
  }
  postgres_ca_bundle_file = "rds-global-bundle.pem"
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

  postgres_ca_bundle          = local.postgres_ca_bundle
  runtime_db_roles            = var.runtime_db_roles
  runtime_db_password_version = var.runtime_db_password_version

  chart_values = {
    postgres = {
      enabled  = false
      host     = module.platform.postgres_host
      port     = module.platform.postgres_port
      user     = module.platform.postgres_user
      database = module.platform.postgres_database
      # verify-full against the managed server's CA (the kgf-nexus module
      # mounts postgres_ca_bundle and sets it); the services connect as
      # their own runtime logins, not the admin (runtime_db_roles).
      sslmode = "verify-full"
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

    serviceAccounts = {
      apiGateway = local.artifact_identity
      agent      = local.artifact_identity
      artifactGC = local.artifact_identity
    }

    agents = local.agents
  }

  chart_values_overrides = var.chart_values_overrides

  depends_on = [module.platform]
}
