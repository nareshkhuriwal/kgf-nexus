#!/usr/bin/env bash

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Runs the NATS/outbox integration tests against a REAL PostgreSQL and a REAL
# NATS JetStream server.
#
#   tests/nats/run-integration.sh            # create a DB, migrate, test, drop it
#   KEEP=1 tests/nats/run-integration.sh     # leave the database and brokers
#   PKGS="./internal/store/" tests/nats/run-integration.sh
#
# POSTGRES comes from the host, never a container (the project rule; see
# tests/lib/hostpg.sh). By default the script creates a throwaway database on
# the server NEXUS_TEST_PG_SERVER names (default: the workstation's PostgreSQL
# on 127.0.0.1:5432), migrates it with deploy/migrate.sh, and drops it on exit:
#
#   NEXUS_TEST_PG_SERVER  admin DSN of the server   (default postgres://127.0.0.1:5432/postgres)
#   NEXUS_NATS_DB         database name to create   (default nexus_nats_it_<pid>)
#   NEXUS_NATS_DATABASE_URL  use THIS existing database instead (migrated, never
#                         dropped). It must be dedicated to the run: the relay
#                         processes every unpublished outbox row it can see, and
#                         TestNATSRolledBackFactIsNeverPublished needs an empty
#                         backlog (F153).
#
# WHY ITS OWN DATABASE AND BROKERS rather than a shared stack: several of these
# tests stop and restart the broker on purpose (the outage test), and the relay
# processes every unpublished outbox row it can see. Pointing either at a shared
# stack would make one person's test run another person's outage. The broker
# names and ports are fixed and distinct (nexus-nats-agent-*, 55512-55514) so
# they cannot collide with the compose stack.
#
# WHY -p 1: the outage test stops the NATS container. Run in parallel with the
# agent package's tests, it would turn their assertions red for a reason that has
# nothing to do with what they check (F86: a red-when-fine test trains re-runs).
#
# AUTHORIZATION (TestNATSAuthz*, docs/OPERATIONS.md §11.7) runs against two more
# brokers whose configuration is the one deploy/ ships, never a hand-written one:
#   compose  keys + config from deploy/nats/nexus-nats-auth.sh, plaintext
#   helm     the chart's nats-config ConfigMap (helm template), TLS with
#            client verification, certificates from deploy/nexus-certs.sh
# Both get two tenants so cross-tenant isolation is actually exercised. Keys and
# certificates live in a temporary directory that is deleted on exit. The
# semantic suites above run against the unauthenticated broker, as before, so
# they keep testing relay semantics rather than permissions.
#
# JETSTREAM STORAGE (F162, docs/OPERATIONS.md §11.10). nats-server reserves a
# file stream's whole size when it is created. The compose broker's store is a
# deliberately SMALL 3 GiB tmpfs, so its derived limit (75% of free space) is
# the same on every machine, and the suite's stream creation proves the DEFAULT
# sizes fit a small volume -- rather than passing or failing with the runner's
# free disk, which is how 2 x 4 GiB streams once failed CI. The Helm broker
# runs the chart's explicit max_file_store, and the script checks the broker
# reports exactly the rendered value. Neither store allocates memory it does not
# use: tmpfs holds only what is written.
set -euo pipefail

cd "$(dirname "$0")/../.."

# shellcheck source=tests/lib/hostpg.sh
. tests/lib/hostpg.sh
HOSTPG_TAG=org.kgf-nexus.nats-integration
hostpg_suite_pool_floor

NATS_NAME=${NATS_NAME:-nexus-nats-agent-nats}
NATS_PORT=${NATS_PORT:-55512}
# Pinned to the same image compose ships, so a test green here says something
# about the stack people actually run.
NATS_IMAGE=${NATS_IMAGE:-nats:2.14.7-alpine}
PKGS=${PKGS:-./internal/bus/... ./internal/store/ ./services/event-service/... ./agent/internal/worker/}
# Only the TestNATS* tests by default: the older integration fixtures in
# internal/store predate tenant registration (migration 0014) and fail on a
# freshly migrated database for reasons unrelated to the bus. Override with
# RUN= to run everything.
RUN=${RUN-TestNATS}
# At most this many tests at once within a package (go test -parallel), which
# bounds the connections the suite opens on a shared server (F153).
PARALLEL=${PARALLEL:-2}
# The authorization brokers. AUTHZ=0 skips them, loudly (it is not a pass).
AUTHZ=${AUTHZ:-1}
AUTHZ_NAME=${AUTHZ_NAME:-nexus-nats-agent-authz}
AUTHZ_PORT=${AUTHZ_PORT:-55513}
HELM_NAME=${HELM_NAME:-nexus-nats-agent-authz-helm}
HELM_PORT=${HELM_PORT:-55514}
AUTHZ_TENANTS=${AUTHZ_TENANTS:-"authz-a authz-b"}
AUTHZ_PKGS=${AUTHZ_PKGS:-./internal/bus/... ./services/event-service/relay/ ./agent/internal/worker/}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/nexus-nats-authz.XXXXXX")
OWN_DB=""
cleanup() {
  if [ "${KEEP:-0}" != "1" ]; then
    docker rm -f "$NATS_NAME" "$AUTHZ_NAME" "$HELM_NAME" >/dev/null 2>&1 || true
    if [ -n "$OWN_DB" ]; then hostpg_drop "$OWN_DB"; fi
    rm -rf "$WORK"
  else
    echo "KEEP=1: brokers, ${OWN_DB:-the database} and $WORK (NATS seeds, test CA) left in place"
  fi
}
trap cleanup EXIT

