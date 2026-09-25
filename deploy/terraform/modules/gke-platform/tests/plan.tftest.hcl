# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline plan of the module with a MOCKED google provider: no credentials, no
# network, nothing created. Asserts the security defaults.
#   terraform -chdir=deploy/terraform/modules/gke-platform test

mock_provider "google" {}
mock_provider "random" {}

variables {
  name                    = "kgf-nexus-test"
  project_id              = "my-nexus-project"
  region                  = "us-central1"
  master_authorized_cidrs = ["203.0.113.0/24"]
}

run "secure_defaults" {
  command = plan

  assert {
    condition     = google_container_cluster.this.private_cluster_config[0].enable_private_nodes
    error_message = "Nodes must be private."
  }
  assert {
    condition     = google_container_cluster.this.workload_identity_config[0].workload_pool == "my-nexus-project.svc.id.goog"
    error_message = "Workload Identity must be on."
  }
  assert {
    condition     = google_container_cluster.this.datapath_provider == "ADVANCED_DATAPATH"
    error_message = "Dataplane V2 must enforce NetworkPolicy."
  }
  assert {
    condition     = google_container_node_pool.cpu.node_config[0].workload_metadata_config[0].mode == "GKE_METADATA"
    error_message = "Pods must not see the node service account."
  }
  assert {
    condition     = !google_sql_database_instance.this.settings[0].ip_configuration[0].ipv4_enabled
    error_message = "Cloud SQL must have no public IP."
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].ip_configuration[0].ssl_mode == "ENCRYPTED_ONLY"
    error_message = "Cloud SQL must require TLS."
  }
  assert {
    condition     = google_sql_database_instance.this.settings[0].backup_configuration[0].point_in_time_recovery_enabled
    error_message = "Point-in-time recovery must be on."
  }
  assert {
    condition     = google_storage_bucket.artifacts.public_access_prevention == "enforced" && google_storage_bucket.artifacts.uniform_bucket_level_access
    error_message = "The artifact bucket must be private with uniform access."
  }
  assert {
    condition     = length(google_container_node_pool.gpu) == 0
    error_message = "The GPU pool must be opt-in."
  }
}

run "gpu_pool_is_tainted" {
  command = plan

  variables {
    gpu_pool = {
      enabled           = true
      machine_type      = "g2-standard-8"
      accelerator_type  = "nvidia-l4"
      accelerator_count = 1
      min_per_zone      = 0
      max_per_zone      = 1
      disk_size_gb      = 200
      zones             = ["us-central1-a"]
    }
  }

  assert {
    condition     = google_container_node_pool.gpu[0].node_config[0].taint[0].key == "nvidia.com/gpu"
    error_message = "GPU nodes must carry the nvidia.com/gpu taint."
  }
}

run "refuses_open_control_plane" {
  command = plan

  variables {
    master_authorized_cidrs = []
  }

  expect_failures = [var.master_authorized_cidrs]
}
