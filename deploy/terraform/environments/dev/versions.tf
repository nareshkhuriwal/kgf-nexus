# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.6"

  required_providers {
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.38"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }

  # Local state, gitignored. It holds the generated credentials (sensitive),
  # which is acceptable for a laptop cluster and NOT for anything shared: a
  # shared environment must use an encrypted remote backend instead.
  backend "local" {
    path = "terraform.tfstate"
  }
}
