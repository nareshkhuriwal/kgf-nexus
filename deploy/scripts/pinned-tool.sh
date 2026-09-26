#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Pinned, checksum-verified developer and CI tools.
#
#   deploy/scripts/pinned-tool.sh <tool>          install if missing; print its path
#   deploy/scripts/pinned-tool.sh --version <tool> print the pinned version
#   deploy/scripts/pinned-tool.sh --list           print every pin
#
# Tools: helm, kubeconform, buf.
#
# WHY. CI used whatever helm the runner image happened to carry and did not run
# kubeconform or buf at all. A lint gate whose tool version floats changes its
# verdict without a commit, and a binary fetched without a checksum is whatever
# the network returned. Every tool here is one exact release, and the download
# is refused unless its SHA-256 equals the value committed below.
#
# WHERE. $NEXUS_TOOLS_DIR (default <repo>/.tools, gitignored), one directory per
# tool and version: .tools/kubeconform-v0.8.0/kubeconform. A second run is a
# cache hit and needs no network; a hit is re-verified against the pin, so a
# replaced binary is refused too. CI caches this directory.
#
# UPDATING A PIN. Change the version and all four checksums together, taking the
# checksums from the project's own release checksum file (URLs below), never
# by hashing a download yourself: that proves only that you got what you got.
#   helm        https://get.helm.sh/helm-<v>-<os>-<arch>.tar.gz.sha256sum
#   kubeconform https://github.com/yannh/kubeconform/releases/download/<v>/CHECKSUMS
#   buf         https://github.com/bufbuild/buf/releases/download/<v>/sha256.txt
# Record the change in docs/OPERATIONS.md §22 (third-party versions).
set -euo pipefail

HELM_VERSION=v4.3.0
KUBECONFORM_VERSION=v0.8.0
BUF_VERSION=v1.73.0

# sha256 <tool> <os> <arch> -- the pinned digest of the release ARCHIVE (helm,
# kubeconform) or BINARY (buf) for that platform.
sha256_pin() {
  case "$1/$2/$3" in
    helm/linux/amd64)         echo 86584a54def73570558f66f5111cc53dfed56689637ae32c1201205d494f54fb ;;
    helm/linux/arm64)         echo 31c5794dd55c66a51e6b7d2e2ac7a114ae8b1de41ff1d9ba51748ac973b06a08 ;;
    helm/darwin/arm64)        echo d3870437e1e95b67f8edbde964156c84a26503f560821d40c542441658934fba ;;
    helm/darwin/amd64)        echo 347a784877e0e20eac865e8d1c36a80f6bb0861d6f29abd34defb6570ef95d92 ;;
    kubeconform/linux/amd64)  echo 9bc2bffbf71f261128533edaf912153948b7ff238f9a531ae6d34466ec287883 ;;
    kubeconform/linux/arm64)  echo 1f53fc8e81258197a35e8603054162a5af1de8c5af13746c71ab680d9534ed87 ;;
    kubeconform/darwin/arm64) echo f84f4dfbebf4a6b0b230385fa065a39ea35e02608c2b50d025dcf64775a69d67 ;;
    kubeconform/darwin/amd64) echo 71dbc87ac9f24099a62b93570e65aa06312ba6ac8aea63b7f86e9d999edf5a92 ;;
    buf/linux/amd64)          echo 8f2986298ad08f0cc1bf999b9797b7c383adf32d7edf0f73d6f1e1a701baeac1 ;;
    buf/linux/arm64)          echo 902b75267db7f4391e99b7fa0756050e5354234cc0437ef50eee9c788950c7a3 ;;
    buf/darwin/arm64)         echo 6e6df0fef4522e4e43dfe7c341873c3f2c29ceb45a9dfa5e0bad5580b8b2022f ;;
    buf/darwin/amd64)         echo ff78d0ebf34180ebfa81d370275851ec630fcb088bf33e213fd723d0fd7444a6 ;;
    *) return 1 ;;
  esac
}

die() { echo "pinned-tool: $*" >&2; exit 1; }

