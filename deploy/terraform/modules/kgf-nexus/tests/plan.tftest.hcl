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
