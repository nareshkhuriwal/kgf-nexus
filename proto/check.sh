#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Proto checks with the pinned, checksum-verified buf (deploy/scripts/pinned-tool.sh).
#
#   proto/check.sh lint        buf lint, rules and exceptions in proto/buf.yaml
#   proto/check.sh breaking    buf breaking against the previous main
#   proto/check.sh all         both
#
# WHY breaking. proto/nexus/v1 is the wire contract between the control plane
# and every deployed agent, and a rolling upgrade runs old agents against new
# services (ADR-0011 rule 5, docs/COMPATIBILITY.md). A renumbered field, a
# changed type or a deleted RPC compiles fine on both sides and fails only at
# runtime, on the old side. Nothing blocked such a change before this check.
#
# WHAT "previous main" MEANS (the same rule as tests/compat/previous-release-smoke.sh):
#   1. BUF_AGAINST, if set (any rev git can resolve).
#   2. HEAD is the main tip (a push to main)  -> its first parent, HEAD~1;
#      HEAD is anything else (a PR, a branch) -> the main tip.
#      "main" is BUF_MAIN_REF, default origin/main, falling back to main.
#   The against tree is extracted with `git archive` (no checkout, and it works
#   from a git worktree). CI needs the history: checkout with fetch-depth: 0.
#
# An intentional break (a new major package, nexus.v2) is not something to
# force through this gate: add the new package alongside v1 instead.
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
mode=${1:-all}
case "$mode" in lint | breaking | all) ;; *) echo "usage: $0 [lint|breaking|all]" >&2; exit 2 ;; esac

buf=${BUF:-$("$ROOT/deploy/scripts/pinned-tool.sh" buf)}
echo "proto: buf $("$buf" --version)"

rc=0
if [ "$mode" != breaking ]; then
  echo "== buf lint"
  if (cd proto && "$buf" lint); then echo "ok   lint"; else rc=1; fi
fi

if [ "$mode" != lint ]; then
  head_sha=$(git rev-parse --verify HEAD^{commit})
  if [ -n "${BUF_AGAINST:-}" ]; then
    ref=$BUF_AGAINST; why="BUF_AGAINST"
  else
    main_ref=${BUF_MAIN_REF:-}
    if [ -z "$main_ref" ]; then
      if git rev-parse --verify -q origin/main^{commit} >/dev/null; then main_ref=origin/main
      elif git rev-parse --verify -q main^{commit} >/dev/null; then main_ref=main
      else echo "proto: neither origin/main nor main exists; set BUF_AGAINST (CI: fetch-depth: 0)" >&2; exit 2
      fi
    fi
    if [ "$(git rev-parse --verify "$main_ref^{commit}")" = "$head_sha" ]; then
      ref="$main_ref~1"; why="HEAD is the $main_ref tip: its first parent"
    else
      ref=$main_ref; why="the $main_ref tip"
    fi
  fi
  against_sha=$(git rev-parse --verify -q "$ref^{commit}") \
    || { echo "proto: cannot resolve '$ref' (shallow clone? CI needs fetch-depth: 0)" >&2; exit 2; }
  echo "== buf breaking against $(git log -1 --format='%h %s' "$against_sha") ($why)"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  if git cat-file -e "$against_sha:proto" 2>/dev/null; then
    git archive --format=tar "$against_sha" proto | tar -x -C "$tmp"
    # The against tree's own buf.yaml (if any) defines only its module layout;
    # the rules come from the CURRENT proto/buf.yaml, as buf documents.
    if (cd proto && "$buf" breaking --against "$tmp/proto"); then
      echo "ok   no breaking change against ${against_sha:0:12}"
    else
      echo "proto: BREAKING change against ${against_sha:0:12} (see above). Old agents and clients" >&2
      echo "       would misread or reject the new wire format during a rolling upgrade." >&2
      rc=1
    fi
  else
    echo "proto: $ref has no proto/ directory; nothing to compare" >&2
  fi
fi
exit "$rc"
