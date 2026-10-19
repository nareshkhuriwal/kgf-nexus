# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan with MOCKED providers: no cloud credentials, no cluster, no
# network, nothing created. The existing Azure resources are mocked data
# sources (azurerm parses resource IDs even under a mock, so every ID is well
# formed). What this proves: the example tfvars satisfy every variable's type
# and validation, the identities trust the right ServiceAccounts and reach
# only the right scopes, the chart values place each workload on its pool,
# and every refusal fires.
#   terraform -chdir=deploy/terraform/environments/aks-existing test -var-file=terraform.tfvars.example

mock_provider "azurerm" {
  # The identities' client IDs and the container ID feed the chart values and
  # role scopes the runs assert on; give them their mocked values at plan.
  override_during = plan

  mock_data "azurerm_client_config" {
    defaults = {
      tenant_id       = "00000000-0000-0000-0000-0000000000aa"
      object_id       = "00000000-0000-0000-0000-0000000000bb"
      subscription_id = "00000000-0000-0000-0000-000000000000"
    }
  }
  mock_data "azurerm_kubernetes_cluster" {
    defaults = {
      id                  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks"
      location            = "westeurope"
      oidc_issuer_enabled = true
      oidc_issuer_url     = "https://westeurope.oic.prod-aks.azure.com/00000000-0000-0000-0000-0000000000aa/11111111-1111-1111-1111-111111111111/"
      kube_config = [{
        host                   = "https://prod-weu-aks.hcp.westeurope.azmk8s.io:443"
        cluster_ca_certificate = "Y2E="
        client_certificate     = ""
        client_key             = ""
        username               = "clusterUser_prod-weu-aks-rg_prod-weu-aks"
        password               = ""
      }]
      kube_admin_config = []
      azure_active_directory_role_based_access_control = [{
        tenant_id              = "00000000-0000-0000-0000-0000000000aa"
        azure_rbac_enabled     = true
        admin_group_object_ids = []
      }]
    }
  }
  mock_data "azurerm_kubernetes_cluster_node_pool" {
    defaults = {
      id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/general"
      os_type     = "Linux"
      mode        = "User"
      priority    = "Regular"
      vm_size     = "Standard_D4as_v5"
      node_taints = []
    }
  }
  mock_data "azurerm_postgresql_flexible_server" {
    defaults = {
      id                  = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-data-rg/providers/Microsoft.DBforPostgreSQL/flexibleServers/prod-weu-pg"
      fqdn                = "prod-weu-pg.postgres.database.azure.com"
      administrator_login = "pgadmin"
      version             = "16"
    }
  }
  mock_data "azurerm_storage_account" {
    defaults = {
      id                    = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-data-rg/providers/Microsoft.Storage/storageAccounts/prodweunexusresults"
      primary_blob_endpoint = "https://prodweunexusresults.blob.core.windows.net/"
    }
  }
  mock_data "azurerm_storage_container" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-data-rg/providers/Microsoft.Storage/storageAccounts/prodweunexusresults/blobServices/default/containers/nexus-results"
    }
  }
  mock_data "azurerm_key_vault" {
    defaults = {
      id                         = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-data-rg/providers/Microsoft.KeyVault/vaults/prod-weu-nexus-kv"
      vault_uri                  = "https://prod-weu-nexus-kv.vault.azure.net/"
      rbac_authorization_enabled = true
    }
  }
  mock_data "azurerm_key_vault_secret" {
    defaults = {
      value = "OwnerPasswordFromKeyVault0123456789"
    }
  }
  mock_resource "azurerm_user_assigned_identity" {
    defaults = {
      id           = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/prod-weu-aks-nexus-artifacts"
      client_id    = "00000000-0000-0000-0000-00000000c1d0"
      principal_id = "00000000-0000-0000-0000-00000000c1d1"
    }
  }
  mock_resource "azurerm_storage_container" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-data-rg/providers/Microsoft.Storage/storageAccounts/prodweunexusresults/blobServices/default/containers/nexus-results"
    }
  }
}

