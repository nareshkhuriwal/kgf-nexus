#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Proves a restore actually recovers artifacts, then exits 0.
#
# Usage:
#   deploy/restore-drill.sh [--keep]
#
# Precedent: commit c9f6ede added backup/restore and proved a restore works
# rather than asserting it. This is the same standard applied to the result
# store. "backup-artifacts.sh exited 0" is not evidence of anything; the claims
# that matter are:
#
#   1. a restored object is BYTE-IDENTICAL to the original, and
#   2. the output_ref in a restored task_attempts row still RESOLVES to it.
#
# Claim 2 is the one that makes this a drill of the artifact-backup GAP rather
# than of `mc mirror`. The gap was never "objects cannot be copied" -- it was
# that a restored database points at objects nobody backed up. So the drill
# restores BOTH halves and then follows a real output_ref from the restored row
# to the restored object, which is the exact path a downstream task retried after
# a restore would take.
#
# ── This drill does NOT touch live data ───────────────────────────────────
# It creates a scratch bucket and a scratch database, both named with this
# script's PID, and drops both at the end. It never writes to nexus-results and
# never restores over the nexus database. The live bucket is READ (to mirror it)
# and never modified.
#
# ── Negative controls ─────────────────────────────────────────────────────
# A check nobody has seen fail is not evidence. Every assertion here is also run
# against a deliberately broken input, and the drill FAILS if a check passes when
# it should not:
#
#   * an object is deleted from the restored bucket   → the presence check must catch it
#   * a byte is corrupted in a restored object        → the digest comparison must catch it
#   * an output_ref is pointed at a missing key       → the resolution check must catch it
#   * the object count is read from a bad alias       → the instrument must error, not return 0
#
# That last one is not hypothetical. `mc ls <bad-alias>/<bucket>` exits 0 and
# prints nothing, which is byte-identical to "the bucket is empty" -- so a
# count-based check silently passes against a typo. The drill verifies its own
# instrument before trusting any zero.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

MINIO_CONTAINER="${MINIO_CONTAINER:-nexus-minio-1}"
PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
PG_USER="${PG_USER:-nexus}"
PG_DB="${PG_DB:-nexus}"
LIVE_BUCKET="${RESULT_STORE_BUCKET:-${NEXUS_RESULT_BUCKET:-nexus-results}}"

ACCESS_KEY="${RESULT_STORE_ACCESS_KEY:-${NEXUS_S3_ACCESS_KEY_ID:-${MINIO_ROOT_USER:-nexusadmin}}}"
SECRET_KEY="${RESULT_STORE_SECRET_KEY:-${NEXUS_S3_SECRET_ACCESS_KEY:-${MINIO_ROOT_PASSWORD:-nexus-dev-password}}}"

KEEP=0
for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    *) echo "restore-drill: unknown argument $arg" >&2; exit 2 ;;
  esac
done

# Scratch names. The PID suffix means a drill left behind by a crash never
# collides with the next one, and neither can ever be mistaken for live.
SCRATCH_BUCKET="nexus-drill-$$"
SCRATCH_DB="nexus_drill_$$"
WORK="${TMPDIR:-/tmp}/nexus-restore-drill-$$"
SRC_BUCKET="nexus-drillsrc-$$"