start() {
  local name=$1; shift
  if docker ps --format '{{.Names}}' | grep -qx "$name"; then
    echo "reusing running container $name"
    return
  fi
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" "$@" >/dev/null
}

if [ -n "${NEXUS_NATS_DATABASE_URL:-}" ]; then
  DSN=$NEXUS_NATS_DATABASE_URL
  echo "using the database in NEXUS_NATS_DATABASE_URL ($(hostpg_redact "$DSN")); it is migrated, not dropped"
else
  hostpg_check
  OWN_DB=${NEXUS_NATS_DB:-nexus_nats_it_$$}
  hostpg_create "$OWN_DB"
  DSN=$(hostpg_url "$HOSTPG_SERVER" "$OWN_DB")
  echo "created database $OWN_DB on $(hostpg_redact "$HOSTPG_SERVER")"
fi
# The same script and algorithm as the migration job, including the
# no-transaction opt-out, so the schema under test is the one it would produce.
hostpg_migrate "$DSN" >/dev/null

# -js enables JetStream; -sd puts its store on the container filesystem, which
# survives `docker stop`/`docker start` -- the outage test depends on that.
start "$NATS_NAME" -p "127.0.0.1:${NATS_PORT}:4222" "$NATS_IMAGE" -js -sd /data

echo "waiting for nats"
for i in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${NATS_PORT}" >/dev/null 2>&1 || nc -z 127.0.0.1 "$NATS_PORT" 2>/dev/null; then break; fi
  [ "$i" = 30 ] && { echo "nats did not become ready" >&2; exit 1; }
  sleep 1
done

export NEXUS_TEST_DATABASE_URL="$DSN"
export NEXUS_TEST_NATS_URL="nats://127.0.0.1:${NATS_PORT}"
# Lets the outage test stop and start the broker. Unset, that test SKIPS with
# the standard "skipping integration test" marker rather than passing vacuously.
export NEXUS_TEST_NATS_CONTAINER="$NATS_NAME"

# run_suite LABEL PKGS go-test-args...: run, summarise, and fail on any skip.
run_suite() {
  local label=$1 pkgs=$2
  shift 2
  echo "── $label"
  local out status
  # shellcheck disable=SC2086
  out=$(go test -tags integration -count=1 -p 1 -parallel "${PARALLEL:-2}" -timeout 15m -v "$@" $pkgs 2>&1) && status=0 || status=$?
  echo "$out" | grep -E '^(=== RUN|--- |PASS|FAIL|ok|panic)|skipping integration test' || true
  if grep -q "skipping integration test" <<<"$out"; then
    echo "$label: tests SKIPPED -- a dependency was unreachable; treating as failure (F85)" >&2
    return 1
  fi
  if [ "$status" != 0 ]; then
    echo "$out" | tail -200 >&2
  fi
  return "$status"
}

# Semantics against the unauthenticated broker. The authorization tests need a
# broker that has authorization, so they are excluded here and run below.
sem_args=(-skip TestNATSAuthz)
[ -n "$RUN" ] && sem_args+=(-run "$RUN")
run_suite "semantics (unauthenticated broker)" "$PKGS" "${sem_args[@]}"

if [ "$AUTHZ" != "1" ]; then
  echo "AUTHZ=0: the broker AUTHORIZATION tests were NOT run" >&2
  exit 0
fi

wait_healthy() { # container
  for i in $(seq 1 30); do
    if docker exec "$1" wget -q -O- 'http://127.0.0.1:8222/healthz?js-enabled-only=true' >/dev/null 2>&1; then return 0; fi
    [ "$i" = 30 ] && { docker logs "$1" >&2; echo "$1 did not become healthy" >&2; return 1; }
    sleep 1
  done
}

# js_max_storage CONTAINER: the JetStream file-store limit the broker enforces,
# in bytes (/jsz, on the container's loopback-only monitoring port).
js_max_storage() {
  docker exec "$1" wget -q -O- 'http://127.0.0.1:8222/jsz' |
    python3 -c 'import json, sys; print(json.load(sys.stdin)["config"]["max_storage"])'
}

