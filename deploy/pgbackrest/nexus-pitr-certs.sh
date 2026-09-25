#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Generates a throwaway CA and a server certificate for the DEV backup store.
#
# pgBackRest's S3 client speaks TLS only, so the local backup store needs a
# certificate. This makes one on first start and leaves it alone afterwards, so
# the CA that pgBackRest trusts (PGBACKREST_REPO1_STORAGE_CA_FILE) stays stable
# across restarts. It is DEV ONLY: production object storage (S3, Azure Blob,
# GCS, a real MinIO deployment) already has a publicly- or internally-issued
# certificate, and the overlay's CA file setting is simply dropped.
#
# Usage: nexus-pitr-certs <out-dir> <dns-name> [<dns-name> ...]
# Writes MinIO's naming: <out-dir>/public.crt, private.key, CAs/ca.crt, ca.crt
set -eu

OUT="${1:?usage: nexus-pitr-certs <out-dir> <dns-name>...}"
shift
[ "$#" -ge 1 ] || { echo "nexus-pitr-certs: at least one DNS name is required" >&2; exit 2; }

if [ -s "$OUT/public.crt" ] && [ -s "$OUT/private.key" ] && [ -s "$OUT/ca.crt" ]; then
  echo "nexus-pitr-certs: certificates already present in $OUT, keeping them"
  exit 0
fi

mkdir -p "$OUT/CAs"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

san=""
for name in "$@"; do san="${san:+$san,}DNS:$name"; done

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -days 825 -subj "/CN=nexus-pitr-dev-ca" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -keyout "$work/ca.key" -out "$work/ca.crt" 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -subj "/CN=$1" -keyout "$work/server.key" -out "$work/server.csr" 2>/dev/null
printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\nkeyUsage=critical,digitalSignature\n' "$san" > "$work/ext"
openssl x509 -req -in "$work/server.csr" -CA "$work/ca.crt" -CAkey "$work/ca.key" \
  -CAcreateserial -days 825 -extfile "$work/ext" -out "$work/server.crt" 2>/dev/null

cp "$work/server.crt" "$OUT/public.crt"
cp "$work/server.key" "$OUT/private.key"
cp "$work/ca.crt" "$OUT/ca.crt"
cp "$work/ca.crt" "$OUT/CAs/ca.crt"
# The CA key is discarded: nothing ever needs to sign a second leaf, and a CA key
# that does not exist cannot leak. Delete the volume to rotate.
# World-readable because the store runs as a different uid than this job; this is
# a dev-only volume mounted into the backup store alone.
chmod 0644 "$OUT/public.crt" "$OUT/private.key" "$OUT/ca.crt" "$OUT/CAs/ca.crt"
echo "nexus-pitr-certs: wrote CA and server certificate for $* into $OUT"
