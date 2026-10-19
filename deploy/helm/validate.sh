#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Static validation of the kgf-nexus chart. No cluster needed.
#
#   deploy/helm/validate.sh                       # the check (make helm-validate, CI)
#   deploy/helm/validate.sh --update-schema-lock  # re-pin schemas.lock hashes
#
# 1. helm lint --strict with every values file in deploy/helm/kgf-nexus/ci/,
#    then helm template with the same values, whose exit status is the
#    assertion and whose error is printed. Helm 4 logs a template `fail` hit
#    during lint at INFO (level=INFO msg="funcMap fail") and still exits 0, so
#    lint alone passed charts whose own guards refused the values under test;
#    and lint ignores `key: null` overrides, so its fail lines can be false.
#    helm template coalesces values exactly as install does
#    (docs/OPERATIONS.md §20).
# 2. The rendered manifests go through kubeconform -strict against the
#    Kubernetes schemas for KUBE_VERSION (the chart's floor by default) and the
#    CRD schemas (cert-manager, prometheus-operator, Gateway API), all read
#    from a local cache filled from COMMIT-PINNED sources and checked against
#    the SHA-256 of every file in schemas.lock. No schema is read from a
#    floating branch, and a warm cache needs no network at all.
# 3. Every pod template of every render passes Pod Security "restricted"
#    (pss-restricted.py), so a namespace enforcing it admits the whole release.
# 4. The shipped examples (kgf-nexus/examples/*.yaml) are exactly their ci/
#    copies, <owner> aside, so step 1 validates what users copy.
# 5. Negative cases: values that MUST be refused -- by values.schema.json or by
#    a template guard -- and the message that must name the fix. Then positive
#    cases: what given values must render (worker-pool releases, runtime
#    identity, the agents' control-plane settings, placement).
#
# Tools: helm and kubeconform are the versions pinned in
# deploy/scripts/pinned-tool.sh, downloaded once into .tools/ and verified by
# SHA-256. HELM=<path> or KUBECONFORM=<path> use another binary instead; its
# version is printed, and results from a different version are not what CI sees.
#
# Environment:
#   KUBE_VERSION              default 1.29.0; needs schemas in schemas.lock
#   KUBECONFORM_SCHEMA_CACHE  default ~/.cache/kgf-nexus/kubeconform-schemas
#   SCHEMA_OFFLINE=1          never download: a schema missing from the cache
#                             is an error (air-gapped runs with a primed cache)
#   NEXUS_TOOLS_DIR           where pinned tools live (default <repo>/.tools)
#
# Network: needed only to fill a cold tool or schema cache (github.com,
# get.helm.sh, raw.githubusercontent.com). CI caches both directories.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
chart="$here/kgf-nexus"
lock="$here/schemas.lock"
kube_version=${KUBE_VERSION:-1.29.0}
cache=${KUBECONFORM_SCHEMA_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/kgf-nexus/kubeconform-schemas}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# ── schemas ─────────────────────────────────────────────────────────────────
lock_commit() { awk -v w="$1" '$1=="commit" && $2==w {print $3}' "$lock"; }
k8s_commit=$(lock_commit k8s)
crds_commit=$(lock_commit crds)
if [ -z "$k8s_commit" ] || [ -z "$crds_commit" ]; then
  echo "validate: $lock lacks its commit lines" >&2; exit 1
