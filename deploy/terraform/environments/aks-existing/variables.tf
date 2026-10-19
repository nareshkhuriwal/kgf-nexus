# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# ── Azure: what already exists ────────────────────────────────────────────
#
# Everything in this block is READ (data sources). Nothing here is created,
# changed or deleted by this root, apart from the optional Blob container.

variable "subscription_id" {
  description = "Azure subscription holding the cluster, the database, the storage account and (if used) the Key Vault. Explicit, so a stale `az account set` cannot redirect an apply."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", var.subscription_id))
    error_message = "subscription_id must be a subscription GUID (lower case)."
  }
}

variable "aks_cluster" {
  description = "The existing AKS cluster Nexus is installed on. It must have the OIDC issuer enabled and the workload identity webhook installed (az aks update --enable-oidc-issuer --enable-workload-identity); the plan refuses a cluster without the issuer."
  type = object({
    name                = string
    resource_group_name = string
  })

  validation {
    condition     = can(regex("^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$", var.aks_cluster.name)) && length(var.aks_cluster.resource_group_name) > 0
    error_message = "aks_cluster.name must be an AKS cluster name (1-63 letters, digits, '-' or '_') and resource_group_name must be set."
  }
}

variable "kube_auth" {
  description = <<-EOT
    How Terraform authenticates to the cluster's API server.
      mode = "azure-ad" (default): an Entra ID token from kubelogin on every
        call, for clusters with Entra ID integration (the AKS default, and
        required when local accounts are disabled). kubelogin_login picks the
        credential: azurecli (whoever ran `az login`), workloadidentity (a CI
        job federated to an Entra application: AZURE_CLIENT_ID,
        AZURE_TENANT_ID, AZURE_FEDERATED_TOKEN_FILE), spn (a service
        principal secret in AAD_SERVICE_PRINCIPAL_CLIENT_ID /
        AAD_SERVICE_PRINCIPAL_CLIENT_SECRET) or msi (the managed identity of
        the VM Terraform runs on). The identity needs Azure Kubernetes Service
        RBAC Cluster Admin, or a Kubernetes binding to cluster-admin.
      mode = "admin": the cluster's local admin credential (client
        certificate) read from the AKS API. Only for clusters WITHOUT Entra ID
        integration or with local accounts deliberately left enabled; the
        plan refuses it when the cluster returns no such credential. It never
        expires and cannot be attributed to a person: prefer azure-ad.
  EOT
  type = object({
    mode            = optional(string, "azure-ad")
    kubelogin_login = optional(string, "azurecli")
  })
  default = {}

  validation {
    condition     = contains(["azure-ad", "admin"], var.kube_auth.mode)
    error_message = "kube_auth.mode must be azure-ad or admin."
  }
  validation {
    condition     = contains(["azurecli", "workloadidentity", "spn", "msi"], var.kube_auth.kubelogin_login)
    error_message = "kube_auth.kubelogin_login must be azurecli, workloadidentity, spn or msi."
  }
}

variable "postgres" {
  description = <<-EOT
    The existing Azure Database for PostgreSQL Flexible Server (docs/INSTALL-AKS.md, step 3).
      database     must already exist, owned by owner_login (Terraform does not
                   create it: destroying this root must never be able to drop
                   the data).
      owner_login  owns the schema and runs the migrate, bootstrap and
                   db-roles Jobs; it needs CREATEROLE. "" = the server's
                   administrator login. Services connect as their own runtime
                   logins (runtime_db_roles), never as this one.
      password_key_vault_secret
                   the name of a secret in key_vault holding owner_login's
                   password; "" = pass it in postgres_password instead.
    The server must allow-list pgcrypto (azure.extensions) and accept TLS
    connections from the cluster's pods (VNet integration or a firewall rule).
  EOT
  type = object({
    server_name               = string
    resource_group_name       = string
    database                  = optional(string, "nexus")
    owner_login               = optional(string, "")
    password_key_vault_secret = optional(string, "")
  })

  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.postgres.server_name)) && length(var.postgres.resource_group_name) > 0
    error_message = "postgres.server_name must be a Flexible Server name (3-63 lower-case letters, digits and '-') and resource_group_name must be set."
  }
  validation {
    condition     = can(regex("^[A-Za-z_][A-Za-z0-9_]{0,62}$", var.postgres.database)) && (var.postgres.owner_login == "" || can(regex("^[A-Za-z_][A-Za-z0-9_]{0,62}$", var.postgres.owner_login)))
    error_message = "postgres.database and postgres.owner_login must be plain PostgreSQL identifiers (letters, digits, '_'; at most 63)."
  }
}

