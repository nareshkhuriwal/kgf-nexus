#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# The point-in-time-recovery drill: proves a restore lands EXACTLY on a chosen
# moment, and measures RTO and RPO while doing it. Exits 0 only if every
# assertion held.
#
# Usage:
#   deploy/pitr-drill.sh --yes [--report FILE] [--runs-a 3000] [--runs-b 1000]
#                        [--runs-c 500] [--nodes 10] [--artifacts-a 1500]
#                        [--artifacts-b 300] [--artifacts-c 200] [--artifact-kib 1024]
#                        [--inflight 20] [--tick-seconds 150] [--skip-disaster]
#
# DESTRUCTIVE to the stack it runs against: it seeds data, rewinds the database
# and, in scenario 2, deletes the data directory. Run it on a drill stack started
# with the PITR overlay (see OPERATIONS §4.5); it refuses the default `nexus`
# compose project unless DRILL_ALLOW_DEFAULT_PROJECT=1.
#
# ── Scenario 1: "undo the bad change" -- restore to a timestamp ───────────
#   seed batch A  ->  FULL base backup  ->  seed batch B (lives only in WAL)
#   ->  record target time T (the database's own clock)  ->  seed batch C
#   ->  timed recovery to T:
#         stop, restore_db (deploy/restore-pitr.sh --time T), restore_artifacts
#         (skipped: see below), migrate_verify, services_healthy,
#         fleet_reconciliation
#   Assertions: every A and B run and succeeded attempt is present with the exact
#   pre-restore counts; NO C run exists; every output_ref still resolves; a
#   sample of objects re-hashes to its key; the in-flight state at T (B's
#   synthetic workers and their RUNNING attempts) is reconciled by the real
#   scheduler.
#   Artifacts are not restored because PITR does not rewind the result store:
#   objects are content-addressed and immutable (ADR-0008), so every object a
#   pre-T row references is still there, and C's objects become orphans for the
#   GC sweep (OPERATIONS §7.7). The drill counts those orphans.
#
# ── Scenario 2: "the primary is gone" -- restore to the latest WAL ────────
#   a writer commits one tick row every ~200ms into a scratch database and
#   records each acknowledged commit; after --tick-seconds, postgres is SIGKILLed
#   (no clean shutdown, no final archive) and its data directory is deleted.
#   Timed recovery with deploy/restore-pitr.sh --latest, same phases.
#   RPO = (commit time of the last ACKNOWLEDGED tick) - (commit time of the newest
#   tick that survived). Bounded by archive_timeout on a quiet database: the WAL
#   segment being written when the host died was never archived.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=deploy/lib/drill-report.sh
. "$HERE/lib/drill-report.sh"
# shellcheck source=deploy/lib/nexus-stack.sh
. "$HERE/lib/nexus-stack.sh"

PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
MINIO_CONTAINER="${MINIO_CONTAINER:-nexus-minio-1}"
PG_USER="${PG_USER:-nexus}"
PG_DB="${PG_DB:-nexus}"
BUCKET="${RESULT_STORE_BUCKET:-${NEXUS_RESULT_BUCKET:-nexus-results}}"
ACCESS_KEY="${RESULT_STORE_ACCESS_KEY:-${NEXUS_S3_ACCESS_KEY_ID:-${MINIO_ROOT_USER:-nexusadmin}}}"
SECRET_KEY="${RESULT_STORE_SECRET_KEY:-${NEXUS_S3_SECRET_ACCESS_KEY:-${MINIO_ROOT_PASSWORD:-nexus-dev-password}}}"
MC_ALIAS=nexuspitrdrill
# Per run: a tick database left by an earlier, interrupted drill must never be
# mistaken for this run's survivors.
TICK_DB="nexus_pitr_ticks_$$"

