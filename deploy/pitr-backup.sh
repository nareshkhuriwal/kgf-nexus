#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Takes a pgBackRest base backup NOW, then exits 0.
#
# Usage:
#   deploy/pitr-backup.sh [full|diff|incr]        (default: full)
#
# The pgbackrest sidecar already takes full/diff backups on a cadence; this is
# for "before a risky change" and for the drills. Continuous WAL archiving does
# not depend on it -- but a PITR target is only reachable from a base backup that
# ENDED before it, so the first base backup is what opens the recovery window.
#
# Where it runs:
#   default        docker exec into $PG_CONTAINER (compose PITR overlay)
#   PITR_EXEC=...  any command prefix that runs pgbackrest next to the data dir,
#                  e.g. Kubernetes:
#                    PITR_EXEC="kubectl -n nexus exec -i postgres-0 -c pgbackrest --"
#
# Prints one JSON line last (label, type, sizes, seconds) for automation, after
# the human-readable output. Retention (expire) runs automatically afterwards,
# per PGBACKREST_REPO1_RETENTION_*.
set -eu

TYPE="${1:-full}"
case "$TYPE" in full|diff|incr) ;; *) echo "pitr-backup: type must be full, diff or incr" >&2; exit 2 ;; esac

PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
STANZA="${PGBACKREST_STANZA:-nexus}"
EXEC="${PITR_EXEC:-docker exec -i -u postgres $PG_CONTAINER}"

# shellcheck disable=SC2086
pgbr() { $EXEC pgbackrest --stanza="$STANZA" "$@"; }

start=$(date +%s)
echo "pitr-backup: $TYPE backup of stanza $STANZA"
pgbr --type="$TYPE" backup
end=$(date +%s)

# The newest backup's record, via jq inside the same environment (the image ships
# jq; this host need not).
# shellcheck disable=SC2086,SC2016
$EXEC sh -c 'pgbackrest --stanza="$1" --output=json info | jq -c --argjson secs "$2" "
  .[0].backup[-1] | {label, type,
    database_bytes: .info.size, repo_bytes: .info.repository.delta,
    wal_start: .archive.start, wal_stop: .archive.stop,
    started_at: (.timestamp.start | todate), stopped_at: (.timestamp.stop | todate),
    seconds: \$secs}"' sh "$STANZA" "$((end - start))"
