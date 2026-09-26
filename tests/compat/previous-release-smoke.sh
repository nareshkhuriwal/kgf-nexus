#!/usr/bin/env bash

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Previous-release skew smoke: runs the PREVIOUS release's database integration
# suite against a database migrated by THIS tree. See docs/COMPATIBILITY.md §7.
#
#   tests/compat/previous-release-smoke.sh
#   NEXUS_COMPAT_PREVIOUS_REF=626f90b tests/compat/previous-release-smoke.sh
#   NEXUS_COMPAT_KEEP=1 tests/compat/previous-release-smoke.sh   # leave DB + tree
#
# WHY. ADR-0011 rule 5 promises that a rolling deploy can run the previous
# release's services against the new schema: migrations are applied first, and
# the old replicas keep serving until they are replaced (docs/OPERATIONS.md
# §6.3). internal/migrations checks the SHAPE of new SQL (no DROP COLUMN, no
# RENAME, ...). It cannot tell whether the old code's actual queries still work
# against the new schema -- a new CHECK constraint, trigger, NOT NULL-with-default
# that an old INSERT trips, a changed function signature. This script runs the
# old code's own queries (its store layer and in-process service tests) against
# the new schema, which is the closest thing to "old replicas on the new schema"
# that can run without building and deploying images.
#
# WHAT "PREVIOUS RELEASE" MEANS, in order:
#   1. NEXUS_COMPAT_PREVIOUS_REF, if set (any rev git can resolve).
#   2. The newest tag matching vMAJOR.MINOR.PATCH (strict semver, no suffix),
#      excluding a tag that points at HEAD itself. Tags are what a release process
#      would cut; when they exist, they are what production runs.
#   3. Without such a tag (the case today: the repository has no release tags),
#      the previous merge on main is the best available proxy for what
#      production runs, because every merge to main is what gets deployed next:
#        - HEAD is the main tip (a push to main)  -> main~1, its first parent;
#        - HEAD is anything else (a PR, a branch) -> the main tip.
#      "main" is NEXUS_COMPAT_MAIN_REF, default origin/main, falling back to main.
#   The resolved revision, and why, is printed first.
#
# WHAT IT DOES
#   a. Resolves the previous revision and extracts it with `git archive` into a
#      scratch directory (no worktree, no checkout: nothing in this checkout's git
#      state changes).
#   b. Checks migration history: every previous *.up.sql must exist in this tree
#      byte-for-byte (an applied migration is never re-run, so an edit silently
#      diverges fresh and upgraded databases), and every new one must sort after
#      the previous highest (otherwise upgraded and fresh databases apply it in
#      different orders -- services/migrations/README.md).
#   c. Creates two throwaway databases, <prefix>_new and <prefix>_base
#      (default prefix nexus_compat_<pid>), on the PostgreSQL server
#      NEXUS_TEST_PG_SERVER names -- the host's own server by default; no
#      Postgres container is started (the project rule, tests/lib/hostpg.sh).
#      Each carries a COMMENT tag; the script never drops a database without
#      its tag, and refuses a name that exists untagged.
#   d. UPGRADE PATH, not a fresh install: applies the previous tree's migrations
#      with the previous tree's deploy/migrate.sh (the database production has),
#      then this tree's with this tree's deploy/migrate.sh
#      (MIGRATIONS_DIR=services/migrations). Asserts the ledger then equals this
#      tree's migration set exactly.
#   e. Runs the previous tree's `go test -tags integration` suite, -p 1, against
#      that database via NEXUS_TEST_DATABASE_URL. The tests do NOT create or drop
#      their own schema (they rely on a pre-migrated database, as CI's
#      integration job does), so they genuinely run against the new schema.
#      A database-missing skip fails the run (F85); zero passing tests fails it.
#   f. On failure, triages: the failing tests are re-run against the second
#      database, which has ONLY the previous tree's migrations. A test that fails there too is a pre-existing failure of the
#      old suite, not skew; one that passes there and fails on the new schema is
#      a real incompatibility.
#   g. Always drops both databases and removes the scratch tree (trap),
#      unless NEXUS_COMPAT_KEEP=1.
#
# EXIT CODES
#   0  the previous release's suite passes against the new schema
#   1  SKEW INCOMPATIBILITY: a test that passes on the old schema fails on the new
#      one, or migration history was rewritten
#   2  usage / environment error (missing tool, bad port, unresolvable ref)
#   3  inconclusive: the old suite also fails on its OWN schema, so the failure
#      says nothing about compatibility -- fix the baseline, then re-run
#
# WHAT IT DOES NOT PROVE (also in docs/COMPATIBILITY.md §7.3)
#   - Built binaries, gRPC between services of different versions, or the agent:
#     it runs the old code's Go tests in-process, not the old images.
#   - Old code reading rows WRITTEN BY new code: the database starts empty apart
#     from what the old tests themselves write.
#   - Anything the old suite does not exercise, and the NATS/S3/Mailpit-dependent
#     tests, which skip themselves here (listed in the summary).
#   - Forward skew (new code on the old schema): unsupported by design --
#     migrations run first.
#
# ENVIRONMENT
#   NEXUS_COMPAT_PREVIOUS_REF   override the previous revision
#   NEXUS_COMPAT_MAIN_REF       main ref for the fallback (default origin/main)
#   NEXUS_TEST_PG_SERVER        admin DSN of the PostgreSQL server the two
#                               databases are created on, as a role that may
#                               CREATE DATABASE (default
#                               postgres://127.0.0.1:5432/postgres?sslmode=disable,
#                               the workstation's server; in CI, the job's
#                               service container)
#   NEXUS_COMPAT_DB_PREFIX      database name prefix (default nexus_compat_<pid>);
#                               ^[a-z_][a-z0-9_]{0,50}$. Concurrent runs on one
#                               server get distinct names from the pid.
#   NEXUS_COMPAT_PKGS           packages of the OLD tree to test (default: every
#                               package with an integration-tagged _test.go file)
#   NEXUS_COMPAT_SKIP           passed to go test -skip, for a documented exclusion
#   NEXUS_COMPAT_TIMEOUT        go test -timeout (default 20m)
#   NEXUS_COMPAT_PARALLEL       go test -parallel (default 2), which bounds the
#                               connections the old suite opens at once
#   NEXUS_COMPAT_REQUIRE_SKEW   1 = fail (exit 1) when the two trees have the same
#                               migration set, i.e. there is no schema skew to test
#   NEXUS_COMPAT_LOG            where the full go test log is copied (survives
#                               cleanup; default $TMPDIR/nexus-compat-<sha>.log)
#   NEXUS_COMPAT_KEEP           1 = leave the databases and scratch tree in place
#   NEXUS_COMPAT_MIGRATIONS_DIR the CURRENT migration set (default
#                               services/migrations); for the negative-control
#                               drill only, never in CI
set -euo pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)