RUNS_A=3000; RUNS_B=1000; RUNS_C=500; NODES=10
ART_A=1500; ART_B=300; ART_C=200; ART_KIB=1024; INFLIGHT=20
TICK_SECONDS=150; DISASTER=1; FORCE=0; REPORT=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --runs-a) RUNS_A="$2"; shift 2 ;;
    --runs-b) RUNS_B="$2"; shift 2 ;;
    --runs-c) RUNS_C="$2"; shift 2 ;;
    --nodes) NODES="$2"; shift 2 ;;
    --artifacts-a) ART_A="$2"; shift 2 ;;
    --artifacts-b) ART_B="$2"; shift 2 ;;
    --artifacts-c) ART_C="$2"; shift 2 ;;
    --artifact-kib) ART_KIB="$2"; shift 2 ;;
    --inflight) INFLIGHT="$2"; shift 2 ;;
    --tick-seconds) TICK_SECONDS="$2"; shift 2 ;;
    --skip-disaster) DISASTER=0; shift ;;
    --report) REPORT="$2"; shift 2 ;;
    --yes|-y) FORCE=1; shift ;;
    *) echo "pitr-drill: unknown argument $1" >&2; exit 2 ;;
  esac
done
REPORT="${REPORT:-${TMPDIR:-/tmp}/nexus-pitr-drill-$$.json}"
[ "$FORCE" = 1 ] || { echo "pitr-drill: DESTRUCTIVE to the stack behind $PG_CONTAINER; re-run with --yes on a drill stack" >&2; exit 2; }

docker exec "$PG_CONTAINER" pgbackrest version >/dev/null 2>&1 || {
  echo "pitr-drill: $PG_CONTAINER has no pgbackrest; start the stack with deploy/docker-compose.pitr.yml" >&2; exit 1; }
PROJECT=$(stack_project)
if [ "$PROJECT" = nexus ] && [ "${DRILL_ALLOW_DEFAULT_PROJECT:-0}" != 1 ]; then
  echo "pitr-drill: refusing the default 'nexus' compose project; use a drill stack" >&2; exit 1
fi

WORK="${TMPDIR:-/tmp}/nexus-pitr-drill-$$"
mkdir -p "$WORK"
TICKER_PID=""
cleanup() {
  status=$?
  if [ -n "$TICKER_PID" ]; then kill "$TICKER_PID" 2>/dev/null || true; fi
  docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d postgres -q \
    -c "DROP DATABASE IF EXISTS $TICK_DB WITH (FORCE)" >/dev/null 2>&1 || true
  rm -rf "$WORK"
  return $status
}
trap cleanup EXIT

PASS=0
FAIL=0
ok()  { PASS=$((PASS + 1)); echo "  PASS  $1"; }
bad() { FAIL=$((FAIL + 1)); echo "  FAIL  $1" >&2; }

report_init pitr-drill "$WORK/report"
report_set_str project "$PROJECT"
report_set_str pgbackrest "$(docker exec "$PG_CONTAINER" pgbackrest version)"
report_set_str archive_timeout "$(pg_q 'SHOW archive_timeout')"
report_set_str postgres "$(pg_q 'SHOW server_version')"

docker exec -i "$MINIO_CONTAINER" mc alias set "$MC_ALIAS" http://localhost:9000 "$ACCESS_KEY" "$SECRET_KEY" >/dev/null
docker exec -i "$MINIO_CONTAINER" mc stat "$MC_ALIAS/$BUCKET" >/dev/null || {
  echo "pitr-drill: cannot stat $BUCKET -- refusing to trust any object count" >&2; exit 1; }

echo "pitr-drill: [0] preflight: the archive pipeline must be healthy before we rely on it"
if PG_CONTAINER="$PG_CONTAINER" sh "$HERE/pitr-verify.sh" --no-verify > "$WORK/verify0.log" 2>&1; then
  ok "pitr-verify: archive_command round trip works"
else
  # No base backup yet is expected on a fresh stack; anything else is not.
  if ! grep -q "FAIL  no base backup" "$WORK/verify0.log" || [ "$(grep -c FAIL "$WORK/verify0.log")" != 1 ]; then
    cat "$WORK/verify0.log" >&2; echo "pitr-drill: archiving is not healthy" >&2; exit 1
  fi
  ok "pitr-verify: archiving healthy (no base backup yet, the drill takes one)"
fi

