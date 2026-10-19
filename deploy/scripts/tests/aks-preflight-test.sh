#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Tests for deploy/scripts/aks-preflight.sh against a stub kubectl (and a stub
# az) on PATH: no cluster, no network, nothing created anywhere. Each case
# shapes the fake cluster with STUB_* variables (aks-preflight-stub/kubectl)
# and asserts the exit code, the status of named checks, and -- in every
# default-mode case -- that no call other than a read reached kubectl.
#
#   deploy/scripts/tests/aks-preflight-test.sh        (bash 3.2 or later)
set -u

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PREFLIGHT="$HERE/../aks-preflight.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/aks-preflight-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

# az: logged in, workload identity on, pools system/cpu/gput4.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/az" <<'AZ'
#!/usr/bin/env bash
printf '%s\n' "az $*" >>"${STUB_LOG:-/dev/null}"
case "$*" in
  "account show -o none") exit 0 ;;
  "aks show "*) printf '%s\n%s\n%s\n%s\n' "${STUB_AZ_OIDC:-true}" "${STUB_AZ_WI:-true}" 1.33.4 "https://westeurope.oic.prod-aks.azure.com/t/u/" ;;
  "aks nodepool list "*) printf 'system\ncpu\ngput4\n' ;;
  *) echo "stub az: unexpected call: $*" >&2; exit 99 ;;
esac
AZ
chmod +x "$WORK/bin/az"
export PATH="$HERE/aks-preflight-stub:$WORK/bin:$PATH"
export NO_COLOR=1

PASSED=0
FAILED=0
OUT="$WORK/out"
export STUB_LOG="$WORK/calls"

# run ARGS... -> $OUT holds stdout, $RC the exit code; $STUB_LOG the calls.
run() {
  : >"$STUB_LOG"
  bash "$PREFLIGHT" "$@" >"$OUT" 2>"$WORK/stderr"
  RC=$?
}

fail() {
  echo "FAIL  $CASE: $*"
  sed 's/^/        /' "$OUT"
  FAILED=$((FAILED + 1))
  CASE_OK=0
}

start() { CASE=$1; CASE_OK=1; }
done_case() { if [ "$CASE_OK" = 1 ]; then echo "ok    $CASE"; PASSED=$((PASSED + 1)); fi; }

expect_rc() { [ "$RC" = "$1" ] || fail "exit code $RC, want $1"; }
# expect_check ID STATUS
expect_check() {
  grep -Eq "^$2 +$1 " "$OUT" || fail "check $1 is not $2"
}
expect_read_only() {
  if grep -Ev '^(get|config|version|api-resources|auth) |^az (account show|aks show|aks nodepool list)' "$STUB_LOG" | grep -q .; then
    fail "a non-read call reached kubectl/az: $(grep -Ev '^(get|config|version|api-resources|auth) |^az ' "$STUB_LOG" | head -n 3 | tr '\n' ' ')"
  fi
}
expect_no_unexpected() {
  if grep -q '^UNEXPECTED' "$STUB_LOG"; then fail "the stub saw a call it does not model: $(grep '^UNEXPECTED' "$STUB_LOG" | head -n 1)"; fi
}

AKS_ARGS=(--context aks-test --namespace nexus --system-pool system --pool cpu --gpu-pool gput4
          --secret nexus-credentials --db-logins-secret nexus-db-logins --ca-secret nexus-postgres-ca)

# ── A healthy cluster ───────────────────────────────────────────────────

start "healthy AKS cluster: every check passes, read-only"
run "${AKS_ARGS[@]}" --tls --az-resource-group rg --az-cluster aks
expect_rc 0
for c in cluster.reachable k8s.version nodes.ready cluster.aks pool.system pool.cpu pool.gput4 gpu.gput4 \
         wi.webhook wi.oidc-issuer metrics-server storage-class cert-manager namespace secret db-logins db-ca az.cluster; do
  expect_check "$c" PASS
