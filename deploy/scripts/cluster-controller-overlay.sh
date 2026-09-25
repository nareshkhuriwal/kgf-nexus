#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Render the cluster-controller kustomization with a release's images, and prove
# the images took.
#
# The kustomize base names its images kgf-nexus/cluster-controller:0.1.0 and
# (in worker-deployment.example.yaml) kgf-nexus/agent:0.1.0, which no registry
# serves; the release pushes <acr>/nexus/<image> and knows each digest. This
# writes a throwaway overlay over base/ (plan mode) or apply/ whose `images:`
# entries retarget those names -- exactly what `kustomize edit set image
# kgf-nexus/cluster-controller=<ref>` writes, without needing the kustomize CLI
# or editing the checked-in base -- renders it with `kubectl kustomize`, and
# FAILS unless every rendered container of that image runs exactly the ref.
#
# Usage:
#   [NAMESPACE=ns] [AGENT_IMAGE=ref] [ALLOW_TAG=1] \
#     cluster-controller-overlay.sh MODE CONTROLLER_IMAGE > rendered.yaml
#
#   MODE              base (plan mode, read-only) | apply
#   CONTROLLER_IMAGE  <repo>@sha256:<64 hex>; with ALLOW_TAG=1 (a local dev
#                     cluster only) also <repo>:<tag>
#   AGENT_IMAGE       the same form, for the example worker Deployment; unset
#                     leaves the base's placeholder
#   NAMESPACE         overrides the base's kgf-nexus
#
# The rendered manifests go to stdout; `kubectl apply -f` them.
set -euo pipefail

usage="usage: cluster-controller-overlay.sh base|apply CONTROLLER_IMAGE"
mode=${1:?$usage}
controller=${2:?$usage}

case "$mode" in base|apply) ;; *) echo "mode must be base or apply, got '$mode'" >&2; exit 2 ;; esac

# image_entry NAME REF: one kustomize `images:` entry retargeting NAME to REF.
image_entry() {
  local from=$1 ref=$2
  if [[ "$ref" =~ ^([^@[:space:]]+)@(sha256:[0-9a-f]{64})$ ]]; then
    printf '  - name: %s\n    newName: %s\n    digest: %s\n' "$from" "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
  elif [ "${ALLOW_TAG:-0}" = 1 ] && [[ "$ref" =~ ^([^@[:space:]]+):([A-Za-z0-9_.-]+)$ ]]; then
    printf '  - name: %s\n    newName: %s\n    newTag: "%s"\n' "$from" "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
  else
    echo "$from: image must be <repo>@sha256:<64 hex> (ALLOW_TAG=1 also accepts <repo>:<tag>), got '$ref'" >&2
    return 2
  fi
}

# assert_image RENDERED MATCH REF: every rendered image line containing MATCH is REF.
assert_image() {
  local rendered=$1 match=$2 ref=$3 images bad
  images=$(sed -nE 's/^[[:space:]]*(- )?image:[[:space:]]*"?([^"[:space:]]+)"?.*/\2/p' "$rendered" | grep -F "$match" || true)
  if [ -z "$images" ]; then
    echo "rendered $mode overlay contains no $match image" >&2
    return 1
  fi
  bad=$(grep -vxF "$ref" <<<"$images" || true)
  if [ -n "$bad" ]; then
    echo "$match image did not take; rendered:" >&2
    echo "$bad" >&2
    return 1
  fi
  echo "cluster-controller overlay ($mode): every $match container runs $ref" >&2
}

root=$(cd "$(dirname "$0")/../k8s/cluster-controller" && pwd)
# Inside the kustomization tree, because kustomize refuses an absolute path to
# a directory resource. Dot-named and removed on exit.
work=$(mktemp -d "$root/.release-overlay.XXXXXX")
trap 'rm -rf "$work"' EXIT

{
  echo "apiVersion: kustomize.config.k8s.io/v1beta1"
  echo "kind: Kustomization"
  echo "resources:"
  echo "  - ../$mode"
  if [ -n "${NAMESPACE:-}" ]; then echo "namespace: $NAMESPACE"; fi
  echo "images:"
  image_entry kgf-nexus/cluster-controller "$controller"
  if [ -n "${AGENT_IMAGE:-}" ]; then image_entry kgf-nexus/agent "$AGENT_IMAGE"; fi
} >"$work/kustomization.yaml"

kubectl kustomize "$work" >"$work/rendered.yaml"

assert_image "$work/rendered.yaml" /cluster-controller "$controller"
if [ -n "${AGENT_IMAGE:-}" ]; then assert_image "$work/rendered.yaml" /agent "$AGENT_IMAGE"; fi
cat "$work/rendered.yaml"
