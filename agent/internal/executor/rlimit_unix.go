//go:build linux || darwin

// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os/exec"
	"strconv"
	"strings"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// applyRlimits installs the workload's resource ceiling.
//
// WHY A SHELL TRAMPOLINE. The Go standard library has no pre-exec hook: there
// is no supported place to call setrlimit between fork and exec, and calling it
// in the agent would lower the AGENT's own limits, eventually killing the agent
// and turning one over-limit task into every sibling task becoming LOST. The
// portable mechanism that does run in the child, after fork and before exec, is
// the shell's own `ulimit` builtin followed by `exec`. It costs one short-lived
// /bin/sh and it is correct on both platforms.
//
// The `exec` in the shell line is load-bearing: without it the shell would
// remain as an extra process, the pid we track would be the shell's rather than
// the workload's, and signals would be delivered to a process that does not
// forward them. With `exec` the shell REPLACES itself, so the pid we hold is the
// real workload.
func applyRlimits(cmd *exec.Cmd, limit *nexusv1.ResourceVector, deadline time.Time) error {
	if limit == nil {
		return nil
	}
	memBytes := limit.GetMemoryBytes()
	cpuSecs := deriveCPUSeconds(limit, deadline)
	fid := SubprocessFidelity()

	var ulimits []string
	if memBytes > 0 && fid.Memory != "none" {
		// ulimit -v takes kilobytes. Rounding UP means we never impose a
		// stricter cap than the scheduler promised the task.
		kb := (memBytes + 1023) / 1024
		ulimits = append(ulimits, "ulimit -v "+strconv.FormatInt(kb, 10))
	}
	if cpuSecs > 0 {
		ulimits = append(ulimits, "ulimit -t "+strconv.FormatInt(cpuSecs, 10))
	}
	if len(ulimits) == 0 {
		return nil
	}

	// Quote every argument for the shell. A task argument containing a quote or
	// a semicolon must not be able to inject shell commands into the workload's
	// own launch line — that would be a command-injection hole reachable from a
	// workflow spec.
	quoted := make([]string, 0, len(cmd.Args))
	for _, a := range cmd.Args {
		quoted = append(quoted, shellQuote(a))
	}
	line := strings.Join(ulimits, "; ") + "; exec " + strings.Join(quoted, " ")

	cmd.Path = "/bin/sh"
	cmd.Args = []string{"/bin/sh", "-c", line}
	return nil
}

// deriveCPUSeconds converts a millicore share into an RLIMIT_CPU ceiling.
//
// This is an APPROXIMATION and a weak one: RLIMIT_CPU caps cumulative CPU
// seconds, which cannot express "half a core". A task limited to 500 millicores
// that runs for its full 60s deadline should consume at most 30 CPU-seconds, so
// we set that as the ceiling. The effect is a backstop against a runaway
// spin-loop, not a share guarantee: within the ceiling the task can and will use
// every core it can grab. Real CPU shares require cgroup cpu.max, which is the
// docker executor's job.
//
// Returns 0 (no limit) when there is no deadline to bound the integral, since a
// cumulative-seconds cap with no time bound would kill a legitimately
// long-running task.
func deriveCPUSeconds(limit *nexusv1.ResourceVector, deadline time.Time) int64 {
	millis := limit.GetCpuMillis()
	if millis <= 0 || deadline.IsZero() {
		return 0
	}
	window := time.Until(deadline).Seconds()
	if window <= 0 {
		return 0
	}
	secs := int64(window*float64(millis)/1000.0) + 1 // +1 so a tiny window is never a 0-second cap
	return secs
}

// shellQuote wraps s in single quotes, escaping embedded single quotes. The
// POSIX-portable form: inside single quotes nothing is special except the
// closing quote itself.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