done
expect_check ingress WARN
grep -q 'pool.cpu .*1/2 nodes Ready\|pool.cpu .*2/2 nodes Ready' "$OUT" || fail "pool.cpu does not count its nodes"
grep -q 'pool.cpu .*215 free pod slots' "$OUT" || fail "pool.cpu headroom is not 2 x 110 - 5"
grep -q 'db-ca .*2 PEM certificates' "$OUT" || fail "db-ca does not count the certificates"
expect_read_only
expect_no_unexpected
done_case

start "the schedulers' system pool may carry CriticalAddonsOnly"
run "${AKS_ARGS[@]}"
expect_check pool.system PASS
done_case

# ── Failures ────────────────────────────────────────────────────────────

start "old Kubernetes, no webhook, no OIDC issuer, no metrics-server"
STUB_VERSION=v1.28.9 STUB_WEBHOOK=0 STUB_ISSUER=https://kubernetes.default.svc.cluster.local STUB_METRICS=missing \
  run "${AKS_ARGS[@]}"
expect_rc 1
expect_check k8s.version FAIL
expect_check wi.webhook FAIL
expect_check wi.oidc-issuer FAIL
expect_check metrics-server FAIL
expect_read_only
done_case

start "a GPU pool without the device plugin"
STUB_GPU="" STUB_DS="" run "${AKS_ARGS[@]}"
expect_rc 1
expect_check gpu.gput4 FAIL
grep -q 'no NVIDIA device plugin DaemonSet' "$OUT" || fail "the GPU failure does not say the plugin is missing"
done_case

start "agents on the CriticalAddonsOnly system pool are refused"
run --context aks-test --namespace nexus --pool system
expect_rc 1
expect_check pool.system FAIL
done_case

start "a pool that does not exist (az knows the pools) and one scaled to zero (no az)"
run --context aks-test --namespace nexus --pool nosuchpool --az-resource-group rg --az-cluster aks
expect_check pool.nosuchpool FAIL
run --context aks-test --namespace nexus --pool nosuchpool
expect_check pool.nosuchpool WARN
done_case

start "az: workload identity disabled"
STUB_AZ_WI=false run "${AKS_ARGS[@]}" --az-resource-group rg --az-cluster aks
expect_rc 1
expect_check az.cluster FAIL
done_case

start "a missing StorageClass, and the default one"
run "${AKS_ARGS[@]}" --storage-class premium-zrs
expect_check storage-class FAIL
run "${AKS_ARGS[@]}" --storage-class ""
expect_check storage-class PASS
done_case

start "Secret: a missing key, a URL-unsafe password, a short one"
STUB_SECRET_KEYS="postgres-password worker-api-key" run "${AKS_ARGS[@]}"
expect_check secret FAIL
grep -q 'lacks: nats-event-service-nkey nats-agent-nkey' "$OUT" || fail "the missing NATS seeds are not named"
STUB_SECRET_KEYS="postgres-password worker-api-key" run "${AKS_ARGS[@]}" --no-nats
expect_check secret PASS
STUB_PG_PASSWORD='p@ss/word:0123456789' run "${AKS_ARGS[@]}"
expect_check secret FAIL
STUB_PG_PASSWORD='short1' run "${AKS_ARGS[@]}"
expect_check secret WARN
if grep -q 'p@ss\|OwnerPassword\|nxs_worker_0123' "$OUT"; then fail "a secret value was printed"; fi
done_case

start "runtime logins: one missing"
STUB_GC_LOGIN="" run "${AKS_ARGS[@]}"
expect_check db-logins FAIL
done_case

start "namespace: missing, unlabelled, baseline"
run --context aks-test --namespace missing --secret nexus-credentials
expect_check namespace WARN
expect_check secret SKIP
STUB_ENFORCE="" run "${AKS_ARGS[@]}"
expect_check namespace WARN
STUB_ENFORCE=baseline run "${AKS_ARGS[@]}"
expect_check namespace PASS
done_case

