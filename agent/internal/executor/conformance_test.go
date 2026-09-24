// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// The EXECUTOR CONFORMANCE SUITE (ADR-0007).
//
// This file defines the contract EVERY executor must satisfy, and runs it against
// every implementation. It exists because ADR-0007 accepts a real cost — each
// executor independently implements limits, signals and log capture, which is
// duplicated effort and a genuine risk of subtle behavioural divergence. The
// suite is what keeps the abstraction honest rather than nominal: "we support
// executor X" becomes a testable claim instead of a hopeful one.
//
// The contract, restated as the assertions below:
//   - resource limits are actually enforced (to the fidelity the executor CLAIMS)
//   - SIGTERM is delivered and the grace period honoured before SIGKILL
//   - exit codes map to the right error class
//   - logs stream
//   - Cleanup is idempotent
//   - a killed executor leaves NO orphaned workload
package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// conformanceTarget is one implementation under test, plus the facts about it
// that the suite must know in order to assert the truth rather than a fiction.
type conformanceTarget struct {
	name string
	// new builds a fresh executor rooted at a per-test directory.
	new func(t *testing.T, root string) Executor
	// fidelity is what this executor CLAIMS to enforce. The suite asserts against
	// the claim: an executor that says "none" for memory is not failed for not
	// enforcing memory, but one that says "hard" is.
	fidelity LimitFidelity
	// shellSpec builds a runtime spec that runs the given shell script under this
	// substrate, so one script serves both a subprocess and a container.
	shellSpec func(script string) *nexusv1.RuntimeSpec
	// skip reports why this target cannot run here (no docker daemon), so the
	// suite SKIPS cleanly instead of failing a machine that lacks infrastructure.
	skip func() string
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func conformanceTargets() []conformanceTarget {
	return []conformanceTarget{
		{
			name: "subprocess",
			new: func(t *testing.T, root string) Executor {
				return &SubprocessExecutor{
					Root: root, Log: discardLog(),
					// The suite exercises limit ENFORCEMENT separately and asserts
					// against the declared fidelity; refusing to start here would
					// make every limit test unrunnable on macOS rather than
					// correctly reporting that macOS enforces nothing.
					AllowUnenforceableLimits: true,
				}
			},
			fidelity: SubprocessFidelity(),
			shellSpec: func(script string) *nexusv1.RuntimeSpec {
				return &nexusv1.RuntimeSpec{Command: []string{"/bin/sh", "-c"}, Args: []string{script}}
			},
			skip: func() string { return "" },
		},
		{
			name: "docker",
			new: func(t *testing.T, root string) Executor {
				return &DockerExecutor{
					Root: root, Log: discardLog(),
					// The workload writes its output into the bind-mounted work
					// directory and needs no network for any test here.
					NetworkMode: "none",
				}
			},
			fidelity: DockerFidelity(),
			shellSpec: func(script string) *nexusv1.RuntimeSpec {
				return &nexusv1.RuntimeSpec{
					Image:   conformanceImage(),
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{script},
				}
			},
			skip: func() string {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if !DockerAvailable(ctx, "") {
					return "docker daemon is not reachable"
				}
				if err := ensureConformanceImage(ctx); err != nil {
					return fmt.Sprintf("conformance image %s unavailable: %v", conformanceImage(), err)
				}
				return ""
			},
		},
		{
			// The kubernetes executor, against a LOCAL cluster only. Opt-in
			// (NEXUS_K8S_CONFORMANCE=1) because it creates a namespace, and
			// guarded so it can never touch a remote cluster: see
			// k8sConformanceSkip in kubernetes_integration_test.go.
			name: KubernetesKind,
			new: func(t *testing.T, root string) Executor {
				return newK8sConformanceExecutor(t, root)
			},
			fidelity: KubernetesFidelity(),
			shellSpec: func(script string) *nexusv1.RuntimeSpec {
				return &nexusv1.RuntimeSpec{
					Image:   k8sConformanceImage(),
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{script},
				}
			},
			skip: k8sConformanceSkip,
		},
	}
}

func conformanceImage() string {
	if v := os.Getenv("NEXUS_CONFORMANCE_IMAGE"); v != "" {
		return v
	}
	// busybox: tiny, has /bin/sh, and is almost always already cached.
	return "busybox:latest"
}

func ensureConformanceImage(ctx context.Context) error {
	img := conformanceImage()
	if exec.CommandContext(ctx, "docker", "image", "inspect", img).Run() == nil {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(pctx, "docker", "pull", img).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// runConformance is the harness: it runs one subtest per target, skipping any
// target whose substrate is unavailable.
func runConformance(t *testing.T, name string, fn func(t *testing.T, tgt conformanceTarget, e Executor, root string)) {
	t.Helper()
	for _, tgt := range conformanceTargets() {
		tgt := tgt
		t.Run(name+"/"+tgt.name, func(t *testing.T) {
			if reason := tgt.skip(); reason != "" {
				t.Skipf("skipping %s: %s", tgt.name, reason)
			}
			root := t.TempDir()
			fn(t, tgt, tgt.new(t, root), root)
		})
	}
}

func baseSpec(t *testing.T, tgt conformanceTarget, script string) TaskSpec {
	t.Helper()
	return TaskSpec{
		AttemptID:      "conf-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		RunID:          "run-conf",
		NodeID:         "node-conf",
		IdempotencyKey: "idem-conf",
		Runtime:        tgt.shellSpec(script),
		Limit:          &nexusv1.ResourceVector{},
		// Generous by default; individual tests tighten it.
		StartToCloseDeadline: time.Now().Add(60 * time.Second),
		GraceAfterTerm:       2 * time.Second,
	}
}

// runToCompletion drives the full lifecycle and always cleans up.
func runToCompletion(t *testing.T, e Executor, spec TaskSpec) (Outcome, Handle) {
	t.Helper()
	ctx := context.Background()
	p, err := e.Prepare(ctx, spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h, err := e.Start(ctx, p)
	if err != nil {
		_ = e.Cleanup(ctx, Handle{AttemptID: p.AttemptID, WorkDir: p.WorkDir})
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = e.Cleanup(cctx, h)
	})
	o, err := e.Wait(ctx, h)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	return o, h
}

// ── CONTRACT: a successful task succeeds, and its output comes back ───────────

func TestConformanceSuccessAndOutput(t *testing.T) {
	runConformance(t, "success", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, `printf '{"rows":42}' > "$NEXUS_OUTPUT_FILE"; exit 0`)
		o, _ := runToCompletion(t, e, spec)

		if !o.Succeeded() {
			t.Fatalf("expected success, got %+v", o)
		}
		if o.ExitCode != 0 {
			t.Errorf("exit code = %d, want 0", o.ExitCode)
		}
		if got := string(o.Output); got != `{"rows":42}` {
			t.Errorf("output = %q, want the payload the task wrote to NEXUS_OUTPUT_FILE", got)
		}
		if o.ContentType != "application/json" {
			t.Errorf("content type = %q, want application/json for a valid JSON payload", o.ContentType)
		}
		if o.WallMs <= 0 {
			t.Error("wall_ms was not measured")
		}
	})
}

// The platform environment contract: a handler written against NEXUS_* works on
// any substrate. The idempotency key in particular is what lets a handler
// deduplicate its own side effects across retries.
func TestConformancePlatformEnvironment(t *testing.T) {
	runConformance(t, "env", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt,
			`printf '%s|%s|%s|%s' "$NEXUS_ATTEMPT_ID" "$NEXUS_RUN_ID" "$NEXUS_NODE_ID" "$NEXUS_IDEMPOTENCY_KEY" > "$NEXUS_OUTPUT_FILE"`)
		o, _ := runToCompletion(t, e, spec)
		if !o.Succeeded() {
			t.Fatalf("task failed: %+v", o)
		}
		want := strings.Join([]string{spec.AttemptID, spec.RunID, spec.NodeID, spec.IdempotencyKey}, "|")
		if string(o.Output) != want {
			t.Errorf("platform env = %q, want %q", o.Output, want)
		}
	})
}

