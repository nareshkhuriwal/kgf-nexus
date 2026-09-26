// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package secrets

import "syscall"

const prSetDumpable = 4 // PR_SET_DUMPABLE, <linux/prctl.h>

// HardenProcess makes the agent non-dumpable and disables core files.
//
// PR_SET_DUMPABLE=0 does two things: the kernel writes no core dump, and
// /proc/<pid>/{mem,environ,maps} become root-owned, so a subprocess task
// running as the agent's own UID can no longer ptrace or read the agent's
// memory -- where every resolved secret, and the agent's own credential,
// lives. RLIMIT_CORE=0 is inherited by tasks too, which is intended.
func HardenProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
}
