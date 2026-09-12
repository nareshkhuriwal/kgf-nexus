#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Applies services/migrations/*.up.sql in lexical order, then exits 0.
#
# Idempotence comes from a schema_migrations ledger table rather than from the
# migration SQL itself: 0001_init.up.sql is frozen and contains plain CREATE
# TABLE statements, so re-running it would fail. Recording what has been applied
# is the only way to make `docker compose up` twice in a row work.
#
# `set -e` is load-bearing: a partially applied migration set must fail the job
# loudly, because every service depends on this container exiting 0 and a silent
# half-migration would start six services against a schema that is wrong.
set -eu

: "${DATABASE_URL:?DATABASE_URL must be set}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-/migrations}"

echo "migrate: waiting for postgres to accept connections"
for i in $(seq 1 60); do
  if psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; then
    break
  fi
  if [ "$i" = 60 ]; then
    echo "migrate: postgres did not become reachable in 60s" >&2
    exit 1
  fi
  sleep 1
done

# The ledger is created outside the migration set so migration 0001 does not
# have to know about the mechanism that tracks it.
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q <<'SQL'
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
SQL

applied=0
for file in "$MIGRATIONS_DIR"/*.up.sql; do
  [ -e "$file" ] || { echo "migrate: no migrations found in $MIGRATIONS_DIR" >&2; exit 1; }
  version=$(basename "$file" .up.sql)

  already=$(psql "$DATABASE_URL" -tAc \
    "SELECT 1 FROM schema_migrations WHERE version = '$version'")
  if [ "$already" = "1" ]; then
    echo "migrate: $version already applied"
    continue
  fi

  echo "migrate: applying $version"
  # Single transaction per migration: a failure leaves the schema untouched
  # rather than half-migrated, so a retry starts from a known state.
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -q -f "$file"
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
    "INSERT INTO schema_migrations (version) VALUES ('$version')"
  applied=$((applied + 1))
done

echo "migrate: done ($applied newly applied)"