version_of() {
  case "$1" in
    helm) echo "$HELM_VERSION" ;;
    kubeconform) echo "$KUBECONFORM_VERSION" ;;
    buf) echo "$BUF_VERSION" ;;
    *) die "unknown tool '$1' (helm, kubeconform, buf)" ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1
  else die "neither sha256sum nor shasum is installed"
  fi
}

platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;; Darwin) os=darwin ;;
    *) die "unsupported OS $(uname -s)" ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  echo "$os $arch"
}

# url <tool> <version> <os> <arch>
url() {
  local v=$2 os=$3 arch=$4
  case "$1" in
    helm) echo "https://get.helm.sh/helm-$v-$os-$arch.tar.gz" ;;
    kubeconform) echo "https://github.com/yannh/kubeconform/releases/download/$v/kubeconform-$os-$arch.tar.gz" ;;
    buf)
      # buf names platforms the uname way.
      local bos barch
      bos=$([ "$os" = linux ] && echo Linux || echo Darwin)
      if [ "$arch" = amd64 ]; then barch=x86_64
      elif [ "$os" = linux ]; then barch=aarch64
      else barch=arm64
      fi
      echo "https://github.com/bufbuild/buf/releases/download/$v/buf-$bos-$barch" ;;
  esac
}

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
TOOLS_DIR=${NEXUS_TOOLS_DIR:-$REPO/.tools}

case "${1:-}" in
  --list)
    printf 'helm %s\nkubeconform %s\nbuf %s\n' "$HELM_VERSION" "$KUBECONFORM_VERSION" "$BUF_VERSION"
    exit 0 ;;
  --version)
    [ -n "${2:-}" ] || die "usage: $0 --version <tool>"
    version_of "$2"; exit 0 ;;
  "" | -h | --help)
    sed -n '5,10p' "$0" >&2; exit 2 ;;
esac

tool=$1
version=$(version_of "$tool")
read -r os arch <<<"$(platform)"
want=$(sha256_pin "$tool" "$os" "$arch") || die "no pinned checksum for $tool on $os/$arch"
dest="$TOOLS_DIR/$tool-$version"
bin="$dest/$tool"
stamp="$dest/.sha256"

# Cache hit: the binary is there AND the archive it came from matched the pin.
# The stamp records the binary's own hash at install time, so a binary edited
# or replaced afterwards is caught here, not trusted.
if [ -x "$bin" ] && [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$(sha256_of "$bin")" ]; then
  echo "$bin"
  exit 0
fi

command -v curl >/dev/null || die "curl is needed to download $tool $version"
mkdir -p "$TOOLS_DIR"
tmp=$(mktemp -d "$TOOLS_DIR/.dl.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
src=$(url "$tool" "$version" "$os" "$arch")
echo "pinned-tool: downloading $tool $version ($os/$arch)" >&2
curl -fsSL --retry 3 --retry-delay 2 --proto '=https' --tlsv1.2 -o "$tmp/download" "$src" \
  || die "download failed: $src"
got=$(sha256_of "$tmp/download")
if [ "$got" != "$want" ]; then
  die "CHECKSUM MISMATCH for $src
  expected $want
  got      $got
Refusing to install. If the release was re-published, verify it and update the pin."
fi

mkdir -p "$tmp/out"
case "$tool" in
  helm) tar -xzf "$tmp/download" -C "$tmp/out" && mv "$tmp/out/$os-$arch/helm" "$tmp/out/helm.bin" ;;
  kubeconform) tar -xzf "$tmp/download" -C "$tmp/out" kubeconform && mv "$tmp/out/kubeconform" "$tmp/out/kubeconform.bin" ;;
  buf) mv "$tmp/download" "$tmp/out/buf.bin" ;;
esac
chmod 0755 "$tmp/out/$tool.bin"
rm -rf "$dest"
mkdir -p "$dest"
mv "$tmp/out/$tool.bin" "$bin"
sha256_of "$bin" >"$stamp"
echo "pinned-tool: installed $bin (sha256 verified against the pin)" >&2
echo "$bin"
