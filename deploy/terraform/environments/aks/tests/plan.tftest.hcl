# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan with MOCKED providers: no cloud credentials, no network,
# nothing created. The Azure platform module is replaced by fixed outputs
# (azurerm validates resource IDs even under a mock, so it cannot be planned
# offline); what this proves is that the example tfvars satisfy every root
# variable's type and validation, and that the kgf-nexus module accepts what
# this environment feeds it.
#   terraform -chdir=deploy/terraform/environments/aks test -var-file=terraform.tfvars.example

mock_provider "azurerm" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "random" {}

override_module {
  target = module.platform
  outputs = {
    kube_host                         = "https://aks.example.test:443"
    kube_cluster_ca_certificate       = "Y2E="
    kubeconfig_command                = "az aks get-credentials --resource-group rg --name aks"
    registry_login_server             = "kgfnexusprodcinabcde.azurecr.io"
    postgres_host                     = "kgf-nexus-prod-cin-pg.kgf-nexus-prod-cin.private.postgres.database.azure.com"
    postgres_user                     = "nexus"
    postgres_database                 = "nexus"
    postgres_password                 = "MockPassword0123456789abcdefABCDEF"
    postgres_password_secret_id       = "https://kv.vault.azure.net/secrets/nexus-postgres-password"
    key_vault_uri                     = "https://kv.vault.azure.net/"
    artifact_storage_account          = "kgfnexusprodcinabcde"
    artifact_container                = "nexus-results"
    nexus_workload_identity_client_id = "00000000-0000-0000-0000-000000000001"
  }
}

variables {
  # The example points at a real key directory; the plan needs none.
  nats_credentials_dir = ""
}

run "plan_example" {
  command = plan

  assert {
    condition     = local.artifact_identity.name == "nexus-artifacts" && length(local.agents) == 1
    error_message = "The result-store ServiceAccount must be the one the platform identity trusts, and the GPU agent present only with the GPU pool."
  }

  assert {
    condition     = output.registry == "kgfnexusprodcinabcde.azurecr.io"
    error_message = "The registry output should be the ACR login server."
  }

  assert {
    condition     = kubernetes_storage_class_v1.premium_ssd.parameters.skuName == "Premium_ZRS"
    error_message = "The Premium SSD StorageClass should default to Premium_ZRS."
  }
}

run "refuses_blob_backend" {
  command = plan

  variables {
    azure_blob_native_backend = true
  }

  expect_failures = [var.azure_blob_native_backend]
}

run "gpu_pool_hosts_a_gpu_agent" {
  command = plan

  variables {
    gpu_pool = merge(var.gpu_pool, { enabled = true })
  }

  assert {
    condition     = length(local.agents) == 2 && local.agents[1].gpuCount >= 1 && local.agents[1].nodeSelector["nexus.khuriwalgroup.com/pool"] == "gpu"
    error_message = "With the GPU pool on, a GPU agent requesting its devices must be placed on it."
  }
}

run "refuses_an_unpinned_ca_bundle" {
  command = plan

  override_resource {
    target = terraform_data.postgres_ca_bundle_pin
  }

  assert {
    condition     = sha256(local.postgres_ca_bundle) == local.ca_bundle_pins[local.postgres_ca_bundle_file]
    error_message = "The vendored CA bundle must match its pin in ca-bundles/SHA256SUMS."
  }
}
