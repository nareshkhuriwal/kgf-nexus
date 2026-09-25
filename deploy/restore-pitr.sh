#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Point-in-time restore of the Postgres cluster from the pgBackRest repository,
# IN PLACE, then exits 0 once the server has recovered, promoted and is archiving
# again on its new timeline.
#
# Usage:
#   deploy/restore-pitr.sh --time '2026-09-24 14:31:59+00' --yes
#   deploy/restore-pitr.sh --lsn 0/5A0012F8 --yes
#   deploy/restore-pitr.sh --latest --yes          # replay every archived WAL
#   options: --exclusive      stop JUST BEFORE the target instead of at it
#            --report FILE    write phase timings as JSON (default: none)
#
# For the compose PITR overlay (deploy/docker-compose.pitr.yml). On Kubernetes
# the same pgBackRest command runs in the postgres pod; see OPERATIONS §4.4.
# Managed Postgres (Azure Flexible Server, RDS, Cloud SQL) restores through the
# provider instead -- this script does not apply there.
#
# What it does, in order:
#   1. prints the plan and the repository's restorable window, and refuses
#      without --yes: a restore REWRITES the data directory. Everything committed
#      after the target is discarded from the database (it is still in the WAL
#      archive, so a second restore to a later target can get it back).
#   2. stops the postgres container and the pgbackrest sidecar.
#   3. runs `pgbackrest restore --delta --type=time|lsn|default` in a one-off
#      container that mounts EXACTLY the postgres container's volumes
#      (--volumes-from) with the SAME repository settings (the PGBACKREST_*
#      environment is copied from the container, never from a file on disk).
#      pgBackRest picks the newest backup that ends before the target itself.
#   4. starts postgres; it replays WAL from the repository up to the target and
#      promotes (--target-action=promote). Waits for pg_is_in_recovery() = false.
#   5. runs `pgbackrest check` -- the new timeline archives -- and restarts the
#      sidecar.
#
# It does NOT stop the Nexus services: stop them first (the drill does), or they
# will reconnect mid-restore and see a database that moves backwards under them.
# The plan shows how many client connections are open so this is visible.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=deploy/lib/drill-report.sh
. "$HERE/lib/drill-report.sh"

PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
PG_USER="${PG_USER:-nexus}"
PG_DB="${PG_DB:-nexus}"
STANZA="${PGBACKREST_STANZA:-nexus}"
RECOVERY_TIMEOUT="${PITR_RECOVERY_TIMEOUT:-3600}"

TYPE=""
TARGET=""
FORCE=0
EXCLUSIVE=0
REPORT=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --time) TYPE=time; TARGET="$2"; shift 2 ;;
    --lsn) TYPE=lsn; TARGET="$2"; shift 2 ;;
    --latest) TYPE=default; shift ;;
    --exclusive) EXCLUSIVE=1; shift ;;
    --yes|-y) FORCE=1; shift ;;
    --report) REPORT="$2"; shift 2 ;;
    *) echo "restore-pitr: unknown argument $1" >&2; exit 2 ;;
  esac
done
[ -n "$TYPE" ] || { echo "restore-pitr: one of --time, --lsn or --latest is required" >&2; exit 2; }

docker inspect "$PG_CONTAINER" >/dev/null 2>&1 || {
  echo "restore-pitr: container $PG_CONTAINER does not exist" >&2; exit 1; }
IMAGE=$(docker inspect -f '{{.Config.Image}}' "$PG_CONTAINER")
# Checked against the IMAGE, not the container: after a crash the container is not running.
docker run --rm --entrypoint pgbackrest "$IMAGE" version >/dev/null 2>&1 || {
  echo "restore-pitr: $PG_CONTAINER has no pgbackrest -- is the PITR overlay in use?" >&2
  echo "restore-pitr:   docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.pitr.yml up -d" >&2
  exit 1
}
NETWORK=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$PG_CONTAINER" | awk '{print $1}')
PROJECT=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$PG_CONTAINER")
SIDECAR=""
if [ -n "$PROJECT" ]; then
  SIDECAR=$(docker ps -a --filter "label=com.docker.compose.project=$PROJECT" \
    --filter "label=com.docker.compose.service=pgbackrest" --format '{{.Names}}' | head -n 1)
fi

# The repository settings, from the container's own environment: the restore
# must read the SAME repository with the SAME cipher passphrase the server
# archives to. Exported here and passed by NAME (-e VAR), so no secret appears on
# a command line (ps) or in a file.
PGBR_VARS=""
set -f   # a value containing '*' must not glob
for kv in $(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$PG_CONTAINER" | grep '^PGBACKREST_' | tr ' ' '\001'); do
  kv=$(printf '%s' "$kv" | tr '\001' ' ')
  name="${kv%%=*}"
  export "$name=${kv#*=}"
  PGBR_VARS="$PGBR_VARS -e $name"
done
set +f
[ -n "$PGBR_VARS" ] || { echo "restore-pitr: no PGBACKREST_* settings on $PG_CONTAINER" >&2; exit 1; }

running=$(docker inspect -f '{{.State.Running}}' "$PG_CONTAINER")
clients="unknown (server stopped)"
if [ "$running" = true ]; then
  clients=$(docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -tAc \
    "SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()" 2>/dev/null || echo "unknown")
fi

# The restorable window, straight from the repository.
# jq runs in the image (it ships there for the scheduler), not on this host.
# shellcheck disable=SC2086,SC2016
window=$(docker run --rm --network "$NETWORK" --volumes-from "$PG_CONTAINER" --user postgres \
  $PGBR_VARS -e STANZA="$STANZA" --entrypoint /bin/sh "$IMAGE" -c \
  'pgbackrest --stanza="$STANZA" --output=json info | jq -r ".[0] | \"backups=\(.backup | length) oldest_backup_start=\((.backup[0].timestamp.start // 0) | todate) newest_backup_stop=\((.backup[-1].timestamp.stop // 0) | todate) wal=\(.archive[-1].min // \"-\")..\(.archive[-1].max // \"-\")\""' 2>/dev/null ||
  echo "unreadable (check repository settings)")

echo "restore-pitr: PLAN"
echo "  container    : $PG_CONTAINER ($IMAGE)"
echo "  stanza       : $STANZA"
case "$TYPE" in
  time) echo "  target       : time $TARGET$( [ "$EXCLUSIVE" = 1 ] && echo ' (exclusive)')" ;;
  lsn)  echo "  target       : lsn $TARGET$( [ "$EXCLUSIVE" = 1 ] && echo ' (exclusive)')" ;;
  *)    echo "  target       : latest archived WAL" ;;
