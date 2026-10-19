# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "namespace" {
  description = "Kubernetes namespace of the release."
  value       = local.namespace
}

output "release_status" {
  description = "Helm release status."
  value       = helm_release.nexus.status
}

output "admin_api_key" {
  description = "Bootstrap admin API key for the console and nexusctl. Read with `terraform output -raw admin_api_key`."
  value       = local.admin_api_key
  sensitive   = true
}