seed() {  # $1 batch  $2 runs  $3 artifacts  $4 inflight
  PG_CONTAINER="$PG_CONTAINER" MINIO_CONTAINER="$MINIO_CONTAINER" RESULT_STORE_BUCKET="$BUCKET" \
    sh "$HERE/pitr-seed.sh" --batch "$1" --runs "$2" --nodes "$NODES" \
      --artifacts "$3" --artifact-kib "$ART_KIB" --inflight "$4" | tee "$WORK/seed-$1.log" | tail -n 1
}
batch_counts() {  # $1 batch -> "runs succeeded_attempts"
  pg_q "SELECT (SELECT count(*) FROM workflow_runs WHERE params->>'drill_batch' = '$1') || ' ' ||
               (SELECT count(*) FROM task_attempts a JOIN workflow_runs r ON r.id = a.run_id
                 WHERE r.params->>'drill_batch' = '$1' AND a.state = 'SUCCEEDED')"
}

TAG="d$$"
A="${TAG}a"; B="${TAG}b"; C="${TAG}c"

# ── Scenario 1 ────────────────────────────────────────────────────────────
echo
echo "pitr-drill: [1] seed batch A ($RUNS_A runs, $ART_A x ${ART_KIB}KiB artifacts)"
phase_begin seed_a
report_set seed_a "$(seed "$A" "$RUNS_A" "$ART_A" "$INFLIGHT")"
phase_end seed_a

echo "pitr-drill: [2] full base backup"
phase_begin base_backup_full
report_set base_backup "$(PG_CONTAINER="$PG_CONTAINER" sh "$HERE/pitr-backup.sh" full 2>"$WORK/backup.log" | tail -n 1)"
phase_end base_backup_full

echo "pitr-drill: [3] seed batch B after the backup (recoverable only through WAL replay)"
phase_begin seed_b
report_set seed_b "$(seed "$B" "$RUNS_B" "$ART_B" "$INFLIGHT")"
phase_end seed_b

# The target, from the DATABASE clock: recovery compares commit timestamps
# recorded by this server, so the host's clock is irrelevant. The pause either
# side keeps every B commit strictly before T and every C commit strictly after.
sleep 2
TARGET=$(pg_q "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS.US') || '+00'")
TARGET_LSN=$(pg_q "SELECT pg_current_wal_lsn()")
sleep 2
echo "pitr-drill: [4] target time $TARGET (lsn $TARGET_LSN)"
report_set_str target_time "$TARGET"
report_set_str target_lsn "$TARGET_LSN"
BEFORE_A=$(batch_counts "$A"); BEFORE_B=$(batch_counts "$B")

# How long a just-finished WAL segment takes to reach the repository. This is the
# floor of the RPO on a busy database (segments fill and ship continuously).
phase_begin archive_push
SEG=$(pg_q "SELECT pg_walfile_name(pg_switch_wal())")
until [ "$(pg_q "SELECT coalesce(last_archived_wal, '') >= '$SEG' FROM pg_stat_archiver")" = t ]; do sleep 0.2; done
phase_end archive_push

echo "pitr-drill: [5] seed batch C after the target (must NOT survive the restore)"
phase_begin seed_c
report_set seed_c "$(seed "$C" "$RUNS_C" "$ART_C" 0)"
phase_end seed_c
BEFORE_C=$(batch_counts "$C")
report_set dataset_before_restore "$(pg_q "SELECT json_build_object(
  'workflow_runs', (SELECT count(*) FROM workflow_runs),
  'task_attempts', (SELECT count(*) FROM task_attempts),
  'run_events', (SELECT count(*) FROM run_events),
  'db_bytes', pg_database_size(current_database()))")"
report_set_str counts_before "A=$BEFORE_A B=$BEFORE_B C=$BEFORE_C"

echo
echo "pitr-drill: [6] RECOVERY to $TARGET"
RECOVERY_START=$(date +%s)
phase_begin s1_stop
stack_stop_app
phase_end s1_stop
phase_begin s1_restore_db
PG_CONTAINER="$PG_CONTAINER" sh "$HERE/restore-pitr.sh" --time "$TARGET" --yes \
  --report "$WORK/s1-restore.json" > "$WORK/s1-restore.log" 2>&1 || {
    cat "$WORK/s1-restore.log" >&2; phase_end s1_restore_db failed; report_finish "$REPORT"; exit 1; }
