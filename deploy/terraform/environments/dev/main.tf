# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# KGF Nexus dev environment on Docker Desktop's Kubernetes.
#
#   make k8s-dev-up        # build + load images, then terraform apply
#
# Everything environment-specific lives here; the module and the chart are
# shared with every other environment.

provider "kubernetes" {
  config_path    = pathexpand(var.kubeconfig_path)
  config_context = var.kube_context
}

provider "helm" {
  kubernetes = {
    config_path    = pathexpand(var.kubeconfig_path)
    config_context = var.kube_context
  }
}

locals {
  gib = 1073741824

  # The same three agents as the compose stack. The GPU capacity is SIMULATED
  # (there is no GPU on a laptop) and labelled so: simulated resources must be
  # explicit and never reach a production default.
  agents = [
    {
      name            = "agent-cpu"
      queues          = "default"
      labels          = "zone=local,accelerator=none"
      executors       = "subprocess"
      cpuMillis       = 8000
      memoryBytes     = 16 * local.gib
      gpuCount        = 0
      diskBytes       = 50 * local.gib
      customResources = "license_token=2"
      resources       = { requests = { cpu = "100m", memory = "128Mi" }, limits = { memory = "2Gi" } }
    },
    {
      name           = "agent-gpu"
      queues         = "default,gpu"
      labels         = "zone=local,accelerator=nvidia,gpu-interconnect=nvlink,simulated=true"
      executors      = "subprocess"
      cpuMillis      = 4000
      memoryBytes    = 32 * local.gib
      gpuCount       = 2
      gpuKind        = "nvidia-a100"
      gpuMemoryBytes = 40 * local.gib
      diskBytes      = 50 * local.gib
      resources      = { requests = { cpu = "100m", memory = "128Mi" }, limits = { memory = "2Gi" } }
    },
    {
      name           = "agent-gpu-a10"
      queues         = "default,gpu"
      labels         = "zone=local,accelerator=nvidia,simulated=true"
      executors      = "subprocess"
      cpuMillis      = 4000
      memoryBytes    = 16 * local.gib
      gpuCount       = 1
      gpuKind        = "nvidia-a10"
      gpuMemoryBytes = 24 * local.gib
      diskBytes      = 50 * local.gib
      resources      = { requests = { cpu = "100m", memory = "128Mi" }, limits = { memory = "2Gi" } }
    },
  ]
}

# The Gateway API implementation that serves the hostnames on port 80. A
# separate release, installed first: it brings the Gateway API CRDs the Nexus
# chart's Gateway and HTTPRoutes need. Envoy Gateway, not ingress-nginx, which
# was retired in 2026.
resource "helm_release" "envoy_gateway" {
  name             = "envoy-gateway"
  namespace        = "envoy-gateway-system"
  create_namespace = true
  repository       = "oci://docker.io/envoyproxy"
  chart            = "gateway-helm"
  version          = var.envoy_gateway_version
  wait             = true
  timeout          = 600
}

module "nexus" {
  source     = "../../modules/kgf-nexus"
  depends_on = [helm_release.envoy_gateway]

  namespace  = var.namespace
  chart_path = "${path.module}/../../../helm/kgf-nexus"
  image_tag  = var.image_tag

  # NATS identities from `make k8s-dev-up` (deploy/nats/nexus-nats-auth.sh
  # into the gitignored .dev-nats/). The broker requires them.
  nats_credentials_dir = var.nats_credentials_dir

  # The workstation's own Postgres, not an in-cluster one.
  postgres_password = var.postgres_password

  chart_values = {
    logLevel = "info"

    # The host machine's PostgreSQL (deploy/scripts/dev-host-postgres.sh).
    postgres = merge({ enabled = false }, var.postgres)

    # Laptop-sized volumes; the defaults are sized for a shared cluster.
    minio = { storage = { size = "5Gi" } }
    nats  = { storage = { size = "1Gi" } }

    # Mail goes to Mailpit, so verification and password-reset emails can be
    # read at http://localhost:<ports.mailpit>.
    mailpit = { enabled = true, serviceType = "LoadBalancer", uiPort = var.ports.mailpit }
    smtp = {
      host = "mailpit"
      port = 1025
      tls  = "none"
      from = "KGF Nexus <no-reply@nexus.localhost>"
      # Emailed links (verification, password reset) open the console here.
      publicUrl = "http://${var.hostnames.console}"
    }

    gateway = {
      enabled            = true
      createGatewayClass = true
      className          = "eg"
      port               = 80
      hosts              = var.hostnames
    }

    services = {
      apiGateway = {
        external = { enabled = true, type = "LoadBalancer", httpPort = var.ports.api, grpcPort = var.ports.grpc }
      }
    }

    agents = local.agents

    ui = { service = { type = "LoadBalancer", port = var.ports.ui } }
  }
}
