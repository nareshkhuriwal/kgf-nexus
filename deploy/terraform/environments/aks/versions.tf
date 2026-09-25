# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

terraform {
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
  # (no storage keys). State holds every generated credential: the container
  # must be private, versioned, and readable only by the deployers.
  #   terraform init -backend-config=backend.hcl   (see backend.hcl.example)
  backend "azurerm" {}
}