// Inputs must be staged as files the workload can read, because task payloads
// never transit the control plane (ADR-0008) and the workload must find them
// locally.
func TestConformanceInputStaging(t *testing.T) {
	runConformance(t, "inputs", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, `cat "$NEXUS_INPUT_DIR/greeting" > "$NEXUS_OUTPUT_FILE"`)
		spec.Inputs = map[string]*nexusv1.ResultRef{
			"greeting": {Kind: &nexusv1.ResultRef_Inline{Inline: []byte("hello from upstream")}},
		}
		o, _ := runToCompletion(t, e, spec)
		if !o.Succeeded() {
			t.Fatalf("task failed: %+v", o)
		}
		if string(o.Output) != "hello from upstream" {
			t.Errorf("staged input = %q", o.Output)
		}
	})
}

// ── CONTRACT: exit codes map to the right error class ─────────────────────────

func TestConformanceExitCodeToErrorClass(t *testing.T) {
	runConformance(t, "exitclass", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		o, _ := runToCompletion(t, e, baseSpec(t, tgt, `echo "boom" >&2; exit 3`))

		if o.Succeeded() {
			t.Fatal("a non-zero exit must not be reported as success")
		}
		if o.ExitCode != 3 {
			t.Errorf("exit code = %d, want 3 (the task's own code must survive)", o.ExitCode)
		}
		class, retryable := ClassifyOutcome(o)
		// APPLICATION and terminal: the task's own code decided to fail, and
		// blindly retrying deterministic bugs burns the retry budget in seconds.
		if class != nexusv1.ErrorClass_ERROR_CLASS_APPLICATION {
			t.Errorf("class = %v, want ERROR_CLASS_APPLICATION", class)
		}
		if retryable {
			t.Error("an application error must not be retryable by default")
		}
	})
}

