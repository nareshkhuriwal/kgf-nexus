#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Download the contact sheet of an image-processing-gpu run from the Kubernetes
# dev environment and open it.
#
#   examples/image_processing_gpu/fetch-contact-sheet.sh            # latest run
#   examples/image_processing_gpu/fetch-contact-sheet.sh <run-id>   # a given run
#
# DEV-ONLY. The platform API does not serve task outputs yet, so this reads the
# output's object key from the dev database and streams the object out of MinIO
# through a short-lived mc pod. Writes nexus-contact-sheet-<run>.pgm (and a .png
# on macOS) in the current directory.
set -euo pipefail

ns=${NEXUS_SUITE_K8S_NAMESPACE:-nexus-dev}
pw=$(kubectl -n "$ns" get secret kgf-nexus-credentials -o jsonpath='{.data.postgres-password}' | base64 -d)
sql() { kubectl -n "$ns" exec postgres-0 -- env PGPASSWORD="$pw" psql -U nexus -d nexus -tA -c "$1"; }

run=${1:-$(sql "SELECT r.id FROM workflow_runs r JOIN workflow_specs s ON s.id = r.spec_id
                 WHERE s.name = 'image-processing-gpu' ORDER BY r.created_at DESC LIMIT 1")}
[ -n "$run" ] || { echo "no image-processing-gpu run found" >&2; exit 1; }

ref=$(sql "SELECT output_ref->'contact_sheet'->'blob' FROM task_attempts
            WHERE run_id = '$run' AND node_id = 'contact_sheet' AND state = 'SUCCEEDED' LIMIT 1")
[ -n "$ref" ] || { echo "run $run has no finished contact sheet yet" >&2; exit 1; }
bucket=$(printf '%s' "$ref" | python3 -c 'import sys,json; print(json.load(sys.stdin)["bucket"])')
key=$(printf '%s' "$ref" | python3 -c 'import sys,json; print(json.load(sys.stdin)["key"])')

user=$(kubectl -n "$ns" get secret kgf-nexus-credentials -o jsonpath='{.data.minio-root-user}' | base64 -d)
pass=$(kubectl -n "$ns" get secret kgf-nexus-credentials -o jsonpath='{.data.minio-root-password}' | base64 -d)
mc_image=$(kubectl -n "$ns" get statefulset minio -o jsonpath='{.spec.template.spec.containers[0].image}' | sed 's#/minio:#/mc:#; s#RELEASE\.2025-04-22T22-12-26Z#RELEASE.2025-04-16T18-13-26Z#')

out="nexus-contact-sheet-${run:0:8}.pgm"
# The credentials go in as the throwaway pod's environment, not its command
# line. They are still visible in the pod spec to anyone who can read pods in
# this namespace for the few seconds it exists -- acceptable for a dev cluster.
# The pod meets the "restricted" Pod Security profile like everything else in
# the namespace: non-root, no privilege escalation, no capabilities.
pod="fetch-sheet-$$"
overrides=$(POD="$pod" IMG="$mc_image" SRC="nexus/${bucket}/${key}" HOSTVAR="http://${user}:${pass}@minio:9000" python3 -c '
import json, os
print(json.dumps({"spec": {
  "securityContext": {"runAsNonRoot": True, "runAsUser": 1000, "seccompProfile": {"type": "RuntimeDefault"}},
  "containers": [{
    "name": os.environ["POD"], "image": os.environ["IMG"], "stdin": True, "stdinOnce": True,
    "command": ["mc", "cat", os.environ["SRC"]],
    "env": [{"name": "MC_HOST_nexus", "value": os.environ["HOSTVAR"]}, {"name": "MC_CONFIG_DIR", "value": "/tmp/mc"}],
    "securityContext": {"allowPrivilegeEscalation": False, "capabilities": {"drop": ["ALL"]}}}]}}))')
kubectl -n "$ns" run "$pod" --rm -i --quiet --restart=Never --image="$mc_image" \
  --overrides="$overrides" > "$out"

echo "run $run -> $out ($(wc -c < "$out" | tr -d ' ') bytes, $(sed -n 3p "$out") pixels)"
if command -v sips >/dev/null 2>&1; then
  sips -s format png "$out" --out "${out%.pgm}.png" >/dev/null 2>&1 && echo "   ${out%.pgm}.png" && open "${out%.pgm}.png" 2>/dev/null || true
fi
