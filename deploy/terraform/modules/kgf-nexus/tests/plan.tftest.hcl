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

run "cloud_identity_writes_no_key_pair" {
  command = plan

  variables {
    postgres_password                 = "ManagedDbPassword0123456789"
    generate_object_store_credentials = false
  }

  assert {
    condition     = length(random_password.minio) == 0 && !contains(keys(kubernetes_secret_v1.credentials.data), "minio-root-user")
    error_message = "A store reached with a cloud identity must get no S3 key pair: one would shadow the identity."
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

# ── Existing namespace and the published chart ───────────────────────────

run "creates_and_labels_the_namespace_by_default" {
  command = plan

  assert {
    condition     = length(kubernetes_namespace_v1.this) == 1 && kubernetes_namespace_v1.this[0].metadata[0].labels["pod-security.kubernetes.io/enforce"] == "baseline"
    error_message = "By default the module creates the namespace with the baseline Pod Security profile enforced."
  }
}

run "installs_into_an_existing_namespace" {
  command = plan

  variables {
    namespace        = "platform-nexus"
    create_namespace = false
  }

  assert {
    condition     = length(kubernetes_namespace_v1.this) == 0
    error_message = "With create_namespace = false the module must not create (or later delete) the namespace."
  }
  assert {
    condition     = kubernetes_secret_v1.credentials.metadata[0].namespace == "platform-nexus" && helm_release.nexus.namespace == "platform-nexus"
    error_message = "The Secrets and the release must go into the existing namespace."
  }
  assert {
    condition     = output.namespace == "platform-nexus"
    error_message = "The namespace output must name the existing namespace."
  }
}

run "installs_the_published_chart_at_one_version" {
  command = plan

  variables {
    chart_path              = null
    chart_repository        = "oci://ghcr.io/example/charts"
    chart_version           = "0.3.0"
    image_registry          = "ghcr.io/"
    image_repository_prefix = "example/kgf-nexus/"
  }

  assert {
    condition     = helm_release.nexus.repository == "oci://ghcr.io/example/charts" && helm_release.nexus.chart == "kgf-nexus" && helm_release.nexus.version == "0.3.0"
    error_message = "The published chart must be installed from its repository at the exact version given."
  }
  assert {
    condition     = yamldecode(helm_release.nexus.values[0]).image.repositoryPrefix == "example/kgf-nexus/" && yamldecode(helm_release.nexus.values[0]).image.registry == "ghcr.io/"
    error_message = "The images must be named the way the release workflow publishes them: ghcr.io/<owner>/kgf-nexus/<component>."
  }
}

run "refuses_a_repository_without_a_version" {
  command = plan

  variables {
    chart_path       = null
    chart_repository = "oci://ghcr.io/example/charts"
  }

  expect_failures = [helm_release.nexus]
}

run "refuses_both_chart_sources" {
  command = plan

  variables {
    chart_repository = "oci://ghcr.io/example/charts"
    chart_version    = "0.3.0"
  }

  expect_failures = [helm_release.nexus]
}

run "refuses_a_version_range" {
  command = plan

  variables {
    chart_path       = null
    chart_repository = "oci://ghcr.io/example/charts"
    chart_version    = "~0.3"
  }

  expect_failures = [var.chart_version]
}

run "refuses_an_invalid_namespace_name" {
  command = plan

  variables {
    namespace = "Nexus_Prod"
  }

  expect_failures = [var.namespace]
}