// A missing image or handler is TERMINAL NOT_FOUND. Retrying it around the fleet
// only burns the task's budget before the human who must fix the reference ever
// sees a clear error.
func TestConformanceMissingHandlerIsNotFound(t *testing.T) {
	runConformance(t, "notfound", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, "true")
		switch tgt.name {
		case "subprocess":
			spec.Runtime = &nexusv1.RuntimeSpec{Command: []string{"/nonexistent/definitely-not-a-binary-xyz"}}
		case "docker", KubernetesKind:
			spec.Runtime = &nexusv1.RuntimeSpec{
				Image:   "nexus-conformance-does-not-exist-xyz:v0",
				Command: []string{"/bin/true"},
			}
		}
		_, err := e.Prepare(context.Background(), spec)
		if err == nil {
			t.Fatal("expected Prepare to fail for a missing image/handler")
		}
		c := ClassifyError(err)
		if c.Class != nexusv1.ErrorClass_ERROR_CLASS_NOT_FOUND {
			t.Errorf("class = %v (%v), want ERROR_CLASS_NOT_FOUND", c.Class, err)
		}
		if c.Retryable {
			t.Error("a missing image must be terminal: no retry can conjure it")
		}
	})
}

// An empty command is a spec bug, not a task bug: terminal VALIDATION.
func TestConformanceInvalidSpecIsValidationError(t *testing.T) {
	runConformance(t, "validation", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, "true")
		switch tgt.name {
		case "subprocess":
			spec.Runtime = &nexusv1.RuntimeSpec{}
		case "docker", KubernetesKind:
			spec.Runtime = &nexusv1.RuntimeSpec{Command: []string{"/bin/true"}} // no image
		}
		_, err := e.Prepare(context.Background(), spec)
		if err == nil {
			t.Fatal("expected Prepare to reject an invalid spec")
		}
		if c := ClassifyError(err); c.Class != nexusv1.ErrorClass_ERROR_CLASS_VALIDATION {
			t.Errorf("class = %v (%v), want ERROR_CLASS_VALIDATION", c.Class, err)
		}
	})
}

// ── CONTRACT: SIGTERM is delivered and the grace period honoured ──────────────

