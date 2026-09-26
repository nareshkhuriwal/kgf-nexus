// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// fakeDockerInspect writes a `docker` stand-in whose `inspect` answers with
// OOMKilled=false for the first `lateAfter` calls and true afterwards, the way
// the daemon does when containerd delivers the task's exit before its OOM
// event: `docker wait` returns on the exit, and the flag is set only when the
// OOM event is processed, a moment later (longer on a loaded host).
func fakeDockerInspect(t *testing.T, exitCode int, oomAfter int) (bin string, calls func() int) {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "calls")
	script := `#!/bin/sh
[ "$1" = inspect ] || exit 0
n=$(cat "` + counter + `" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "` + counter + `"
oom=false
if [ "` + strconv.Itoa(oomAfter) + `" -ge 0 ] && [ "$n" -gt ` + strconv.Itoa(oomAfter) + ` ]; then oom=true; fi
if [ "$2" = --format ]; then printf '%s' "$oom"; exit 0; fi
printf '[{"State":{"ExitCode":` + strconv.Itoa(exitCode) + `,"OOMKilled":%s,"Error":"","StartedAt":"2026-09-26T10:00:00Z","FinishedAt":"2026-09-26T10:00:01Z"}}]' "$oom"
`
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		b, _ := os.ReadFile(counter)
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
}

func oomState(t *testing.T, memory int64) *dockerState {
	return &dockerState{
		containerID: "c0ffee",
		startedA:    time.Now(),
		workDir:     t.TempDir(),
		limit:       &nexusv1.ResourceVector{MemoryBytes: memory},
	}
}

// TestDockerOOMFlagThatSettlesAfterTheExitIsReported is the deterministic form
// of TestConformanceMemoryLimitEnforced/docker failing under load with "did not
// report OOMKilled": the outcome was read from ONE inspect straight after
// `docker wait`, before the daemon had recorded the OOM event.
func TestDockerOOMFlagThatSettlesAfterTheExitIsReported(t *testing.T) {
	bin, calls := fakeDockerInspect(t, 137, 2)
	e := &DockerExecutor{Binary: bin}
	o := e.inspectOutcome(oomState(t, 32<<20), false)
	if !o.OOMKilled {
		t.Fatalf("OOMKilled = false after %d inspects; a kill by the kernel OOM killer was reported as a plain SIGKILL", calls())
	}
	if o.PeakMemoryBytes != 32<<20 {
		t.Errorf("peak = %d", o.PeakMemoryBytes)
	}
}

// The settle wait is bounded and spent only when an OOM is plausible: a
// SIGKILLed container with a memory limit.
func TestDockerOOMSettleIsBoundedAndTargeted(t *testing.T) {
	t.Run("a SIGKILL that was not an OOM costs at most the settle window", func(t *testing.T) {
		bin, _ := fakeDockerInspect(t, 137, -1)
		e := &DockerExecutor{Binary: bin}
		start := time.Now()
		o := e.inspectOutcome(oomState(t, 32<<20), false)
		if o.OOMKilled {
			t.Fatal("reported an OOM the daemon never recorded")
		}
		if el := time.Since(start); el > oomSettleWindow+2*time.Second {
			t.Fatalf("took %s; the settle window is %s", el, oomSettleWindow)
		}
	})
	for _, tc := range []struct {
		name     string
		exit     int
		memory   int64
		timedOut bool
	}{
		{"a clean exit", 0, 32 << 20, false},
		{"an application failure", 1, 32 << 20, false},
		{"a SIGKILL with no memory limit", 137, 0, false},
		{"a deadline kill", 137, 32 << 20, true},
	} {
		t.Run(tc.name+" is read once", func(t *testing.T) {
			bin, calls := fakeDockerInspect(t, tc.exit, 5)
			e := &DockerExecutor{Binary: bin}
			_ = e.inspectOutcome(oomState(t, tc.memory), tc.timedOut)
			if n := calls(); n != 1 {
				t.Fatalf("inspected %d times; only a SIGKILL under a memory limit waits for the OOM flag", n)
			}
		})
	}
}

// A workload that ran for any time at all reports a non-zero wall time; the
// subprocess conformance test failed on fast Linux hosts because a trivial
// shell command finished inside one millisecond and truncated to 0.
func TestElapsedMsRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want int64
	}{{0, 0}, {-time.Second, 0}, {time.Microsecond, 1}, {999 * time.Microsecond, 1}, {time.Millisecond, 1}, {1500 * time.Microsecond, 2}, {2 * time.Second, 2000}} {
		if got := elapsedMs(tc.d); got != tc.want {
			t.Errorf("elapsedMs(%s) = %d, want %d", tc.d, got, tc.want)
		}
	}
}