phase_end s1_restore_db
report_set s1_restore_detail "$(cat "$WORK/s1-restore.json")"
phase_skip s1_restore_artifacts "PITR does not rewind the result store; content-addressed objects referenced before the target are unchanged"

phase_begin s1_migrate_verify
MIGRATE=$(stack_containers migrate all | head -n 1)
[ -z "$MIGRATE" ] || docker start -a "$MIGRATE" > "$WORK/migrate1.log" 2>&1 || { cat "$WORK/migrate1.log" >&2; bad "migrate job failed"; }
AFTER_A=$(batch_counts "$A"); AFTER_B=$(batch_counts "$B"); AFTER_C=$(batch_counts "$C")
report_set_str counts_after "A=$AFTER_A B=$AFTER_B C=$AFTER_C"
if [ "$AFTER_A" = "$BEFORE_A" ]; then ok "batch A (before the base backup) fully present: $AFTER_A"; else bad "batch A: $AFTER_A, expected $BEFORE_A"; fi
if [ "$AFTER_B" = "$BEFORE_B" ]; then ok "batch B (WAL-only, before T) fully present: $AFTER_B"; else bad "batch B: $AFTER_B, expected $BEFORE_B"; fi
if [ "$AFTER_C" = "0 0" ]; then ok "batch C (after T) absent -- the restore stopped at the target"
else bad "batch C (after T) SURVIVED: $AFTER_C (had $BEFORE_C) -- the target was not honoured"; fi
# Negative control: batch C really was committed before the restore, so its
# absence is evidence of the target and not of a seed that never landed.
case "$BEFORE_C" in "0 "*) bad "negative control: batch C never landed ($BEFORE_C), so its absence proves nothing" ;;
  *) ok "negative control: batch C had been committed ($BEFORE_C) before the restore" ;; esac
stack_check_refs "$BUCKET"
if [ "$REFS_MISSING" -eq 0 ] && [ "$REFS_TOTAL" -gt 0 ]; then ok "all $REFS_TOTAL distinct output_refs resolve ($STORE_KEYS objects in the bucket)"
else bad "$REFS_MISSING of $REFS_TOTAL output_refs are dangling"; fi
ORPHANS=$((STORE_KEYS - REFS_TOTAL))
report_set s1_refs "$(printf '{"refs_total":%s,"refs_missing":%s,"store_objects":%s,"orphans_after_restore":%s}' "$REFS_TOTAL" "$REFS_MISSING" "$STORE_KEYS" "$ORPHANS")"
stack_verify_sample "$BUCKET" 25
if [ "$SAMPLE_BAD" -eq 0 ] && [ "$SAMPLE_OK" -gt 0 ]; then ok "$SAMPLE_OK sampled objects re-hash to their keys"; else bad "sample re-hash $SAMPLE_OK ok / $SAMPLE_BAD bad"; fi
report_set s1_inflight_restored "$(stack_inflight_json)"
phase_end s1_migrate_verify

phase_begin s1_services_healthy
stack_start_app
stack_wait_healthy "${DRILL_HEALTH_TIMEOUT:-300}" || bad "services did not become healthy"
phase_end s1_services_healthy
phase_begin s1_fleet_reconciliation
if stack_wait_reconciled "${DRILL_RECONCILE_TIMEOUT:-600}" "$RECOVERY_START"; then
  ok "fleet reconciled: snapshot workers DEAD, in-flight attempts expired, $RECONCILE_FRESH_WORKERS live worker(s)"
  phase_end s1_fleet_reconciliation
else
  bad "fleet did not reconcile"; phase_end s1_fleet_reconciliation failed
fi
S1_RTO=$(report_rto s1_rto s1_stop s1_fleet_reconciliation)
echo "pitr-drill: scenario 1 RTO ${S1_RTO}s"