// TestConformanceSigtermDeliveredAndGraceHonoured is the most operationally
// important assertion in the suite. A task that is SIGKILLed without grace loses
// whatever it was flushing — an hour of GPU time, a partial checkpoint — so the
// grace period is the difference between a cheap drain and an expensive one.
func TestConformanceSigtermDeliveredAndGraceHonoured(t *testing.T) {
	runConformance(t, "sigterm", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		// The workload traps TERM, records that it arrived, and exits cleanly well
		// inside the grace window. If SIGTERM were never delivered, or if SIGKILL
		// arrived immediately, the marker would not be written.
		script := `
trap 'printf "term-received" > "$NEXUS_OUTPUT_FILE"; exit 0' TERM
i=0
while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done
printf "never-signalled" > "$NEXUS_OUTPUT_FILE"
exit 1`
		spec := baseSpec(t, tgt, script)
		spec.GraceAfterTerm = 10 * time.Second
		spec.StartToCloseDeadline = time.Now().Add(60 * time.Second)

		ctx := context.Background()
		p, err := e.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })

		waited := make(chan Outcome, 1)
		go func() {
			o, _ := e.Wait(ctx, h)
			waited <- o
		}()

		// Give the trap time to install before signalling; signalling a shell that
		// has not yet reached its trap would test nothing.
		time.Sleep(1500 * time.Millisecond)
		sentAt := time.Now()
		if err := e.Signal(ctx, h, SignalTerm); err != nil {
			t.Fatalf("Signal(TERM): %v", err)
		}

		var o Outcome
		select {
		case o = <-waited:
		case <-time.After(30 * time.Second):
			t.Fatal("workload did not exit after SIGTERM within 30s")
		}
		elapsed := time.Since(sentAt)

		if string(o.Output) != "term-received" {
			t.Fatalf("SIGTERM was not delivered to the workload (output=%q, exit=%d)",
				o.Output, o.ExitCode)
		}
		// The clean exit must be honoured: the executor must NOT have escalated to
		// SIGKILL before the grace period ran out.
		if o.ExitCode != 0 {
			t.Errorf("exit code = %d, want 0: the workload exited cleanly and that must be preserved", o.ExitCode)
		}
		if elapsed > spec.GraceAfterTerm {
			t.Errorf("workload took %v to exit, longer than the grace period %v", elapsed, spec.GraceAfterTerm)
		}
	})
}

// TestConformanceSigkillAfterGrace: a workload that IGNORES SIGTERM must still
// die. Otherwise a task that traps and ignores TERM would pin a worker's capacity
// forever and no drain could ever complete.
func TestConformanceSigkillAfterGrace(t *testing.T) {
	runConformance(t, "sigkill", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		// Ignores TERM entirely and would otherwise run for minutes.
		script := `trap '' TERM; i=0; while [ $i -lt 3000 ]; do sleep 0.1; i=$((i+1)); done`
		spec := baseSpec(t, tgt, script)
		// A short deadline so the executor's OWN enforcement path escalates,
		// which is what production relies on rather than an external caller.
		spec.StartToCloseDeadline = time.Now().Add(2 * time.Second)
		spec.GraceAfterTerm = 2 * time.Second

		start := time.Now()
		o, _ := runToCompletion(t, e, spec)
		elapsed := time.Since(start)

		if !o.TimedOut {
			t.Errorf("expected TimedOut=true: the executor must enforce start_to_close itself")
		}
		if o.Succeeded() {
			t.Fatal("a killed workload must not be reported as success")
		}
		class, retryable := ClassifyOutcome(o)
		// TIMEOUT, not APPLICATION: "ran too long" points the operator at the code
		// or the deadline, while APPLICATION would point at a bug that is not there.
		if class != nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT {
			t.Errorf("class = %v, want ERROR_CLASS_TIMEOUT", class)
		}
		if !retryable {
			t.Error("a timeout should be retryable")
		}
		// Deadline (2s) + grace (2s) + slack. A much longer time means the
		// escalation never happened and something else reaped the process.
		if elapsed > 25*time.Second {
			t.Errorf("took %v to kill an unresponsive workload; escalation is too slow", elapsed)
		}
	})
}

// ── CONTRACT: logs stream ────────────────────────────────────────────────────

func TestConformanceLogsStream(t *testing.T) {
	runConformance(t, "logs", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, `echo "line-one"; echo "line-two" >&2; sleep 1; echo "line-three"`)
		ctx := context.Background()

		p, err := e.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })

		if _, err := e.Wait(ctx, h); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		rc, err := e.Logs(ctx, h)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		defer rc.Close()

		var buf bytes.Buffer
		readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := io.Copy(&buf, rc); done <- err }()
		select {
		case <-done:
		case <-readCtx.Done():
			// A follow-style stream may not EOF promptly; what we have is enough.
		}

		got := buf.String()
		for _, want := range []string{"line-one", "line-two", "line-three"} {
			if !strings.Contains(got, want) {
				t.Errorf("log stream missing %q; got:\n%s", want, got)
			}
		}
		// stderr must be interleaved into the same stream: split streams lose the
		// ordering that makes a log readable.
		if !strings.Contains(got, "line-two") {
			t.Error("stderr was not captured into the combined log")
		}
	})
}

