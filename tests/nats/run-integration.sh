#!/usr/bin/env bash

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Runs the NATS/outbox integration tests against a REAL Postgres and a REAL NATS
# JetStream server, each in its own throwaway Docker container.
#
#   tests/nats/run-integration.sh            # start, migrate, test, tear down
#   KEEP=1 tests/nats/run-integration.sh     # leave the containers running
#   PKGS="./internal/store/" tests/nats/run-integration.sh
#
# WHY ISOLATED CONTAINERS rather than the shared dev database: several of these
# tests stop and restart the broker on purpose (the outage test), and the relay
# processes every unpublished outbox row it can see. Pointing either at a shared
# stack would make one person's test run another person's outage. The names and
# ports are fixed and distinct (nexus-nats-agent-*, 55511/55512) so they cannot
# collide with the compose stack (55433) or the CONTRACT.md dev DB (55432).
#
# WHY -p 1: the outage test stops the NATS container. Run in parallel with the
# agent package's tests, it would turn their assertions red for a reason that has
# nothing to do with what they check (F86: a red-when-fine test trains re-runs).
set -euo pipefail

cd "$(dirname "$0")/../.."

PG_NAME=${PG_NAME:-nexus-nats-agent-pg}
NATS_NAME=${NATS_NAME:-nexus-nats-agent-nats}
PG_PORT=${PG_PORT:-55511}
NATS_PORT=${NATS_PORT:-55512}
# Pinned to the same images compose ships, so a test green here says something
# about the stack people actually run.
PG_IMAGE=${PG_IMAGE:-postgres:16-alpine}
NATS_IMAGE=${NATS_IMAGE:-nats:2.14.7-alpine}
PKGS=${PKGS:-./internal/bus/... ./internal/store/ ./services/event-service/... ./agent/internal/worker/}
# Only the TestNATS* tests by default: the older integration fixtures in
# internal/store predate tenant registration (migration 0014) and fail on a
# freshly migrated database for reasons unrelated to the bus. Override with
# RUN= to run everything.
RUN=${RUN-TestNATS}

cleanup() {
  if [ "${KEEP:-0}" != "1" ]; then
    docker rm -f "$PG_NAME" "$NATS_NAME" >/dev/null 2>&1 || true
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

start "$PG_NAME" -p "127.0.0.1:${PG_PORT}:5432" \
  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=nexus "$PG_IMAGE"
# -js enables JetStream; -sd puts its store on the container filesystem, which
# survives `docker stop`/`docker start` -- the outage test depends on that.
start "$NATS_NAME" -p "127.0.0.1:${NATS_PORT}:4222" "$NATS_IMAGE" -js -sd /data

echo "waiting for postgres"
for i in $(seq 1 60); do
  if docker exec "$PG_NAME" pg_isready -U postgres -d nexus >/dev/null 2>&1; then break; fi
  [ "$i" = 60 ] && { echo "postgres did not become ready" >&2; exit 1; }
  sleep 1
done

psql_in() { docker exec -i "$PG_NAME" psql -U postgres -d nexus -v ON_ERROR_STOP=1 -q "$@"; }

psql_in -c "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"
# Same algorithm as deploy/migrate.sh, including the no-transaction opt-out, so
# the schema under test is the one the migration job would produce.
for file in services/migrations/*.up.sql; do
  version=$(basename "$file" .up.sql)
  if [ "$(psql_in -tAc "SELECT 1 FROM schema_migrations WHERE version='$version'")" = "1" ]; then
    continue
  fi
  echo "applying $version"
  if grep -qiE '^[[:space:]]*--[[:space:]]*migrate:[[:space:]]*no-transaction[[:space:]]*$' "$file"; then
    psql_in < "$file"
  else
    psql_in --single-transaction < "$file"
  fi
  psql_in -c "INSERT INTO schema_migrations (version) VALUES ('$version')"
done

echo "waiting for nats"
for i in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${NATS_PORT}" >/dev/null 2>&1 || nc -z 127.0.0.1 "$NATS_PORT" 2>/dev/null; then break; fi
  [ "$i" = 30 ] && { echo "nats did not become ready" >&2; exit 1; }
  sleep 1
done

DSN="postgres://postgres:test@127.0.0.1:${PG_PORT}/nexus?sslmode=disable"
export NEXUS_TEST_DATABASE_URL="$DSN"
export NEXUS_TEST_DSN="$DSN"
export NEXUS_TEST_NATS_URL="nats://127.0.0.1:${NATS_PORT}"
# Lets the outage test stop and start the broker. Unset, that test SKIPS with
# the standard "skipping integration test" marker rather than passing vacuously.
export NEXUS_TEST_NATS_CONTAINER="$NATS_NAME"

args=(-tags integration -count=1 -p 1 -timeout 15m)
[ -n "$RUN" ] && args+=(-run "$RUN")
# shellcheck disable=SC2086
out=$(go test "${args[@]}" -v $PKGS 2>&1) && status=0 || status=$?
echo "$out" | grep -E '^(=== RUN|--- |PASS|FAIL|ok|panic)|skipping integration test' || true
if grep -q "skipping integration test" <<<"$out"; then
  echo "NATS integration tests SKIPPED -- a dependency was unreachable; treating as failure (F85)" >&2
  exit 1
fi
if [ "$status" != 0 ]; then
  echo "$out" | tail -200 >&2
fi
exit "$status"
