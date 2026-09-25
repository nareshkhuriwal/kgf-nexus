# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.6"

  required_providers {
    kubernetes = {
      source = "hashicorp/kubernetes"
      # 2.38+ or 3.x: only the _v1 resources are used, which both majors keep.
      # Each root module pins the exact line it runs with.
      version = ">= 2.38, < 4.0"
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
}