# ── Scenario 2 ────────────────────────────────────────────────────────────
if [ "$DISASTER" = 1 ]; then
  echo
  echo "pitr-drill: [7] loss of primary: ticking for ${TICK_SECONDS}s, then SIGKILL + delete the data directory"
  pg_q "DROP DATABASE IF EXISTS $TICK_DB" postgres >/dev/null
  pg_q "CREATE DATABASE $TICK_DB" postgres >/dev/null
  pg_q "CREATE TABLE ticks (seq bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT clock_timestamp())" "$TICK_DB" >/dev/null
  : > "$WORK/ticks.acked"
  (
    while [ ! -f "$WORK/stop-ticks" ]; do
      # Recorded only after the server acknowledged the commit
      # (synchronous_commit=on): this is work a client was TOLD is durable.
      r=$(docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$TICK_DB" -tAqc \
        "INSERT INTO ticks DEFAULT VALUES RETURNING seq || ' ' || extract(epoch FROM at)" 2>/dev/null) &&
        echo "$r" >> "$WORK/ticks.acked"
      sleep 0.2
    done
  ) &
  TICKER_PID=$!
  sleep "$TICK_SECONDS"
  KILL_AT=$(pg_q "SELECT extract(epoch FROM clock_timestamp())")
  docker kill -s KILL "$PG_CONTAINER" >/dev/null
  touch "$WORK/stop-ticks"
  wait "$TICKER_PID" 2>/dev/null || true
  TICKER_PID=""
  LAST_ACK=$(tail -n 1 "$WORK/ticks.acked")
  ACKED=$(wc -l < "$WORK/ticks.acked" | tr -d ' ')
  echo "pitr-drill: killed at epoch $KILL_AT; $ACKED ticks acknowledged, last: $LAST_ACK"
  # The disk is gone too. pg_wal goes with it, including the segment that was
  # being written -- the part of the history that was never archived.
  IMAGE=$(docker inspect -f '{{.Config.Image}}' "$PG_CONTAINER")
  docker run --rm --volumes-from "$PG_CONTAINER" --user postgres --entrypoint find "$IMAGE" \
    /var/lib/postgresql/data -mindepth 1 -delete
  report_set_str s2_killed_at_epoch "$KILL_AT"

  RECOVERY_START=$(date +%s)
  phase_begin s2_stop
  stack_stop_app
  phase_end s2_stop
  phase_begin s2_restore_db
  PG_CONTAINER="$PG_CONTAINER" sh "$HERE/restore-pitr.sh" --latest --yes \
    --report "$WORK/s2-restore.json" > "$WORK/s2-restore.log" 2>&1 || {
      cat "$WORK/s2-restore.log" >&2; phase_end s2_restore_db failed; report_finish "$REPORT"; exit 1; }
  phase_end s2_restore_db
  report_set s2_restore_detail "$(cat "$WORK/s2-restore.json")"
  phase_skip s2_restore_artifacts "the result store survived; only the database host was lost"

  phase_begin s2_migrate_verify
  [ -z "$MIGRATE" ] || docker start -a "$MIGRATE" > "$WORK/migrate2.log" 2>&1 || { cat "$WORK/migrate2.log" >&2; bad "migrate job failed"; }
  # shellcheck disable=SC2086
  set -- $LAST_ACK
  ACK_SEQ="$1"; ACK_AT="$2"
  FIRST_AT=$(head -n 1 "$WORK/ticks.acked" | cut -d' ' -f2)
  if [ "$(pg_q "SELECT count(*) FROM pg_database WHERE datname = '$TICK_DB'" postgres)" = 1 ]; then
    SURV=$(pg_q "SELECT count(*) || ' ' || coalesce(max(seq), 0) || ' ' || coalesce(extract(epoch FROM max(at)), 0) FROM ticks" "$TICK_DB")
  else
    # The CREATE DATABASE itself was in the unarchived tail: every tick was lost.
    SURV="0 0 $FIRST_AT"
  fi
  # shellcheck disable=SC2086
  set -- $SURV
  SURV_N="$1"; SURV_SEQ="$2"; SURV_AT="$3"
  LOST=$((ACK_SEQ - SURV_SEQ))
  # RPO = the span of ACKNOWLEDGED commits that did not survive. When nothing
  # survived it is at least the whole ticking window (a lower bound).
  RPO=$(awk -v a="$ACK_AT" -v s="$SURV_AT" 'BEGIN { d = a - s; if (d < 0) d = 0; printf "%.3f", d }')
  echo "pitr-drill: survived $SURV_N ticks (up to seq $SURV_SEQ); acknowledged up to $ACK_SEQ -> $LOST lost, RPO ${RPO}s"
  report_set s2_rpo "$(printf '{"tick_seconds":%s,"ticks_acknowledged":%s,"ticks_survived":%s,"ticks_lost":%s,"rpo_seconds":%s,"rpo_is_lower_bound":%s,"archive_timeout":"%s"}' \
    "$TICK_SECONDS" "$ACK_SEQ" "$SURV_SEQ" "$LOST" "$RPO" "$( [ "$SURV_N" = 0 ] && echo true || echo false)" "$(pg_q 'SHOW archive_timeout')")"
  # Not an assertion that ticks survived: with ticking shorter than
  # archive_timeout, losing all of them IS the correct outcome. The assertion is
  # that the restore neither invented nor reordered history.
  if [ "$SURV_SEQ" -le "$ACK_SEQ" ] && [ "$SURV_N" = "$SURV_SEQ" ]; then
    ok "restored ticks are a gap-free prefix (1..$SURV_SEQ) of the $ACK_SEQ acknowledged"
  else bad "restored ticks are not a prefix of the acknowledged ones: n=$SURV_N max=$SURV_SEQ acked=$ACK_SEQ"; fi
  AFTER2_A=$(batch_counts "$A"); AFTER2_B=$(batch_counts "$B"); AFTER2_C=$(batch_counts "$C")
  if [ "$AFTER2_A" = "$BEFORE_A" ] && [ "$AFTER2_B" = "$BEFORE_B" ] && [ "$AFTER2_C" = "0 0" ]; then
    ok "latest restore followed the post-PITR timeline: A and B present, C still absent"
  else bad "after latest restore A=$AFTER2_A B=$AFTER2_B C=$AFTER2_C (expected $BEFORE_A / $BEFORE_B / 0 0)"; fi
  stack_check_refs "$BUCKET"
  if [ "$REFS_MISSING" -eq 0 ]; then ok "all $REFS_TOTAL output_refs resolve after the latest restore"; else bad "$REFS_MISSING dangling refs"; fi
  phase_end s2_migrate_verify

  phase_begin s2_services_healthy
  stack_start_app
  stack_wait_healthy "${DRILL_HEALTH_TIMEOUT:-300}" || bad "services did not become healthy"
  phase_end s2_services_healthy
  phase_begin s2_fleet_reconciliation
  if stack_wait_reconciled "${DRILL_RECONCILE_TIMEOUT:-600}" "$RECOVERY_START"; then
    ok "fleet reconciled after the latest restore"; phase_end s2_fleet_reconciliation
  else bad "fleet did not reconcile"; phase_end s2_fleet_reconciliation failed; fi
  S2_RTO=$(report_rto s2_rto s2_stop s2_fleet_reconciliation)
  echo "pitr-drill: scenario 2 RTO ${S2_RTO}s, RPO ${RPO}s"
fi

echo
echo "pitr-drill: [8] post-drill: the archive pipeline must be healthy on the new timeline"
if PG_CONTAINER="$PG_CONTAINER" sh "$HERE/pitr-verify.sh" > "$WORK/verify-end.log" 2>&1; then
  ok "pitr-verify (including pgbackrest verify of every backup and WAL file) passes"
else
  cat "$WORK/verify-end.log" >&2; bad "pitr-verify failed after the drill"
fi
report_set checks_passed "$PASS"
report_set checks_failed "$FAIL"
report_finish "$REPORT" s1_stop s1_fleet_reconciliation
echo "pitr-drill: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || { echo "pitr-drill: DRILL FAILED" >&2; exit 1; }
echo "pitr-drill: DRILL PASSED"
