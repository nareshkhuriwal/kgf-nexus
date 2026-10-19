# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# KGF Nexus on an EXISTING Azure Kubernetes Service cluster, with an existing
# PostgreSQL Flexible Server and storage account (docs/INSTALL-AKS.md).
#
# READ, never changed: the cluster and its node pools, the database server,
# the storage account (and container, unless storage.create_container), the
# optional Key Vault.
# CREATED: only what Nexus itself needs --
#   - a user-assigned identity for the result store, federated to the
#     release's result-store and agent ServiceAccounts, with Storage Blob Data
#     Contributor on the ONE container;
#   - with key_vault, a second identity with Key Vault Secrets User on that
#     vault, federated to the agents' accounts only;
#   - the Nexus release (modules/kgf-nexus): namespace (optional), generated
#     credentials, the pinned database CA bundle, the chart.
# Optionally: the Blob container, and the NVIDIA device plugin.
#
# Unlike environments/aks there is no two-phase apply: the cluster exists, so
# the kubernetes and helm providers are configured from it on the first plan.

provider "azurerm" {
  subscription_id = var.subscription_id
  # Data-plane calls authenticate with Entra ID, never an account key.
  storage_use_azuread = true
  # Register no resource providers: this root only reads the subscription's
  # existing services and creates identities and role assignments.
  resource_provider_registrations = "none"

  features {}
}

data "azurerm_client_config" "current" {}

# ── The cluster ───────────────────────────────────────────────────────────

data "azurerm_kubernetes_cluster" "this" {
  name                = var.aks_cluster.name
  resource_group_name = var.aks_cluster.resource_group_name

  lifecycle {
    # Workload identity federates a Kubernetes ServiceAccount token, signed by
    # the cluster's OIDC issuer, to an Entra identity. Without the issuer there
    # is nothing to federate and every result-store call would fail at runtime.
    postcondition {
      condition     = self.oidc_issuer_enabled && self.oidc_issuer_url != ""
      error_message = "The cluster has no OIDC issuer. Enable it and the workload identity webhook first: az aks update -g <rg> -n <cluster> --enable-oidc-issuer --enable-workload-identity (docs/INSTALL-AKS.md, \"The AKS cluster\")."
    }
    postcondition {
      condition     = var.kube_auth.mode != "azure-ad" || length(self.azure_active_directory_role_based_access_control) > 0
      error_message = "kube_auth.mode = azure-ad needs a cluster with Entra ID integration, and this one has none. Enable it (az aks update --enable-aad --aad-admin-group-object-ids <group>), or set kube_auth.mode = admin for a cluster with local accounts."
    }
    postcondition {
      condition     = var.kube_auth.mode != "admin" || length(self.kube_admin_config) > 0 || try(self.kube_config[0].client_certificate, "") != ""
      error_message = "kube_auth.mode = admin, but the cluster returns no local admin credential (local accounts are disabled, which is the right setting). Use kube_auth.mode = azure-ad."
    }
  }
}

locals {
  cluster = data.azurerm_kubernetes_cluster.this

  # admin: the AAD cluster's local admin credential when local accounts are
  # on, else (a cluster without Entra ID) the user credential, which is a
  # client certificate. azure-ad: only the endpoint and CA; the token comes
  # from kubelogin.
  kube_admin = var.kube_auth.mode == "admin"
  kube_cfg = (
    local.kube_admin && length(local.cluster.kube_admin_config) > 0
    ? local.cluster.kube_admin_config[0]
    : local.cluster.kube_config[0]
  )

  # kubelogin's arguments per credential source. 6dae42f8-... is the fixed
  # application ID of the AKS Entra ID server. spn and workloadidentity need
  # the tenant; azurecli and msi find it themselves.
  aad_tenant_id = try(local.cluster.azure_active_directory_role_based_access_control[0].tenant_id, data.azurerm_client_config.current.tenant_id)
  kube_exec = {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "kubelogin"
    args = concat(
      ["get-token", "--login", var.kube_auth.kubelogin_login, "--server-id", "6dae42f8-4368-4678-94ff-3960e28e3630"],
      contains(["spn", "workloadidentity"], var.kube_auth.kubelogin_login) ? ["--tenant-id", local.aad_tenant_id] : [],
    )
  }
}

