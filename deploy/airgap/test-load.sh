#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# End-to-end test of an air-gapped bundle (make airgap-test, docs/AIRGAP.md):
#
#   1. start a throwaway registry (digest-pinned registry:2) on localhost;
#   2. run the bundle's own load.sh against it (which verifies checksums,
#      pushes, records registry digests and renders values-airgap.yaml);
#   3. independently re-render the chart with those values and every probe
#      (all optional components on) and require ONLY <registry>/...@sha256;
#   4. prove the registry serves every recorded digest (manifest inspect), and
#      pull a few back by digest after removing the local copies.
#
#   deploy/airgap/test-load.sh dist/nexus-airgap-<version>.tar.gz
#
# Env: AIRGAP_TEST_NAME (container, default nexus-airgap-test-registry),
# AIRGAP_TEST_PORT (default 55570), KEEP=1 leaves the registry running.
set -euo pipefail

bundle=${1:?usage: test-load.sh BUNDLE.tar.gz}
name=${AIRGAP_TEST_NAME:-nexus-airgap-test-registry}
port=${AIRGAP_TEST_PORT:-55570}
registry_image=${AIRGAP_REGISTRY_IMAGE:-docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373}
reg="localhost:${port}/nexus"
work=$(mktemp -d)
log() { printf 'airgap-test: %s\n' "$*" >&2; }
cleanup() {
  rm -rf "$work"
  if [ "${KEEP:-0}" != "1" ]; then docker rm -f "$name" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT

docker rm -f "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" -p "127.0.0.1:${port}:5000" "$registry_image" >/dev/null
for _ in $(seq 1 30); do curl -fsS "http://localhost:${port}/v2/" >/dev/null 2>&1 && break; sleep 1; done
log "registry $name on localhost:${port}"

tar -xzf "$bundle" -C "$work"
dir=$(find "$work" -mindepth 1 -maxdepth 1 -type d | head -1)
"$dir/load.sh" --bundle "$dir" --registry "$reg" --plain-http \
  --values-out "$work/values-airgap.yaml" --digests-out "$work/pushed.tsv"

chart=$(find "$dir/chart" -maxdepth 1 -name '*.tgz' | head -1)
total=0
for probe in "$dir"/airgap/probe-*.json; do
  helm template nexus "$chart" -f "$probe" -f "$work/values-airgap.yaml" >"$work/render.yaml"
  while read -r img; do
    [ -z "$img" ] && continue
    total=$((total + 1))
    case "$img" in
      "$reg"/*@sha256:*) ;;
      *) log "FAIL: not redirected by digest: $img"; exit 1 ;;
    esac
  done < <(sed -n "s/^[[:space:]]*\(- \)\{0,1\}image:[[:space:]]*[\"']\{0,1\}\([^\"' ]*\).*/\2/p" "$work/render.yaml" | sort -u)
done
log "helm template: $total image references across the probes, all $reg/...@sha256"

n=0
while IFS=$'\t' read -r nm target digest _bundle_digest; do
  case "$nm" in ''|'#'*) continue ;; esac
  ref="${target%:*}@${digest}"
  docker manifest inspect --insecure "$ref" >/dev/null || { log "FAIL: the registry does not serve $ref"; exit 1; }
  n=$((n + 1))
  if [ "$n" -le 3 ]; then
    docker image rm "$ref" >/dev/null 2>&1 || true
    docker pull -q "$ref" >/dev/null || { log "FAIL: cannot pull $ref"; exit 1; }
    docker image rm "$ref" >/dev/null 2>&1 || true
    log "pulled back by digest: $ref"
  fi
done <"$work/pushed.tsv"
[ "$n" -gt 0 ] || { log "FAIL: no pushed images recorded"; exit 1; }
log "PASS: $n images mirrored and served by digest"
