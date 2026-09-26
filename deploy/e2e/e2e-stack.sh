#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Isolated compose stack for the browser end-to-end suite (ui/e2e).
#
#   deploy/e2e/e2e-stack.sh up      build + start, seed API keys, wait until a
#                                   worker is ACTIVE, write ui/e2e/.stack.env
#   deploy/e2e/e2e-stack.sh down    stop and remove containers AND volumes
#   deploy/e2e/e2e-stack.sh logs    dump every service's logs (CI artefact)
#   deploy/e2e/e2e-stack.sh ps      container status
#
# Settings (environment):
#   E2E_PROJECT      compose project name             (default nexus-e2e)
#   E2E_PORT_BASE    first of NINE consecutive host ports (default 55711)
#   E2E_MINIO_IMAGE  optional: use this local MinIO-compatible image (must
#                    contain `mc`) instead of the digest-pinned Silo image the
#                    base compose file uses; see docker-compose.minio-local.yml
#   E2E_NAMESPACE    namespace the keys are minted in (default default)
#   E2E_READY_TIMEOUT_S  seconds to wait for the stack (default 600)
#   E2E_EXTRA_COMPOSE    optional overlay file(s), space-separated, applied last
#
# Database: the stack runs on the HOST's PostgreSQL (the project rule: Postgres
# comes from the host, never a container), in its own database, created by `up`
# (kept across `up` calls) and dropped by `down`:
#   NEXUS_TEST_PG_SERVER admin DSN of the server (default the workstation's,
#                        postgres://127.0.0.1:5432/postgres; tests/lib/hostpg.sh)
#   E2E_DB               the database (default the project name with every
#                        character outside [a-z0-9] turned into '_', so
#                        nexus-e2e -> nexus_e2e)
#   NEXUS_DB_MAX_CONNS   caps every service's pool; set it on a shared server
#   E2E_BUNDLED_POSTGRES=1  opt in to the compose file's own postgres
#                        container instead (a machine or CI runner with no
#                        PostgreSQL). Only then is E2E_DATABASE_URL's default
#                        the container's published port.
#   E2E_DATABASE_URL     the DSN `make seed` uses from the host (default: the
#                        database above)
#
# Port layout, from E2E_PORT_BASE (B):
#   B+0 console (nginx)   B+1 REST gateway   B+2 Postgres (bundled only)   B+3 MinIO S3
#   B+4 MinIO console     B+5 Mailpit UI     B+6 NATS       B+7 worker gRPC
#   B+8 gateway /metrics
# Every other published port of the dev compose file is dropped by
# docker-compose.e2e.yml, so this stack never collides with `make up`.
#
# The API keys are generated per stack, inserted hashed by `make seed`, and
# written only to ui/e2e/.stack.env (gitignored), which the Playwright config
# reads. Nothing here is a fixed or committed credential.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PROJECT="${E2E_PROJECT:-nexus-e2e}"
BASE="${E2E_PORT_BASE:-55711}"
NS="${E2E_NAMESPACE:-default}"
READY_TIMEOUT_S="${E2E_READY_TIMEOUT_S:-600}"
STACK_ENV="$ROOT/ui/e2e/.stack.env"

export NEXUS_UI_PORT=$((BASE + 0))
export NEXUS_API_PORT=$((BASE + 1))
export NEXUS_POSTGRES_PORT=$((BASE + 2))
export NEXUS_MINIO_PORT=$((BASE + 3))
export NEXUS_MINIO_CONSOLE_PORT=$((BASE + 4))
export NEXUS_MAILPIT_UI_PORT=$((BASE + 5))
export NEXUS_NATS_PORT=$((BASE + 6))
export NEXUS_GRPC_PORT=$((BASE + 7))
export NEXUS_GATEWAY_METRICS_PORT=$((BASE + 8))
export NEXUS_PUBLIC_URL="http://localhost:${NEXUS_UI_PORT}"
export NEXUS_NAMESPACE="$NS"

files=(-f deploy/docker-compose.yml -f deploy/e2e/docker-compose.e2e.yml)
BUNDLED_PG="${E2E_BUNDLED_POSTGRES:-}"
if [[ "$BUNDLED_PG" != 1 ]]; then
  # shellcheck source=tests/lib/hostpg.sh
  . tests/lib/hostpg.sh
  HOSTPG_TAG=org.kgf-nexus.e2e-stack
  E2E_DB="${E2E_DB:-$(printf '%s' "$PROJECT" | tr -c 'a-z0-9' '_')}"
  hostpg_valid_name "$E2E_DB" || { echo "E2E_DB '$E2E_DB' must match ^[a-z_][a-z0-9_]{0,62}\$" >&2; exit 2; }
  host_dsn="$(hostpg_url "$HOSTPG_SERVER" "$E2E_DB")"
  export NEXUS_HOST_DATABASE_URL="${NEXUS_HOST_DATABASE_URL:-$(hostpg_container_url "$host_dsn")}"
  export E2E_DATABASE_URL="${E2E_DATABASE_URL:-$host_dsn}"
  # For the per-service-login overlay: its services and db-roles job reach the
  # same database (NEXUS_E2E_DB is host:port/database, without credentials).
  _c="${NEXUS_HOST_DATABASE_URL#*://}"; _c="${_c#*@}"
  export NEXUS_E2E_DB="${NEXUS_E2E_DB:-${_c%%\?*}}"
  export NEXUS_E2E_OWNER_URL="${NEXUS_E2E_OWNER_URL:-$NEXUS_HOST_DATABASE_URL}"
