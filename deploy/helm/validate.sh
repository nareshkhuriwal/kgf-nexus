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
# 3. Negative cases: values that MUST be refused -- by values.schema.json or by
#    a template guard -- and the message that must name the fix.
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

# An agent serving another tenant (agents[].namespace) needs that tenant's NATS
# identity: refused without it, rendered with the tenant's key and worker key.
nkeys=(--set nats.auth.mode=nkey
  --set-json 'nats.auth.eventService={"publicKeys":["UEVENTSERVICEKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}'
  --set-json 'nats.auth.monitor={"publicKeys":["UMONITORKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}')
team_agent='agents=[{"name":"agent-team","namespace":"team-ml","workerApiKeySecretKey":"worker-api-key-team-ml","natsSeedKey":"nats-agent-nkey-team-ml","queues":"default","cpuMillis":1000,"memoryBytes":1073741824,"diskBytes":1073741824}]'
must_fail "agent namespace without a NATS tenant" 'no entry for namespace "team-ml"' "${nkeys[@]}" \
  --set-json 'nats.auth.tenants=[{"namespace":"default","publicKeys":["UAGENTKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}]' \
  --set-json "$team_agent"
if helm template nexus "$chart" -f "$chart/ci/minimal-values.yaml" "${nkeys[@]}" \
    --set-json 'nats.auth.tenants=[{"namespace":"team-ml","publicKeys":["UAGENTKEYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}]' \
    --set-json "$team_agent" >"$tmp/team" 2>&1 &&
   grep -qF 'name: NEXUS_NAMESPACE, value: "team-ml"' "$tmp/team" &&
   grep -qF 'key: worker-api-key-team-ml' "$tmp/team"; then
  echo "ok   rendered: agent in namespace team-ml with its own worker key"
else
  echo "FAIL: an agent's namespace and worker key override did not render"; tail -3 "$tmp/team"; fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "validate: FAILED" >&2
  exit 1
fi
echo "validate: all values files lint, render and validate; every bad value is refused"