variable "postgres_password" {
  description = "Password of postgres.owner_login, when it is not read from Key Vault (postgres.password_key_vault_secret). Set it through TF_VAR_postgres_password, never in a committed file. Embedded unescaped in a postgres:// URL, so alphanumeric, at least 16 characters (create the owner login with such a password: docs/INSTALL-AKS.md)."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.postgres_password == null || can(regex("^[A-Za-z0-9]{16,}$", coalesce(var.postgres_password, "x")))
    error_message = "postgres_password must be at least 16 alphanumeric characters: it is embedded unescaped in a postgres:// URL."
  }
  validation {
    condition     = (var.postgres_password == null) != (var.postgres.password_key_vault_secret == "")
    error_message = "Give the database owner password exactly one way: postgres_password, or postgres.password_key_vault_secret (with key_vault)."
  }
}

variable "storage" {
  description = <<-EOT
    The existing storage account Nexus keeps results and task logs in, through
    the native Azure Blob backend and workload identity (no account key is
    ever used). container_name is used as it is unless create_container is
    true, in which case Terraform creates it (private) and protects it with
    prevent_destroy: removing this root then leaves the container and its
    data in place (terraform state rm first).
  EOT
  type = object({
    account_name        = string
    resource_group_name = string
    container_name      = optional(string, "nexus-results")
    create_container    = optional(bool, false)
  })

  validation {
    condition     = can(regex("^[a-z0-9]{3,24}$", var.storage.account_name)) && length(var.storage.resource_group_name) > 0
    error_message = "storage.account_name must be a storage account name (3-24 lower-case letters and digits) and resource_group_name must be set."
  }
  validation {
    condition     = can(regex("^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){2,62}$", var.storage.container_name))
    error_message = "storage.container_name must be a Blob container name: 3-63 lower-case letters, digits and single '-', starting and ending with a letter or digit."
  }
}

variable "key_vault" {
  description = "Optional existing Key Vault (Azure RBAC authorization) for two things: the database owner password (postgres.password_key_vault_secret) and akv:// task secrets. With it, a SEPARATE identity gets Key Vault Secrets User on this vault and is federated to the agents' ServiceAccounts only, so the gateway, workflow-manager and artifact-gc never hold secret access. null: no vault."
  type = object({
    name                = string
    resource_group_name = string
  })
  default = null

  validation {
    condition     = var.key_vault == null || can(regex("^[A-Za-z][A-Za-z0-9-]{1,22}[A-Za-z0-9]$", try(var.key_vault.name, "")))
    error_message = "key_vault.name must be a Key Vault name (3-24 letters, digits and '-', starting with a letter)."
  }
}

variable "identity" {
  description = "Where the Nexus managed identities are created. name_prefix \"\" = \"<cluster name>-nexus\" (identities <prefix>-artifacts and, with key_vault, <prefix>-secrets); resource_group_name \"\" = the cluster's resource group; location \"\" = the cluster's region."
  type = object({
    name_prefix         = optional(string, "")
    resource_group_name = optional(string, "")
    location            = optional(string, "")
  })
  default = {}

  validation {
    condition     = var.identity.name_prefix == "" || can(regex("^[A-Za-z0-9][A-Za-z0-9_-]{0,100}$", var.identity.name_prefix))
    error_message = "identity.name_prefix must be letters, digits, '-' and '_' (at most 101 characters)."
  }
}

variable "tags" {
  description = "Tags on the Azure resources this root creates (the identities). Existing resources are never re-tagged."
  type        = map(string)
  default     = {}
}

# ── Kubernetes ────────────────────────────────────────────────────────────

