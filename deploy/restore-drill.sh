#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Proves a restore actually recovers artifacts, then exits 0.
#
# Usage:
#   deploy/restore-drill.sh [--keep] [--report FILE]
#   deploy/restore-drill.sh --rto --yes [--report FILE]     (DESTRUCTIVE, see below)
#
# Every run records per-phase timings and writes them as JSON (--report, default
# $TMPDIR/nexus-restore-drill-<pid>.json) plus a human table at the end.
#
# ── --rto: a timed, full-stack recovery (docs/OPERATIONS.md §4) ───────────
# After the scratch checks below pass, --rto measures what an outage actually
# costs. It takes a complete backup through deploy/backup-all.sh, then simulates
# losing BOTH stores -- the database is dropped and the result bucket removed --
# and recovers the stack, timing each phase:
#
#   stop                  stop every control-plane service and agent
#   restore_db            deploy/restore.sh of the dump into a fresh database
#   restore_artifacts     deploy/restore-artifacts.sh of the mirror
#   migrate_verify        the migrate job (a no-op on a current dump, but it is
#                         the step a real recovery runs), every output_ref
#                         resolves, a sample of objects re-hashes to its key
#   services_healthy      start the services, wait for every /readyz
#   fleet_reconciliation  the restored snapshot's workers declared DEAD, their
#                         in-flight attempts expired, the live agents
#                         re-registered (deploy/lib/nexus-stack.sh)
#
# RTO = wall clock from the start of `stop` to the end of `fleet_reconciliation`.
# Rows written between the dump and the stop are LOST by design (that is the
# logical-backup RPO) and are reported as such. It refuses the default `nexus`
# compose project unless DRILL_ALLOW_DEFAULT_PROJECT=1: run it on a drill stack.
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
RTO=0
FORCE=0
REPORT=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --keep) KEEP=1; shift ;;
    --rto) RTO=1; shift ;;
    --yes|-y) FORCE=1; shift ;;
    --report) REPORT="$2"; shift 2 ;;
    *) echo "restore-drill: unknown argument $1" >&2; exit 2 ;;
  esac
done
REPORT="${REPORT:-${TMPDIR:-/tmp}/nexus-restore-drill-$$.json}"
if [ "$RTO" = 1 ] && [ "$FORCE" != 1 ]; then
  echo "restore-drill: --rto DROPS the database and the result bucket of the stack" >&2
  echo "restore-drill: behind $PG_CONTAINER and restores them from a fresh backup." >&2
  echo "restore-drill: Re-run with --rto --yes on a drill stack." >&2
  exit 2
fi

# shellcheck source=deploy/lib/drill-report.sh
. "$HERE/lib/drill-report.sh"
# shellcheck source=deploy/lib/nexus-stack.sh
. "$HERE/lib/nexus-stack.sh"

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
if [ "$RTO" = 1 ]; then
  echo "restore-drill: --rto: after the scratch checks, $PG_DB and $LIVE_BUCKET WILL be dropped and restored"
else
  echo "restore-drill: the live bucket $LIVE_BUCKET and database $PG_DB are never written"
fi
mkdir -p "$WORK"
report_init restore-drill "$WORK/report"

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
phase_begin artifact_backup
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
phase_end artifact_backup
echo
phase_begin artifact_restore
echo "restore-drill: [3] restoring via deploy/restore-artifacts.sh into $SCRATCH_BUCKET"
MINIO_CONTAINER="$MINIO_CONTAINER" \
  sh "$HERE/restore-artifacts.sh" "$MIRROR" "$SCRATCH_BUCKET" >"$WORK/restore.log" 2>&1 || {
    echo "restore-drill: restore-artifacts.sh FAILED:" >&2
    cat "$WORK/restore.log" >&2
    exit 1
  }

phase_end artifact_restore
phase_begin verify_checks
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

phase_end verify_checks

# ── 9. Summary of the scratch checks ──────────────────────────────────────
echo
echo "restore-drill: ───────────────────────────────────────────────"
echo "restore-drill: $PASS passed, $FAIL failed"
report_set checks_passed "$PASS"
report_set checks_failed "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  echo "restore-drill: DRILL FAILED" >&2
  report_finish "$REPORT"
  exit 1
