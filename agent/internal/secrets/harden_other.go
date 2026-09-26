// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !linux

package secrets

import "syscall"

// HardenProcess disables core files. There is no portable equivalent of
// Linux's PR_SET_DUMPABLE here, so a same-UID process can still inspect the
// agent on these platforms (development hosts, per ADR-0007).
func HardenProcess() error {
	return syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0})
}
