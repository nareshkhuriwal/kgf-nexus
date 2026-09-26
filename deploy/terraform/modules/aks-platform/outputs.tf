# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "resource_group_name" {
  description = "Resource group holding every resource of this environment."
  value       = azurerm_resource_group.this.name
}

output "cluster_name" {
  description = "AKS cluster name."
  value       = azurerm_kubernetes_cluster.this.name
}

output "cluster_id" {
  description = "AKS cluster resource ID."
  value       = azurerm_kubernetes_cluster.this.id
}

output "kube_host" {
  description = "API server URL (the private FQDN for a private cluster)."
  value       = azurerm_kubernetes_cluster.this.kube_config[0].host
  sensitive   = true
}

output "kube_cluster_ca_certificate" {
  description = "Base64 PEM of the cluster CA."
  value       = azurerm_kubernetes_cluster.this.kube_config[0].cluster_ca_certificate
  sensitive   = true
}

output "kubeconfig_command" {
  description = "Fetch an Entra ID kubeconfig (local accounts are disabled) and convert it for non-interactive use."
  value       = "az aks get-credentials --resource-group ${azurerm_resource_group.this.name} --name ${azurerm_kubernetes_cluster.this.name} && kubelogin convert-kubeconfig -l azurecli"
}

output "oidc_issuer_url" {
  description = "OIDC issuer for workload-identity federation."
  value       = azurerm_kubernetes_cluster.this.oidc_issuer_url
}

output "registry_login_server" {
  description = "ACR login server, e.g. <name>.azurecr.io."
  value       = azurerm_container_registry.this.login_server
}

output "postgres_host" {
  description = "Private FQDN of the PostgreSQL server (resolves only inside the VNet)."
  value       = azurerm_postgresql_flexible_server.this.fqdn
}

output "postgres_user" {
  description = "Database user Nexus connects as."
  value       = azurerm_postgresql_flexible_server.this.administrator_login
}

output "postgres_database" {
  description = "Database name."
  value       = azurerm_postgresql_flexible_server_database.nexus.name
}

output "postgres_password" {
  description = "Database password, for the kgf-nexus credentials Secret."
  value       = random_password.postgres.result
  sensitive   = true
}

output "postgres_password_secret_id" {
  description = "Key Vault secret holding the database password (versionless ID)."
  value       = azurerm_key_vault_secret.postgres_password.versionless_id
}

output "key_vault_uri" {
  description = "Key Vault URI."
  value       = azurerm_key_vault.this.vault_uri
}

output "artifact_storage_account" {
  description = "Storage account holding Nexus results and logs (native azblob backend)."
  value       = azurerm_storage_account.artifacts.name
}

output "artifact_container" {
  description = "Blob container for Nexus results and logs (the chart's minio.bucket under backend azblob)."
  value       = azurerm_storage_container.artifacts.name
}

output "nexus_workload_identity_client_id" {
  description = "Client ID of the Nexus workload identity (azure.workload.identity/client-id annotation)."
  value       = azurerm_user_assigned_identity.nexus.client_id
}

output "log_analytics_workspace_id" {
  description = "Log Analytics workspace receiving cluster and database logs."
  value       = azurerm_log_analytics_workspace.this.id
}
