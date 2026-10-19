#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# aks-preflight.sh -- is this AKS cluster ready for KGF Nexus?
# (docs/INSTALL-AKS.md, step 0; deploy/terraform/environments/aks-existing)
#
# READ-ONLY by default: every Kubernetes call is a get or a list against the
# current (or --context) kube context, and `az` is called only with read
# commands, only when --az-resource-group/--az-cluster are given and a login
# exists. Two checks CREATE a short-lived pod and are off unless asked for:
#   --pull-test IMAGE   pulls IMAGE on a node (the container never runs any of
#                       the image's code: its command does not exist)
#   --db-test HOST[:PORT]   pg_isready from inside the cluster (TCP + the
#                       PostgreSQL handshake; no credentials)
# Both run in --namespace, which must exist, meet the "restricted" Pod
# Security profile, and are deleted on exit, whatever happens.
#
# Output: one line per check, PASS / WARN / FAIL / SKIP, or --json. Exit code
# 0 when nothing FAILed (with --strict: nothing WARNed either), 1 when a check
# FAILed, 2 on a usage error.
#
# Portable: bash 3.2 (macOS) or later, kubectl, and the usual POSIX tools.
# No jq: kubectl's jsonpath does the parsing.
#
#   deploy/scripts/aks-preflight.sh --namespace nexus --system-pool system \
#     --pool cpu --gpu-pool gput4 --secret nexus-credentials \
#     --db-logins-secret nexus-db-logins --ca-secret nexus-postgres-ca
#   deploy/scripts/aks-preflight.sh --help

set -u
set -o pipefail

PROG=${0##*/}
MIN_K8S_MINOR=29 # Chart.yaml kubeVersion ">=1.29.0-0"

usage() {
  cat <<'EOF'
Usage: aks-preflight.sh [options]

Cluster:
  --context NAME            kube context (default: the current one)
  --namespace NS            release namespace (default: nexus)
  --pool-label KEY          node label naming the pool (default: kubernetes.azure.com/agentpool)
  --system-pool NAME        pool for the control plane / schedulers (optional)
  --pool NAME               a CPU worker pool that must exist (repeatable)
  --gpu-pool NAME           a GPU worker pool: must advertise nvidia.com/gpu (repeatable)
  --min-pod-headroom N      free pod slots wanted per pool (default: 10)
  --storage-class NAME      StorageClass the release uses (default: managed-csi-premium;
                            "" = the cluster default must exist)
  --tls                     the release uses mTLS: cert-manager must be installed

Release Secrets (checked only when named; key NAMES and formats, values never printed):
  --secret NAME             existingSecret: postgres-password, worker-api-key,
                            admin-api-key, NATS seeds
  --no-nats                 the release has no NATS (agents long-poll): no seed keys needed
  --db-logins-secret NAME   runtime logins: postgres-password-<component>
  --ca-secret NAME          the database CA bundle (key ca.crt)

Azure (read-only, optional; needs `az login`):
  --az-resource-group RG    with --az-cluster: OIDC issuer, workload identity,
  --az-cluster NAME         version and node pools from the AKS API

Opt-in checks that CREATE a pod in --namespace (deleted on exit):
  --pull-test IMAGE         pull IMAGE on a node, e.g. ghcr.io/<owner>/kgf-nexus/agent:<tag>
  --pull-secret NAME        imagePullSecret for --pull-test (private registries)
  --db-test HOST[:PORT]     pg_isready against the database from inside the cluster
  --db-test-image IMAGE     image with pg_isready (default: postgres:16-alpine)
  --test-pool NAME          run the test pods on this pool
  --test-timeout SECONDS    how long a test pod may take (default: 180)

Output:
  --json                    one JSON document instead of lines
  --strict                  exit 1 on WARN as well
  -h, --help                this text
EOF
}

# ── Arguments ───────────────────────────────────────────────────────────

CONTEXT=""
NAMESPACE="nexus"
POOL_LABEL="kubernetes.azure.com/agentpool"
SYSTEM_POOL=""
POOLS=""
GPU_POOLS=""
MIN_HEADROOM=10
STORAGE_CLASS="managed-csi-premium"
TLS=0
SECRET=""
NATS=1
DB_LOGINS_SECRET=""
CA_SECRET=""
AZ_RG=""
AZ_CLUSTER=""
PULL_IMAGE=""
PULL_SECRET=""
DB_TEST=""
DB_TEST_IMAGE="postgres:16-alpine"
TEST_POOL=""
TEST_TIMEOUT=180
JSON=0
STRICT=0

die_usage() {
  echo "$PROG: $*" >&2
  echo "Run $PROG --help for usage." >&2
  exit 2
}

need_value() {
  [ $# -ge 2 ] || die_usage "$1 needs a value"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --context) need_value "$@"; CONTEXT=$2; shift 2 ;;
    --namespace) need_value "$@"; NAMESPACE=$2; shift 2 ;;
    --pool-label) need_value "$@"; POOL_LABEL=$2; shift 2 ;;
    --system-pool) need_value "$@"; SYSTEM_POOL=$2; shift 2 ;;
    --pool) need_value "$@"; POOLS="$POOLS $2"; shift 2 ;;
    --gpu-pool) need_value "$@"; GPU_POOLS="$GPU_POOLS $2"; shift 2 ;;
    --min-pod-headroom) need_value "$@"; MIN_HEADROOM=$2; shift 2 ;;
    --storage-class) need_value "$@"; STORAGE_CLASS=$2; shift 2 ;;
    --tls) TLS=1; shift ;;
    --secret) need_value "$@"; SECRET=$2; shift 2 ;;
    --no-nats) NATS=0; shift ;;
    --db-logins-secret) need_value "$@"; DB_LOGINS_SECRET=$2; shift 2 ;;
    --ca-secret) need_value "$@"; CA_SECRET=$2; shift 2 ;;
    --az-resource-group) need_value "$@"; AZ_RG=$2; shift 2 ;;
    --az-cluster) need_value "$@"; AZ_CLUSTER=$2; shift 2 ;;
    --pull-test) need_value "$@"; PULL_IMAGE=$2; shift 2 ;;
    --pull-secret) need_value "$@"; PULL_SECRET=$2; shift 2 ;;
    --db-test) need_value "$@"; DB_TEST=$2; shift 2 ;;
    --db-test-image) need_value "$@"; DB_TEST_IMAGE=$2; shift 2 ;;
    --test-pool) need_value "$@"; TEST_POOL=$2; shift 2 ;;
    --test-timeout) need_value "$@"; TEST_TIMEOUT=$2; shift 2 ;;
    --json) JSON=1; shift ;;
    --strict) STRICT=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) die_usage "unknown argument '$1'" ;;
  esac
