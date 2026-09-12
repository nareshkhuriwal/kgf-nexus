#!/bin/sh
# Mirrors the MinIO result bucket to a timestamped directory, then exits 0.
#
# Postgres is the system of record (ADR-0002), but it is not the whole story.
# Task outputs above RESULT_STORE_INLINE_THRESHOLD_BYTES (64 KiB) do NOT live in
# Postgres -- the row holds only a reference, and the bytes live in the result
# store (ADR-0008). Backing up Postgres alone therefore produces a history whose
# large output_refs point at objects that may no longer exist: the run list looks
# complete, and every attempt to read one of those outputs fails. This script is
# the other half of deploy/backup.sh, and neither is a backup of the platform on
# its own.
#
# `mc mirror` rather than a filesystem copy of the MinIO data directory. A raw
# copy of /data captures MinIO's internal layout (xl.meta, erasure shards, the
# version of the format that release writes), which restores only into a byte-
# compatible MinIO of the same lineage. `mc mirror` speaks S3, so the output is
# plain objects that restore into any S3-compatible target -- a different MinIO
# release, or real S3 on the day production moves there. The backup should not
# encode a bet on the storage engine staying the same.
#
# `set -e` is load-bearing, for the same reason it is in backup.sh: a partial
# mirror that exits 0 is worse than no mirror, because it will be trusted. The
# object count is verified against the source before the mirror is committed.
set -eu

# ── Consistency ordering: MinIO FIRST, then Postgres ──────────────────────
# Two stores cannot be snapshotted atomically. There is no shared transaction,
# no shared clock, and the stack keeps running while both are read, so whichever
# is captured second includes writes the first one missed. That skew is not
# avoidable; it is only steerable. The choice is which of two failure modes it
# produces.
#
#   dangling output_ref -- the row exists, the object does not.
#   orphan object       -- the object exists, no row references it.
#
# These are NOT equally bad, and the asymmetry is the whole argument.
#
# A dangling ref is silent, unrecoverable data loss that ASSERTS it is not lost.
# The run shows SUCCEEDED, the UI renders an output link, and a downstream task
# retried after the restore fails reading an input the database swears exists.
# Nothing can reconstruct those bytes: the producing attempt is over, and the
# object was content-addressed, so there is no other copy to fall back to.
#
# An orphan object is wasted disk. ADR-0008 ALREADY mandates a reachability-keyed
# GC sweep over live output_refs, so orphans are a condition the design expects
# and already has a cleaner for. Nothing breaks; a sweep reclaims the space.
#
# So: capture MinIO first, Postgres second. The skew window then contains only
# writes that land in Postgres after the object mirror closed -- and the ordering
# in agent/internal/resultstore.Put is what makes that a guarantee rather than a
# hope: Put returns the ResultRef only AFTER PutObject succeeds, so the object is
# durable strictly before the row naming it is written. An attempt row captured
# in the later Postgres dump therefore references an object that already existed
# when the earlier mirror ran, so the mirror caught it. The ordering converts an
# unavoidable skew into orphans only, and never into dangling refs.
#
# Reversing the order inverts exactly this, which is why the order is not
# arbitrary and must not be "optimised" by running the two in parallel. Running
# them concurrently gives up the guarantee entirely: with no ordering between
# them, a row written during the overlap can reference an object the mirror had
# already passed, which is the dangling ref this ordering exists to prevent.
#
# deploy/backup-all.sh runs both in this order. Running this script alone is
# supported (it is the mirror half), but the pair is what constitutes a backup.
#
# What this does NOT give you: a consistent point in time. Neither half is an
# instant, and objects written during the mirror may or may not be included. The
# claim is only the directional one above -- no dangling refs, orphans possible.

# ── Connection ────────────────────────────────────────────────────────────
# Two modes, mirroring backup.sh exactly, so the same script runs in dev and in
# production. A procedure only ever exercised against production is a procedure
# nobody has tested.
#
#   1. RESULT_STORE_ENDPOINT (or NEXUS_RESULT_STORE_ENDPOINT) set
#      → run mc on THIS host against that endpoint.
#   2. nothing set, compose container present
#      → run mc INSIDE the minio container, which ships the binary.
#
# Mode 2 is preferred when there is no endpoint because the container's mc is
# always version-matched to the server, and because it needs no mc on the host.
MINIO_CONTAINER="${MINIO_CONTAINER:-nexus-minio-1}"
BUCKET="${RESULT_STORE_BUCKET:-${NEXUS_RESULT_BUCKET:-nexus-results}}"

ENDPOINT=""
for name in RESULT_STORE_ENDPOINT NEXUS_RESULT_STORE_ENDPOINT NEXUS_S3_ENDPOINT; do
  eval "value=\${$name:-}"
  if [ -n "$value" ]; then
    ENDPOINT="$value"
    echo "backup-artifacts: using $name"
    break
  fi
