# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Stateful services: PostgreSQL, the artifact storage account, Key Vault.

# ── PostgreSQL Flexible Server (private access) ───────────────────────────

# The nexus user's password. Alphanumeric: the chart embeds it unescaped in a
# postgres:// URL. It reaches the server write-only (not stored on the server
# resource), Key Vault write-only, and the kgf-nexus credentials Secret.
resource "random_password" "postgres" {
  length  = 40
  special = false
  # A new version generates a new password and pushes it everywhere at once.
  keepers = { version = var.postgres_password_version }
}

resource "azurerm_private_dns_zone" "postgres" {
  # Must end in .postgres.database.azure.com for private access.
  name                = "${var.name}.private.postgres.database.azure.com"
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "postgres" {
  name                 = "${var.name}-postgres"
  private_dns_zone_id  = azurerm_private_dns_zone.postgres.id
  virtual_network_id   = azurerm_virtual_network.this.id
  registration_enabled = false
  tags                 = local.tags
}

resource "azurerm_postgresql_flexible_server" "this" {
  name                = "${var.name}-pg"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  version             = var.postgres.version
  sku_name            = var.postgres.sku_name
  storage_mb          = var.postgres.storage_mb
  storage_tier        = var.postgres.storage_tier
  auto_grow_enabled   = true
  zone                = var.zones[0]

  # Private access: a delegated subnet and a private DNS zone. There is no
  # public endpoint to firewall.
  delegated_subnet_id           = azurerm_subnet.postgres.id
  private_dns_zone_id           = azurerm_private_dns_zone.postgres.id
  public_network_access_enabled = false

  # The admin login is the platform's database user: migrations create the
  # per-service NOLOGIN roles (needs CREATEROLE, which azure_pg_admin has).
  administrator_login               = "nexus"
  administrator_password_wo         = random_password.postgres.result
  administrator_password_wo_version = var.postgres_password_version

  authentication {
    password_auth_enabled         = true
    active_directory_auth_enabled = false
  }

  # Point-in-time restore window; the provider's PITR is the supported path for
  # a managed server (docs/OPERATIONS.md §4).
  backup_retention_days        = var.postgres.backup_retention_days
  geo_redundant_backup_enabled = var.postgres.geo_redundant_backup

  dynamic "high_availability" {
    for_each = var.postgres.high_availability ? [1] : []
    content {
      mode                      = "ZoneRedundant"
      standby_availability_zone = var.zones[1]
    }
  }

  maintenance_window {
    day_of_week  = 0
    start_hour   = 2
    start_minute = 0
  }

  tags       = local.tags
  depends_on = [azurerm_private_dns_zone_virtual_network_link.postgres]

  lifecycle {
    # Azure moves the primary to the standby's zone on failover.
    ignore_changes = [zone, high_availability[0].standby_availability_zone]
  }
}

resource "azurerm_postgresql_flexible_server_database" "nexus" {
  name      = "nexus"
  server_id = azurerm_postgresql_flexible_server.this.id
  charset   = "UTF8"
  collation = "en_US.utf8"
}

# TLS is already required by default; pinned here so nobody can turn it off
# without it showing up in a plan.
resource "azurerm_postgresql_flexible_server_configuration" "require_tls" {
  name      = "require_secure_transport"
  server_id = azurerm_postgresql_flexible_server.this.id
  value     = "ON"
}

resource "azurerm_postgresql_flexible_server_configuration" "log_connections" {
  name      = "log_connections"
  server_id = azurerm_postgresql_flexible_server.this.id
  value     = "ON"
}

resource "azurerm_monitor_diagnostic_setting" "postgres" {
  name                       = "to-log-analytics"
  target_resource_id         = azurerm_postgresql_flexible_server.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  # allLogs is a superset of the audit group.
  enabled_log {
    category_group = "allLogs"
  }
  enabled_metric {
    category = "AllMetrics"
  }
}

# ── Artifact storage (Blob) ───────────────────────────────────────────────
#
# PROVISIONED BUT NOT YET USED BY NEXUS. The result store speaks only the S3
# API (minio-go), and Azure Blob has no S3 endpoint. Until the native Blob
# backend exists (F115), Nexus artifacts go to the in-cluster MinIO on Premium
# SSD. This account is where that backend will write, and it is created now so
# the identity, network path and retention are settled before data arrives.

resource "azurerm_storage_account" "artifacts" {
  name                     = local.storage_name
  location                 = azurerm_resource_group.this.location
  resource_group_name      = azurerm_resource_group.this.name
  account_kind             = "StorageV2"
  account_tier             = "Standard"
  account_replication_type = var.storage_replication_type
  access_tier              = "Hot"

  min_tls_version                   = "TLS1_2"
  https_traffic_only_enabled        = true
  allow_nested_items_to_be_public   = false
  shared_access_key_enabled         = false
  default_to_oauth_authentication   = true
  infrastructure_encryption_enabled = true
  cross_tenant_replication_enabled  = false
  public_network_access             = "Disabled"
  local_user_enabled                = false
  sftp_enabled                      = false

  blob_properties {
    versioning_enabled = true
    delete_retention_policy {
      days = var.blob_soft_delete_days
    }
    container_delete_retention_policy {
      days = var.blob_soft_delete_days
    }
  }

  network_rules {
    default_action = "Deny"
    bypass         = ["AzureServices"]
  }

  tags = local.tags
}

# Blob read/write/delete audit trail in Log Analytics.
resource "azurerm_monitor_diagnostic_setting" "artifacts_blob" {
  name                       = "to-log-analytics"
  target_resource_id         = "${azurerm_storage_account.artifacts.id}/blobServices/default"
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category = "StorageRead"
  }
  enabled_log {
    category = "StorageWrite"
  }
  enabled_log {
    category = "StorageDelete"
  }
  enabled_metric {
    category = "Transaction"
  }
}

