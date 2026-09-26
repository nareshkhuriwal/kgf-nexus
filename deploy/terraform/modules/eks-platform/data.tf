# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Stateful services and their encryption: one customer-managed KMS key for
# this environment's data (RDS, S3, ECR, Secrets Manager, CloudWatch Logs, EBS
# volumes from the StorageClass), RDS PostgreSQL, the artifact bucket and the
# identities that may use it.

# ── KMS ───────────────────────────────────────────────────────────────────

data "aws_iam_policy_document" "kms_data" {
  # Key administration stays with IAM in this account (the AWS default).
  statement {
    sid       = "AccountAdministration"
    actions   = ["kms:*"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = ["arn:${local.partition}:iam::${local.account_id}:root"]
    }
  }

  # CloudWatch Logs encrypts the control-plane, flow and RDS log groups, and
  # only for log groups in this account and region.
  statement {
    sid = "CloudWatchLogs"
    actions = [
      "kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey",
    ]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["logs.${local.region}.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:${local.partition}:logs:${local.region}:${local.account_id}:log-group:*"]
    }
  }
}

resource "aws_kms_key" "data" {
  description             = "${var.name}: Nexus data at rest (RDS, S3, ECR, Secrets Manager, logs, EBS)"
  enable_key_rotation     = true
  rotation_period_in_days = 365
  deletion_window_in_days = 30
  policy                  = data.aws_iam_policy_document.kms_data.json
  tags                    = local.tags
}

resource "aws_kms_alias" "data" {
  name          = "alias/${var.name}-data"
  target_key_id = aws_kms_key.data.key_id
}

# ── RDS PostgreSQL ────────────────────────────────────────────────────────

# Alphanumeric: the chart embeds it unescaped in a postgres:// URL. It reaches
# RDS and Secrets Manager write-only, and the kgf-nexus credentials Secret.
resource "random_password" "postgres" {
  length  = 40
  special = false
  # A new version generates a new password and pushes it everywhere at once.
  keepers = { version = var.postgres_password_version }
}

resource "aws_security_group" "postgres" {
  name        = "${var.name}-postgres"
  description = "PostgreSQL: reachable from EKS nodes only"
  vpc_id      = module.vpc.vpc_id
  tags        = merge(local.tags, { Name = "${var.name}-postgres" })
}

# Pods use their node's ENIs (no security groups for pods), so the node and
# cluster-primary security groups are the sources to allow.
resource "aws_vpc_security_group_ingress_rule" "postgres_from_nodes" {
  for_each = {
    node    = module.eks.node_security_group_id
    primary = module.eks.cluster_primary_security_group_id
  }
  security_group_id            = aws_security_group.postgres.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  description                  = "PostgreSQL from EKS ${each.key} security group"
}

resource "aws_db_parameter_group" "postgres" {
  name        = "${var.name}-postgres${split(".", var.postgres.engine_version)[0]}"
  family      = "postgres${split(".", var.postgres.engine_version)[0]}"
  description = "Nexus: TLS forced, connection logging"
  tags        = local.tags

  parameter {
    name  = "rds.force_ssl"
    value = "1"
  }
  parameter {
    name  = "log_connections"
    value = "1"
  }
  parameter {
    name  = "log_disconnections"
    value = "1"
  }
  parameter {
    name  = "log_min_duration_statement"
    value = "1000"
  }

  lifecycle {
    create_before_destroy = true
  }
}

# Created up front so RDS log exports land with a retention and KMS key rather
# than in a never-expiring, unencrypted group RDS would create itself.
resource "aws_cloudwatch_log_group" "postgres" {
  for_each          = toset(["postgresql", "upgrade"])
  name              = "/aws/rds/instance/${var.name}-postgres/${each.value}"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.data.arn
  tags              = local.tags
}

