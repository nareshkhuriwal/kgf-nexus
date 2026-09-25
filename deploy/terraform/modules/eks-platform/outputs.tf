# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "region" {
  description = "AWS region."
  value       = local.region
}

output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "EKS API server endpoint."
  value       = module.eks.cluster_endpoint
}

output "cluster_certificate_authority_data" {
  description = "Base64 PEM of the cluster CA."
  value       = module.eks.cluster_certificate_authority_data
}

output "kubeconfig_command" {
  description = "Write a kubeconfig entry for this cluster (needs an admin access entry)."
  value       = "aws eks update-kubeconfig --region ${local.region} --name ${module.eks.cluster_name}"
}

output "oidc_provider_arn" {
  description = "IAM OIDC provider for IRSA."
  value       = module.eks.oidc_provider_arn
}

output "registry" {
  description = "ECR registry host, e.g. <account>.dkr.ecr.<region>.amazonaws.com. Images live at <registry>/kgf-nexus/<component>."
  value       = "${local.account_id}.dkr.ecr.${local.region}.amazonaws.com"
}

output "ecr_repository_urls" {
  description = "Repository URL per component."
  value       = { for k, r in aws_ecr_repository.this : k => r.repository_url }
}

output "postgres_host" {
  description = "RDS endpoint address (private; resolves to VPC addresses only)."
  value       = aws_db_instance.postgres.address
}

output "postgres_port" {
  description = "RDS port."
  value       = aws_db_instance.postgres.port
}

output "postgres_user" {
  description = "Database user Nexus connects as."
  value       = aws_db_instance.postgres.username
}

output "postgres_database" {
  description = "Database name."
  value       = aws_db_instance.postgres.db_name
}

output "postgres_password" {
  description = "Database password, for the kgf-nexus credentials Secret."
  value       = random_password.postgres.result
  sensitive   = true
}

output "postgres_secret_arn" {
  description = "Secrets Manager secret with the full connection (host, port, user, password, sslmode)."
  value       = aws_secretsmanager_secret.postgres.arn
}

output "artifact_bucket" {
  description = "S3 bucket for Nexus results and logs."
  value       = aws_s3_bucket.artifacts.id
}

output "artifact_s3_endpoint" {
  description = "Regional S3 endpoint for the chart's minio.endpoint (reached through the VPC gateway endpoint)."
  value       = "https://s3.${local.region}.amazonaws.com"
}

output "artifact_role_arn" {
  description = "IRSA / Pod Identity role for the result store (target design; unused until F115 is closed)."
  value       = aws_iam_role.artifacts.arn
}

output "artifact_credentials" {
  description = "Static key pair for the result store (null when artifact_static_credentials = false)."
  value = var.artifact_static_credentials ? {
    access_key_id     = aws_iam_access_key.artifacts[0].id
    secret_access_key = aws_iam_access_key.artifacts[0].secret
  } : null
  sensitive = true
}

output "kms_key_arn" {
  description = "KMS key encrypting this environment's data (use it for the gp3 StorageClass)."
  value       = aws_kms_key.data.arn
}
