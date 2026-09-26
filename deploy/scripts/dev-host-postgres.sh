#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Prepare the HOST machine's PostgreSQL for the local Kubernetes dev
# environment (deploy/terraform/environments/dev).
#
# Development uses the Postgres already running on the workstation (for
# example Homebrew postgresql on 127.0.0.1:5432, shared with the other KGF
# projects) instead of a Postgres container or StatefulSet. Pods reach it at
# host.docker.internal.
#
# Idempotent. Creates, if missing:
#   - a login role (default nexus_dev) with CREATEROLE, because migration 0005
#     creates the shared nexus_* service roles;
#   - a database (default nexus_dev) owned by that role;
#   - a random alphanumeric password in .dev-postgres/password (gitignored,
#     mode 0600), reused on later runs.
# It never touches any other database or role.
#
# Prints the password file path on success.
set -euo pipefail

PGHOST=${NEXUS_DEV_PG_ADMIN_HOST:-127.0.0.1}
PGPORT=${NEXUS_DEV_PG_PORT:-5432}
ADMIN_DB=${NEXUS_DEV_PG_ADMIN_DB:-postgres}
ROLE=${NEXUS_DEV_PG_USER:-nexus_dev}
DB=${NEXUS_DEV_PG_DATABASE:-nexus_dev}
DIR=${NEXUS_DEV_PG_DIR:-.dev-postgres}

case "$ROLE$DB" in
  *[!a-z0-9_]*) echo "dev-host-postgres: role and database names must match [a-z0-9_]" >&2; exit 2 ;;
esac

if ! psql -h "$PGHOST" -p "$PGPORT" -d "$ADMIN_DB" -Atqc 'select 1' >/dev/null 2>&1; then
  echo "dev-host-postgres: cannot reach Postgres at $PGHOST:$PGPORT as $(id -un)." >&2
  echo "  Start it (e.g. 'brew services start postgresql@18') or set NEXUS_DEV_PG_ADMIN_HOST/PORT." >&2
  exit 1
fi

mkdir -p "$DIR"
chmod 700 "$DIR"
if [ ! -s "$DIR/password" ]; then
  (umask 077; LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32 >"$DIR/password")
fi
chmod 600 "$DIR/password"
PW=$(cat "$DIR/password")

psql -h "$PGHOST" -p "$PGPORT" -d "$ADMIN_DB" -v ON_ERROR_STOP=1 -q \
  -v role="$ROLE" -v pw="$PW" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN CREATEROLE PASSWORD %L', :'role', :'pw')
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'role') \gexec
SELECT format('ALTER ROLE %I LOGIN CREATEROLE PASSWORD %L', :'role', :'pw') \gexec
SQL

if ! psql -h "$PGHOST" -p "$PGPORT" -d "$ADMIN_DB" -Atqc \
     "select 1 from pg_database where datname = '$DB'" | grep -q 1; then
  psql -h "$PGHOST" -p "$PGPORT" -d "$ADMIN_DB" -v ON_ERROR_STOP=1 -qc \
    "CREATE DATABASE \"$DB\" OWNER \"$ROLE\""
fi

echo "$DIR/password"