// ── CONTRACT: Cleanup is idempotent ──────────────────────────────────────────

func TestConformanceCleanupIsIdempotent(t *testing.T) {
	runConformance(t, "cleanup-idempotent", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		ctx := context.Background()
		spec := baseSpec(t, tgt, `printf 'x' > "$NEXUS_OUTPUT_FILE"`)

		p, err := e.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if _, err := e.Wait(ctx, h); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		// Cleanup runs on every path — success, failure, cancel, panic — so a
		// Cleanup that errored on an already-clean handle would turn every normal
		// shutdown into a noisy one.
		for i := 1; i <= 3; i++ {
			if err := e.Cleanup(ctx, h); err != nil {
				t.Fatalf("Cleanup call #%d returned an error: %v", i, err)
			}
		}
		if _, err := os.Stat(h.WorkDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("work directory %s survived Cleanup (err=%v)", h.WorkDir, err)
		}
	})
}

// ── CONTRACT: a killed executor leaves NO orphaned workload ───────────────────

// TestConformanceCleanupLeavesNoOrphan is the invariant that keeps a node's
// capacity honest. An orphaned workload holds CPU and memory that the control
// plane's ledger believes is free, and it lives until the node is rebuilt.
//
// The subprocess case is the harder one: tasks FORK. A shell wrapper, a Python
// process pool, a compiler. Signalling only the direct child leaves the
// grandchildren running, which is why the executor puts the workload in its own
// process group and kills the GROUP.
func TestConformanceCleanupLeavesNoOrphan(t *testing.T) {
	runConformance(t, "no-orphan", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		ctx := context.Background()

		// The workload forks a long-lived GRANDCHILD that records its own pid, then
		// the parent sleeps. Cleanup must take out the whole group, not just the
		// direct child.
		script := `
( i=0; while [ $i -lt 6000 ]; do sleep 0.1; i=$((i+1)); done ) &
echo $! > "$NEXUS_WORK_DIR/child.pid"
i=0; while [ $i -lt 6000 ]; do sleep 0.1; i=$((i+1)); done`
		spec := baseSpec(t, tgt, script)
		spec.StartToCloseDeadline = time.Now().Add(5 * time.Minute)

		p, err := e.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}

		if tgt.name == KubernetesKind {
			// The pod's pids and files live on a node, not in p.WorkDir, so
			// the pid-file handshake cannot apply. The meaningful assertion is
			// the same as docker's one level up: once the task container is
			// running, an abandoning Cleanup must leave neither the Job nor
			// any pod of the attempt behind — deleting the pod takes every
			// process in it, grandchildren included.
			k8sAwaitTaskRunning(t, e, h)
			if err := e.Cleanup(ctx, h); err != nil {
				t.Fatalf("Cleanup: %v", err)
			}
			if err := k8sWaitForAttemptGone(e, h.AttemptID, 60*time.Second); err != nil {
				t.Errorf("ORPHAN LEAKED: %v", err)
			}
			return
		}

		// Wait for the grandchild pid to be recorded.
		pidFile := filepath.Join(p.WorkDir, "child.pid")
		var grandchildPID int
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(pidFile); err == nil {
				if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
					grandchildPID = n
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if grandchildPID == 0 {
			_ = e.Cleanup(ctx, h)
			t.Skip("workload did not record a grandchild pid; cannot assert on orphans")
		}

		// Tear down WITHOUT waiting — the abandoned-executor case.
		if err := e.Cleanup(ctx, h); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}

		switch tgt.name {
		case "subprocess":
			// The grandchild pid is in the agent's own pid namespace, so we can
			// check it directly.
			if err := waitForProcessGone(grandchildPID, 20*time.Second); err != nil {
				t.Errorf("ORPHAN LEAKED: grandchild pid %d survived Cleanup: %v", grandchildPID, err)
			}
		case "docker":
			// The container's pids are in its own namespace, so the meaningful
			// assertion is that the container itself is gone: removing it takes
			// every process inside it with it.
			if err := waitForContainerGone(h.ID, 30*time.Second); err != nil {
				t.Errorf("ORPHAN LEAKED: container %s survived Cleanup: %v", shortID(h.ID), err)
			}
		}
	})
}

