#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# The base-backup cadence for continuous archiving, run as a sidecar next to
# Postgres (compose: the `pgbackrest` service; Helm: the `pgbackrest` container
# in the postgres pod). WAL archiving itself is NOT done here -- Postgres's own
# archive_command pushes every segment. This process only:
#
#   1. creates the stanza (idempotent) and runs `pgbackrest check`, which forces
#      a WAL switch and waits for it to land in the repository -- so a broken
#      archive_command is reported at startup instead of discovered at restore;
#   2. takes a FULL backup when none exists or the newest is older than
#      PITR_FULL_INTERVAL_SECONDS, otherwise a DIFF when the newest backup of any
#      type is older than PITR_DIFF_INTERVAL_SECONDS. Retention (expire) runs
#      automatically after each backup, per the PGBACKREST_REPO1_RETENTION_* knobs;
#   3. enforces the archive-age bound: forces a WAL switch when nothing has been
#      archived for PITR_WAL_SWITCH_SECONDS (see wal_guard for why archive_timeout
#      alone was not enough).
#
# Why state-based, not cron: the decision reads the REPOSITORY (`pgbackrest info`),
# so a restarted pod neither skips a due backup nor takes a duplicate, and two
# replicas of this loop cannot both decide a backup is due (pgBackRest takes a
# repository lock; the loser logs and retries next poll).
#
# The result of every attempt is written to PITR_STATUS_FILE as one JSON line; the
# container healthcheck and deploy/pitr-verify.sh read it. A backup that has not
# SUCCEEDED within 2x the diff interval turns the container unhealthy -- the
# "cron job that fails silently for three weeks" failure OPERATIONS §5 warns about.
set -eu

STANZA="${PGBACKREST_STANZA:-nexus}"
FULL_EVERY="${PITR_FULL_INTERVAL_SECONDS:-604800}"   # weekly full
DIFF_EVERY="${PITR_DIFF_INTERVAL_SECONDS:-86400}"    # daily differential
POLL="${PITR_POLL_SECONDS:-300}"
# 0 disables the archive-age guard (see wal_guard below). Match archive_timeout.
SWITCH_EVERY="${PITR_WAL_SWITCH_SECONDS:-60}"
STATUS_FILE="${PITR_STATUS_FILE:-/var/lib/pgbackrest/scheduler.status}"
SOCKET="${PGBACKREST_PG1_SOCKET_PATH:-/var/run/postgresql}"
PGUSER_="${PGBACKREST_PG1_USER:-${POSTGRES_USER:-postgres}}"
PGDB_="${PGBACKREST_PG1_DATABASE:-postgres}"