provider "kubernetes" {
  host                   = local.kube_cfg.host
  cluster_ca_certificate = base64decode(local.kube_cfg.cluster_ca_certificate)
  client_certificate     = local.kube_admin ? base64decode(local.kube_cfg.client_certificate) : null
  client_key             = local.kube_admin ? base64decode(local.kube_cfg.client_key) : null

  dynamic "exec" {
    for_each = local.kube_admin ? [] : [local.kube_exec]
    content {
      api_version = exec.value.api_version
      command     = exec.value.command
      args        = exec.value.args
    }
  }
}

provider "helm" {
  kubernetes = {
    host                   = local.kube_cfg.host
    cluster_ca_certificate = base64decode(local.kube_cfg.cluster_ca_certificate)
    client_certificate     = local.kube_admin ? base64decode(local.kube_cfg.client_certificate) : null
    client_key             = local.kube_admin ? base64decode(local.kube_cfg.client_key) : null
    exec                   = local.kube_admin ? null : local.kube_exec
  }
}

# The cluster identity the controller's apply mode is pinned to
# (docs/OPERATIONS.md §12.3): the kube-system namespace's UID.
data "kubernetes_namespace_v1" "kube_system" {
  metadata {
    name = "kube-system"
  }
}

# ── Node pools: placement and taints ──────────────────────────────────────
#
# Nexus is pinned to node pools by the label AKS puts on every node,
# kubernetes.azure.com/agentpool=<pool>, so nothing has to be added to an
# existing pool. Each workload tolerates exactly the taints of the pool it is
# pinned to (a GPU pool's sku=gpu or nvidia.com/gpu, a Spot pool's
# kubernetes.azure.com/scalesetpriority=spot), read from the pool itself.

locals {
  pool_label = "kubernetes.azure.com/agentpool"

  # "" = not pinned (placement_for[""] is empty).
  scheduler_pool = var.scheduler.node_pool != "" ? var.scheduler.node_pool : var.control_plane_node_pool

  node_pool_names = toset(compact(concat(
    [var.control_plane_node_pool, var.scheduler.node_pool],
    [for p in var.worker_pools : p.node_pool],
  )))

  gpu_node_pools = toset([for p in var.worker_pools : p.node_pool if p.gpu_count > 0])

  # A pool whose executor is kubernetes runs its TASKS as Jobs on node_pool;
  # the agent pod itself is small and stays with the control plane.
  agent_pool_for = {
    for p in var.worker_pools : p.name => (p.executor == "kubernetes" ? var.control_plane_node_pool : p.node_pool)
  }
}

data "azurerm_kubernetes_cluster_node_pool" "nexus" {
  for_each = local.node_pool_names

  name                    = each.key
  kubernetes_cluster_name = local.cluster.name
  resource_group_name     = var.aks_cluster.resource_group_name

  lifecycle {
    postcondition {
      condition     = self.os_type == "Linux"
      error_message = "Node pool ${each.key} is not a Linux pool: every Nexus image is linux/amd64 or linux/arm64."
    }
  }
}

locals {
  pool_taints = {
    for name, pool in data.azurerm_kubernetes_cluster_node_pool.nexus : name => (pool.node_taints == null ? [] : pool.node_taints)
  }

  # "key=value:Effect" or "key:Effect" -> a toleration of exactly that taint.
  pool_tolerations = {
    for name, taints in local.pool_taints : name => [
      for t in taints : (
        can(regex("^[^=:]+=[^:]*:[A-Za-z]+$", t))
        ? {
          key      = regex("^([^=:]+)=([^:]*):([A-Za-z]+)$", t)[0]
          operator = "Equal"
          value    = regex("^([^=:]+)=([^:]*):([A-Za-z]+)$", t)[1]
          effect   = regex("^([^=:]+)=([^:]*):([A-Za-z]+)$", t)[2]
        }
        : {
          key      = regex("^([^=:]+):([A-Za-z]+)$", t)[0]
          operator = "Exists"
          value    = null
          effect   = regex("^([^=:]+):([A-Za-z]+)$", t)[1]
        }
      )
    ]
  }

  # Placement for one pool name ("" = none): a nodeSelector and the pool's
  # tolerations, with null values dropped so the chart's schema sees clean
  # Kubernetes tolerations.
  placement_for = {
    for name in concat(tolist(local.node_pool_names), [""]) : name => (
      name == "" ? { nodeSelector = {}, tolerations = [] } : {
        nodeSelector              = { (local.pool_label) = name }
        tolerations = [
          for t in local.pool_tolerations[name] : { for k, v in t : k => v if v != null }
        ]
      }
    )
  }
}

