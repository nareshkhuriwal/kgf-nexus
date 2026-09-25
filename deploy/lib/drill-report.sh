# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Phase timing and reporting for the recovery drills. SOURCED, not executed:
#
#   . "$HERE/lib/drill-report.sh"
#   report_init restore-drill "$WORK"
#   phase_begin stop;  ...;  phase_end stop
#   phase_skip restore_artifacts "result store is not rolled back by PITR"
#   report_set rows_restored 31337          # any JSON value
#   report_set_str target_time "2026-..."   # a string
#   report_finish "$REPORT_FILE"            # writes JSON, prints the human table
#
# Two outputs on purpose. The JSON is what CI, a dashboard or the next drill's
# comparison reads; the table is what the operator reads. Both come from the same
# recorded phases, so they cannot disagree.
#
# Timestamps are wall-clock milliseconds from THIS host. A phase's duration is the
# difference of two readings on the same clock, so host/VM clock skew does not
# affect durations; it does affect the absolute ISO times, which are labels only.
#
# POSIX sh: the drills run from macOS laptops (BSD userland) and Linux CI alike.

# ── Clock ─────────────────────────────────────────────────────────────────
now_ms() {
  # GNU date has %N; BSD date prints a literal "N". perl ships with macOS and
  # nearly every Linux image; python3 is the last resort; seconds the fallback.
  _t=$(date +%s%3N 2>/dev/null)
  case "$_t" in
    *[!0-9]*|'') ;;
    *) printf '%s\n' "$_t"; return ;;
  esac
  if command -v perl >/dev/null 2>&1; then
    perl -MTime::HiRes=time -e 'printf("%d\n", time()*1000)'
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c 'import time; print(int(time.time()*1000))'
  else
    printf '%s000\n' "$(date +%s)"
  fi
}

iso_from_ms() {
  _s=$(( $1 / 1000 ))
  _ms=$(( $1 % 1000 ))
  _d=$(date -u -d "@$_s" +%Y-%m-%dT%H:%M:%S 2>/dev/null || date -u -r "$_s" +%Y-%m-%dT%H:%M:%S)
  printf '%s.%03dZ\n' "$_d" "$_ms"
}

# Seconds with millisecond precision, from a millisecond difference.
ms_to_s() { awk -v ms="$1" 'BEGIN { printf "%.3f", ms / 1000 }'; }

# ── Recording ─────────────────────────────────────────────────────────────
report_init() {
  REPORT_NAME="$1"
  REPORT_DIR="$2"
  mkdir -p "$REPORT_DIR"
  REPORT_PHASES="$REPORT_DIR/.phases"
  REPORT_EXTRA="$REPORT_DIR/.extra"
  : > "$REPORT_PHASES"
  : > "$REPORT_EXTRA"
  REPORT_T0=$(now_ms)
}

phase_begin() {
  eval "PHASE_START_$1=\$(now_ms)"
  echo "[$(iso_from_ms "$(now_ms)")] phase $1 ..."
}

# phase_end <name> [ok|failed]
phase_end() {
  _end=$(now_ms)
  eval "_start=\${PHASE_START_$1:-}"
  [ -n "$_start" ] || { echo "drill-report: phase_end $1 without phase_begin" >&2; return 1; }
  printf '%s|%s|%s|%s|\n' "$1" "$_start" "$_end" "${2:-ok}" >> "$REPORT_PHASES"
  echo "[$(iso_from_ms "$_end")] phase $1 ${2:-ok} in $(ms_to_s $((_end - _start)))s"
}

# A phase that is part of the procedure but not needed this time. Recorded with
# its reason so a reader of the JSON sees it was considered, not forgotten.
phase_skip() {
  _now=$(now_ms)
  printf '%s|%s|%s|skipped|%s\n' "$1" "$_now" "$_now" "$2" >> "$REPORT_PHASES"
  echo "[$(iso_from_ms "$_now")] phase $1 skipped: $2"
}

# report_set <key> <raw JSON value>;  report_set_str <key> <string>
report_set() { printf '%s\t%s\n' "$1" "$2" >> "$REPORT_EXTRA"; }
report_set_str() {
  _v=$(printf '%s' "$2" | sed 's/\\/\\\\/g; s/"/\\"/g')
  printf '%s\t"%s"\n' "$1" "$_v" >> "$REPORT_EXTRA"
}

# Duration (ms) of a recorded phase, or empty.
phase_ms() { awk -F'|' -v n="$1" '$1 == n { print $3 - $2 }' "$REPORT_PHASES" | tail -n 1; }

# report_finish <json-file> [first-rto-phase] [last-rto-phase]
# The RTO total is wall clock from the START of the first RTO phase to the END of
# the last one -- not a sum, so any untimed gap between phases is included.
report_finish() {
  _file="$1"
  _first="${2:-}"
  _last="${3:-}"
  _t1=$(now_ms)
  _rto=""
  if [ -n "$_first" ] && [ -n "$_last" ]; then
    _a=$(awk -F'|' -v n="$_first" '$1 == n { print $2 }' "$REPORT_PHASES" | tail -n 1)
    _b=$(awk -F'|' -v n="$_last" '$1 == n { print $3 }' "$REPORT_PHASES" | tail -n 1)
    if [ -n "$_a" ] && [ -n "$_b" ]; then _rto=$((_b - _a)); fi
  fi
  {
    printf '{\n  "drill": "%s",\n' "$REPORT_NAME"
    printf '  "started_at": "%s",\n  "finished_at": "%s",\n' "$(iso_from_ms "$REPORT_T0")" "$(iso_from_ms "$_t1")"
    printf '  "wall_seconds": %s,\n' "$(ms_to_s $((_t1 - REPORT_T0)))"
    if [ -n "$_rto" ]; then
      printf '  "rto": {"from_phase": "%s", "to_phase": "%s", "seconds": %s},\n' "$_first" "$_last" "$(ms_to_s "$_rto")"
    fi
    printf '  "phases": [\n'
    _n=$(wc -l < "$REPORT_PHASES" | tr -d ' ')
    _i=0
    while IFS='|' read -r _p _s _e _st _why; do
      _i=$((_i + 1))
      _sep=","; [ "$_i" -eq "$_n" ] && _sep=""
      _why=$(printf '%s' "$_why" | sed 's/\\/\\\\/g; s/"/\\"/g')
      printf '    {"name": "%s", "status": "%s", "start": "%s", "end": "%s", "seconds": %s%s}%s\n' \
        "$_p" "$_st" "$(iso_from_ms "$_s")" "$(iso_from_ms "$_e")" "$(ms_to_s $((_e - _s)))" \
        "$( [ -n "$_why" ] && printf ', "reason": "%s"' "$_why")" "$_sep"
    done < "$REPORT_PHASES"
    printf '  ]'
    while IFS="$(printf '\t')" read -r _k _v; do
      printf ',\n  "%s": %s' "$_k" "$_v"
    done < "$REPORT_EXTRA"
    printf '\n}\n'
  } > "$_file"

  echo
  echo "── $REPORT_NAME: phase timings ─────────────────────────────"
  printf '  %-28s %-8s %10s\n' phase status seconds
  while IFS='|' read -r _p _s _e _st _why; do
    printf '  %-28s %-8s %10s\n' "$_p" "$_st" "$(ms_to_s $((_e - _s)))"
  done < "$REPORT_PHASES"
  if [ -n "$_rto" ]; then
    printf '  %-28s %-8s %10s\n' "RTO ($_first..$_last)" "" "$(ms_to_s "$_rto")"
  fi
  echo "  report: $_file"
}
