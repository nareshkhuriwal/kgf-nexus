# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "kubeconfig_command" {
  description = "Write a kubeconfig entry (the caller needs an EKS access entry)."
  value       = module.platform.kubeconfig_command
}

output "registry" {
  description = "ECR registry. Push images as <registry>/kgf-nexus/<component>:<image_tag>."
  value       = module.platform.registry
}

output "database" {
  description = "Database endpoint and where its credentials live (never the password itself)."
  value = {
    host            = module.platform.postgres_host
    port            = module.platform.postgres_port
    database        = module.platform.postgres_database
    user            = module.platform.postgres_user
    sslmode         = "verify-full"
    ca_bundle       = "${module.nexus.namespace}/kgf-nexus-postgres-ca (key ca.crt)"
    runtime_logins  = var.runtime_db_roles ? "${module.nexus.namespace}/kgf-nexus-db-logins (postgres-password-<component>); the admin login is used only by the migrate, bootstrap and db-roles Jobs" : "off: every service connects as the admin login"
    password_secret = module.platform.postgres_secret_arn
    k8s_secret      = "${module.nexus.namespace}/kgf-nexus-credentials (key postgres-password)"
  }
}

output "object_store" {
  description = "Nexus result store: Amazon S3 through the VPC gateway endpoint, and how the pods authenticate."
  value = {
    endpoint        = module.platform.artifact_s3_endpoint
    bucket          = module.platform.artifact_bucket
    identity        = var.artifact_identity
    service_account = local.artifact_service_account
    role_arn        = module.platform.artifact_role_arn
    credentials = {
      pod-identity = "EKS Pod Identity: role ${module.platform.artifact_role_arn}, no key"
      irsa         = "IRSA: role ${module.platform.artifact_role_arn} (ServiceAccount annotation), no key"
      static       = "${module.nexus.namespace}/kgf-nexus-credentials (keys minio-root-user, minio-root-password): IAM user usable only via the VPC endpoint"
    }[var.artifact_identity]
  }
}

output "namespace" {
  description = "Kubernetes namespace of the release."
  value       = module.nexus.namespace
}

output "console_access" {
  description = "The console and API are ClusterIP by default; reach them like this until a Gateway/ingress is configured."
  value       = "kubectl -n ${module.nexus.namespace} port-forward svc/ui 8080:80   # and svc/api-gateway 8090:8080"
}

output "admin_api_key" {
  description = "Bootstrap admin API key: terraform output -raw admin_api_key"
  value       = module.nexus.admin_api_key
  sensitive   = true
}
