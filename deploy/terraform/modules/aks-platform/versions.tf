# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  # 1.11+: write-only arguments (administrator_password_wo, value_wo) keep the
  # database password out of the plan and out of these resources' state.
  required_version = ">= 1.11, < 2.0"

  required_providers {
    # Native azurerm resources, not the Azure/aks community module: its newest
    # release (11.7.0) requires azurerm < 5.0, and the 4.x line stopped at
    # 4.81.0 (2026-07-14). Pinning the whole stack to a provider line that no
    # longer receives releases is the wrong trade for a new platform. See
    # deploy/terraform/README.md, "Version choices".
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 5.7"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.9"
    }
  }
}