fi
schema_url() {
  case "$1" in
    k8s/*)  echo "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/$k8s_commit/${1#k8s/}" ;;
    crds/*) echo "https://raw.githubusercontent.com/datreeio/CRDs-catalog/$crds_commit/${1#crds/}" ;;
    *) echo "validate: bad schema path $1 in $lock" >&2; return 1 ;;
  esac
}
fetch_schema() { # <lock path> <destination file>
  command -v curl >/dev/null || { echo "validate: curl is needed to fetch schemas (or prime $cache)" >&2; return 1; }
  mkdir -p "$(dirname "$2")"
  curl -fsSL --retry 3 --retry-delay 2 --proto '=https' -o "$2.part" "$(schema_url "$1")" && mv "$2.part" "$2"
}

if [ "${1:-}" = "--update-schema-lock" ]; then
  out="$tmp/lock"
  while IFS= read -r line; do
    case "$line" in
      '#'* | '' | 'commit '*) printf '%s\n' "$line" ;;
      *)
        path=${line#* }
        fetch_schema "$path" "$tmp/s/$path" || { echo "validate: cannot fetch $path" >&2; exit 1; }
        printf '%s %s\n' "$(sha256_of "$tmp/s/$path")" "$path" ;;
    esac
  done <"$lock" >"$out"
  diff -u "$lock" "$out" || true
  cp "$out" "$lock"
  echo "validate: $lock updated; review the diff above before committing"
  exit 0
fi
[ $# -eq 0 ] || { echo "usage: $0 [--update-schema-lock]" >&2; exit 2; }

# Every locked file into the cache, verified. A cached file is re-hashed on
# every run: cheap, and a corrupted or edited cache fails here, loudly.
missing=0
while read -r want path; do
  case "$want" in '#'* | '' | commit) continue ;; esac
  f="$cache/$path"
  if [ -f "$f" ] && [ "$(sha256_of "$f")" = "$want" ]; then continue; fi
  if [ "${SCHEMA_OFFLINE:-0}" = 1 ]; then
    echo "validate: schema $path is missing from $cache or differs from schemas.lock, and SCHEMA_OFFLINE=1" >&2
    missing=1; continue
  fi
  fetch_schema "$path" "$f" || { echo "validate: cannot fetch $path from $(schema_url "$path")" >&2; exit 1; }
  got=$(sha256_of "$f")
  if [ "$got" != "$want" ]; then
    rm -f "$f"
    echo "validate: CHECKSUM MISMATCH for schema $path: expected $want, got $got" >&2
    exit 1
  fi
done <"$lock"
if [ "$missing" != 0 ]; then
  echo "validate: prime the cache by running once with network access" >&2; exit 1
fi
if ! grep -q " k8s/v${kube_version}-standalone-strict/" "$lock"; then
  echo "validate: schemas.lock has no schemas for KUBE_VERSION=$kube_version; add them (see the lock's header)" >&2
  exit 1
fi
# Local paths only: kubeconform never reaches the network from here. A kind
# with no locked schema fails as "could not find schema" -- add it to the lock.
schema_args=(
  -schema-location "$cache/k8s/{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"
  -schema-location "$cache/crds/{{ .Group }}/{{ .ResourceKind }}_{{ .ResourceAPIVersion }}.json"
)

# ── tools ───────────────────────────────────────────────────────────────────
helm=${HELM:-$("$repo/deploy/scripts/pinned-tool.sh" helm)}
kubeconform=${KUBECONFORM:-$("$repo/deploy/scripts/pinned-tool.sh" kubeconform)}
echo "validate: helm $("$helm" version --short 2>/dev/null), kubeconform $("$kubeconform" -v)," \
  "schemas k8s@${k8s_commit:0:12} crds@${crds_commit:0:12}"

# lint_and_render <name> <out.yaml> <helm args...>: lint --strict, fail on any
# template `fail` the lint swallowed, then render and assert the render.
lint_and_render() {
  local name=$1 out=$2
  shift 2
  if ! "$helm" lint --strict "$chart" --kube-version "$kube_version" "$@" >"$tmp/lint" 2>&1; then
    echo "FAIL lint ($name):"; sed 's/^/    /' "$tmp/lint"; return 1
  fi
  # The render is the assertion: `helm template` runs the same value
  # coalescing and the same guards as `helm install`, and exits non-zero on a
  # `fail`. Release name "on": YAML 1.1 reads it as a boolean, so every
  # unquoted use of the release name in a label fails validation (it did,
  # before the fix).
  if ! "$helm" template on "$chart" -n nexus-validate --kube-version "$kube_version" "$@" >"$out" 2>"$tmp/render"; then
    echo "FAIL render ($name): a template guard or error refused these values:"
    sed 's/^/    /' "$tmp/render"; return 1
  fi
  # Helm 4 logs a `fail` hit during lint at INFO and exits 0, so the lint exit
  # code can never be the gate. Lint also does not apply `key: null` overrides
  # from -f files the way install and template do (it merges both bounds of a
  # PDB, say), so its `fail` lines can be false. Since the render above passed,
  # show them as a note: a real refusal has already failed the render.
  if grep -qE 'funcMap fail|error calling fail' "$tmp/lint"; then
    echo "note ($name): helm lint reported template fail(s) that helm template does not (lint ignores null overrides):"
    grep -E 'funcMap fail|error calling fail' "$tmp/lint" | sed 's/^/    /' | sort -u | head -3
  fi
}

fail=0
for values in "$chart"/ci/*-values.yaml; do
  name=$(basename "$values" -values.yaml)
  echo "== $name"
  lint_and_render "$name" "$tmp/$name.yaml" -f "$values" || { fail=1; continue; }
  if ! "$kubeconform" -strict -summary -kubernetes-version "$kube_version" "${schema_args[@]}" "$tmp/$name.yaml"; then
    fail=1
  fi
  python3 -I "$here/pss-restricted.py" "$tmp/$name.yaml" || fail=1
done

# The examples users copy are validated through their ci/ copies: identical
# but for the <owner> placeholder (an image path a render cannot leave open).
echo "== examples match their ci/ copies"
for example in "$chart"/examples/*.yaml; do
  copy="$chart/ci/$(basename "$example" .yaml)-values.yaml"
  if [ ! -f "$copy" ]; then
    echo "FAIL: $example has no validated copy $copy"; fail=1
  elif ! sed 's/<owner>/example-owner/g' "$example" | diff -u - "$copy" >"$tmp/example.diff"; then
    echo "FAIL: $copy differs from $example (regenerate: sed 's/<owner>/example-owner/g'):"
    sed 's/^/    /' "$tmp/example.diff" | head -20; fail=1
  else
    echo "ok   $(basename "$example") = ci/$(basename "$copy")"
  fi
done

# The wrapper itself: values a template guard refuses must fail it, whatever
# Helm's lint exit code says. Without the grep above this passes on Helm 4.
echo "== the gate catches a template fail that helm lint swallows"
if lint_and_render guard-probe "$tmp/probe.yaml" -f "$chart/ci/minimal-values.yaml" \
    --set artifactGC.mode=delete >"$tmp/probe.out"; then
  echo "FAIL: values refused by a template guard passed lint_and_render"; fail=1
elif ! grep -q 'confirmDelete=true' "$tmp/probe.out"; then
  echo "FAIL: guard probe refused for the wrong reason:"; sed 's/^/    /' "$tmp/probe.out"; fail=1
else
  echo "ok   refused at render: artifactGC.mode=delete without confirmDelete (helm lint alone exits 0)"
fi

# must_fail <description> <expected message fragment> <helm --set args...>
must_fail() {
  local what=$1 expect=$2; shift 2
  if "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" "$@" >"$tmp/neg" 2>&1; then
    echo "FAIL (rendered, should be refused): $what"; fail=1
  elif ! grep -qF -- "$expect" "$tmp/neg"; then
    echo "FAIL (refused, wrong reason): $what"; sed 's/^/    /' "$tmp/neg" | tail -3; fail=1
  else
    echo "ok   refused: $what"
  fi
}

echo "== refused values"
must_fail "misspelt top-level section"          "additional properties 'clustercontroller' not allowed" --set clustercontroller.enabled=true
must_fail "floating latest tag"                 "/image/tag"                   --set image.tag=latest
must_fail "missing existingSecret"              "existingSecret is required"   --set existingSecret=
must_fail "GC delete without confirmation"      "confirmDelete=true"           --set artifactGC.mode=delete
must_fail "GC unknown mode"                     "/artifactGC/mode"             --set artifactGC.mode=purge
must_fail "controller without a cluster name"   "clusterController.clusterName is required" --set clusterController.enabled=true
must_fail "apply mode without the cluster UID"  "expectedClusterUID"           --set clusterController.enabled=true --set clusterController.clusterName=c --set clusterController.apply.enabled=true
must_fail "PDB that blocks every drain"         "would block every node drain" --set podDisruptionBudget.maxUnavailable=null --set podDisruptionBudget.minAvailable=1
must_fail "PDB with both bounds"                "not both"                     --set podDisruptionBudget.minAvailable=1 --set services.apiGateway.replicas=3
must_fail "HPA min above max"                   "above maxReplicas"            --set autoscaling.enabled=true --set autoscaling.minReplicas=5 --set autoscaling.maxReplicas=3
must_fail "HPA on resource-manager"             "not autoscalable"             --set services.resourceManager.autoscaling.enabled=true
must_fail "bad duration"                        "/tls/reloadInterval"          --set tls.reloadInterval=ten
must_fail "malformed image digest"              "/image/digests/migrate"       --set image.digests.migrate=sha256:abc
must_fail "bad topology policy"                 "whenUnsatisfiable"            --set placement.topologySpread.whenUnsatisfiable=Sometimes
must_fail "no scheduler at all"                 "schedulers"                   --set-json 'schedulers=[]'
must_fail "peer TLS with a plaintext broker"    "enable nats.tls too"          --set tls.enabled=true --set nats.auth.mode=none
must_fail "unparsable minimum agent version"   "/agentPolicy/minVersion"      --set agentPolicy.minVersion=latest

# The kubernetes executor (ADR-0029 §2, F148): a MIG prefix that no device
# plugin could ever advertise is refused; a pool agent renders the device-plugin
# names, the workload-API listener and its task-namespace RBAC, and its OWN pod
# requests no GPU (the task pods do).
k8s_agent='agents=[{"name":"agent-k8s","queues":"gpu","executors":"kubernetes","cpuMillis":1000,"memoryBytes":1073741824,"diskBytes":1073741824,"gpuCount":2,"gpuMig":"1g.10gb=7","kubernetes":{"namespace":"nexus-tasks","sharedGpuResourceName":"nvidia.com/gpu.shared"}}]'
must_fail "MIG prefix without a separator"      "/migResourcePrefix"           --set agentDefaults.kubernetes.migResourcePrefix=nvidia.com/mig
must_fail "shared GPU resource not qualified"   "/sharedGpuResourceName"       --set agentDefaults.kubernetes.sharedGpuResourceName=gpu-shared
if "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" --set-json "$k8s_agent" >"$tmp/k8s" 2>&1 &&
   grep -qF 'name: NEXUS_K8S_NAMESPACE, value: "nexus-tasks"' "$tmp/k8s" &&
   grep -qF 'name: NEXUS_K8S_MIG_RESOURCE_PREFIX, value: "nvidia.com/mig-"' "$tmp/k8s" &&
   grep -qF 'name: NEXUS_K8S_GPU_SHARED_RESOURCE, value: "nvidia.com/gpu.shared"' "$tmp/k8s" &&
   grep -qF 'name: NEXUS_K8S_TASK_API_ADVERTISE, value: "$(NEXUS_POD_IP):9443"' "$tmp/k8s" &&
   grep -qF 'name: agent-k8s-task-api' "$tmp/k8s" &&
   grep -qF 'name: agent-k8s-executor' "$tmp/k8s" &&
   ! grep -qE '^\s+nvidia.com/gpu: "?2' "$tmp/k8s"; then
  echo "ok   rendered: kubernetes pool agent with device-plugin names, workload API and no GPU of its own"
else
  echo "FAIL: the kubernetes pool agent did not render as expected"; tail -5 "$tmp/k8s"; fail=1
fi

# Per-installation group roles and PUBLIC CONNECT (F153): the prefix reaches
# the migrate Job and db-roles, and the logins join the prefixed roles.
must_fail "group role prefix is the shared name" "/postgres/groupRolePrefix"   --set postgres.groupRolePrefix=nexus
must_fail "group role prefix malformed"          "/postgres/groupRolePrefix"   --set postgres.groupRolePrefix=Acme-1
if "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" --set postgres.groupRolePrefix=acme \
    --set postgres.runtimeRoles.enabled=true --set postgres.runtimeRoles.revokePublicConnect=true >"$tmp/prefix" 2>&1 &&
   [ "$(grep -cF 'name: NEXUS_DB_ROLE_PREFIX, value: "acme"' "$tmp/prefix")" = 2 ] &&
   grep -qF 'name: NEXUS_DB_REVOKE_PUBLIC_CONNECT, value: "true"' "$tmp/prefix" &&
   grep -qF 'acme_scheduler' "$tmp/prefix" && ! grep -qF '\"nexus_scheduler\"' "$tmp/prefix"; then
  echo "ok   rendered: group role prefix acme on migrate and db-roles, PUBLIC CONNECT revoked"
else
  echo "FAIL: the group role prefix did not render as expected"; tail -5 "$tmp/prefix"; fail=1
fi

# Per-service pool caps (F160): postgres.pool.perService.<connection key>
# overrides the shared cap for that service only; a key that names no
# connection is refused rather than silently ignored.
must_fail "per-service pool for an unknown service" "invalid propertyName 'dagengine'" --set postgres.pool.perService.dagengine.maxConns=8
if "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" --set postgres.pool.maxConns=3 \
    --set postgres.pool.perService.dagEngine.maxConns=8 >"$tmp/pool" 2>&1 &&
   grep -A12 -E 'image: .*/dag-engine:' "$tmp/pool" | grep -qF 'name: NEXUS_DB_MAX_CONNS, value: "8"' &&
   grep -A12 -E 'image: .*/scheduler:' "$tmp/pool" | grep -qF 'name: NEXUS_DB_MAX_CONNS, value: "3"' &&
   grep -A12 -E 'image: .*/api-gateway:' "$tmp/pool" | grep -qF 'name: NEXUS_DB_MAX_CONNS, value: "3"' &&
   [ "$(grep -cF 'name: NEXUS_DB_MAX_CONNS, value: "8"' "$tmp/pool")" -le 2 ]; then
  echo "ok   rendered: dag-engine pool 8 over a shared cap of 3, every other service keeps 3"
else
  echo "FAIL: postgres.pool.perService did not render as expected"; tail -5 "$tmp/pool"; fail=1
fi

# An agent serving another tenant (agents[].namespace) needs that tenant's NATS
# identity: refused without it, rendered with the tenant's key and worker key.
nkeys=(--set nats.auth.mode=nkey
  --set-json 'nats.auth.eventService={"publicKeys":["UEVENTSERVICEKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}'
  --set-json 'nats.auth.monitor={"publicKeys":["UMONITORKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}')
team_agent='agents=[{"name":"agent-team","namespace":"team-ml","workerApiKeySecretKey":"worker-api-key-team-ml","natsSeedKey":"nats-agent-nkey-team-ml","queues":"default","cpuMillis":1000,"memoryBytes":1073741824,"diskBytes":1073741824}]'
must_fail "agent namespace without a NATS tenant" 'no entry for namespace "team-ml"' "${nkeys[@]}" \
  --set-json 'nats.auth.tenants=[{"namespace":"default","publicKeys":["UAGENTKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}]' \
  --set-json "$team_agent"
if "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" "${nkeys[@]}" \
    --set-json 'nats.auth.tenants=[{"namespace":"team-ml","publicKeys":["UAGENTKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}]' \
    --set-json "$team_agent" >"$tmp/team" 2>&1 &&
   grep -qF 'name: NEXUS_NAMESPACE, value: "team-ml"' "$tmp/team" &&
   grep -qF 'key: worker-api-key-team-ml' "$tmp/team"; then
  echo "ok   rendered: agent in namespace team-ml with its own worker key"
else
  echo "FAIL: an agent's namespace and worker key override did not render"; tail -3 "$tmp/team"; fail=1
fi
# Database TLS and runtime logins (F115, OPERATIONS §20.11-20.12).
ext=(--set postgres.enabled=false --set postgres.host=db.example.com)
ca=(--set postgres.sslmode=verify-full --set postgres.caBundle.secretName=pg-ca)
must_fail "verify-full without a CA bundle"     "needs postgres.caBundle"      "${ext[@]}" --set postgres.sslmode=verify-full
must_fail "CA bundle but no verification"       "does not verify the server"   "${ext[@]}" --set postgres.sslmode=require --set postgres.caBundle.secretName=pg-ca
must_fail "CA from a Secret and a ConfigMap"    "not both"                     "${ext[@]}" "${ca[@]}" --set postgres.caBundle.configMapName=pg-ca
must_fail "verify-full on in-cluster postgres"  "serves no TLS"                --set postgres.sslmode=verify-full --set postgres.caBundle.secretName=pg-ca
must_fail "sslmode smuggled in params"          "has its own setting"          "${ext[@]}" "${ca[@]}" --set postgres.params.sslmode=disable
must_fail "user smuggled in a connection"       "has its own setting"          "${ext[@]}" "${ca[@]}" --set postgres.connections.scheduler.params.user=nexus
must_fail "runtime login is the owner"          "must be a separate role"      "${ext[@]}" "${ca[@]}" --set postgres.runtimeRoles.enabled=true --set postgres.connections.scheduler.user=nexus
must_fail "two services share a login"          "each service needs its own"   "${ext[@]}" "${ca[@]}" --set postgres.runtimeRoles.enabled=true --set postgres.connections.dagEngine.user=nexus_scheduler_login
must_fail "runtime login on owner password"     "OWNER's password"             "${ext[@]}" "${ca[@]}" --set postgres.runtimeRoles.enabled=true --set postgres.connections.eventService.passwordKey=postgres-password
must_fail "unknown runtime-role scope"          "/postgres/runtimeRoles/scope" --set postgres.runtimeRoles.scope=all
must_fail "login name not an identifier"       "/postgres/connections/scheduler/user" --set postgres.connections.scheduler.user=Bad-Name
# Agent GPUs, placement and identity.
gpu=(--set agents[0].name=agent-gpu --set agents[0].queues=gpu --set agents[0].cpuMillis=1000 --set agents[0].memoryBytes=1073741824 --set agents[0].diskBytes=1073741824 --set agents[0].gpuCount=2)
must_fail "GPU limit disagrees with gpuCount"   "disagrees with gpuCount"      "${gpu[@]}" --set 'agents[0].resources.limits.nvidia\.com/gpu=1'
must_fail "unlabelled simulated GPUs"           "simulated=true"               "${gpu[@]}" --set agents[0].gpu.requestDevices=false
must_fail "bad GPU resource name"               "/agents/0/gpu/resourceName"   "${gpu[@]}" --set agents[0].gpu.resourceName=gpu
must_fail "shared SA, conflicting identity"     "different annotations"        --set serviceAccounts.apiGateway.name=nexus-artifacts --set serviceAccounts.agent.name=nexus-artifacts --set-string 'serviceAccounts.agent.annotations.eks\.amazonaws\.com/role-arn=arn:aws:iam::1:role/x'
must_fail "SA taken from the cluster controller" "owns with its RBAC"          --set serviceAccounts.ui.name=nexus-cluster-controller
must_fail "misspelt ServiceAccount key"         "additional properties 'annotation' not allowed" --set serviceAccounts.agent.annotation.x=y
must_fail "unknown result-store backend"        "/resultStore/backend"         --set resultStore.backend=swift
must_fail "unknown S3 credential mode"          "/resultStore/s3/credentials"  --set resultStore.s3.credentials=imds
must_fail "part size below S3's 5 MiB minimum"  "/resultStore/partSizeBytes"   --set resultStore.partSizeBytes=1048576
must_fail "in-cluster MinIO with Azure Blob"    "set minio.enabled=false"      --set resultStore.backend=azblob --set resultStore.azure.account=acct123
must_fail "in-cluster MinIO with IRSA"          "set minio.enabled=false or credentials=static" --set resultStore.s3.credentials=web-identity
must_fail "azblob without a storage account"    "needs resultStore.azure.account" --set minio.enabled=false --set resultStore.backend=azblob
must_fail "azblob shared key without a Secret"  "needs resultStore.azure.keySecret.name" --set minio.enabled=false --set resultStore.backend=azblob --set resultStore.azure.account=acct123 --set resultStore.azure.auth=shared-key
must_fail "workload identity, SA not annotated" "azure.workload.identity/client-id" --set minio.enabled=false --set resultStore.backend=azblob --set resultStore.azure.account=acct123 --set resultStore.azure.auth=workload-identity
must_fail "IRSA, SA not annotated"              "eks.amazonaws.com/role-arn"   --set minio.enabled=false --set resultStore.s3.credentials=web-identity
must_fail "unauthenticated GCS against real GCS" "only for an emulator"        --set minio.enabled=false --set resultStore.backend=gcs --set resultStore.gcs.auth=none
must_fail "tracing without a collector"         "needs tracing.endpoint"       --set tracing.enabled=true
must_fail "tracing with a bad protocol"         "/tracing/protocol"            --set tracing.protocol=http/json
must_fail "tracing headers Secret without key"  "needs tracing.headersSecret.key" --set tracing.enabled=true --set tracing.endpoint=http://c:4317 --set tracing.headersSecret.name=otel
# One identity model: the result-store clients' serviceAccounts, not a
# resultStore-level account (retired before release).
must_fail "retired resultStore.serviceAccountName" "additional properties 'serviceAccountName' not allowed" --set resultStore.serviceAccountName=kgf-nexus
must_fail "retired agent secrets SA"            "additional properties 'serviceAccountName' not allowed" --set agentDefaults.secrets.serviceAccountName=nexus-agent
# One KGF verifier: attribution keys live in the kgf block, not auditExport.
must_fail "retired auditExport.kgfContext"      "additional properties 'kgfContext' not allowed" --set auditExport.kgfContext.publicKeys=x
# Azure workload identity reaches exactly the three result-store clients: they
# run as the annotated account and carry the webhook's pod label; nothing else
# does.
if "$helm" template nexus "$chart" -f "$chart/ci/azblob-workload-identity-values.yaml" >"$tmp/wi" 2>&1 &&
   python3 - "$tmp/wi" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d.get("kind") in ("Deployment", "CronJob")]
