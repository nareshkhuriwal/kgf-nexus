# Installing KGF Nexus on an existing AKS cluster

This guide installs the whole of Nexus on an Azure Kubernetes Service cluster
you already run: the control plane, the schedulers, worker pools and the
console, against an **Azure Database for PostgreSQL Flexible Server** and
**Azure Blob Storage** reached through **workload identity**. Afterwards the
in-cluster cluster controller reports the cluster to the console and, once you
allow it, applies the scheduler and worker-pool settings you save there.

There are two ways through it, and they meet in the middle:

- **Helm**, from the published chart `oci://ghcr.io/<owner>/charts/kgf-nexus`
  and its example values
  [`examples/aks-existing-cluster.yaml`](../deploy/helm/kgf-nexus/examples/aks-existing-cluster.yaml).
  Every step below.
- **Terraform**, `deploy/terraform/environments/aks-existing`
  ([README](../deploy/terraform/environments/aks-existing/README.md)), which
  reads the existing cluster, server and storage account and creates the
  identities, the Secrets and the release for you. It replaces the steps
  marked *(Terraform does this)*.

The console's **Install on AKS** tab (Infrastructure page) generates the same
values and commands from a form, and links back to the sections here.

**Status.** Chart 0.3.0, validated statically (`deploy/helm/validate.sh`,
OPERATIONS §20.18) and with mocked-provider Terraform plans. Neither path has
yet been run against a live AKS subscription from this repository (finding
F115 in `CONTRACT-FINDINGS.md`): run the preflight below, and read every
refusal it and the chart print.

## Architecture

```
                         Azure subscription
 ┌───────────────────────────────────────────────────────────────────────────────┐
 │ AKS cluster  (OIDC issuer + workload identity webhook)                        │
 │                                                                               │
 │  namespace nexus  ── control-plane release ─────────────────────────────────  │
 │   ui ──/v1──▶ api-gateway :8080 REST ◀── nexusctl, browsers (port-forward,    │
 │                           :9091 worker gRPC ◀── agents            Gateway API)│
 │   workflow-manager   dag-engine   resource-manager   event-service ──▶ nats   │
 │   scheduler, scheduler-<queue>   (system pool)         (JetStream on a PVC)   │
 │   agent-*  ── kubernetes executor ──▶ namespace nexus-tasks: task Jobs        │
 │   nexus-cluster-controller (plan | apply) ─ reports ─▶ resource-manager       │
 │   migrate / bootstrap / db-roles Jobs (each upgrade)                          │
 │                                                                               │
 │  namespace nexus-pool-gpu ── worker-pool release (agents only) ──▶ :9091, nats│
 └───────┬──────────────────────────────────────┬────────────────────────────────┘
         │ TLS, sslmode=verify-full             │ HTTPS, Entra ID token from the
         │ (pinned Azure root CAs)              │ pod's federated ServiceAccount
         ▼                                      ▼
  Azure Database for PostgreSQL           Azure Blob Storage: container nexus-results
  Flexible Server: database nexus,        (results and task logs), role Storage Blob
  owner login + per-service logins        Data Contributor on the container only
                                          Key Vault (optional): akv:// task secrets
```

- **Identity.** No storage key, client secret or connection string exists.
  The result-store pods (api-gateway, workflow-manager, artifact-gc and the
  agents) run as ServiceAccounts that a user-assigned managed identity trusts
  through federated credentials; the AKS webhook injects the token.
- **Database.** The owner login runs only the migrate, bootstrap and db-roles
  Jobs; every service connects as its own least-privilege login
  (OPERATIONS §20.12), over `verify-full` TLS (§20.11).
- **Pod Security.** Every pod the chart renders, and every task pod, passes
  the `restricted` profile (OPERATIONS §20.17).
- **Worker pools** can live in the control-plane release (`agents[]`) or in
  their own releases, one namespace each, scaled and upgraded on their own
  (OPERATIONS §20.16).

## Prerequisites

### Tools

On the machine you install from:

| Tool | Version | For |
|---|---|---|
| Azure CLI (`az`) | 2.60 or later | every Azure step |
| `kubectl` and `kubelogin` | `az aks install-cli` installs both | the cluster (Entra ID clusters need kubelogin) |
| Helm | 3.8 or later (OCI charts); CI validates with 4.3.0 | the Helm path |
| Terraform | 1.11 or later | the Terraform path |
| `psql` | 16 or later | creating the database and its owner |
| `openssl`, a checkout of this repository | | generating credentials; `deploy/nats/nexus-nats-auth.sh`, the CA bundle, the preflight |

You need cluster-admin on the target cluster (Azure Kubernetes Service RBAC
Cluster Admin, or a Kubernetes binding): the chart creates namespaced Roles and
one read-only ClusterRole for the cluster controller.

### What must exist in Azure

- An **AKS cluster** on Kubernetes **1.29 or later** (Chart.yaml `kubeVersion`),
  with Linux user node pools: one for the control plane and agents, optionally
  a GPU pool. A network-policy engine (Azure NPM, Calico or Cilium) for the
  chart's NetworkPolicies to take effect.
- An **Azure Database for PostgreSQL Flexible Server**, PostgreSQL 16 or later,
  with password authentication enabled, reachable from the cluster's pods.
- A general-purpose v2 **storage account**. Nexus uses the Blob API only.
- Optionally a **Key Vault** with Azure RBAC authorization, for `akv://` task
  secrets.

### Permissions

| Who | Role | Scope |
|---|---|---|
| You | Azure Kubernetes Service Contributor (to enable the OIDC issuer and workload identity, add node pools) | the cluster |
| You | Managed Identity Contributor | the resource group the identity goes in |
| You | Role Based Access Control Administrator (or User Access Administrator, or Owner) | the storage container (and the vault) |
| You | the server's admin login | the Flexible Server (once, to create the database and its owner) |
| The Nexus identity | Storage Blob Data Contributor | the one Blob container |
| The secrets identity (optional) | Key Vault Secrets User | the one vault |