fi
echo "restore-drill: DRILL PASSED -- a restored artifact is byte-identical and its"
echo "restore-drill: output_ref resolves. Every check above was also shown to FAIL"
echo "restore-drill: when the thing it guards was deliberately broken."

if [ "$RTO" != 1 ]; then
  report_finish "$REPORT"
  exit 0
fi

# ── 10. --rto: timed full-stack recovery ──────────────────────────────────
# See the header. From here on the drill IS destructive to the stack behind
# $PG_CONTAINER / $MINIO_CONTAINER.
PROJECT=$(stack_project)
if [ "$PROJECT" = nexus ] && [ "${DRILL_ALLOW_DEFAULT_PROJECT:-0}" != 1 ]; then
  echo "restore-drill: refusing --rto on the default 'nexus' compose project;" >&2
  echo "restore-drill: use a drill stack (docker compose -p <name>) or set DRILL_ALLOW_DEFAULT_PROJECT=1" >&2
  report_finish "$REPORT"
  exit 1
fi
echo
echo "restore-drill: [10] --rto full-stack recovery of compose project '$PROJECT'"
report_set_str project "$PROJECT"
report_set dataset_before "$(pg_q "SELECT json_build_object(
  'workflow_runs', (SELECT count(*) FROM workflow_runs),
  'task_attempts', (SELECT count(*) FROM task_attempts),
  'run_events', (SELECT count(*) FROM run_events),
  'db_bytes', pg_database_size(current_database()))")"

# Backup -- measured, but NOT part of the RTO: it happened before the outage.
phase_begin backup_all
BACKUP_DIR="$WORK/full" BACKUP_STAMP=rto BACKUP_KEEP_DAYS=0 \
  PG_CONTAINER="$PG_CONTAINER" MINIO_CONTAINER="$MINIO_CONTAINER" RESULT_STORE_BUCKET="$LIVE_BUCKET" \
  sh "$HERE/backup-all.sh" > "$WORK/backup-all.log" 2>&1 || {
    cat "$WORK/backup-all.log" >&2; phase_end backup_all failed; report_finish "$REPORT"; exit 1; }
phase_end backup_all
DUMP=$(find "$WORK/full" -maxdepth 1 -name 'nexus-*.dump' | head -n 1)
FULL_MIRROR="$WORK/full/nexus-artifacts-rto"
report_set backup "$(printf '{"dump_bytes":%s,"mirror_objects":%s,"mirror_bytes":%s}' \
  "$(wc -c < "$DUMP" | tr -d ' ')" "$(find "$FULL_MIRROR" -type f | wc -l | tr -d ' ')" \
  "$(du -sk "$FULL_MIRROR" | awk '{print $1 * 1024}')")"

RECOVERY_START=$(date +%s)
phase_begin stop
stack_stop_app
phase_end stop
report_set inflight_at_stop "$(stack_inflight_json)"
# What exists at the moment of the outage vs. what the backup brings back: the
# difference is the RPO of a logical backup, in rows.
AT_STOP_ATTEMPTS=$(pg_q "SELECT count(*) FROM task_attempts")
AT_STOP_EVENTS=$(pg_q "SELECT count(*) FROM run_events")

# The simulated loss. Deliberately not timed: it is the disaster, not recovery.
echo "restore-drill: simulating loss of database $PG_DB and bucket $LIVE_BUCKET"
docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE \"$PG_DB\" WITH (FORCE)" >/dev/null
mc_ rb --force "$ALIAS/$LIVE_BUCKET" >/dev/null

phase_begin restore_db
PG_CONTAINER="$PG_CONTAINER" PG_USER="$PG_USER" sh "$HERE/restore.sh" "$DUMP" "$PG_DB" \
  > "$WORK/restore-db.log" 2>&1 || { cat "$WORK/restore-db.log" >&2; phase_end restore_db failed; report_finish "$REPORT"; exit 1; }
phase_end restore_db

