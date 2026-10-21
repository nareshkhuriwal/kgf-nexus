#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# The body of a release tag's GitHub Release, on stdout. ci.yml's
# `github-release` job runs it; run it locally to preview a release's notes.
#
#   release-notes.sh TAG CHANGELOG [CHART_JSON DIGESTS_JSON]
#
# TAG is vX.Y.Z or vX.Y.Z-PRE. The notes are:
#
#   1. CHANGELOG's section for the version, without its heading (the release's
#      title names the version). That is `## X.Y.Z-PRE` if there is one;
#      otherwise, for a pre-release, `## X.Y.Z`, the release it is a candidate
#      for, under a line that says so. A heading may bracket the version and
#      follow it with a date (`## [0.3.0] - 2026-10-21`). Relative links are
#      pointed into the tagged tree, because a release page would resolve them
#      against /releases/. A missing section is not an error: the notes are
#      then the rest, and stderr says so (a ::warning:: in Actions).
#   2. With CHART_JSON and DIGESTS_JSON (the release workflow's chart.json and
#      digests.json, which are also the release's assets): how to install
#      exactly this release, the chart by version and every image by digest.
#
# GitHub's generated notes (merged pull requests and new contributors since
# the previous release) are appended by the API, not here
# (`gh release create --generate-notes`).
#
# GITHUB_REPOSITORY (owner/name, set in Actions) and GITHUB_SERVER_URL name the
# repository that links point into. ATTESTED=true adds the commands that verify
# the provenance attestations, which the release workflow attaches only when
# the repository is public.
set -euo pipefail

usage() {
  echo "usage: $0 TAG CHANGELOG [CHART_JSON DIGESTS_JSON]" >&2
  exit 2
}
[ $# -eq 2 ] || [ $# -eq 4 ] || usage
tag=$1 changelog=$2 chart_json=${3:-} digests_json=${4:-}

# The release workflow's own rule (release.yml, plan): SemVer, v-prefixed.
if ! [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$ ]]; then
  echo "release-notes: '$tag' is not a release tag (vMAJOR.MINOR.PATCH[-PRERELEASE])" >&2
  exit 2
fi
[ -f "$changelog" ] || { echo "release-notes: $changelog does not exist" >&2; exit 2; }
version=${tag#v}
core=${version%%-*}
: "${GITHUB_REPOSITORY:?names the repository the notes link into (owner/name)}"
repo_url="${GITHUB_SERVER_URL:-https://github.com}/$GITHUB_REPOSITORY"

# The lines under the `## <version>` heading, up to the next `## ` heading,
# without leading or trailing blank lines. Nothing when there is no such
# heading. `###` and deeper headings are part of the section.
section() {
  awk -v want="$1" '
    /^## / {
      if (found) exit
      v = $2
      sub(/^\[/, "", v)
      sub(/\]$/, "", v)
      if (v == want) found = 1
      next
    }
    found {
      if ($0 ~ /^[ \t]*$/) { if (printed) blank++; next }
      for (; blank > 0; blank--) print ""
      print
      printed = 1
    }
  ' "$changelog"
}

# [text](relative/path) -> [text](<repo>/blob/<tag>/relative/path). Absolute
# URLs (anything with a ':'), root-relative paths and #fragments are kept.
absolute_links() {
  sed -E "s|\]\(([^):/#][^):]*)\)|](${repo_url}/blob/${tag}/\1)|g"
}

notes=$(section "$version")
from=$version
if [ -z "$notes" ] && [ "$version" != "$core" ]; then
  notes=$(section "$core")
  from=$core
fi
if [ -n "$notes" ]; then
  if [ "$from" != "$version" ]; then
    printf '> **Pre-release of %s.** Not for production. The notes are those of %s as they stand at this tag.\n\n' "$core" "$core"
  fi
  absolute_links <<<"$notes"
else
  wanted="## $version"
  [ "$version" != "$core" ] && wanted="$wanted or ## $core"
  echo "::warning::$changelog has no section for $version ($wanted): the release notes are the install section and GitHub's generated notes only" >&2
fi

[ -n "$chart_json" ] || exit 0

for f in "$chart_json" "$digests_json"; do
  [ -s "$f" ] || { echo "release-notes: $f does not exist or is empty" >&2; exit 1; }
done
chart=$(jq -er .chart "$chart_json")
chart_version=$(jq -er .version "$chart_json")
chart_digest=$(jq -er .digest "$chart_json")
app_version=$(jq -er .appVersion "$chart_json")
# The release workflow enforces both; a release that says otherwise is wrong.
if [ "$chart_version" != "$version" ] || [ "$app_version" != "$tag" ]; then
  echo "release-notes: $chart_json is chart $chart_version (appVersion $app_version), not $version ($tag)" >&2
  exit 1
fi
image_count=$(jq -er 'length' "$digests_json")

[ -n "$notes" ] && echo
cat <<EOF
## Install

The Helm chart is \`$chart\`, version \`$chart_version\` (\`$chart_digest\`).
The images are tagged \`$tag\`. \`release-images.yaml\` pins all $image_count
of them to the digests this release published; pass it last.

\`\`\`sh
helm upgrade --install nexus $chart --version $chart_version -n nexus \\
  -f my-values.yaml -f release-images.yaml --set image.tag=$tag
\`\`\`

[docs/INSTALL-AKS.md]($repo_url/blob/$tag/docs/INSTALL-AKS.md) installs it on an existing AKS cluster.
[docs/OPERATIONS.md]($repo_url/blob/$tag/docs/OPERATIONS.md) §22.4 describes how the release is built and published.

| Image | Reference |
|---|---|
EOF
jq -r 'to_entries[] | "| \(.key) | `\(.value)` |"' "$digests_json"
cat <<EOF

| Asset | What it is |
|---|---|
| \`kgf-nexus-$chart_version.tgz\` | the chart package, byte for byte the one in GHCR |
| \`release-images.yaml\` | Helm values: \`image.registry\`, \`image.repositoryPrefix\`, \`image.digests\` |
| \`digests.json\` | every image's digest-pinned reference (also for \`deploy/render_compose.py --digests\`) |
| \`chart.json\` | the chart's OCI reference, version, digest and \`appVersion\` |
EOF

if [ "${ATTESTED:-}" = true ]; then
  first=$(jq -er 'to_entries[0].value' "$digests_json")
  cat <<EOF

Every image and the chart carry a Sigstore-signed SLSA build-provenance
attestation from the release workflow:

\`\`\`sh
gh attestation verify oci://$first --repo $GITHUB_REPOSITORY
gh attestation verify $chart:$chart_version --repo $GITHUB_REPOSITORY
\`\`\`
EOF
fi