done

is_uint() { case "$1" in '' | *[!0-9]*) return 1 ;; *) return 0 ;; esac; }
is_uint "$MIN_HEADROOM" || die_usage "--min-pod-headroom must be a non-negative integer"
is_uint "$TEST_TIMEOUT" || die_usage "--test-timeout must be a non-negative integer"
case "$NAMESPACE" in
  '' | *[!a-z0-9-]* | -* | *-) die_usage "--namespace must be a Kubernetes namespace name" ;;
esac
if { [ -n "$AZ_RG" ] && [ -z "$AZ_CLUSTER" ]; } || { [ -z "$AZ_RG" ] && [ -n "$AZ_CLUSTER" ]; }; then
  die_usage "--az-resource-group and --az-cluster go together"
fi

# ── Results ─────────────────────────────────────────────────────────────

N=0
COUNT_PASS=0
COUNT_WARN=0
COUNT_FAIL=0
COUNT_SKIP=0
R_ID=()
R_STATUS=()
R_DETAIL=()

if [ "$JSON" = 0 ] && [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_PASS=$'\033[32m' C_WARN=$'\033[33m' C_FAIL=$'\033[31m' C_SKIP=$'\033[2m' C_OFF=$'\033[0m'
else
  C_PASS="" C_WARN="" C_FAIL="" C_SKIP="" C_OFF=""
fi

# record ID STATUS DETAIL
record() {
  R_ID[N]=$1
  R_STATUS[N]=$2
  R_DETAIL[N]=$3
  N=$((N + 1))
  local color
  case "$2" in
    PASS) COUNT_PASS=$((COUNT_PASS + 1)); color=$C_PASS ;;
    WARN) COUNT_WARN=$((COUNT_WARN + 1)); color=$C_WARN ;;
    FAIL) COUNT_FAIL=$((COUNT_FAIL + 1)); color=$C_FAIL ;;
    *) COUNT_SKIP=$((COUNT_SKIP + 1)); color=$C_SKIP ;;
  esac
  if [ "$JSON" = 0 ]; then
    printf '%s%-4s%s  %-28s %s\n' "$color" "$2" "$C_OFF" "$1" "$3"
  fi
}

json_escape() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\r'/\\r}
  s=${s//$'\t'/\\t}
  printf '%s' "$s"
}

