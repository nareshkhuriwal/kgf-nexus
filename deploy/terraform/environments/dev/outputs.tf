# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "console_url" {
  value = "http://${var.hostnames.console}"
}

output "api_url" {
  description = "Set as NEXUS_ENDPOINT for nexusctl and the SDK."
  value       = "http://${var.hostnames.api}"
}

output "mailpit_url" {
  value = "http://${var.hostnames.mail}"
}

output "direct_port_urls" {
  description = "The same services on localhost ports, bypassing the gateway."
  value = {
    console = "http://localhost:${var.ports.ui}"
    api     = "http://localhost:${var.ports.api}"
    mailpit = "http://localhost:${var.ports.mailpit}"
  }
}

output "namespace" {
  value = module.nexus.namespace
}

output "admin_api_key" {
  description = "terraform output -raw admin_api_key"
  value       = module.nexus.admin_api_key
  sensitive   = true
}
