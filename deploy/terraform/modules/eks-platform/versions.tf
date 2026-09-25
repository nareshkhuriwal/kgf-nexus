# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.11, < 2.0"

  required_providers {
    # terraform-aws-modules/eks 21.26.0 needs aws >= 6.59; vpc 6.7.3 >= 6.28.
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.66"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}