finish() {
  if [ "$JSON" = 1 ]; then
    local i sep=""
    printf '{"tool":"aks-preflight","context":"%s","namespace":"%s",' "$(json_escape "$CONTEXT_NAME")" "$(json_escape "$NAMESPACE")"
    printf '"summary":{"pass":%d,"warn":%d,"fail":%d,"skip":%d},"checks":[' "$COUNT_PASS" "$COUNT_WARN" "$COUNT_FAIL" "$COUNT_SKIP"
    i=0
    while [ "$i" -lt "$N" ]; do
      printf '%s{"id":"%s","status":"%s","detail":"%s"}' "$sep" "$(json_escape "${R_ID[i]}")" "${R_STATUS[i]}" "$(json_escape "${R_DETAIL[i]}")"
      sep=","
      i=$((i + 1))
    done
    printf ']}\n'
  else
    printf '\n%d passed, %d warnings, %d failed, %d skipped (context %s)\n' \
      "$COUNT_PASS" "$COUNT_WARN" "$COUNT_FAIL" "$COUNT_SKIP" "$CONTEXT_NAME"
  fi
  if [ "$COUNT_FAIL" -gt 0 ] || { [ "$STRICT" = 1 ] && [ "$COUNT_WARN" -gt 0 ]; }; then
    exit 1
  fi
  exit 0
}

# ── kubectl: read-only, and the two opt-in mutations ────────────────────

TMP=$(mktemp -d "${TMPDIR:-/tmp}/aks-preflight.XXXXXX") || { echo "$PROG: mktemp failed" >&2; exit 2; }
CREATED_PODS=""

# shellcheck disable=SC2329 # run by the EXIT trap below
cleanup() {
  local p
  for p in $CREATED_PODS; do
    kmut delete pod "$p" -n "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# Every read goes through kro, which admits only read verbs, so a later edit
# cannot quietly make the default mode write to the cluster.
kro() {
  case "$1" in
    get | version | config | api-resources | auth) ;;
    *) echo "$PROG: internal error: '$1' is not a read-only kubectl verb" >&2; exit 2 ;;
  esac
  if [ -n "$CONTEXT" ]; then
    kubectl --context "$CONTEXT" --request-timeout=20s "$@"
  else
    kubectl --request-timeout=20s "$@"
  fi
}

# The opt-in tests only: create and delete the preflight's own pods.
kmut() {
  case "$1" in
    create | delete) ;;
    *) echo "$PROG: internal error: '$1' is not an allowed mutation" >&2; exit 2 ;;
  esac
  if [ -n "$CONTEXT" ]; then
    kubectl --context "$CONTEXT" --request-timeout=20s "$@"
  else
    kubectl --request-timeout=20s "$@"
  fi
}

# A jsonpath key: dots escaped (kubernetes.azure.com/agentpool).
jp_key() { printf '%s' "$1" | sed 's/\./\\./g'; }

first_line() { printf '%s\n' "$1" | head -n 1; }

# ── Reachability and version ────────────────────────────────────────────

CONTEXT_NAME=$CONTEXT
if ! command -v kubectl >/dev/null 2>&1; then
  CONTEXT_NAME=${CONTEXT_NAME:-none}
  record tools.kubectl FAIL "kubectl is not on PATH (az aks install-cli installs it)"
  finish
fi
if [ -z "$CONTEXT_NAME" ]; then
  CONTEXT_NAME=$(kro config current-context 2>/dev/null) || CONTEXT_NAME=""
  CONTEXT_NAME=${CONTEXT_NAME:-none}
fi

if ! version_json=$(kro get --raw /version 2>"$TMP/err"); then
  record cluster.reachable FAIL "cannot reach the API server of context $CONTEXT_NAME: $(first_line "$(cat "$TMP/err")")"
  finish
fi
record cluster.reachable PASS "context $CONTEXT_NAME"

git_version=$(printf '%s' "$version_json" | tr -d ' \n' | sed -n 's/.*"gitVersion":"\([^"]*\)".*/\1/p')
major=$(printf '%s' "$git_version" | sed -n 's/^v\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1/p')
minor=$(printf '%s' "$git_version" | sed -n 's/^v\([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\2/p')
if [ -z "$major" ] || [ -z "$minor" ]; then
  record k8s.version WARN "could not parse the server version '$git_version'"
elif [ "$major" -gt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -ge "$MIN_K8S_MINOR" ]; }; then
  record k8s.version PASS "Kubernetes $git_version (>= 1.$MIN_K8S_MINOR)"
else
  record k8s.version FAIL "Kubernetes $git_version: the chart needs >= 1.$MIN_K8S_MINOR (az aks upgrade)"
fi

# ── Nodes and pools ─────────────────────────────────────────────────────

