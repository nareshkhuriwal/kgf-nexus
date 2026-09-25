#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Verifies the PITR pipeline end to end; exits nonzero if it is broken or stale.
# Built to be run by a human AND wired into monitoring (the exit code is the
# signal; the last line is one JSON object).
#
# Usage:
#   deploy/pitr-verify.sh [--no-verify]
#
# Checks, each of which has failed silently somewhere in the industry:
#   1. archive_command works NOW       `pgbackrest check`: forces a WAL switch and
#                                       waits for that segment to reach the repo.
#   2. archiving is not failing        pg_stat_archiver: last_failed_time must not
#                                       be newer than last_archived_time.
#   3. archive lag                      seconds since the last successful archive
#                                       (on a quiet database this approaches
#                                       archive_timeout -- that IS the RPO).
#   4. a recent base backup exists      newest backup younger than
#                                       PITR_MAX_BACKUP_AGE_SECONDS (default 2 days:
#                                       one missed daily diff is tolerated, two is not).
#   5. the repository is intact         `pgbackrest verify`: re-reads and checksums
#                                       every backup file and WAL segment. The slow
#                                       one; --no-verify skips it for frequent probes.
#
# Environment: PG_CONTAINER, PG_USER, PITR_EXEC (see pitr-backup.sh).
set -eu

PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
PG_USER="${PG_USER:-nexus}"
STANZA="${PGBACKREST_STANZA:-nexus}"
EXEC="${PITR_EXEC:-docker exec -i -u postgres $PG_CONTAINER}"
MAX_AGE="${PITR_MAX_BACKUP_AGE_SECONDS:-172800}"
MAX_LAG="${PITR_MAX_ARCHIVE_LAG_SECONDS:-900}"

VERIFY=1
for arg in "$@"; do
  case "$arg" in
    --no-verify) VERIFY=0 ;;
    *) echo "pitr-verify: unknown argument $arg" >&2; exit 2 ;;
  esac
done

FAIL=0
fail() { FAIL=$((FAIL + 1)); echo "  FAIL  $1" >&2; }
ok() { echo "  PASS  $1"; }

# shellcheck disable=SC2086
pgbr() { $EXEC pgbackrest --stanza="$STANZA" "$@"; }
# shellcheck disable=SC2086
sql() { $EXEC psql -U "$PG_USER" -d postgres -tAc "$1"; }

echo "pitr-verify: [1] archive_command round trip (pgbackrest check)"
if pgbr check >/dev/null 2>&1; then ok "a forced WAL segment reached the repository"; check=true
else fail "pgbackrest check failed -- archiving is BROKEN; run it by hand for the error"; check=false; fi

echo "pitr-verify: [2,3] pg_stat_archiver"
row=$(sql "SELECT archived_count, failed_count,
                  coalesce(extract(epoch FROM now() - last_archived_time)::int, -1),
                  (last_failed_time IS NOT NULL AND (last_archived_time IS NULL OR last_failed_time > last_archived_time))
             FROM pg_stat_archiver" | tr '|' ' ')
# shellcheck disable=SC2086
set -- $row
archived="$1"; failed="$2"; lag="$3"; failing="$4"
if [ "$failing" = t ]; then fail "the most recent archive attempt FAILED ($failed failures total)"
else ok "archiver healthy ($archived archived, $failed historical failures)"; fi
if [ "$lag" -lt 0 ]; then fail "nothing has ever been archived"
elif [ "$lag" -gt "$MAX_LAG" ]; then fail "last successful archive ${lag}s ago (> ${MAX_LAG}s)"
else ok "last successful archive ${lag}s ago"; fi

echo "pitr-verify: [4] base backups"
# shellcheck disable=SC2016
summary=$($EXEC sh -c 'pgbackrest --stanza="$1" --output=json info | jq -r "
  .[0] | [ (.backup | length),
           ((.backup | map(.timestamp.stop) | max) // 0),
           ((.backup[0].timestamp.start) // 0),
           (.status.message) ] | @tsv"' sh "$STANZA")
count=$(printf '%s' "$summary" | cut -f1)
newest=$(printf '%s' "$summary" | cut -f2)
oldest=$(printf '%s' "$summary" | cut -f3)
now=$(date +%s)
age=$((now - newest))
if [ "$count" -eq 0 ]; then fail "no base backup exists -- nothing is restorable yet"
elif [ "$age" -gt "$MAX_AGE" ]; then fail "newest base backup is ${age}s old (> ${MAX_AGE}s)"
else ok "$count backup(s); newest ${age}s old; PITR window opens at $(date -u -r "$oldest" +%FT%TZ 2>/dev/null || date -u -d "@$oldest" +%FT%TZ)"; fi

verified=skipped
if [ "$VERIFY" = 1 ]; then
  echo "pitr-verify: [5] repository integrity (pgbackrest verify)"
  if pgbr verify >/dev/null 2>&1; then ok "every backup file and WAL segment checksums clean"; verified=true
  else fail "pgbackrest verify reported problems -- run it by hand for the list"; verified=false; fi
fi

echo
if [ "$FAIL" -gt 0 ]; then echo "pitr-verify: $FAIL check(s) FAILED" >&2; else echo "pitr-verify: all checks passed"; fi
printf '{"check":%s,"archiver_failing":%s,"archive_lag_seconds":%s,"backups":%s,"newest_backup_age_seconds":%s,"window_start_epoch":%s,"verify":"%s","failures":%s}\n' \
  "$check" "$( [ "$failing" = t ] && echo true || echo false)" "$lag" "$count" "$age" "$oldest" "$verified" "$FAIL"
[ "$FAIL" -eq 0 ]