data "aws_iam_policy_document" "rds_monitoring_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["monitoring.rds.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "rds_monitoring" {
  name               = "${var.name}-rds-monitoring"
  assume_role_policy = data.aws_iam_policy_document.rds_monitoring_trust.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "rds_monitoring" {
  role       = aws_iam_role.rds_monitoring.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AmazonRDSEnhancedMonitoringRole"
}

resource "aws_db_instance" "postgres" {
  identifier     = "${var.name}-postgres"
  engine         = "postgres"
  engine_version = var.postgres.engine_version
  instance_class = var.postgres.instance_class

  allocated_storage     = var.postgres.allocated_storage_gb
  max_allocated_storage = var.postgres.max_storage_gb
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = aws_kms_key.data.arn

  db_name  = "nexus"
  username = "nexus"
  # The master user is the platform's user: migrations create the per-service
  # NOLOGIN roles, which needs CREATEROLE (rds_superuser has it).
  password_wo         = random_password.postgres.result
  password_wo_version = var.postgres_password_version

  db_subnet_group_name   = module.vpc.database_subnet_group_name
  vpc_security_group_ids = [aws_security_group.postgres.id]
  publicly_accessible    = false
  parameter_group_name   = aws_db_parameter_group.postgres.name
  ca_cert_identifier     = "rds-ca-rsa2048-g1"

  multi_az = var.postgres.multi_az

  # Automated backups + transaction logs = point-in-time restore within this
  # window (the managed-database path, docs/OPERATIONS.md §4).
  backup_retention_period   = var.postgres.backup_retention_days
  backup_window             = "02:00-03:00"
  maintenance_window        = "sun:03:30-sun:04:30"
  copy_tags_to_snapshot     = true
  delete_automated_backups  = false
  deletion_protection       = var.postgres.deletion_protection
  skip_final_snapshot       = false
  final_snapshot_identifier = "${var.name}-postgres-final"

  auto_minor_version_upgrade  = true
  allow_major_version_upgrade = false
  apply_immediately           = false

  iam_database_authentication_enabled = true
  enabled_cloudwatch_logs_exports     = ["postgresql", "upgrade"]

  performance_insights_enabled          = true
  performance_insights_kms_key_id       = aws_kms_key.data.arn
  performance_insights_retention_period = 7
  monitoring_interval                   = 60
  monitoring_role_arn                   = aws_iam_role.rds_monitoring.arn

  tags       = local.tags
  depends_on = [aws_cloudwatch_log_group.postgres]
}

# The database connection, for humans and tools that need it outside Terraform
# (and for a future External Secrets integration). Nexus itself receives the
# password through the kgf-nexus credentials Secret.
resource "aws_secretsmanager_secret" "postgres" {
  name                    = "${var.name}/postgres"
  description             = "Nexus PostgreSQL connection (user nexus)"
  kms_key_id              = aws_kms_key.data.arn
  recovery_window_in_days = 30
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "postgres" {
  secret_id = aws_secretsmanager_secret.postgres.id
  secret_string_wo = jsonencode({
    engine   = "postgres"
    host     = aws_db_instance.postgres.address
    port     = aws_db_instance.postgres.port
    dbname   = aws_db_instance.postgres.db_name
    username = aws_db_instance.postgres.username
    password = random_password.postgres.result
    # Clients verify the server: sslmode=verify-full with the RDS CA bundle
    # (deploy/terraform/ca-bundles). Write-only: reaches Secrets Manager on
    # the next postgres_password_version bump.
    sslmode = "verify-full"
  })
  secret_string_wo_version = var.postgres_password_version
}

# ── Artifact bucket (S3) ──────────────────────────────────────────────────

resource "aws_s3_bucket" "artifacts" {
  bucket        = "${var.name}-results-${random_string.suffix.result}"
  force_destroy = false
  tags          = local.tags
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_versioning" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.data.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id

  rule {
    id     = "abort-incomplete-multipart"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  # Objects are immutable and content-addressed (ADR-0008); a noncurrent
  # version exists only after a delete (GC sweep), so this is the undelete
  # window, not an overwrite history.
  rule {
    id     = "expire-noncurrent"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = var.artifact_noncurrent_version_days
    }
  }

  depends_on = [aws_s3_bucket_versioning.artifacts]
}

data "aws_iam_policy_document" "artifacts_bucket" {
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.artifacts.arn, "${aws_s3_bucket.artifacts.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}

resource "aws_s3_bucket_policy" "artifacts" {
  bucket     = aws_s3_bucket.artifacts.id
  policy     = data.aws_iam_policy_document.artifacts_bucket.json
  depends_on = [aws_s3_bucket_public_access_block.artifacts]
}

# What the result store needs and nothing more: objects in this one bucket,
# plus the key that encrypts them.
data "aws_iam_policy_document" "artifacts_access" {
  statement {
    sid       = "Bucket"
    actions   = ["s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"]
    resources = [aws_s3_bucket.artifacts.arn]
  }
  statement {
    sid = "Objects"
    actions = [
      "s3:GetObject", "s3:PutObject", "s3:DeleteObject",
      "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts",
    ]
    resources = ["${aws_s3_bucket.artifacts.arn}/*"]
  }
  statement {
    sid       = "Key"
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
    resources = [aws_kms_key.data.arn]
  }
}

resource "aws_iam_policy" "artifacts" {
  name        = "${var.name}-artifacts"
  description = "Nexus result store: read/write the artifact bucket only"
  policy      = data.aws_iam_policy_document.artifacts_access.json
  tags        = local.tags
}

# The result store's identity: a role for the result-store pods (api-gateway,
# agents, artifact-gc), bound to their ServiceAccount through EKS Pod Identity
# (the association below; environments/eks default) or IRSA (the SA's
# eks.amazonaws.com/role-arn annotation). Both trusts are present so switching
# is a values change. No key exists to leak.
data "aws_iam_policy_document" "artifacts_role_trust" {
  statement {
    sid     = "PodIdentity"
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "Service"
      identifiers = ["pods.eks.amazonaws.com"]
    }
  }
  statement {
    sid     = "IRSA"
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [module.eks.oidc_provider_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "${module.eks.oidc_provider}:sub"
      values   = ["system:serviceaccount:${var.nexus_kubernetes_namespace}:${var.nexus_service_account}"]
    }
    condition {
      test     = "StringEquals"
      variable = "${module.eks.oidc_provider}:aud"
      values   = ["sts.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "artifacts" {
  name               = "${var.name}-nexus-artifacts"
  assume_role_policy = data.aws_iam_policy_document.artifacts_role_trust.json
  tags               = local.tags
}

resource "aws_iam_role_policy_attachment" "artifacts" {
  role       = aws_iam_role.artifacts.name
  policy_arn = aws_iam_policy.artifacts.arn
}

# The same network fence the static key has: S3 requests on the bucket made
# with this role are denied unless they arrive through this VPC's gateway
# endpoint, so credentials lifted from a pod are worthless elsewhere.
resource "aws_iam_role_policy" "artifacts_vpce_only" {
  name   = "deny-outside-vpc-endpoint"
  role   = aws_iam_role.artifacts.id
  policy = data.aws_iam_policy_document.artifacts_vpce_only.json
}

resource "aws_eks_pod_identity_association" "artifacts" {
  cluster_name    = module.eks.cluster_name
  namespace       = var.nexus_kubernetes_namespace
  service_account = var.nexus_service_account
  role_arn        = aws_iam_role.artifacts.arn
  tags            = local.tags
}

# OPT-IN fallback (artifact_static_credentials, default false): an IAM user and
# key pair, for a cluster where neither Pod Identity nor IRSA can be used.
# Contained as far as a static key can be: the same least-privilege policy,
# and an explicit deny for any request that does not arrive through this VPC's
# S3 gateway endpoint, so a leaked key is useless outside the cluster's
# network. It still never expires on its own; prefer the role.
resource "aws_iam_user" "artifacts" {
  count = var.artifact_static_credentials ? 1 : 0
  name  = "${var.name}-nexus-artifacts"
  path  = "/nexus/"
  tags  = local.tags
}

resource "aws_iam_user_policy_attachment" "artifacts" {
  count      = var.artifact_static_credentials ? 1 : 0
  user       = aws_iam_user.artifacts[0].name
  policy_arn = aws_iam_policy.artifacts.arn
}

data "aws_iam_policy_document" "artifacts_vpce_only" {
  statement {
    sid       = "DenyOutsideVpcEndpoint"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.artifacts.arn, "${aws_s3_bucket.artifacts.arn}/*"]
    condition {
      test     = "StringNotEquals"
      variable = "aws:SourceVpce"
      values   = [aws_vpc_endpoint.s3.id]
    }
  }
}

resource "aws_iam_user_policy" "artifacts_vpce_only" {
  count  = var.artifact_static_credentials ? 1 : 0
  name   = "deny-outside-vpc-endpoint"
  user   = aws_iam_user.artifacts[0].name
  policy = data.aws_iam_policy_document.artifacts_vpce_only.json
}

resource "aws_iam_access_key" "artifacts" {
  count = var.artifact_static_credentials ? 1 : 0
  user  = aws_iam_user.artifacts[0].name
}