log() { printf '%s nexus-pitr-scheduler: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }
ago() { if [ "$1" -eq 0 ]; then echo never; else echo "$(( $(date -u +%s) - $1 ))s ago"; fi; }

status() {
  # $1 result, $2 type, $3 detail. Written atomically: a healthcheck must never
  # read half a line.
  printf '{"at_epoch":%s,"result":"%s","type":"%s","detail":"%s"}\n' \
    "$(date -u +%s)" "$1" "$2" "$3" > "$STATUS_FILE.tmp"
  mv "$STATUS_FILE.tmp" "$STATUS_FILE"
}

if [ "${1:-}" = "healthcheck" ]; then
  # Healthy = the last attempt succeeded and is recent enough. Before the first
  # attempt there is no status file; the healthcheck's start_period covers that.
  [ -s "$STATUS_FILE" ] || exit 1
  last=$(jq -r '.at_epoch' "$STATUS_FILE")
  result=$(jq -r '.result' "$STATUS_FILE")
  now=$(date -u +%s)
  [ "$result" = "ok" ] || exit 1
  [ $((now - last)) -le $((DIFF_EVERY * 2 + POLL)) ] || exit 1
  exit 0
fi

stop=0
trap 'stop=1' TERM INT

nap() {
  # Interruptible sleep: `sleep` in the foreground would delay SIGTERM handling
  # by up to POLL seconds, which is longer than most termination grace periods.
  sleep "$1" &
  wait $! 2>/dev/null || true
}

log "stanza=$STANZA full_every=${FULL_EVERY}s diff_every=${DIFF_EVERY}s poll=${POLL}s"

# ── 1. Wait for Postgres, then stanza-create + check ──────────────────────
until pg_isready -q -h "$SOCKET" -U "$PGUSER_" -d "$PGDB_"; do
  [ "$stop" = 1 ] && exit 0
  log "waiting for postgres on $SOCKET"
  nap 2
done

# Backoff from 5s, not a flat POLL: on a FRESH volume the image's entrypoint runs
# a temporary initdb server, then restarts the real one, and the first check
# routinely lands in that restart ("the database system is in recovery mode").
# Waiting a full poll interval for that would leave the first minutes of a new
# deployment unarchived for no reason.
backoff=5
until pgbackrest --stanza="$STANZA" stanza-create && pgbackrest --stanza="$STANZA" check; do
  [ "$stop" = 1 ] && exit 0
  status error stanza "stanza-create or check failed; see log"
  log "stanza-create/check failed; retrying in ${backoff}s"
  nap "$backoff"
  backoff=$((backoff * 2))
  [ "$backoff" -le "$POLL" ] || backoff="$POLL"
done
log "stanza $STANZA ready; archive_command verified by pgbackrest check"

# ── 2. Backup cadence, and the archive-age guard ──────────────────────────
# The guard: if nothing has been archived for PITR_WAL_SWITCH_SECONDS, force a
# segment switch. archive_timeout is supposed to do exactly this, and usually
# does -- but the PITR drill measured it NOT firing for minutes after a server
# start or promotion (OPERATIONS §4.6), which is precisely when an operator
# assumes the RPO bound holds. pg_switch_wal() is a no-op when no WAL was written
# since the last switch, so on an idle database this costs nothing.
wal_guard() {
  [ "$SWITCH_EVERY" -gt 0 ] || return 0
  age=$(psql -h "$SOCKET" -U "$PGUSER_" -d "$PGDB_" -tAqc \
    "SELECT extract(epoch FROM now() - greatest(last_archived_time, pg_postmaster_start_time()))::int FROM pg_stat_archiver" 2>/dev/null || echo "")
  case "$age" in ''|*[!0-9]*) return 0 ;; esac
  if [ "$age" -ge "$SWITCH_EVERY" ]; then
    psql -h "$SOCKET" -U "$PGUSER_" -d "$PGDB_" -tAqc "SELECT pg_switch_wal()" >/dev/null 2>&1 || true
  fi
}

# With the guard on, look every 10s (one tiny query), so the worst case is
# SWITCH_EVERY + 10s + the push itself. Measured: OPERATIONS §4.6.
TICK="$POLL"
if [ "$SWITCH_EVERY" -gt 0 ] && [ "$POLL" -gt 10 ]; then TICK=10; fi
next_backup_check=0
while [ "$stop" = 0 ]; do
  wal_guard
  now=$(date -u +%s)
  if [ "$now" -lt "$next_backup_check" ]; then nap "$TICK"; continue; fi
  next_backup_check=$((now + POLL))
  if ! info=$(pgbackrest --stanza="$STANZA" --output=json info 2>/dev/null); then
    status error info "pgbackrest info failed"
    log "pgbackrest info failed; retrying in ${POLL}s"
    nap "$TICK"
    continue
  fi
  last_full=$(printf '%s' "$info" | jq '[.[0].backup[]? | select(.type=="full") | .timestamp.stop] | max // 0')
  last_any=$(printf '%s' "$info" | jq '[.[0].backup[]? | .timestamp.stop] | max // 0')

  type=""
  if [ "$last_full" -eq 0 ] || [ $((now - last_full)) -ge "$FULL_EVERY" ]; then
    type="full"
  elif [ $((now - last_any)) -ge "$DIFF_EVERY" ]; then
    type="diff"
  fi

  if [ -n "$type" ]; then
    log "taking $type backup (last full: $(ago "$last_full"), last any: $(ago "$last_any"))"
    started=$(date -u +%s)
    if pgbackrest --stanza="$STANZA" --type="$type" backup; then
      status ok "$type" "took $(( $(date -u +%s) - started ))s"
      log "$type backup ok"
    else
      status error "$type" "backup failed; see log"
      log "$type backup FAILED"
    fi
  elif [ ! -s "$STATUS_FILE" ]; then
    # A restart with a fresh backup already in the repository: record that the
    # repository is current so the healthcheck does not report a false failure.
    status ok none "repository current at startup"
    log "no backup due (last full: $(ago "$last_full"), last any: $(ago "$last_any"))"
  fi
  nap "$TICK"
done
log "stopping"
