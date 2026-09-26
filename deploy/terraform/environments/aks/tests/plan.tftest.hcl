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
    nexus_workload_identity_client_id = "00000000-0000-0000-0000-00000000c1d0"
  }
}

variables {
  # The example points at a real key directory; the plan needs none.
  nats_credentials_dir = ""
}

run "plan_example" {
  command = plan

  assert {
    condition     = output.registry == "kgfnexusprodcinabcde.azurecr.io"
    error_message = "The registry output should be the ACR login server."
  }

  assert {
    condition     = kubernetes_storage_class_v1.premium_ssd.parameters.skuName == "Premium_ZRS"
    error_message = "The Premium SSD StorageClass should default to Premium_ZRS."
  }
}

# The default: native Blob through workload identity, no MinIO, no key.
run "blob_backend_by_default" {
  command = plan

  assert {
    condition     = output.object_store.backend == "azblob"
    error_message = "AKS should store results in the provisioned Blob container by default."
  }

  assert {
    condition     = module.nexus.result_store_service_account == "kgf-nexus"
    error_message = "The result-store pods must run as the ServiceAccount the federated credential trusts."
  }

  assert {
    condition     = startswith(output.object_store.blob_backend_status, "enabled")
    error_message = "blob_backend_status must say the Blob backend is enabled."
  }
}

# The opt-out: the in-cluster MinIO, as before the Blob backend existed.
run "minio_when_blob_disabled" {
  command = plan

  variables {
    azure_blob_native_backend = false
  }

  assert {
    condition     = output.object_store.backend == "s3" && startswith(output.object_store.active, "in-cluster MinIO")
    error_message = "With the Blob backend off, results go to the in-cluster MinIO."
  }
}
