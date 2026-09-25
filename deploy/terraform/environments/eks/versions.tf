# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.11, < 2.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.66"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 3.2"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.3"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }

  # Remote state in S3 with native lock files (no DynamoDB table needed since
  # Terraform 1.10). State holds every generated credential: encrypt the
  # bucket with KMS, block public access, version it, and limit who can read.
  #   terraform init -backend-config=backend.hcl   (see backend.hcl.example)
  backend "s3" {}
}
