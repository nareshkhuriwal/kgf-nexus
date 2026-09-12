#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Takes a complete backup: result-store objects FIRST, then Postgres.
#
# This script exists for one reason: the ORDER of the two halves is a
# correctness property, and leaving it to whoever writes the cron entry means it
# will eventually be got wrong, in parallel, at 3am. Encoding it here makes the
# safe order the default and the only documented way to take a backup.
#
# Neither half is a backup of the platform on its own:
#   deploy/backup.sh           -- Postgres: runs, attempts, leases, timers, and
#                                 results small enough to be inlined (<= 64 KiB).
#   deploy/backup-artifacts.sh -- MinIO: every result too large to inline, which
#                                 Postgres holds only a REFERENCE to (ADR-0008).
#
# Why objects first, in one paragraph -- the full argument is in the header of
# backup-artifacts.sh, which is where it belongs. Two stores cannot be
# snapshotted atomically, so the second one captured always includes writes the
# first one missed. Capturing objects first means the skew can only produce
# ORPHAN OBJECTS (an object no row references: wasted disk, and ADR-0008 already
# mandates a GC sweep for exactly this). Capturing Postgres first would produce
# DANGLING REFS (a row whose object is missing: silent, unrecoverable data loss
# that asserts the data is present). Orphans are recoverable; dangling refs are
# not. Do not parallelise these two -- concurrency discards the ordering that the
# guarantee rests on.
#
# `set -e` is load-bearing, and here it is what makes the ordering meaningful: if
# the object mirror fails, the Postgres dump must NOT run. A Postgres dump with
# no matching mirror is precisely the dangling-ref backup this ordering exists to
# prevent, and it would sit in the directory looking like a valid backup.
set -eu

HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

# One timestamp for both halves, exported so each script names its output with
# the same stamp. Pairing a dump with its mirror by eyeballing two nearby
# timestamps is exactly the sort of thing that goes wrong during an incident;
# an identical stamp makes the pair unambiguous.
BACKUP_STAMP="${BACKUP_STAMP:-$(date -u +%Y%m%dT%H%M%SZ)}"
export BACKUP_STAMP

OUT_DIR="${BACKUP_DIR:-${1:-./backups}}"
export BACKUP_DIR="$OUT_DIR"

echo "backup-all: stamp $BACKUP_STAMP, output $OUT_DIR"
echo "backup-all: [1/2] result store (captured FIRST -- see header)"
"$HERE/backup-artifacts.sh"

echo
echo "backup-all: [2/2] postgres"
"$HERE/backup.sh"

echo
echo "backup-all: complete."
echo "backup-all:   objects : $OUT_DIR/nexus-artifacts-$BACKUP_STAMP"
echo "backup-all:   database: $OUT_DIR/nexus-<stamp>.dump"
echo "backup-all: the dump's stamp is its own (it is taken second, so it is later"
echo "backup-all: by design). Restore with deploy/restore.sh THEN"
echo "backup-all: deploy/restore-artifacts.sh -- that order is also deliberate."