store = {"api-gateway", "workflow-manager", "agent-cpu", "artifact-gc"}
seen = set()
for d in docs:
    name = d["metadata"]["name"].removeprefix("nexus-")
    tpl = d["spec"]["jobTemplate"]["spec"]["template"] if d["kind"] == "CronJob" else d["spec"]["template"]
    labelled = tpl["metadata"].get("labels", {}).get("azure.workload.identity/use") == "true"
    sa = tpl["spec"].get("serviceAccountName")
    if any(name.endswith(s) for s in store):
        seen.add(next(s for s in store if name.endswith(s)))
        assert labelled and sa == "nexus-artifacts", (name, labelled, sa)
    else:
        assert not labelled and sa != "nexus-artifacts", (name, labelled, sa)
assert seen == store, seen
PY
then
  echo "ok   rendered: azblob workload identity on exactly api-gateway, workflow-manager, agents and artifact-gc"
else
  echo "FAIL: azblob workload identity did not reach exactly the result-store clients"; tail -5 "$tmp/wi"; fail=1
fi

# ── Component-scoped releases, runtime identity, agent dialing (ADR-0035) ──
pool=(--set components.controlPlane.enabled=false)
must_fail "worker pool with no agents"          "agents is empty"              "${pool[@]}" --set-json 'agents=[]'
must_fail "worker pool without existingSecret"  "a worker-pool release (components.controlPlane.enabled=false) needs the Secret" "${pool[@]}" --set existingSecret=
must_fail "worker pool on a self-signed CA"     "needs tls.certManager.issuerRef" "${pool[@]}" --set tls.enabled=true --set tls.requirePeerTLS=false
must_fail "worker pool NATS mTLS, no client cert" "needs nats.tls.clientSecrets.agent" "${pool[@]}" --set nats.tls.enabled=true
must_fail "worker pool NATS TLS, no CA"         "needs nats.tls.caSecret"      "${pool[@]}" --set nats.tls.enabled=true --set nats.tls.verifyClients=false
must_fail "misspelt components key"            "additional properties 'controlplane' not allowed" --set components.controlplane.enabled=false
must_fail "control-plane TLS without a CA"      "needs agentDefaults.controlPlane.tls.caSecretName" --set agentDefaults.controlPlane.tls.enabled=true
must_fail "server name without any TLS"         "has no effect without TLS"    --set agentDefaults.controlPlane.tls.serverName=workers.example.com
must_fail "control-plane address without port"  "/agentDefaults/controlPlane/address" --set agentDefaults.controlPlane.address=api-gateway
must_fail "NATS URL that is not nats://"        "/agentDefaults/nats/url"      --set agentDefaults.nats.url=http://nats:4222
must_fail "cluster name the registry refuses"   "/cluster/name"                --set cluster.name=Prod_Cluster
must_fail "scheduler spread policy misspelt"    "whenUnsatisfiable"            --set-json 'schedulers=[{"name":"scheduler","queue":"default","topologySpread":{"whenUnsatisfiable":"Never"}}]'
must_fail "task toleration with a bad effect"   "/agentDefaults/kubernetes/taskTolerations/0/effect" --set-json 'agentDefaults.kubernetes.taskTolerations=[{"key":"sku","effect":"Never"}]'
must_fail "misspelt networkPolicy key"          "additional properties 'ingresFrom' not allowed" --set-json 'networkPolicy={"enabled":true,"ingresFrom":[]}'
must_fail "component topology the gateway refuses" "/services/apiGateway/componentTopology" --set services.apiGateway.componentTopology=everyone