start "an unreachable API server stops early with a FAIL"
STUB_UNREACHABLE=1 run "${AKS_ARGS[@]}"
expect_rc 1
expect_check cluster.reachable FAIL
[ "$(grep -c '^[A-Z][A-Z]* ' "$OUT")" -eq 1 ] || fail "checks ran after the cluster was unreachable"
done_case

start "--strict fails on a warning"
run "${AKS_ARGS[@]}" --strict
expect_rc 1
done_case

# ── JSON ────────────────────────────────────────────────────────────────

start "--json is one valid document with the summary"
STUB_PG_PASSWORD='tab	and "quote" password' run "${AKS_ARGS[@]}" --json
expect_rc 1
if command -v python3 >/dev/null 2>&1; then
  python3 - "$OUT" <<'PY' || fail "invalid JSON or wrong summary"
import json, sys
d = json.load(open(sys.argv[1]))
s = d["summary"]
assert d["tool"] == "aks-preflight" and d["context"] == "aks-test" and d["namespace"] == "nexus"
assert s["fail"] == 1 and s["pass"] + s["warn"] + s["fail"] + s["skip"] == len(d["checks"])
assert [c for c in d["checks"] if c["id"] == "secret"][0]["status"] == "FAIL"
PY
else
  grep -q '^{"tool":"aks-preflight"' "$OUT" || fail "not a JSON document"
fi
[ "$(wc -l <"$OUT" | tr -d ' ')" = 1 ] || fail "JSON mode printed more than one line"
done_case

# ── Opt-in checks that create a pod ─────────────────────────────────────

start "--pull-test: pulls, then deletes its pod"
run "${AKS_ARGS[@]}" --pull-test ghcr.io/example/kgf-nexus/agent:v0.3.0 --test-pool cpu
expect_check image-pull PASS
grep -q '^create -f -' "$STUB_LOG" || fail "no pod was created"
grep -q '^delete pod nexus-preflight-pull-' "$STUB_LOG" || fail "the test pod was not deleted"
grep -q 'command: \["/nexus-preflight-does-not-exist"\]' "$STUB_LOG" || fail "the pull-test pod could run the image's code"
grep -q 'runAsNonRoot: true' "$STUB_LOG" || fail "the test pod is not restricted"
grep -q '"kubernetes.azure.com/agentpool": "cpu"' "$STUB_LOG" || fail "--test-pool was not applied"
done_case

start "--pull-test: an image that cannot be pulled fails, and the pod is still deleted"
STUB_PULL=ImagePullBackOff run "${AKS_ARGS[@]}" --pull-test ghcr.io/example/private:v1
expect_rc 1
expect_check image-pull FAIL
grep -q '^delete pod nexus-preflight-pull-' "$STUB_LOG" || fail "the test pod was not deleted"
done_case

start "--db-test: pg_isready's exit codes"
run "${AKS_ARGS[@]}" --db-test prod-weu-pg.postgres.database.azure.com
expect_check db-reachable PASS
grep -q '"pg_isready", "-h", "prod-weu-pg.postgres.database.azure.com", "-p", "5432"' "$STUB_LOG" || fail "pg_isready was not pointed at the host and default port"
STUB_DB_EXIT=2 run "${AKS_ARGS[@]}" --db-test db.example.test:6432
expect_check db-reachable FAIL
grep -q '"-p", "6432"' "$STUB_LOG" || fail "the port was not taken from HOST:PORT"
STUB_DB_EXIT=1 run "${AKS_ARGS[@]}" --db-test db.example.test
expect_check db-reachable WARN
done_case

# ── Usage ───────────────────────────────────────────────────────────────

start "usage errors exit 2"
run --no-such-flag
expect_rc 2
run --az-resource-group rg
expect_rc 2
run --namespace Bad_Name
expect_rc 2
run --min-pod-headroom lots
expect_rc 2
run --help
expect_rc 0
done_case

echo
echo "aks-preflight tests: $PASSED passed, $FAILED failed"
[ "$FAILED" -eq 0 ]
