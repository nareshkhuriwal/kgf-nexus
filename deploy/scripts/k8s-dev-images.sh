#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Build every Nexus image and load it into a LOCAL kind-based cluster (Docker
# Desktop's Kubernetes, or kind itself), for deploy/terraform/environments/dev.
#
# Why load instead of push: a kind node is a container with its own containerd
# image store, so images in the host's Docker are invisible to it, and a dev
# loop should not need a registry. `docker save | ctr import` puts them where
# the kubelet looks, under the same name the chart renders
# (docker.io/kgf-nexus/<component>:<tag>).
#
# The tag is the git commit, plus a timestamped -dirty suffix for an
# uncommitted tree: a dirty build must never pass for the commit it started
# from, and a unique tag is also what makes Kubernetes roll the pods.
#
# Prints the tag as the LAST line of stdout, so callers can capture it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

context=$(kubectl config current-context)
case "$context" in
  docker-desktop|kind-*) ;;
  *) echo "refusing: kube context '$context' is not a local kind cluster (docker-desktop or kind-*)" >&2; exit 1 ;;
esac

tag=${TAG:-$(git rev-parse --short=12 HEAD)}
if [ -z "${TAG:-}" ] && [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  tag="${tag}-dirty-$(date +%Y%m%d%H%M%S)"
fi
prefix=kgf-nexus

build() { # component dockerfile [build-arg...]
  local component=$1 dockerfile=$2; shift 2
  local args=()
  for a in "$@"; do args+=(--build-arg "$a"); done
  echo "build ${prefix}/${component}:${tag}" >&2
  docker build --quiet -f "$dockerfile" ${args[@]+"${args[@]}"} -t "${prefix}/${component}:${tag}" . >/dev/null
}

# The same Dockerfiles and build args as deploy/docker-compose.yml.
build api-gateway      deploy/Dockerfile.service SERVICE=api-gateway      SERVICE_PATH=services/api-gateway/cmd/api-gateway           VERSION="$tag"
build workflow-manager deploy/Dockerfile.service SERVICE=workflow-manager SERVICE_PATH=services/workflow-manager/cmd/workflow-manager VERSION="$tag"
build dag-engine       deploy/Dockerfile.service SERVICE=dag-engine       SERVICE_PATH=services/dag-engine                            VERSION="$tag"
build scheduler        deploy/Dockerfile.service SERVICE=scheduler        SERVICE_PATH=services/scheduler                             VERSION="$tag"
build resource-manager deploy/Dockerfile.service SERVICE=resource-manager SERVICE_PATH=services/resource-manager/cmd/resource-manager VERSION="$tag"
build event-service    deploy/Dockerfile.service SERVICE=event-service    SERVICE_PATH=services/event-service/cmd/event-service       VERSION="$tag"
build agent            deploy/Dockerfile.service SERVICE=agent            SERVICE_PATH=agent/cmd/nexus-agent                          VERSION="$tag"
build migrate          deploy/Dockerfile.migrate
build ui               deploy/Dockerfile.ui      VITE_NEXUS_API_BASE= VITE_NEXUS_NAMESPACE=default

components=(api-gateway workflow-manager dag-engine scheduler resource-manager event-service agent migrate ui)
images=()
for c in "${components[@]}"; do images+=("${prefix}/${c}:${tag}"); done

# Every node of the cluster is a container named after the node.
nodes=$(kubectl get nodes -o jsonpath='{.items[*].metadata.name}')
for node in $nodes; do
  if ! docker inspect "$node" >/dev/null 2>&1; then
    echo "refusing: node '$node' is not a local container; this script only loads into kind nodes" >&2
    exit 1
  fi
  echo "load ${#images[@]} images into node ${node}" >&2
  docker save "${images[@]}" | docker exec -i "$node" ctr -n k8s.io images import - >/dev/null
done

echo "$tag"
