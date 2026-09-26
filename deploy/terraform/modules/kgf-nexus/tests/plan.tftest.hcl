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

run "verify_full_and_runtime_logins" {
  command = plan

  variables {
    postgres_password  = "ManagedDbPassword0123456789"
    postgres_ca_bundle = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
    runtime_db_roles   = true
  }

  assert {
    condition     = kubernetes_secret_v1.postgres_ca[0].data["ca.crt"] == var.postgres_ca_bundle
    error_message = "The CA bundle must land in the <release>-postgres-ca Secret."
  }
  assert {
    condition     = length(random_password.runtime_db) == 7 && length(kubernetes_secret_v1.db_logins) == 1
    error_message = "One generated password per runtime login (six services and artifact-gc)."
  }
  assert {
    condition     = local.database_values.postgres.sslmode == "verify-full" && local.runtime_role_values.postgres.runtimeRoles.enabled
    error_message = "A CA bundle must turn on verify-full, and runtime_db_roles the chart's runtime logins."
  }
  assert {
    condition     = !contains(keys(kubernetes_secret_v1.credentials.data), "postgres-password-scheduler")
    error_message = "Runtime passwords live in their own Secret, not next to the owner password."
  }
}

run "no_tls_material_for_in_cluster_postgres" {
  command = plan

  assert {
    condition     = length(kubernetes_secret_v1.postgres_ca) == 0 && length(random_password.runtime_db) == 0 && length(keys(local.database_values)) == 0
    error_message = "Without a bundle or runtime logins, nothing changes for the in-cluster Postgres."
  }
}

run "refuses_non_pem_bundle" {
  command = plan

  variables {
    postgres_ca_bundle = "not a certificate"
  }

  expect_failures = [var.postgres_ca_bundle]
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
