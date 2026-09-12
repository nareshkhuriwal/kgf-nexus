#!/bin/sh
# Restores a backup-artifacts.sh mirror into a result-store bucket, then exits 0.
#
# Usage:
#   deploy/restore-artifacts.sh <mirror-dir> [target-bucket] [--yes]
#
# The counterpart to deploy/restore.sh, and written in the same voice: this runs
# when something has already gone wrong, so it is un-surprising rather than
# clever. It prints the full plan, it refuses a target that already holds objects
# unless told twice, and it never guesses the bucket.
#
# ── Restore ordering: Postgres FIRST, then artifacts ──────────────────────
# The mirror is written with MinIO captured BEFORE Postgres (see
# backup-artifacts.sh), which guarantees the backup pair contains orphan objects
# but never a dangling output_ref. Restore runs in the OPPOSITE order --
# Postgres, then objects -- and the inversion is deliberate.
#
# During a restore the system is being rebuilt, and the window between the two
# halves is a window in which something may read what has landed so far. If
# objects were restored first, that intermediate state is a store full of objects
# no row references: harmless, but it also means a service pointed at the
# half-restored database sees runs whose outputs are missing -- the dangling-ref
# failure, reintroduced during recovery. Restoring Postgres first means the
# intermediate state is rows whose objects are still arriving, which is the same
# transient condition as a task that has not finished uploading yet, and which
# the retry path already handles.
#
# Both orderings converge on the same end state. The ordering only decides which
# failure mode is visible while the restore is in flight, and the argument is the
# same asymmetry as in backup-artifacts.sh: orphans are recoverable, dangling
# refs are not. Do not point services at the restored stack until BOTH halves
# have finished -- the ordering reduces the blast radius of doing so, it does not
# make it safe.
#
# `set -e` is load-bearing: a half-restored bucket that exits 0 would be mistaken
# for a recovered one.
set -eu

MINIO_CONTAINER="${MINIO_CONTAINER:-nexus-minio-1}"
FORCE="${FORCE:-0}"

MIRROR=""
TARGET=""
for arg in "$@"; do
  case "$arg" in
    --yes|-y) FORCE=1 ;;
    --*) echo "restore-artifacts: unknown flag $arg" >&2; exit 2 ;;
    *)
      if [ -z "$MIRROR" ]; then MIRROR="$arg"
      elif [ -z "$TARGET" ]; then TARGET="$arg"
      else echo "restore-artifacts: unexpected argument $arg" >&2; exit 2
      fi
      ;;
  esac
done

if [ -z "$MIRROR" ]; then
  echo "restore-artifacts: usage: $0 <mirror-dir> [target-bucket] [--yes]" >&2
  exit 2
fi
[ -d "$MIRROR" ] || { echo "restore-artifacts: $MIRROR is not a directory" >&2; exit 1; }

# ── Connection ────────────────────────────────────────────────────────────
# Same two modes as backup-artifacts.sh and restore.sh. The target bucket is
# deliberately NOT defaulted from the mirror's name: the common recovery move is
# "restore last night's objects into a scratch bucket and look at them", and that
# must not require renaming a directory.
ENDPOINT=""
for name in RESULT_STORE_ENDPOINT NEXUS_RESULT_STORE_ENDPOINT NEXUS_S3_ENDPOINT; do
  eval "value=\${$name:-}"
  if [ -n "$value" ]; then
    ENDPOINT="$value"
    echo "restore-artifacts: using $name"
    break
  fi
done

ACCESS_KEY="${RESULT_STORE_ACCESS_KEY:-${NEXUS_S3_ACCESS_KEY_ID:-${MINIO_ROOT_USER:-nexusadmin}}}"
SECRET_KEY="${RESULT_STORE_SECRET_KEY:-${NEXUS_S3_SECRET_ACCESS_KEY:-${MINIO_ROOT_PASSWORD:-nexus-dev-password}}}"
[ -n "$TARGET" ] || TARGET="${RESULT_STORE_BUCKET:-${NEXUS_RESULT_BUCKET:-nexus-results}}"

ALIAS="nexusrestore"

if [ -n "$ENDPOINT" ]; then
  MODE=host
  command -v mc >/dev/null 2>&1 || {
    echo "restore-artifacts: mc not found on PATH and an endpoint was given" >&2
    exit 1
  }
  MC_CONFIG="${MC_CONFIG_DIR:-$MIRROR/../.mc}"
  mkdir -p "$MC_CONFIG"
  run_mc() { mc --config-dir "$MC_CONFIG" "$@"; }
else
  MODE=container
  docker inspect "$MINIO_CONTAINER" >/dev/null 2>&1 || {
    echo "restore-artifacts: no endpoint set and container $MINIO_CONTAINER does not exist" >&2
    exit 1
  }
  echo "restore-artifacts: no endpoint set, restoring inside $MINIO_CONTAINER"
  ENDPOINT="http://localhost:9000"
  run_mc() { docker exec -i "$MINIO_CONTAINER" mc "$@"; }
fi

echo "restore-artifacts: waiting for the result store to accept connections"
i=1
while :; do
  if run_mc alias set "$ALIAS" "$ENDPOINT" "$ACCESS_KEY" "$SECRET_KEY" >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -ge 60 ]; then
    echo "restore-artifacts: result store at $ENDPOINT did not become reachable in 60s" >&2
    exit 1
  fi
  i=$((i + 1))
  sleep 1
