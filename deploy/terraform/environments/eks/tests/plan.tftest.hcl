# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan with MOCKED providers: no cloud credentials, no network,
# nothing created. The AWS platform module is replaced by fixed outputs (the
# aws provider validates ARNs even under a mock, so it cannot be planned
# offline); what this proves is that the example tfvars satisfy every root
# variable's type and validation, and that the kgf-nexus module accepts what
# this environment feeds it.
#   terraform -chdir=deploy/terraform/environments/eks test -var-file=terraform.tfvars.example

mock_provider "aws" {}
mock_provider "kubernetes" {}
mock_provider "helm" {}
mock_provider "random" {}

override_module {
  target = module.platform
  outputs = {
    region                             = "us-east-1"
    cluster_name                       = "kgf-nexus-prod-use1"
    cluster_endpoint                   = "https://EXAMPLE.gr7.us-east-1.eks.amazonaws.com"
    cluster_certificate_authority_data = "Y2E="
    kubeconfig_command                 = "aws eks update-kubeconfig --region us-east-1 --name kgf-nexus-prod-use1"
    registry                           = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
    postgres_host                      = "kgf-nexus-prod-use1-postgres.abc.us-east-1.rds.amazonaws.com"
    postgres_port                      = 5432
    postgres_user                      = "nexus"
    postgres_database                  = "nexus"
    postgres_password                  = "MockPassword0123456789abcdefABCDEF"
    postgres_secret_arn                = "arn:aws:secretsmanager:us-east-1:123456789012:secret:kgf-nexus-prod-use1/postgres-AbCdEf"
    artifact_bucket                    = "kgf-nexus-prod-use1-results-abc123"
    artifact_s3_endpoint               = "https://s3.us-east-1.amazonaws.com"
    artifact_role_arn                  = "arn:aws:iam::123456789012:role/kgf-nexus-prod-use1-nexus-artifacts"
    artifact_credentials               = { access_key_id = "AKIAEXAMPLE", secret_access_key = "mock-secret" }
    kms_key_arn                        = "arn:aws:kms:us-east-1:123456789012:key/00000000-0000-0000-0000-000000000000"
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
    condition     = output.object_store.endpoint == "https://s3.us-east-1.amazonaws.com"
    error_message = "Nexus should use the regional S3 endpoint."
  }

  assert {
    condition     = kubernetes_storage_class_v1.gp3.parameters.encrypted == "true"
    error_message = "The gp3 StorageClass must encrypt volumes."
  }

  assert {
    condition     = output.object_store.identity == "pod-identity" && output.object_store.service_account == "nexus-artifacts"
    error_message = "The result store should authenticate with EKS Pod Identity as the nexus-artifacts ServiceAccount by default."
  }
}

run "irsa" {
  command = plan

  variables {
    artifact_identity = "irsa"
  }

  assert {
    condition     = startswith(output.object_store.credentials, "IRSA")
    error_message = "artifact_identity = irsa should use the role through IRSA."
  }

  assert {
    condition     = local.artifact_identity.annotations["eks.amazonaws.com/role-arn"] == "arn:aws:iam::123456789012:role/kgf-nexus-prod-use1-nexus-artifacts"
    error_message = "Under IRSA the chart-created nexus-artifacts ServiceAccount must carry the role annotation."
  }
}

run "refuses_missing_static_credentials" {
  command = plan

  variables {
    artifact_identity = "static"
  }

  override_module {
    target = module.platform
    outputs = {
      region                             = "us-east-1"
      cluster_name                       = "kgf-nexus-prod-use1"
      cluster_endpoint                   = "https://EXAMPLE.gr7.us-east-1.eks.amazonaws.com"
      cluster_certificate_authority_data = "Y2E="
      registry                           = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
      postgres_host                      = "db.example"
      postgres_port                      = 5432
      postgres_user                      = "nexus"
      postgres_database                  = "nexus"
      postgres_password                  = "MockPassword0123456789abcdefABCDEF"
      artifact_bucket                    = "bucket"
      artifact_s3_endpoint               = "https://s3.us-east-1.amazonaws.com"
      artifact_role_arn                  = "arn:aws:iam::123456789012:role/kgf-nexus-prod-use1-nexus-artifacts"
      artifact_credentials               = null
      kms_key_arn                        = "arn:aws:kms:us-east-1:123456789012:key/00000000-0000-0000-0000-000000000000"
    }
  }

  expect_failures = [terraform_data.artifact_credentials_guard]
}

run "gpu_pool_hosts_a_gpu_agent" {
  command = plan

  variables {
    gpu_node_group = merge(var.gpu_node_group, { enabled = true })
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

# The device plugin must actually land on the GPU pool: its chart's default
# affinity needs node-feature-discovery labels the nodes do not have.
run "device_plugin_schedules_on_the_gpu_pool" {
  command = plan

  variables {
    gpu_node_group = merge(var.gpu_node_group, { enabled = true })
  }

  assert {
    condition = (yamldecode(helm_release.nvidia_device_plugin[0].values[0]).affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms ==
    [{ matchExpressions = [{ key = "nexus.khuriwalgroup.com/pool", operator = "In", values = ["gpu"] }] }])
    error_message = "The device plugin's affinity must replace the chart default and select the GPU pool's label."
  }
}