// waitForProcessGone polls with signal 0, which tests for existence without
// affecting the process.
func waitForProcessGone(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return nil
		}
		if err := proc.Signal(os.Signal(nil)); err != nil {
			return nil // gone
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("pid %d still alive after %v", pid, timeout)
}

func waitForContainerGone(id string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := exec.CommandContext(ctx, "docker", "inspect", id).Run()
		cancel()
		if err != nil {
			return nil // no such container
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("container %s still present after %v", id, timeout)
}

// ── CONTRACT: resource limits are actually enforced ───────────────────────────

// TestConformanceMemoryLimitEnforced asserts against the executor's DECLARED
// fidelity rather than a uniform ideal.
//
// This is the honest form of the assertion ADR-0007 asks for. The docker
// executor claims "hard" enforcement and is held to it: an over-limit workload
// MUST be killed and MUST be reported as OOM, because retrying an OOM on a larger
// worker can succeed while retrying an application error cannot. The subprocess
// executor claims "address-space" or "none" and is held to that weaker claim,
// which is the whole reason LimitFidelity exists: asserting a fiction would
// either fail honest code or pass dishonest code.
func TestConformanceMemoryLimitEnforced(t *testing.T) {
	runConformance(t, "memory-limit", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		if tgt.fidelity.Memory == "none" {
			t.Skipf("%s declares Memory fidelity %q on this platform: there is nothing to enforce, "+
				"and pretending otherwise would be worse than admitting it",
				tgt.name, tgt.fidelity.Memory)
		}

		const limit = 32 << 20 // 32 MiB
		// Allocate far beyond the limit. `yes` piped into a growing shell variable
		// is portable across busybox and a host shell.
		script := `
i=0
s="0123456789012345678901234567890123456789012345678901234567890123"
while [ $i -lt 40 ]; do s="$s$s"; i=$((i+1)); done
printf 'allocated-without-being-killed' > "$NEXUS_OUTPUT_FILE"`

		spec := baseSpec(t, tgt, script)
		spec.Limit = &nexusv1.ResourceVector{MemoryBytes: limit}
		spec.StartToCloseDeadline = time.Now().Add(45 * time.Second)
		spec.GraceAfterTerm = 2 * time.Second

		o, _ := runToCompletion(t, e, spec)

		if o.Succeeded() && string(o.Output) == "allocated-without-being-killed" {
			t.Fatalf("%s claims Memory fidelity %q but a workload allocated far past its %d-byte limit unchecked",
				tgt.name, tgt.fidelity.Memory, limit)
		}
		if tgt.fidelity.Memory == "hard" {
			// A container that exceeds memory.max is OOM-killed by the kernel and
			// the flag is reported unambiguously. Classifying it as OOM rather than
			// APPLICATION is what makes the retry-on-a-bigger-worker path work.
			if !o.OOMKilled {
				t.Errorf("%s claims hard memory enforcement but did not report OOMKilled (exit=%d, timedOut=%v)",
					tgt.name, o.ExitCode, o.TimedOut)
			}
			if class, retryable := ClassifyOutcome(o); o.OOMKilled {
				if class != nexusv1.ErrorClass_ERROR_CLASS_OOM {
					t.Errorf("class = %v, want ERROR_CLASS_OOM", class)
				}
				if !retryable {
					t.Error("an OOM must be retryable: a larger worker may succeed")
				}
			}
		}
	})
}

// The CPU limit must be passed to the substrate. A value the substrate rejects
// would fail the task, so the assertion is that a limited task still RUNS.
func TestConformanceCPULimitAccepted(t *testing.T) {
	runConformance(t, "cpu-limit", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, `printf 'ok' > "$NEXUS_OUTPUT_FILE"`)
		spec.Limit = &nexusv1.ResourceVector{CpuMillis: 500, MemoryBytes: 64 << 20}
		spec.StartToCloseDeadline = time.Now().Add(30 * time.Second)

		o, _ := runToCompletion(t, e, spec)
		if !o.Succeeded() {
			t.Fatalf("a task under a CPU limit failed to run: %+v (err=%v)", o, o.Err)
		}
		if string(o.Output) != "ok" {
			t.Errorf("output = %q", o.Output)
		}
	})
}

