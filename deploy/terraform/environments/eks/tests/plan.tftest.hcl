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
    condition     = output.object_store.endpoint == "https://s3.us-east-1.amazonaws.com"
    error_message = "Nexus should use the regional S3 endpoint."
  }

  assert {
    condition     = kubernetes_storage_class_v1.gp3.parameters.encrypted == "true"
    error_message = "The gp3 StorageClass must encrypt volumes."
  }

  assert {
    condition     = output.object_store.identity == "pod-identity" && output.object_store.service_account == "kgf-nexus"
    error_message = "The result store should authenticate with EKS Pod Identity as the kgf-nexus ServiceAccount by default."
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