mock_provider "kubernetes" {
  mock_data "kubernetes_namespace_v1" {
    defaults = {
      metadata = { name = "kube-system", uid = "6f2d7c1e-0a9b-4c8d-b7e6-5f4a3b2c1d0e" }
    }
  }
}
mock_provider "helm" {}
mock_provider "random" {}

# The example's GPU pool: an N-series pool with AKS's recommended GPU taint.
override_data {
  target = data.azurerm_kubernetes_cluster_node_pool.nexus["gput4"]
  values = {
    id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/gput4"
    os_type     = "Linux"
    mode        = "User"
    priority    = "Regular"
    vm_size     = "Standard_NC4as_T4_v3"
    node_taints = ["sku=gpu:NoSchedule"]
  }
}

variables {
  # The example points at a real key directory; the plan needs none.
  nats_credentials_dir = ""
}

# ── The example ───────────────────────────────────────────────────────────

run "plan_example" {
  command = plan

  assert {
    condition     = azurerm_user_assigned_identity.artifacts.name == "prod-weu-aks-nexus-artifacts" && azurerm_user_assigned_identity.artifacts.resource_group_name == "prod-weu-aks-rg" && azurerm_user_assigned_identity.artifacts.location == "westeurope"
    error_message = "The result-store identity is created next to the cluster, named after it."
  }
  assert {
    condition     = azurerm_federated_identity_credential.artifacts["nexus/nexus-artifacts"].subject == "system:serviceaccount:nexus:nexus-artifacts" && azurerm_federated_identity_credential.artifacts["nexus/nexus-artifacts"].issuer == data.azurerm_kubernetes_cluster.this.oidc_issuer_url
    error_message = "The identity must trust the release's nexus-artifacts ServiceAccount, through the cluster's OIDC issuer."
  }
  assert {
    condition     = keys(azurerm_federated_identity_credential.artifacts) == ["nexus/nexus-agent", "nexus/nexus-artifacts"] && azurerm_federated_identity_credential.artifacts["nexus/nexus-artifacts"].audience == tolist(["api://AzureADTokenExchange"])
    error_message = "The Blob identity trusts the result-store account and the agents' account, with the token-exchange audience the webhook requests."
  }
  assert {
    condition     = azurerm_role_assignment.artifacts_blob.role_definition_name == "Storage Blob Data Contributor" && endswith(azurerm_role_assignment.artifacts_blob.scope, "/blobServices/default/containers/nexus-results")
    error_message = "The result-store identity gets Storage Blob Data Contributor on the one container, not the account."
  }
  assert {
    condition     = length(azurerm_storage_container.created) == 1 && azurerm_storage_container.created[0].container_access_type == "private"
    error_message = "create_container makes a private container."
  }
  assert {
    condition     = length(azurerm_user_assigned_identity.secrets) == 1 && azurerm_role_assignment.secrets_kv[0].role_definition_name == "Key Vault Secrets User" && endswith(azurerm_role_assignment.secrets_kv[0].scope, "/vaults/prod-weu-nexus-kv")
    error_message = "With a vault, a separate identity gets Key Vault Secrets User on that vault only."
  }
  assert {
    condition     = keys(azurerm_federated_identity_credential.secrets) == ["nexus/nexus-agent"]
    error_message = "The secrets identity trusts the agents' account ONLY: a federated credential trusts every pod of an account, and nexus-artifacts also runs the gateway, workflow-manager and artifact-gc."
  }
  assert {
    condition     = local.postgres_password == "OwnerPasswordFromKeyVault0123456789" && local.postgres_owner == "nexus"
    error_message = "The owner password comes from the named Key Vault secret, for the named owner login."
  }
}