# Executors are discovered by the agent, never read from a list: the chart
# renders the kubernetes executor's configuration for "kubernetes", refuses
# docker (no daemon socket in a chart pod), and renders neither
# NEXUS_EXECUTORS nor NEXUS_HEARTBEAT_INTERVAL_MS, which no binary reads.
one_agent='{"name":"agent-x","queues":"default","cpuMillis":1000,"memoryBytes":1073741824,"diskBytes":1073741824'
must_fail "docker executor in a chart pod"      "the chart mounts no daemon socket" --set-json "agents[0]=$one_agent,\"executors\":\"subprocess,docker\"}"
must_fail "an executor the agent does not have" "/agents/0/executors"          --set-json "agents[0]=$one_agent,\"executors\":\"podman\"}"
dead=0
for values in "$chart"/ci/*-values.yaml; do
  name=$(basename "$values" -values.yaml)
  if [ -f "$tmp/$name.yaml" ] && grep -qE 'NEXUS_EXECUTORS|NEXUS_HEARTBEAT_INTERVAL_MS' "$tmp/$name.yaml"; then
    echo "FAIL: ci/$name renders NEXUS_EXECUTORS or NEXUS_HEARTBEAT_INTERVAL_MS, which the agent does not read"; dead=1; fail=1
  fi
done
if [ "$dead" = 0 ] && "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" --set agentDefaults.heartbeatIntervalMs=5000 >/dev/null 2>&1; then
  echo "ok   rendered: no dead agent variable in any ci/ render; agentDefaults.heartbeatIntervalMs still accepted"
elif [ "$dead" = 0 ]; then
  echo "FAIL: agentDefaults.heartbeatIntervalMs is no longer accepted (older values files set it)"; fail=1
fi

# NEXUS_COMPONENT_TOPOLOGY (ADR-0037 §4) reaches the api-gateway, and only it:
# platform-operators by default, infrastructure-viewers in the AKS example.
for case in "minimal:platform-operators" "aks-existing-cluster:infrastructure-viewers"; do
  file=${case%%:*} want=${case#*:}
  if "$helm" template nexus "$chart" -f "$chart/ci/$file-values.yaml" >"$tmp/topology" 2>&1 &&
     [ "$(grep -c 'name: NEXUS_COMPONENT_TOPOLOGY' "$tmp/topology")" = 1 ] &&
     grep -A80 -E 'image: .*/api-gateway:' "$tmp/topology" | grep -qF "name: NEXUS_COMPONENT_TOPOLOGY, value: \"$want\""; then
    echo "ok   rendered: NEXUS_COMPONENT_TOPOLOGY=$want on the api-gateway alone ($file)"
  else
    echo "FAIL: NEXUS_COMPONENT_TOPOLOGY did not render $want on the api-gateway alone ($file)"; fail=1
  fi