# ── The database ──────────────────────────────────────────────────────────

data "azurerm_postgresql_flexible_server" "this" {
  name                = var.postgres.server_name
  resource_group_name = var.postgres.resource_group_name
}

locals {
  postgres_owner = coalesce(var.postgres.owner_login, data.azurerm_postgresql_flexible_server.this.administrator_login)
  postgres_password = (
    var.postgres_password != null ? var.postgres_password : data.azurerm_key_vault_secret.postgres_password[0].value
  )
}

# The Azure Database for PostgreSQL root CA bundle, vendored with its upstream
# source and SHA-256 pinned (deploy/terraform/ca-bundles/refresh.sh), exactly
# as environments/aks uses it. Refused if the file no longer matches
# SHA256SUMS, so a hand edit cannot change the trust anchors unseen.
locals {
  ca_bundle_dir = "${path.module}/../../ca-bundles"
  ca_bundle_pins = {
    for l in compact(split("\n", file("${local.ca_bundle_dir}/SHA256SUMS"))) :
    element(split("  ", l), 1) => element(split("  ", l), 0)
  }
  postgres_ca_bundle_file = "azure-postgres-roots.pem"
  postgres_ca_bundle      = file("${local.ca_bundle_dir}/${local.postgres_ca_bundle_file}")
}

resource "terraform_data" "postgres_ca_bundle_pin" {
  lifecycle {
    precondition {
      condition     = sha256(local.postgres_ca_bundle) == lookup(local.ca_bundle_pins, local.postgres_ca_bundle_file, "")
      error_message = "ca-bundles/${local.postgres_ca_bundle_file} does not match its pin in ca-bundles/SHA256SUMS. Regenerate it with ca-bundles/refresh.sh --write (which verifies the upstream source) instead of editing it."
    }
  }
}

# ── The result store: Blob container and its identity ─────────────────────

data "azurerm_storage_account" "this" {
  name                = var.storage.account_name
  resource_group_name = var.storage.resource_group_name
}

data "azurerm_storage_container" "existing" {
  count = var.storage.create_container ? 0 : 1

  name               = var.storage.container_name
  storage_account_id = data.azurerm_storage_account.this.id
}

# Created through the resource manager (storage_account_id), so it works on an
# account with public network access disabled. It holds every result and log:
# prevent_destroy makes `terraform destroy` stop here instead of deleting
# them. To remove the stack and keep the data, `terraform state rm` it first.
resource "azurerm_storage_container" "created" {
  count = var.storage.create_container ? 1 : 0

  name                  = var.storage.container_name
  storage_account_id    = data.azurerm_storage_account.this.id
  container_access_type = "private"

  lifecycle {
    prevent_destroy = true
  }
}