variable "namespace" {
  description = "Kubernetes namespace of the Nexus release (the console's control_plane_namespace when the cluster is registered)."
  type        = string
  default     = "nexus"

  validation {
    condition     = can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", var.namespace))
    error_message = "namespace must be a Kubernetes namespace name (RFC 1123 label)."
  }
}

variable "create_namespace" {
  description = "true: create the namespace, enforcing the Pod Security restricted profile (every pod of chart 0.3.0 passes it). false: it already exists (a platform team owns namespaces); label it pod-security.kubernetes.io/enforce=restricted, which deploy/scripts/aks-preflight.sh checks."
  type        = bool
  default     = true
}

variable "release_name" {
  description = "Helm release name."
  type        = string
  default     = "kgf-nexus"
}

variable "nexus_namespace" {
  description = "Nexus tenant namespace the agents serve and bootstrap creates (not a Kubernetes namespace)."
  type        = string
  default     = "default"
}

variable "control_plane_node_pool" {
  description = "AKS node pool (agent pool name) for the control-plane services, the console, the cluster controller and the agent pods of kubernetes-executor worker pools: chart placement.nodeSelector on kubernetes.azure.com/agentpool, tolerating that pool's taints. \"\" = no pinning (the Kubernetes scheduler places them)."
  type        = string
  default     = ""
}

variable "scheduler" {
  description = "The scheduler Deployments: one per queue the worker pools serve (always including default), each with this many replicas, on node_pool (\"\" = control_plane_node_pool). Replicas are safe to scale; past a ~10k pending backlog Postgres, not replica count, is the limit (ADR-0018). Saved autoscaling in the console is applied by the cluster controller, not here."
  type = object({
    node_pool = optional(string, "")
    replicas  = optional(number, 2)
  })
  default = {}

  validation {
    condition     = var.scheduler.replicas >= 1 && floor(var.scheduler.replicas) == var.scheduler.replicas
    error_message = "scheduler.replicas must be a positive integer."
  }
}

