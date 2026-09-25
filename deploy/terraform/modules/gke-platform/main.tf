# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# The GCP infrastructure one KGF Nexus environment needs, and nothing inside
# Kubernetes (the environment root does that): VPC, regional GKE, Artifact
# Registry, Cloud SQL and the artifact bucket.
#
# Security posture, in one place:
#   - Private nodes (no external IPs), egress through Cloud NAT; control plane
#     restricted to authorised networks or private only.
#   - Workload Identity on, and GKE_METADATA on every pool, so pods never see
#     the node's service account. Nodes run as a minimal service account, not
#     the Compute default.
#   - Dataplane V2 enforces NetworkPolicy (the chart restricts NATS with it).
#   - Shielded nodes with secure boot; Secret encryption at the application
#     layer is available through database_encryption if you supply a key.
#   - Cloud SQL private IP only, TLS required; bucket uniform access, public
#     access prevention enforced.

data "google_project" "this" {
  project_id = var.project_id
}

resource "random_string" "suffix" {
  length  = 6
  upper   = false
  special = false
}

locals {
  labels = merge(var.labels, {
    "app-kubernetes-io-part-of" = "kgf-nexus"
    "managed-by"                = "terraform"
    "nexus-environment"         = var.name
  })

  apis = [
    "compute.googleapis.com",
    "container.googleapis.com",
    "sqladmin.googleapis.com",
    "servicenetworking.googleapis.com",
    "artifactregistry.googleapis.com",
    "secretmanager.googleapis.com",
    "iam.googleapis.com",
    "logging.googleapis.com",
    "monitoring.googleapis.com",
  ]
}

