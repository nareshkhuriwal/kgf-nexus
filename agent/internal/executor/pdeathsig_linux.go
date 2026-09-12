//go:build linux

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import "syscall"

// setPdeathsig makes the kernel SIGKILL the workload if the agent thread that
// spawned it dies. Without it, a SIGKILLed agent (which runs no cleanup code)
// leaves the workload running and holding capacity the ledger believes is free.
func setPdeathsig(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