locals {
  container_id = var.storage.create_container ? azurerm_storage_container.created[0].id : data.azurerm_storage_container.existing[0].id

  identity_prefix   = coalesce(var.identity.name_prefix, "${var.aks_cluster.name}-nexus")
  identity_rg       = coalesce(var.identity.resource_group_name, var.aks_cluster.resource_group_name)
  identity_location = coalesce(var.identity.location, local.cluster.location)

  tags = merge(var.tags, {
    "app.kubernetes.io/part-of" = "kgf-nexus"
    "managed-by"                = "terraform"
    "nexus-cluster"             = var.aks_cluster.name
  })

  # The ServiceAccounts the identities trust. A federated credential trusts an
  # ACCOUNT, not a pod, so every pod running as a trusted account can obtain
  # the identity: the accounts are split by what their pods may reach.
  #   nexus-artifacts          api-gateway (task logs), workflow-manager
  #                            (object-store sensors), artifact-gc: Blob only.
  #   nexus-agent              subprocess agents: Blob, and Key Vault secrets.
  #   nexus-agent-kubernetes   kubernetes-executor agents: the same, but they
  #                            mount an API token to create task Jobs (the
  #                            chart refuses one account with two settings).
  artifact_service_account = "nexus-artifacts"
  agent_accounts = {
    subprocess = "nexus-agent"
    kubernetes = "nexus-agent-kubernetes"
  }
  agent_subjects = {
    for e in distinct([for p in var.worker_pools : p.executor]) :
    "${var.namespace}/${local.agent_accounts[e]}" => { namespace = var.namespace, name = local.agent_accounts[e] }
  }
  # Accounts of worker-pool releases elsewhere: agents too.
  extra_subjects = { for s in var.extra_federated_service_accounts : "${s.namespace}/${s.name}" => s }

  artifact_subjects = merge(
    { "${var.namespace}/${local.artifact_service_account}" = { namespace = var.namespace, name = local.artifact_service_account } },
    local.agent_subjects,
    local.extra_subjects,
  )
  secret_subjects = merge(local.agent_subjects, local.extra_subjects)
}

resource "azurerm_user_assigned_identity" "artifacts" {
  name                = "${local.identity_prefix}-artifacts"
  location            = local.identity_location
  resource_group_name = local.identity_rg
  tags                = local.tags
}

resource "azurerm_federated_identity_credential" "artifacts" {
  for_each = local.artifact_subjects

  name                      = "kubernetes-${each.value.namespace}-${each.value.name}"
  user_assigned_identity_id = azurerm_user_assigned_identity.artifacts.id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = local.cluster.oidc_issuer_url
  subject                   = "system:serviceaccount:${each.value.namespace}:${each.value.name}"
}

# Data access to the one container and nothing else. Azure RBAC has no prefix
# scope below a container: a container per trust boundary (OPERATIONS §26.4).
resource "azurerm_role_assignment" "artifacts_blob" {
  scope                = local.container_id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.artifacts.principal_id
  # A new identity may not have replicated through Entra ID yet; stating the
  # type skips the lookup that would fail on it.
  principal_type = "ServicePrincipal"
}

# ── Key Vault (optional): the owner password and akv:// task secrets ─────

data "azurerm_key_vault" "this" {
  count = var.key_vault == null ? 0 : 1

  name                = var.key_vault.name
  resource_group_name = var.key_vault.resource_group_name

  lifecycle {
    postcondition {
      condition     = self.rbac_authorization_enabled
      error_message = "The Key Vault uses access policies. The Key Vault Secrets User role assignment this root makes only works with Azure RBAC authorization (az keyvault update --enable-rbac-authorization true, after migrating its access policies)."
    }
  }
}

data "azurerm_key_vault_secret" "postgres_password" {
  count = var.postgres.password_key_vault_secret == "" ? 0 : 1

  name         = var.postgres.password_key_vault_secret
  key_vault_id = data.azurerm_key_vault.this[0].id

  lifecycle {
    precondition {
      condition     = var.key_vault != null
      error_message = "postgres.password_key_vault_secret needs key_vault: the vault holding the secret."
    }
    postcondition {
      condition     = can(regex("^[A-Za-z0-9]{16,}$", self.value))
      error_message = "The database owner password in Key Vault must be at least 16 alphanumeric characters: it is embedded unescaped in a postgres:// URL."
    }
  }
}

# A separate identity for secrets, trusted by the AGENTS' accounts only, so
# the gateway, workflow-manager and artifact-gc (nexus-artifacts) can never
# obtain secret access. The agents select it explicitly (AZURE_CLIENT_ID from
# agentDefaults.secrets.workloadIdentityClientId) and keep the Blob identity
# for results (resultStore.azure.clientId).
resource "azurerm_user_assigned_identity" "secrets" {
  count = var.key_vault == null ? 0 : 1

  name                = "${local.identity_prefix}-secrets"
  location            = local.identity_location
  resource_group_name = local.identity_rg
  tags                = local.tags
}

