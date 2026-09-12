#!/bin/sh
# Restores a backup.sh archive into a target database, then exits 0.
#
# Usage:
#   deploy/restore.sh <dump-file> [target-db] [--yes]
#
# This is the script that runs when something has already gone wrong, so it is
# written to be un-surprising rather than clever: it prints the full plan, it
# refuses to touch a database that already has tables unless told twice, and it
# never guesses the target.
#
# The refusal is the important part. A restore over a live database is
# unrecoverable -- pg_restore --clean drops the real tables before it writes the
# archive's, so a mistyped target destroys current data and there is no undo. The
# only protection is to make the destructive path require an explicit statement of
# intent.
#
# `set -e` is load-bearing: a half-restored database that exits 0 would be
# mistaken for a recovered one.
set -eu

PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
PG_USER="${PG_USER:-nexus}"
PG_ADMIN_DB="${PG_ADMIN_DB:-postgres}"
FORCE="${FORCE:-0}"

DUMP=""
TARGET=""
for arg in "$@"; do
  case "$arg" in
    --yes|-y) FORCE=1 ;;
    --*) echo "restore: unknown flag $arg" >&2; exit 2 ;;
    *)
      if [ -z "$DUMP" ]; then DUMP="$arg"
      elif [ -z "$TARGET" ]; then TARGET="$arg"
      else echo "restore: unexpected argument $arg" >&2; exit 2
      fi
      ;;
  esac
done

if [ -z "$DUMP" ]; then
  echo "restore: usage: $0 <dump-file> [target-db] [--yes]" >&2
  exit 2
fi
[ -f "$DUMP" ] || { echo "restore: $DUMP does not exist" >&2; exit 1; }
[ -s "$DUMP" ] || { echo "restore: $DUMP is empty" >&2; exit 1; }

# ── Connection ────────────────────────────────────────────────────────────
# Same two modes as backup.sh. The target database name is deliberately NOT read
# from the DSN's path when a target is given on the command line: the common
# recovery move is "restore last night's dump into a scratch database and look at
# it", and that must not require editing a DSN.
DSN=""
for name in NEXUS_DATABASE_URL DATABASE_URL NEXUS_POSTGRES_DSN NEXUS_DSN; do
  eval "value=\${$name:-}"
  if [ -n "$value" ]; then
    DSN="$value"
    echo "restore: using $name"
    break
  fi
done

if [ -n "$DSN" ]; then
  MODE=host
  command -v pg_restore >/dev/null 2>&1 || {
    echo "restore: pg_restore not found on PATH and a DSN was given" >&2
    exit 1
  }
  # Split the DSN into "everything before the database name" and "the query
  # string", so the target database can be swapped without hand-editing a URL.
  # Two things force this to be parameter expansion rather than sed: the query
  # string (sslmode=require, which production needs) must survive the swap, and
  # BSD sed has no optional-group operator, so the obvious one-liner silently
  # matched nothing on macOS and left the admin connection pointed at the wrong
  # database.
  without_query="${DSN%%\?*}"
  query=""
  case "$DSN" in
    *\?*) query="?${DSN#*\?}" ;;
  esac
  base="${without_query%/*}"
  if [ -z "$TARGET" ]; then
    TARGET="${without_query##*/}"
  fi
  [ -n "$TARGET" ] || { echo "restore: DSN names no database and no target was given" >&2; exit 2; }
  TARGET_DSN="$base/$TARGET$query"
  ADMIN_DSN="$base/$PG_ADMIN_DB$query"
  psql_target() { psql "$TARGET_DSN" "$@"; }
  psql_admin() { psql "$ADMIN_DSN" "$@"; }
