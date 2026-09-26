# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "kubeconfig_command" {
  description = "Write a kubeconfig entry (needs gke-gcloud-auth-plugin)."
  value       = module.platform.kubeconfig_command
}

output "registry" {
  description = "Artifact Registry prefix. Push images as <registry>kgf-nexus/<component>:<image_tag>."
  value       = module.platform.registry
}

output "database" {
  description = "Database endpoint and where its password lives (never the password itself)."
  value = {
    host            = module.platform.postgres_host
    port            = 5432
    database        = module.platform.postgres_database
    user            = module.platform.postgres_user
    sslmode         = "verify-full"
    ca_bundle       = "${module.nexus.namespace}/kgf-nexus-postgres-ca (key ca.crt)"
    runtime_logins  = var.runtime_db_roles ? "${module.nexus.namespace}/kgf-nexus-db-logins (postgres-password-<component>); the admin login is used only by the migrate, bootstrap and db-roles Jobs" : "off: every service connects as the admin login"
    connection_name = module.platform.postgres_instance_connection_name
    password_secret = module.platform.postgres_password_secret
    k8s_secret      = "${module.nexus.namespace}/kgf-nexus-credentials (key postgres-password)"
  }
}

output "object_store" {
  description = "Nexus result store: GCS, natively through Workload Identity or through the S3 interoperability API with an HMAC key."
  value = {
    backend             = var.artifact_identity == "workload-identity" ? "gcs" : "s3"
    endpoint            = var.artifact_identity == "workload-identity" ? "GCS JSON API (default endpoint)" : "https://storage.googleapis.com"
    bucket              = module.platform.artifact_bucket
    service_account     = module.platform.artifact_service_account
    k8s_service_account = local.artifact_service_account
    credentials = (var.artifact_identity == "workload-identity"
      ? "GKE Workload Identity as ${module.platform.artifact_service_account}, no key"
    : "${module.nexus.namespace}/kgf-nexus-credentials (keys minio-root-user, minio-root-password): HMAC key of the service account")
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