die()  { echo "compat: ERROR: $1" >&2; exit "${2:-2}"; }
say()  { echo "compat: $*"; }
rule() { echo "────────────────────────────────────────────────────────────────"; }

for tool in git go psql tar; do
  command -v "$tool" >/dev/null || die "$tool not found on PATH"
done

# shellcheck source=tests/lib/hostpg.sh
. "$ROOT/tests/lib/hostpg.sh"
HOSTPG_TAG=org.kgf-nexus.compat-smoke
hostpg_suite_pool_floor
DB_PREFIX=${NEXUS_COMPAT_DB_PREFIX:-nexus_compat_$$}
# An identifier with no room for quoting tricks: the names are the only
# databases this script ever drops, so they must not be able to match others.
[[ "$DB_PREFIX" =~ ^[a-z_][a-z0-9_]{0,50}$ ]] \
  || die "NEXUS_COMPAT_DB_PREFIX must match ^[a-z_][a-z0-9_]{0,50}\$ (got '$DB_PREFIX')"
DB_NEW="${DB_PREFIX}_new"
DB_BASE="${DB_PREFIX}_base"
PKGS=${NEXUS_COMPAT_PKGS:-}
TIMEOUT=${NEXUS_COMPAT_TIMEOUT:-20m}
# Tests run one package at a time (-p 1) and at most this many at once within
# a package: on a shared server near max_connections, the default (GOMAXPROCS)
# fails tests at connect with "too many clients" (F153).
PARALLEL=${NEXUS_COMPAT_PARALLEL:-2}
[[ "$PARALLEL" =~ ^[1-9][0-9]*$ ]] || die "NEXUS_COMPAT_PARALLEL must be a positive integer (got '$PARALLEL')"
# The migration set under test. Always services/migrations in CI; overridable
# ONLY so the negative-control drill (docs/COMPATIBILITY.md §7.4) can prove this
# gate fails on a breaking migration without editing the tree.
CUR_MIG=${NEXUS_COMPAT_MIGRATIONS_DIR:-$ROOT/services/migrations}
[ -d "$CUR_MIG" ] || die "migrations directory $CUR_MIG does not exist"
CUR_MIG=$(cd "$CUR_MIG" && pwd)
SKIP_RE=${NEXUS_COMPAT_SKIP:-}

