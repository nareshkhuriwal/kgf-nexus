// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// SubprocessExecutor runs the workload as a child process of the agent.
//
// HONEST LIMITATION, per ADR-0007: this executor is DEVELOPMENT-GRADE. It
// enforces CPU and memory limits with the POSIX rlimit facility, which is
// materially weaker than a container:
//
//   - Memory (RLIMIT_AS) caps the process's ADDRESS SPACE, not its resident
//     set. A process that maps a large region without touching it is killed by
//     rlimit but would be fine under a cgroup; conversely rlimit makes
//     allocation FAIL rather than triggering the kernel OOM killer, so a
//     workload can catch the failure and continue in a degraded state. We
//     report OOM when we see the signature, but the signal is weaker.
//   - On Darwin, RLIMIT_AS is not honoured at all by the kernel, so memory
//     limits on a developer laptop are advisory only. This is reported through
//     LimitFidelity so the conformance suite can assert the truth rather than
//     assert a fiction.
//   - CPU (RLIMIT_CPU) caps cumulative CPU SECONDS, not a share of cores. It
//     cannot express "half a core"; it can only eventually kill a spinning
//     process. We derive a ceiling from cpu_millis and the deadline.
//   - GPU isolation is IMPOSSIBLE here. A GPU request is honoured by pinning
//     CUDA_VISIBLE_DEVICES, which a workload can simply override.
//
// Production work belongs on the docker executor. Stating this in code rather
// than only in a doc matters because the failure mode of pretending otherwise
// is a task that silently exceeds the limit it was scheduled under and takes
// the node down with it.
type SubprocessExecutor struct {
	// Root is the parent directory for per-attempt work directories.
	Root string
	Log  *slog.Logger

	// AllowUnenforceableLimits lets a development deployment run tasks whose
	// limits this executor cannot honour. Default false: refusing to run is
	// safer than silently running unconstrained, and the resulting
	// INFRASTRUCTURE error tells the operator to use docker.
	AllowUnenforceableLimits bool
}

// subprocState is the per-attempt private state hung off Prepared/Handle.
type subprocState struct {
	argv    []string
	env     []string
	workDir string
	// outputDir is the agent-owned attempt directory. Kept separate from
	// workDir because a task's declared working_dir must not move the platform's
	// output drop.
	outputDir string
	limit     *nexusv1.ResourceVector
	deadline  time.Time
	grace     time.Duration

	// declaredOutputs and stdoutOutput are copied from the spec at Prepare so
	// buildOutcome can collect outputs without the spec being threaded through
	// Wait, which takes only a Handle.
	declaredOutputs []string
	stdoutOutput    string

	cmd  *exec.Cmd
	logF *os.File
	// stdoutBuf captures stdout SEPARATELY from the combined log when the task
	// opted into stdout capture. Separately because the combined log interleaves
	// stderr, and a result with a stray warning spliced into it is corrupt: an
	// output is data, while the log is a diagnostic.
	stdoutBuf *boundedBuffer
	pgid      int
	waitMu    sync.Mutex
	// waited memoises the wait result so Wait is safe to call twice (the
	// deadline enforcer and the caller can race).
	waited   bool
	outcome  Outcome
	waitDone chan struct{}
}

func (e *SubprocessExecutor) Kind() string { return "subprocess" }

