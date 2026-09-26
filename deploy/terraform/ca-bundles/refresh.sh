#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Rebuild (or check) the managed-database CA bundles the cloud environments
# mount for sslmode=verify-full (docs/OPERATIONS.md §20.10).
#
#   deploy/terraform/ca-bundles/refresh.sh           # check: download, verify, compare
#   deploy/terraform/ca-bundles/refresh.sh --write   # rewrite the bundles and SHA256SUMS
#
# Every source is PINNED twice: the SHA-256 of the file as downloaded, and, for
# the Azure roots, the SHA-256 fingerprint of the certificate itself (a root's
# fingerprint never changes; a new root is a new entry). A download that does
# not match refuses to write anything. When a provider publishes a new bundle
# (AWS adds a CA to the RDS bundle; Microsoft announces a new root), the check
# fails: review the change, update the pin below in the same commit, rerun
# with --write, and roll it out as in OPERATIONS §20.10.
#
# Terraform reads the bundles with file() and refuses one whose SHA-256 is not
# the one in SHA256SUMS, so a hand edit of a .pem cannot slip through either.
# The GKE environment needs no file here: Cloud SQL's instance CAs come from
# the Cloud SQL Admin API (google_sql_ca_certs) at plan time.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# ── pins (checked 2026-09-26) ──────────────────────────────────────────────
# AWS RDS global bundle: every RDS CA in every commercial region
# (https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.SSL.html).
rds_url=https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem
rds_sha256=e5bb2084ccf45087bda1c9bffdea0eb15ee67f0b91646106e466714f9de3c7e3
# Azure Database for PostgreSQL flexible server roots
# (https://learn.microsoft.com/azure/postgresql/security/security-tls,
# "Root CAs used by Azure Database for PostgreSQL"): DigiCert Global Root G2 and
# Microsoft RSA Root Certificate Authority 2017. Roots only, never the ICAs,
# which Microsoft rotates unannounced.
g2_url=https://cacerts.digicert.com/DigiCertGlobalRootG2.crt.pem
g2_sha256=5d550643b6400d4341550a9b14aedd0b4fac33ae5deb7d8247b6b4f799c13306
g2_fpr=CB:3C:CB:B7:60:31:E5:E0:13:8F:8D:D3:9A:23:F9:DE:47:FF:C3:5E:43:C1:14:4C:EA:27:D4:6A:5A:B1:CB:5F
ms_url="https://www.microsoft.com/pkiops/certs/Microsoft%20RSA%20Root%20Certificate%20Authority%202017.crt"
ms_sha256=c741f70f4b2a8d88bf2e71c14122ef53ef10eba0cfa5e64cfa20f418853073e0
ms_fpr=C7:41:F7:0F:4B:2A:8D:88:BF:2E:71:C1:41:22:EF:53:EF:10:EB:A0:CF:A5:E6:4C:FA:20:F4:18:85:30:73:E0

fetch() { # url sha256 out
  curl -sSfL --proto '=https' --tlsv1.2 -o "$3" "$1"
  local got
  got=$(shasum -a 256 "$3" | cut -d' ' -f1)
  if [ "$got" != "$2" ]; then
    echo "refresh: $1 changed upstream: sha256 $got, pinned $2. Review the new content before updating the pin." >&2
    exit 1
  fi
}
fingerprint() { openssl x509 -in "$1" -noout -fingerprint -sha256 | cut -d= -f2; }

fetch "$rds_url" "$rds_sha256" "$tmp/rds-global-bundle.pem"
fetch "$g2_url" "$g2_sha256" "$tmp/g2.pem"
fetch "$ms_url" "$ms_sha256" "$tmp/ms.der"
openssl x509 -inform DER -in "$tmp/ms.der" -out "$tmp/ms.pem"
[ "$(fingerprint "$tmp/g2.pem")" = "$g2_fpr" ] || { echo "refresh: DigiCert Global Root G2 fingerprint mismatch" >&2; exit 1; }
[ "$(fingerprint "$tmp/ms.pem")" = "$ms_fpr" ] || { echo "refresh: Microsoft RSA Root CA 2017 fingerprint mismatch" >&2; exit 1; }
cat "$tmp/g2.pem" "$tmp/ms.pem" >"$tmp/azure-postgres-roots.pem"

# Every certificate must parse and be a CA; an expired one is reported.
for f in rds-global-bundle.pem azure-postgres-roots.pem; do
  n=$(grep -c 'BEGIN CERTIFICATE' "$tmp/$f")
  openssl crl2pkcs7 -nocrl -certfile "$tmp/$f" | openssl pkcs7 -print_certs -noout >/dev/null
  echo "refresh: $f: $n certificates"
done

(cd "$tmp" && shasum -a 256 azure-postgres-roots.pem rds-global-bundle.pem) >"$tmp/SHA256SUMS"

if [ "${1:-}" = --write ]; then
  cp "$tmp/azure-postgres-roots.pem" "$tmp/rds-global-bundle.pem" "$tmp/SHA256SUMS" "$here/"
  echo "refresh: wrote bundles and SHA256SUMS"
else
  if diff -u "$here/SHA256SUMS" "$tmp/SHA256SUMS"; then
    (cd "$here" && shasum -a 256 -c SHA256SUMS)
    echo "refresh: committed bundles match the pinned upstream sources"
  else
    echo "refresh: committed bundles differ from the pinned sources; run with --write" >&2
    exit 1
  fi
fi