else
  MODE=container
  docker inspect "$PG_CONTAINER" >/dev/null 2>&1 || {
    echo "restore: no DSN set and container $PG_CONTAINER does not exist" >&2
    exit 1
  }
  [ -n "$TARGET" ] || TARGET="${PG_DB:-nexus}"
  echo "restore: no DSN set, restoring inside $PG_CONTAINER"
  psql_target() { docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$TARGET" "$@"; }
  psql_admin() { docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_ADMIN_DB" "$@"; }
fi

echo "restore: waiting for postgres to accept connections"
i=1
while :; do
  if psql_admin -c 'SELECT 1' >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -ge 60 ]; then
    echo "restore: postgres did not become reachable in 60s" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 1
done

# ── Target inspection ─────────────────────────────────────────────────────
exists=$(psql_admin -tAc \
  "SELECT 1 FROM pg_database WHERE datname = '$TARGET'" || true)
tables=0
if [ "$exists" = "1" ]; then
  tables=$(psql_target -tAc \
    "SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'" || echo 0)
fi

dump_size=$(wc -c < "$DUMP" | tr -d ' ')

echo
echo "restore: PLAN"
echo "  archive      : $DUMP ($dump_size bytes)"
echo "  mode         : $MODE"
echo "  target db    : $TARGET"
if [ "$exists" = "1" ]; then
  echo "  target state : EXISTS, $tables table(s) in schema public"
else
  echo "  target state : does not exist, will be created"
fi
if [ "$tables" -gt 0 ]; then
  echo "  action       : DROP and recreate every object in the archive (DESTRUCTIVE)"
else
  echo "  action       : load the archive into an empty database"
fi
echo

# The guard. A non-empty target is the one case where getting this wrong destroys
# data that exists nowhere else, so it needs a deliberate act, not a -f typed out
# of habit on every invocation.
if [ "$tables" -gt 0 ] && [ "$FORCE" != "1" ]; then
  cat >&2 <<EOF
restore: REFUSING to restore over $TARGET -- it already has $tables table(s).

This would destroy the data currently in $TARGET, and nothing here can put it
back. If that is genuinely what you want:

  1. take a backup of $TARGET FIRST:  deploy/backup.sh
  2. stop the services writing to it, or the restore races them
  3. re-run with --yes (or FORCE=1)

To inspect a backup without risking live data, restore into a new database:

  deploy/restore.sh $DUMP nexus_restore_check
EOF
  exit 1
fi

if [ "$exists" != "1" ]; then
  echo "restore: creating database $TARGET"
  psql_admin -v ON_ERROR_STOP=1 -q -c "CREATE DATABASE \"$TARGET\""
fi

echo "restore: restoring into $TARGET"
# --clean --if-exists only when there is something to clean: on a fresh database
# every DROP would emit a notice for an object that was never there, which buries
# the errors that matter in noise.
clean=""
if [ "$tables" -gt 0 ]; then
  clean="--clean --if-exists"
fi

# No --exit-on-error. pg_restore reports non-fatal errors for objects the dump
# references but does not own (roles, extensions owned by superuser), and aborting
# on those would leave the database half-loaded when the data itself restored
# perfectly. The row-count check below is what actually decides whether the
# restore worked -- see docs/OPERATIONS.md.
if [ "$MODE" = host ]; then
  # shellcheck disable=SC2086
  pg_restore $clean --no-owner --no-privileges --dbname="$TARGET_DSN" "$DUMP" || rc=$?
else
  # Copied into the container instead of piped: a custom-format archive keeps its
  # table of contents at the end of the file, so pg_restore seeks and cannot read
  # one from a pipe. Piping it "works" only for plain-format dumps, and failing
  # over to that would silently give up selective and parallel restore -- the
  # reason for choosing custom format in the first place.
  staged="/tmp/nexus-restore-$$.dump"
  docker exec -i "$PG_CONTAINER" sh -c "cat > $staged" < "$DUMP"
  # shellcheck disable=SC2086
  docker exec -i "$PG_CONTAINER" \
    pg_restore $clean --no-owner --no-privileges -U "$PG_USER" -d "$TARGET" \
    "$staged" || rc=$?
  docker exec -i "$PG_CONTAINER" rm -f "$staged"
fi

if [ "${rc:-0}" != "0" ]; then
  echo "restore: pg_restore exited $rc -- non-fatal object errors are normal, but CHECK THE OUTPUT ABOVE" >&2
fi

# ── Verification ──────────────────────────────────────────────────────────
# Reporting the row counts is not decoration: "pg_restore finished" is not the
# same claim as "the data is there", and the only way to tell them apart is to
# count. An operator comparing these against the source is the documented
# acceptance test (docs/OPERATIONS.md).
echo
echo "restore: row counts in $TARGET"
psql_target -v ON_ERROR_STOP=1 -P pager=off <<'SQL'
SELECT 'workflow_specs' AS table_name, count(*) FROM workflow_specs
UNION ALL SELECT 'workflow_runs', count(*) FROM workflow_runs
UNION ALL SELECT 'task_nodes',    count(*) FROM task_nodes
UNION ALL SELECT 'task_attempts', count(*) FROM task_attempts
UNION ALL SELECT 'run_events',    count(*) FROM run_events
UNION ALL SELECT 'workers',       count(*) FROM workers
UNION ALL SELECT 'api_keys',      count(*) FROM api_keys
ORDER BY 1;
SQL

echo
echo "restore: done. Compare the counts above against the source database before"
echo "restore: pointing any service at $TARGET."
