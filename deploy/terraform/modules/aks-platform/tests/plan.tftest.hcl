# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan of the module with a MOCKED azurerm provider: no credentials,
# no network, nothing created. azurerm parses resource IDs even under a mock,
# so each referenced type gets a well-formed ID. Asserts the security defaults.
#   terraform -chdir=deploy/terraform/modules/aks-platform test

mock_provider "azurerm" {
  mock_data "azurerm_client_config" {
    defaults = {
      tenant_id = "00000000-0000-0000-0000-000000000001"
      object_id = "00000000-0000-0000-0000-000000000002"
    }
  }
  mock_resource "azurerm_resource_group" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg" }
  }
  mock_resource "azurerm_virtual_network" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet" }
  }
  mock_resource "azurerm_subnet" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/snet" }
  }
  mock_resource "azurerm_network_security_group" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/nsg" }
  }
  mock_resource "azurerm_log_analytics_workspace" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.OperationalInsights/workspaces/law" }
  }
  mock_resource "azurerm_user_assigned_identity" {
    defaults = {
      id           = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/uai"
      principal_id = "00000000-0000-0000-0000-000000000005"
      client_id    = "00000000-0000-0000-0000-000000000006"
    }
  }
  mock_resource "azurerm_kubernetes_cluster" {
    defaults = {
      id              = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks"
      oidc_issuer_url = "https://oidc.example.test/"
    }
  }
  mock_resource "azurerm_container_registry" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerRegistry/registries/acr" }
  }
  mock_resource "azurerm_private_dns_zone" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Network/privateDnsZones/zone.example" }
  }
  mock_resource "azurerm_postgresql_flexible_server" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.DBforPostgreSQL/flexibleServers/pg" }
  }
  mock_resource "azurerm_storage_account" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/st" }
  }
  mock_resource "azurerm_storage_container" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/st/blobServices/default/containers/nexus-results" }
  }
  mock_resource "azurerm_key_vault" {
    defaults = { id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.KeyVault/vaults/kv" }
  }
}
mock_provider "random" {}

variables {
  name                            = "kgf-nexus-test"
  location                        = "centralindia"
  admin_group_object_ids          = ["00000000-0000-0000-0000-000000000009"]
  api_server_authorized_ip_ranges = ["203.0.113.0/24"]
}

run "secure_defaults" {
  command = plan

  assert {
    condition     = azurerm_kubernetes_cluster.this.local_account_disabled
    error_message = "Local accounts must be disabled."
  }
  assert {
    condition     = azurerm_kubernetes_cluster.this.oidc_issuer_enabled && azurerm_kubernetes_cluster.this.workload_identity_enabled
    error_message = "OIDC issuer and workload identity must be on."
  }
  assert {
    condition     = azurerm_kubernetes_cluster.this.network_profile[0].network_plugin_mode == "overlay" && azurerm_kubernetes_cluster.this.network_profile[0].network_policy == "cilium"
    error_message = "Azure CNI overlay with an enforcing network policy is required."
  }
  assert {
    condition     = !azurerm_postgresql_flexible_server.this.public_network_access_enabled
    error_message = "PostgreSQL must have no public endpoint."
  }
  assert {
    condition     = contains(split(",", upper(azurerm_postgresql_flexible_server_configuration.extensions.value)), "PGCRYPTO")
    error_message = "pgcrypto must be allow-listed (azure.extensions): migration 0001 creates it."
  }
  assert {
    condition     = azurerm_postgresql_flexible_server.this.backup_retention_days >= 7
    error_message = "PITR retention must be at least 7 days."
  }
  assert {
    condition     = azurerm_storage_account.artifacts.public_network_access == "Disabled" && !azurerm_storage_account.artifacts.shared_access_key_enabled
    error_message = "The storage account must be private and key-less."
  }
  assert {
    condition     = azurerm_key_vault.this.purge_protection_enabled && azurerm_key_vault.this.rbac_authorization_enabled
    error_message = "Key Vault must use RBAC and purge protection."
  }
  assert {
    condition     = azurerm_role_assignment.kubelet_acr_pull.role_definition_name == "AcrPull"
    error_message = "The kubelet identity must get AcrPull and nothing more."
  }
  assert {
    condition     = !azurerm_container_registry.this.admin_enabled
    error_message = "The ACR admin user must be off."
  }
  assert {
    condition     = length(azurerm_kubernetes_cluster_node_pool.gpu) == 0
    error_message = "The GPU pool must be opt-in."
  }
}

run "gpu_pool_is_tainted" {
  command = plan

  variables {
    gpu_pool = {
      enabled   = true
      vm_size   = "Standard_NC4as_T4_v3"
      min_count = 0
      max_count = 2
    }
  }

  assert {
    condition     = contains(azurerm_kubernetes_cluster_node_pool.gpu[0].node_taints, "nvidia.com/gpu=present:NoSchedule")
    error_message = "GPU nodes must carry the nvidia.com/gpu taint."
  }
}

run "refuses_open_public_api_server" {
  command = plan

  variables {
    api_server_authorized_ip_ranges = []
  }

  expect_failures = [var.api_server_authorized_ip_ranges]
}

run "refuses_short_pitr_window" {
  command = plan

  variables {
    postgres = {
      version               = "16"
      sku_name              = "GP_Standard_D2ds_v5"
      storage_mb            = 65536
      storage_tier          = "P6"
      backup_retention_days = 3
      geo_redundant_backup  = false
      high_availability     = false
    }
  }

  expect_failures = [var.postgres]
}