done

# ── Target inspection ─────────────────────────────────────────────────────
# `mc ls` on a bucket that does not exist errors, and on an alias that does not
# resolve returns empty with rc=0 (see the instrument note in
# backup-artifacts.sh). The alias was just set above, so it resolves; this
# distinguishes "bucket missing" from "bucket empty" by statting the bucket
# rather than reading a count and guessing.
if run_mc stat "$ALIAS/$TARGET" >/dev/null 2>&1; then
  exists=1
  existing=$(run_mc ls --recursive "$ALIAS/$TARGET" | wc -l | tr -d ' ')
else
  exists=0
  existing=0
fi

SRC_COUNT=$(find "$MIRROR" -type f | wc -l | tr -d ' ')

echo
echo "restore-artifacts: PLAN"
echo "  mirror        : $MIRROR ($SRC_COUNT objects)"
echo "  mode          : $MODE"
echo "  endpoint      : $ENDPOINT"
echo "  target bucket : $TARGET"
if [ "$exists" = "1" ]; then
  echo "  target state  : EXISTS, $existing object(s)"
else
  echo "  target state  : does not exist, will be created"
fi
if [ "$existing" -gt 0 ]; then
  echo "  action        : upload into a NON-EMPTY bucket (objects may be overwritten)"
else
  echo "  action        : upload into an empty bucket"
fi
echo

# The guard. Objects are immutable and content-addressed, so an overwrite writes
# identical bytes to the same key and is in principle harmless -- but "in
# principle" is doing too much work to bet live data on. If the mirror came from
# a different bucket, a different namespace, or a different deployment, the keys
# collide on content that is not the same content, and there is no undo. Same
# posture as restore.sh: the destructive path requires an explicit statement of
# intent.
if [ "$existing" -gt 0 ] && [ "$FORCE" != "1" ]; then
  cat >&2 <<EOF
restore-artifacts: REFUSING to restore into $TARGET -- it already holds $existing object(s).

Objects here are content-addressed and immutable, so re-uploading the same
content is a no-op. But nothing here can verify this mirror came from THIS
deployment, and a key collision across deployments overwrites data that exists
nowhere else. If that is genuinely what you want:

  1. back the bucket up FIRST:  deploy/backup-artifacts.sh
  2. re-run with --yes (or FORCE=1)

To inspect a mirror without risking live data, restore into a scratch bucket:

  deploy/restore-artifacts.sh $MIRROR nexus-restore-check
EOF
  exit 1
fi

if [ "$exists" != "1" ]; then
  echo "restore-artifacts: creating bucket $TARGET"
  run_mc mb --ignore-existing "$ALIAS/$TARGET"
  # Matching minio-init: results are private, and a restore must not quietly
  # widen access that the original bucket did not grant.
  run_mc anonymous set none "$ALIAS/$TARGET" >/dev/null
fi

echo "restore-artifacts: uploading $SRC_COUNT objects into $TARGET"
if [ "$MODE" = host ]; then
  run_mc mirror --overwrite "$MIRROR" "$ALIAS/$TARGET"
else
  # `docker cp` in, the inverse of the backup path, because the container cannot
  # read the host directory. NOT a tar pipe: the MinIO image ships no tar (nor
  # find) binary, so the obvious `tar -cf - | docker exec tar -xf -` fails with
  # "Unrecognized archive format". `docker cp` runs in the daemon and needs
  # nothing inside the container. See the matching note in backup-artifacts.sh.
  #
  # The trailing `/.` copies the CONTENTS of the mirror rather than nesting the
  # directory itself, which would prefix every object key with the mirror's name.
  staged="/tmp/nexus-restore-artifacts-$$"
  docker exec -i "$MINIO_CONTAINER" rm -rf "$staged"
  docker exec -i "$MINIO_CONTAINER" mkdir -p "$staged"
  docker cp "$MIRROR/." "$MINIO_CONTAINER:$staged"
  run_mc mirror --overwrite "$staged" "$ALIAS/$TARGET"
  docker exec -i "$MINIO_CONTAINER" rm -rf "$staged"
fi

# ── Verification ──────────────────────────────────────────────────────────
# "mc mirror finished" is not the claim "the objects are in the bucket", and the
# only way to tell them apart is to count what is actually there now. This is the
# same reasoning as the row counts restore.sh prints: an operator comparing these
# is the documented acceptance test (docs/OPERATIONS.md).
DST_COUNT=$(run_mc ls --recursive "$ALIAS/$TARGET" | wc -l | tr -d ' ')
echo
echo "restore-artifacts: $DST_COUNT object(s) now in $TARGET (mirror held $SRC_COUNT)"

if [ "$DST_COUNT" -lt "$SRC_COUNT" ]; then
  echo "restore-artifacts: FEWER objects in the bucket than in the mirror -- restore is INCOMPLETE" >&2
  exit 1
fi

echo
echo "restore-artifacts: done. Restore Postgres BEFORE pointing services at this"
echo "restore-artifacts: stack, and run deploy/restore-drill.sh to prove the two"
echo "restore-artifacts: halves actually agree."