func (e *SubprocessExecutor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Prepare materialises the work directory and resolves the command. It does
// NOT start anything, so a validation or not-found failure is reported with its
// own specific error class instead of masquerading as a workload crash.
func (e *SubprocessExecutor) Prepare(ctx context.Context, spec TaskSpec) (Prepared, error) {
	cmdline := spec.Runtime.GetCommand()
	if len(cmdline) == 0 {
		return Prepared{}, fmt.Errorf("%w: runtime.command is empty for attempt %s",
			ErrValidation, spec.AttemptID)
	}
	argv := append([]string{}, cmdline...)
	argv = append(argv, spec.Runtime.GetArgs()...)

	// Resolve the executable up front so a typo'd binary is NOT_FOUND at
	// prepare time rather than an opaque start failure.
	if _, err := exec.LookPath(argv[0]); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return Prepared{}, fmt.Errorf("%w: %q: %v", ErrImageNotFound, argv[0], err)
		}
		if errors.Is(err, os.ErrPermission) {
			return Prepared{}, fmt.Errorf("%w: %q not executable", os.ErrPermission, argv[0])
		}
		return Prepared{}, fmt.Errorf("resolve command %q: %w", argv[0], err)
	}

	if !e.AllowUnenforceableLimits {
		if err := checkSubprocessLimitFidelity(spec.Limit); err != nil {
			return Prepared{}, err
		}
	}

	workDir, err := makeWorkDir(e.Root, spec.AttemptID)
	if err != nil {
		return Prepared{}, err
	}
	if err := stageInputs(workDir, spec.Inputs); err != nil {
		os.RemoveAll(workDir)
		return Prepared{}, err
	}

	st := &subprocState{
		argv:      argv,
		outputDir: workDir,
		env: buildEnv(spec, workDir, filepath.Join(workDir, InputDirName),
			OutputFilePath(spec, workDir), filepath.Join(workDir, OutputDirName)),
		workDir:         workDir,
		declaredOutputs: spec.DeclaredOutputs,
		stdoutOutput:    spec.StdoutOutput,
		limit:           spec.Limit,
		deadline:        spec.StartToCloseDeadline,
		grace:           orDefaultGrace(spec.GraceAfterTerm),
		waitDone:        make(chan struct{}),
	}
	if spec.StdoutOutput != "" {
		st.stdoutBuf = newBoundedBuffer(MaxStdoutOutputBytes)
	}
	if wd := spec.Runtime.GetWorkingDir(); wd != "" {
		st.workDir = wd
	}
	return Prepared{AttemptID: spec.AttemptID, WorkDir: workDir, State: st}, nil
}

// Start launches the workload in its OWN PROCESS GROUP.
//
// The process group is the single most important detail in this file. Tasks
// fork: a shell wrapper, a python process pool, a compiler. Signalling only the
// direct child leaves the grandchildren running, still holding the node's CPU
// and memory, invisible to the agent and to the control plane's ledger — an
// orphan that lives until the node is rebuilt. Setpgid at spawn plus a negative
// kill at teardown is what makes "a killed executor leaves NO orphaned
// workload" true rather than aspirational.
func (e *SubprocessExecutor) Start(ctx context.Context, p Prepared) (Handle, error) {
	st, ok := p.State.(*subprocState)
	if !ok {
		return Handle{}, fmt.Errorf("%w: subprocess: Start called with foreign Prepared", ErrValidation)
	}

	logPath := filepath.Join(p.WorkDir, LogFileName)
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Handle{}, fmt.Errorf("open log file %s: %w", logPath, err)
	}

	// context.Background, deliberately NOT ctx: we own this process's lifetime
	// through Signal/Cleanup and the deadline enforcer. If the process died
	// whenever the caller's context was cancelled, exec would SIGKILL it with
	// no grace period, defeating the SIGTERM-then-grace contract the
	// conformance suite requires.
	cmd := exec.CommandContext(context.Background(), st.argv[0], st.argv[1:]...)
	cmd.Dir = st.workDir
	cmd.Env = st.env
	cmd.Stderr = logF
	if st.stdoutBuf != nil {
		// Tee, not redirect: stdout is still the operator's log. Losing the log
		// because the task opted into stdout capture would make the feature cost
		// debuggability, which is not a trade anyone asked for.
		cmd.Stdout = io.MultiWriter(logF, st.stdoutBuf)
	} else {
		cmd.Stdout = logF
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	setPdeathsig(cmd.SysProcAttr)

	// Must run AFTER SysProcAttr is set and BEFORE Start: it may rewrite
	// cmd.Path/cmd.Args into a shell trampoline that applies the limits in the
	// child (see applyRlimits for why that indirection is necessary).
	if err := applyRlimits(cmd, st.limit, st.deadline); err != nil {
		logF.Close()
		return Handle{}, err
	}

	if err := cmd.Start(); err != nil {
		logF.Close()
		if errors.Is(err, os.ErrNotExist) {
			return Handle{}, fmt.Errorf("%w: %q: %v", ErrImageNotFound, st.argv[0], err)
		}
		return Handle{}, fmt.Errorf("start %q: %w", st.argv[0], err)
	}

	st.cmd = cmd
	st.logF = logF
	// The pgid equals the child's pid because of Setpgid; reading it back would
	// race with the child exiting, and the identity is guaranteed by the spawn.
	st.pgid = cmd.Process.Pid

	h := Handle{
		AttemptID:    p.AttemptID,
		ExecutorKind: e.Kind(),
		ID:           strconv.Itoa(cmd.Process.Pid),
		WorkDir:      p.WorkDir,
		StartedAt:    time.Now(),
		State:        st,
	}
	e.logger().Info("subprocess started", "attempt_id", p.AttemptID,
		"pid", cmd.Process.Pid, "argv0", st.argv[0])
	return h, nil
}