resource "azurerm_federated_identity_credential" "secrets" {
  for_each = var.key_vault == null ? {} : local.secret_subjects

  name                      = "kubernetes-${each.value.namespace}-${each.value.name}"
  user_assigned_identity_id = azurerm_user_assigned_identity.secrets[0].id
  audience                  = ["api://AzureADTokenExchange"]
  issuer                    = local.cluster.oidc_issuer_url
  subject                   = "system:serviceaccount:${each.value.namespace}:${each.value.name}"
}

resource "azurerm_role_assignment" "secrets_kv" {
  count = var.key_vault == null ? 0 : 1

  scope                = data.azurerm_key_vault.this[0].id
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.secrets[0].principal_id
  principal_type       = "ServicePrincipal"
}

# ── NVIDIA device plugin (optional) ───────────────────────────────────────

# On the GPU worker pools only. The chart's own default affinity requires
# node-feature-discovery labels that AKS nodes do not carry, so it is
# replaced, not merged: without that the DaemonSet schedules nowhere and
# nvidia.com/gpu is never advertised.
resource "helm_release" "nvidia_device_plugin" {
  count = var.install_nvidia_device_plugin && length(local.gpu_node_pools) > 0 ? 1 : 0

  name             = "nvidia-device-plugin"
  namespace        = "nvidia-device-plugin"
  create_namespace = true
  repository       = "https://nvidia.github.io/k8s-device-plugin"
  chart            = "nvidia-device-plugin"
  version          = var.nvidia_device_plugin_version
  wait             = true

  values = [yamlencode({
    affinity = {
      nodeAffinity = {
        requiredDuringSchedulingIgnoredDuringExecution = {
          nodeSelectorTerms = [{
            matchExpressions = [{ key = local.pool_label, operator = "In", values = sort(tolist(local.gpu_node_pools)) }]
          }]
        }
      }
    }
    tolerations = concat(
      [{ key = "nvidia.com/gpu", operator = "Exists", effect = "NoSchedule" }],
      distinct(flatten([for p in sort(tolist(local.gpu_node_pools)) : local.placement_for[p].tolerations])),
    )
  })]
}

# ── Nexus ─────────────────────────────────────────────────────────────────

