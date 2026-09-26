#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Mirror a Nexus air-gapped bundle into a private registry and write the Helm
# values that install from it (ADR-0025 §3, docs/AIRGAP.md).
#
#   load.sh --registry registry.corp.example:5000[/path] [options]
#
# In order, and stopping at the first failure:
#   1. verify every file against SHA256SUMS (and nothing listed is missing);
#   2. verify the cosign signature over SHA256SUMS when the bundle is signed
#      and cosign is installed (--require-signature makes both mandatory);
#   3. load each image archive and push it to <registry>/<repository>:<tag>
#      (skopeo when installed: no daemon, exact bytes; docker load/tag/push
#      otherwise), using the credentials of an existing `docker login`;
#   4. record the digest the REGISTRY reports for each push -- not the bundle's:
#      `docker load` + `docker push` may re-encode a manifest, and a values file
#      that pinned a digest the registry does not serve would fail every pull;
#   5. render values-airgap.yaml, pointing every chart image at the registry BY
#      DIGEST, and (when helm is installed) prove it: `helm template` with the
#      bundle's chart must render only <registry>/...@sha256 images.
#
# Needs bash, coreutils (sha256sum or shasum), tar, and docker or skopeo.
# Nothing here reaches the internet.
set -euo pipefail

prog=load.sh
log() { printf '%s: %s\n' "$prog" "$*" >&2; }
die() { printf '%s: ERROR: %s\n' "$prog" "$*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: load.sh --registry HOST[:PORT][/PATH] [options]

  -r, --registry REG       private registry (and optional path) to push to
                           (env NEXUS_AIRGAP_REGISTRY)
  -b, --bundle PATH        bundle directory or nexus-airgap-*.tar.gz
                           (default: the directory holding this script)
  -o, --values-out FILE    values file to write (default ./values-airgap.yaml)
      --digests-out FILE   pushed-image record (default ./airgap-pushed.tsv)
      --plain-http         the registry speaks plain HTTP (local test registries
                           only; docker also needs it in insecure-registries
                           unless it is localhost)
      --tool auto|skopeo|docker
                           how images are pushed (default auto: skopeo if found)
      --require-signature  fail unless the bundle is signed AND cosign verifies it
      --verify-only        verify checksums and signature, push nothing
      --keep-local         keep the loaded images in the local docker store
      --work-dir DIR       where a .tar.gz bundle is unpacked (default: TMPDIR)
  -h, --help

Signature verification passes COSIGN_VERIFY_ARGS to `cosign verify-blob`, e.g.
  COSIGN_VERIFY_ARGS="--key cosign.pub"
  COSIGN_VERIFY_ARGS="--certificate-identity=... --certificate-oidc-issuer=..."
(COSIGN_PUBLIC_KEY=<file> is shorthand for the first.)
EOF
}

registry=${NEXUS_AIRGAP_REGISTRY:-}
bundle=""
values_out="values-airgap.yaml"
digests_out="airgap-pushed.tsv"
plain_http=0
tool=auto
require_sig=0
verify_only=0
keep_local=0
work_dir=""

