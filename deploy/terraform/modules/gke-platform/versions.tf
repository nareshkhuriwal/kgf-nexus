# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  required_version = ">= 1.11, < 2.0"

  required_providers {
    # Native google resources, not terraform-google-modules/kubernetes-engine:
    # its newest release (45.0.0) requires google < 8.0, and 8.x is the current
    # major. The cluster here is ~150 lines of plain resources, which is less to
    # review than the module's surface. See deploy/terraform/README.md.
    google = {
      source  = "hashicorp/google"
      version = "~> 8.4"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}