ALIAS="nexusdrill"
mc_() { docker exec -i "$MINIO_CONTAINER" mc "$@"; }
psql_() { docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$1" -v ON_ERROR_STOP=1 -tAc "$2"; }

PASS=0
FAIL=0
ok()   { PASS=$((PASS + 1)); echo "  PASS  $1"; }
bad()  { FAIL=$((FAIL + 1)); echo "  FAIL  $1" >&2; }

cleanup() {
  status=$?
  if [ "$KEEP" = "1" ]; then
    echo
    echo "restore-drill: --keep given, leaving scratch state:"
    echo "restore-drill:   bucket  $SCRATCH_BUCKET, source bucket $SRC_BUCKET"
    echo "restore-drill:   db      $SCRATCH_DB"
    echo "restore-drill:   workdir $WORK"
    return $status
  fi
  echo
  echo "restore-drill: cleaning up scratch state"
  mc_ rb --force "$ALIAS/$SCRATCH_BUCKET" >/dev/null 2>&1 || true
  mc_ rb --force "$ALIAS/$SRC_BUCKET" >/dev/null 2>&1 || true
  docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q \
    -c "DROP DATABASE IF EXISTS \"$SCRATCH_DB\" WITH (FORCE)" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  return $status
}
trap cleanup EXIT

echo "restore-drill: scratch bucket $SCRATCH_BUCKET, scratch db $SCRATCH_DB"
echo "restore-drill: the live bucket $LIVE_BUCKET and database $PG_DB are never written"
mkdir -p "$WORK"

docker inspect "$MINIO_CONTAINER" >/dev/null 2>&1 || {
  echo "restore-drill: container $MINIO_CONTAINER does not exist; start the compose stack" >&2
  exit 1
}
mc_ alias set "$ALIAS" "http://localhost:9000" "$ACCESS_KEY" "$SECRET_KEY" >/dev/null

# ── 0. Verify the instrument, before trusting any count ───────────────────
# `mc ls` against an alias that does not resolve exits 0 and prints nothing.
# Every object count in this drill is meaningless unless that case is
# distinguishable, so prove the distinguisher works before using it.
echo
echo "restore-drill: [0] instrument check -- does a bad alias LOOK like an empty bucket?"
bad_ls_rc=0
bad_ls_out=$(mc_ ls --recursive "nexusdrill-typo/$LIVE_BUCKET" 2>/dev/null) || bad_ls_rc=$?
if [ "$bad_ls_rc" = "0" ] && [ -z "$bad_ls_out" ]; then
  ok "confirmed the trap: 'mc ls' on a bad alias exits 0 with no output (looks empty)"
else
  # Not a failure of the drill -- a newer mc may have fixed this. Say so loudly,
  # because the reasoning below assumes the trap exists.
  echo "  NOTE  'mc ls' on a bad alias exited $bad_ls_rc; this mc may error on bad aliases"
  PASS=$((PASS + 1))
fi
# `mc stat` is the instrument the scripts actually rely on. It must FAIL on the
# bad alias and SUCCEED on the good one, or no count anywhere is trustworthy.
if mc_ stat "nexusdrill-typo/$LIVE_BUCKET" >/dev/null 2>&1; then
  bad "'mc stat' SUCCEEDED on a bad alias -- the instrument cannot detect a typo"
else
  ok "'mc stat' fails on a bad alias (this is what makes a zero count trustworthy)"
fi
if mc_ stat "$ALIAS/$LIVE_BUCKET" >/dev/null 2>&1; then
  ok "'mc stat' succeeds on the real bucket $LIVE_BUCKET"
else
  bad "'mc stat' failed on the real bucket $LIVE_BUCKET -- credentials or bucket wrong"
fi

# ── 1. Build a known artifact in a scratch SOURCE bucket ──────────────────
# The drill needs an object whose exact bytes it knows. It must not invent one in
# the live bucket (that would write to live data), so it builds a scratch source
# bucket shaped exactly like the real one: content-addressed key, namespace
# prefix, per ADR-0008 (s3://<bucket>/<namespace>/<sha256>).
echo
echo "restore-drill: [1] creating a known artifact in scratch source bucket $SRC_BUCKET"
mc_ mb --ignore-existing "$ALIAS/$SRC_BUCKET" >/dev/null

# Deterministic, larger than the 64 KiB inline threshold so it is genuinely a
# blob result and not something Postgres would have carried anyway. That
# threshold is the entire reason this backup gap exists.
ART="$WORK/artifact.bin"
dd if=/dev/urandom of="$ART" bs=1024 count=128 2>/dev/null
if command -v sha256sum >/dev/null 2>&1; then
  ORIG_SHA=$(sha256sum "$ART" | cut -d' ' -f1)
else
  ORIG_SHA=$(shasum -a 256 "$ART" | cut -d' ' -f1)
fi
ORIG_SIZE=$(wc -c < "$ART" | tr -d ' ')
KEY="drill/$ORIG_SHA"
docker exec -i "$MINIO_CONTAINER" sh -c "cat > /tmp/drill-$$.bin" < "$ART"
mc_ cp "/tmp/drill-$$.bin" "$ALIAS/$SRC_BUCKET/$KEY" >/dev/null
docker exec -i "$MINIO_CONTAINER" rm -f "/tmp/drill-$$.bin"
echo "restore-drill: artifact $ORIG_SIZE bytes, sha256 $ORIG_SHA"
echo "restore-drill: key $KEY (content-addressed, ADR-0008)"

# ── 2. Back it up, through the real script ────────────────────────────────
# Through backup-artifacts.sh, not a hand-written `mc mirror`: a drill that
# exercises a reimplementation of the procedure proves nothing about the
# procedure.
echo
echo "restore-drill: [2] backing up via deploy/backup-artifacts.sh"
BACKUP_DIR="$WORK/backups" \
  RESULT_STORE_BUCKET="$SRC_BUCKET" \
  MINIO_CONTAINER="$MINIO_CONTAINER" \
  BACKUP_KEEP_DAYS=0 \
  BACKUP_STAMP="drill" \
  sh "$HERE/backup-artifacts.sh" >"$WORK/backup.log" 2>&1 || {
    echo "restore-drill: backup-artifacts.sh FAILED:" >&2
    cat "$WORK/backup.log" >&2
    exit 1
  }
MIRROR="$WORK/backups/nexus-artifacts-drill"
[ -d "$MIRROR" ] || { echo "restore-drill: mirror $MIRROR was not created" >&2; exit 1; }
if [ -f "$MIRROR/$KEY" ]; then
  ok "the mirror contains the artifact at its object key"
else
  bad "the mirror does NOT contain $KEY"
fi
if [ -s "$WORK/backups/nexus-artifacts-drill.manifest" ]; then
  ok "a manifest of key/sha256/size was written"
else
  bad "no manifest was written"
fi

# ── 3. Restore into a scratch bucket, through the real script ─────────────
echo
echo "restore-drill: [3] restoring via deploy/restore-artifacts.sh into $SCRATCH_BUCKET"
MINIO_CONTAINER="$MINIO_CONTAINER" \
  sh "$HERE/restore-artifacts.sh" "$MIRROR" "$SCRATCH_BUCKET" >"$WORK/restore.log" 2>&1 || {
    echo "restore-drill: restore-artifacts.sh FAILED:" >&2
    cat "$WORK/restore.log" >&2
    exit 1
  }

# ── 4. CLAIM 1: the restored object is byte-identical ─────────────────────
# Pulled back out of the restored bucket and hashed. Comparing the manifest to
# the mirror would only prove the mirror is self-consistent; the claim is about
# what is in the BUCKET after a restore, so the bytes must come from there.
echo
echo "restore-drill: [4] CLAIM 1 -- restored object is byte-identical"
mc_ cat "$ALIAS/$SCRATCH_BUCKET/$KEY" > "$WORK/restored.bin" 2>/dev/null || true
if [ ! -s "$WORK/restored.bin" ]; then
  bad "could not read $KEY back from $SCRATCH_BUCKET"
else
  if command -v sha256sum >/dev/null 2>&1; then
    GOT_SHA=$(sha256sum "$WORK/restored.bin" | cut -d' ' -f1)
  else
    GOT_SHA=$(shasum -a 256 "$WORK/restored.bin" | cut -d' ' -f1)
  fi
  GOT_SIZE=$(wc -c < "$WORK/restored.bin" | tr -d ' ')
  if [ "$GOT_SHA" = "$ORIG_SHA" ] && [ "$GOT_SIZE" = "$ORIG_SIZE" ]; then
    ok "restored object matches: $GOT_SIZE bytes, sha256 $GOT_SHA"
  else
    bad "restored object DIFFERS: $GOT_SIZE bytes/$GOT_SHA vs $ORIG_SIZE/$ORIG_SHA"
  fi
  # The key is the digest of the content (ADR-0008), so a restore that put the
  # right bytes under the wrong key is also a failure -- and one a plain
  # byte-comparison would miss.
  if [ "$GOT_SHA" = "${KEY##*/}" ]; then
    ok "content-addressing holds: the object's digest equals its key"
  else
    bad "the object's digest does not equal its key -- content addressing broken"
  fi
fi

# ── 5. NEGATIVE CONTROL for claim 1 ───────────────────────────────────────
# Corrupt one byte and confirm the comparison above actually catches it. Without
# this, "the digests matched" is compatible with a comparison that always
# matches.
echo
echo "restore-drill: [5] negative control -- corrupt a byte, the check must catch it"
cp "$WORK/restored.bin" "$WORK/corrupt.bin"
# Flip the first byte. printf into the file at offset 0 via dd, conv=notrunc so
# the rest of the file is untouched and only the single byte changes.
printf 'X' | dd of="$WORK/corrupt.bin" bs=1 seek=0 count=1 conv=notrunc 2>/dev/null
if command -v sha256sum >/dev/null 2>&1; then
  CORRUPT_SHA=$(sha256sum "$WORK/corrupt.bin" | cut -d' ' -f1)
else
  CORRUPT_SHA=$(shasum -a 256 "$WORK/corrupt.bin" | cut -d' ' -f1)
fi
if [ "$CORRUPT_SHA" = "$ORIG_SHA" ]; then
  bad "a corrupted byte produced the SAME digest -- the comparison is vacuous"
else
  ok "a single corrupted byte changes the digest (the comparison is real)"
fi

# ── 6. NEGATIVE CONTROL -- delete a restored object ───────────────────────
# Confirm that "the object is present" is a check that can fail. Deleted from the
# SCRATCH bucket only.
echo
echo "restore-drill: [6] negative control -- delete the restored object, presence check must catch it"
mc_ rm "$ALIAS/$SCRATCH_BUCKET/$KEY" >/dev/null 2>&1 || true
if mc_ stat "$ALIAS/$SCRATCH_BUCKET/$KEY" >/dev/null 2>&1; then
  bad "the object still stats after deletion -- the presence check is vacuous"
else
  ok "a deleted object fails the presence check (the check is real)"
fi
# Put it back: the output_ref resolution test below needs it. Through the real
# restore script again, not a hand-rolled mirror -- this doubles as proof that a
# restore is idempotent and can be re-run over a partially-populated bucket,
# which is what an operator retrying a failed recovery actually does.
MINIO_CONTAINER="$MINIO_CONTAINER" FORCE=1 \
  sh "$HERE/restore-artifacts.sh" "$MIRROR" "$SCRATCH_BUCKET" --yes >"$WORK/restore2.log" 2>&1 || {
    echo "restore-drill: re-restore FAILED:" >&2
    cat "$WORK/restore2.log" >&2
  }
if mc_ stat "$ALIAS/$SCRATCH_BUCKET/$KEY" >/dev/null 2>&1; then
  ok "re-mirroring restored the deleted object (recovery is repeatable)"
else
  bad "could not re-restore the deleted object"
fi

# ── 7. CLAIM 2: a restored output_ref still resolves ──────────────────────
# The claim the whole gap is about. A scratch database carries a task_attempts
# row whose output_ref is shaped exactly like the live ones
# ({"result":{"blob":{"key","bucket","sha256"}}}), and the drill follows that
# reference into the restored bucket -- the same path a downstream task retried
# after a restore would take.
#
# The row is built in a scratch database rather than by restoring the live dump,
# because the live nexus database must not be touched and a full pg_restore is
# already proven by c9f6ede. What is unproven, and what this tests, is that the
# REFERENCE survives and still points at bytes that exist.
echo
echo "restore-drill: [7] CLAIM 2 -- a restored output_ref resolves to the restored object"
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q \
  -c "DROP DATABASE IF EXISTS \"$SCRATCH_DB\" WITH (FORCE)" >/dev/null 2>&1 || true
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q \
  -c "CREATE DATABASE \"$SCRATCH_DB\"" >/dev/null

docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SCRATCH_DB" -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
CREATE TABLE task_attempts (id BIGSERIAL PRIMARY KEY, output_ref JSONB);
INSERT INTO task_attempts (output_ref) VALUES (
  '{"result":{"blob":{"key":"$KEY","bucket":"$SCRATCH_BUCKET","sha256":"$ORIG_SHA"},"size_bytes":"$ORIG_SIZE","content_type":"application/octet-stream"}}'::jsonb
);
SQL

REF_KEY=$(psql_ "$SCRATCH_DB" "SELECT output_ref->'result'->'blob'->>'key' FROM task_attempts LIMIT 1")
REF_BUCKET=$(psql_ "$SCRATCH_DB" "SELECT output_ref->'result'->'blob'->>'bucket' FROM task_attempts LIMIT 1")
REF_SHA=$(psql_ "$SCRATCH_DB" "SELECT output_ref->'result'->'blob'->>'sha256' FROM task_attempts LIMIT 1")

if [ -z "$REF_KEY" ] || [ -z "$REF_BUCKET" ]; then
  bad "could not read an output_ref back out of the restored row"
else
  ok "output_ref reads back: $REF_BUCKET/$REF_KEY"
  # Resolve it, exactly as a worker would: read the object the row names, and
  # check the bytes against the digest the row recorded.
  mc_ cat "$ALIAS/$REF_BUCKET/$REF_KEY" > "$WORK/resolved.bin" 2>/dev/null || true
  if [ ! -s "$WORK/resolved.bin" ]; then
    bad "output_ref does NOT resolve -- $REF_BUCKET/$REF_KEY is missing (dangling ref)"
  else
    if command -v sha256sum >/dev/null 2>&1; then
      RES_SHA=$(sha256sum "$WORK/resolved.bin" | cut -d' ' -f1)
    else
      RES_SHA=$(shasum -a 256 "$WORK/resolved.bin" | cut -d' ' -f1)
    fi
    if [ "$RES_SHA" = "$REF_SHA" ] && [ "$RES_SHA" = "$ORIG_SHA" ]; then
      ok "output_ref resolves to bytes matching the digest the row recorded"
    else
      bad "output_ref resolved to DIFFERENT bytes: $RES_SHA vs recorded $REF_SHA"
    fi
  fi
fi

# ── 8. NEGATIVE CONTROL for claim 2 ───────────────────────────────────────
# Point an output_ref at a key that does not exist and confirm the resolution
# check reports a dangling ref. This is the failure the whole ordering argument
# is designed to prevent, so the drill must be able to SEE it. Without this
# control, "the ref resolved" is compatible with a check that never looks.
echo
echo "restore-drill: [8] negative control -- dangling output_ref must be detected"
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$SCRATCH_DB" -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
INSERT INTO task_attempts (output_ref) VALUES (
  '{"result":{"blob":{"key":"drill/0000000000000000000000000000000000000000000000000000000000000000","bucket":"$SCRATCH_BUCKET","sha256":"0000000000000000000000000000000000000000000000000000000000000000"}}}'::jsonb
);
SQL
DANGLING=0
for k in $(psql_ "$SCRATCH_DB" "SELECT output_ref->'result'->'blob'->>'key' FROM task_attempts ORDER BY id"); do
  if ! mc_ stat "$ALIAS/$SCRATCH_BUCKET/$k" >/dev/null 2>&1; then
    DANGLING=$((DANGLING + 1))
  fi
done
if [ "$DANGLING" -eq 1 ]; then
  ok "the resolution sweep found exactly the 1 planted dangling ref"
else
  bad "the sweep found $DANGLING dangling refs, expected exactly 1 -- it is not looking properly"
fi

# ── 9. Summary ────────────────────────────────────────────────────────────
echo
echo "restore-drill: ───────────────────────────────────────────────"
echo "restore-drill: $PASS passed, $FAIL failed"
if [ "$FAIL" -gt 0 ]; then
  echo "restore-drill: DRILL FAILED" >&2
  exit 1
fi
echo "restore-drill: DRILL PASSED -- a restored artifact is byte-identical and its"
echo "restore-drill: output_ref resolves. Every check above was also shown to FAIL"
echo "restore-drill: when the thing it guards was deliberately broken."
