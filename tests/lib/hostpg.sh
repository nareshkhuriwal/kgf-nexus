# shellcheck shell=bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Throwaway databases on an EXISTING PostgreSQL server, for the test and bench
# scripts. SOURCED, not executed.
#
# The project rule is that Postgres comes from the host: the server already
# running on the workstation (Homebrew on 127.0.0.1:5432 by default), or in CI
# the job's service container. No script here starts a Postgres container.
# Each run gets its own database, created here and dropped by the caller's
# cleanup, so runs never see each other's rows or backlog.
#
#   NEXUS_TEST_PG_SERVER   a DSN for the server's maintenance database, as a
#                          role that may CREATE DATABASE (default
#                          postgres://127.0.0.1:5432/postgres?sslmode=disable:
#                          the current OS user over TCP, which a Homebrew
#                          server trusts)
#   NEXUS_TEST_PG_MIN_FREE fail early unless the server has this many
#                          connection slots free (default 10). A shared server
#                          near max_connections otherwise fails tests at
#                          connect with "too many clients", which reads like a
#                          product bug.
#
# Ownership: a database created here carries a COMMENT naming its owner tag,
# and hostpg_drop refuses to drop a database without that comment, so a name
# collision can never drop someone else's database.

HOSTPG_SERVER=${NEXUS_TEST_PG_SERVER:-postgres://127.0.0.1:5432/postgres?sslmode=disable}
HOSTPG_TAG=${HOSTPG_TAG:-org.kgf-nexus.test-db}

hostpg_die() { echo "hostpg: ERROR: $*" >&2; exit 2; }

# hostpg_valid_name NAME: an unquoted-safe PostgreSQL identifier.
hostpg_valid_name() {
  [[ "$1" =~ ^[a-z_][a-z0-9_]{0,62}$ ]]
}

# hostpg_url DSN DB: DSN with its database replaced by DB (query kept).
hostpg_url() {
  local dsn=$1 db=$2 base query=""
  base=${dsn%%\?*}
  [ "$base" != "$dsn" ] && query="?${dsn#*\?}"
  # scheme://authority[/path]: keep scheme://authority, replace the path.
  local rest=${base#*://} scheme=${base%%://*}
  local authority=${rest%%/*}
  echo "${scheme}://${authority}/${db}${query}"
}

# hostpg_container_url DSN: DSN as a container on this machine reaches it.
# Loopback becomes host.docker.internal (Docker Desktop forwards it to the
# host's loopback, so a server listening on localhost only, trusting
# 127.0.0.1, still accepts it; on Linux the compose services map the name to
# host-gateway). A DSN with no user gets the host user it implied: libpq in a
# container would otherwise log in as "root".
hostpg_container_url() {
  local dsn=$1 rest authority
  rest=${dsn#*://}
  authority=${rest%%/*}
  if [[ "$authority" != *@* ]]; then
    dsn="${dsn%%://*}://$(id -un)@${rest}"
  fi
  echo "$dsn" | sed -E 's#@(127\.0\.0\.1|localhost|\[::1\])([:/])#@host.docker.internal\2#'
}

hostpg_psql() { # DSN args...
  # Retried while the server is full: on a server shared with other workloads,
  # "too many clients" is momentary, and a cleanup that gave up on it would
  # leave its database behind.
  local dsn=$1 out rc attempt
  shift
  for attempt in 1 2 3 4 5 6 7 8 9 10; do
    rc=0
    out=$(PGOPTIONS='-c client_min_messages=warning' psql "$dsn" -v ON_ERROR_STOP=1 -qAt "$@" 2>&1) || rc=$?
    if [ "$rc" -ne 0 ] && [[ "$out" == *"too many clients"* ]] && [ "$attempt" -lt 10 ]; then
      sleep 3
      continue
    fi
    break
  done
  if [ "$rc" -ne 0 ]; then
    printf '%s\n' "$out" >&2
    return "$rc"
  fi
  [ -z "$out" ] || printf '%s\n' "$out"
}

# hostpg_check: the server is reachable, the role may create databases, and
# enough connection slots are free.
hostpg_check() {
  command -v psql >/dev/null || hostpg_die "psql not found on PATH (the scripts run against the host's PostgreSQL; install its client)"
  local info
  info=$(hostpg_psql "$HOSTPG_SERVER" -F ' ' -c \
    "SELECT (SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user),
            current_setting('max_connections')::int - (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
            current_setting('server_version')" 2>&1) \
    || hostpg_die "cannot reach PostgreSQL at $(hostpg_redact "$HOSTPG_SERVER"): $info
  Start it (macOS: 'brew services start postgresql@18'; Linux: 'sudo systemctl start postgresql') or set NEXUS_TEST_PG_SERVER.
  To run a compose stack on its bundled Postgres container instead, opt in: NEXUS_BUNDLED_POSTGRES=1 (make up, nexusctl up) or E2E_BUNDLED_POSTGRES=1 (e2e-stack.sh)."
  local can free version
  read -r can free version <<<"$info"
  [ "$can" = t ] || hostpg_die "the role in NEXUS_TEST_PG_SERVER cannot CREATE DATABASE"
  local want=${NEXUS_TEST_PG_MIN_FREE:-10}
  if [ "$free" -lt "$want" ]; then
    hostpg_die "only $free connection slots are free on the server (need $want; NEXUS_TEST_PG_MIN_FREE). Wait for other workloads, or lower the pools (NEXUS_DB_MAX_CONNS)."
  fi
  echo "hostpg: PostgreSQL $version at $(hostpg_redact "$HOSTPG_SERVER"), $free connection slots free"
}

# hostpg_redact DSN: the DSN without its password, for logs.
hostpg_redact() { echo "$1" | sed -E 's#(://[^:/@]+):[^@]*@#\1:***@#'; }

# hostpg_create DB: create an empty database owned by this run. Refuses a name
# that already exists: a leftover from a crashed run of the same tool is
# dropped first only if it carries this tool's tag.
hostpg_create() {
  local db=$1
  hostpg_valid_name "$db" || hostpg_die "database name '$db' must match ^[a-z_][a-z0-9_]{0,62}\$"
  if [ "$(hostpg_psql "$HOSTPG_SERVER" -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" = 1 ]; then
    if [ "$(hostpg_owner "$db")" = "$HOSTPG_TAG" ]; then
      echo "hostpg: dropping $db, left behind by an earlier run"
      hostpg_drop "$db"
    else
      hostpg_die "database $db exists and was not created by $HOSTPG_TAG; refusing to touch it. Choose another name."
    fi
  fi
  hostpg_psql "$HOSTPG_SERVER" -c "CREATE DATABASE $db" >/dev/null
  hostpg_psql "$HOSTPG_SERVER" -c "COMMENT ON DATABASE $db IS '$HOSTPG_TAG'" >/dev/null
}

# hostpg_ensure DB: create DB unless it already exists as this tool's own; a
# long-lived stack database (make up, an e2e project) survives re-runs.
hostpg_ensure() {
  local db=$1
  hostpg_valid_name "$db" || hostpg_die "database name '$db' must match ^[a-z_][a-z0-9_]{0,62}\$"
  if [ "$(hostpg_psql "$HOSTPG_SERVER" -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" = 1 ]; then
    [ "$(hostpg_owner "$db")" = "$HOSTPG_TAG" ] \
      || hostpg_die "database $db exists and was not created by $HOSTPG_TAG; refusing to use it. Choose another name."
    return 0
  fi
  hostpg_psql "$HOSTPG_SERVER" -c "CREATE DATABASE $db" >/dev/null
  hostpg_psql "$HOSTPG_SERVER" -c "COMMENT ON DATABASE $db IS '$HOSTPG_TAG'" >/dev/null
}

hostpg_owner() {
  hostpg_psql "$HOSTPG_SERVER" -c \
    "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = '$1'"
}

# hostpg_drop DB: drop a database this tool created (WITH (FORCE) ends any
# session a failed test left open). Never drops an untagged database.
hostpg_drop() {
  local db=$1
  hostpg_valid_name "$db" || return 0
  [ "$(hostpg_psql "$HOSTPG_SERVER" -c "SELECT 1 FROM pg_database WHERE datname = '$db'")" = 1 ] || return 0
  if [ "$(hostpg_owner "$db")" != "$HOSTPG_TAG" ]; then
    echo "hostpg: NOT dropping $db: it does not carry the tag $HOSTPG_TAG" >&2
    return 0
  fi
  hostpg_psql "$HOSTPG_SERVER" -c "DROP DATABASE IF EXISTS $db WITH (FORCE)" >/dev/null
}

# hostpg_suite_pool_floor: NEXUS_DB_MAX_CONNS caps every pool built through
# store.NewPool, the integration tests' included. Several tests hold one
# connection (a row lock, an open transaction) while the code under test needs
# another, so a cap below 3 deadlocks them until go test's timeout (observed:
# the scheduler's MarkStarted race tests hung 20 minutes at 2). A shared
# server is protected by go test -parallel instead; raise a smaller cap to 3.
hostpg_suite_pool_floor() {
  local cap=${NEXUS_DB_MAX_CONNS:-}
  if [[ "$cap" =~ ^[0-9]+$ ]] && [ "$cap" -lt 3 ]; then
    echo "hostpg: NEXUS_DB_MAX_CONNS=$cap would deadlock integration tests that need a second connection; using 3 for the suite" >&2
    export NEXUS_DB_MAX_CONNS=3
  fi
}

# hostpg_migrate DSN [MIGRATE_SCRIPT] [DIR]: apply migrations with
# deploy/migrate.sh (or the given copy of it), exactly as the migration job does.
hostpg_migrate() {
  local dsn=$1 script=${2:-deploy/migrate.sh} dir=${3:-services/migrations}
  DATABASE_URL=$dsn MIGRATIONS_DIR=$dir PGOPTIONS='-c client_min_messages=warning' sh "$script"
}
