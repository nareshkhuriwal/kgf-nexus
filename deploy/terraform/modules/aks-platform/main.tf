# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# The Azure infrastructure one KGF Nexus environment needs, and nothing that
# lives inside Kubernetes (that is the environment root's job): network, AKS,
# registry, managed PostgreSQL, artifact storage, Key Vault and logging.
#
# Security posture, in one place:
#   - Entra ID + Azure RBAC for the cluster; local accounts disabled.
#   - Workload identity + OIDC issuer on; the kubelet identity can only pull.
#   - Azure CNI overlay with Cilium, so the chart's NetworkPolicies (NATS) are
#     actually enforced.
#   - PostgreSQL on a delegated subnet with no public endpoint; TLS required.
#   - Storage account and Key Vault: no anonymous access, TLS 1.2+, private
#     endpoints, soft delete; storage has shared keys disabled.

data "azurerm_client_config" "current" {}

resource "random_string" "suffix" {
  length  = 5
  upper   = false
  special = false
}

locals {
  compact = replace(var.name, "-", "")
  suffix  = random_string.suffix.result

  tags = merge(var.tags, {
    "app.kubernetes.io/part-of" = "kgf-nexus"
    "managed-by"                = "terraform"
    "nexus-environment"         = var.name
  })

  # Globally unique names, within each service's length rules.
  acr_name     = substr("${local.compact}${local.suffix}", 0, 50)
  storage_name = "${substr(local.compact, 0, 19)}${local.suffix}"
  kv_name      = "${substr(local.compact, 0, 16)}kv${local.suffix}"
}

resource "azurerm_resource_group" "this" {
  name     = "${var.name}-rg"
  location = var.location
  tags     = local.tags
}

# ── Logging ───────────────────────────────────────────────────────────────

resource "azurerm_log_analytics_workspace" "this" {
  name                = "${var.name}-law"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  sku                 = "PerGB2018"
  retention_in_days   = var.log_retention_days
  tags                = local.tags
}

# ── Network ───────────────────────────────────────────────────────────────

resource "azurerm_virtual_network" "this" {
  name                = "${var.name}-vnet"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  address_space       = [var.vnet_cidr]
  tags                = local.tags
}

resource "azurerm_subnet" "nodes" {
  name                 = "aks-nodes"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.nodes]
  # Nodes egress through the cluster's Standard Load Balancer (outbound_type),
  # not the implicit default outbound access Azure is retiring.
  default_outbound_access_enabled = false
}