run "chart_values_place_every_workload" {
  command = plan

  assert {
    condition     = local.chart_values.resultStore.backend == "azblob" && local.chart_values.resultStore.azure.auth == "workload-identity" && local.chart_values.resultStore.azure.account == "prodweunexusresults" && local.chart_values.minio.bucket == "nexus-results" && !local.chart_values.minio.enabled
    error_message = "Results go to the existing account's container through the native Blob backend and workload identity; no MinIO."
  }
  assert {
    condition     = local.chart_values.resultStore.azure.clientId == "00000000-0000-0000-0000-00000000c1d0" && local.chart_values.serviceAccounts.agent.name == "nexus-agent" && local.chart_values.serviceAccounts.agent.annotations["azure.workload.identity/client-id"] == "00000000-0000-0000-0000-00000000c1d0" && local.chart_values.serviceAccounts.artifactGC.name == "nexus-artifacts"
    error_message = "The result-store pods run as federated accounts, the agents as their own, and the Blob client is pinned to the Blob identity."
  }
  assert {
    condition     = local.chart_values.agentDefaults.secrets.workloadIdentityClientId == azurerm_user_assigned_identity.secrets[0].client_id
    error_message = "The agents select the secrets identity for akv://."
  }
  assert {
    condition     = local.chart_values.postgres.host == "prod-weu-pg.postgres.database.azure.com" && local.chart_values.postgres.sslmode == "verify-full" && !local.chart_values.postgres.enabled
    error_message = "The existing Flexible Server, verified with verify-full."
  }
  assert {
    condition     = local.chart_values.placement.nodeSelector["kubernetes.azure.com/agentpool"] == "general" && length(local.chart_values.placement.tolerations) == 0
    error_message = "The control plane is pinned to control_plane_node_pool by the label AKS puts on every node."
  }
  assert {
    condition     = [for s in local.chart_values.schedulers : s.name] == ["scheduler", "scheduler-gpu"] && alltrue([for s in local.chart_values.schedulers : s.replicas == 2 && s.nodeSelector["kubernetes.azure.com/agentpool"] == "general"])
    error_message = "One scheduler per served queue (default always), on the scheduler pool."
  }
  assert {
    condition     = local.chart_values.agents[0].name == "agent-cpu" && local.chart_values.agents[0].nodeSelector["kubernetes.azure.com/agentpool"] == "cpu" && local.chart_values.agents[0].gpuCount == 0 && !contains(keys(local.chart_values.agents[0]), "gpuKind")
    error_message = "The CPU pool's agent is pinned to its pool and advertises no GPU."
  }
  assert {
    condition = (local.chart_values.agents[1].gpuCount == 1 && local.chart_values.agents[1].gpuKind == "nvidia-t4" &&
      jsonencode(local.chart_values.agents[1].gpuMemoryBytes) == "17179869184" &&
      local.chart_values.agents[1].nodeSelector["kubernetes.azure.com/agentpool"] == "gput4" &&
    jsonencode(local.chart_values.agents[1].tolerations) == jsonencode([{ effect = "NoSchedule", key = "sku", operator = "Equal", value = "gpu" }]))
    error_message = "The GPU pool's agent advertises its devices (the byte count a number) and tolerates exactly the pool's own taint, read from the pool."
  }
  assert {
    condition     = local.chart_values.cluster.name == "prod-weu-aks" && local.chart_values.clusterController.clusterName == "prod-weu-aks" && local.chart_values.clusterController.enabled && !local.chart_values.clusterController.apply.enabled
    error_message = "The controller reports the cluster under its AKS name, in plan mode."
  }
  assert {
    condition     = output.cluster_registration.body.expected_cluster_uid == "6f2d7c1e-0a9b-4c8d-b7e6-5f4a3b2c1d0e" && output.cluster_registration.body.mode == "plan" && output.cluster_registration.body.bindings[1].worker_deployment == "agent-gpu" && output.cluster_registration.body.bindings[1].max_replicas == 4
    error_message = "The registration output pins the live kube-system UID, starts in plan mode and binds every agent Deployment."
  }
  assert {
    condition     = local.chart_values.nats.storage.storageClass == "managed-csi-premium"
    error_message = "NATS JetStream goes on AKS's built-in Premium SSD class."
  }
}

run "device_plugin_targets_gpu_pools_only" {
  command = plan

  assert {
    condition     = length(helm_release.nvidia_device_plugin) == 1
    error_message = "install_nvidia_device_plugin with a GPU pool installs the plugin."
  }
  assert {
    condition = (yamldecode(helm_release.nvidia_device_plugin[0].values[0]).affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms ==
    [{ matchExpressions = [{ key = "kubernetes.azure.com/agentpool", operator = "In", values = ["gput4"] }] }])
    error_message = "The plugin's affinity must REPLACE the chart default (which needs NFD labels AKS nodes lack) and select the GPU pools."
  }
  assert {
    condition     = contains(yamldecode(helm_release.nvidia_device_plugin[0].values[0]).tolerations, { key = "sku", operator = "Equal", value = "gpu", effect = "NoSchedule" })
    error_message = "The plugin tolerates the GPU pool's own taint."
  }
}