# One line per node: name, pool, Ready, allocatable pods, allocatable GPUs,
# OS, AKS cluster label, taints (key=value:Effect,...).
nodes_tpl="{range .items[*]}{.metadata.name}{\"\\t\"}{.metadata.labels.$(jp_key "$POOL_LABEL")}{\"\\t\"}{range .status.conditions[?(@.type==\"Ready\")]}{.status}{end}{\"\\t\"}{.status.allocatable.pods}{\"\\t\"}{.status.allocatable.nvidia\\.com/gpu}{\"\\t\"}{.metadata.labels.kubernetes\\.io/os}{\"\\t\"}{.metadata.labels.kubernetes\\.azure\\.com/cluster}{\"\\t\"}{range .spec.taints[*]}{.key}={.value}:{.effect},{end}{\"\\n\"}{end}"
if kro get nodes -o "jsonpath=$nodes_tpl" >"$TMP/nodes" 2>"$TMP/err"; then
  total=$(awk 'END { print NR }' "$TMP/nodes")
  ready=$(awk -F'\t' '$3 == "True"' "$TMP/nodes" | awk 'END { print NR }')
  if [ "$ready" -gt 0 ]; then
    record nodes.ready PASS "$ready of $total nodes Ready"
  else
    record nodes.ready FAIL "no Ready node ($total nodes)"
  fi
  aks_nodes=$(awk -F'\t' '$7 != ""' "$TMP/nodes" | awk 'END { print NR }')
  if [ "$aks_nodes" -gt 0 ]; then
    record cluster.aks PASS "AKS nodes (kubernetes.azure.com/cluster label on $aks_nodes)"
  else
    record cluster.aks WARN "no node carries kubernetes.azure.com/cluster: not an AKS cluster; the Azure-specific checks below will not apply"
  fi
else
  : >"$TMP/nodes"
  record nodes.ready FAIL "cannot list nodes: $(first_line "$(cat "$TMP/err")")"
fi

# Pods per node (anything not finished occupies a slot).
if ! kro get pods -A --field-selector=status.phase!=Succeeded,status.phase!=Failed \
     -o 'jsonpath={range .items[*]}{.spec.nodeName}{"\n"}{end}' >"$TMP/pods" 2>"$TMP/err"; then
  : >"$TMP/pods"
  PODS_LISTED=0
else
  PODS_LISTED=1
fi

# check_pool NAME ROLE   ROLE: system | worker | gpu
check_pool() {
  local name=$1 role=$2 id summary nodes_in ready_in nonlinux headroom critical gpus
  id="pool.$name"
  awk -F'\t' -v p="$name" '$2 == p' "$TMP/nodes" >"$TMP/pool"
  nodes_in=$(awk 'END { print NR }' "$TMP/pool")
  if [ "$nodes_in" -eq 0 ]; then
    if [ -n "$AZ_OK_POOLS" ] && ! printf '%s\n' "$AZ_OK_POOLS" | grep -qx "$name"; then
      record "$id" FAIL "no node pool '$name' in cluster $AZ_CLUSTER (az aks nodepool list)"
    else
      record "$id" WARN "no node carries $POOL_LABEL=$name: the pool is scaled to zero or the name is wrong"
    fi
    [ "$role" = gpu ] && record "gpu.$name" SKIP "no nodes in pool $name to inspect"
    return
  fi
  ready_in=$(awk -F'\t' '$3 == "True"' "$TMP/pool" | awk 'END { print NR }')
  nonlinux=$(awk -F'\t' '$6 != "linux"' "$TMP/pool" | awk 'END { print NR }')
  critical=$(awk -F'\t' '$8 ~ /(^|,)CriticalAddonsOnly/' "$TMP/pool" | awk 'END { print NR }')
  summary="$ready_in/$nodes_in nodes Ready"
  if [ "$PODS_LISTED" = 1 ]; then
    headroom=$(awk -F'\t' 'NR == FNR { used[$1]++; next } { free += $4 - used[$1] } END { print free + 0 }' "$TMP/pods" "$TMP/pool")
    summary="$summary, $headroom free pod slots"
  else
    headroom=""
    summary="$summary, pod headroom unknown (cannot list pods)"
  fi
  taints=$(awk -F'\t' '{ print $8 }' "$TMP/pool" | tr ',' '\n' | sed '/^$/d' | sort -u | paste -sd ' ' -)
  [ -n "$taints" ] && summary="$summary, taints: $taints"

  if [ "$nonlinux" -gt 0 ]; then
    record "$id" FAIL "$summary; $nonlinux nodes are not Linux (Nexus images are linux/amd64 and linux/arm64)"
  elif [ "$ready_in" -eq 0 ]; then
    record "$id" FAIL "$summary"
  elif [ "$role" != system ] && [ "$critical" -gt 0 ]; then
    record "$id" FAIL "$summary; tainted CriticalAddonsOnly (a dedicated system pool): agents run task code and belong on a user pool"
  elif [ -n "$headroom" ] && [ "$headroom" -lt "$MIN_HEADROOM" ]; then
    record "$id" WARN "$summary (< $MIN_HEADROOM wanted: raise max-pods or add nodes)"
  else
    record "$id" PASS "$summary"
  fi

  if [ "$role" = gpu ]; then
    gpus=$(awk -F'\t' '{ s += ($5 == "" ? 0 : $5) } END { print s + 0 }' "$TMP/pool")
    if [ "$gpus" -gt 0 ]; then
      record "gpu.$name" PASS "$gpus nvidia.com/gpu allocatable in pool $name"
    else
      record "gpu.$name" FAIL "pool $name advertises no nvidia.com/gpu: install the NVIDIA device plugin on it (environments/aks-existing install_nvidia_device_plugin) or check the VM size${DEVICE_PLUGIN_NOTE}"
    fi
  fi
}

