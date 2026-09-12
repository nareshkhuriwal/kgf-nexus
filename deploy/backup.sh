#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Dumps the nexus database to a timestamped custom-format archive, then exits 0.
#
# Postgres is the sole system of record: every run, attempt, lease, timer and API
# key lives here and nowhere else (ADR-0002). Losing it loses the platform's
# entire history, and until this script existed there was no copy of it at all.
#
# Custom format (-Fc) rather than plain SQL, for two reasons that only matter
# during an incident: pg_restore can restore a SINGLE table out of a custom
# archive (`-t workflow_runs`), and it can restore with -j to use several cores.
# A plain .sql file is one serial psql stream, all or nothing -- exactly the wrong
# shape when the pressure is on and you need one table back in a hurry.
#
# `set -e` is load-bearing: a truncated dump that exits 0 is worse than no dump,
# because it will be trusted. Every failure here must be loud.
set -eu

# ── Connection ────────────────────────────────────────────────────────────
# Two modes on purpose, so the same script is what runs in dev and in
# production. A procedure that is only ever exercised against production is a
# procedure nobody has tested.
#
#   1. DATABASE_URL (or any spelling internal/config/dsn.go accepts) set
#      → run pg_dump on THIS host against that URL.
#   2. nothing set, compose container present
#      → run pg_dump INSIDE the container.
#
# Mode 2 is preferred when there is no URL because the in-container pg_dump
# always matches the server version. A host pg_dump from a newer major produces
# an archive the container's pg_restore cannot read, which is discovered at
# restore time -- the worst possible moment.
PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
PG_USER="${PG_USER:-nexus}"
PG_DB="${PG_DB:-nexus}"

DSN=""
for name in NEXUS_DATABASE_URL DATABASE_URL NEXUS_POSTGRES_DSN NEXUS_DSN; do
  eval "value=\${$name:-}"
  if [ -n "$value" ]; then
    DSN="$value"
    echo "backup: using $name"
    break
  fi
done

OUT_DIR="${BACKUP_DIR:-${1:-./backups}}"
KEEP_DAYS="${BACKUP_KEEP_DAYS:-7}"

if [ -n "$DSN" ]; then
  MODE=host
  command -v pg_dump >/dev/null 2>&1 || {
    echo "backup: pg_dump not found on PATH and a DSN was given" >&2
    exit 1
  }
  run_psql() { psql "$DSN" "$@"; }
else
  MODE=container
  docker inspect "$PG_CONTAINER" >/dev/null 2>&1 || {
    echo "backup: no DSN set and container $PG_CONTAINER does not exist" >&2
    echo "backup: set DATABASE_URL, or start the compose stack" >&2
    exit 1
  }
  echo "backup: no DSN set, dumping inside $PG_CONTAINER"
  run_psql() { docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" "$@"; }
fi

# Same wait as migrate.sh, for the same reason: in compose this script may run
# while Postgres is still doing crash recovery, and "connection refused" is not a
# backup failure worth paging about if it clears in a second.
echo "backup: waiting for postgres to accept connections"
i=1
while :; do
  if run_psql -c 'SELECT 1' >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -ge 60 ]; then
    echo "backup: postgres did not become reachable in 60s" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 1
done

mkdir -p "$OUT_DIR"

# UTC, and sortable. Local time in a filename is ambiguous twice a year and
# unsortable across a DST boundary, which matters precisely when someone is
# picking "the last good backup" from a directory listing.
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
OUT_FILE="$OUT_DIR/nexus-$STAMP.dump"

echo "backup: dumping $PG_DB to $OUT_FILE (custom format)"
# Writing to a .partial name first means a dump killed halfway leaves a file
# nobody will mistake for a complete backup. The rename is the commit point.
if [ "$MODE" = host ]; then
  pg_dump --format=custom --no-owner --no-privileges --file="$OUT_FILE.partial" "$DSN"
else
  # -i, no -t: the archive is binary and must not go through a tty.
  docker exec -i "$PG_CONTAINER" \
    pg_dump --format=custom --no-owner --no-privileges -U "$PG_USER" -d "$PG_DB" \
    > "$OUT_FILE.partial"
fi

# ── Verification ──────────────────────────────────────────────────────────
# A zero-byte or truncated archive is the failure this catches. pg_restore
# --list reads the archive's table of contents, so it fails on a header that is
# not a valid custom-format archive -- which is what a pg_dump killed mid-write,
# or a full disk, actually produces.
if [ ! -s "$OUT_FILE.partial" ]; then
  echo "backup: dump is empty, refusing to keep it" >&2
  rm -f "$OUT_FILE.partial"
  exit 1
fi

if [ "$MODE" = host ]; then
  entries=$(pg_restore --list "$OUT_FILE.partial" | grep -c ';' || true)
else
  # Staged to a file inside the container rather than piped on stdin: a
  # custom-format archive's table of contents lives at the END of the file, so
  # pg_restore must be able to seek and cannot read one from a pipe. The same
  # constraint applies to `pg_restore --list`, which is why this is not simply
  # `docker exec -i ... pg_restore --list /dev/stdin`.
  staged="/tmp/nexus-verify-$STAMP.dump"
  docker exec -i "$PG_CONTAINER" sh -c "cat > $staged" < "$OUT_FILE.partial"
  entries=$(docker exec -i "$PG_CONTAINER" pg_restore --list "$staged" | grep -c ';' || true)
  docker exec -i "$PG_CONTAINER" rm -f "$staged"
fi
if [ "${entries:-0}" -lt 1 ]; then
  echo "backup: dump is not a readable custom-format archive, refusing to keep it" >&2
  rm -f "$OUT_FILE.partial"
  exit 1
fi

mv "$OUT_FILE.partial" "$OUT_FILE"

size=$(wc -c < "$OUT_FILE" | tr -d ' ')
echo "backup: wrote $OUT_FILE ($size bytes, $entries archive entries)"

# ── Pruning ───────────────────────────────────────────────────────────────
# Off by default is the wrong default for a backup directory: unbounded growth
# fills the disk, and a full disk is itself an outage. But deleting the only
# copy is unrecoverable, so the retention is explicit and the script says which
# files it removed rather than doing it silently.
if [ "$KEEP_DAYS" = "0" ]; then
  echo "backup: pruning disabled (BACKUP_KEEP_DAYS=0)"
else
  pruned=0
  # -mtime +N is "older than N days" in both GNU and BSD find, which is the only
  # portability this needs (Linux CI and macOS laptops).
  for old in $(find "$OUT_DIR" -maxdepth 1 -name 'nexus-*.dump' -mtime "+$KEEP_DAYS" 2>/dev/null); do
    echo "backup: pruning $old (older than $KEEP_DAYS days)"
    rm -f "$old"
    pruned=$((pruned + 1))
  done
  echo "backup: done (pruned $pruned older than $KEEP_DAYS days)"
fi