while [ $# -gt 0 ]; do
  case "$1" in
    -r|--registry) registry=${2:?--registry needs a value}; shift 2 ;;
    --registry=*) registry=${1#*=}; shift ;;
    -b|--bundle) bundle=${2:?--bundle needs a value}; shift 2 ;;
    --bundle=*) bundle=${1#*=}; shift ;;
    -o|--values-out) values_out=${2:?--values-out needs a value}; shift 2 ;;
    --values-out=*) values_out=${1#*=}; shift ;;
    --digests-out) digests_out=${2:?--digests-out needs a value}; shift 2 ;;
    --digests-out=*) digests_out=${1#*=}; shift ;;
    --plain-http) plain_http=1; shift ;;
    --tool) tool=${2:?--tool needs a value}; shift 2 ;;
    --tool=*) tool=${1#*=}; shift ;;
    --require-signature) require_sig=1; shift ;;
    --verify-only) verify_only=1; shift ;;
    --keep-local) keep_local=1; shift ;;
    --work-dir) work_dir=${2:?--work-dir needs a value}; shift 2 ;;
    --work-dir=*) work_dir=${1#*=}; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; die "unknown argument: $1" ;;
  esac
done

# ── the bundle ────────────────────────────────────────────────────────────
cleanup_dirs=()
cleanup() {
  local d
  for d in ${cleanup_dirs[@]+"${cleanup_dirs[@]}"}; do rm -rf -- "$d"; done
}
trap cleanup EXIT

sha256_check() { # reads "<sha>  <path>" lines on stdin, in the current dir
  if command -v sha256sum >/dev/null 2>&1; then sha256sum --quiet -c -
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 --quiet -c -
  else die "neither sha256sum nor shasum is installed"; fi
}

[ -n "$bundle" ] || bundle=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ -f "$bundle" ]; then
  case "$bundle" in *.tar.gz|*.tgz) ;; *) die "--bundle $bundle: expected a directory or a .tar.gz" ;; esac
  if [ -f "$bundle.sha256" ]; then
    log "verifying $(basename "$bundle") against $(basename "$bundle").sha256"
    (cd "$(dirname "$bundle")" && sha256_check < "$(basename "$bundle").sha256") \
      || die "$bundle does not match $bundle.sha256: corrupted or tampered in transfer"
  else
    log "no $(basename "$bundle").sha256 beside the tarball; relying on SHA256SUMS inside it"
  fi
  unpack=$(mktemp -d "${work_dir:-${TMPDIR:-/tmp}}/nexus-airgap.XXXXXX")
  cleanup_dirs+=("$unpack")
  log "unpacking $bundle into $unpack"
  tar -xzf "$bundle" -C "$unpack" --no-same-owner
  top=$(find "$unpack" -mindepth 1 -maxdepth 1 -type d)
  [ "$(printf '%s\n' "$top" | wc -l | tr -d ' ')" = 1 ] || die "$bundle: expected one top-level directory"
  bundle=$top
fi
[ -d "$bundle" ] || die "bundle $bundle not found"
bundle=$(cd "$bundle" && pwd)
for f in SHA256SUMS manifest.json images.tsv airgap/values-airgap.template.yaml; do
  [ -f "$bundle/$f" ] || die "$bundle is not a Nexus air-gap bundle: $f is missing"
done
version=$(sed -n 's/^  "version": "\([^"]*\)",$/\1/p' "$bundle/manifest.json" | head -1)
log "bundle $(basename "$bundle") (version ${version:-unknown})"

# ── 1. checksums ──────────────────────────────────────────────────────────
log "verifying $(wc -l < "$bundle/SHA256SUMS" | tr -d ' ') files against SHA256SUMS"
(cd "$bundle" && sha256_check < SHA256SUMS) || die "checksum verification FAILED: do not use this bundle"
# A file the manifest does not list is not ours: refuse rather than ignore it.
unlisted=$(cd "$bundle" && find . -type f ! -name SHA256SUMS ! -name 'SHA256SUMS.cosign.*' | sed 's|^\./||' | sort \
  | comm -23 - <(awk '{ sub(/^[0-9a-f]+  /, ""); print }' SHA256SUMS | sort))
[ -z "$unlisted" ] || die "files in the bundle that SHA256SUMS does not cover: $unlisted"
log "checksums OK"