# ── Variations ────────────────────────────────────────────────────────────

run "minimal_without_vault_into_an_existing_namespace" {
  command = plan

  variables {
    key_vault               = null
    postgres                = { server_name = "prod-weu-pg", resource_group_name = "prod-weu-data-rg" }
    postgres_password       = "OwnerPasswordFromTfVar0123456789"
    storage                 = { account_name = "prodweunexusresults", resource_group_name = "prod-weu-data-rg" }
    create_namespace        = false
    namespace               = "platform-nexus"
    control_plane_node_pool = ""
    scheduler               = {}
    worker_pools            = [{ name = "cpu", node_pool = "cpu", cpu_millis = 3500, memory_gib = 12 }]
  }

  assert {
    condition     = length(azurerm_user_assigned_identity.secrets) == 0 && length(azurerm_role_assignment.secrets_kv) == 0 && length(azurerm_federated_identity_credential.secrets) == 0 && !contains(keys(local.chart_values), "agentDefaults")
    error_message = "Without a vault there is no secrets identity and no secret access at all."
  }
  assert {
    condition     = length(azurerm_storage_container.created) == 0 && local.container_id == data.azurerm_storage_container.existing[0].id
    error_message = "An existing container is used as it is."
  }
  assert {
    condition     = local.postgres_owner == "pgadmin" && local.postgres_password == "OwnerPasswordFromTfVar0123456789"
    error_message = "owner_login \"\" falls back to the server's administrator login."
  }
  assert {
    condition     = output.namespace == "platform-nexus" && azurerm_federated_identity_credential.artifacts["platform-nexus/nexus-artifacts"].subject == "system:serviceaccount:platform-nexus:nexus-artifacts"
    error_message = "The existing namespace is used, and the federated subject names it."
  }
  assert {
    condition     = [for s in local.chart_values.schedulers : s.name] == ["scheduler"] && length(local.chart_values.placement.nodeSelector) == 0 && length(local.chart_values.schedulers[0].nodeSelector) == 0
    error_message = "With no control-plane or scheduler pool nothing is pinned, and only the default queue gets a scheduler."
  }
  assert {
    condition     = length(helm_release.nvidia_device_plugin) == 0
    error_message = "No GPU pool, no device plugin."
  }
}

run "worker_pool_release_in_another_namespace_is_federated" {
  command = plan

  variables {
    extra_federated_service_accounts = [{ namespace = "nexus-pool-gpu", name = "nexus-agent" }]
  }

  assert {
    condition     = length(azurerm_federated_identity_credential.artifacts) == 3 && azurerm_federated_identity_credential.artifacts["nexus-pool-gpu/nexus-agent"].subject == "system:serviceaccount:nexus-pool-gpu:nexus-agent" && contains(keys(azurerm_federated_identity_credential.secrets), "nexus-pool-gpu/nexus-agent")
    error_message = "A worker-pool release's agent account in another namespace gets its own federated credential on both identities."
  }
}

run "spot_pool_agent_is_marked_spot" {
  command = plan

  override_data {
    target = data.azurerm_kubernetes_cluster_node_pool.nexus["cpu"]
    values = {
      id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/cpu"
      os_type     = "Linux"
      mode        = "User"
      priority    = "Spot"
      vm_size     = "Standard_D4as_v5"
      node_taints = ["kubernetes.azure.com/scalesetpriority=spot:NoSchedule"]
    }
  }

  assert {
    condition     = local.chart_values.agents[0].capacityType == "spot" && jsonencode(local.chart_values.agents[0].tolerations) == jsonencode([{ effect = "NoSchedule", key = "kubernetes.azure.com/scalesetpriority", operator = "Equal", value = "spot" }])
    error_message = "An agent on a Spot pool tolerates the Spot taint and reports capacityType spot (evictions are then retried, not charged)."
  }
}

