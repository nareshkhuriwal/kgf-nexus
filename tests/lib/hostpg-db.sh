#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Create or drop a migrated throwaway database on the host's PostgreSQL, for
# Make targets and stacks that need one (tests/lib/hostpg.sh has the details).
#
#   tests/lib/hostpg-db.sh create NAME   create NAME, migrate it, print its DSN
#   tests/lib/hostpg-db.sh ensure NAME   create NAME unless it already exists as
#                                        this tool's (a stack's database, which the
#                                        stack's own migration job fills); print its DSN
#   tests/lib/hostpg-db.sh url NAME      print NAME's DSN (as the host reaches it)
#   tests/lib/hostpg-db.sh container-url NAME
#                                        print NAME's DSN as a container on this
#                                        machine reaches it (host.docker.internal)
#   tests/lib/hostpg-db.sh drop NAME     drop NAME, if this tool created it
#
# NEXUS_TEST_PG_SERVER chooses the server; HOSTPG_TAG (default
# org.kgf-nexus.test-db) is the ownership tag written on create and required on
# drop. Only the DSN goes to stdout, so `$(... create NAME)` captures it.
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=tests/lib/hostpg.sh
. tests/lib/hostpg.sh

cmd=${1:-}
name=${2:-}
if [ -z "$cmd" ] || [ -z "$name" ]; then echo "usage: $0 create|ensure|url|container-url|drop NAME" >&2; exit 2; fi
hostpg_valid_name "$name" || hostpg_die "database name '$name' must match ^[a-z_][a-z0-9_]{0,62}\$"

case "$cmd" in
  create)
    hostpg_check >&2
    hostpg_create "$name" >&2
    hostpg_migrate "$(hostpg_url "$HOSTPG_SERVER" "$name")" >/dev/null
    echo "hostpg: created and migrated $name" >&2
    hostpg_url "$HOSTPG_SERVER" "$name"
    ;;
  ensure)
    hostpg_check >&2
    hostpg_ensure "$name"
    hostpg_url "$HOSTPG_SERVER" "$name"
    ;;
  url) hostpg_url "$HOSTPG_SERVER" "$name" ;;
  container-url) hostpg_container_url "$(hostpg_url "$HOSTPG_SERVER" "$name")" ;;
  drop) hostpg_drop "$name" ;;
  *) echo "usage: $0 create|ensure|url|container-url|drop NAME" >&2; exit 2 ;;
esac
