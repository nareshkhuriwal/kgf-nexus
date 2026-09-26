// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package quickstart

import (
	"os"
	"strings"
)

// agentWorkDirEnv names the host directory the compose agents share with the
// task containers they start (deploy/docker-compose.yml, F144). The docker
// executor bind-mounts an attempt's work directory into its task container by
// PATH, and that path is resolved by the Docker daemon on the host, not inside
// the agent container, so the agent must keep its work directories at a host
// path mounted at the same path in the agent.
const agentWorkDirEnv = "NEXUS_AGENT_WORK_DIR"

// defaultAgentWorkDir must match the compose file's default.
const defaultAgentWorkDir = "/tmp/nexus-agent-work"

// ensureAgentWorkDir creates the shared work directory before compose does.
// Created by the daemon it would be owned by root on a Linux host, and the
// agents (uid 10001) could not write their attempt directories into it. Mode
// 1777, like /tmp: every agent may create entries, and none may remove
// another's.
func ensureAgentWorkDir(env []string) error {
	dir := defaultAgentWorkDir
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, agentWorkDirEnv+"="); ok && v != "" {
			dir = v
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o777 == 0o777 && fi.Mode()&os.ModeSticky != 0 {
		return nil
	}
	// A directory someone else owns with other permissions is left alone
	// rather than failing the whole stack; the agent reports the write error.
	_ = os.Chmod(dir, 0o777|os.ModeSticky)
	return nil
}