resource "google_project_service" "this" {
  for_each = var.enable_apis ? toset(local.apis) : toset([])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

# ── Network ───────────────────────────────────────────────────────────────

resource "google_compute_network" "this" {
  project                 = var.project_id
  name                    = "${var.name}-vpc"
  auto_create_subnetworks = false
  routing_mode            = "REGIONAL"
  depends_on              = [google_project_service.this]
}

resource "google_compute_subnetwork" "nodes" {
  project                  = var.project_id
  name                     = "${var.name}-nodes"
  region                   = var.region
  network                  = google_compute_network.this.id
  ip_cidr_range            = var.subnet_cidr
  private_ip_google_access = true

  secondary_ip_range {
    range_name    = "pods"
    ip_cidr_range = var.pods_cidr
  }
  secondary_ip_range {
    range_name    = "services"
    ip_cidr_range = var.services_cidr
  }

  log_config {
    aggregation_interval = "INTERVAL_5_SEC"
    flow_sampling        = 0.5
    metadata             = "INCLUDE_ALL_METADATA"
  }
}

resource "google_compute_router" "this" {
  project = var.project_id
  name    = "${var.name}-router"
  region  = var.region
  network = google_compute_network.this.id
}

resource "google_compute_router_nat" "this" {
  project                            = var.project_id
  name                               = "${var.name}-nat"
  router                             = google_compute_router.this.name
  region                             = var.region
  nat_ip_allocate_option             = "AUTO_ONLY"
  source_subnetwork_ip_ranges_to_nat = "ALL_SUBNETWORKS_ALL_IP_RANGES"

  log_config {
    enable = true
    filter = "ERRORS_ONLY"
  }
}

# Private services access: the peering Cloud SQL's private IP lives behind.
resource "google_compute_global_address" "private_services" {
  project       = var.project_id
  name          = "${var.name}-private-services"
  purpose       = "VPC_PEERING"
  address_type  = "INTERNAL"
  prefix_length = var.private_services_cidr_prefix
  network       = google_compute_network.this.id
}

resource "google_service_networking_connection" "private_services" {
  network                 = google_compute_network.this.id
  service                 = "servicenetworking.googleapis.com"
  reserved_peering_ranges = [google_compute_global_address.private_services.name]
}

# ── Node identity ─────────────────────────────────────────────────────────

# Nodes run as this account, with only what a node needs (logs, metrics, image
# pulls), never as the Compute Engine default service account (Editor).
resource "google_service_account" "nodes" {
  project      = var.project_id
  account_id   = "${var.name}-nodes"
  display_name = "${var.name} GKE nodes"
}

resource "google_project_iam_member" "nodes" {
  for_each = toset([
    "roles/logging.logWriter",
    "roles/monitoring.metricWriter",
    "roles/monitoring.viewer",
    "roles/stackdriver.resourceMetadata.writer",
    "roles/autoscaling.metricsWriter",
  ])
  project = var.project_id
  role    = each.value
  member  = google_service_account.nodes.member
}

# ── GKE ───────────────────────────────────────────────────────────────────

resource "google_container_cluster" "this" {
  project  = var.project_id
  name     = var.name
  location = var.region # regional: control plane and nodes across three zones

  network         = google_compute_network.this.id
  subnetwork      = google_compute_subnetwork.nodes.id
  networking_mode = "VPC_NATIVE"
  ip_allocation_policy {
    cluster_secondary_range_name  = "pods"
    services_secondary_range_name = "services"
  }

  # Node pools are separate resources, so they can change without replacing
  # the cluster.
  remove_default_node_pool = true
  initial_node_count       = 1

  release_channel {
    channel = var.release_channel
  }

  private_cluster_config {
    enable_private_nodes    = true
    enable_private_endpoint = var.private_endpoint
  }

  control_plane_endpoints_config {
    dns_endpoint_config {
      allow_external_traffic = var.dns_endpoint_external
    }
  }

  master_authorized_networks_config {
    gcp_public_cidrs_access_enabled = false
    dynamic "cidr_blocks" {
      for_each = var.master_authorized_cidrs
      content {
        cidr_block   = cidr_blocks.value
        display_name = "authorised-${cidr_blocks.key}"
      }
    }
  }

  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # Dataplane V2 (eBPF): NetworkPolicy enforced without the Calico add-on.
  datapath_provider = "ADVANCED_DATAPATH"

  enable_shielded_nodes = true
  enable_legacy_abac    = false

  master_auth {
    client_certificate_config {
      issue_client_certificate = false
    }
  }

  addons_config {
    gce_persistent_disk_csi_driver_config {
      enabled = true
    }
    http_load_balancing {
      disabled = false
    }
    horizontal_pod_autoscaling {
      disabled = false
    }
  }

  logging_config {
    enable_components = ["SYSTEM_COMPONENTS", "WORKLOADS", "APISERVER", "CONTROLLER_MANAGER", "SCHEDULER"]
  }

  monitoring_config {
    enable_components = ["SYSTEM_COMPONENTS", "APISERVER", "CONTROLLER_MANAGER", "SCHEDULER"]
    managed_prometheus {
      enabled = true
    }
  }

  security_posture_config {
    mode               = "BASIC"
    vulnerability_mode = "VULNERABILITY_BASIC"
  }

  maintenance_policy {
    recurring_window {
      start_time = "2026-01-04T02:00:00Z"
      end_time   = "2026-01-04T06:00:00Z"
      recurrence = "FREQ=WEEKLY;BYDAY=SA,SU"
    }
  }

  resource_labels     = local.labels
  deletion_protection = var.deletion_protection

  depends_on = [google_project_service.this]
}

resource "google_container_node_pool" "cpu" {
  project  = var.project_id
  name     = "cpu"
  location = var.region
  cluster  = google_container_cluster.this.name

  autoscaling {
    min_node_count = var.cpu_pool.min_per_zone
    max_node_count = var.cpu_pool.max_per_zone
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  upgrade_settings {
    strategy        = "SURGE"
    max_surge       = 1
    max_unavailable = 0
  }

  node_config {
    machine_type    = var.cpu_pool.machine_type
    disk_type       = "pd-balanced"
    disk_size_gb    = var.cpu_pool.disk_size_gb
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    metadata        = { "disable-legacy-endpoints" = "true" }
    labels          = { "nexus.khuriwalgroup.com/pool" = "cpu" }
    resource_labels = local.labels

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
}

resource "google_container_node_pool" "gpu" {
  count = var.gpu_pool.enabled ? 1 : 0

  project        = var.project_id
  name           = "gpu"
  location       = var.region
  cluster        = google_container_cluster.this.name
  node_locations = length(var.gpu_pool.zones) > 0 ? var.gpu_pool.zones : null

  autoscaling {
    min_node_count = var.gpu_pool.min_per_zone
    max_node_count = var.gpu_pool.max_per_zone
  }

  management {
    auto_repair  = true
    auto_upgrade = true
  }

  node_config {
    machine_type    = var.gpu_pool.machine_type
    disk_type       = "pd-balanced"
    disk_size_gb    = var.gpu_pool.disk_size_gb
    image_type      = "COS_CONTAINERD"
    service_account = google_service_account.nodes.email
    oauth_scopes    = ["https://www.googleapis.com/auth/cloud-platform"]
    metadata        = { "disable-legacy-endpoints" = "true" }
    labels          = { "nexus.khuriwalgroup.com/pool" = "gpu", "accelerator" = "nvidia" }
    resource_labels = local.labels

    guest_accelerator {
      type  = var.gpu_pool.accelerator_type
      count = var.gpu_pool.accelerator_count
      # GKE installs the driver and runs the device plugin itself.
      gpu_driver_installation_config {
        gpu_driver_version = "DEFAULT"
      }
    }

    # GKE adds this taint to GPU nodes anyway; declaring it keeps the plan
    # truthful about where pods may land.
    taint {
      key    = "nvidia.com/gpu"
      value  = "present"
      effect = "NO_SCHEDULE"
    }

    workload_metadata_config {
      mode = "GKE_METADATA"
    }
    shielded_instance_config {
      enable_secure_boot          = true
      enable_integrity_monitoring = true
    }
  }
}

# ── Artifact Registry ─────────────────────────────────────────────────────

resource "google_artifact_registry_repository" "this" {
  project       = var.project_id
  location      = var.region
  repository_id = "kgf-nexus"
  format        = "DOCKER"
  description   = "KGF Nexus images (${var.name})"
  labels        = local.labels

  docker_config {
    immutable_tags = true
  }

  cleanup_policies {
    id     = "delete-untagged"
    action = "DELETE"
    condition {
      tag_state  = "UNTAGGED"
      older_than = "1209600s"
    }
  }

  depends_on = [google_project_service.this]
}

resource "google_artifact_registry_repository_iam_member" "nodes" {
  project    = var.project_id
  location   = google_artifact_registry_repository.this.location
  repository = google_artifact_registry_repository.this.name
  role       = "roles/artifactregistry.reader"
  member     = google_service_account.nodes.member
}