run "schedulers_may_use_the_system_pool" {
  command = plan

  variables {
    scheduler = { node_pool = "system", replicas = 2 }
  }

  override_data {
    target = data.azurerm_kubernetes_cluster_node_pool.nexus["system"]
    values = {
      id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/system"
      os_type     = "Linux"
      mode        = "System"
      priority    = "Regular"
      vm_size     = "Standard_D4as_v5"
      node_taints = ["CriticalAddonsOnly=true:NoSchedule"]
    }
  }

  assert {
    condition = (local.chart_values.schedulers[0].nodeSelector["kubernetes.azure.com/agentpool"] == "system" &&
    jsonencode(local.chart_values.schedulers[0].tolerations) == jsonencode([{ effect = "NoSchedule", key = "CriticalAddonsOnly", operator = "Equal", value = "true" }]))
    error_message = "The schedulers may share the system pool (small, and dispatch stops without them); they then tolerate its taint, read from the pool."
  }
}

run "kubernetes_executor_pool_runs_tasks_on_its_pool" {
  command = plan

  variables {
    worker_pools = [{
      name       = "gpu", node_pool = "gput4", queues = "gpu", executor = "kubernetes",
      cpu_millis = 32000, memory_gib = 220, gpu_count = 2, gpu_kind = "nvidia-a100", gpu_memory_gib = 80
    }]
  }

  assert {
    condition = (local.chart_values.agents[0].nodeSelector["kubernetes.azure.com/agentpool"] == "general" &&
      local.chart_values.agents[0].kubernetes.taskNodeSelector["kubernetes.azure.com/agentpool"] == "gput4" &&
    jsonencode(local.chart_values.agents[0].kubernetes.taskTolerations) == jsonencode([{ effect = "NoSchedule", key = "sku", operator = "Equal", value = "gpu" }]))
    error_message = "A kubernetes-executor agent stays on the control-plane pool and places its task Jobs on the worker pool, tolerating its taint."
  }
  assert {
    condition     = local.chart_values.agents[0].serviceAccount.name == "nexus-agent-kubernetes" && azurerm_federated_identity_credential.artifacts["nexus/nexus-agent-kubernetes"].subject == "system:serviceaccount:nexus:nexus-agent-kubernetes" && !contains(keys(azurerm_federated_identity_credential.artifacts), "nexus/nexus-agent")
    error_message = "Its agents mount a token, so they get their own account, federated to the same identity."
  }
  assert {
    condition     = local.chart_values.agents[0].resources.limits.memory == "1Gi"
    error_message = "The agent pod itself stays small: the advertised memory is the task pool's."
  }
}

run "kubelogin_for_ci_names_the_tenant" {
  command = plan

  variables {
    kube_auth = { kubelogin_login = "workloadidentity" }
  }

  assert {
    condition     = local.kube_exec.args == ["get-token", "--login", "workloadidentity", "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630", "--tenant-id", "00000000-0000-0000-0000-0000000000aa"]
    error_message = "kubelogin's workload-identity login needs the cluster's Entra tenant."
  }
}

run "apply_mode_pinned_to_the_live_cluster" {
  command = plan

  variables {
    cluster_controller = { mode = "apply", expected_cluster_uid = "6f2d7c1e-0a9b-4c8d-b7e6-5f4a3b2c1d0e" }
  }

  assert {
    condition     = local.chart_values.clusterController.apply.enabled && local.chart_values.clusterController.apply.expectedClusterUID == "6f2d7c1e-0a9b-4c8d-b7e6-5f4a3b2c1d0e"
    error_message = "Apply mode passes the pinned UID to the chart, which checks it again at install."
  }
}

# ── Refusals ──────────────────────────────────────────────────────────────