# Azure's view, when asked for and logged in (read-only: show/list).
AZ_OK_POOLS=""
if [ -n "$AZ_RG" ]; then
  if ! command -v az >/dev/null 2>&1; then
    record az.cluster SKIP "az is not on PATH"
  elif ! az account show -o none >/dev/null 2>&1; then
    record az.cluster SKIP "not logged in to Azure (az login)"
  elif ! az_out=$(az aks show -g "$AZ_RG" -n "$AZ_CLUSTER" \
          --query '[oidcIssuerProfile.enabled, securityProfile.workloadIdentity.enabled, currentKubernetesVersion, oidcIssuerProfile.issuerUrl]' \
          -o tsv 2>"$TMP/err"); then
    record az.cluster FAIL "az aks show $AZ_RG/$AZ_CLUSTER: $(first_line "$(cat "$TMP/err")")"
  else
    az_out=$(printf '%s' "$az_out" | tr '\n' '\t')
    az_oidc=$(printf '%s' "$az_out" | cut -f1)
    az_wi=$(printf '%s' "$az_out" | cut -f2)
    az_ver=$(printf '%s' "$az_out" | cut -f3)
    if [ "$az_oidc" = true ] && [ "$az_wi" = true ]; then
      record az.cluster PASS "OIDC issuer and workload identity enabled (Kubernetes $az_ver)"
    else
      record az.cluster FAIL "oidcIssuerProfile.enabled=$az_oidc, workloadIdentity.enabled=$az_wi: az aks update -g $AZ_RG -n $AZ_CLUSTER --enable-oidc-issuer --enable-workload-identity"
    fi
    AZ_OK_POOLS=$(az aks nodepool list -g "$AZ_RG" --cluster-name "$AZ_CLUSTER" --query '[].name' -o tsv 2>/dev/null || true)
  fi
fi

# Is an NVIDIA device plugin (or the GPU Operator) installed at all?
DEVICE_PLUGIN_NOTE=""
if [ -n "$GPU_POOLS" ]; then
  if kro get daemonsets -A -o 'jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' >"$TMP/ds" 2>/dev/null; then
    dp=$(grep -Ei 'nvidia-device-plugin|gpu-operator|nvidia-driver' "$TMP/ds" | paste -sd ' ' -)
    if [ -n "$dp" ]; then
      DEVICE_PLUGIN_NOTE=" (DaemonSets present: $dp; check their node affinity)"
    else
      DEVICE_PLUGIN_NOTE=" (no NVIDIA device plugin DaemonSet found in any namespace)"
    fi
  fi
fi

[ -n "$SYSTEM_POOL" ] && check_pool "$SYSTEM_POOL" system
for p in $POOLS; do check_pool "$p" worker; done
for p in $GPU_POOLS; do check_pool "$p" gpu; done
if [ -z "$SYSTEM_POOL$POOLS$GPU_POOLS" ]; then
  record pools SKIP "no --system-pool/--pool/--gpu-pool given"
fi

# ── Workload identity ───────────────────────────────────────────────────

if kro get mutatingwebhookconfigurations -o 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}' >"$TMP/mwc" 2>"$TMP/err"; then
  wh=$(grep -i 'azure-wi-webhook' "$TMP/mwc" | head -n 1)
  if [ -n "$wh" ]; then
    record wi.webhook PASS "workload identity webhook installed ($wh)"
  else
    record wi.webhook FAIL "no azure-wi-webhook mutating webhook: pods would get no federated token (az aks update --enable-workload-identity)"
  fi
else
  record wi.webhook WARN "cannot list mutatingwebhookconfigurations: $(first_line "$(cat "$TMP/err")")"
fi