resource "azurerm_subnet" "postgres" {
  name                 = "postgres"
  resource_group_name  = azurerm_resource_group.this.name
  virtual_network_name = azurerm_virtual_network.this.name
  address_prefixes     = [var.subnet_cidrs.postgres]
  # Nothing here needs the implicit internet egress Azure is retiring.
  default_outbound_access_enabled = false

  delegation {
    name = "postgres-flexible"
    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

resource "azurerm_subnet" "private_endpoints" {
  name                            = "private-endpoints"
  resource_group_name             = azurerm_resource_group.this.name
  virtual_network_name            = azurerm_virtual_network.this.name
  address_prefixes                = [var.subnet_cidrs.private_endpoints]
  default_outbound_access_enabled = false
  # Without this, NSG rules do not apply to private endpoints at all.
  private_endpoint_network_policies = "Enabled"
}

# Only the node subnet may reach the database port; everything else in the
# VNet (private endpoints, future subnets) may not.
resource "azurerm_network_security_group" "postgres" {
  name                = "${var.name}-postgres-nsg"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags

  security_rule {
    name                       = "allow-aks-nodes-postgres"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "5432"
    source_address_prefix      = var.subnet_cidrs.nodes
    destination_address_prefix = var.subnet_cidrs.postgres
  }

  security_rule {
    name                       = "deny-vnet-postgres"
    priority                   = 200
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "VirtualNetwork"
    destination_address_prefix = var.subnet_cidrs.postgres
  }
}

resource "azurerm_subnet_network_security_group_association" "postgres" {
  subnet_id                 = azurerm_subnet.postgres.id
  network_security_group_id = azurerm_network_security_group.postgres.id
}

# Private endpoints (Key Vault, Blob): VNet-internal only; the default rules
# already deny the internet, this makes it explicit and auditable.
resource "azurerm_network_security_group" "private_endpoints" {
  name                = "${var.name}-pe-nsg"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags

  security_rule {
    name                       = "allow-vnet-https"
    priority                   = 100
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_range     = "443"
    source_address_prefix      = "VirtualNetwork"
    destination_address_prefix = var.subnet_cidrs.private_endpoints
  }

  security_rule {
    name                       = "deny-all-inbound"
    priority                   = 4000
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}

resource "azurerm_subnet_network_security_group_association" "private_endpoints" {
  subnet_id                 = azurerm_subnet.private_endpoints.id
  network_security_group_id = azurerm_network_security_group.private_endpoints.id
}

# The node subnet deliberately has NO subnet-level NSG: AKS manages the
# node NSG in the node resource group and adds rules for LoadBalancer
# Services there; a subnet NSG it does not manage would silently block them.

# ── AKS ───────────────────────────────────────────────────────────────────

# A user-assigned control-plane identity, so its subnet permission exists
# BEFORE the cluster does (with a system-assigned one it cannot).
resource "azurerm_user_assigned_identity" "cluster" {
  name                = "${var.name}-aks-identity"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags
}

resource "azurerm_role_assignment" "cluster_subnet" {
  scope                = azurerm_subnet.nodes.id
  role_definition_name = "Network Contributor"
  principal_id         = azurerm_user_assigned_identity.cluster.principal_id
}

# The kubelet identity is ours too, not AKS-generated: its AcrPull grant then
# exists before the first node boots (no ImagePullBackOff race on a fresh
# cluster), and it survives a cluster rebuild. The control-plane identity must
# be able to assign it to the nodes.
resource "azurerm_user_assigned_identity" "kubelet" {
  name                = "${var.name}-kubelet-identity"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  tags                = local.tags
}

resource "azurerm_role_assignment" "cluster_kubelet_operator" {
  scope                = azurerm_user_assigned_identity.kubelet.id
  role_definition_name = "Managed Identity Operator"
  principal_id         = azurerm_user_assigned_identity.cluster.principal_id
}

resource "azurerm_kubernetes_cluster" "this" {
  name                = "${var.name}-aks"
  location            = azurerm_resource_group.this.location
  resource_group_name = azurerm_resource_group.this.name
  dns_prefix          = var.name
  node_resource_group = "${var.name}-nodes-rg"
  kubernetes_version  = var.kubernetes_version
  sku_tier            = var.sku_tier
  tags                = local.tags

  private_cluster_enabled = var.private_cluster_enabled
  private_dns_zone_id     = var.private_cluster_enabled ? "System" : null

  dynamic "api_server_access_profile" {
    for_each = var.private_cluster_enabled ? [] : [1]
    content {
      authorized_ip_ranges = var.api_server_authorized_ip_ranges
    }
  }

  # Entra ID only. No static admin kubeconfig can exist.
  local_account_disabled            = true
  role_based_access_control_enabled = true
  azure_active_directory_role_based_access_control {
    tenant_id              = data.azurerm_client_config.current.tenant_id
    admin_group_object_ids = var.admin_group_object_ids
    azure_rbac_enabled     = true
  }

  oidc_issuer_enabled       = true
  workload_identity_enabled = true

  # Patch releases and node images roll automatically; minor upgrades are a
  # deliberate change to kubernetes_version.
  automatic_upgrade_channel = "patch"
  node_os_upgrade_channel   = "NodeImage"

  azure_policy_enabled         = true
  image_cleaner_enabled        = true
  image_cleaner_interval_hours = 48

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.cluster.id]
  }

  kubelet_identity {
    client_id                 = azurerm_user_assigned_identity.kubelet.client_id
    object_id                 = azurerm_user_assigned_identity.kubelet.principal_id
    user_assigned_identity_id = azurerm_user_assigned_identity.kubelet.id
  }

  default_node_pool {
    name                         = "system"
    temporary_name_for_rotation  = "systemtmp"
    vm_size                      = var.system_pool.vm_size
    vnet_subnet_id               = azurerm_subnet.nodes.id
    zones                        = var.zones
    auto_scaling_enabled         = true
    min_count                    = var.system_pool.min_count
    max_count                    = var.system_pool.max_count
    only_critical_addons_enabled = true
    host_encryption_enabled      = var.host_encryption_enabled
    os_sku                       = "AzureLinux"
    max_pods                     = 110
    node_labels                  = { "nexus.khuriwalgroup.com/pool" = "system" }
    tags                         = local.tags

    upgrade_settings {
      max_surge                     = "33%"
      drain_timeout_in_minutes      = 30
      node_soak_duration_in_minutes = 0
    }
  }

  network_profile {
    network_plugin      = "azure"
    network_plugin_mode = "overlay"
    # Cilium enforces NetworkPolicy; the chart relies on it for the NATS broker.
    network_data_plane = "cilium"
    network_policy     = "cilium"
    load_balancer_sku  = "standard"
    outbound_type      = "loadBalancer"
    pod_cidr           = var.pod_cidr
    service_cidr       = var.service_cidr
    dns_service_ip     = var.dns_service_ip
  }

  oms_agent {
    log_analytics_workspace_id      = azurerm_log_analytics_workspace.this.id
    msi_auth_for_monitoring_enabled = true
  }

  # Secrets Store CSI driver with the Key Vault provider, for workloads that
  # mount Key Vault secrets through workload identity.
  key_vault_secrets_provider {
    secret_rotation_enabled  = true
    secret_rotation_interval = "2m"
  }

  storage_profile {
    disk_driver_enabled         = true
    file_driver_enabled         = false
    blob_driver_enabled         = false
    snapshot_controller_enabled = true
  }

  # Explicit node pools managed here (Manual), not Node Auto Provisioning:
  # the pools, sizes and GPU taints are part of the reviewed configuration.
  node_provisioning_profile {
    mode = "Manual"
  }

  auto_scaler_profile {
    balance_similar_node_groups = true
    expander                    = "least-waste"
    scale_down_unneeded         = "10m"
  }

  depends_on = [
    azurerm_role_assignment.cluster_subnet,
    azurerm_role_assignment.cluster_kubelet_operator,
    azurerm_role_assignment.kubelet_acr_pull,
  ]

  lifecycle {
    # The autoscaler owns node counts once the pool exists.
    ignore_changes = [default_node_pool[0].node_count]
  }
}

resource "azurerm_kubernetes_cluster_node_pool" "cpu" {
  name                        = "cpu"
  temporary_name_for_rotation = "cputmp"
  kubernetes_cluster_id       = azurerm_kubernetes_cluster.this.id
  mode                        = "User"
  vm_size                     = var.cpu_pool.vm_size
  vnet_subnet_id              = azurerm_subnet.nodes.id
  zones                       = var.zones
  auto_scaling_enabled        = true
  min_count                   = var.cpu_pool.min_count
  max_count                   = var.cpu_pool.max_count
  host_encryption_enabled     = var.host_encryption_enabled
  os_sku                      = "AzureLinux"
  max_pods                    = 110
  node_labels                 = { "nexus.khuriwalgroup.com/pool" = "cpu" }
  tags                        = local.tags

  upgrade_settings {
    max_surge                = "33%"
    drain_timeout_in_minutes = 30
  }

  lifecycle {
    ignore_changes = [node_count]
  }
}

resource "azurerm_kubernetes_cluster_node_pool" "gpu" {
  count = var.gpu_pool.enabled ? 1 : 0

  name                    = "gpu"
  kubernetes_cluster_id   = azurerm_kubernetes_cluster.this.id
  mode                    = "User"
  vm_size                 = var.gpu_pool.vm_size
  vnet_subnet_id          = azurerm_subnet.nodes.id
  zones                   = var.zones
  auto_scaling_enabled    = true
  min_count               = var.gpu_pool.min_count
  max_count               = var.gpu_pool.max_count
  host_encryption_enabled = var.host_encryption_enabled
  os_sku                  = "Ubuntu"
  # AKS installs the NVIDIA driver; the device plugin that advertises
  # nvidia.com/gpu is installed by the environment root.
  gpu_driver  = "Install"
  node_labels = { "nexus.khuriwalgroup.com/pool" = "gpu", "accelerator" = "nvidia" }
  node_taints = ["nvidia.com/gpu=present:NoSchedule"]
  tags        = local.tags

  upgrade_settings {
    max_surge = "1"
  }

  lifecycle {
    ignore_changes = [node_count]
  }
}

# ── Container registry ────────────────────────────────────────────────────

resource "azurerm_container_registry" "this" {
  name                          = local.acr_name
  location                      = azurerm_resource_group.this.location
  resource_group_name           = azurerm_resource_group.this.name
  sku                           = var.acr_sku
  admin_enabled                 = false
  anonymous_pull_enabled        = false
  public_network_access_enabled = var.acr_public_network_access_enabled
  zone_redundancy_enabled       = var.acr_sku == "Premium"
  # Regional data endpoints, so a firewall can allow exactly this registry's
  # blob traffic instead of *.blob.core.windows.net.
  data_endpoint_enabled = var.acr_sku == "Premium"
  # Untagged manifests (superseded digests) are purged after a week.
  retention_policy_in_days = var.acr_sku == "Premium" ? 7 : null
  tags                     = local.tags
}

# The kubelet identity pulls images and can do nothing else in the registry.
resource "azurerm_role_assignment" "kubelet_acr_pull" {
  scope                            = azurerm_container_registry.this.id
  role_definition_name             = "AcrPull"
  principal_id                     = azurerm_user_assigned_identity.kubelet.principal_id
  skip_service_principal_aad_check = true
}