# ── a. resolve the previous revision ──────────────────────────────────────
head_sha=$(git rev-parse --verify HEAD^{commit})
prev_ref="" ; prev_why=""
if [ -n "${NEXUS_COMPAT_PREVIOUS_REF:-}" ]; then
  prev_ref=$NEXUS_COMPAT_PREVIOUS_REF
  prev_why="NEXUS_COMPAT_PREVIOUS_REF"
else
  # Strict vX.Y.Z only: a pre-release tag is not what production runs.
  while IFS= read -r tag; do
    [ -n "$tag" ] || continue
    [ "$(git rev-parse "$tag^{commit}")" = "$head_sha" ] && continue
    prev_ref=$tag
    prev_why="newest release tag"
    break
  done < <(git tag --list 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true)
fi
if [ -z "$prev_ref" ]; then
  main_ref=${NEXUS_COMPAT_MAIN_REF:-}
  if [ -z "$main_ref" ]; then
    if git rev-parse --verify -q origin/main^{commit} >/dev/null; then main_ref=origin/main
    elif git rev-parse --verify -q main^{commit} >/dev/null; then main_ref=main
    else die "no release tag and neither origin/main nor main exists; set NEXUS_COMPAT_PREVIOUS_REF (CI: checkout with fetch-depth: 0)"
    fi
  fi
  main_sha=$(git rev-parse --verify "$main_ref^{commit}") || die "cannot resolve $main_ref"
  if [ "$main_sha" = "$head_sha" ]; then
    prev_ref="$main_ref~1"
    prev_why="no vX.Y.Z tag; HEAD is the $main_ref tip, so its first parent (the previous merge)"
  else
    prev_ref=$main_ref
    prev_why="no vX.Y.Z tag; HEAD is not the $main_ref tip, so the $main_ref tip (what production runs before this change merges)"
  fi
fi
prev_sha=$(git rev-parse --verify -q "$prev_ref^{commit}") \
  || die "cannot resolve previous revision '$prev_ref' (shallow clone? CI needs fetch-depth: 0)"
[ "$prev_sha" != "$head_sha" ] || die "previous revision $prev_ref is HEAD itself; nothing to compare"

rule
say "current  : $(git log -1 --format='%h %cs %s' "$head_sha")"
say "previous : $(git log -1 --format='%h %cs %s' "$prev_sha")"
say "           ref=$prev_ref ($prev_why)"
rule