locals {
  gib = 1073741824

  # The registry name; a DNS label (it names the controller's Lease), so an
  # AKS name's capitals and underscores are folded.
  cluster_name = coalesce(var.cluster_controller.cluster_name, replace(lower(var.aks_cluster.name), "_", "-"))
  apply_mode   = var.cluster_controller.enabled && var.cluster_controller.mode == "apply"
  live_uid     = data.kubernetes_namespace_v1.kube_system.metadata[0].uid

  # The webhook reads the client id from the annotation; the chart adds the
  # pod label azure.workload.identity/use to the result-store pods itself.
  wi_annotations = { "azure.workload.identity/client-id" = azurerm_user_assigned_identity.artifacts.client_id }
  artifact_identity = {
    name        = local.artifact_service_account
    annotations = local.wi_annotations
  }

  # One scheduler per queue any pool serves, and always one for default:
  # a queue without a scheduler is never dispatched.
  queues = distinct(concat(["default"], flatten([
    for p in var.worker_pools : [for q in split(",", p.queues) : trimspace(q)]
  ])))
  schedulers = [
    for q in local.queues : merge(
      {
        name     = q == "default" ? "scheduler" : "scheduler-${q}"
        queue    = q
        replicas = var.scheduler.replicas
      },
      local.placement_for[local.scheduler_pool],
      # With an HPA the chart omits spec.replicas, so neither Helm nor this
      # root fights the autoscaler (or the controller that tunes it).
      [{
        autoscaling = {
          enabled                        = true
          minReplicas                    = try(var.scheduler.autoscaling.min_replicas, 1)
          maxReplicas                    = try(var.scheduler.autoscaling.max_replicas, 1)
          targetCPUUtilizationPercentage = try(var.scheduler.autoscaling.target_cpu_percent, 70)
        }
      }, {}][var.scheduler.autoscaling == null ? 1 : 0],
    )
  ]

  # subprocess: tasks run inside the agent pod, on the pool; the pod's memory
  # limit is what it advertises. kubernetes: the agent pod (small) stays with
  # the control plane and each attempt is a Job on the pool; cpu/memory/gpu
  # then describe the TASK capacity it hands out, and the agent pod itself
  # requests no GPU.
  #
  # The optional parts are picked as [with, without][index]: their shapes
  # differ from the empty object, which a conditional cannot return (and for
  # the GPU part it would unify them into map(string), turning the byte count
  # into a string the chart's schema refuses).
  agents = [
    for p in var.worker_pools : merge(
      {
        name         = "agent-${p.name}"
        replicas     = p.replicas
        queues       = join(",", [for q in split(",", p.queues) : trimspace(q)])
        labels       = p.labels != "" ? p.labels : "pool=${p.name},nodepool=${p.node_pool},accelerator=${p.gpu_count > 0 ? "nvidia" : "none"}"
        executors    = p.executor
        cpuMillis    = p.cpu_millis
        memoryBytes  = p.memory_gib * local.gib
        diskBytes    = p.disk_gib * local.gib
        gpuCount     = p.gpu_count
        nodeSelector = local.placement_for[local.agent_pool_for[p.name]].nodeSelector
        tolerations  = local.placement_for[local.agent_pool_for[p.name]].tolerations
        resources = {
          requests = { cpu = p.cpu_request, memory = p.memory_request }
          limits   = { memory = p.executor == "kubernetes" ? "1Gi" : "${ceil(p.memory_gib)}Gi" }
        }
      },
      [{ gpuKind = p.gpu_kind, gpuMemoryBytes = p.gpu_memory_gib * local.gib }, {}][p.gpu_count > 0 ? 0 : 1],
      [{
        serviceAccount = {
          name        = local.agent_accounts.kubernetes
          annotations = local.wi_annotations
        }
        kubernetes = {
          taskNodeSelector = local.placement_for[p.node_pool].nodeSelector
          taskTolerations  = local.placement_for[p.node_pool].tolerations
        }
      }, {}][p.executor == "kubernetes" ? 0 : 1],
      # Spot nodes are evicted with 30 s notice; the agent then drains and its
      # attempts are retried without spending max_attempts (ADR-0029).
      data.azurerm_kubernetes_cluster_node_pool.nexus[p.node_pool].priority == "Spot" ? { capacityType = "spot" } : {},
    )
  ]
}

# Cross-checks that need values only the cluster has.
resource "terraform_data" "release_checks" {
  lifecycle {
    precondition {
      condition     = !local.apply_mode || var.cluster_controller.expected_cluster_uid == local.live_uid
      error_message = "cluster_controller.expected_cluster_uid (${var.cluster_controller.expected_cluster_uid}) is not this cluster's kube-system UID (${local.live_uid}): refusing to enable apply mode on a cluster other than the one intended."
    }
    # A dedicated system pool keeps workloads off with this taint. Agents run
    # arbitrary task code: never next to CoreDNS and the metrics server. (The
    # schedulers and the control plane may go there; they tolerate the pool's
    # taints like any other pool's.)
    precondition {
      condition = alltrue([
        for p in var.worker_pools : !anytrue([
          for t in local.pool_taints[p.node_pool] : startswith(t, "CriticalAddonsOnly")
        ])
      ])
      error_message = "A worker pool's node_pool is tainted CriticalAddonsOnly (a dedicated system pool). Agents and their tasks belong on a user pool."
    }
    precondition {
      condition = alltrue([
        for p in var.worker_pools : p.gpu_count == 0 || can(regex("(?i)^standard_n", data.azurerm_kubernetes_cluster_node_pool.nexus[p.node_pool].vm_size))
      ])
      error_message = "A worker pool with gpu_count > 0 is pinned to a node pool whose VM size is not an N-series (GPU) size: its agent would request nvidia.com/gpu that no node has, and stay Pending."
    }
  }
}

module "nexus" {
  source = "../../modules/kgf-nexus"