# prevent_destroy cannot be switched per environment, so the stateful
# resources get Azure delete locks instead: `terraform destroy` (or a stray
# portal click) fails until delete_locks_enabled is set false and applied.
resource "azurerm_management_lock" "data" {
  for_each = var.delete_locks_enabled ? {
    postgres  = azurerm_postgresql_flexible_server.this.id
    artifacts = azurerm_storage_account.artifacts.id
    vault     = azurerm_key_vault.this.id
  } : {}

  name       = "${var.name}-${each.key}-no-delete"
  scope      = each.value
  lock_level = "CanNotDelete"
  notes      = "Holds Nexus data. Set delete_locks_enabled = false and apply before destroying."
}

resource "azurerm_storage_container" "artifacts" {
  name                  = "nexus-results"
  storage_account_id    = azurerm_storage_account.artifacts.id
  container_access_type = "private"
}

resource "azurerm_private_dns_zone" "blob" {
  name                = "privatelink.blob.core.windows.net"
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "blob" {
  name                = "${var.name}-blob"
  private_dns_zone_id = azurerm_private_dns_zone.blob.id
  virtual_network_id  = azurerm_virtual_network.this.id
  tags                = local.tags
}

resource "azurerm_private_endpoint" "blob" {
  name                = "${var.name}-blob-pe"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.tags

  private_service_connection {
    name                           = "blob"
    private_connection_resource_id = azurerm_storage_account.artifacts.id
    subresource_names              = ["blob"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "blob"
    private_dns_zone_ids = [azurerm_private_dns_zone.blob.id]
  }
}

# The identity the native Blob backend will use (workload identity, no keys),
# with data access to the one container and nothing else.
resource "azurerm_user_assigned_identity" "nexus" {
  name                = "${var.name}-nexus-workload"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags
}

resource "azurerm_federated_identity_credential" "nexus" {
  name                      = "kubernetes-${var.nexus_kubernetes_namespace}-${var.nexus_service_account}"
  user_assigned_identity_id = azurerm_user_assigned_identity.nexus.id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = azurerm_kubernetes_cluster.this.oidc_issuer_url
  subject                   = "system:serviceaccount:${var.nexus_kubernetes_namespace}:${var.nexus_service_account}"
}

resource "azurerm_role_assignment" "nexus_blob" {
  scope                = azurerm_storage_container.artifacts.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.nexus.principal_id
}

# ── Key Vault ─────────────────────────────────────────────────────────────

resource "azurerm_key_vault" "this" {
  name                       = local.kv_name
  location                   = azurerm_resource_group.this.location
  resource_group_name        = azurerm_resource_group.this.name
  tenant_id                  = data.azurerm_client_config.current.tenant_id
  sku_name                   = "standard"
  rbac_authorization_enabled = true
  purge_protection_enabled   = true
  soft_delete_retention_days = 90

  # Terraform writes the database secret through the firewall from the IPs
  # listed; workloads read through the private endpoint.
  public_network_access_enabled = length(var.key_vault_allowed_ip_ranges) > 0
  network_acls {
    default_action = "Deny"
    bypass         = "AzureServices"
    ip_rules       = var.key_vault_allowed_ip_ranges
  }

  tags = local.tags
}

resource "azurerm_private_dns_zone" "vault" {
  name                = "privatelink.vaultcore.azure.net"
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "vault" {
  name                = "${var.name}-vault"
  private_dns_zone_id = azurerm_private_dns_zone.vault.id
  virtual_network_id  = azurerm_virtual_network.this.id
  tags                = local.tags
}

resource "azurerm_private_endpoint" "vault" {
  name                = "${var.name}-kv-pe"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  subnet_id           = azurerm_subnet.private_endpoints.id
  tags                = local.tags

  private_service_connection {
    name                           = "vault"
    private_connection_resource_id = azurerm_key_vault.this.id
    subresource_names              = ["vault"]
    is_manual_connection           = false
  }

  private_dns_zone_group {
    name                 = "vault"
    private_dns_zone_ids = [azurerm_private_dns_zone.vault.id]
  }
}

# Whoever runs Terraform manages the secrets it creates; the Nexus workload
# identity may read them and nothing more.
resource "azurerm_role_assignment" "deployer_kv" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets Officer"
  principal_id         = data.azurerm_client_config.current.object_id
}

resource "azurerm_role_assignment" "nexus_kv" {
  scope                = azurerm_key_vault.this.id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.nexus.principal_id
}

resource "azurerm_key_vault_secret" "postgres_password" {
  name             = "nexus-postgres-password"
  key_vault_id     = azurerm_key_vault.this.id
  value_wo         = random_password.postgres.result
  value_wo_version = var.postgres_password_version
  content_type     = "text/plain"
  # Rotation is a deliberate act (bump postgres_password_version); the date is
  # a reminder, not an enforcement.
  expiration_date = timeadd(plantimestamp(), "8760h")

  depends_on = [azurerm_role_assignment.deployer_kv]

  lifecycle {
    ignore_changes = [expiration_date]
  }
}

resource "azurerm_monitor_diagnostic_setting" "vault" {
  name                       = "to-log-analytics"
  target_resource_id         = azurerm_key_vault.this.id
  log_analytics_workspace_id = azurerm_log_analytics_workspace.this.id

  enabled_log {
    category_group = "audit"
  }
  enabled_metric {
    category = "AllMetrics"
  }
}