WORK=$(mktemp -d "${TMPDIR:-/tmp}/nexus-compat.XXXXXX")
PREV="$WORK/prev"
LOG_DEST=${NEXUS_COMPAT_LOG:-${TMPDIR:-/tmp}/nexus-compat-${prev_sha:0:12}.log}
LOG_DEST=${LOG_DEST//\/\//\/}

DBS_CREATED=""
cleanup() {
  local rc=$?
  if [ "${NEXUS_COMPAT_KEEP:-0}" = "1" ]; then
    say "NEXUS_COMPAT_KEEP=1: leaving databases ${DBS_CREATED:-none} and $WORK in place"
  else
    # Only the databases this run created (and tagged), never a namesake.
    for db in $DBS_CREATED; do hostpg_drop "$db" || true; done
    rm -rf "$WORK"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

mkdir -p "$PREV"
git archive --format=tar "$prev_sha" | tar -x -C "$PREV"
[ -f "$PREV/go.mod" ] || die "previous tree has no go.mod"
[ -d "$PREV/services/migrations" ] || die "previous tree has no services/migrations"
[ -f "$PREV/deploy/migrate.sh" ] || die "previous tree has no deploy/migrate.sh"

# ── b. migration history ───────────────────────────────────────────────────
say "checking migration history (previous -> current: $CUR_MIG)"
history_bad=0
prev_last=""
for f in "$PREV"/services/migrations/*.up.sql; do
  b=$(basename "$f")
  prev_last=$b
  if [ ! -f "$CUR_MIG/$b" ]; then
    echo "  REMOVED  $b: applied on production databases, gone from this tree" >&2
    history_bad=1
  elif ! cmp -s "$f" "$CUR_MIG/$b"; then
    echo "  EDITED   $b: the ledger will never re-run it, so upgraded and fresh databases now differ" >&2
    history_bad=1
  fi
done
new_migrations=()
for f in "$CUR_MIG"/*.up.sql; do
  b=$(basename "$f")
  [ -f "$PREV/services/migrations/$b" ] && continue
  new_migrations+=("$b")
  if [[ "$b" < "$prev_last" ]]; then
    echo "  ORDER    $b sorts before $prev_last: an upgraded database applies it last, a fresh one earlier" >&2
    history_bad=1
  fi
done
if [ "$history_bad" = 1 ]; then
  die "migration history was rewritten between the two revisions (see above)" 1
fi
if [ "${#new_migrations[@]}" = 0 ]; then
  say "WARNING: no new migrations since the previous revision -- the schema is identical,"
  say "         so this run exercises the harness, not schema skew."
  if [ "${NEXUS_COMPAT_REQUIRE_SKEW:-0}" = "1" ]; then
    die "NEXUS_COMPAT_REQUIRE_SKEW=1 and there is no schema skew to test" 1
  fi
else
  say "new migrations the previous release has never seen:"
  printf '  + %s\n' "${new_migrations[@]}"
fi

# ── c. throwaway databases on the host's PostgreSQL ────────────────────
hostpg_check | sed 's/^/compat: /'
for db in "$DB_NEW" "$DB_BASE"; do
  hostpg_create "$db"
  DBS_CREATED="$DBS_CREATED $db"
done
say "databases $DB_NEW (upgrade path) and $DB_BASE (previous schema only) on $(hostpg_redact "$HOSTPG_SERVER")"

dsn() { hostpg_url "$HOSTPG_SERVER" "$1"; }
DSN_NEW=$(dsn "$DB_NEW")
DSN_BASE=$(dsn "$DB_BASE")

# migrate SCRIPT DIR DBNAME: run a deploy/migrate.sh over a migrations
# directory, the host's psql against the host's server. NOTICEs ("... does not
# exist, skipping") are silenced; errors are not.
migrate() {
  hostpg_migrate "$(dsn "$3")" "$1" "$2"
}
ledger() {
  hostpg_psql "$(dsn "$1")" -c 'SELECT version FROM schema_migrations ORDER BY version'
}
expected_ledger() {
  for f in "$1"/*.up.sql; do basename "$f" .up.sql; done | LC_ALL=C sort
}

# ── d. the upgrade path ────────────────────────────────────────────────────
rule
say "applying the PREVIOUS release's migrations with its own migrate.sh (the database production has)"
migrate "$PREV/deploy/migrate.sh" "$PREV/services/migrations" "$DB_NEW" | sed 's/^/  /' \
  || die "the previous release's own migrations failed to apply to an empty database" 3
say "upgrading with the CURRENT tree's migrate.sh and $CUR_MIG"
migrate "$ROOT/deploy/migrate.sh" "$CUR_MIG" "$DB_NEW" | sed 's/^/  /' \
  || die "the upgrade from the previous release's schema FAILED: a database at $prev_ref cannot be migrated to this tree" 1
if ! diff <(ledger "$DB_NEW" | LC_ALL=C sort) <(expected_ledger "$CUR_MIG") >/dev/null; then
  diff <(ledger "$DB_NEW" | LC_ALL=C sort) <(expected_ledger "$CUR_MIG") >&2 || true
  die "schema_migrations does not match this tree's migration set after the upgrade" 1
fi
say "ledger matches this tree ($(ledger "$DB_NEW" | wc -l | tr -d ' ') migrations)"
say "baseline database: PREVIOUS migrations only (for triage)"
migrate "$PREV/deploy/migrate.sh" "$PREV/services/migrations" "$DB_BASE" | sed 's/^/  /' \
  || die "the previous release's migrations failed on the baseline database" 3

# ── e. the previous release's suite against the new schema ────────────────
# The marker a database-missing skip prints (internal/testdb.SkipMarker). Read
# from the OLD tree, since that is the code running.
skip_marker=$(sed -n 's/^const SkipMarker = "\(.*\)"$/\1/p' "$PREV/internal/testdb/testdb.go" 2>/dev/null || true)
skip_marker=${skip_marker:-skipping integration test: no test database}

run_suite() { # DSN LOG -- extra go test args...
  local dsn=$1 log=$2; shift 3
  local args=(-tags integration -count=1 -p 1 -parallel "$PARALLEL" -timeout "$TIMEOUT" -json)
  [ -n "$SKIP_RE" ] && args+=(-skip "$SKIP_RE")
  ( cd "$PREV" && NEXUS_TEST_DATABASE_URL=$dsn go test "${args[@]}" "$@" ) >"$log" 2>&1
}
# Pulls "pkg<TAB>Test" for every failed top-level test out of go test -json.
# Empty output, not a failed pipeline, when only a PACKAGE failed (a test
# timeout, a panic): under pipefail the empty grep used to end the script with
# exit 1 and no message, before the package-level diagnosis below.
failed_tests() {
  { grep '"Action":"fail"' "$1" | grep '"Test":"' || true; } \
    | sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"/]*)[^"]*".*/\1	\2/' | LC_ALL=C sort -u
}
# Top-level tests only (no "/" in the name): subtests would inflate the counts.
count_action() { grep -cE "\"Action\":\"$2\",\"Package\":\"[^\"]*\",\"Test\":\"[^\"/]*\"[,}]" "$1" || true; }

# Default: exactly the packages of the OLD tree that carry integration-tagged
# files, so the pass count means "database tests", not "every unit test too".
if [ -z "$PKGS" ]; then
  PKGS=$(cd "$PREV" && grep -rlE --include='*_test.go' '^//go:build .*integration' . \
    | xargs -n1 dirname | LC_ALL=C sort -u | sed 's|$|/|' | tr '\n' ' ')
  [ -n "$PKGS" ] || die "no integration-tagged test packages in the previous tree" 3
fi

rule
say "running the PREVIOUS release's integration suite against the upgraded schema"
say "packages: $PKGS"
LOG="$WORK/skew.jsonl"
start=$(date +%s)
status=0
# shellcheck disable=SC2086
run_suite "$DSN_NEW" "$LOG" -- $PKGS || status=$?
elapsed=$(( $(date +%s) - start ))
cp "$LOG" "$LOG_DEST" 2>/dev/null || true

passed=$(count_action "$LOG" pass)
failed=$(count_action "$LOG" fail)
skipped=$(count_action "$LOG" skip)
say "suite finished in ${elapsed}s: pass=$passed fail=$failed skip=$skipped (go test exit $status)"
say "full log (go test -json): $LOG_DEST"

# Build failures produce no test events; surface them rather than a bare exit.
if grep -qE '"Action":"build-fail"|\[build failed\]|\[setup failed\]' "$LOG"; then
  grep -h '"Output"' "$LOG" | grep -iE 'build failed|cannot|undefined|error' | head -40 >&2 || true
  die "the previous tree's integration suite did not build" 3
fi

# Skips for a reason other than a missing database (NATS, S3, Mailpit, a
# feature flag) are legitimate here but are listed, so nobody reads them as passes.
other_skips=$(grep '"Action":"skip"' "$LOG" | grep '"Test":"' \
  | sed -E 's/.*"Package":"([^"]*)".*"Test":"([^"]*)".*/  \1 \2/' | LC_ALL=C sort -u || true)
if grep -q "$skip_marker" "$LOG"; then
  grep -h "$skip_marker" "$LOG" | head -5 >&2
  die "integration tests skipped for want of a database; that is not a pass (F85)" 2
fi
if [ -n "$other_skips" ]; then
  say "skipped for reasons other than the database (NOT exercised by this run):"
  echo "$other_skips"
fi
if [ "$passed" -eq 0 ]; then
  die "zero tests passed: the run proved nothing" 3
fi

if [ "$status" = 0 ] && [ "$failed" -eq 0 ]; then
  rule
  say "PASS: $passed tests of $prev_ref (${prev_sha:0:12}) pass against the schema of ${head_sha:0:12}"
  [ "${#new_migrations[@]}" = 0 ] && say "      (no schema skew between the two revisions; see WARNING above)"
  exit 0
fi

# ── f. triage ──────────────────────────────────────────────────────────────
rule
fails=$(failed_tests "$LOG")
if [ -z "$fails" ]; then
  grep -h '"Output"' "$LOG" | tail -60 >&2
  die "go test failed without a failing test (package-level failure); see $LOG_DEST" 3
fi
say "failing tests on the NEW schema:"
echo "$fails" | sed 's/^/  /'
say "re-running them against the baseline (PREVIOUS schema only)"
skew=() ; preexisting=()
while IFS=$'\t' read -r pkg test; do
  [ -n "$pkg" ] || continue
  rel=".${pkg#github.com/khuriwalgroup/kgf-nexus}"
  blog="$WORK/baseline-$(echo "$pkg-$test" | tr '/' '_').jsonl"
  if run_suite "$DSN_BASE" "$blog" -- -run "^${test}\$" "$rel"; then
    skew+=("$pkg $test")
  else
    preexisting+=("$pkg $test")
  fi
done <<<"$fails"

# Show the failure output of each failing test so the log is actionable.
while IFS=$'\t' read -r pkg test; do
  echo "── $pkg $test (new schema)" >&2
  grep -F "\"Test\":\"$test" "$LOG" | grep -F "\"Package\":\"$pkg\"" | grep '"Output"' \
    | sed -E 's/,"OutputType":"[a-z-]*"}$/}/; s/.*"Output":"(.*)"}$/\1/; s/\\n$//; s/\\t/    /g; s/\\"/"/g' \
    | uniq | tail -25 >&2
done <<<"$fails"

rule
if [ "${#preexisting[@]}" -gt 0 ]; then
  say "fail on the PREVIOUS schema too (pre-existing, not skew):"
  printf '  %s\n' "${preexisting[@]}"
fi
if [ "${#skew[@]}" -gt 0 ]; then
  say "SKEW INCOMPATIBILITY -- pass on the previous schema, fail after the upgrade:"
  printf '  %s\n' "${skew[@]}"
  die "the previous release cannot run against this tree's schema; a rolling deploy would break it" 1
fi
die "every failure also fails on the previous release's own schema: inconclusive, fix the baseline" 3