done

# A worker-pool release renders agents and nothing else -- no Service,
# StatefulSet, Job, CronJob, ConfigMap or control-plane account -- even with
# no schedulers, and its agents dial what agentDefaults names.
if "$helm" template nexus-pool "$chart" -n nexus-pool -f "$chart/ci/aks-worker-pool-values.yaml" \
    --set-json 'schedulers=[]' >"$tmp/pool" 2>&1 &&
   python3 - "$tmp/pool" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
kinds = {d["kind"] for d in docs}
assert kinds <= {"ServiceAccount", "Deployment", "PodDisruptionBudget", "Role", "RoleBinding", "NetworkPolicy"}, kinds
sas = {d["metadata"]["name"] for d in docs if d["kind"] == "ServiceAccount"}
assert sas == {"nexus-agent"}, sas
deps = [d for d in docs if d["kind"] == "Deployment"]
assert deps, "no agent Deployment"
for d in deps:
    tpl = d["spec"]["template"]
    assert tpl["metadata"]["labels"].get("nexus.khuriwalgroup.com/role") == "agent", d["metadata"]["name"]
    env = {e["name"]: e.get("value") for e in tpl["spec"]["containers"][0]["env"]}
    assert env["NEXUS_CONTROL_PLANE"] == "api-gateway.nexus.svc:9091", env["NEXUS_CONTROL_PLANE"]
    assert env["NEXUS_NATS_URL"] == "nats://nats.nexus.svc:4222", env.get("NEXUS_NATS_URL")
    assert env["NEXUS_CLUSTER_NAME"] == "aks-prod-weu"
    assert env["NEXUS_K8S_NODE_SELECTOR"] == "kubernetes.azure.com/agentpool=gpu"
    assert env["NEXUS_K8S_TOLERATIONS"] == "sku=gpu:NoSchedule"