if oidc=$(kro get --raw /.well-known/openid-configuration 2>"$TMP/err"); then
  issuer=$(printf '%s' "$oidc" | tr -d ' \n' | sed -n 's/.*"issuer":"\([^"]*\)".*/\1/p')
  case "$issuer" in
    https://*.oic.prod-aks.azure.com/* | https://*.oic.prod-aks.azure.us/* | https://*.oic.prod-aks.azure.cn/*)
      record wi.oidc-issuer PASS "service-account issuer $issuer" ;;
    https://kubernetes.default.svc* | "")
      record wi.oidc-issuer FAIL "service-account issuer '${issuer:-none}' is not a public OIDC issuer: Entra ID cannot verify the tokens (az aks update --enable-oidc-issuer)" ;;
    *)
      record wi.oidc-issuer WARN "service-account issuer $issuer is not an AKS issuer; Entra ID must be able to fetch its discovery document and keys" ;;
  esac
else
  record wi.oidc-issuer WARN "cannot read /.well-known/openid-configuration: $(first_line "$(cat "$TMP/err")")"
fi

# ── Cluster services the chart relies on ────────────────────────────────

if ms=$(kro get apiservice v1beta1.metrics.k8s.io -o 'jsonpath={range .status.conditions[?(@.type=="Available")]}{.status}{end}' 2>"$TMP/err"); then
  if [ "$ms" = True ]; then
    record metrics-server PASS "metrics.k8s.io available (HPAs can scale)"
  else
    record metrics-server FAIL "metrics.k8s.io registered but not Available ($ms): HPAs, including the ones the cluster controller applies, cannot scale"
  fi
else
  record metrics-server FAIL "no metrics.k8s.io API (metrics-server): HPAs cannot scale"
fi

if [ -n "$STORAGE_CLASS" ]; then
  if prov=$(kro get storageclass "$STORAGE_CLASS" -o 'jsonpath={.provisioner}' 2>/dev/null); then
    record storage-class PASS "$STORAGE_CLASS ($prov)"
  else
    record storage-class FAIL "StorageClass $STORAGE_CLASS does not exist: the NATS volume would stay Pending (--storage-class \"\" uses the cluster default)"
  fi
else
  def=$(kro get storageclass -o 'jsonpath={range .items[?(@.metadata.annotations.storageclass\.kubernetes\.io/is-default-class=="true")]}{.metadata.name}{" "}{end}' 2>/dev/null || true)
  def=${def% }
  if [ -n "$def" ]; then
    record storage-class PASS "cluster default StorageClass: $def"
  else
    record storage-class FAIL "no default StorageClass, and none named"
  fi
fi

gwc=$(kro get gatewayclasses -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null || true)
igc=$(kro get ingressclasses -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{end}' 2>/dev/null || true)
gwc=${gwc% }
igc=${igc% }
if [ -n "$gwc" ]; then
  record ingress PASS "GatewayClasses: $gwc (the chart's gateway.* routes need one)${igc:+; IngressClasses: $igc}"
elif [ -n "$igc" ]; then
  record ingress WARN "IngressClasses: $igc but no Gateway API GatewayClass: the chart's routes use the Gateway API; reach the console by port-forward or your own Ingress"
else
  record ingress WARN "no GatewayClass or IngressClass: reach the console by port-forward until one exists (informational)"
fi

if [ "$TLS" = 1 ]; then
  if kro get crd certificates.cert-manager.io -o name >/dev/null 2>&1; then
    record cert-manager PASS "cert-manager CRDs present (tls.enabled)"
  else
    record cert-manager FAIL "tls.enabled needs cert-manager (certificates.cert-manager.io CRD missing)"
  fi
fi

# ── The release namespace and its Secrets ───────────────────────────────

NS_EXISTS=0
if ns_labels=$(kro get namespace "$NAMESPACE" -o 'jsonpath={.metadata.labels.pod-security\.kubernetes\.io/enforce}' 2>"$TMP/err"); then
  NS_EXISTS=1
  case "$ns_labels" in
    restricted | baseline)
      record namespace PASS "$NAMESPACE exists, Pod Security enforce=$ns_labels" ;;
    privileged | "")
      record namespace WARN "$NAMESPACE exists with no Pod Security enforcement (enforce=${ns_labels:-unset}): label it pod-security.kubernetes.io/enforce=baseline (or restricted)" ;;
    *)
      record namespace WARN "$NAMESPACE: unexpected enforce label '$ns_labels'" ;;
  esac
elif grep -qi 'not found' "$TMP/err"; then
  record namespace WARN "namespace $NAMESPACE does not exist yet (Terraform create_namespace, or kubectl create namespace)"
else
  record namespace FAIL "cannot read namespace $NAMESPACE: $(first_line "$(cat "$TMP/err")")"
fi

# secret_keys NAME -> key names, one per line, in $TMP/keys; returns 1 if unreadable.
secret_keys() {
  # shellcheck disable=SC2016 # a Go template, not shell: $k is its variable
  kro get secret "$1" -n "$NAMESPACE" -o 'go-template={{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}' >"$TMP/keys" 2>"$TMP/err"
}

