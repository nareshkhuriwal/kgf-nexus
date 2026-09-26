#!/usr/bin/env bash

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# SSO end-to-end tests against a REAL Keycloak (OIDC and SAML 2.0) and a REAL
# PostgreSQL, each in its own throwaway container (ADR-0024, OPERATIONS §24).
#
#   tests/sso/run-keycloak-e2e.sh           # start, migrate, test, tear down
#   KEEP=1 tests/sso/run-keycloak-e2e.sh    # leave the containers running
#
# The gateway under test listens on localhost:55813 inside the Go test process;
# the realm's OIDC redirect URIs and the SAML client (created by the test
# through Keycloak's admin API) point there. Names and ports are fixed and
# distinct (nexus-w3e-*, 55811–55813) so they cannot collide with the compose
# stack or other suites.
set -euo pipefail

cd "$(dirname "$0")/../.."

PG_NAME=${PG_NAME:-nexus-w3e-sso-pg}
KC_NAME=${KC_NAME:-nexus-w3e-keycloak}
PG_PORT=${PG_PORT:-55814}
KC_PORT=${KC_PORT:-55812}
PG_IMAGE=${PG_IMAGE:-postgres:16-alpine}
# Pinned by version AND digest (OPERATIONS §22 rules): Keycloak 26.7.4, released
# 2026-09-16, the current release when this suite was written.
KC_IMAGE=${KC_IMAGE:-quay.io/keycloak/keycloak:26.7.4@sha256:82a77884f3af238beab1e7afd63b5f530e1b5c0590bd7aa60b40a40463e29b2c}

cleanup() {
  if [ "${KEEP:-0}" != 1 ]; then
    docker rm -f "$PG_NAME" "$KC_NAME" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

docker rm -f "$PG_NAME" "$KC_NAME" >/dev/null 2>&1 || true

docker run -d --name "$PG_NAME" -p "127.0.0.1:${PG_PORT}:5432" \
  -e POSTGRES_USER=nexus -e POSTGRES_PASSWORD=nexus-dev-password -e POSTGRES_DB=nexus \
  --tmpfs /var/lib/postgresql/data "$PG_IMAGE" -c fsync=off -c synchronous_commit=off >/dev/null

docker run -d --name "$KC_NAME" -p "127.0.0.1:${KC_PORT}:8080" \
  -e KC_BOOTSTRAP_ADMIN_USERNAME=admin -e KC_BOOTSTRAP_ADMIN_PASSWORD=admin-e2e-password \
  -e KC_HOSTNAME="http://localhost:${KC_PORT}" -e KC_HTTP_ENABLED=true -e KC_HEALTH_ENABLED=true \
  -v "$PWD/tests/sso/keycloak:/opt/keycloak/data/import:ro" \
  "$KC_IMAGE" start-dev --import-realm >/dev/null

DSN="postgres://nexus:nexus-dev-password@127.0.0.1:${PG_PORT}/nexus?sslmode=disable"
for _ in $(seq 1 60); do
  psql "$DSN" -qc 'select 1' >/dev/null 2>&1 && break
  sleep 1
done
DATABASE_URL="$DSN" MIGRATIONS_DIR=services/migrations sh deploy/migrate.sh

echo "waiting for Keycloak on :${KC_PORT} ..."
for i in $(seq 1 180); do
  if curl -sf "http://localhost:${KC_PORT}/realms/nexus-e2e/.well-known/openid-configuration" >/dev/null; then
    break
  fi
  if [ "$i" = 180 ]; then
    docker logs "$KC_NAME" | tail -50 >&2
    echo "Keycloak did not become ready" >&2
    exit 1
  fi
  sleep 1
done

LOG=$(mktemp "${TMPDIR:-/tmp}/nexus-sso-e2e.XXXXXX")
status=0
NEXUS_TEST_DATABASE_URL="$DSN" \
NEXUS_TEST_KEYCLOAK_URL="http://localhost:${KC_PORT}" \
NEXUS_TEST_KEYCLOAK_ADMIN_PASSWORD=admin-e2e-password \
  go test -tags sso_e2e -count=1 -v ./tests/sso/ >"$LOG" 2>&1 || status=$?
cat "$LOG"
# A skip looks like a pass in `go test` output (F85); refuse it.
if grep -q "skipping sso e2e" "$LOG"; then
  echo "SSO e2e tests SKIPPED; treating as failure" >&2
  status=1
fi
rm -f "$LOG"
exit "$status"
