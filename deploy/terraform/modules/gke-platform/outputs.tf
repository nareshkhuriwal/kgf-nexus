# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

output "cluster_name" {
  description = "GKE cluster name."
  value       = google_container_cluster.this.name
}

output "cluster_endpoint" {
  description = "Control-plane IP endpoint (the private IP when private_endpoint is true)."
  value       = google_container_cluster.this.private_cluster_config[0].enable_private_endpoint ? google_container_cluster.this.private_cluster_config[0].private_endpoint : google_container_cluster.this.endpoint
  sensitive   = true
}

output "cluster_dns_endpoint" {
  description = "IAM-authorised DNS endpoint of the control plane."
  value       = try(google_container_cluster.this.control_plane_endpoints_config[0].dns_endpoint_config[0].endpoint, null)
}

output "cluster_ca_certificate" {
  description = "Base64 PEM of the cluster CA."
  value       = google_container_cluster.this.master_auth[0].cluster_ca_certificate
  sensitive   = true
}

output "kubeconfig_command" {
  description = "Write a kubeconfig entry for this cluster (needs gke-gcloud-auth-plugin)."
  value       = "gcloud container clusters get-credentials ${google_container_cluster.this.name} --region ${var.region} --project ${var.project_id}${var.private_endpoint ? " --dns-endpoint" : ""}"
}

output "registry" {
  description = "Image prefix for the chart's image.registry: images live at <this>kgf-nexus/<component>."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/"
}

output "postgres_host" {
  description = "Cloud SQL private IP."
  value       = google_sql_database_instance.this.private_ip_address
}

output "postgres_instance_connection_name" {
  description = "project:region:instance, for the Cloud SQL Auth Proxy."
  value       = google_sql_database_instance.this.connection_name
}

output "postgres_user" {
  description = "Database user Nexus connects as."
  value       = google_sql_user.nexus.name
}

output "postgres_database" {
  description = "Database name."
  value       = google_sql_database.nexus.name
}

output "postgres_password" {
  description = "Database password, for the kgf-nexus credentials Secret."
  value       = random_password.postgres.result
  sensitive   = true
}

output "postgres_password_secret" {
  description = "Secret Manager secret holding the database password."
  value       = google_secret_manager_secret.postgres_password.id
}

output "artifact_bucket" {
  description = "GCS bucket for Nexus results and logs."
  value       = google_storage_bucket.artifacts.name
}

output "artifact_hmac_credentials" {
  description = "HMAC key pair for GCS S3 interoperability, for the kgf-nexus credentials Secret."
  value = {
    access_key_id     = google_storage_hmac_key.artifacts.access_id
    secret_access_key = google_storage_hmac_key.artifacts.secret
  }
  sensitive = true
}

output "artifact_service_account" {
  description = "Service account the result store acts as."
  value       = google_service_account.artifacts.email
}

output "node_service_account" {
  description = "Service account the nodes run as."
  value       = google_service_account.nodes.email
}