// Wait blocks for the workload and enforces the start_to_close deadline with
// SIGTERM, then SIGKILL after the grace period.
//
// Enforcement lives in Wait rather than in a caller-side timer because only the
// executor knows how to kill its own substrate correctly, and because a
// deadline enforced by the caller would have to reach through the abstraction
// to find a pid.
func (e *SubprocessExecutor) Wait(ctx context.Context, h Handle) (Outcome, error) {
	st, ok := h.State.(*subprocState)
	if !ok {
		return Outcome{}, fmt.Errorf("%w: subprocess: Wait called with foreign Handle", ErrValidation)
	}

	st.waitMu.Lock()
	if st.waited {
		o := st.outcome
		st.waitMu.Unlock()
		return o, nil
	}
	st.waitMu.Unlock()

	// One goroutine, and only one, ever calls cmd.Wait.
	reaped := make(chan error, 1)
	go func() { reaped <- st.cmd.Wait() }()

	var timedOut bool
	var deadlineC <-chan time.Time
	if !st.deadline.IsZero() {
		t := time.NewTimer(time.Until(st.deadline))
		defer t.Stop()
		deadlineC = t.C
	}

	var waitErr error
enforce:
	for {
		select {
		case waitErr = <-reaped:
			break enforce

		case <-deadlineC:
			timedOut = true
			e.logger().Warn("subprocess exceeded start_to_close deadline; SIGTERM then grace",
				"attempt_id", h.AttemptID, "pid", st.pgid, "grace", st.grace)
			_ = e.Signal(ctx, h, SignalTerm)
			grace := time.NewTimer(st.grace)
			select {
			case waitErr = <-reaped:
				grace.Stop()
			case <-grace.C:
				// Grace expired: the workload ignored SIGTERM (or is wedged in
				// uninterruptible I/O). SIGKILL the whole group.
				e.logger().Warn("grace period expired after SIGTERM; SIGKILL",
					"attempt_id", h.AttemptID, "pid", st.pgid)
				_ = e.Signal(ctx, h, SignalKill)
				waitErr = <-reaped
			}
			break enforce

		case <-ctx.Done():
			// The caller abandoned the attempt (lease revoked, agent draining).
			// Tear the workload down on the same TERM-then-KILL contract; a
			// revoked lease means any work we continue is guaranteed waste.
			e.logger().Warn("wait context cancelled; terminating workload",
				"attempt_id", h.AttemptID, "reason", ctx.Err())
			_ = e.Signal(context.Background(), h, SignalTerm)
			grace := time.NewTimer(st.grace)
			select {
			case waitErr = <-reaped:
				grace.Stop()
			case <-grace.C:
				_ = e.Signal(context.Background(), h, SignalKill)
				waitErr = <-reaped
			}
			break enforce
		}
	}

	o := e.buildOutcome(st, waitErr, timedOut, h.StartedAt)

	st.waitMu.Lock()
	st.waited, st.outcome = true, o
	close(st.waitDone)
	st.waitMu.Unlock()

	// Flush and close the log sink only after the process is reaped, so no
	// write races the close.
	if st.logF != nil {
		_ = st.logF.Sync()
		_ = st.logF.Close()
	}
	return o, nil
}

