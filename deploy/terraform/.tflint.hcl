# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# tflint for every module and environment (make tf-lint). Plugins are pinned;
# `tflint --init` downloads them from GitHub releases.

config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
  version = "0.15.0"
  source  = "github.com/terraform-linters/tflint-ruleset-terraform"
}

plugin "azurerm" {
  enabled = true
  version = "0.32.0"
  source  = "github.com/terraform-linters/tflint-ruleset-azurerm"
}

# prevent_destroy cannot vary per environment (it must be a literal), and a
# test environment has to be destroyable. The stateful Azure resources get
# CanNotDelete management locks instead (aks-platform delete_locks_enabled).
rule "azurerm_resources_missing_prevent_destroy" {
  enabled = false
}

plugin "aws" {
  enabled = true
  version = "0.49.0"
  source  = "github.com/terraform-linters/tflint-ruleset-aws"
}

plugin "google" {
  enabled = true
  version = "0.40.0"
  source  = "github.com/terraform-linters/tflint-ruleset-google"
}
