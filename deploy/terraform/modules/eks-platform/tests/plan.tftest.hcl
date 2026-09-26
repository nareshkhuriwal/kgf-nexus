# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan of the module with a MOCKED aws provider: no credentials, no
# network, nothing created. Asserts the security defaults.
#   terraform -chdir=deploy/terraform/modules/eks-platform test

mock_provider "aws" {
  mock_data "aws_availability_zones" {
    defaults = { names = ["us-east-1a", "us-east-1b", "us-east-1c"], zone_ids = ["use1-az1", "use1-az2", "use1-az4"] }
  }
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012", arn = "arn:aws:iam::123456789012:user/test" }
  }
  mock_data "aws_region" {
    defaults = { region = "us-east-1", name = "us-east-1" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws", dns_suffix = "amazonaws.com" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
  mock_data "aws_iam_session_context" {
    defaults = { issuer_arn = "arn:aws:iam::123456789012:user/test" }
  }
  mock_data "aws_eks_addon_version" {
    defaults = { version = "v1.0.0-eksbuild.1" }
  }
  mock_data "aws_ssm_parameter" {
    defaults = { value = "ami-0123456789abcdef0" }
  }
  mock_resource "aws_kms_key" {
    defaults = { arn = "arn:aws:kms:us-east-1:123456789012:key/00000000-0000-0000-0000-000000000000" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/mock" }
  }
  mock_resource "aws_iam_policy" {
    defaults = { arn = "arn:aws:iam::123456789012:policy/mock" }
  }
  mock_resource "aws_eks_cluster" {
    defaults = {
      arn      = "arn:aws:eks:us-east-1:123456789012:cluster/mock"
      identity = [{ oidc = [{ issuer = "https://oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE" }] }]
    }
  }
  mock_resource "aws_iam_openid_connect_provider" {
    defaults = { arn = "arn:aws:iam::123456789012:oidc-provider/oidc.eks.us-east-1.amazonaws.com/id/EXAMPLE" }
  }
  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:123456789012:log-group:mock" }
  }
  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::mock-bucket" }
  }
}
mock_provider "random" {}
mock_provider "tls" {}
mock_provider "time" {}

variables {
  name                         = "kgf-nexus-test"
  endpoint_public_access_cidrs = ["203.0.113.0/24"]
}

run "secure_defaults" {
  command = plan

  assert {
    condition     = !aws_db_instance.postgres.publicly_accessible && aws_db_instance.postgres.storage_encrypted
    error_message = "RDS must be private and encrypted."
  }
  assert {
    condition     = aws_db_instance.postgres.backup_retention_period >= 1
    error_message = "RDS backups (and so PITR) must be on."
  }
  assert {
    condition     = aws_s3_bucket_public_access_block.artifacts.block_public_policy && aws_s3_bucket_public_access_block.artifacts.restrict_public_buckets
    error_message = "The artifact bucket must block public access."
  }
  assert {
    condition     = one(aws_s3_bucket_server_side_encryption_configuration.artifacts.rule).apply_server_side_encryption_by_default[0].sse_algorithm == "aws:kms"
    error_message = "The artifact bucket must be KMS-encrypted."
  }
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_tag_mutability == "IMMUTABLE"])
    error_message = "ECR tags must be immutable."
  }
  assert {
    condition     = length(aws_iam_access_key.artifacts) == 0
    error_message = "No static artifact key by default: the result store uses the Pod Identity / IRSA role."
  }
  assert {
    condition     = aws_eks_pod_identity_association.artifacts.service_account == "kgf-nexus"
    error_message = "The Pod Identity association must name the result-store ServiceAccount."
  }
}

run "refuses_open_public_endpoint" {
  command = plan

  variables {
    endpoint_public_access_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [var.endpoint_public_access_cidrs]
}

run "refuses_disabled_pitr" {
  command = plan

  variables {
    postgres = {
      engine_version        = "16"
      instance_class        = "db.m7g.large"
      allocated_storage_gb  = 50
      max_storage_gb        = 500
      multi_az              = false
      backup_retention_days = 0
      deletion_protection   = true
    }
  }

  expect_failures = [var.postgres]
}
