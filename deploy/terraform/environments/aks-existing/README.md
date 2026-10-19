# KGF Nexus on an existing AKS cluster

This root installs Nexus on an AKS cluster you already run, against an Azure
Database for PostgreSQL Flexible Server and a storage account you already
have. It **reads** those resources and **creates** only what Nexus itself
needs. The step-by-step guide, including the Helm path without Terraform, is
[docs/INSTALL-AKS.md](../../../../docs/INSTALL-AKS.md). To create a new cluster
and every service around it instead, use [environments/aks](../aks/README.md).

It has been **validated offline only** (`make tf-validate`: mocked-provider
plans, `make tf-lint`), not applied to a subscription. See
[../../README.md](../../README.md) for version choices and finding F115.

## What it reads and what it creates

| | Resources |
|---|---|
| **Read** (data sources, never changed) | The AKS cluster (`aks_cluster`): API endpoint, CA, OIDC issuer, Entra ID tenant. Each node pool Nexus is placed on: OS, VM size, taints, Spot priority. The kube-system namespace UID. The Flexible Server (`postgres`): FQDN, admin login. The storage account (`storage`), and the container unless `create_container`. The Key Vault (`key_vault`, optional) and the owner-password secret in it. |
| **Created in Azure** | `<prefix>-artifacts`: a user-assigned identity with **Storage Blob Data Contributor on the one container**, federated to the release's result-store and agent ServiceAccounts. With `key_vault`: `<prefix>-secrets`, a second identity with **Key Vault Secrets User on that vault**, federated to the **agent** accounts only. Optionally the Blob container (`create_container`, protected by `prevent_destroy`). |
| **Created in the cluster** | The namespace (unless `create_namespace = false`), the `kgf-nexus-credentials`, `kgf-nexus-postgres-ca` and `kgf-nexus-db-logins` Secrets (modules/kgf-nexus), the Nexus release, and optionally the NVIDIA device plugin on the GPU pools. |
| **Never created** | The database (destroying this root must never be able to drop it), the server's parameters, network rules, private endpoints, node pools, cluster add-ons. docs/INSTALL-AKS.md has the `az` commands for each. |

`<prefix>` defaults to `<cluster name>-nexus`; the identities go in the
cluster's resource group unless `identity.resource_group_name` says otherwise.

### Identities and ServiceAccounts

A federated credential trusts a ServiceAccount, not a pod: every pod running
as a trusted account can obtain the identity. So the accounts are split by
what their pods may reach:

| ServiceAccount | Pods | Identities it can obtain |
|---|---|---|
| `nexus-artifacts` | api-gateway, workflow-manager, artifact-gc | Blob |
| `nexus-agent` | subprocess agents | Blob, and secrets (with `key_vault`) |
| `nexus-agent-kubernetes` | kubernetes-executor agents (they mount an API token to create task Jobs) | Blob, and secrets |
| `extra_federated_service_accounts` | agents of worker-pool releases in other namespaces | Blob, and secrets |

The result-store pods carry `azure.workload.identity/use: "true"` (the chart
adds it); the Blob client is pinned to the Blob identity
(`resultStore.azure.clientId`) and the agents select the secrets identity for
`akv://` references (`agentDefaults.secrets.workloadIdentityClientId`, which
sets `AZURE_CLIENT_ID`). Put that client ID in the agents' `secrets.yaml`
(`auth: { method: workload_identity, client_id: ... }`, docs/OPERATIONS.md
§25.1). No account key, client secret or connection string exists anywhere.

## How Terraform reaches the cluster (`kube_auth`)

| `mode` | Credential | Use it for |
|---|---|---|
| `azure-ad` (default) | An Entra ID token from `kubelogin` on every call, for the cluster's Entra ID integration. `kubelogin_login` selects the source: `azurecli` (whoever ran `az login`), `workloadidentity` (a CI job federated to an Entra application: `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_FEDERATED_TOKEN_FILE`), `spn` (`AAD_SERVICE_PRINCIPAL_CLIENT_ID` / `_SECRET`), `msi` (the VM's managed identity). | Every cluster with Entra ID integration, which includes every cluster with local accounts disabled. The principal needs **Azure Kubernetes Service RBAC Cluster Admin** on the cluster (Azure RBAC), or a cluster-admin binding (Kubernetes RBAC). |
| `admin` | The cluster's local admin client certificate, read from the AKS API (`listClusterAdminCredential`). | Only clusters without Entra ID integration, or with local accounts deliberately enabled. The plan refuses it when the cluster returns no such credential. The certificate cannot be attributed to a person and is valid for years: prefer `azure-ad`. |

`kubelogin` must be on the `PATH` of whoever runs Terraform (`az aks
install-cli` installs it). A private cluster's API server is reachable only
from its VNet or a peered network: run Terraform there (a jump host or a
self-hosted runner).

## Prerequisites

Run `deploy/scripts/aks-preflight.sh` against the cluster first: it checks
everything below that the cluster can tell, read-only.

- **Cluster:** Kubernetes 1.29 or later; OIDC issuer and workload identity
  enabled (`az aks update -g <rg> -n <cluster> --enable-oidc-issuer
  --enable-workload-identity`). The plan refuses a cluster without the issuer.
  metrics-server (AKS always runs it). Linux user node pools for the worker
  pools; GPU pools on N-series sizes.