variable "worker_pools" {
  description = <<-EOT
    Agent Deployments (agent-<name>), each pinned to one AKS node pool by its
    kubernetes.azure.com/agentpool label and tolerating that pool's taints.
    What an agent ADVERTISES (cpu_millis, memory_gib, gpu_*) is what the
    scheduler packs tasks against: size it inside one node of the pool. With
    gpu_count > 0 the pod also requests that many nvidia.com/gpu, so the pool
    needs the NVIDIA device plugin (install_nvidia_device_plugin, or your own)
    and an N-series VM size. A Spot pool's agent is marked capacityType spot.
    executor = "subprocess" (default): tasks run inside the agent pod on
    node_pool. executor = "kubernetes": the agent pod stays on
    control_plane_node_pool and runs each attempt as a Job on node_pool
    (docs/OPERATIONS.md §13); the sizes then describe the task capacity.
    max_replicas is the ceiling the cluster controller may scale the pool to
    in apply mode (the registration output's binding), default = replicas.
  EOT
  type = list(object({
    name           = string
    node_pool      = string
    queues         = optional(string, "default")
    replicas       = optional(number, 1)
    max_replicas   = optional(number)
    labels         = optional(string, "")
    executor       = optional(string, "subprocess")
    cpu_millis     = number
    memory_gib     = number
    disk_gib       = optional(number, 10)
    gpu_count      = optional(number, 0)
    gpu_kind       = optional(string, "")
    gpu_memory_gib = optional(number, 0)
    cpu_request    = optional(string, "250m")
    memory_request = optional(string, "256Mi")
  }))

  validation {
    condition     = length(var.worker_pools) >= 1
    error_message = "worker_pools needs at least one pool: without an agent nothing runs."
  }
  validation {
    condition     = length(distinct([for p in var.worker_pools : p.name])) == length(var.worker_pools)
    error_message = "worker_pools names must be unique (each is an agent Deployment agent-<name>)."
  }
  validation {
    condition     = alltrue([for p in var.worker_pools : can(regex("^[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?$", p.name)) && can(regex("^[a-z][a-z0-9]{0,11}$", p.node_pool))])
    error_message = "worker_pools[].name must be a lower-case DNS label of at most 42 characters, and node_pool an AKS node pool name (lower-case letters and digits, starting with a letter, at most 12)."
  }
  validation {
    condition = alltrue([for p in var.worker_pools : alltrue([
      for q in split(",", p.queues) : can(regex("^[a-z0-9]([-a-z0-9]{0,40}[a-z0-9])?$", trimspace(q)))
    ])])
    error_message = "worker_pools[].queues must be comma-separated queue names that are lower-case DNS labels (each queue gets a scheduler Deployment named after it)."
  }
  validation {
    condition = alltrue([for p in var.worker_pools :
      p.replicas >= 0 && floor(p.replicas) == p.replicas && p.cpu_millis > 0 && p.memory_gib > 0 && p.disk_gib > 0 &&
      p.gpu_count >= 0 && floor(p.gpu_count) == p.gpu_count && coalesce(p.max_replicas, p.replicas) >= p.replicas
    ])
    error_message = "worker_pools: replicas and gpu_count must be non-negative integers, cpu_millis, memory_gib and disk_gib positive, and max_replicas (if set) at least replicas."
  }
  validation {
    condition     = alltrue([for p in var.worker_pools : contains(["subprocess", "kubernetes"], p.executor)])
    error_message = "worker_pools[].executor must be subprocess or kubernetes."
  }
  validation {
    condition     = alltrue([for p in var.worker_pools : p.gpu_count == 0 || (p.gpu_kind != "" && p.gpu_memory_gib > 0)])
    error_message = "A GPU pool (gpu_count > 0) must say what it advertises: gpu_kind (e.g. nvidia-t4) and gpu_memory_gib."
  }
}

variable "storage_class" {
  description = "StorageClass for the chart's one stateful volume (the NATS JetStream store). AKS ships managed-csi-premium (Premium SSD, CSI driver); \"\" = the cluster default."
  type        = string
  default     = "managed-csi-premium"
}

variable "install_nvidia_device_plugin" {
  description = "Install the NVIDIA k8s-device-plugin DaemonSet on the GPU worker pools' node pools. AKS installs the driver on GPU nodes but not the plugin that advertises nvidia.com/gpu. Leave false when the cluster already runs it (or the GPU Operator): two plugins on one node fight over the devices."
  type        = bool
  default     = false
}

variable "nvidia_device_plugin_version" {
  description = "NVIDIA k8s-device-plugin Helm chart version (install_nvidia_device_plugin)."
  type        = string
  default     = "0.20.1"
}

variable "cluster_controller" {
  description = <<-EOT
    The in-cluster cluster controller (docs/OPERATIONS.md §12): reports this
    cluster to the console and, in apply mode only, applies the saved
    scheduler and worker-pool settings. cluster_name is the name the cluster
    is registered under in the console ("" = the AKS cluster name, lower
    case). mode = "plan" (default) observes and reports what it WOULD do.
    mode = "apply" also grants the scale permissions, and requires
    expected_cluster_uid: the kube-system namespace UID of THIS cluster
    (kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'), which
    the plan compares with the live cluster. The console registration must be
    switched to apply with the same UID as well; the controller acts only
    when all three agree.
  EOT
  type = object({
    enabled              = optional(bool, true)
    cluster_name         = optional(string, "")
    mode                 = optional(string, "plan")
    expected_cluster_uid = optional(string, "")
  })
  default = {}

  validation {
    condition     = contains(["plan", "apply"], var.cluster_controller.mode)
    error_message = "cluster_controller.mode must be plan or apply."
  }
  validation {
    condition     = var.cluster_controller.mode == "plan" || can(regex("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", var.cluster_controller.expected_cluster_uid))
    error_message = "cluster_controller.mode = apply requires expected_cluster_uid: `kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'` against the intended cluster."
  }
  validation {
    condition     = var.cluster_controller.cluster_name == "" || can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", var.cluster_controller.cluster_name))
    error_message = "cluster_controller.cluster_name must be a lower-case DNS label."
  }
}

variable "extra_federated_service_accounts" {
  description = "More AGENT ServiceAccounts (namespace + name) the identities trust: the agents' account of a worker-pool release in another namespace (docs/INSTALL-AKS.md, \"Adding worker pools\"). They get the result store and, with key_vault, the secrets identity. The release's own accounts (nexus-artifacts, nexus-agent, nexus-agent-kubernetes) are always included. An identity takes at most 20 federated credentials."
  type = list(object({
    namespace = string
    name      = string
  }))
  default = []

  validation {
    condition     = length(var.extra_federated_service_accounts) <= 17
    error_message = "A user-assigned identity holds at most 20 federated credentials; the release's own accounts use up to three."
  }
  validation {
    condition = alltrue([for s in var.extra_federated_service_accounts :
      can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", s.namespace)) && can(regex("^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$", s.name))
    ])
    error_message = "extra_federated_service_accounts entries must name a Kubernetes namespace and ServiceAccount."
  }
}

# ── The release ───────────────────────────────────────────────────────────

variable "image" {
  description = "Nexus images: <registry><repository_prefix><component>:<tag>[@digest]. The release workflow publishes ghcr.io/<owner>/kgf-nexus/<component> (docs/OPERATIONS.md §22.4); its image-digests artifact's release-images.yaml holds the digests map. pull_secrets: docker-registry Secrets in the namespace, for a private registry (GHCR packages that are not public)."
  type = object({
    tag               = string
    registry          = optional(string, "ghcr.io/")
    repository_prefix = optional(string, "nareshkhuriwal/kgf-nexus/")
    digests           = optional(map(string), {})
    pull_secrets      = optional(list(string), [])
  })

  validation {
    condition     = length(var.image.tag) > 0 && var.image.tag != "latest" && var.image.tag != "main"
    error_message = "image.tag must be an immutable tag: a commit (12 hex digits) or a vX.Y.Z release, never latest or main (main moves with every merge)."
  }
  validation {
    condition     = alltrue([for k, d in var.image.digests : can(regex("^sha256:[0-9a-f]{64}$", d))])
    error_message = "image.digests values must be sha256:<64 hex> digests (release-images.yaml from the release workflow)."
  }
  validation {
    condition     = var.image.registry == "" || endswith(var.image.registry, "/")
    error_message = "image.registry must end with '/' (e.g. ghcr.io/) or be empty."
  }
}

variable "chart" {
  description = "The chart to install. repository \"\" = this checkout's deploy/helm/kgf-nexus (versions always match the code you are reading). Otherwise the published chart: repository oci://ghcr.io/<owner>/charts at one exact version (Chart.yaml version; the release workflow's summary names it)."
  type = object({
    repository = optional(string, "")
    version    = optional(string, "")
  })
  default = {}

  validation {
    condition     = var.chart.repository == "" || (startswith(var.chart.repository, "oci://") && can(regex("^[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$", var.chart.version)))
    error_message = "chart.repository must be an oci:// URL, and needs chart.version: one exact chart version (MAJOR.MINOR.PATCH)."
  }
}

variable "nats_credentials_dir" {
  description = "Directory written on an admin machine by `deploy/nats/nexus-nats-auth.sh init --tenant <nexus_namespace>` (docs/OPERATIONS.md §11.7). Required: the chart refuses an unauthenticated broker. The seeds land in the credentials Secret and in Terraform state."
  type        = string
}

variable "runtime_db_roles" {
  description = "Services connect as their own least-privilege logins, created by the chart's db-roles Job; the owner login then reaches only the migrate, bootstrap and db-roles Jobs (docs/OPERATIONS.md §20.12)."
  type        = bool
  default     = true
}

variable "runtime_db_password_version" {
  description = "Bump to rotate every runtime login's password in one apply (the db-roles Job re-runs and the pods roll)."
  type        = number
  default     = 1
}

variable "helm_timeout_seconds" {
  description = "How long Helm waits for every workload and Job (image pulls on new nodes included)."
  type        = number
  default     = 900
}

variable "chart_values_overrides" {
  description = "Chart values deep-merged last (gateway, SMTP, tracing, mTLS, autoscaling, kgf.*...). They win over everything this root derives, so prefer the variables above for what they cover."
  type        = any
  default     = {}
}
