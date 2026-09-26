#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Static validation of the kgf-nexus chart. No cluster needed.
#
#   deploy/helm/validate.sh
#
# 1. helm lint --strict with every values file in deploy/helm/kgf-nexus/ci/.
# 2. helm template with each, piped through kubeconform against the Kubernetes
#    OpenAPI schemas for KUBE_VERSION (the chart's floor by default), strict
#    mode. CRD kinds (cert-manager, prometheus-operator, Gateway API) are
#    validated against the datreeio CRDs catalog; with SCHEMA_OFFLINE=1 they
#    are skipped instead (-ignore-missing-schemas) and only core kinds checked.
# 3. Negative cases: values that MUST be refused -- by values.schema.json or by
#    a template guard -- and the message that must name the fix.
#
# Needs helm (v3.14+ or v4) and kubeconform on PATH.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
chart="$here/kgf-nexus"
kube_version=${KUBE_VERSION:-1.29.0}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

command -v helm >/dev/null || { echo "validate: helm not found" >&2; exit 1; }
command -v kubeconform >/dev/null || { echo "validate: kubeconform not found (https://github.com/yannh/kubeconform)" >&2; exit 1; }

cache=${KUBECONFORM_CACHE:-$HOME/.cache/kubeconform}
mkdir -p "$cache"
schema_args=(-cache "$cache" -schema-location default)
if [ "${SCHEMA_OFFLINE:-0}" = 1 ]; then
  schema_args+=(-ignore-missing-schemas)
else
  schema_args+=(-schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json')
fi

fail=0
for values in "$chart"/ci/*-values.yaml; do
  name=$(basename "$values" -values.yaml)
  echo "== $name"
  if ! helm lint --strict "$chart" -f "$values" --kube-version "$kube_version" >"$tmp/lint" 2>&1; then
    cat "$tmp/lint"; fail=1; continue
  fi
  # Release name "on": YAML 1.1 reads it as a boolean, so every unquoted use
  # of the release name in a label fails validation (it did, before the fix).
  helm template on "$chart" -n nexus-validate -f "$values" --kube-version "$kube_version" >"$tmp/$name.yaml"
  if kubeconform -strict -summary -kubernetes-version "$kube_version" "${schema_args[@]}" "$tmp/$name.yaml"; then
    :
  else
    fail=1
  fi
done

# must_fail <description> <expected message fragment> <helm --set args...>
must_fail() {
  local what=$1 expect=$2; shift 2
  if helm template nexus "$chart" -f "$chart/ci/minimal-values.yaml" "$@" >"$tmp/neg" 2>&1; then
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