- **Database:** the database exists, owned by `postgres.owner_login`, which
  has `CREATEROLE`; `pgcrypto` is allow-listed (`azure.extensions`) and created;
  the server accepts TLS connections from the pods (VNet integration, or a
  firewall rule for the cluster's egress IPs). The owner password is
  alphanumeric, at least 16 characters. docs/INSTALL-AKS.md, step 3.
- **Storage:** the account is reachable from the pods (public endpoint, or a
  private endpoint with the `privatelink.blob.core.windows.net` zone linked to
  the cluster's VNet).
- **Key Vault** (optional): Azure RBAC authorization (the plan refuses access
  policies).
- **NATS identities** generated on an admin machine:
  `NEXUS_NATS_DIR=/secure/nexus-nats deploy/nats/nexus-nats-auth.sh init --tenant default`
  (docs/OPERATIONS.md §11.7). The chart refuses an unauthenticated broker.
- **Deployer permissions:** Reader on the cluster, server, storage account and
  vault; Managed Identity Contributor on the identity resource group; **Role
  Based Access Control Administrator** (or User Access Administrator) on the
  container and the vault, to make the two role assignments; Key Vault Secrets
  User on the vault when the owner password is read from it; and the cluster
  role above.

## Apply

```sh
cp terraform.tfvars.example terraform.tfvars      # fill in
cp backend.hcl.example backend.hcl                # fill in
export TF_VAR_postgres_password=...               # unless read from Key Vault
terraform init -backend-config=backend.hcl
terraform plan -out=nexus.tfplan                  # read every refusal before applying
terraform apply nexus.tfplan
```

One apply: the cluster already exists, so there is no two-phase apply. The
first apply waits (`helm_timeout_seconds`, 15 minutes) for the migrate,
bootstrap and db-roles Jobs and every Deployment.

Azure refuses concurrent writes of federated credentials on one identity. If
an apply fails with a conflict on `azurerm_federated_identity_credential`,
run it again (or apply with `-parallelism=1`); the credentials already
written are kept.

## Worker pools

Each `worker_pools` entry is an agent Deployment `agent-<name>`, pinned to the
AKS node pool `node_pool` by the label AKS puts on every node
(`kubernetes.azure.com/agentpool`), tolerating exactly that pool's taints as
read from the pool (`sku=gpu:NoSchedule`, a Spot pool's
`kubernetes.azure.com/scalesetpriority=spot:NoSchedule`, ...). Nothing needs
adding to an existing pool.

- What an agent advertises (`cpu_millis`, `memory_gib`, `gpu_*`) is what the
  scheduler packs tasks against: size it inside one node.
- `gpu_count > 0`: the pod requests that many `nvidia.com/gpu`, so the pool
  needs the NVIDIA device plugin (`install_nvidia_device_plugin`, or the
  cluster's own) and an N-series size; the plan refuses a GPU pool on another
  size.
- A Spot pool's agent is marked `capacityType: spot`: evictions are retried
  without spending `max_attempts`.
- `executor = "kubernetes"`: the agent pod stays on `control_plane_node_pool`
  and runs every attempt as a Job on `node_pool` (docs/OPERATIONS.md §13).
- Agents never go on a pool tainted `CriticalAddonsOnly` (refused). The
  schedulers may, and then tolerate it.

One scheduler Deployment is rendered per queue the pools serve, always
including `default`, with `scheduler.replicas` each.

## The cluster controller: plan, then apply

The controller is installed in **plan** mode: it reports the cluster, every
scheduler and worker pool to the console and shows what it would scale,
without scaling anything.

1. Register the cluster: the `cluster_registration` output is the body of
   `PUT /v1/namespaces/<nexus_namespace>/platform/clusters/<name>` (or use the
   console's Infrastructure page). Each binding's `pool` must be a worker pool
   saved in the namespace's platform settings.
2. Watch the plan in the console until its decisions are the ones you would
   make by hand.
3. Apply mode needs all three opt-ins (docs/OPERATIONS.md §12.3): set
   `cluster_controller = { mode = "apply", expected_cluster_uid = "<uid>" }`
   with the UID from `terraform output cluster_uid` (the plan compares it with
   the live cluster and refuses a mismatch), apply, and switch the console
   registration to apply with the same UID.

Back out with `mode = "plan"` and an apply, or the console's **Switch to
plan** (effective on the next cycle).

## Outputs

| Output | Meaning |
|---|---|
| `namespace` | The release namespace |
| `kubeconfig_command` | `az aks get-credentials ...` (plus `kubelogin convert-kubeconfig` for Entra ID) |
| `console_access` | Port-forward commands for the console and the REST API; the URL when `chart_values_overrides.gateway` names a console host |
| `workload_identity` | Tenant, issuer, both client IDs, and exactly which ServiceAccounts each identity trusts |
| `object_store` | Account, container, role, ServiceAccount |
| `database` | Host, database, owner login, `verify-full`, and the Secrets holding the CA bundle and the runtime logins |
| `cluster_uid` | The kube-system UID that pins apply mode |
| `cluster_registration` | The console registration body (plan mode, one binding per worker pool) |
| `cluster_controller` | Mode as installed, and how to see the leader |
| `admin_api_key` | Sensitive: `terraform output -raw admin_api_key` |

## Removing it

`terraform destroy` uninstalls the release, deletes the namespace if this root
created it (and with it the NATS volume and the generated Secrets), and
deletes the identities, federated credentials and role assignments. It never
touches the cluster, the database or the storage account. A container created
here stops the destroy (`prevent_destroy`): to keep its data, run
`terraform state rm 'azurerm_storage_container.created[0]'` first.

## Cost

Nothing billable is created: managed identities, federated credentials and
role assignments are free. Nexus consumes capacity on the existing node pools
(with the example's two queues and two scheduler replicas each, the control
plane, console, controller and NATS request about 1 vCPU and 0.8 GiB in total,
measured from the rendered chart; the agents add their own requests), the
database and the storage account.