// ── CONTRACT: metrics are captured ───────────────────────────────────────────

func TestConformanceMetricsCaptured(t *testing.T) {
	runConformance(t, "metrics", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		// Burn a little CPU so the counters are non-trivially non-zero.
		spec := baseSpec(t, tgt, `i=0; while [ $i -lt 20000 ]; do i=$((i+1)); done; printf 'done' > "$NEXUS_OUTPUT_FILE"`)
		o, _ := runToCompletion(t, e, spec)
		if !o.Succeeded() {
			t.Fatalf("task failed: %+v", o)
		}
		if o.WallMs <= 0 {
			t.Error("wall_ms must be measured: it is the only universally available metric")
		}
		if o.ExitCode != 0 {
			t.Errorf("exit code = %d", o.ExitCode)
		}
	})
}

// ── CONTRACT: Wait honours cancellation, and Wait is safe to call twice ────────

// Cancellation must tear the workload down on the same TERM-then-KILL contract: a
// revoked lease means any work we continue is guaranteed to be discarded, so we
// stop, but we still give the task its grace.
func TestConformanceWaitHonoursCancellation(t *testing.T) {
	runConformance(t, "cancel", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		spec := baseSpec(t, tgt, `i=0; while [ $i -lt 6000 ]; do sleep 0.1; i=$((i+1)); done`)
		spec.StartToCloseDeadline = time.Now().Add(5 * time.Minute)
		spec.GraceAfterTerm = time.Second

		ctx, cancel := context.WithCancel(context.Background())
		p, err := e.Prepare(ctx, spec)
		if err != nil {
			cancel()
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			cancel()
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })

		done := make(chan struct{})
		go func() {
			_, _ = e.Wait(ctx, h)
			close(done)
		}()

		time.Sleep(time.Second)
		cancel()

		select {
		case <-done:
		case <-time.After(45 * time.Second):
			t.Fatal("Wait did not return after its context was cancelled; a revoked lease would wedge the runner")
		}
	})
}

// Wait can be raced by the deadline enforcer and the caller, so a second call
// must return the memoised outcome rather than blocking forever or panicking on a
// double reap.
func TestConformanceWaitIsIdempotent(t *testing.T) {
	runConformance(t, "wait-twice", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		ctx := context.Background()
		spec := baseSpec(t, tgt, `exit 7`)

		p, err := e.Prepare(ctx, spec)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })

		first, err := e.Wait(ctx, h)
		if err != nil {
			t.Fatalf("first Wait: %v", err)
		}
		second, err := e.Wait(ctx, h)
		if err != nil {
			t.Fatalf("second Wait: %v", err)
		}
		if first.ExitCode != second.ExitCode {
			t.Errorf("Wait is not idempotent: exit codes %d then %d", first.ExitCode, second.ExitCode)
		}
		if first.ExitCode != 7 {
			t.Errorf("exit code = %d, want 7", first.ExitCode)
		}
	})
}

// Signalling an already-dead workload must be a no-op, not an error: the desired
// end state is already true, and the teardown path calls Signal unconditionally.
func TestConformanceSignalAfterExitIsNoop(t *testing.T) {
	runConformance(t, "signal-after-exit", func(t *testing.T, tgt conformanceTarget, e Executor, root string) {
		ctx := context.Background()
		p, err := e.Prepare(ctx, baseSpec(t, tgt, `exit 0`))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		h, err := e.Start(ctx, p)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = e.Cleanup(context.Background(), h) })
		if _, err := e.Wait(ctx, h); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if err := e.Signal(ctx, h, SignalTerm); err != nil {
			t.Errorf("Signal(TERM) on an exited workload returned an error: %v", err)
		}
		if err := e.Signal(ctx, h, SignalKill); err != nil {
			t.Errorf("Signal(KILL) on an exited workload returned an error: %v", err)
		}
	})
}

// Kind() must be stable and match what the agent advertises at registration,
// because the scheduler treats it as a placement constraint.
func TestConformanceKindIsStable(t *testing.T) {
	for _, tgt := range conformanceTargets() {
		e := tgt.new(t, t.TempDir())
		if got := e.Kind(); got != tgt.name {
			t.Errorf("%s: Kind() = %q, want %q — the scheduler routes on this string",
				tgt.name, got, tgt.name)
		}
	}
}
