#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Offline checks for every Terraform module and environment under
# deploy/terraform. Nothing here reads remote state, uses cloud credentials
# or creates anything.
#
#   validate (make tf-validate)
#     1. terraform fmt -check over the whole tree
#     2. per directory: init -backend=false, validate, and `terraform test`
#        where tests/ exists. The tests PLAN with MOCKED providers, so no cloud
#        is contacted. Environments init with -lockfile=readonly: their
#        committed .terraform.lock.hcl must already cover what they need.
#   lint (make tf-lint)
#     3. tflint, checkov and trivy config, in pinned containers (need Docker).
#        Accepted findings, each with its reason, live in .tflint.hcl,
#        .checkov.yaml and .trivyignore.
#
# Network access is needed only to download providers, registry modules and
# lint plugins. Set TF_PLUGIN_CACHE_DIR to reuse provider downloads.
#
#   deploy/terraform/validate.sh [validate|lint|all]
set -euo pipefail

MODE="${1:-validate}"
case "$MODE" in
  validate | lint | all) ;;
  *)
    echo "usage: $0 [validate|lint|all]" >&2
    exit 2
    ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$ROOT/../.." && pwd)"

# Pinned tool versions. Terraform matches the CI setup step; the linters run
# from their official images.
TERRAFORM_VERSION="${TERRAFORM_VERSION:-1.14.3}"
TFLINT_IMAGE="${TFLINT_IMAGE:-ghcr.io/terraform-linters/tflint:v0.64.0}"
CHECKOV_IMAGE="${CHECKOV_IMAGE:-bridgecrew/checkov:3.3.19}"
TRIVY_IMAGE="${TRIVY_IMAGE:-aquasec/trivy:0.74.0}"

export TF_IN_AUTOMATION=1
failed=()

# Every module and environment directory, in a stable order.
dirs() {
  for d in "$ROOT"/modules/*/ "$ROOT"/environments/*/; do
    printf '%s\n' "${d%/}"
  done
}

validate_all() {
  local tf dir rel init_log
  # TF_USE_DOCKER=1 forces the pinned container even when a terraform binary
  # exists (CI agents whose installed version is not the pinned one).
  if [ "${TF_USE_DOCKER:-0}" != 1 ] && command -v terraform >/dev/null 2>&1; then
    tf=(terraform)
  elif command -v docker >/dev/null 2>&1; then
    # No local terraform: the official image, with the repo mounted at the
    # same path so relative module and chart paths resolve.
    tf=(docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e TF_IN_AUTOMATION=1
        -e TF_PLUGIN_CACHE_DIR -v "$REPO:$REPO" -w "$REPO")
    if [ -n "${TF_PLUGIN_CACHE_DIR:-}" ]; then
      tf+=(-v "$TF_PLUGIN_CACHE_DIR:$TF_PLUGIN_CACHE_DIR")
    fi
    tf+=("hashicorp/terraform:$TERRAFORM_VERSION")
  else
    echo "tf-validate: needs terraform (>= 1.11) or Docker" >&2
    exit 2
  fi
  if [ -n "${TF_PLUGIN_CACHE_DIR:-}" ]; then
    mkdir -p "$TF_PLUGIN_CACHE_DIR"
  fi

  echo "== $("${tf[@]}" version | head -n 1)"
  echo "== fmt -check"
  if ! "${tf[@]}" fmt -check -recursive -diff "$ROOT"; then
    failed+=("fmt")
  fi

  init_log="$(mktemp)"
  while read -r dir; do
    rel="${dir#"$REPO"/}"
    echo "== $rel"

    local lock_flag=()
    case "$rel" in
      */environments/*) lock_flag=(-lockfile=readonly) ;;
    esac

    if ! "${tf[@]}" -chdir="$dir" init -backend=false -input=false -no-color \
         ${lock_flag[@]+"${lock_flag[@]}"} >"$init_log" 2>&1; then
      cat "$init_log" >&2
      failed+=("$rel (init)")
      continue
    fi
    if ! "${tf[@]}" -chdir="$dir" validate -no-color; then
      failed+=("$rel (validate)")
      continue
    fi
    if [ -d "$dir/tests" ]; then
      local test_args=()
      if [ -f "$dir/terraform.tfvars.example" ]; then
        test_args=(-var-file=terraform.tfvars.example)
      fi
      if ! "${tf[@]}" -chdir="$dir" test -no-color ${test_args[@]+"${test_args[@]}"}; then
        failed+=("$rel (test)")
      fi
    fi
  done < <(dirs)
  rm -f "$init_log"
}

lint_all() {
  local dir rel plugin_dir tflint
  command -v docker >/dev/null 2>&1 || {
    echo "tf-lint: needs Docker" >&2
    exit 2
  }

  # tflint plugins are downloaded once into a shared cache (GitHub's anonymous
  # API limit is 60 requests an hour; CI passes GITHUB_TOKEN to lift it).
  plugin_dir="${TFLINT_PLUGIN_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/nexus-tflint-plugins}"
  mkdir -p "$plugin_dir"
  tflint=(docker run --rm -e GITHUB_TOKEN -e TFLINT_PLUGIN_DIR=/plugins
          -v "$plugin_dir:/plugins" -v "$REPO:$REPO")
  echo "== tflint --init"
  if ! "${tflint[@]}" -w "$ROOT" "$TFLINT_IMAGE" --config="$ROOT/.tflint.hcl" --init; then
    failed+=("tflint --init (plugin download; set GITHUB_TOKEN if rate-limited)")
  fi
  while read -r dir; do
    rel="${dir#"$REPO"/}"
    echo "== tflint $rel"
    if ! "${tflint[@]}" -w "$dir" "$TFLINT_IMAGE" --config="$ROOT/.tflint.hcl" --format=compact; then
      failed+=("$rel (tflint)")
    fi
  done < <(dirs)

  echo "== checkov"
  if ! docker run --rm -v "$REPO:$REPO" -w "$ROOT" "$CHECKOV_IMAGE" \
       --directory "$ROOT" --config-file "$ROOT/.checkov.yaml" --quiet; then
    failed+=("checkov")
  fi

  echo "== trivy config (HIGH, CRITICAL)"
  if ! docker run --rm -v "$ROOT:/src:ro" "$TRIVY_IMAGE" config --quiet --exit-code 1 \
       --severity HIGH,CRITICAL --skip-dirs "**/.terraform" --ignorefile /src/.trivyignore /src; then
    failed+=("trivy")
  fi
}

case "$MODE" in
  validate) validate_all ;;
  lint) lint_all ;;
  all)
    validate_all
    lint_all
    ;;
esac

if [ "${#failed[@]}" -gt 0 ]; then
  printf 'deploy/terraform %s FAILED:\n' "$MODE" >&2
  printf '  %s\n' "${failed[@]}" >&2
  exit 1
fi
echo "deploy/terraform $MODE: OK"