run "refuses_a_cluster_without_oidc_issuer" {
  command = plan

  override_data {
    target = data.azurerm_kubernetes_cluster.this
    values = {
      id                                               = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks"
      location                                         = "westeurope"
      oidc_issuer_enabled                              = false
      oidc_issuer_url                                  = ""
      kube_config                                      = [{ host = "https://x.example.test:443", cluster_ca_certificate = "Y2E=", client_certificate = "", client_key = "", username = "u", password = "" }]
      kube_admin_config                                = []
      azure_active_directory_role_based_access_control = [{ tenant_id = "00000000-0000-0000-0000-0000000000aa", azure_rbac_enabled = true, admin_group_object_ids = [] }]
    }
  }

  expect_failures = [data.azurerm_kubernetes_cluster.this]
}

run "refuses_admin_credentials_when_local_accounts_are_off" {
  command = plan

  variables {
    kube_auth = { mode = "admin" }
  }

  expect_failures = [data.azurerm_kubernetes_cluster.this]
}

run "refuses_agents_on_a_dedicated_system_pool" {
  command = plan

  override_data {
    target = data.azurerm_kubernetes_cluster_node_pool.nexus["cpu"]
    values = {
      id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/cpu"
      os_type     = "Linux"
      mode        = "System"
      priority    = "Regular"
      vm_size     = "Standard_D4as_v5"
      node_taints = ["CriticalAddonsOnly=true:NoSchedule"]
    }
  }

  expect_failures = [terraform_data.release_checks]
}

run "refuses_a_windows_pool" {
  command = plan

  override_data {
    target = data.azurerm_kubernetes_cluster_node_pool.nexus["cpu"]
    values = {
      id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/cpu"
      os_type     = "Windows"
      mode        = "User"
      priority    = "Regular"
      vm_size     = "Standard_D4as_v5"
      node_taints = []
    }
  }

  expect_failures = [data.azurerm_kubernetes_cluster_node_pool.nexus]
}

run "refuses_a_gpu_agent_on_a_cpu_vm_size" {
  command = plan

  override_data {
    target = data.azurerm_kubernetes_cluster_node_pool.nexus["gput4"]
    values = {
      id          = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-aks-rg/providers/Microsoft.ContainerService/managedClusters/prod-weu-aks/agentPools/gput4"
      os_type     = "Linux"
      mode        = "User"
      priority    = "Regular"
      vm_size     = "Standard_D8as_v5"
      node_taints = []
    }
  }

  expect_failures = [terraform_data.release_checks]
}

run "refuses_apply_mode_without_the_cluster_uid" {
  command = plan

  variables {
    cluster_controller = { mode = "apply" }
  }

  expect_failures = [var.cluster_controller]
}

run "refuses_apply_mode_pinned_to_another_cluster" {
  command = plan

  variables {
    cluster_controller = { mode = "apply", expected_cluster_uid = "00000000-1111-2222-3333-444444444444" }
  }

  expect_failures = [terraform_data.release_checks]
}

run "refuses_an_access_policy_vault" {
  command = plan

  override_data {
    target = data.azurerm_key_vault.this[0]
    values = {
      id                         = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/prod-weu-data-rg/providers/Microsoft.KeyVault/vaults/prod-weu-nexus-kv"
      rbac_authorization_enabled = false
    }
  }

  expect_failures = [data.azurerm_key_vault.this]
}

run "refuses_two_password_sources" {
  command = plan

  variables {
    postgres_password = "OwnerPasswordFromTfVar0123456789"
  }

  expect_failures = [var.postgres_password]
}

run "refuses_no_password_source" {
  command = plan

  variables {
    postgres = { server_name = "prod-weu-pg", resource_group_name = "prod-weu-data-rg" }
  }

  expect_failures = [var.postgres_password]
}

run "refuses_a_moving_image_tag" {
  command = plan

  variables {
    image = { tag = "main" }
  }

  expect_failures = [var.image]
}

run "refuses_duplicate_worker_pools" {
  command = plan

  variables {
    worker_pools = [
      { name = "cpu", node_pool = "cpu", cpu_millis = 2000, memory_gib = 4 },
      { name = "cpu", node_pool = "cpu", cpu_millis = 2000, memory_gib = 4 },
    ]
  }

  expect_failures = [var.worker_pools]
}

run "refuses_a_chart_repository_without_a_version" {
  command = plan

  variables {
    chart = { repository = "oci://ghcr.io/nareshkhuriwal/charts" }
  }

  expect_failures = [var.chart]
}
