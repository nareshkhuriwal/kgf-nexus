# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "kubeconfig_command" {
  description = "Get cluster credentials (Entra ID; local accounts are disabled)."
  value       = module.platform.kubeconfig_command
}

output "registry" {
  description = "ACR login server. Push images as <registry>/kgf-nexus/<component>:<image_tag>."
  value       = module.platform.registry_login_server
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
    password_secret = module.platform.postgres_password_secret_id
    k8s_secret      = "${module.nexus.namespace}/kgf-nexus-credentials (key postgres-password)"
  }
}

output "object_store" {
  description = "Where Nexus artifacts live on AKS today, and the provisioned-but-unused Blob target (F115)."
  value = {
    active              = "in-cluster MinIO (PVC on StorageClass nexus-premium-ssd)"
    blob_account        = module.platform.artifact_storage_account
    blob_container      = module.platform.artifact_container
    blob_backend_status = var.azure_blob_native_backend ? "enabled" : "not implemented (F115)"
  }
}

output "key_vault_uri" {
  description = "Key Vault holding the environment's secrets."
  value       = module.platform.key_vault_uri
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
