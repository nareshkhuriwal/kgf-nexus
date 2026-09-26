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
  #
  # A migration may opt out by declaring `-- migrate: no-transaction` on a line
  # of its own. Some statements CANNOT run inside a transaction block at all --
  # CREATE INDEX CONCURRENTLY is the one this exists for -- and without an
  # opt-out such a migration simply cannot be applied by this job, which is a
  # trap: it is written, it is committed, and it fails the first time it runs.
  #
  # The cost is real and belongs to the migration that opts in: a failure part
  # way through leaves the earlier statements applied. Such a migration must be
  # written so re-running it is safe -- IF NOT EXISTS, IF EXISTS -- because a
  # retry is what recovers it. The ledger is only written on success, so a
  # partial application is retried rather than recorded as done.
  if grep -qiE '^[[:space:]]*--[[:space:]]*migrate:[[:space:]]*no-transaction[[:space:]]*$' "$file"; then
    echo "migrate: $version declares no-transaction; applying without --single-transaction"
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -f "$file"
  else
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -q -f "$file"
  fi
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -c \
    "INSERT INTO schema_migrations (version) VALUES ('$version')"
  applied=$((applied + 1))
done

# Per-installation group-role names (0038, F153). Runs after EVERY migrate,
# not only when 0038 is new: it also moves whatever a later migration granted
# to the shared nexus_* names. NEXUS_DB_ROLE_PREFIX unset keeps whatever this
# database already uses (the shared names on every existing installation).
role_prefix="${NEXUS_DB_ROLE_PREFIX:-}"
if [ -n "$role_prefix" ] && ! printf '%s' "$role_prefix" | grep -Eq '^[a-z][a-z0-9_]{0,39}$'; then
  echo "migrate: NEXUS_DB_ROLE_PREFIX=$role_prefix must match ^[a-z][a-z0-9_]{0,39}\$" >&2
  exit 1
fi
has_prefix_fn=$(psql "$DATABASE_URL" -tAc \
  "SELECT 1 FROM pg_proc WHERE proname = 'nexus_ensure_role_prefix'")
if [ "$has_prefix_fn" = "1" ]; then
  in_force=$(printf "SELECT nexus_ensure_role_prefix(:'prefix');\n" |
    psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -q -tA --single-transaction -v prefix="$role_prefix")
  echo "migrate: service group roles are ${in_force}_*"
elif [ -n "$role_prefix" ] && [ "$role_prefix" != nexus ]; then
  echo "migrate: NEXUS_DB_ROLE_PREFIX needs migration 0038, which is not in $MIGRATIONS_DIR" >&2
  exit 1
fi

echo "migrate: done ($applied newly applied)"
