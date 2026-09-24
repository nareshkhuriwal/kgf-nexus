#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Run the workload suite end to end and report each workflow's own verdict.
#
#   examples/run-suite.sh                 # all four, default sizes
#   SUITE="csv ml" examples/run-suite.sh  # a subset
#   SCALE=large examples/run-suite.sh     # bigger inputs
#
# Every workflow checks its own result (row accounting, a model quality gate, a
# GPU oversubscription audit, an exact fan-in sum) and FAILS its run if the
# check fails, so this script's exit status is the suite's verdict.
#
# Targets the Kubernetes dev environment by default. Override NEXUS_ENDPOINT
# and NEXUS_API_KEY to point at anything else.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

export NEXUS_ENDPOINT=${NEXUS_ENDPOINT:-http://api.nexus.localhost}
if [ -z "${NEXUS_API_KEY:-}" ]; then
  NEXUS_API_KEY=$(terraform -chdir=deploy/terraform/environments/dev output -raw admin_api_key 2>/dev/null) || {
    echo "NEXUS_API_KEY is unset and the dev environment has no admin key output (run make k8s-dev-up)" >&2
    exit 2
  }
  export NEXUS_API_KEY
fi

bin=$(mktemp -d)/nexusctl
trap 'rm -rf "$(dirname "$bin")"' EXIT
go build -o "$bin" ./cli/cmd/nexusctl
ctl() { "$bin" "$@"; }

SCALE=${SCALE:-default}
if [ "$SCALE" = large ]; then
  csv_params="rows_per_region=100000 seed=7"; ml_params="rows=50000 seed=11 min_r2=0.95"
  gpu_params="hold_seconds=20";               scale_params="count=1500"
else
  csv_params="rows_per_region=20000 seed=7";  ml_params="rows=20000 seed=11 min_r2=0.95"
  gpu_params="hold_seconds=15";               scale_params="count=500"
fi

# key | example dir | workflow name | params | node whose output is the verdict
workloads=(
  "csv|csv_sales_pipeline|csv-sales-pipeline|$csv_params|report"
  "ml|ml_training_pipeline|ml-training-pipeline|$ml_params|quality_gate"
  "gpu|gpu_scheduling|gpu-scheduling|$gpu_params|audit"
  "scale|fanout_scale|fanout-scale|$scale_params|verify"
)
SUITE=${SUITE:-csv ml gpu scale}

json() { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }

# Show the verdict node's output. The platform API does not expose task outputs
# or logs yet (logs return 501; attempts carry no output reference), so on the
# Kubernetes dev environment this reads the verdict straight from the dev
# database. That is a dev-only diagnostic, not how a client should read
# results; elsewhere the run state above is the verdict.
verdict() { # run node
  local ns=${NEXUS_SUITE_K8S_NAMESPACE:-nexus-dev} pw out
  pw=$(kubectl -n "$ns" get secret kgf-nexus-credentials -o jsonpath='{.data.postgres-password}' 2>/dev/null | base64 -d 2>/dev/null) || true
  [ -n "$pw" ] || { echo "   (verdict output not retrievable: the API does not expose task outputs yet)"; return; }
  out=$(kubectl -n "$ns" exec postgres-0 -- env PGPASSWORD="$pw" psql -U nexus -d nexus -tA -c \
    "SELECT o.value->>'inline' FROM task_attempts a, jsonb_each(a.output_ref) o
      WHERE a.run_id = '$1' AND a.node_id = '$2' AND a.state = 'SUCCEEDED' LIMIT 1" 2>/dev/null | base64 -d 2>/dev/null) || true
  [ -n "$out" ] || { echo "   (no verdict output recorded for $2)"; return; }
  echo "   verdict ($2):"
  printf '%s' "$out" | python3 -m json.tool 2>/dev/null | sed 's/^/     /' || echo "     $out"
}

summary=()
failed=0
for w in "${workloads[@]}"; do
  IFS='|' read -r key dir name params final <<<"$w"
  case " $SUITE " in *" $key "*) ;; *) continue ;; esac

  echo
  echo "== $name  ($params)"
  # Quiet on success; a failed submit prints why and stops the suite. (The
  # canonicalization WARNING nexusctl prints today is a known, separate bug.)
  if ! out=$(ctl workflow submit "examples/$dir/compiled.yaml" 2>&1); then
    echo "$out" >&2
    exit 1
  fi

  args=()
  for kv in $params; do args+=(--param "$kv"); done
  run=$(ctl run create "$name" "${args[@]}" -o json | json 'd["id"]')
  echo "   run $run"

  start=$(date +%s)
  state=""
  while :; do
    detail=$(ctl run get "$run" -o json)
    state=$(json 'd.get("state","")' <<<"$detail")
    progress=$(json "f\"{sum(n['state']=='SUCCEEDED' for n in d['nodes'])}/{len(d['nodes'])} tasks succeeded\"" <<<"$detail")
    printf '\r   %-10s %-28s %4ss' "$state" "$progress" "$(( $(date +%s) - start ))"
    case "$state" in SUCCEEDED|FAILED|CANCELLED|TIMED_OUT) break ;; esac
    sleep 2
  done
  elapsed=$(( $(date +%s) - start ))
  echo

  verdict "$run" "$final"

  summary+=("$(printf '%-22s %-10s %5ss  run %s' "$name" "$state" "$elapsed" "$run")")
  [ "$state" = SUCCEEDED ] || failed=$((failed + 1))
done

echo
echo "== suite summary (console: http://nexus.localhost/runs)"
for s in "${summary[@]}"; do echo "   $s"; done
exit "$failed"
