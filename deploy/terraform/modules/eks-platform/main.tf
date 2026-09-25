# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# The AWS infrastructure one KGF Nexus environment needs, and nothing inside
# Kubernetes (the environment root does that): VPC, EKS, ECR, RDS, S3, KMS
# and CloudWatch logging.
#
# Security posture, in one place:
#   - EKS access entries (API auth mode), no aws-auth ConfigMap; Kubernetes
#     Secrets envelope-encrypted with the module's own KMS key.
#   - IRSA (OIDC provider) AND the Pod Identity agent, so either binding works.
#   - VPC CNI network policy on, so the chart's NetworkPolicies are enforced.
#   - Nodes: private subnets, IMDSv2 only, one hop (pods cannot borrow the
#     node role), encrypted gp3 root volumes.
#   - RDS in isolated subnets with no internet route, not publicly accessible,
#     TLS forced, KMS-encrypted; S3 private, TLS-only, KMS-encrypted.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

resource "random_string" "suffix" {
  length  = 6
  upper   = false
  special = false
}

locals {
  account_id = data.aws_caller_identity.current.account_id
  region     = data.aws_region.current.region
  partition  = data.aws_partition.current.partition

  tags = merge(var.tags, {
    "app.kubernetes.io/part-of" = "kgf-nexus"
    "managed-by"                = "terraform"
    "nexus-environment"         = var.name
  })

  node_block_devices = {
    for k, g in { cpu = var.cpu_node_group, gpu = var.gpu_node_group } : k => {
      xvda = {
        device_name = "/dev/xvda"
        ebs = {
          volume_size           = g.disk_size_gb
          volume_type           = "gp3"
          encrypted             = true
          delete_on_termination = true
        }
      }
    }
  }

  # IMDSv2 only, and a hop limit of 1 so a pod cannot reach the node's
  # instance-profile credentials; pods get AWS access through IRSA / Pod Identity.
  node_metadata = {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
    instance_metadata_tags      = "disabled"
  }

  admin_access_entries = {
    for i, arn in var.admin_principal_arns : "admin-${i}" => {
      principal_arn = arn
      policy_associations = {
        admin = {
          policy_arn   = "arn:${local.partition}:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
          access_scope = { type = "cluster" }
        }
      }
    }
  }

  gpu_node_groups = var.gpu_node_group.enabled ? {
    gpu = {
      name           = "gpu"
      ami_type       = "AL2023_x86_64_NVIDIA"
      instance_types = var.gpu_node_group.instance_types
      min_size       = var.gpu_node_group.min_size
      max_size       = var.gpu_node_group.max_size
      desired_size   = var.gpu_node_group.desired_size

      block_device_mappings = local.node_block_devices.gpu
      metadata_options      = local.node_metadata

      labels = { "nexus.khuriwalgroup.com/pool" = "gpu", "accelerator" = "nvidia" }
      taints = {
        gpu = { key = "nvidia.com/gpu", value = "present", effect = "NO_SCHEDULE" }
      }
      update_config = { max_unavailable = 1 }
    }
  } : {}
}

# terraform-aws-modules/eks: the most widely used EKS module; it gets access
# entries, add-ons, IRSA and managed node groups right, and tracks new EKS
# features quickly.
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "21.26.0"

  name               = var.name
  kubernetes_version = var.kubernetes_version

  vpc_id                   = module.vpc.vpc_id
  subnet_ids               = module.vpc.private_subnets
  control_plane_subnet_ids = module.vpc.private_subnets

  endpoint_private_access      = true
  endpoint_public_access       = var.endpoint_public_access
  endpoint_public_access_cidrs = var.endpoint_public_access ? var.endpoint_public_access_cidrs : []

  authentication_mode                      = "API"
  enable_cluster_creator_admin_permissions = var.cluster_creator_admin
  access_entries                           = local.admin_access_entries

  # Kubernetes Secrets envelope-encrypted with a dedicated key (module default).
  encryption_config = { resources = ["secrets"] }

  enable_irsa = true

  enabled_log_types                      = ["api", "audit", "authenticator", "controllerManager", "scheduler"]
  cloudwatch_log_group_retention_in_days = var.log_retention_days
  cloudwatch_log_group_kms_key_id        = aws_kms_key.data.arn

  deletion_protection = var.cluster_deletion_protection

  addons = merge({
    vpc-cni = {
      before_compute = true
      # Enforce NetworkPolicy (the chart restricts who may talk to NATS).
      configuration_values = jsonencode({ enableNetworkPolicy = "true" })
    }
    kube-proxy             = {}
    coredns                = {}
    eks-pod-identity-agent = { before_compute = true }
    aws-ebs-csi-driver = {
      pod_identity_association = [{
        role_arn        = aws_iam_role.ebs_csi.arn
        service_account = "ebs-csi-controller-sa"
      }]
    }
    }, var.container_insights ? {
    amazon-cloudwatch-observability = {}
  } : {})

  eks_managed_node_groups = merge({
    cpu = {
      name           = "cpu"
      ami_type       = "AL2023_x86_64_STANDARD"
      instance_types = var.cpu_node_group.instance_types
      min_size       = var.cpu_node_group.min_size
      max_size       = var.cpu_node_group.max_size
      desired_size   = var.cpu_node_group.desired_size

      block_device_mappings = local.node_block_devices.cpu
      metadata_options      = local.node_metadata

      labels        = { "nexus.khuriwalgroup.com/pool" = "cpu" }
      update_config = { max_unavailable_percentage = 33 }
    }
  }, local.gpu_node_groups)

  tags = local.tags
}

# ── EBS CSI driver identity (Pod Identity) ────────────────────────────────

data "aws_iam_policy_document" "pod_identity_trust" {
  statement {
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "ebs_csi" {
  name               = "${var.name}-ebs-csi"
  assume_role_policy = data.aws_iam_policy_document.pod_identity_trust.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "ebs_csi" {
  role       = aws_iam_role.ebs_csi.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AmazonEBSCSIDriverPolicy"
}

# The managed policy covers aws/ebs keys only; volumes in the StorageClass are
# encrypted with this environment's key, so the driver needs grants on it.
data "aws_iam_policy_document" "ebs_csi_kms" {
  statement {
    actions   = ["kms:CreateGrant", "kms:ListGrants", "kms:RevokeGrant"]
    resources = [aws_kms_key.data.arn]
    condition {
      test     = "Bool"
      variable = "kms:GrantIsForAWSResource"
      values   = ["true"]
    }
  }
  statement {
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey"]
    resources = [aws_kms_key.data.arn]
  }
}

resource "aws_iam_role_policy" "ebs_csi_kms" {
  name   = "kms"
  role   = aws_iam_role.ebs_csi.id
  policy = data.aws_iam_policy_document.ebs_csi_kms.json
}

# ── Container registry ────────────────────────────────────────────────────

resource "aws_ecr_repository" "this" {
  for_each = toset(var.ecr_repositories)

  name                 = "kgf-nexus/${each.value}"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = false

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "KMS"
    kms_key         = aws_kms_key.data.arn
  }

  tags = local.tags
}

resource "aws_ecr_lifecycle_policy" "this" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Expire untagged images (superseded pushes) after 14 days"
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = 14
      }
      action = { type = "expire" }
    }]
  })
}