else
  files+=(-f deploy/docker-compose.bundled-postgres.yml)
fi
if [[ -n "${E2E_MINIO_IMAGE:-}" ]]; then
  files+=(-f deploy/e2e/docker-compose.minio-local.yml)
fi
# E2E_AI=1 adds the AI-workload overlay: a Python-capable agent and the mock
# LLM sidecar, for examples/llm_* (ADR-0032, OPERATIONS §29.8).
if [[ "${E2E_AI:-}" == 1 ]]; then
  files+=(-f deploy/e2e/docker-compose.ai.yml)
fi
# Optional overlays, space-separated, applied last (e.g. the simulated-GPU and
# spot overlay deploy/e2e/docker-compose.gpu-sim.yml, ADR-0029/0030).
for extra in ${E2E_EXTRA_COMPOSE:-}; do
  files+=(-f "$extra")
done
compose() { docker compose -p "$PROJECT" "${files[@]}" "$@"; }

log() { printf '[e2e-stack] %s\n' "$*" >&2; }

gen_key() {
  printf 'nxs_%s_%s' "$1" "$(openssl rand -hex 32)"
}

# Keys survive between `up` calls on the same stack: agents already running
# hold the worker key, so minting a new one would strand them.
load_or_mint_keys() {
  if [[ -f "$STACK_ENV" ]] && grep -q "^E2E_PROJECT=$PROJECT$" "$STACK_ENV"; then
    # shellcheck disable=SC1090
    source "$STACK_ENV"
    ADMIN_KEY="$E2E_API_KEY"
    WORKER_KEY="$E2E_WORKER_API_KEY"
  else
    ADMIN_KEY="$(gen_key admin)"
    WORKER_KEY="$(gen_key worker)"
  fi
  export NEXUS_WORKER_API_KEY="$WORKER_KEY"
}

write_stack_env() {
  umask 077
  mkdir -p "$(dirname "$STACK_ENV")"
  cat >"$STACK_ENV" <<EOF
# Written by deploy/e2e/e2e-stack.sh for project $PROJECT. Gitignored; the
# keys are valid only against this disposable stack.
E2E_PROJECT=$PROJECT
E2E_BASE_URL=http://localhost:${NEXUS_UI_PORT}
E2E_API_URL=http://localhost:${NEXUS_API_PORT}
E2E_MAILPIT_URL=http://localhost:${NEXUS_MAILPIT_UI_PORT}
E2E_NAMESPACE=$NS
E2E_API_KEY=$ADMIN_KEY
E2E_WORKER_API_KEY=$WORKER_KEY
E2E_MINIO_IMAGE=${E2E_MINIO_IMAGE:-}
EOF
}

# Polls `cmd` until it succeeds or the shared deadline passes. The e2e suite
# itself never sleeps; the stack bring-up is the one place that must wait on
# something outside a browser, and it waits on a condition, not a duration.
deadline=0
wait_for() {
  local what="$1"; shift
  log "waiting for $what"
  until "$@" >/dev/null 2>&1; do
    if (( SECONDS > deadline )); then
      log "timed out waiting for $what"
      compose ps -a >&2 || true
      return 1
    fi
    sleep 2
  done
}

migrate_done() {
  local id
  id="$(compose ps -a -q migrate)"
  [[ -n "$id" ]] || return 1
  [[ "$(docker inspect -f '{{.State.Status}} {{.State.ExitCode}}' "$id")" == "exited 0" ]]
}

migrate_failed() {
  local id
  id="$(compose ps -a -q migrate)"
  [[ -n "$id" ]] && [[ "$(docker inspect -f '{{.State.Status}}' "$id")" == "exited" ]] \
    && [[ "$(docker inspect -f '{{.State.ExitCode}}' "$id")" != "0" ]]
}

gateway_ready() {
  curl -fsS -H "Authorization: Bearer $ADMIN_KEY" \
    "http://localhost:${NEXUS_API_PORT}/v1/namespaces/${NS}/runs?limit=1"
}