done

ACCESS_KEY="${RESULT_STORE_ACCESS_KEY:-${NEXUS_S3_ACCESS_KEY_ID:-${MINIO_ROOT_USER:-nexusadmin}}}"
SECRET_KEY="${RESULT_STORE_SECRET_KEY:-${NEXUS_S3_SECRET_ACCESS_KEY:-${MINIO_ROOT_PASSWORD:-nexus-dev-password}}}"

OUT_DIR="${ARTIFACT_BACKUP_DIR:-${BACKUP_DIR:-${1:-./backups}}}"
KEEP_DAYS="${BACKUP_KEEP_DAYS:-7}"

# A private alias name, not `local` or `nexus`. The minio container already has
# aliases named `local`, `l` and `gcs` in its config, and reusing one would make
# this script's behaviour depend on whatever someone configured there. Scoping
# the alias to this script makes the connection explicit.
ALIAS="nexusbackup"

if [ -n "$ENDPOINT" ]; then
  MODE=host
  command -v mc >/dev/null 2>&1 || {
    echo "backup-artifacts: mc not found on PATH and an endpoint was given" >&2
    echo "backup-artifacts: install mc, or unset the endpoint to use $MINIO_CONTAINER" >&2
    exit 1
  }
  MC_CONFIG="${MC_CONFIG_DIR:-$OUT_DIR/.mc}"
  mkdir -p "$MC_CONFIG"
  run_mc() { mc --config-dir "$MC_CONFIG" "$@"; }
else
  MODE=container
  docker inspect "$MINIO_CONTAINER" >/dev/null 2>&1 || {
    echo "backup-artifacts: no endpoint set and container $MINIO_CONTAINER does not exist" >&2
    echo "backup-artifacts: set RESULT_STORE_ENDPOINT, or start the compose stack" >&2
    exit 1
  }
  echo "backup-artifacts: no endpoint set, mirroring from inside $MINIO_CONTAINER"
  ENDPOINT="http://localhost:9000"
  run_mc() { docker exec -i "$MINIO_CONTAINER" mc "$@"; }
fi

echo "backup-artifacts: waiting for the result store to accept connections"
i=1
while :; do
  if run_mc alias set "$ALIAS" "$ENDPOINT" "$ACCESS_KEY" "$SECRET_KEY" >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -ge 60 ]; then
    echo "backup-artifacts: result store at $ENDPOINT did not become reachable in 60s" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 1
done

# ── Instrument check ──────────────────────────────────────────────────────
# THIS IS NOT CEREMONY. `mc ls <misspelled-alias>/<bucket>` exits 0 and prints
# NOTHING -- byte-identical to "the bucket is empty". Verified against this
# stack: a bad bucket errors with rc=1, but a bad ALIAS succeeds silently. So a
# zero object count means either "nothing to back up" or "this script has been
# reading a typo this whole time", and those must never be confused: the second
# one produces an empty backup that reports success.
#
# `mc stat` on the bucket distinguishes them, because it fails on an alias that
# does not resolve. Every count below is only trustworthy because this ran first.
if ! run_mc stat "$ALIAS/$BUCKET" >/dev/null 2>&1; then
  echo "backup-artifacts: cannot stat $ALIAS/$BUCKET at $ENDPOINT" >&2
  echo "backup-artifacts: the bucket does not exist, or the credentials are wrong." >&2
  echo "backup-artifacts: refusing to report a count from an unverified alias." >&2
  exit 1
fi

mkdir -p "$OUT_DIR"

# UTC and sortable, matching backup.sh: local time in a filename is ambiguous
# twice a year and unsortable across a DST boundary, which matters exactly when
# someone is picking "the last good backup" out of a directory listing.
STAMP="${BACKUP_STAMP:-$(date -u +%Y%m%dT%H%M%SZ)}"
OUT_BASE="$OUT_DIR/nexus-artifacts-$STAMP"

SRC_COUNT=$(run_mc ls --recursive "$ALIAS/$BUCKET" | wc -l | tr -d ' ')
echo "backup-artifacts: mirroring $BUCKET ($SRC_COUNT objects) to $OUT_BASE"

# Same .partial discipline as backup.sh: a mirror killed halfway leaves a
# directory nobody will mistake for a complete backup, and the rename at the end
# is the commit point.
rm -rf "$OUT_BASE.partial"
mkdir -p "$OUT_BASE.partial"

if [ "$MODE" = host ]; then
  run_mc mirror --overwrite --remove "$ALIAS/$BUCKET" "$OUT_BASE.partial"