Set these once for the commands below:

```sh
az account set --subscription <subscription-id>
export AKS_RG=<cluster resource group>  AKS=<cluster name>
export PG_RG=<server resource group>    PG=<flexible server name>
export ST_RG=<storage resource group>   ST=<storage account name>   CONTAINER=nexus-results
export NS=nexus TASK_NS=nexus-tasks NEXUS_NS=default
export CLUSTER_NAME=aks-prod-weu   # the name the cluster is registered under (cluster.name): a DNS label
export OWNER=<GitHub owner of the release, lower case>  CHART_VERSION=0.3.0  TAG=<vX.Y.Z or a 12-hex commit>
export LOCATION=$(az aks show -g "$AKS_RG" -n "$AKS" --query location -o tsv)
umask 077; mkdir -p ~/nexus-install && cd ~/nexus-install   # credentials land here: keep it private
```

## Preflight

`deploy/scripts/aks-preflight.sh` checks, **read-only**, everything the cluster
can tell about the steps below: the Kubernetes version, the node pools (Ready,
Linux, free pod slots, taints, GPUs advertised), the workload identity webhook
and OIDC issuer, metrics-server, the StorageClass, Gateway/Ingress classes,
the namespace's Pod Security label and, once they exist, the Secrets' keys.
With `--az-resource-group`/`--az-cluster` it asks the AKS API too.

```sh
deploy/scripts/aks-preflight.sh --namespace $NS --system-pool system --pool cpu --gpu-pool gpu \
  --az-resource-group $AKS_RG --az-cluster $AKS
```

Every line is `PASS`, `WARN`, `FAIL` or `SKIP`; the exit code is 1 when anything
failed (`--strict`: also on a warning), and `--json` prints one document for
automation. Run it again after each step. Two opt-in checks create a
short-lived, restricted pod in the namespace and delete it on exit:
`--pull-test <image>` (pulls an image without running any of its code) and
`--db-test <host>` (`pg_isready` from inside the cluster).

## The AKS cluster

**Workload identity.** Check, then enable what is missing:

```sh
az aks show -g $AKS_RG -n $AKS --query '{k8s:currentKubernetesVersion, oidc:oidcIssuerProfile.enabled,
  workloadIdentity:securityProfile.workloadIdentity.enabled, networkPolicy:networkProfile.networkPolicy}' -o table
az aks update -g $AKS_RG -n $AKS --enable-oidc-issuer --enable-workload-identity
export OIDC_ISSUER=$(az aks show -g $AKS_RG -n $AKS --query oidcIssuerProfile.issuerUrl -o tsv)
az aks get-credentials -g $AKS_RG -n $AKS
kubelogin convert-kubeconfig -l azurecli        # clusters with Entra ID integration
```

Enabling the OIDC issuer on a running cluster changes its service-account
token issuer and restarts the API server; pods that use projected tokens may
need a restart afterwards. The issuer URL never changes once set.

**Node pools.** Nexus selects pools by the label AKS puts on every node,
`kubernetes.azure.com/agentpool=<pool>`, so nothing has to be added to an
existing pool. The example values assume:

| Pool | Runs | Notes |
|---|---|---|
| `system` (the AKS system pool, `CriticalAddonsOnly` taint) | the schedulers | small, and dispatch stops without them; they tolerate the taint |
| `cpu` (user) | control plane, console, controller, agents, CPU task pods | spread over zones |
| `gpu` (user, optional) | GPU task pods only | tainted `sku=gpu:NoSchedule` |

```sh
az aks nodepool list -g $AKS_RG --cluster-name $AKS \
  --query '[].{name:name, mode:mode, vm:vmSize, taints:nodeTaints, min:minCount, max:maxCount}' -o table
# A CPU pool and a GPU pool, if you have neither:
az aks nodepool add -g $AKS_RG --cluster-name $AKS -n cpu --mode User --node-vm-size Standard_D8ds_v5 \
  --enable-cluster-autoscaler --min-count 2 --max-count 10 --zones 1 2 3
az aks nodepool add -g $AKS_RG --cluster-name $AKS -n gpu --mode User --node-vm-size Standard_NC24ads_A100_v4 \
  --node-taints sku=gpu:NoSchedule --enable-cluster-autoscaler --min-count 0 --max-count 2
```

**GPUs.** AKS installs the NVIDIA driver on GPU nodes (unless the pool was
created with `--gpu-driver none` for the GPU Operator), but not the device
plugin that advertises `nvidia.com/gpu`. Install it on the GPU pool only. The
plugin chart's default affinity requires node-feature-discovery labels AKS
nodes do not have, so the values below **replace** it; without that the
DaemonSet schedules nowhere:

```sh
cat > nvdp-values.yaml <<'EOF'
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - { key: kubernetes.azure.com/agentpool, operator: In, values: [gpu] }
tolerations:
  - { key: nvidia.com/gpu, operator: Exists, effect: NoSchedule }
  - { key: sku, operator: Equal, value: gpu, effect: NoSchedule }
EOF
helm upgrade --install nvidia-device-plugin nvidia-device-plugin --repo https://nvidia.github.io/k8s-device-plugin \
  --version 0.20.1 -n nvidia-device-plugin --create-namespace -f nvdp-values.yaml --wait
kubectl get nodes -l kubernetes.azure.com/agentpool=gpu -o custom-columns='NODE:.metadata.name,GPUS:.status.allocatable.nvidia\.com/gpu'
```

(Terraform does this with `install_nvidia_device_plugin = true`.) A pool scaled
to zero shows no nodes until a GPU task needs one.