PY
then
  echo "ok   rendered: worker-pool release = agents only, dialing agentDefaults.controlPlane and agentDefaults.nats"
else
  echo "FAIL: the worker-pool release did not render as expected"; tail -5 "$tmp/pool"; fail=1
fi

# Runtime identity on every Nexus process: the downward-API pod, namespace
# and node on each container of a Nexus image but the console and the Jobs,
# NEXUS_CLUSTER_NAME = cluster.name (claimPlacement.clusterName for the
# agents), and no container that sets one variable twice. agentDefaults.nats.url
# "" takes NATS away from the agents only. The cluster controller reports
# clusterController.clusterName (which defaults to cluster.name), and keeps
# POD_NAME / POD_NAMESPACE for its Lease.
if "$helm" template nexus "$chart" -f "$chart/ci/full-values.yaml" --set claimPlacement.clusterName=gpu-claims \
    --set agentDefaults.nats.url= >"$tmp/identity" 2>&1 &&
   python3 - "$tmp/identity" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d and d["kind"] in ("Deployment", "CronJob")]
seen = set()
for d in docs:
    tpl = d["spec"]["jobTemplate"]["spec"]["template"] if d["kind"] == "CronJob" else d["spec"]["template"]
    agent = tpl["metadata"]["labels"].get("nexus.khuriwalgroup.com/role") == "agent"
    for c in tpl["spec"].get("initContainers", []) + tpl["spec"]["containers"]:
        names = [e["name"] for e in c.get("env", [])]
        assert len(names) == len(set(names)), (d["metadata"]["name"], c["name"], "duplicate env")
    c = tpl["spec"]["containers"][0]
    if "kgf-nexus/ui:" in c["image"]:
        continue
    env = {e["name"]: e for e in c.get("env", [])}
    for var, field in (("NEXUS_POD_NAME", "metadata.name"), ("NEXUS_POD_NAMESPACE", "metadata.namespace"), ("NEXUS_NODE_NAME", "spec.nodeName")):
        assert env[var]["valueFrom"]["fieldRef"]["fieldPath"] == field, (d["metadata"]["name"], var)
    want = "gpu-claims" if agent else ("dev" if d["metadata"]["name"] == "nexus-cluster-controller" else "ci-cluster")
    assert env["NEXUS_CLUSTER_NAME"]["value"] == want, (d["metadata"]["name"], env["NEXUS_CLUSTER_NAME"])
    if d["metadata"]["name"] == "nexus-cluster-controller":
        assert env["POD_NAME"]["valueFrom"]["fieldRef"]["fieldPath"] == "metadata.name"
        assert env["POD_NAMESPACE"]["valueFrom"]["fieldRef"]["fieldPath"] == "metadata.namespace"
    if agent:
        assert "NEXUS_NATS_URL" not in env, d["metadata"]["name"]
    if d["metadata"]["name"] == "event-service":
        assert env["NEXUS_NATS_URL"]["value"] == "nats://nats:4222"
    seen.add(d["metadata"]["name"])