# ── 2. signature ──────────────────────────────────────────────────────────
if grep -q '^  "signed": true,' "$bundle/manifest.json"; then
  sigfile="$bundle/SHA256SUMS.cosign.bundle"
  [ -f "$sigfile" ] || die "manifest says the bundle is signed but $sigfile is missing"
  if command -v cosign >/dev/null 2>&1; then
    verify_args=${COSIGN_VERIFY_ARGS:-}
    if [ -z "$verify_args" ] && [ -n "${COSIGN_PUBLIC_KEY:-}" ]; then verify_args="--key $COSIGN_PUBLIC_KEY"; fi
    [ -n "$verify_args" ] || die "signed bundle: set COSIGN_PUBLIC_KEY or COSIGN_VERIFY_ARGS to say whose signature to trust"
    # shellcheck disable=SC2086 # COSIGN_VERIFY_ARGS is a list of flags by design
    cosign verify-blob --bundle "$sigfile" $verify_args "$bundle/SHA256SUMS" \
      || die "cosign signature verification FAILED: do not use this bundle"
    log "cosign signature OK"
  elif [ "$require_sig" = 1 ]; then
    die "--require-signature: the bundle is signed but cosign is not installed here"
  else
    log "WARNING: the bundle is signed but cosign is not installed; the signature was NOT verified"
  fi
else
  [ "$require_sig" = 0 ] || die "--require-signature: this bundle is UNSIGNED (see manifest.json \"signature\")"
  log "WARNING: the bundle is UNSIGNED; its integrity rests on SHA256SUMS and the tarball's .sha256 obtained out of band"
fi

if [ "$verify_only" = 1 ]; then
  log "--verify-only: done"
  exit 0
fi

# ── 3. push ───────────────────────────────────────────────────────────────
[ -n "$registry" ] || die "--registry (or NEXUS_AIRGAP_REGISTRY) is required"
case "$registry" in
  *://*) die "--registry takes HOST[:PORT][/PATH], not a URL: $registry" ;;
esac
registry=${registry%/}
reg_host=${registry%%/*}
reg_path=""
[ "$reg_host" = "$registry" ] || reg_path=${registry#*/}/

case "$tool" in
  auto) if command -v skopeo >/dev/null 2>&1; then tool=skopeo; else tool=docker; fi ;;
  skopeo|docker) command -v "$tool" >/dev/null 2>&1 || die "--tool $tool: $tool is not installed" ;;
  *) die "--tool must be auto, skopeo or docker" ;;
esac
log "pushing to $registry with $tool"

scheme=https
[ "$plain_http" = 0 ] || scheme=http

# The digest the registry serves for repo:tag, straight from the registry API
# (anonymous; empty when the registry wants credentials curl does not have).
registry_digest() { # repo tag
  command -v curl >/dev/null 2>&1 || return 0
  local tls=()
  [ "$plain_http" = 0 ] || tls=(--insecure)
  curl -fsS -I ${tls[@]+"${tls[@]}"} --max-time 30 \
    -H 'Accept: application/vnd.oci.image.index.v1+json' \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json' \
    -H 'Accept: application/vnd.docker.distribution.manifest.v2+json' \
    "$scheme://$reg_host/v2/$reg_path$1/manifests/$2" 2>/dev/null \
    | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:" { print $2 }' || true
}

push_skopeo() { # archive tag target -> digest on stdout
  local df tls=()
  df=$(mktemp)
  [ "$plain_http" = 0 ] || tls=(--dest-tls-verify=false)
  skopeo copy --all --quiet --digestfile "$df" ${tls[@]+"${tls[@]}"} \
    "oci-archive:$1:$2" "docker://$3" >&2
  cat "$df"; rm -f "$df"
}

push_docker() { # archive load_name target -> digest on stdout
  local out digest
  docker load -q -i "$1" >&2
  docker image inspect "$2" >/dev/null 2>&1 || die "$1 did not load as $2"
  docker tag "$2" "$3"
  out=$(docker push "$3" 2>&1) || { printf '%s\n' "$out" >&2; die "docker push $3 failed (docker login $reg_host?)"; }
  digest=$(printf '%s\n' "$out" | sed -n 's/.*digest: \(sha256:[0-9a-f]\{64\}\).*/\1/p' | tail -1)
  if [ "$keep_local" = 0 ]; then
    docker image rm "$3" "$2" >/dev/null 2>&1 || true
  fi
  printf '%s\n' "$digest"
}

