# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan with MOCKED providers: no cluster, nothing created.
#   terraform -chdir=deploy/terraform/modules/kgf-nexus test

mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "random" {}

variables {
  namespace  = "nexus"
  chart_path = "../../helm/kgf-nexus"
  image_tag  = "0123456789abcdef"
}

run "generates_credentials_for_in_cluster_stores" {
  command = plan

  assert {
    condition     = length(random_password.postgres) == 1 && length(random_password.minio) == 1
    error_message = "With no external stores, both passwords are generated."
  }
}

run "uses_external_credentials" {
  command = plan

  variables {
    postgres_password        = "ManagedDbPassword0123456789"
    object_store_credentials = { access_key_id = "AKIAEXAMPLE", secret_access_key = "example-secret" }
  }

  assert {
    condition     = length(random_password.postgres) == 0 && length(random_password.minio) == 0
    error_message = "External credentials must replace the generated ones."
  }
  assert {
    condition     = kubernetes_secret_v1.credentials.data["minio-root-user"] == "AKIAEXAMPLE"
    error_message = "The S3 access key must land in the credentials Secret."
  }
}

run "cloud_identity_writes_no_key_pair" {
  command = plan

  variables {
    postgres_password                 = "ManagedDbPassword0123456789"
    generate_object_store_credentials = false
    service_account = {
      name        = "kgf-nexus"
      annotations = { "eks.amazonaws.com/role-arn" = "arn:aws:iam::123456789012:role/nexus-artifacts" }
    }
  }

  assert {
    condition     = length(random_password.minio) == 0 && !contains(keys(kubernetes_secret_v1.credentials.data), "minio-root-user")
    error_message = "A store reached with a cloud identity must get no S3 key pair: one would shadow the identity."
  }
  assert {
    condition     = kubernetes_service_account_v1.result_store[0].metadata[0].annotations["eks.amazonaws.com/role-arn"] == "arn:aws:iam::123456789012:role/nexus-artifacts"
    error_message = "The ServiceAccount must carry the cloud-identity annotation."
  }
  assert {
    condition     = !kubernetes_service_account_v1.result_store[0].automount_service_account_token
    error_message = "The result-store ServiceAccount must not mount a Kubernetes API token."
  }
}

run "refuses_url_unsafe_password" {
  command = plan

  variables {
    postgres_password = "has/slash@and:colon0123"
  }

  expect_failures = [var.postgres_password]
}

run "refuses_latest_tag" {
  command = plan

  variables {
    image_tag = "latest"
  }

  expect_failures = [var.image_tag]
}
