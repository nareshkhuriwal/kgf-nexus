//go:build darwin

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"syscall"
)

func runtimeName() string { return "darwin" }

// SubprocessFidelity reports the truth on macOS: there is NO memory
// enforcement available to an unprivileged process here. Darwin accepts
// RLIMIT_AS and does not honour it, so pretending otherwise would be worse than
// admitting it — an operator who believes a limit is enforced sizes their nodes
// on that belief.
//
// Consequence: on macOS the subprocess executor refuses tasks that declare a
// memory limit unless AllowUnenforceableLimits is set. Development on a laptop
// sets it; a production worker uses docker.
func SubprocessFidelity() LimitFidelity {
	return LimitFidelity{Memory: "none", CPU: "cpu-seconds", GPU: "none"}
}

// maxRSSBytes converts rusage.Maxrss, which Darwin reports in BYTES (unlike
// Linux, which reports kilobytes).
func maxRSSBytes(ru *syscall.Rusage) int64 { return ru.Maxrss }