tmp_digests=$(mktemp)
cleanup_dirs+=("$tmp_digests")
printf '# name\ttarget\tregistry_digest\tbundle_digest\n' > "$tmp_digests"
sed_script=$(mktemp)
cleanup_dirs+=("$sed_script")
esc_registry=$(printf '%s' "$registry" | sed 's/[&|\\]/\\&/g')
printf 's|@@REGISTRY@@|%s|g\n' "$esc_registry" > "$sed_script"

count=0
while IFS=$'\t' read -r name kind archive load_name bundle_digest repo tag _platforms _sources; do
  case "$name" in ''|'#'*) continue ;; esac
  target="$registry/$repo:$tag"
  log "[$kind] $name -> $target"
  if [ "$tool" = skopeo ]; then
    digest=$(push_skopeo "$bundle/$archive" "$tag" "$target")
  else
    digest=$(push_docker "$bundle/$archive" "$load_name" "$target")
  fi
  served=$(registry_digest "$repo" "$tag")
  if [ -z "$digest" ]; then
    [ -n "$served" ] || die "$target: could not determine the digest the registry stored"
    digest=$served
  elif [ -n "$served" ] && [ "$served" != "$digest" ]; then
    die "$target: push reported $digest but the registry serves $served"
  fi
  case "$digest" in sha256:*) ;; *) die "$target: bad digest '$digest'" ;; esac
  [ ${#digest} = 71 ] || die "$target: bad digest '$digest'"
  if [ "$digest" != "$bundle_digest" ]; then
    log "  note: registry digest $digest differs from the bundle's $bundle_digest (manifest re-encoded on push); using the registry's"
  fi
  [ -n "$served" ] || log "  note: could not read the digest back anonymously; trusting the push output"
  printf '%s\t%s\t%s\t%s\n' "$name" "$target" "$digest" "$bundle_digest" >> "$tmp_digests"
  printf 's|@@DIGEST:%s@@|%s|g\n' "$name" "$digest" >> "$sed_script"
  count=$((count + 1))
done < "$bundle/images.tsv"
[ "$count" -gt 0 ] || die "images.tsv lists no images"
cp "$tmp_digests" "$digests_out"
log "pushed $count images; digests recorded in $digests_out"

# ── 5. values ─────────────────────────────────────────────────────────────
tmp_values=$(mktemp)
cleanup_dirs+=("$tmp_values")
sed -f "$sed_script" "$bundle/airgap/values-airgap.template.yaml" > "$tmp_values"
if grep -n '@@' "$tmp_values" >&2; then die "values template has placeholders no pushed image filled"; fi
cp "$tmp_values" "$values_out"
log "wrote $values_out"

if command -v helm >/dev/null 2>&1; then
  chart=$(find "$bundle/chart" -maxdepth 1 -name '*.tgz' | head -1)
  bad=0
  for probe in "$bundle"/airgap/probe-*.json; do
    rendered=$(helm template airgap-check "$chart" -f "$probe" -f "$values_out") \
      || die "helm template failed with $values_out"
    while read -r ref; do
      case "$ref" in
        "$registry"/*@sha256:*) ;;
        *) log "  NOT from $registry by digest: $ref"; bad=1 ;;
      esac
    done < <(printf '%s\n' "$rendered" | sed -n "s/^[[:space:]]*\(- \)\{0,1\}image:[[:space:]]*[\"']\{0,1\}\([^\"' ]*\).*/\2/p" | sort -u)
  done
  [ "$bad" = 0 ] || die "$values_out does not redirect every chart image"
  log "helm template: every chart image resolves to $registry/...@sha256 (all optional components on)"
else
  log "helm not installed: skipped rendering the chart with $values_out"
fi

cat >&2 <<EOF
$prog: done. Install with your own values FIRST and these LAST, so they win:
  helm upgrade --install nexus $(find "$bundle/chart" -maxdepth 1 -name '*.tgz' | head -1 | xargs basename 2>/dev/null) \\
    -f my-values.yaml -f $values_out
EOF
