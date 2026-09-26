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

WAIT="${NEXUS_MIGRATE_WAIT_SECONDS:-60}"
case "$WAIT" in ''|*[!0-9]*) WAIT=60 ;; esac

# The same URL with its database replaced by the maintenance database, for
# creating a missing one.
maintenance_url() {
  base=${DATABASE_URL%%\?*}
  query=""
  [ "$base" != "$DATABASE_URL" ] && query="?${DATABASE_URL#*\?}"
  echo "${base%/*}/postgres${query}"
}
db_name() {
  base=${DATABASE_URL%%\?*}
  echo "${base##*/}"
}
# Where the server is, without credentials, for messages.
server_of() {
  echo "$DATABASE_URL" | sed -E 's#^[a-z]+://([^@/]*@)?([^/?]*).*#\2#'
}

echo "migrate: waiting for postgres at $(server_of) to accept connections"
i=0
created=0
while :; do
  i=$((i + 1))
  err=$(psql "$DATABASE_URL" -c 'SELECT 1' 2>&1 >/dev/null) && break
  # A reachable server without the database: create it once, when allowed
  # (the stack's own database on the host's PostgreSQL).
  if [ "$created" = 0 ] && [ "${NEXUS_MIGRATE_CREATE_DATABASE:-false}" = true ] &&
     printf '%s' "$err" | grep -q 'does not exist'; then
    db=$(db_name)
    case "$db" in
      ''|*[!a-z0-9_]*) echo "migrate: database name '$db' must match [a-z0-9_]+ to be created here" >&2; exit 1 ;;
    esac
    echo "migrate: creating database $db"
    psql "$(maintenance_url)" -v ON_ERROR_STOP=1 -qc "CREATE DATABASE $db" || {
      echo "migrate: could not create database $db; create it yourself, or grant this role CREATEDB" >&2
      exit 1
    }
    created=1
    continue
  fi
  if [ "$i" -ge "$WAIT" ]; then
    echo "migrate: PostgreSQL at $(server_of) did not accept connections in ${WAIT}s:" >&2
    printf '  %s\n' "$err" | head -3 >&2
    cat >&2 <<'HINT'
migrate: The stack uses the HOST machine's PostgreSQL by default. Start it, e.g.
           macOS (Homebrew):  brew services start postgresql@18
           Linux:             sudo systemctl start postgresql
         (on Linux it must also listen on the Docker bridge and allow it in
         pg_hba.conf), or point NEXUS_HOST_DATABASE_URL at a reachable server.
         To use the bundled Postgres container instead, opt in explicitly:
           NEXUS_BUNDLED_POSTGRES=1 make up      NEXUS_BUNDLED_POSTGRES=1 nexusctl up
           docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.bundled-postgres.yml up
HINT
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
