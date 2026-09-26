# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan with MOCKED providers: no cloud credentials, no network,
# nothing created. The GCP platform module is replaced by fixed outputs, so
# this proves the example tfvars satisfy every root variable's type and
# validation, and that the kgf-nexus module accepts what this environment
# feeds it (modules/gke-platform has its own mocked plan test).
#   terraform -chdir=deploy/terraform/environments/gke test -var-file=terraform.tfvars.example

mock_provider "google" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "random" {}

override_module {
  target = module.platform
  outputs = {
    cluster_name                      = "kgf-nexus-prod-usc1"
    cluster_endpoint                  = "203.0.113.10"
    cluster_dns_endpoint              = "gke-0123456789.us-central1.gke.goog"
    cluster_ca_certificate            = "Y2E="
    kubeconfig_command                = "gcloud container clusters get-credentials kgf-nexus-prod-usc1 --region us-central1 --project my-nexus-project"
    registry                          = "us-central1-docker.pkg.dev/my-nexus-project/"
    postgres_host                     = "10.100.0.3"
    postgres_instance_connection_name = "my-nexus-project:us-central1:kgf-nexus-prod-usc1-pg-abc123"
    postgres_user                     = "nexus"
    postgres_database                 = "nexus"
    postgres_password                 = "MockPassword0123456789abcdefABCDEF"
    postgres_password_secret          = "projects/my-nexus-project/secrets/kgf-nexus-prod-usc1-postgres-password"
    artifact_bucket                   = "kgf-nexus-prod-usc1-results-abc123"
    artifact_hmac_credentials         = { access_key_id = "GOOGEXAMPLE", secret_access_key = "mock-secret" }
    artifact_service_account          = "kgf-nexus-prod-usc1-art@my-nexus-project.iam.gserviceaccount.com"
    postgres_ca_bundle                = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"
  }
}

variables {
  nats_credentials_dir = ""
}

run "plan_example" {
  command = plan

  assert {
    condition     = local.artifact_identity.name == "nexus-artifacts" && length(local.agents) == 1
    error_message = "The result-store ServiceAccount must be the one the platform identity trusts, and the GPU agent present only with the GPU pool."
  }

  assert {
    condition     = output.object_store.endpoint == "https://storage.googleapis.com"
    error_message = "Nexus should use the GCS XML (S3 interoperability) endpoint."
  }

  assert {
    condition     = kubernetes_storage_class_v1.regional_ssd.parameters["replication-type"] == "regional-pd"
    error_message = "The StorageClass should use regional persistent disks."
  }
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