  namespace        = var.namespace
  create_namespace = var.create_namespace
  # Every pod the chart renders, and every task pod the kubernetes executor
  # creates, passes restricted (OPERATIONS §20.17).
  pod_security_enforce = "restricted"
  release_name         = var.release_name
  nexus_namespace      = var.nexus_namespace

  chart_path       = var.chart.repository == "" ? "${path.module}/../../../helm/kgf-nexus" : null
  chart_repository = var.chart.repository == "" ? null : var.chart.repository
  chart_version    = var.chart.repository == "" ? null : var.chart.version

  image_tag               = var.image.tag
  image_registry          = var.image.registry
  image_repository_prefix = var.image.repository_prefix

  nats_credentials_dir = var.nats_credentials_dir

  # The existing server's owner login; the Secret holds no S3 key pair, so the
  # result store can only be reached with the workload identity.
  postgres_password                 = local.postgres_password
  generate_object_store_credentials = false

  postgres_ca_bundle          = local.postgres_ca_bundle
  runtime_db_roles            = var.runtime_db_roles
  runtime_db_password_version = var.runtime_db_password_version
  helm_timeout_seconds        = var.helm_timeout_seconds

  chart_values = local.chart_values

  chart_values_overrides = var.chart_values_overrides

  # The identity must be able to reach the container before the first agent
  # or gateway pod opens the store; federation before any pod starts.
  depends_on = [
    terraform_data.postgres_ca_bundle_pin,
    terraform_data.release_checks,
    azurerm_role_assignment.artifacts_blob,
    azurerm_federated_identity_credential.artifacts,
    azurerm_role_assignment.secrets_kv,
    azurerm_federated_identity_credential.secrets,
    helm_release.nvidia_device_plugin,
  ]
}

# The values this root derives from the cluster and the Azure resources. The
# kgf-nexus module layers them over its own (credentials Secret, image tag,
# NATS identities) and puts the CA bundle, the runtime logins and then
# chart_values_overrides on top.
locals {
  chart_values = merge(
    {
      # NEXUS_CLUSTER_NAME on every component, next to its pod and node: the
      # console's Infrastructure page places each instance by it.
      cluster = { name = local.cluster_name }

      image = merge(
        { digests = var.image.digests },
        length(var.image.pull_secrets) > 0 ? { pullSecrets = var.image.pull_secrets } : {},
      )

      postgres = {
        enabled  = false
        host     = data.azurerm_postgresql_flexible_server.this.fqdn
        port     = 5432
        user     = local.postgres_owner
        database = var.postgres.database
        # verify-full against the pinned Azure roots (the module mounts them).
        sslmode = "verify-full"
      }

      # The Blob container through the native backend and workload identity.
      # minio.bucket is the CONTAINER under backend azblob. clientId pins the
      # Blob identity explicitly, so the agents' secrets identity
      # (AZURE_CLIENT_ID) is never used for results.
      minio = { enabled = false, bucket = var.storage.container_name }
      resultStore = {
        backend = "azblob"
        azure = {
          account  = data.azurerm_storage_account.this.name
          auth     = "workload-identity"
          clientId = azurerm_user_assigned_identity.artifacts.client_id
        }
      }

      nats = { storage = { storageClass = var.storage_class } }

      serviceAccounts = {
        apiGateway      = local.artifact_identity
        workflowManager = local.artifact_identity
        agent           = { name = local.agent_accounts.subprocess, annotations = local.wi_annotations }
        artifactGC      = local.artifact_identity
      }

      # Control plane, console and controller on control_plane_node_pool;
      # schedulers and agents carry their own placement.
      placement  = local.placement_for[var.control_plane_node_pool]
      schedulers = local.schedulers
      agents     = local.agents

      claimPlacement = { clusterName = local.cluster_name }

      clusterController = {
        enabled     = var.cluster_controller.enabled
        clusterName = local.cluster_name
        apply = {
          enabled            = local.apply_mode
          expectedClusterUID = local.apply_mode ? var.cluster_controller.expected_cluster_uid : ""
        }
      }
    },
    var.key_vault == null ? {} : {
      agentDefaults = { secrets = { workloadIdentityClientId = azurerm_user_assigned_identity.secrets[0].client_id } }
    },
  )
}
