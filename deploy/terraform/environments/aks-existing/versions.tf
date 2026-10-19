# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
  # 1.11+, as every cloud environment here (deploy/terraform/README.md,
  # "Version choices"); optional() object attributes and cross-variable
  # validation need 1.9.
  required_version = ">= 1.11, < 2.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 5.7"
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

  # Remote state in an Azure Storage container, authenticated with Entra ID
  # (no storage keys). State holds every generated credential and the database
  # owner password: the container must be private, versioned, and readable
  # only by the deployers.
  #   terraform init -backend-config=backend.hcl   (see backend.hcl.example)
  backend "azurerm" {}
}