else
  # The container cannot write to the host directory, so the mirror lands on a
  # scratch path inside the container and is copied out.
  #
  # `docker cp`, NOT a `tar -cf -` stream piped over `docker exec`. The obvious
  # tar pipe was written first and failed in the drill with "Unrecognized archive
  # format", because THE MINIO IMAGE SHIPS NO tar BINARY -- and no `find`
  # either; it is a deliberately minimal image carrying little beyond mc and a
  # shell. `docker cp` is handled by the Docker daemon and needs no binary inside
  # the container at all, which is the only reason it works here. This is exactly
  # the class of bug a drill exists to find: the tar version failed at backup
  # time, but an equivalent mistake on the restore side would have failed only
  # during a real recovery.
  #
  # The trailing `/.` is load-bearing: `docker cp src/. dst` copies the CONTENTS
  # of src into dst, whereas `docker cp src dst` would nest it as dst/src and put
  # a stray path component in front of every object key. The restore path depends
  # on the archive holding bare keys.
  staged="/tmp/nexus-artifacts-$STAMP"
  docker exec -i "$MINIO_CONTAINER" rm -rf "$staged"
  docker exec -i "$MINIO_CONTAINER" mkdir -p "$staged"
  run_mc mirror --overwrite --remove "$ALIAS/$BUCKET" "$staged"
  docker cp "$MINIO_CONTAINER:$staged/." "$OUT_BASE.partial"
  docker exec -i "$MINIO_CONTAINER" rm -rf "$staged"
fi

# ── Verification ──────────────────────────────────────────────────────────
# "mc mirror exited 0" and "the objects are on disk" are different claims, and
# the only way to tell them apart is to count what actually landed. A mirror
# interrupted mid-transfer, or a full disk, exits non-zero under `set -e` -- but
# a mirror that silently copied nothing because the bucket listing was empty
# would not, which is what the instrument check above already ruled out.
DST_COUNT=$(find "$OUT_BASE.partial" -type f | wc -l | tr -d ' ')

if [ "$SRC_COUNT" -ne "$DST_COUNT" ]; then
  echo "backup-artifacts: mirrored $DST_COUNT objects but the bucket listed $SRC_COUNT" >&2
  echo "backup-artifacts: refusing to keep an incomplete mirror" >&2
  rm -rf "$OUT_BASE.partial"
  exit 1
fi

mv "$OUT_BASE.partial" "$OUT_BASE"

# A manifest of key + sha256 + size, written alongside the objects. Objects are
# content-addressed (ADR-0008), so the key already carries the digest of the
# content -- which makes the manifest a genuine integrity check rather than a
# restatement of the filename: the restore drill recomputes each digest and
# compares. It is also the only record of what the mirror BELIEVED it captured,
# so a later "is this backup complete?" is answerable without the live stack.
MANIFEST="$OUT_BASE.manifest"
: > "$MANIFEST.partial"
if [ "$DST_COUNT" -gt 0 ]; then
  # Keys, not host paths: `cd` into the mirror so the recorded name is exactly
  # the object key the restore will PUT back.
  ( cd "$OUT_BASE" && find . -type f | sed 's|^\./||' | LC_ALL=C sort ) > "$OUT_BASE.keys"
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    # shasum is present on macOS and in coreutils-less containers alike;
    # sha256sum is not universally available on the hosts this runs on.
    if command -v sha256sum >/dev/null 2>&1; then
      digest=$(sha256sum "$OUT_BASE/$key" | cut -d' ' -f1)
    else
      digest=$(shasum -a 256 "$OUT_BASE/$key" | cut -d' ' -f1)
    fi
    size=$(wc -c < "$OUT_BASE/$key" | tr -d ' ')
    printf '%s  %s  %s\n' "$digest" "$size" "$key" >> "$MANIFEST.partial"
  done < "$OUT_BASE.keys"
  rm -f "$OUT_BASE.keys"
fi
mv "$MANIFEST.partial" "$MANIFEST"

bytes=$(find "$OUT_BASE" -type f -exec wc -c {} + 2>/dev/null | tail -1 | awk '{print $1}')
echo "backup-artifacts: wrote $OUT_BASE ($DST_COUNT objects, ${bytes:-0} bytes)"
echo "backup-artifacts: wrote $MANIFEST"

# ── Pruning ───────────────────────────────────────────────────────────────
# Same policy and same reasoning as backup.sh: unbounded growth fills the disk
# and a full disk is itself an outage, but deleting the only copy is
# unrecoverable, so retention is explicit and every removal is announced.
if [ "$KEEP_DAYS" = "0" ]; then
  echo "backup-artifacts: pruning disabled (BACKUP_KEEP_DAYS=0)"
else
  pruned=0
  for old in $(find "$OUT_DIR" -maxdepth 1 -type d -name 'nexus-artifacts-*' -mtime "+$KEEP_DAYS" 2>/dev/null); do
    echo "backup-artifacts: pruning $old (older than $KEEP_DAYS days)"
    rm -rf "$old" "$old.manifest"
    pruned=$((pruned + 1))
  done
  echo "backup-artifacts: done (pruned $pruned older than $KEEP_DAYS days)"
fi