# At least one ACTIVE worker serving the default queue: the suite's workflow
# runs there, and a run submitted before any worker registers would still
# pass, only slower -- but a stack with NO worker would fail every run test
# with a timeout that says nothing about why.
worker_active() {
  curl -fsS -H "Authorization: Bearer $ADMIN_KEY" \
    "http://localhost:${NEXUS_API_PORT}/v1/namespaces/${NS}/workers" \
    | grep -q '"ACTIVE"'
}

ui_ready() {
  curl -fsS "http://localhost:${NEXUS_UI_PORT}/healthz"
}

seed() {
  # E2E_DATABASE_URL: the stack's database as the HOST reaches it, when an
  # overlay points the services at a database outside the stack.
  local dsn="${E2E_DATABASE_URL:-postgres://nexus:nexus-dev-password@localhost:${NEXUS_POSTGRES_PORT}/nexus?sslmode=disable}"
  if command -v psql >/dev/null 2>&1; then
    NEXUS_DEV_API_KEY="$ADMIN_KEY" NEXUS_WORKER_API_KEY="$WORKER_KEY" \
      make --no-print-directory seed DATABASE_URL="$dsn" NAMESPACE="$NS" >/dev/null
  else
    # Same statements `make seed` runs, through the container's psql, for a
    # host without a Postgres client.
    [[ "$BUNDLED_PG" == 1 ]] || { log "psql is required: the stack uses the host's PostgreSQL"; exit 1; }
    log "psql not on PATH; seeding through the postgres container"
    local role key hash
    compose exec -T postgres psql -U nexus -d nexus -v ON_ERROR_STOP=1 -q -c \
      "INSERT INTO tenants (namespace, name) VALUES ('$NS', '$NS') ON CONFLICT (namespace) DO NOTHING" >/dev/null
    for role in admin worker; do
      if [[ $role == admin ]]; then key="$ADMIN_KEY"; else key="$WORKER_KEY"; fi
      hash="$(printf '%s' "$key" | shasum -a 256 | cut -d' ' -f1)"
      compose exec -T postgres psql -U nexus -d nexus -v ON_ERROR_STOP=1 -q -c \
        "INSERT INTO api_keys (namespace, name, key_hash, role)
         VALUES ('$NS', '$role-e2e', '$hash', '$role')
         ON CONFLICT (key_hash) DO NOTHING" >/dev/null
    done
  fi
  log "seeded admin and worker API keys (hashes only in the database)"
}

cmd_up() {
  command -v openssl >/dev/null || { log "openssl is required"; exit 1; }
  load_or_mint_keys
  write_stack_env
  if [[ -n "${E2E_MINIO_IMAGE:-}" ]]; then
    log "NOTE: the S3 store runs from local image $E2E_MINIO_IMAGE (not the pinned Silo digest)"
  fi
  if [[ "$BUNDLED_PG" == 1 ]]; then
    log "project $PROJECT: console :$NEXUS_UI_PORT, gateway :$NEXUS_API_PORT, bundled postgres :$NEXUS_POSTGRES_PORT"
  else
    hostpg_check >&2
    hostpg_ensure "$E2E_DB"
    log "project $PROJECT: console :$NEXUS_UI_PORT, gateway :$NEXUS_API_PORT, database $E2E_DB on $(hostpg_redact "$HOSTPG_SERVER")"
  fi
  compose up -d --build
  deadline=$((SECONDS + READY_TIMEOUT_S))
  until migrate_done; do
    if migrate_failed; then
      log "migrate failed"; compose logs migrate >&2; exit 1
    fi
    (( SECONDS > deadline )) && { log "timed out waiting for migrate"; compose ps -a >&2; exit 1; }
    sleep 2
  done
  seed
  wait_for "the gateway to accept the admin key" gateway_ready
  wait_for "an ACTIVE worker" worker_active
  wait_for "the console" ui_ready
  log "ready: $(grep -E '^E2E_(BASE|API)_URL' "$STACK_ENV" | tr '\n' ' ')"
}

cmd_down() {
  compose down -v --remove-orphans
  if [[ "$BUNDLED_PG" != 1 ]]; then
    hostpg_drop "$E2E_DB"
  fi
  rm -f "$STACK_ENV"
}

# Compose interpolates the whole file for every command, and the agents'
# NEXUS_API_KEY is required (`:?`) there. Only `up` needs the real worker key;
# down/logs/ps get a placeholder so they work even after .stack.env is gone.
if [[ "${1:-}" != up ]]; then
  export NEXUS_WORKER_API_KEY="${NEXUS_WORKER_API_KEY:-not-needed-for-$1}"
fi

case "${1:-}" in
  up) cmd_up ;;
  down) cmd_down ;;
  logs) compose logs --no-color --timestamps ;;
  ps) compose ps -a ;;
  *) echo "usage: $0 up|down|logs|ps" >&2; exit 2 ;;
esac
