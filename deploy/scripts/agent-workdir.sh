#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Create the host directory the compose agents share with their task containers
# (NEXUS_AGENT_WORK_DIR, default /tmp/nexus-agent-work) with mode 1777, before
# `docker compose up` would.
#
# deploy/docker-compose.yml bind-mounts it at the same path into every agent:
# the docker executor bind-mounts an attempt's work directory into its task
# container by path, and the daemon resolves that path on the host (F144). Left
# to Docker, a missing bind-mount source is created owned by root, and the
# agents (uid 10001) cannot create their attempt directories, so every task
# fails with PermissionDenied ("prepare: create workdir ..."). `nexusctl up`
# does the same in cli/internal/quickstart/workdir.go; this is the equivalent
# for `make up`, its variants and the browser e2e stack.
#
# Prints the directory. Exits 1, with the fix, when the directory exists, is
# owned by someone else and is not already world-writable.
set -euo pipefail

dir=${NEXUS_AGENT_WORK_DIR:-/tmp/nexus-agent-work}
mkdir -p "$dir"

# Like /tmp: every agent may create entries, and none may remove another's.
sticky_world_writable() { [ -n "$(find "$dir" -maxdepth 0 -perm -1777 2>/dev/null)" ]; }

if ! sticky_world_writable; then
  if ! chmod 1777 "$dir" 2>/dev/null || ! sticky_world_writable; then
    echo "agent-workdir: $dir is not writable by the agents (uid 10001) and cannot be fixed by $(id -un)." >&2
    echo "  It was probably created by the Docker daemon as root. Fix it once with:" >&2
    echo "      sudo chmod 1777 '$dir'" >&2
    echo "  or point NEXUS_AGENT_WORK_DIR at another directory." >&2
    exit 1
  fi
fi
echo "$dir"