## Namespaces

*(Terraform creates the release namespace, `restricted`.)* One namespace for
the control-plane release and one for the kubernetes executor's task pods,
both enforcing Pod Security `restricted`:

```sh
kubectl create namespace $NS
kubectl create namespace $TASK_NS
kubectl label namespace $NS $TASK_NS pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted
```

Each worker-pool release gets its own namespace later
([Adding worker pools](#adding-worker-pools)).

## Azure Database for PostgreSQL

**1. Check the server.** PostgreSQL 16 or later, password authentication on
(Nexus authenticates with passwords), TLS required (the default):

```sh
az postgres flexible-server show -g $PG_RG -n $PG \
  --query '{version:version, state:state, publicAccess:network.publicNetworkAccess, auth:authConfig}' -o json
az postgres flexible-server parameter show -g $PG_RG -s $PG -n require_secure_transport --query value -o tsv   # ON
export PGHOST=$(az postgres flexible-server show -g $PG_RG -n $PG --query fullyQualifiedDomainName -o tsv)
```

**2. Allow-list pgcrypto.** Migration 0001 runs `CREATE EXTENSION pgcrypto`, and
Flexible Server refuses any extension missing from `azure.extensions`. The
value is the whole list: append, never replace.

```sh
current=$(az postgres flexible-server parameter show -g $PG_RG -s $PG -n azure.extensions --query value -o tsv)
case ",$current," in
  *,PGCRYPTO,*) echo "pgcrypto already allow-listed" ;;
  *) az postgres flexible-server parameter set -g $PG_RG -s $PG -n azure.extensions --value "${current:+$current,}PGCRYPTO" ;;
esac
```

**3. Network: pods must reach port 5432.**

- **Private access** (the server in a delegated subnet): its VNet must be the
  cluster's or peered with it, and the server's private DNS zone must be
  linked to the cluster's VNet:
  `az network private-dns link vnet create -g <zone resource group> -z <zone> -n aks -v <cluster VNet ID> -e false`.
  The cluster's VNet is the one in `az aks show -g $AKS_RG -n $AKS --query 'agentPoolProfiles[0].vnetSubnetId'`,
  or, for a cluster with a managed VNet, the VNet in its node resource group.
- **Public access**: add a firewall rule for each of the cluster's egress
  addresses (outbound type `loadBalancer` shown here; a NAT gateway has its
  own):

  ```sh
  for id in $(az aks show -g $AKS_RG -n $AKS --query 'networkProfile.loadBalancerProfile.effectiveOutboundIPs[].id' -o tsv); do
    ip=$(az network public-ip show --ids "$id" --query ipAddress -o tsv)
    az postgres flexible-server firewall-rule create -g $PG_RG -n $PG -r "aks-egress-${ip//./-}" --start-ip-address "$ip" --end-ip-address "$ip"
  done
  ```

Check from inside the cluster: `deploy/scripts/aks-preflight.sh --namespace $NS --db-test $PGHOST`.

**4. The database and its owner login.** A dedicated owner, not the server
admin: it needs `CREATEROLE` (the migrations create group roles, the db-roles
Job creates the per-service logins) and owns the database. Its password is
embedded in a `postgres://` URL, so keep it to letters and digits:

```sh
openssl rand -hex 24 > postgres-password          # 48 hex characters
psql "host=$PGHOST port=5432 dbname=postgres user=<server admin login> sslmode=verify-full \
      sslrootcert=$REPO/deploy/terraform/ca-bundles/azure-postgres-roots.pem" <<SQL
CREATE ROLE nexus LOGIN CREATEROLE PASSWORD '$(cat postgres-password)';
-- PostgreSQL 16+: the creator holds ADMIN OPTION on the new role; this lets
-- it create a database owned by nexus. Harmless on older versions.
GRANT nexus TO CURRENT_USER;
CREATE DATABASE nexus OWNER nexus;
\c nexus
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL
```

(`$REPO` is your checkout of this repository.) Terraform never creates the
database: destroying a Terraform stack must never be able to drop it.

**5. The CA bundle.** Every pod verifies the server against the Azure roots
vendored in `deploy/terraform/ca-bundles/` (sources and SHA-256 pins there;
rotation: OPERATIONS §20.11):

```sh
(cd $REPO/deploy/terraform/ca-bundles && shasum -a 256 -c SHA256SUMS)
kubectl -n $NS create secret generic nexus-postgres-ca \
  --from-file=ca.crt=$REPO/deploy/terraform/ca-bundles/azure-postgres-roots.pem
```

*(Terraform creates this Secret from the same pinned file.)*

## Azure Blob Storage

Nexus keeps task results and logs in one private container, through the
native Azure Blob backend (`resultStore.backend: azblob`, OPERATIONS §26.3).
The container is created through the resource manager, which works even when
the account's public endpoint is disabled:

```sh
az storage account show -g $ST_RG -n $ST \
  --query '{kind:kind, sku:sku.name, publicNetworkAccess:publicNetworkAccess, hns:isHnsEnabled}' -o json
az storage container-rm create -g $ST_RG --storage-account $ST -n $CONTAINER --public-access off
```

Recommended, as `deploy/terraform/modules/aks-platform` configures its own
account: versioning and soft delete for accidental deletes, and a lifecycle
rule that expires **noncurrent** versions only. Never age out current objects:
the artifact GC alone decides what is unreferenced (OPERATIONS §26.4).

```sh
az storage account blob-service-properties update -g $ST_RG -n $ST --enable-versioning true \
  --enable-delete-retention true --delete-retention-days 30 \
  --enable-container-delete-retention true --container-delete-retention-days 30
cat > lifecycle.json <<EOF
{"rules": [{"name": "expire-noncurrent-versions", "enabled": true, "type": "Lifecycle",
  "definition": {"filters": {"blobTypes": ["blockBlob"], "prefixMatch": ["$CONTAINER/"]},
                 "actions": {"version": {"delete": {"daysAfterCreationGreaterThan": 30}}}}}]}
EOF
az storage account management-policy create -g $ST_RG --account-name $ST --policy @lifecycle.json
```

**Network.** With the account's public access disabled, the pods need a
private endpoint in (or peered with) the cluster's VNet and the
`privatelink.blob.core.windows.net` zone linked to it. The zone answers the
public name, so `resultStore.azure.endpoint` stays empty:

```sh
az network private-endpoint create -g <rg> -n $ST-blob --vnet-name <cluster VNet> --subnet <subnet> \
  --private-connection-resource-id "$(az storage account show -g $ST_RG -n $ST --query id -o tsv)" \
  --group-id blob --connection-name blob
az network private-dns zone create -g <rg> -n privatelink.blob.core.windows.net
az network private-dns link vnet create -g <rg> -z privatelink.blob.core.windows.net -n aks -v <cluster VNet ID> -e false
az network private-endpoint dns-zone-group create -g <rg> --endpoint-name $ST-blob -n blob \
  --private-dns-zone privatelink.blob.core.windows.net --zone-name blob
```

## Workload identity

*(Terraform does this, with two identities when a Key Vault is used.)* One
user-assigned identity holds **Storage Blob Data Contributor on the container**
(not the account: Azure RBAC has no prefix scope below a container, so a
container is the trust boundary). Federated credentials let exactly these
ServiceAccounts obtain it:

| ServiceAccount | Pods |
|---|---|
| `system:serviceaccount:nexus:nexus-artifacts` | api-gateway (task logs), workflow-manager (object-store sensors), artifact-gc |
| `system:serviceaccount:nexus:nexus-agent` | the control-plane release's agents |
| `system:serviceaccount:<pool namespace>:nexus-agent` | each worker-pool release's agents |

```sh
az identity create -g $AKS_RG -n $AKS-nexus-artifacts -l $LOCATION
export WI_CLIENT_ID=$(az identity show -g $AKS_RG -n $AKS-nexus-artifacts --query clientId -o tsv)
WI_PRINCIPAL_ID=$(az identity show -g $AKS_RG -n $AKS-nexus-artifacts --query principalId -o tsv)
# One at a time: Azure refuses concurrent writes of federated credentials on one identity.
for sa in nexus-artifacts nexus-agent; do
  az identity federated-credential create -g $AKS_RG --identity-name $AKS-nexus-artifacts \
    --name "kubernetes-$NS-$sa" --issuer "$OIDC_ISSUER" \
    --subject "system:serviceaccount:$NS:$sa" --audiences api://AzureADTokenExchange
done
az role assignment create --assignee-object-id "$WI_PRINCIPAL_ID" --assignee-principal-type ServicePrincipal \
  --role "Storage Blob Data Contributor" \
  --scope "$(az storage account show -g $ST_RG -n $ST --query id -o tsv)/blobServices/default/containers/$CONTAINER"
```

Role assignments take a few minutes to reach the data plane; an early `403
AuthorizationPermissionMismatch` resolves itself. An identity holds at most 20
federated credentials.

**Key Vault secrets (optional).** Tasks resolve `akv://` references in the
agents (OPERATIONS §25.1). A federated credential trusts a whole
ServiceAccount, and `nexus-artifacts` also runs the gateway, workflow-manager
and artifact-gc, so give secrets their **own** identity, trusted by the agent
accounts only, with Key Vault Secrets User on the one vault (it must use Azure
RBAC authorization):

```sh
az identity create -g $AKS_RG -n $AKS-nexus-secrets -l $LOCATION
az identity federated-credential create -g $AKS_RG --identity-name $AKS-nexus-secrets \
  --name "kubernetes-$NS-nexus-agent" --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:$NS:nexus-agent" --audiences api://AzureADTokenExchange
az role assignment create --assignee-object-id "$(az identity show -g $AKS_RG -n $AKS-nexus-secrets --query principalId -o tsv)" \
  --assignee-principal-type ServicePrincipal --role "Key Vault Secrets User" \
  --scope "$(az keyvault show -n <vault> --query id -o tsv)"
```

Then, in the values, `agentDefaults.secrets.workloadIdentityClientId: <secrets
client ID>` (the agents' `AZURE_CLIENT_ID`) and
`resultStore.azure.clientId: $WI_CLIENT_ID`, which pins the Blob client to the
Blob identity; the agents' `secrets.yaml` names the secrets identity
(`auth: { method: workload_identity, client_id: ... }`).

## Images and chart

The release workflow (`.github/workflows/release.yml`, OPERATIONS §22.4)
publishes, for every push to `main` and every `vX.Y.Z` tag:

- eleven images, `ghcr.io/<owner>/kgf-nexus/<component>`, tagged with the
  commit (12 hex digits) and, for a tag, `vX.Y.Z`, each with a Sigstore build
  provenance attestation (docs/IMAGES.md);
- the run artifact `image-digests`: `release-images.yaml`, the Helm values that
  pin every image to its digest;
- the chart `oci://ghcr.io/<owner>/charts/kgf-nexus`: a `vX.Y.Z` tag
  publishes version `X.Y.Z`, which must be Chart.yaml's (0.3.0 here); `main`
  publishes `<version>-main.g<commit>`; attested the same way.

```sh
gh run download <release run id> --repo $OWNER/kgf-nexus -n image-digests      # release-images.yaml
helm show chart oci://ghcr.io/$OWNER/charts/kgf-nexus --version $CHART_VERSION
gh attestation verify oci://ghcr.io/$OWNER/charts/kgf-nexus:$CHART_VERSION --repo $OWNER/kgf-nexus
helm pull oci://ghcr.io/$OWNER/charts/kgf-nexus --version $CHART_VERSION --untar   # kgf-nexus/examples/*.yaml
```

Never install the moving `main` tag, and there is no `latest` (the chart
refuses it).

**Private packages.** GHCR creates packages private. Make the eleven image
packages and `charts/kgf-nexus` public, or give the cluster read access with a
GitHub token that has `read:packages`:

```sh
kubectl -n $NS create secret docker-registry ghcr-pull --docker-server=ghcr.io \
  --docker-username=<github user> --docker-password=<token with read:packages>
helm registry login ghcr.io -u <github user> --password-stdin <<<"<token>"
```

and set `image.pullSecrets: [ghcr-pull]` (in every namespace a release runs
in). Clusters that may not pull from GHCR mirror the images into their own
registry by digest (`az acr import`, docs/AIRGAP.md) and set `image.registry`.
`--pull-test ghcr.io/$OWNER/kgf-nexus/agent:$TAG` in the preflight proves a
node can pull.

## NATS

The event bus carries dispatch hints and run events (OPERATIONS §11). The chart
refuses an unauthenticated broker, so generate NKey identities **on an admin
machine**, never in the cluster:

```sh
export NEXUS_NATS_DIR=~/nexus-install/nats
$REPO/deploy/nats/nexus-nats-auth.sh init --tenant $NEXUS_NS     # event-service, monitor, agent-<ns>
$REPO/deploy/nats/nexus-nats-auth.sh helm-values > nats-values.yaml   # public keys, for the values
```

The seeds (`*/user.nk`) go into the credentials Secret below; keep the
directory (it is how you rotate, §11.8). Worker-pool releases serving the same
Nexus namespace use the same `agent-<ns>` seed, or an identity of their own
whose public key you add to that tenant's `publicKeys` (§11.7). Without NATS
(`nats.enabled: false`, `agentDefaults.nats.url: ""`) the agents long-poll the
gateway (ADR-0003): simpler, with more dispatch latency.

## Kubernetes Secrets

*(Terraform generates and creates all three.)* No credential is ever rendered
into a manifest; the chart reads them from Secrets:

| Secret | Keys |
|---|---|
| `nexus-credentials` (`existingSecret`) | `postgres-password` (the owner's), `worker-api-key`, `admin-api-key`, `nats-event-service-nkey`, `nats-agent-nkey` |
| `nexus-db-logins` (`postgres.runtimeRoles.existingSecret`) | `postgres-password-<component>` for api-gateway, workflow-manager, dag-engine, scheduler, resource-manager, event-service, artifact-gc |
| `nexus-postgres-ca` (`postgres.caBundle.secretName`) | `ca.crt` (created above) |

```sh
printf 'nxs_worker_%s' "$(openssl rand -hex 32)" > worker-api-key   # the platform's key format
printf 'nxs_admin_%s'  "$(openssl rand -hex 32)" > admin-api-key
kubectl -n $NS create secret generic nexus-credentials \
  --from-file=postgres-password --from-file=worker-api-key --from-file=admin-api-key \
  --from-file=nats-event-service-nkey=$NEXUS_NATS_DIR/event-service/user.nk \
  --from-file=nats-agent-nkey=$NEXUS_NATS_DIR/agent-$NEXUS_NS/user.nk

args=()
for c in api-gateway workflow-manager dag-engine scheduler resource-manager event-service artifact-gc; do
  openssl rand -hex 24 > "postgres-password-$c"
  args+=(--from-file="postgres-password-$c")
done
kubectl -n $NS create secret generic nexus-db-logins "${args[@]}"

deploy/scripts/aks-preflight.sh --namespace $NS --secret nexus-credentials \
  --db-logins-secret nexus-db-logins --ca-secret nexus-postgres-ca
```

The bootstrap Job registers `worker-api-key` for the agents and installs
`admin-api-key` as an admin key. Keep copies outside the cluster (Key Vault):
a lost `postgres-password` is recovered on the server, a lost NATS seed by
rotation (§11.8).

## Install with Helm

Start from `kgf-nexus/examples/aks-existing-cluster.yaml` (pulled above) and
change every value marked `REPLACE`:

| Value | Set it to |
|---|---|
| `cluster.name`, `clusterController.clusterName` | `$CLUSTER_NAME` |
| `image.repositoryPrefix`, `image.tag` | `$OWNER/kgf-nexus/`, `$TAG` |
| `postgres.host`, `postgres.user` | `$PGHOST`, `nexus` |
| `resultStore.azure.account`, `minio.bucket` | `$ST`, `$CONTAINER` |
| `serviceAccounts.*.annotations` `azure.workload.identity/client-id` | `$WI_CLIENT_ID` |
| `nats.auth.*` | from `nats-values.yaml` (or pass that file with `-f`) |
| `placement.nodeSelector`, `schedulers[].nodeSelector`, `agents[].nodeSelector`, `.kubernetes.taskNodeSelector` | your pool names |
| `agents[]` | what each agent advertises: size it inside one node (or one task pool) |

Then install, passing `release-images.yaml` **last** so nothing overrides the
digests that were built and attested:

```sh
helm upgrade --install nexus oci://ghcr.io/$OWNER/charts/kgf-nexus --version $CHART_VERSION -n $NS \
  -f aks-existing-cluster.yaml -f nats-values.yaml -f release-images.yaml --set image.tag=$TAG \
  --wait --wait-for-jobs --timeout 15m
kubectl -n $NS wait --for=condition=Ready pod \
  -l 'app.kubernetes.io/component notin (migrate,bootstrap,db-roles,minio-init,artifact-gc)' --timeout=5m
```

Do not trust `--wait` alone: with Helm 4 a release has reported `deployed`
while a pod was still crash-looping (OPERATIONS §20.10). Every service waits in
its `wait-for-schema` init container until the migrate Job has recorded this
image's migrations: expected, not a hang. `helm get notes nexus -n $NS`
summarises what is on.

## Install with Terraform

`deploy/terraform/environments/aks-existing` does the namespaces, workload
identity, Secrets and release steps for you, from data sources for what
already exists. You still do [The AKS cluster](#the-aks-cluster),
[Azure Database for PostgreSQL](#azure-database-for-postgresql) steps 1-4, the
storage account's network and lifecycle settings, and [NATS](#nats).

```sh
cd $REPO/deploy/terraform/environments/aks-existing
cp terraform.tfvars.example terraform.tfvars      # the cluster, server, account, pools, image, chart
cp backend.hcl.example backend.hcl                # remote state: it holds every credential
export TF_VAR_postgres_password=$(cat ~/nexus-install/postgres-password)   # or a Key Vault secret
terraform init -backend-config=backend.hcl
terraform plan -out=nexus.tfplan && terraform apply nexus.tfplan
terraform output -json workload_identity cluster_registration
```

It authenticates to the cluster with kubelogin (Entra ID) by default
(`kube_auth`), refuses a cluster without an OIDC issuer, agents on a
`CriticalAddonsOnly` pool and a GPU pool on a non-GPU VM size, reads each
pool's taints to tolerate them, and installs the controller in plan mode.
Its README lists every resource it reads and creates.

## Verify the install

```sh
kubectl -n $NS get pods -o wide                        # every pod Running/Completed, on the intended pools
kubectl -n $NS get jobs                                # migrate-*, bootstrap-*, db-roles-* Complete
kubectl -n $NS logs deploy/api-gateway | grep -m1 'durable task logs enabled'   # names backend azblob
kubectl -n $NS port-forward svc/ui 3000:80             # console: http://localhost:3000
kubectl -n $NS port-forward svc/api-gateway 8090:8080  # REST API: http://localhost:8090/v1
```

Sign in to the console with the admin key (`admin-api-key`). Then run an
example end to end, which exercises the scheduler, an agent, the result store
and task logs. Pick the one your agents' executor runs:

```sh
export NEXUS_ENDPOINT=http://localhost:8090 NEXUS_API_KEY=$(cat ~/nexus-install/admin-api-key) NEXUS_NAMESPACE=$NEXUS_NS
# kubernetes executor (the example values): two busybox Jobs in $TASK_NS
nexusctl workflow submit $REPO/examples/kubernetes_executor/spec.yaml
nexusctl run create kubernetes-executor-demo
kubectl -n $TASK_NS get jobs,pods -w
# subprocess executor: three shell tasks inside the agent pods
nexusctl workflow submit $REPO/examples/hello_dag/compiled.yaml
nexusctl run create hello-dag --param source=s3://example/input.csv
nexusctl events <run-id> --follow
```

A run that reaches `SUCCEEDED` proves the database, the agents' registration,
the scheduler and the Blob store through workload identity. (The task pods
pull `busybox` from Docker Hub; mirror it where the cluster cannot.)

To expose the console beyond port-forwarding, enable the chart's Gateway API
routes (`gateway.*`, with a GatewayClass the cluster has) or point your own
ingress at the `ui` Service (port 80); set `smtp.publicUrl` to that URL.

## Register the cluster: plan, then apply

The cluster controller (`clusterController.enabled`, OPERATIONS §12, design in
docs/CLUSTER-SCALING.md) runs two replicas, one leading. In **plan** mode,
the default, it reports the cluster, its schedulers and worker pools to the
console and shows what it *would* change. In **apply** mode it also applies the
scheduler autoscaling and worker-pool sizes you save in the console.

**1. Register the cluster** under the name in `cluster.name`, on the
Infrastructure page's **Clusters** tab, or through the API (admin key; the
Terraform output `cluster_registration` is this body, filled in):

```sh
UID_KS=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
curl -fsS -X PUT "$NEXUS_ENDPOINT/v1/namespaces/$NEXUS_NS/platform/clusters/$CLUSTER_NAME" \
  -H "Authorization: Bearer $NEXUS_API_KEY" -H 'Content-Type: application/json' -d @- <<EOF
{"provider": "aks",
 "provider_resource_id": "$(az aks show -g $AKS_RG -n $AKS --query id -o tsv)",
 "expected_cluster_uid": "$UID_KS",
 "control_plane_namespace": "$NS",
 "mode": "plan", "desired_revision": 0,
 "bindings": [{"kubernetes_namespace": "$NS", "worker_deployment": "agent-cpu", "pool": "default", "max_replicas": 10}]}
EOF
```

Each binding names an agent Deployment and a worker pool saved in the
namespace's platform settings (the **Scheduler** and **Worker pools** tabs, or
`PUT /v1/namespaces/{ns}/platform/config`).

**2. Watch the plan.** On the **Clusters** and **Scaling plan** tabs: a report
under a minute old, "Controller running: plan", the observed kube-system UID
matching the registration, and plan-only decisions you agree with
(OPERATIONS §12.2). The leader:
`kubectl -n $NS get lease nexus-cluster-controller-$CLUSTER_NAME -o jsonpath='{.spec.holderIdentity}'`.

**3. Apply** needs all three opt-ins (OPERATIONS §12.3); missing any one keeps
plan mode, and the console names which:

- **The registration** switched to apply, with `expected_cluster_uid` =
  `$UID_KS` (the API refuses apply without it): the Clusters tab, or the PUT
  above with `"mode": "apply"` and the current `desired_revision`.
- **The process permission and the scale RBAC** (the other two), from the
  chart:

  ```sh
  helm upgrade nexus oci://ghcr.io/$OWNER/charts/kgf-nexus --version $CHART_VERSION -n $NS \
    -f aks-existing-cluster.yaml -f nats-values.yaml -f release-images.yaml --set image.tag=$TAG \
    --set clusterController.apply.enabled=true --set clusterController.apply.expectedClusterUID=$UID_KS
  ```

  The chart compares that UID with the live cluster at install and refuses a
  mismatch, so a values file written for one cluster cannot enable apply on
  another. (Terraform: `cluster_controller = { mode = "apply",
  expected_cluster_uid = "<uid>" }`, checked the same way at plan time.)

**Scheduler autoscaling.** The Scheduler settings saved in the console (min
and max replicas, CPU target, scale-down window) reach an HPA, never the
scheduler Deployment: give each scheduler one (`schedulers[].autoscaling`, or
Terraform's `scheduler.autoscaling`) and mark it as the controller's:

```sh
kubectl -n $NS annotate hpa scheduler nexus.khuriwalgroup.com/autoscaling-owner=cluster-controller
```

In plan mode the console shows the difference; in apply mode the controller
moves the HPA there in bounded steps. A `helm upgrade` resets the four fields
to the rendered values and the controller restores the saved ones within a
minute (OPERATIONS §12, "Scheduler autoscaling").

**Back out** at any moment: the console's **Switch to plan** (the next cycle
stops mutating), or the upgrade without the two `--set`s (OPERATIONS §12.4).
Keep `max_replicas` within budget: there is no cost estimate.

## Adding worker pools

**In the control-plane release**: add an `agents[]` entry pinned to the pool
(`nodeSelector: { kubernetes.azure.com/agentpool: <pool> }`, plus the pool's
taints as tolerations; a GPU agent tolerates `nvidia.com/gpu` by itself) and a
scheduler for any new queue, then `helm upgrade` as above. Simplest, but every
change rolls through the control-plane release.

**As its own release** (`components.controlPlane.enabled=false`, OPERATIONS
§20.16): a pool scaled, upgraded and placed on its own, in its own namespace
(the chart's names, ServiceAccount `nexus-agent` and the agent Deployments,
are fixed per namespace). Start from `kgf-nexus/examples/aks-worker-pool.yaml`:

```sh
export POOL_NS=nexus-pool-gpu
kubectl create namespace $POOL_NS
kubectl label namespace $POOL_NS pod-security.kubernetes.io/enforce=restricted pod-security.kubernetes.io/warn=restricted
# Its credentials: a worker key of the Nexus namespace (here the control plane's) and a NATS seed.
kubectl -n $POOL_NS create secret generic $POOL_NS-credentials \
  --from-file=worker-api-key --from-file=nats-agent-nkey=$NEXUS_NATS_DIR/agent-$NEXUS_NS/user.nk
# Its agents' account may use the result store (and secrets):
az identity federated-credential create -g $AKS_RG --identity-name $AKS-nexus-artifacts \
  --name "kubernetes-$POOL_NS-nexus-agent" --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:$POOL_NS:nexus-agent" --audiences api://AzureADTokenExchange
helm upgrade --install $POOL_NS oci://ghcr.io/$OWNER/charts/kgf-nexus --version $CHART_VERSION -n $POOL_NS \
  --set components.controlPlane.enabled=false --set existingSecret=$POOL_NS-credentials \
  -f aks-worker-pool.yaml -f release-images.yaml --set image.tag=$TAG
kubectl -n $POOL_NS wait --for=condition=Ready pod -l nexus.khuriwalgroup.com/role=agent --timeout=5m
```

(Terraform: `extra_federated_service_accounts = [{ namespace = "nexus-pool-gpu",
name = "nexus-agent" }]`.) An agent turns Ready only once the control plane
accepted it, so the wait proves the two releases agree. The chart checks each
release on its own, never the pair. These must match, from OPERATIONS §20.16:

| In the pool release | Must match in the control-plane release |
|---|---|
| `agentDefaults.controlPlane.address` | its gateway, `api-gateway.<namespace>.svc:9091` (a bare `api-gateway:9091` resolves only in the control plane's namespace) |
| `tls.enabled` + `tls.certManager.issuerRef` | the same ClusterIssuer |
| `agentDefaults.nats.url` | `nats://nats.<namespace>.svc:4222`, and `nats.networkPolicy.extraFrom` admitting the pool namespace's agents |
| the `nats-agent-nkey` seed | its public key in `nats.auth.tenants` for the Nexus namespace served |
| `worker-api-key` | a worker key of that Nexus namespace |
| `resultStore.*`, `minio.bucket` | the same store, plus the federated credential above |
| `cluster.name` | the registered cluster |
| the pool namespace | listed in `clusterController.workerNamespaces` |

So also upgrade the control-plane release with the pool namespace in
`nats.networkPolicy.extraFrom` and `clusterController.workerNamespaces`, and
bind the pool's agent Deployment in the cluster registration.

**GPU pools**: the device plugin on the pool ([The AKS cluster](#the-aks-cluster)),
and an agent whose `gpuCount`, `gpuKind` and `gpuMemoryBytes` describe what one
node (subprocess executor) or the task pool (kubernetes executor) offers
(OPERATIONS §20.14). Spot pools: taint
`kubernetes.azure.com/scalesetpriority=spot:NoSchedule` tolerated and
`capacityType: spot` on the agent, so evictions are retried, not charged.

## Monitoring in the console

The **Infrastructure** page (administrators) shows the cluster live:

| Tab | What to check after an install |
|---|---|
| Overview | every control-plane component with its instances, each placed on its cluster, node and pod (`cluster.name` and the downward-API identity every process reports) |
| Scheduler | one scheduler Deployment per queue, its replicas, and the autoscaling you saved |
| Worker pools | each pool's workers: registered, live, advertised capacity |
| Task runners | where each running attempt runs: agent, task pod, node |
| Clusters | the registration, the controller's last report and mode |
| Scaling plan | what the controller would change (plan) or changed (apply), with its reasons |

The same data is in the API: `GET /v1/namespaces/{ns}/platform/components`,
`.../platform/task-runners` and `.../platform/clusters`. Prometheus scraping
and alerts: OPERATIONS §17 (`metrics.serviceMonitor`, or Azure Monitor managed
Prometheus with `networkPolicy.metricsFrom`).

## Upgrades

1. Back up first (the next section) and read the release notes.
2. Upgrade the **control-plane release** to the new chart version and image
   tag with the **same values files** and the new `release-images.yaml`
   (`-f`, or `--reset-then-reuse-values`; never `--reuse-values`, which drops
   new defaults). The migrate Job runs first; every service waits for it
   (OPERATIONS §6.3, §20.2).
3. Then each **worker-pool release**, at its own pace: an agent one minor
   version behind is supported (docs/COMPATIBILITY.md).
4. Terraform: bump `image.tag`, `image.digests` and `chart.version`, then plan
   and apply.

## Backup and restore

- **Database**: Flexible Server's automated backups and point-in-time restore
  are the supported path for a managed server (OPERATIONS §4): choose the
  retention (7-35 days) and, for disaster recovery, geo-redundant backup.
  Restore creates a new server (`az postgres flexible-server restore`); point
  `postgres.host` at it and upgrade.
- **Results and logs**: Blob versioning and soft delete (above) cover
  accidental deletes; OPERATIONS §7 covers backing up and restoring the store
  and reconciling it with the database.
- **Credentials**: the NATS key directory and the three Secrets' values,
  outside the cluster (Key Vault). Terraform state holds them too: keep the
  state container private.

## Uninstall

```sh
helm uninstall nexus-pool-gpu -n nexus-pool-gpu     # each pool release first
helm uninstall nexus -n $NS
kubectl delete namespace $NS $TASK_NS nexus-pool-gpu   # the NATS volume and the Secrets go with them
az identity delete -g $AKS_RG -n $AKS-nexus-artifacts  # its federated credentials and role assignment go with it
```

The database and the container keep your data until you drop them
deliberately. Terraform: `terraform destroy` (a container it created is
protected by `prevent_destroy`; `terraform state rm` it to keep it).

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Pods `Pending`: "didn't match Pod's node affinity/selector" | a pool name in the values is wrong, or the pool is at zero and cannot scale up | `kubectl get nodes -L kubernetes.azure.com/agentpool`; fix the `nodeSelector` |
| Pods `Pending`: "untolerated taint" | the pool's taint is not tolerated | add the taint as a toleration on that workload (`schedulers[]`, `agents[]`, `.kubernetes.taskTolerations`) |
| GPU agent or task `Pending`: "Insufficient nvidia.com/gpu" | no device plugin on the pool, or its default affinity kept it off the nodes | install it as in [The AKS cluster](#the-aks-cluster); the preflight's `gpu.<pool>` check |
| `ImagePullBackOff` from `ghcr.io` | the packages are private | make them public, or `image.pullSecrets` (Images and chart) |
| `violates PodSecurity "restricted"` | a chart older than 0.3.0, or a pod of your own | upgrade the chart; label `baseline` only for what cannot be fixed |
| migrate Job fails: extension `pgcrypto` "is not allow-listed" | `azure.extensions` lacks `PGCRYPTO` | step 2 of Azure Database for PostgreSQL |
| migrate Job fails: "permission denied to create role" | the owner login lacks `CREATEROLE` | `ALTER ROLE nexus CREATEROLE;` as the server admin |
| `certificate verify failed` / `x509: certificate signed by unknown authority` | the CA Secret is not the Azure roots, or `postgres.host` is an IP or alias the certificate does not name | recreate `nexus-postgres-ca` from the pinned file; use the server's FQDN |
| `password authentication failed` | `postgres-password` differs from the owner's, or the server allows Entra ID only | fix the Secret; enable password authentication |
| database connection times out | firewall rule or VNet/DNS link missing | Azure Database for PostgreSQL step 3; `--db-test` in the preflight |
| result store: `AADSTS70021: No matching federated identity record found` | the federated credential's subject or issuer does not match the pod's ServiceAccount | subject `system:serviceaccount:<namespace>:<account>`, issuer = `$OIDC_ISSUER`; check `kubectl -n $NS get pod <pod> -o jsonpath='{.spec.serviceAccountName}'` |
| result store: `403 AuthorizationPermissionMismatch` | role assignment missing, on the wrong scope, or still propagating | Storage Blob Data Contributor on the container; wait a few minutes |
| result store: "is the pod labelled azure.workload.identity/use=true" | the webhook did not inject the token | the preflight's `wi.webhook`; restart the pod after enabling workload identity |
| result store: `no such host` for `<account>.blob.core.windows.net` | private endpoint without a linked `privatelink.blob.core.windows.net` zone | link the zone to the cluster's VNet |
| pool agents never `Ready` | `agentDefaults.controlPlane.address` is a bare name, a wrong worker key, or NATS not admitted | the cross-release table in Adding worker pools |
| `helm upgrade` refuses: `expectedClusterUID ... does not match` | the values were written for another cluster | `kubectl get ns kube-system -o jsonpath='{.metadata.uid}'` against the intended cluster |
| controller stays in plan after enabling apply | one of the three opt-ins is missing | the Clusters tab names it (OPERATIONS §12.3) |
| HPAs show `<unknown>` targets | metrics-server unavailable | the preflight's `metrics-server` check |
| `nats-0` `Pending` on its volume | the StorageClass does not exist | `nats.storage.storageClass` (AKS: `managed-csi-premium`) |