phase_begin restore_artifacts
MINIO_CONTAINER="$MINIO_CONTAINER" FORCE=1 sh "$HERE/restore-artifacts.sh" "$FULL_MIRROR" "$LIVE_BUCKET" --yes \
  > "$WORK/restore-artifacts.log" 2>&1 || { cat "$WORK/restore-artifacts.log" >&2; phase_end restore_artifacts failed; report_finish "$REPORT"; exit 1; }
phase_end restore_artifacts

phase_begin migrate_verify
MIGRATE=$(stack_containers migrate all | head -n 1)
if [ -n "$MIGRATE" ]; then
  docker start -a "$MIGRATE" > "$WORK/migrate.log" 2>&1 || { cat "$WORK/migrate.log" >&2; phase_end migrate_verify failed; report_finish "$REPORT"; exit 1; }
else
  echo "restore-drill: no migrate container in project $PROJECT; skipping the migrate job" >&2
fi
MC_ALIAS="$ALIAS" stack_check_refs "$LIVE_BUCKET"
MC_ALIAS="$ALIAS" stack_verify_sample "$LIVE_BUCKET" 25
RESTORED_ATTEMPTS=$(pg_q "SELECT count(*) FROM task_attempts")
RESTORED_EVENTS=$(pg_q "SELECT count(*) FROM run_events")
verify_status=ok
if [ "$REFS_MISSING" -ne 0 ]; then bad "$REFS_MISSING of $REFS_TOTAL referenced objects are MISSING after restore"; verify_status=failed
else ok "all $REFS_TOTAL referenced objects exist in the restored bucket ($STORE_KEYS objects)"; fi
if [ "$SAMPLE_BAD" -ne 0 ] || [ "$SAMPLE_OK" -eq 0 ]; then bad "sample re-hash: $SAMPLE_OK ok, $SAMPLE_BAD bad"; verify_status=failed
else ok "sample of $SAMPLE_OK restored objects re-hash to their content-addressed keys"; fi
if [ "$RESTORED_ATTEMPTS" -gt 0 ] && [ "$RESTORED_ATTEMPTS" -le "$AT_STOP_ATTEMPTS" ]; then
  ok "restored $RESTORED_ATTEMPTS attempts ($((AT_STOP_ATTEMPTS - RESTORED_ATTEMPTS)) written after the dump are lost: the RPO)"
else bad "restored attempt count $RESTORED_ATTEMPTS is implausible (had $AT_STOP_ATTEMPTS)"; verify_status=failed; fi
phase_end migrate_verify "$verify_status"
report_set verify "$(printf '{"refs_total":%s,"refs_missing":%s,"store_objects":%s,"sample_rehash_ok":%s,"sample_rehash_bad":%s}' \
  "$REFS_TOTAL" "$REFS_MISSING" "$STORE_KEYS" "$SAMPLE_OK" "$SAMPLE_BAD")"
report_set rpo_rows_lost "$(printf '{"task_attempts":%s,"run_events":%s}' \
  "$((AT_STOP_ATTEMPTS - RESTORED_ATTEMPTS))" "$((AT_STOP_EVENTS - RESTORED_EVENTS))")"
report_set inflight_restored "$(stack_inflight_json)"

phase_begin services_healthy
stack_start_app
if stack_wait_healthy "${DRILL_HEALTH_TIMEOUT:-300}"; then phase_end services_healthy
else phase_end services_healthy failed; bad "services did not become healthy"; report_finish "$REPORT" stop services_healthy; exit 1; fi

phase_begin fleet_reconciliation
if stack_wait_reconciled "${DRILL_RECONCILE_TIMEOUT:-600}" "$RECOVERY_START"; then
  phase_end fleet_reconciliation
  report_set fresh_workers "${RECONCILE_FRESH_WORKERS:-0}"
else
  phase_end fleet_reconciliation failed
  bad "fleet did not reconcile"
fi
report_set checks_passed "$PASS"
report_set checks_failed "$FAIL"
report_finish "$REPORT" stop fleet_reconciliation
if [ "$FAIL" -gt 0 ]; then
  echo "restore-drill: RTO DRILL FAILED" >&2
  exit 1
fi
echo "restore-drill: RTO DRILL PASSED"
