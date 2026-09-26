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
  }
}

variables {
  nats_credentials_dir = ""
}

run "plan_example" {
  command = plan

  assert {
    condition     = output.object_store.backend == "gcs" && output.object_store.k8s_service_account == "kgf-nexus"
    error_message = "Nexus should use native GCS through Workload Identity (the kgf-nexus ServiceAccount) by default."
  }

  assert {
    condition     = kubernetes_storage_class_v1.regional_ssd.parameters["replication-type"] == "regional-pd"
    error_message = "The StorageClass should use regional persistent disks."
  }
}

run "hmac_fallback" {
  command = plan

  variables {
    artifact_identity = "hmac"
  }

  assert {
    condition     = output.object_store.backend == "s3" && output.object_store.endpoint == "https://storage.googleapis.com"
    error_message = "The HMAC fallback should use the GCS XML (S3 interoperability) endpoint."
  }
}