# secret_value NAME KEY -> decoded value on stdout (never printed by this script).
secret_value() {
  kro get secret "$1" -n "$NAMESPACE" -o "jsonpath={.data.$(jp_key "$2")}" 2>/dev/null | base64 --decode 2>/dev/null
}

has_key() { grep -qx "$1" "$TMP/keys"; }

if [ -n "$SECRET" ]; then
  if [ "$NS_EXISTS" = 0 ]; then
    record secret SKIP "namespace $NAMESPACE does not exist"
  elif ! secret_keys "$SECRET"; then
    record secret FAIL "Secret $NAMESPACE/$SECRET: $(first_line "$(cat "$TMP/err")")"
  else
    missing=""
    for k in postgres-password worker-api-key; do has_key "$k" || missing="$missing $k"; done
    if [ "$NATS" = 1 ]; then
      for k in nats-event-service-nkey nats-agent-nkey; do has_key "$k" || missing="$missing $k"; done
    fi
    if [ -n "$missing" ]; then
      record secret FAIL "Secret $SECRET lacks:$missing"
    else
      notes=""
      has_key admin-api-key || notes="; no admin-api-key (bootstrap installs no admin key: sign in another way)"
      pw=$(secret_value "$SECRET" postgres-password)
      wk=$(secret_value "$SECRET" worker-api-key)
      case "$pw" in
        *[!A-Za-z0-9._~-]*)
          record secret FAIL "postgres-password holds characters a postgres:// URL would need escaped: use letters, digits and . _ ~ - only" ;;
        *)
          if [ "${#pw}" -lt 16 ]; then
            record secret WARN "postgres-password is shorter than 16 characters$notes"
          else
            case "$wk" in
              nxs_worker_*) record secret PASS "Secret $SECRET has every required key$notes" ;;
              *) record secret WARN "worker-api-key is not an nxs_worker_ key$notes" ;;
            esac
          fi ;;
      esac
      pw="" wk=""
    fi
  fi
else
  record secret SKIP "pass --secret <existingSecret> to check its keys"
fi

if [ -n "$DB_LOGINS_SECRET" ]; then
  if [ "$NS_EXISTS" = 0 ]; then
    record db-logins SKIP "namespace $NAMESPACE does not exist"
  elif ! secret_keys "$DB_LOGINS_SECRET"; then
    record db-logins FAIL "Secret $NAMESPACE/$DB_LOGINS_SECRET: $(first_line "$(cat "$TMP/err")")"
  else
    missing=""
    for c in api-gateway workflow-manager dag-engine scheduler resource-manager event-service artifact-gc; do
      has_key "postgres-password-$c" || missing="$missing postgres-password-$c"
    done
    if [ -n "$missing" ]; then
      record db-logins FAIL "Secret $DB_LOGINS_SECRET lacks:$missing (artifact-gc only when artifactGC.enabled)"
    else
      record db-logins PASS "Secret $DB_LOGINS_SECRET has a password for every runtime login"
    fi
  fi
fi

if [ -n "$CA_SECRET" ]; then
  if [ "$NS_EXISTS" = 0 ]; then
    record db-ca SKIP "namespace $NAMESPACE does not exist"
  elif ! secret_keys "$CA_SECRET"; then
    record db-ca FAIL "Secret $NAMESPACE/$CA_SECRET: $(first_line "$(cat "$TMP/err")")"
  elif ! has_key ca.crt; then
    record db-ca FAIL "Secret $CA_SECRET has no ca.crt"
  elif secret_value "$CA_SECRET" ca.crt | grep -q -- '-----BEGIN CERTIFICATE-----'; then
    n=$(secret_value "$CA_SECRET" ca.crt | grep -c -- '-----BEGIN CERTIFICATE-----')
    record db-ca PASS "Secret $CA_SECRET: $n PEM certificates (deploy/terraform/ca-bundles/azure-postgres-roots.pem holds the Azure roots)"
  else
    record db-ca FAIL "Secret $CA_SECRET ca.crt is not PEM"
  fi
fi

# ── Opt-in: a pod that pulls an image / reaches the database ────────────

# pod_spec NAME IMAGE CMD-JSON RUN-AS-UID -> a restricted-profile Pod manifest.
pod_spec() {
  local selector="" pullsecret=""
  [ -n "$TEST_POOL" ] && selector="  nodeSelector: { \"$POOL_LABEL\": \"$TEST_POOL\" }"
  [ -n "$PULL_SECRET" ] && pullsecret="  imagePullSecrets: [{ name: \"$PULL_SECRET\" }]"
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $NAMESPACE
  labels:
    app.kubernetes.io/name: nexus-preflight
    app.kubernetes.io/part-of: kgf-nexus
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  activeDeadlineSeconds: $TEST_TIMEOUT
$selector
$pullsecret
  securityContext:
    runAsNonRoot: true
    runAsUser: $4
    runAsGroup: $4
    seccompProfile: { type: RuntimeDefault }
  containers:
    - name: test
      image: "$2"
      imagePullPolicy: Always
      command: $3
      resources:
        requests: { cpu: 10m, memory: 16Mi }
        limits: { memory: 64Mi }
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: { drop: [ALL] }
EOF
}