func (e *SubprocessExecutor) buildOutcome(st *subprocState, waitErr error, timedOut bool, startedAt time.Time) Outcome {
	o := Outcome{WallMs: time.Since(startedAt).Milliseconds(), TimedOut: timedOut}

	if ps := st.cmd.ProcessState; ps != nil {
		o.ExitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			o.Signalled = true
			o.Signal = ws.Signal().String()
			// Go reports a signalled exit as -1; normalise to the shell
			// convention so the control plane sees a stable, comparable value.
			o.ExitCode = 128 + int(ws.Signal())
		}
		// Resource usage from wait4 is the only peak-RSS figure available for a
		// subprocess, and it is already-exited data, so it cannot be sampled
		// mid-flight the way a cgroup can.
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok {
			o.CPUSeconds = float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
				float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
			o.PeakMemoryBytes = maxRSSBytes(ru)
		}
	} else if waitErr != nil {
		o.Err = fmt.Errorf("wait: %w", waitErr)
	}

	// An rlimit memory kill on Linux surfaces as SIGSEGV/SIGKILL or as an
	// allocation failure inside the workload; we can only report OOM when the
	// evidence is unambiguous, which is a real fidelity gap versus a cgroup and
	// exactly why subprocess is labelled development-grade.
	if o.Signalled && o.Signal == syscall.SIGKILL.String() && !timedOut &&
		st.limit.GetMemoryBytes() > 0 && o.PeakMemoryBytes >= st.limit.GetMemoryBytes() {
		o.OOMKilled = true
	}

	// Outputs are read from the attempt work directory, which is NOT necessarily
	// cmd.Dir: a task may declare its own working_dir, and the output drop must
	// stay where the agent put it.
	if out, ct, err := ReadOutputFile(st.outputDir); err == nil {
		o.Output, o.ContentType = out, ct
	}
	var stdout []byte
	if st.stdoutBuf != nil {
		stdout = st.stdoutBuf.Bytes()
		if st.stdoutBuf.Truncated() {
			// Said out loud rather than silently: a truncated result is still
			// reported as a SUCCESS, so the only signal an operator gets that
			// their output is incomplete is this line.
			e.logger().Warn("stdout output truncated at the agent cap",
				"output", st.stdoutOutput, "limit_bytes", MaxStdoutOutputBytes)
		}
	}
	o.Outputs = CollectOutputs(
		TaskSpec{DeclaredOutputs: st.declaredOutputs, StdoutOutput: st.stdoutOutput},
		st.outputDir, stdout)
	return o
}

// Signal delivers to the process GROUP (negative pid), not just the child, so
// forked grandchildren are included. See the comment on Start.
func (e *SubprocessExecutor) Signal(ctx context.Context, h Handle, sig Signal) error {
	st, ok := h.State.(*subprocState)
	if !ok || st.cmd == nil || st.cmd.Process == nil {
		return nil // nothing running; signalling is a no-op, not an error
	}
	var s syscall.Signal
	switch sig {
	case SignalTerm:
		s = syscall.SIGTERM
	case SignalKill:
		s = syscall.SIGKILL
	default:
		return fmt.Errorf("%w: unknown signal %q", ErrValidation, sig)
	}

	if err := syscall.Kill(-st.pgid, s); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone: the desired end state
		}
		// Fall back to the direct child. Better to kill something than nothing
		// when the group is unreachable (e.g. the child changed its own pgid).
		if perr := st.cmd.Process.Signal(s); perr != nil && !errors.Is(perr, os.ErrProcessDone) {
			return fmt.Errorf("signal %s to pgid %d: %w", sig, st.pgid, err)
		}
	}
	return nil
}

// Logs returns the combined capture. It opens the file rather than tee-ing a
// live pipe so a reader that starts late still gets the beginning of the log —
// and so a slow reader can never apply backpressure to the workload's stdout,
// which would let a log consumer wedge a task.
func (e *SubprocessExecutor) Logs(ctx context.Context, h Handle) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(h.WorkDir, LogFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return io.NopCloser(emptyReader{}), nil
		}
		return nil, fmt.Errorf("open logs for %s: %w", h.AttemptID, err)
	}
	return f, nil
}

// Cleanup is idempotent and kills the process group before removing the work
// directory. Killing first is the ordering that matters: removing the work
// directory of a still-running process leaves a workload writing into deleted
// inodes, which is both an orphan and a very confusing bug report.
func (e *SubprocessExecutor) Cleanup(ctx context.Context, h Handle) error {
	if st, ok := h.State.(*subprocState); ok && st.cmd != nil && st.cmd.Process != nil {
		if err := syscall.Kill(-st.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			e.logger().Warn("cleanup could not kill process group",
				"attempt_id", h.AttemptID, "pgid", st.pgid, "error", err)
		}
		if st.logF != nil {
			_ = st.logF.Close()
		}
	}
	if h.WorkDir == "" {
		return nil
	}
	// RemoveAll on an absent path is already a no-op, which is what makes
	// repeated Cleanup calls safe.
	if err := os.RemoveAll(h.WorkDir); err != nil {
		return fmt.Errorf("cleanup workdir %s: %w", h.WorkDir, err)
	}
	return nil
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
