# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "namespace" {
  description = "Kubernetes namespace of the release."
  value       = module.nexus.namespace
}

output "kubeconfig_command" {
  description = "Fetch a kubeconfig for the cluster (Entra ID clusters: converted for non-interactive kubelogin)."
  value = (var.kube_auth.mode == "azure-ad"
    ? "az aks get-credentials --resource-group ${var.aks_cluster.resource_group_name} --name ${var.aks_cluster.name} && kubelogin convert-kubeconfig -l ${var.kube_auth.kubelogin_login}"
  : "az aks get-credentials --resource-group ${var.aks_cluster.resource_group_name} --name ${var.aks_cluster.name} --admin")
}

output "console_access" {
  description = "How to reach the console and the REST API. ClusterIP by default: port-forward until a Gateway (chart gateway.*) or the console Service type is configured."
  value = {
    console_url = try(var.chart_values_overrides.gateway.enabled, false) && try(var.chart_values_overrides.gateway.hosts.console, "") != "" ? "http://${var.chart_values_overrides.gateway.hosts.console}" : null
    port_forward = [
      "kubectl -n ${module.nexus.namespace} port-forward svc/ui 3000:80             # console: http://localhost:3000",
      "kubectl -n ${module.nexus.namespace} port-forward svc/api-gateway 8090:8080   # REST API: http://localhost:8090/v1",
    ]
    admin_api_key = "terraform output -raw admin_api_key"
  }
}

output "workload_identity" {
  description = "The identities this root created and exactly which ServiceAccounts each one trusts."
  value = {
    tenant_id             = data.azurerm_client_config.current.tenant_id
    oidc_issuer_url       = local.cluster.oidc_issuer_url
    artifacts_client_id   = azurerm_user_assigned_identity.artifacts.client_id
    artifacts_identity_id = azurerm_user_assigned_identity.artifacts.id
    artifacts_trusts      = [for s in values(local.artifact_subjects) : "system:serviceaccount:${s.namespace}:${s.name}"]
    secrets_client_id     = var.key_vault == null ? null : azurerm_user_assigned_identity.secrets[0].client_id
    secrets_trusts        = var.key_vault == null ? [] : [for s in values(local.secret_subjects) : "system:serviceaccount:${s.namespace}:${s.name}"]
  }
}

output "object_store" {
  description = "Where Nexus results and task logs live, and how the pods authenticate."
  value = {
    backend         = "azblob"
    storage_account = data.azurerm_storage_account.this.name
    container       = var.storage.container_name
    container_id    = local.container_id
    role            = "Storage Blob Data Contributor on the container, held by ${azurerm_user_assigned_identity.artifacts.name}"
    service_account = "${module.nexus.namespace}/${local.artifact_service_account}"
  }
}

output "database" {
  description = "Database endpoint and where its credentials live (never a password)."
  value = {
    host           = data.azurerm_postgresql_flexible_server.this.fqdn
    port           = 5432
    database       = var.postgres.database
    owner_login    = local.postgres_owner
    sslmode        = "verify-full"
    ca_bundle      = "${module.nexus.namespace}/${var.release_name}-postgres-ca (key ca.crt)"
    runtime_logins = var.runtime_db_roles ? "${module.nexus.namespace}/${var.release_name}-db-logins; the owner login is used only by the migrate, bootstrap and db-roles Jobs" : "off: every service connects as the owner login"
    k8s_secret     = "${module.nexus.namespace}/${var.release_name}-credentials (key postgres-password)"
  }
}

output "cluster_uid" {
  description = "This cluster's kube-system namespace UID: the expected_cluster_uid that pins the controller's apply mode, here and in the console."
  value       = local.live_uid
}

output "cluster_registration" {
  description = "The body for registering this cluster in the console (PUT /v1/namespaces/<nexus_namespace>/platform/clusters/<name>, or the Infrastructure page). Starts in plan mode; each binding's pool must be a worker pool saved in the namespace's platform settings."
  value = {
    path = "/v1/namespaces/${var.nexus_namespace}/platform/clusters/${local.cluster_name}"
    body = {
      provider                = "aks"
      provider_resource_id    = local.cluster.id
      expected_cluster_uid    = local.live_uid
      control_plane_namespace = module.nexus.namespace
      mode                    = "plan"
      desired_revision        = 0
      bindings = [for p in var.worker_pools : {
        kubernetes_namespace = module.nexus.namespace
        worker_deployment    = "agent-${p.name}"
        pool                 = p.name
        max_replicas         = coalesce(p.max_replicas, p.replicas)
      }]
    }
  }
}

output "cluster_controller" {
  description = "The controller's mode as installed, and how to see its leader."
  value = var.cluster_controller.enabled ? {
    cluster_name = local.cluster_name
    mode         = local.apply_mode ? "apply permitted by process and RBAC (the console registration must also say apply)" : "plan (observe and report only)"
    leader       = "kubectl -n ${module.nexus.namespace} get lease nexus-cluster-controller-${local.cluster_name} -o jsonpath='{.spec.holderIdentity}'"
  } : null
}

output "admin_api_key" {
  description = "Bootstrap admin API key: terraform output -raw admin_api_key"
  value       = module.nexus.admin_api_key
  sensitive   = true
}