rand_suffix() { printf '%s' "$(date +%s)$$" | tail -c 8; }

# wait_pod NAME -> echo "pulled|pullfail:<reason>|exit:<code>|timeout"
wait_pod() {
  local name=$1 deadline st reason code image_id
  deadline=$(($(date +%s) + TEST_TIMEOUT))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    st=$(kro get pod "$name" -n "$NAMESPACE" -o 'jsonpath={.status.containerStatuses[0].state.waiting.reason}{"\t"}{.status.containerStatuses[0].state.terminated.exitCode}{"\t"}{.status.containerStatuses[0].imageID}' 2>/dev/null || true)
    reason=$(printf '%s' "$st" | cut -f1)
    code=$(printf '%s' "$st" | cut -f2)
    image_id=$(printf '%s' "$st" | cut -f3)
    case "$reason" in
      ErrImagePull | ImagePullBackOff | InvalidImageName | ErrImageNeverPull | RegistryUnavailable)
        echo "pullfail:$reason"; return ;;
    esac
    if [ -n "$code" ]; then echo "exit:$code${image_id:+:pulled}"; return; fi
    if [ -n "$image_id" ]; then echo "pulled"; return; fi
    sleep 3
  done
  echo timeout
}

run_test_pod() { # ID NAME IMAGE CMD UID
  if [ "$NS_EXISTS" = 0 ]; then
    record "$1" SKIP "namespace $NAMESPACE does not exist; the test pod needs it"
    return 1
  fi
  if ! pod_spec "$2" "$3" "$4" "$5" | kmut create -f - >/dev/null 2>"$TMP/err"; then
    record "$1" FAIL "could not create the test pod: $(first_line "$(cat "$TMP/err")")"
    return 1
  fi
  CREATED_PODS="$CREATED_PODS $2"
  return 0
}

if [ -n "$PULL_IMAGE" ]; then
  name="nexus-preflight-pull-$(rand_suffix)"
  # The command does not exist in any image: the kubelet pulls the image and
  # the container fails to start, so none of the image's code ever runs.
  if run_test_pod image-pull "$name" "$PULL_IMAGE" '["/nexus-preflight-does-not-exist"]' 65532; then
    res=$(wait_pod "$name")
    case "$res" in
      pulled | exit:*:pulled | exit:*) record image-pull PASS "$PULL_IMAGE pulled on a node (the container was never started)" ;;
      pullfail:*) record image-pull FAIL "$PULL_IMAGE: ${res#pullfail:} (private GHCR package? --pull-secret and image.pullSecrets; kubectl -n $NAMESPACE describe pod $name before this script exits)" ;;
      *) record image-pull FAIL "$PULL_IMAGE: no pull within ${TEST_TIMEOUT}s" ;;
    esac
  fi
fi

if [ -n "$DB_TEST" ]; then
  db_host=${DB_TEST%:*}
  db_port=${DB_TEST##*:}
  [ "$db_host" = "$DB_TEST" ] && db_port=5432
  is_uint "$db_port" || die_usage "--db-test port must be a number"
  name="nexus-preflight-db-$(rand_suffix)"
  # 70 is the postgres user of the alpine images: pg_isready needs no root.
  if run_test_pod db-reachable "$name" "$DB_TEST_IMAGE" "[\"pg_isready\", \"-h\", \"$db_host\", \"-p\", \"$db_port\", \"-t\", \"10\"]" 70; then
    res=$(wait_pod "$name")
    case "$res" in
      exit:0*) record db-reachable PASS "$db_host:$db_port accepts connections from the cluster" ;;
      exit:1*) record db-reachable WARN "$db_host:$db_port answers but rejects connections (starting up, or out of connections)" ;;
      exit:2*) record db-reachable FAIL "$db_host:$db_port: no response from inside the cluster (firewall rule, VNet integration, private DNS zone link)" ;;
      exit:*) record db-reachable FAIL "pg_isready could not attempt a connection to $db_host:$db_port (${res#exit:})" ;;
      pullfail:*) record db-reachable FAIL "test image $DB_TEST_IMAGE: ${res#pullfail:} (--db-test-image)" ;;
      *) record db-reachable FAIL "no answer within ${TEST_TIMEOUT}s" ;;
    esac
  fi
fi

finish