for want in ("api-gateway", "workflow-manager", "dag-engine", "scheduler", "scheduler-gpu", "resource-manager", "event-service", "agent-cpu", "artifact-gc", "nexus-cluster-controller"):
    assert want in seen, (want, sorted(seen))
PY
then
  echo "ok   rendered: pod/namespace/node/cluster identity on every Nexus process; agents' NATS follows agentDefaults.nats.url"
else
  echo "FAIL: runtime identity or agent NATS did not render as expected"; tail -5 "$tmp/identity"; fail=1
fi

# The agents' control-plane TLS over this release's mTLS (ci/full-values.yaml):
# the client leaf stays, the CA comes from caSecretName, the name is overridden.
if grep -A40 'NEXUS_CONTROL_PLANE, value: "api-gateway.nexus-validate.svc:9091"' "$tmp/full.yaml" >"$tmp/cptls" &&
   grep -qF 'name: NEXUS_TLS_CLIENT_CERT_FILE, value: /etc/nexus/tls/tls.crt' "$tmp/cptls" &&
   grep -qF 'name: NEXUS_TLS_CA_FILE, value: "/etc/nexus/control-plane-ca/ca.crt"' "$tmp/cptls" &&
   grep -qF 'name: NEXUS_TLS_SERVER_NAME, value: "workers.example.com"' "$tmp/cptls" &&
   grep -qF 'secretName: nexus-gateway-ca' "$tmp/full.yaml"; then
  echo "ok   rendered: agents verify the control plane against agentDefaults.controlPlane.tls.caSecretName and serverName"
else
  echo "FAIL: agentDefaults.controlPlane.tls did not render as expected"; fail=1
fi

# Scheduler HPAs the cluster controller owns (ADR-0036). One condition
# (nexus.controllerOwnsSchedulerHPAs) both annotates a scheduler's HPA for the
# controller and binds the controller's patch on it: controller on, apply on,
# apply.schedulerAutoscaling not false. The stateless services' HPAs are never
# annotated; every other combination annotates nothing and grants no patch.
hpa_base=(--set autoscaling.enabled=true --set clusterController.clusterName=c
  --set clusterController.apply.expectedClusterUID=00000000-0000-0000-0000-000000000000
  --set-json 'schedulers=[{"name":"scheduler","queue":"default","autoscaling":{"enabled":true,"minReplicas":2,"maxReplicas":4}},{"name":"scheduler-gpu","queue":"gpu"}]')