esac
echo "  repository   : $window"
echo "  clients open : $clients (stop the Nexus services first)"
echo "  sidecar      : ${SIDECAR:-none}"
echo "  action       : REWRITE the data directory in place (pgbackrest restore --delta)"
echo

if [ "$FORCE" != 1 ]; then
  cat >&2 <<EOF
restore-pitr: REFUSING without --yes.

A point-in-time restore rewrites the data directory of $PG_CONTAINER. Every
transaction committed after the target is removed from the database. It stays in
the WAL archive, so restoring again to a LATER target recovers it -- but only
until retention expires it.
EOF
  exit 1
fi

REPORT_WORK="${TMPDIR:-/tmp}/nexus-restore-pitr-$$"
report_init restore-pitr "$REPORT_WORK"
trap 'rm -rf "$REPORT_WORK"' EXIT
report_set_str container "$PG_CONTAINER"
report_set_str target_type "$TYPE"
report_set_str target "$TARGET"

# ── Stop ──────────────────────────────────────────────────────────────────
phase_begin pg_stop
[ -n "$SIDECAR" ] && docker stop -t 30 "$SIDECAR" >/dev/null 2>&1 || true
# A clean shutdown makes postgres archive every COMPLETE segment before exiting.
docker stop -t 120 "$PG_CONTAINER" >/dev/null
phase_end pg_stop

# ── Restore ───────────────────────────────────────────────────────────────
phase_begin pg_restore
set -- --stanza="$STANZA" --delta --log-level-console=info
case "$TYPE" in
  time|lsn)
    set -- "$@" --type="$TYPE" --target="$TARGET" --target-action=promote
    if [ "$EXCLUSIVE" = 1 ]; then set -- "$@" --target-exclusive; fi
    ;;
  default) set -- "$@" --type=default ;;
esac
# shellcheck disable=SC2086
docker run --rm --network "$NETWORK" --volumes-from "$PG_CONTAINER" --user postgres \
  $PGBR_VARS --entrypoint pgbackrest "$IMAGE" "$@" restore
phase_end pg_restore

# ── Recover (WAL replay to the target) and promote ────────────────────────
phase_begin pg_recover
docker start "$PG_CONTAINER" >/dev/null
deadline=$(( $(date +%s) + RECOVERY_TIMEOUT ))
while :; do
  if [ "$(docker inspect -f '{{.State.Running}}' "$PG_CONTAINER")" != true ]; then
    echo "restore-pitr: postgres EXITED during recovery. Last log lines:" >&2
    docker logs --tail 30 "$PG_CONTAINER" >&2
    echo "restore-pitr: a target later than the newest archived WAL fails exactly like this" >&2
    echo "restore-pitr: ('recovery ended before configured recovery target was reached')." >&2
    exit 1
  fi
  state=$(docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -tAc "SELECT pg_is_in_recovery()" 2>/dev/null || true)
  [ "$state" = f ] && break
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "restore-pitr: still in recovery after ${RECOVERY_TIMEOUT}s" >&2
    exit 1
  fi
  sleep 1
done
phase_end pg_recover

# ── Verify ────────────────────────────────────────────────────────────────
phase_begin pg_verify
timeline=$(docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -tAc \
  "SELECT timeline_id FROM pg_control_checkpoint()")
docker exec -i -u postgres "$PG_CONTAINER" pgbackrest --stanza="$STANZA" check
[ -n "$SIDECAR" ] && docker start "$SIDECAR" >/dev/null
phase_end pg_verify
report_set timeline "${timeline:-null}"

echo
echo "restore-pitr: recovered and promoted on timeline $timeline; archiving verified."
echo "restore-pitr: row counts in $PG_DB:"
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -P pager=off -c "
SELECT 'workflow_runs' AS table_name, count(*) FROM workflow_runs
UNION ALL SELECT 'task_attempts', count(*) FROM task_attempts
UNION ALL SELECT 'run_events',    count(*) FROM run_events
UNION ALL SELECT 'workers',       count(*) FROM workers ORDER BY 1"
echo "restore-pitr: take a new backup soon (deploy/pitr-backup.sh diff): the restore"
echo "restore-pitr: point is restorable from the OLD backup + WAL, but a fresh one"
echo "restore-pitr: shortens the next recovery."

report_finish "${REPORT:-${TMPDIR:-/tmp}/nexus-restore-pitr-$(date -u +%Y%m%dT%H%M%SZ).json}" pg_stop pg_verify
