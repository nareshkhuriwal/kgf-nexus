//go:build linux

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"syscall"
)

func runtimeName() string { return "linux" }

// SubprocessFidelity reports what the subprocess executor can actually enforce
// on Linux.
//
// Memory is "address-space", not "hard": the limit is applied with RLIMIT_AS,
// which caps VIRTUAL address space rather than resident set. A workload that
// reserves a large mapping without touching it dies where a cgroup would have
// allowed it, and a workload that catches the allocation failure keeps running
// where a cgroup would have killed it. Labelling this honestly is what keeps
// ADR-0007's "development-grade" claim truthful instead of decorative — the
// alternative is an operator who believes their memory limits are enforced.
//
// A real cgroup v2 implementation (writing memory.max in a delegated subtree)
// is the Phase 2 upgrade; it needs cgroup delegation that a developer laptop
// and an unprivileged CI container generally do not have.
func SubprocessFidelity() LimitFidelity {
	return LimitFidelity{Memory: "address-space", CPU: "cpu-seconds", GPU: "env-hint"}
}

// maxRSSBytes converts rusage.Maxrss, which Linux reports in KILOBYTES. Getting
// this unit wrong silently inflates or deflates every peak-memory metric and
// every OOM inference drawn from it, which is why it is platform-specific code
// rather than a shared constant.
func maxRSSBytes(ru *syscall.Rusage) int64 { return ru.Maxrss * 1024 }