hpa_case() { # <want owned: yes|no> <description> <helm args...>
  local want=$1 what=$2; shift 2
  if "$helm" template nexus "$chart" -f "$chart/ci/minimal-values.yaml" "${hpa_base[@]}" "$@" >"$tmp/hpa" 2>&1 &&
     python3 - "$tmp/hpa" "$want" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
owned = sys.argv[2] == "yes"
key = "nexus.khuriwalgroup.com/autoscaling-owner"
hpas = {d["metadata"]["name"]: d for d in docs if d["kind"] == "HorizontalPodAutoscaler"}
assert set(hpas) >= {"scheduler", "api-gateway"}, sorted(hpas)
assert "scheduler-gpu" not in hpas, "a scheduler without its own autoscaling got an HPA"
for name, h in hpas.items():
    ann = (h["metadata"].get("annotations") or {}).get(key)
    want = "cluster-controller" if (owned and name == "scheduler") else None
    assert ann == want, (name, ann)
s = hpas["scheduler"]["spec"]
assert (s["minReplicas"], s["maxReplicas"]) == (2, 4), s   # values: lookup is empty here
roles = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Role"}
role = roles.get("nexus-cluster-controller-apply-scheduler")
if owned:
    patch = [r for r in role["rules"] if "patch" in r["verbs"]]
    assert patch and patch[0]["resourceNames"] == ["scheduler"], role["rules"]
else:
    assert role is None, "patch on scheduler HPAs bound although the chart does not hand them over"
PY
  then
    echo "ok   rendered: $what"
  else
    echo "FAIL: $what"; tail -5 "$tmp/hpa"; fail=1
  fi
}
hpa_case yes "scheduler HPA annotated for the controller, and patch bound on it, in apply mode" \
  --set clusterController.enabled=true --set clusterController.apply.enabled=true
hpa_case no  "scheduler HPA left to Helm in plan mode" \
  --set clusterController.enabled=true
hpa_case no  "scheduler HPA left to Helm with apply.schedulerAutoscaling=false" \
  --set clusterController.enabled=true --set clusterController.apply.enabled=true --set clusterController.apply.schedulerAutoscaling=false
hpa_case no  "scheduler HPA left to Helm without the controller" \
  --set clusterController.apply.enabled=true

# An owned HPA keeps the controller's four fields across `helm upgrade`: they
# come from the live object when it carries the owner annotation. lookup is
# empty under `helm template`, so the merge is rendered in a probe chart over
# this chart's helpers with live objects given as values.
probe="$tmp/hpa-probe"
mkdir -p "$probe/templates"
cp "$chart"/templates/_*.tpl "$probe/templates/"
printf 'apiVersion: v2\nname: hpa-probe\nversion: 0.0.0\n' >"$probe/Chart.yaml"
cat >"$probe/templates/probe.yaml" <<'TPL'
{{- $a := dict "minReplicas" 2 "maxReplicas" 6 "targetCPUUtilizationPercentage" 70 "targetMemoryUtilizationPercentage" 80 "behavior" (dict "scaleDown" (dict "stabilizationWindowSeconds" 300 "policies" (list (dict "type" "Pods" "value" 1 "periodSeconds" 60)))) }}
{{- range $case, $live := .Values.cases }}
---
apiVersion: v1
kind: ConfigMap
metadata: { name: {{ $case }} }
data:
  settings: {{ include "nexus.hpaOwnedSettings" (list $a $live) | fromYaml | toJson | quote }}
{{- end }}
TPL
cat >"$tmp/hpa-live.yaml" <<'YAML'
cases:
  owned:
    metadata: { annotations: { nexus.khuriwalgroup.com/autoscaling-owner: cluster-controller } }
    spec:
      minReplicas: 3
      maxReplicas: 10
      metrics:
        - { type: Resource, resource: { name: memory, target: { type: Utilization, averageUtilization: 80 } } }
        - { type: Resource, resource: { name: cpu, target: { type: Utilization, averageUtilization: 55 } } }
      behavior: { scaleDown: { stabilizationWindowSeconds: 120 } }
  owned-defaults:
    metadata: { annotations: { nexus.khuriwalgroup.com/autoscaling-owner: cluster-controller } }
    spec: { maxReplicas: 5, metrics: [] }
  not-owned:
    metadata: { annotations: { example.com/other: x } }
    spec: { minReplicas: 5, maxReplicas: 9 }
  absent: {}
YAML
if "$helm" template probe "$probe" -f "$tmp/hpa-live.yaml" >"$tmp/hpa-probe.out" 2>&1 &&
   python3 - "$tmp/hpa-probe.out" <<'PY'
import json, sys, yaml
got = {d["metadata"]["name"]: json.loads(d["data"]["settings"]) for d in yaml.safe_load_all(open(sys.argv[1])) if d}
def four(s):
    return (s["minReplicas"], s["maxReplicas"], s["targetCPUUtilizationPercentage"], s["behavior"]["scaleDown"]["stabilizationWindowSeconds"])
assert four(got["owned"]) == (3, 10, 55, 120), got["owned"]
# Unset on the live object: Kubernetes' minReplicas default, values elsewhere.
assert four(got["owned-defaults"]) == (1, 5, 70, 300), got["owned-defaults"]
assert four(got["not-owned"]) == (2, 6, 70, 300), got["not-owned"]
assert four(got["absent"]) == (2, 6, 70, 300), got["absent"]
for s in got.values():   # never the controller's: still from values
    assert s["targetMemoryUtilizationPercentage"] == 80, s
    assert s["behavior"]["scaleDown"]["policies"] == [{"type": "Pods", "value": 1, "periodSeconds": 60}], s
PY
then
  echo "ok   rendered: an owned HPA keeps the live min/max/CPU/scale-down window; values elsewhere and when not owned"
else
  echo "FAIL: the owned-HPA live merge did not render as expected"; tail -5 "$tmp/hpa-probe.out"; fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "validate: FAILED" >&2
  exit 1
fi
echo "validate: all values files lint, render and validate; every bad value is refused"