# ── compose rendering ──────────────────────────────────────────────────────
KEYS="$WORK/keys"
tenant_args=()
for t in $AUTHZ_TENANTS; do tenant_args+=(--tenant "$t"); done
# --admin: the optional stream-maintenance identity (TestNATSAuthzAdmin*).
NEXUS_NATS_DIR="$KEYS" deploy/nats/nexus-nats-auth.sh init "${tenant_args[@]}" --admin
NEXUS_NATS_DIR="$KEYS" deploy/nats/nexus-nats-auth.sh render
docker rm -f "$AUTHZ_NAME" >/dev/null 2>&1 || true
docker run -d --name "$AUTHZ_NAME" -p "127.0.0.1:${AUTHZ_PORT}:4222" \
  -v "$KEYS/server:/etc/nats:ro" --tmpfs /data:size=3g "$NATS_IMAGE" -c /etc/nats/nats.conf >/dev/null
wait_healthy "$AUTHZ_NAME"
# The rendered config leaves max_file_store unset (the compose default), so the
# limit must come from the 3 GiB store, not from the host's disk.
small=2415919104 # 75% of 3 GiB, as nats-server derives it
max=$(js_max_storage "$AUTHZ_NAME")
if [ -z "$max" ] || [ "$max" -gt "$small" ]; then
  echo "compose broker: JetStream file-store limit '$max' is not derived from its 3 GiB store (at most $small)" >&2
  exit 1
fi
echo "compose broker: JetStream file-store limit $max bytes (75% of a 3 GiB store)"

export NEXUS_TEST_NATS_AUTHZ_DIR="$KEYS"
export NEXUS_TEST_NATS_AUTHZ_TENANTS="$AUTHZ_TENANTS"
NEXUS_TEST_NATS_AUTHZ_VARIANT=compose NEXUS_TEST_NATS_AUTHZ_URL="nats://127.0.0.1:${AUTHZ_PORT}" \
  run_suite "authorization: compose config (nexus-nats-auth.sh), plaintext" "$AUTHZ_PKGS" -run TestNATSAuthz

# ── Helm rendering, with TLS and client verification ───────────────────────
command -v helm >/dev/null || { echo "helm is required for the Helm authorization variant (AUTHZ=0 skips all authorization tests)" >&2; exit 1; }
CERTS="$WORK/certs"
NEXUS_CERT_DIR="$CERTS" deploy/nexus-certs.sh init-ca >/dev/null
NEXUS_CERT_DIR="$CERTS" deploy/nexus-certs.sh issue nats --dns nats,localhost --ip 127.0.0.1 >/dev/null
NEXUS_CERT_DIR="$CERTS" deploy/nexus-certs.sh issue authz-client --client-only >/dev/null
first_tenant=${AUTHZ_TENANTS%% *}
NEXUS_NATS_DIR="$KEYS" deploy/nats/nexus-nats-auth.sh helm-values >"$WORK/values.yaml"
helm template authz deploy/helm/kgf-nexus --show-only templates/nats-config.yaml \
  --set existingSecret=unused --set image.tag=test --set nexusNamespace="$first_tenant" \
  --set nats.tls.enabled=true --set nats.tls.secretName=nats-tls \
  --set nats.tls.clientSecrets.eventService=x --set nats.tls.clientSecrets.agent=y \
  -f "$WORK/values.yaml" >"$WORK/nats-config.yaml"
mkdir -p "$WORK/helm"
python3 - "$WORK/nats-config.yaml" "$WORK/helm" <<'PY'
import sys, yaml
cm = yaml.safe_load(open(sys.argv[1]))
for name, body in cm["data"].items():
    open(f"{sys.argv[2]}/{name}", "w").write(body)
PY
docker rm -f "$HELM_NAME" >/dev/null 2>&1 || true
docker run -d --name "$HELM_NAME" -p "127.0.0.1:${HELM_PORT}:4222" \
  -v "$WORK/helm:/etc/nats:ro" -v "$CERTS/nats:/etc/nats-tls:ro" --tmpfs /var/run/nats \
  "$NATS_IMAGE" -c /etc/nats/nats.conf >/dev/null
wait_healthy "$HELM_NAME"
want=$(sed -n 's/^ *max_file_store: *\([0-9][0-9]*\) *$/\1/p' "$WORK/helm/nats.conf")
max=$(js_max_storage "$HELM_NAME")
if [ -z "$want" ] || [ "$max" != "$want" ]; then
  echo "helm broker: JetStream file-store limit '$max', but the chart rendered max_file_store '$want'" >&2
  exit 1
fi
echo "helm broker: JetStream file-store limit $max bytes = the chart's max_file_store (80% of nats.storage.size)"

NEXUS_TEST_NATS_AUTHZ_VARIANT=helm-tls NEXUS_TEST_NATS_AUTHZ_URL="tls://127.0.0.1:${HELM_PORT}" \
  NEXUS_TEST_NATS_AUTHZ_CA="$CERTS/ca-bundle.pem" \
  NEXUS_TEST_NATS_AUTHZ_CLIENT_CERT="$CERTS/authz-client/tls.crt" \
  NEXUS_TEST_NATS_AUTHZ_CLIENT_KEY="$CERTS/authz-client/tls.key" \
  run_suite "authorization: Helm config, TLS + client verification" "$AUTHZ_PKGS" -run TestNATSAuthz
