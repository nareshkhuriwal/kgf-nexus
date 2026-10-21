#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Tests for deploy/scripts/release-notes.sh against a fixture CHANGELOG and
# fixture release files: which section a stable and a pre-release tag take,
# what is cut and what is rewritten, the install section, and what it refuses.
# No network and no repository state.
#
#   deploy/scripts/tests/release-notes-test.sh        (bash 3.2 or later, jq)
set -u

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
NOTES="$HERE/../release-notes.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/release-notes-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
export GITHUB_SERVER_URL=https://github.com GITHUB_REPOSITORY=example/kgf-nexus
unset ATTESTED

cat >"$WORK/CHANGELOG.md" <<'MD'
# Changelog

Intro text that belongs to no release.

## [0.4.0-rc.2] - 2026-11-02

Candidate notes of its own.

## 0.3.0

The 0.3.0 summary, see [ADR-0035](docs/adr/0035-x.md#decision) and
[the site](https://example.github.io/kgf-nexus/) and [above](#added).

### Added

- A thing ([OPERATIONS §22.4](docs/OPERATIONS.md)).


## 0.2.0

Older notes.
MD

cat >"$WORK/chart.json" <<'JSON'
{"chart":"oci://ghcr.io/example/charts/kgf-nexus","version":"0.3.0","digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","appVersion":"v0.3.0"}
JSON
cat >"$WORK/digests.json" <<'JSON'
{"agent":"ghcr.io/example/kgf-nexus/agent@sha256:2222222222222222222222222222222222222222222222222222222222222222","ui":"ghcr.io/example/kgf-nexus/ui@sha256:3333333333333333333333333333333333333333333333333333333333333333"}
JSON
sed 's/"0.3.0"/"0.3.1"/' "$WORK/chart.json" >"$WORK/chart-0.3.1.json"

PASSED=0
FAILED=0
OUT="$WORK/out"
ERR="$WORK/err"

# run ARGS... -> $OUT stdout, $ERR stderr, $RC the exit code.
run() {
  bash "$NOTES" "$@" >"$OUT" 2>"$ERR"
  RC=$?
}

fail() {
  echo "FAIL  $CASE: $*"
  sed 's/^/        /' "$OUT" "$ERR"
  FAILED=$((FAILED + 1))
  CASE_OK=0
}

start() { CASE=$1; CASE_OK=1; }
done_case() { if [ "$CASE_OK" = 1 ]; then echo "ok    $CASE"; PASSED=$((PASSED + 1)); fi; }

expect_rc() { [ "$RC" = "$1" ] || fail "exit code $RC, want $1"; }
# A line of stdout, exactly.
expect_line() { grep -Fqx -- "$1" "$OUT" || fail "no line: $1"; }
expect_in() { grep -Fq -- "$1" "$OUT" || fail "stdout lacks: $1"; }
expect_not_in() { if grep -Fq -- "$1" "$OUT"; then fail "stdout has: $1"; fi; }
expect_err() { grep -Fq -- "$1" "$ERR" || fail "stderr lacks: $1"; }

start "stable tag: its own section, heading cut, links into the tagged tree"
run v0.3.0 "$WORK/CHANGELOG.md"
expect_rc 0
[ "$(head -n 1 "$OUT")" = "The 0.3.0 summary, see [ADR-0035](https://github.com/example/kgf-nexus/blob/v0.3.0/docs/adr/0035-x.md#decision) and" ] \
  || fail "first line is '$(head -n 1 "$OUT")'"
expect_line "[the site](https://example.github.io/kgf-nexus/) and [above](#added)."
expect_line "### Added"
expect_line "- A thing ([OPERATIONS §22.4](https://github.com/example/kgf-nexus/blob/v0.3.0/docs/OPERATIONS.md))."
[ -n "$(tail -n 1 "$OUT")" ] || fail "trailing blank lines kept"
expect_not_in "## 0.3.0"
expect_not_in "Older notes."
expect_not_in "Intro text"
expect_not_in "Pre-release"
expect_not_in "## Install"
[ -s "$ERR" ] && fail "unexpected stderr"
done_case

start "pre-release tag without a section: the release's section, said to be a candidate"
run v0.3.0-rc.1 "$WORK/CHANGELOG.md"
expect_rc 0
expect_line "> **Pre-release of 0.3.0.** Not for production. The notes are those of 0.3.0 as they stand at this tag."
expect_in "https://github.com/example/kgf-nexus/blob/v0.3.0-rc.1/docs/adr/0035-x.md#decision"
expect_line "### Added"
expect_not_in "Older notes."
done_case

start "pre-release tag with its own bracketed, dated section"
run v0.4.0-rc.2 "$WORK/CHANGELOG.md"
expect_rc 0
[ "$(cat "$OUT")" = "Candidate notes of its own." ] || fail "notes are '$(cat "$OUT")'"
done_case

start "no section: empty notes and a warning, not a failure"
run v0.9.0 "$WORK/CHANGELOG.md"
expect_rc 0
[ -s "$OUT" ] && fail "notes are not empty"
expect_err "::warning::"
expect_err "no section for 0.9.0"
done_case

start "with the release files: the install section, the images, the assets"
run v0.3.0 "$WORK/CHANGELOG.md" "$WORK/chart.json" "$WORK/digests.json"
expect_rc 0
expect_line "### Added"
expect_line "## Install"
expect_line "helm upgrade --install nexus oci://ghcr.io/example/charts/kgf-nexus --version 0.3.0 -n nexus \\"
expect_line "  -f my-values.yaml -f release-images.yaml --set image.tag=v0.3.0"
expect_in "pins all 2"
expect_line "| agent | \`ghcr.io/example/kgf-nexus/agent@sha256:2222222222222222222222222222222222222222222222222222222222222222\` |"
expect_line "| \`kgf-nexus-0.3.0.tgz\` | the chart package, byte for byte the one in GHCR |"
expect_in "https://github.com/example/kgf-nexus/blob/v0.3.0/docs/INSTALL-AKS.md"
expect_not_in "gh attestation verify"
done_case

start "ATTESTED=true: the verification commands"
ATTESTED=true run v0.3.0 "$WORK/CHANGELOG.md" "$WORK/chart.json" "$WORK/digests.json"
expect_rc 0
expect_line "gh attestation verify oci://ghcr.io/example/kgf-nexus/agent@sha256:2222222222222222222222222222222222222222222222222222222222222222 --repo example/kgf-nexus"
expect_line "gh attestation verify oci://ghcr.io/example/charts/kgf-nexus:0.3.0 --repo example/kgf-nexus"
done_case

start "refused: a chart.json for another version, a tag that is not a release, bad usage"
run v0.3.0 "$WORK/CHANGELOG.md" "$WORK/chart-0.3.1.json" "$WORK/digests.json"
expect_rc 1
expect_err "is chart 0.3.1"
run 0.3.0 "$WORK/CHANGELOG.md"
expect_rc 2
run v0.3.0-rc..1 "$WORK/CHANGELOG.md"
expect_rc 2
run v0.3.0 "$WORK/no-such-file.md"
expect_rc 2
run v0.3.0 "$WORK/CHANGELOG.md" "$WORK/chart.json"
expect_rc 2
run v0.3.0 "$WORK/CHANGELOG.md" "$WORK/chart.json" "$WORK/missing.json"
expect_rc 1
GITHUB_REPOSITORY='' run v0.3.0 "$WORK/CHANGELOG.md"
[ "$RC" != 0 ] || fail "ran without GITHUB_REPOSITORY"
done_case

echo
echo "release-notes tests: $PASSED passed, $FAILED failed"
[ "$FAILED" -eq 0 ]
