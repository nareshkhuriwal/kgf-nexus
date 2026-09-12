package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// DockerExecutor runs each attempt as a container. This is the PRODUCTION-grade
// executor: unlike subprocess, the kernel enforces the memory limit via cgroups
// and reports an OOM kill unambiguously, so an over-limit task cannot take the
// node (and every sibling attempt) down with it.
//
// It drives the docker CLI rather than the Docker Engine HTTP API. WHY: the CLI
// is a stable, versioned contract that already handles socket discovery, TLS,
// context selection and remote daemons, and it adds no library dependency that
// must be upgraded in lockstep with the daemon. The cost is process spawn
// overhead per lifecycle call, which is negligible against container startup,
// and parsing CLI output — which we confine to `docker inspect --format` with
// explicit Go-template field selectors so the parse is not screen-scraping.
type DockerExecutor struct {
	// Root is the host directory under which per-attempt work directories are
	// created and bind-mounted into the container.
	Root string
	Log  *slog.Logger

	// Binary is the docker CLI to invoke; overridable for tests.
	Binary string

	// PullTimeout bounds image acquisition. Separate from the task's
	// start_to_close deadline because pulling a 10 GiB image is not the task's
	// runtime and must not consume the task's budget.
	PullTimeout time.Duration

	// NetworkMode is passed to --network. Defaults to "none": a task that has
	// not declared a need for the network should not have it, and the default
	// bridge is the quiet path by which a task reaches the control plane's own
	// network.
	NetworkMode string
}

type dockerState struct {
	image       string
	argv        []string
	env         []string
	workDir     string
	containerWD string
	limit       *nexusv1.ResourceVector
	deadline    time.Time
	grace       time.Duration
	name        string
	// declaredOutputs and stdoutOutput are copied from the spec at Prepare so
	// inspectOutcome can collect outputs without the spec, which Wait does not
	// receive.
	declaredOutputs []string
	stdoutOutput    string

	containerID string

	waitMu   sync.Mutex
	waited   bool
	outcome  Outcome
	startedA time.Time
}

func (e *DockerExecutor) Kind() string { return "docker" }

func (e *DockerExecutor) bin() string {
	if e.Binary != "" {
		return e.Binary
	}
	return "docker"
}

func (e *DockerExecutor) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

func (e *DockerExecutor) network() string {
	if e.NetworkMode != "" {
		return e.NetworkMode
	}
	return "none"
}

// DockerFidelity reports what a container can actually enforce. Memory is
// "hard" because the kernel's cgroup enforcement is real and the OOM kill is
// reported unambiguously in the container state, which is exactly what the
// subprocess executor cannot give us.
func DockerFidelity() LimitFidelity {
	return LimitFidelity{Memory: "hard", CPU: "shares", GPU: "device-isolation"}
}

// DockerAvailable reports whether a usable daemon is reachable. Used to SKIP
// docker-dependent tests cleanly rather than failing a CI box that has no
// daemon — a test that fails for want of infrastructure teaches nobody anything.
func DockerAvailable(ctx context.Context, binary string) bool {
	if binary == "" {
		binary = "docker"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, binary, "info", "--format", "{{.ServerVersion}}").Run() == nil
}

// Prepare creates the work directory, stages inputs, and ensures the image is
// present locally.
//
// Pulling here rather than in Start is what lets a missing image be reported as
// terminal NOT_FOUND instead of an opaque start failure that gets retried
// around the fleet until the task's budget is gone.
func (e *DockerExecutor) Prepare(ctx context.Context, spec TaskSpec) (Prepared, error) {
	image := spec.Runtime.GetImage()
	if image == "" {
		return Prepared{}, fmt.Errorf("%w: runtime.image is required for the docker executor (attempt %s)",
			ErrValidation, spec.AttemptID)
	}
	if !DockerAvailable(ctx, e.bin()) {
		return Prepared{}, fmt.Errorf("%w: docker daemon is not reachable", ErrRuntimeUnavailable)
	}

	workDir, err := makeWorkDir(e.Root, spec.AttemptID)
	if err != nil {
		return Prepared{}, err
	}
	if err := stageInputs(workDir, spec.Inputs); err != nil {
		os.RemoveAll(workDir)
		return Prepared{}, err
	}

	if err := e.ensureImage(ctx, image); err != nil {
		os.RemoveAll(workDir)
		return Prepared{}, err
	}

	const containerWD = "/nexus"
	st := &dockerState{
		image:           image,
		argv:            append(append([]string{}, spec.Runtime.GetCommand()...), spec.Runtime.GetArgs()...),
		env:             dockerEnv(spec, containerWD),
		workDir:         workDir,
		containerWD:     containerWD,
		limit:           spec.Limit,
		deadline:        spec.StartToCloseDeadline,
		grace:           orDefaultGrace(spec.GraceAfterTerm),
		declaredOutputs: spec.DeclaredOutputs,
		stdoutOutput:    spec.StdoutOutput,
		// Deterministic name derived from the attempt id: it makes an orphan
		// findable after an agent restart (`docker ps --filter name=nexus-`),
		// which is what turns "no orphaned workload" into something we can
		// actually reconcile rather than merely intend.
		name: "nexus-" + sanitizeID(spec.AttemptID),
	}
	return Prepared{AttemptID: spec.AttemptID, WorkDir: workDir, State: st}, nil
}

func (e *DockerExecutor) ensureImage(ctx context.Context, image string) error {
	if err := e.run(ctx, 30*time.Second, "image", "inspect", image); err == nil {
		return nil // already local; never pull over a digest we already have
	}
	timeout := e.PullTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	out, err := e.output(ctx, timeout, "pull", image)
	if err == nil {
		return nil
	}
	s := strings.ToLower(out)
	// Distinguishing "not found" from "registry unreachable" is the whole point:
	// the first is terminal and needs a human to fix the reference, the second
	// is transient and a retry elsewhere will succeed.
	switch {
	case strings.Contains(s, "not found"), strings.Contains(s, "manifest unknown"),
		strings.Contains(s, "repository does not exist"), strings.Contains(s, "no such image"):
		return fmt.Errorf("%w: image %q: %s", ErrImageNotFound, image, strings.TrimSpace(out))
	case strings.Contains(s, "unauthorized"), strings.Contains(s, "denied"),
		strings.Contains(s, "authentication required"):
		return fmt.Errorf("%w: pull %q: %s", os.ErrPermission, image, strings.TrimSpace(out))
	default:
		return fmt.Errorf("%w: pull %q: %s", ErrRuntimeUnavailable, image, strings.TrimSpace(out))
	}
}

// Start creates and starts the container with the limits applied.
//
// --rm is deliberately NOT used: we must inspect the exited container to learn
// the exit code and the OOMKilled flag, and --rm deletes that evidence the
// instant the process exits. Cleanup removes it explicitly instead, which is
// also why Cleanup must be idempotent.
func (e *DockerExecutor) Start(ctx context.Context, p Prepared) (Handle, error) {
	st, ok := p.State.(*dockerState)
	if !ok {
		return Handle{}, fmt.Errorf("%w: docker: Start called with foreign Prepared", ErrValidation)
	}

	args := []string{"run", "--detach", "--name", st.name,
		"--network", e.network(),
		"--workdir", st.containerWD,
		"--volume", st.workDir + ":" + st.containerWD,
		// Labels make orphan reconciliation a filtered list rather than a guess.
		"--label", "nexus.attempt_id=" + p.AttemptID,
		"--label", "nexus.managed=true",
	}
	args = append(args, dockerLimitArgs(st.limit)...)
	for _, kv := range st.env {
		args = append(args, "--env", kv)
	}
	args = append(args, st.image)
	args = append(args, st.argv...)

	out, err := e.output(ctx, 2*time.Minute, args...)
	if err != nil {
		// A leftover container from a previous identically-named attempt would
		// otherwise wedge every retry. Remove and retry once.
		if strings.Contains(strings.ToLower(out), "already in use") {
			_ = e.run(ctx, 30*time.Second, "rm", "-f", st.name)
			out, err = e.output(ctx, 2*time.Minute, args...)
		}
		if err != nil {
			return Handle{}, fmt.Errorf("%w: docker run: %s", ErrRuntimeUnavailable, strings.TrimSpace(out))
		}
	}
	st.containerID = strings.TrimSpace(out)
	st.startedA = time.Now()

	e.logger().Info("container started", "attempt_id", p.AttemptID,
		"container", shortID(st.containerID), "image", st.image)

	return Handle{
		AttemptID:    p.AttemptID,
		ExecutorKind: e.Kind(),
		ID:           st.containerID,
		WorkDir:      p.WorkDir,
		StartedAt:    st.startedA,
		State:        st,
	}, nil
}

// Wait blocks on the container and enforces start_to_close with SIGTERM then
// SIGKILL after the grace period.
func (e *DockerExecutor) Wait(ctx context.Context, h Handle) (Outcome, error) {
	st, ok := h.State.(*dockerState)
	if !ok {
		return Outcome{}, fmt.Errorf("%w: docker: Wait called with foreign Handle", ErrValidation)
	}
	st.waitMu.Lock()
	if st.waited {
		o := st.outcome
		st.waitMu.Unlock()
		return o, nil
	}
	st.waitMu.Unlock()

	// `docker wait` blocks until exit and prints the exit code. Running it in a
	// goroutine keeps the deadline and cancellation paths responsive.
	done := make(chan error, 1)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	go func() {
		done <- exec.CommandContext(waitCtx, e.bin(), "wait", st.containerID).Run()
	}()

	var timedOut bool
	var deadlineC <-chan time.Time
	if !st.deadline.IsZero() {
		t := time.NewTimer(time.Until(st.deadline))
		defer t.Stop()
		deadlineC = t.C
	}

wait:
	for {
		select {
		case <-done:
			break wait
		case <-deadlineC:
			timedOut = true
			e.logger().Warn("container exceeded start_to_close deadline; SIGTERM then grace",
				"attempt_id", h.AttemptID, "container", shortID(st.containerID), "grace", st.grace)
			e.stopWithGrace(h, st)
			break wait
		case <-ctx.Done():
			e.logger().Warn("wait context cancelled; stopping container",
				"attempt_id", h.AttemptID, "reason", ctx.Err())
			e.stopWithGrace(h, st)
			break wait
		}
	}

	o := e.inspectOutcome(st, timedOut)
	st.waitMu.Lock()
	st.waited, st.outcome = true, o
	st.waitMu.Unlock()
	return o, nil
}

// stopWithGrace sends SIGTERM and lets docker escalate to SIGKILL after the
// grace period. `docker stop --timeout` implements exactly that contract in the
// daemon, which is more reliable than re-implementing the escalation here: if
// the agent dies mid-grace, the daemon still finishes the job.
func (e *DockerExecutor) stopWithGrace(h Handle, st *dockerState) {
	secs := int(st.grace.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), st.grace+30*time.Second)
	defer cancel()
	if err := e.run(ctx, st.grace+30*time.Second, "stop", "--timeout", strconv.Itoa(secs), st.containerID); err != nil {
		e.logger().Warn("docker stop failed; forcing kill",
			"attempt_id", h.AttemptID, "container", shortID(st.containerID), "error", err)
		_ = e.run(ctx, 30*time.Second, "kill", st.containerID)
	}
}

// dockerInspect is the narrow slice of container state we depend on. Declaring
// it as a struct and parsing JSON (rather than templating individual fields)
// means a docker version that reorders or adds fields cannot break the parse.
type dockerInspect struct {
	State struct {
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
}

func (e *DockerExecutor) inspectOutcome(st *dockerState, timedOut bool) Outcome {
	o := Outcome{TimedOut: timedOut, WallMs: time.Since(st.startedA).Milliseconds()}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	raw, err := e.output(ctx, 30*time.Second, "inspect", st.containerID)
	if err != nil {
		// Without inspect we cannot know the exit code. Reporting an
		// INFRASTRUCTURE error is honest; reporting exit 0 would be a
		// fabricated success, which is the one thing we must never do.
		o.Err = fmt.Errorf("%w: inspect %s: %v", ErrRuntimeUnavailable, shortID(st.containerID), err)
		return o
	}
	var arr []dockerInspect
	if err := json.Unmarshal([]byte(raw), &arr); err != nil || len(arr) == 0 {
		o.Err = fmt.Errorf("%w: parse inspect output for %s: %v", ErrRuntimeUnavailable, shortID(st.containerID), err)
		return o
	}
	insp := arr[0]
	o.ExitCode = insp.State.ExitCode
	o.OOMKilled = insp.State.OOMKilled

	// Docker reports SIGKILL as 137 and SIGTERM as 143. A container killed by
	// the kernel OOM killer also exits 137, which is precisely why OOMKilled is
	// read as its own flag instead of being inferred from the code.
	switch o.ExitCode {
	case 137:
		o.Signalled, o.Signal = true, "killed"
	case 143:
		o.Signalled, o.Signal = true, "terminated"
	}
	if ts, err := time.Parse(time.RFC3339Nano, insp.State.StartedAt); err == nil {
		if te, err := time.Parse(time.RFC3339Nano, insp.State.FinishedAt); err == nil && te.After(ts) {
			o.WallMs = te.Sub(ts).Milliseconds()
		}
	}
	if st.limit.GetMemoryBytes() > 0 && o.OOMKilled {
		o.PeakMemoryBytes = st.limit.GetMemoryBytes()
	}

	// The container wrote into the bind-mounted work directory, so the outputs
	// are readable from the host with no docker cp round-trip.
	if out, ct, err := ReadOutputFile(st.workDir); err == nil {
		o.Output, o.ContentType = out, ct
	}
	var stdout []byte
	if st.stdoutOutput != "" {
		// `docker logs` keeps the two streams distinguishable on the wire, so
		// stdout can be read back WITHOUT stderr spliced into it. That separation
		// is required, not merely tidy: an output with a warning line in the
		// middle of it is corrupt data, and the failure would surface far
		// downstream as a parse error in a different task.
		stdout = e.containerStdout(st)
	}
	o.Outputs = CollectOutputs(
		TaskSpec{DeclaredOutputs: st.declaredOutputs, StdoutOutput: st.stdoutOutput},
		st.workDir, stdout)
	return o
}

// containerStdout reads the exited container's stdout stream only.
//
// It reads after exit rather than streaming during the run because the container
// is already gone by the time an output is needed, and because a streaming reader
// on the result path could apply backpressure to the workload's stdout and wedge
// the task — the log tail deliberately avoids that and so does this.
func (e *DockerExecutor) containerStdout(st *dockerState) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, e.bin(), "logs", st.containerID)
	var buf boundedBuffer
	buf.limit = MaxStdoutOutputBytes
	cmd.Stdout = &buf
	// stderr of the docker CLI itself is discarded: mixing the CLI's own
	// diagnostics into the task's result would corrupt it exactly as the
	// container's stderr would.
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		e.logger().Warn("could not read container stdout for the declared output",
			"container", shortID(st.containerID), "error", err)
		return nil
	}
	if buf.Truncated() {
		e.logger().Warn("stdout output truncated at the agent cap",
			"container", shortID(st.containerID), "limit_bytes", MaxStdoutOutputBytes)
	}
	return buf.Bytes()
}

// Signal maps the neutral signal vocabulary onto docker kill.
func (e *DockerExecutor) Signal(ctx context.Context, h Handle, sig Signal) error {
	st, ok := h.State.(*dockerState)
	if !ok || st.containerID == "" {
		return nil
	}
	var name string
	switch sig {
	case SignalTerm:
		name = "SIGTERM"
	case SignalKill:
		name = "SIGKILL"
	default:
		return fmt.Errorf("%w: unknown signal %q", ErrValidation, sig)
	}
	out, err := e.output(ctx, 30*time.Second, "kill", "--signal", name, st.containerID)
	if err != nil {
		s := strings.ToLower(out)
		// Already gone, or already stopped: the desired end state, not an error.
		if strings.Contains(s, "is not running") || strings.Contains(s, "no such container") {
			return nil
		}
		return fmt.Errorf("docker kill --signal %s %s: %w: %s", name, shortID(st.containerID), err, strings.TrimSpace(out))
	}
	return nil
}

// Logs streams the container's combined output. `--follow` is deliberate: a
// caller tailing a long-running task must see lines as they are produced, not
// only at exit.
func (e *DockerExecutor) Logs(ctx context.Context, h Handle) (io.ReadCloser, error) {
	st, ok := h.State.(*dockerState)
	if !ok || st.containerID == "" {
		return io.NopCloser(emptyReader{}), nil
	}
	cmd := exec.CommandContext(ctx, e.bin(), "logs", "--follow", st.containerID)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker logs pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker logs start: %w", err)
	}
	return &cmdReadCloser{r: pipe, cmd: cmd}, nil
}

// Cleanup force-removes the container and the work directory. Idempotent:
// removing an absent container and an absent directory are both no-ops, which
// matters because Cleanup runs on every path including the ones that already
// cleaned up.
func (e *DockerExecutor) Cleanup(ctx context.Context, h Handle) error {
	var errs []error
	if st, ok := h.State.(*dockerState); ok && st.containerID != "" {
		out, err := e.output(ctx, 60*time.Second, "rm", "--force", "--volumes", st.containerID)
		if err != nil && !strings.Contains(strings.ToLower(out), "no such container") {
			errs = append(errs, fmt.Errorf("docker rm %s: %w: %s", shortID(st.containerID), err, strings.TrimSpace(out)))
		}
	}
	if h.WorkDir != "" {
		if err := os.RemoveAll(h.WorkDir); err != nil {
			errs = append(errs, fmt.Errorf("cleanup workdir %s: %w", h.WorkDir, err))
		}
	}
	return errors.Join(errs...)
}

// ReapOrphans removes containers this agent's predecessor left behind.
//
// This is the restart half of "a killed executor leaves no orphaned workload":
// process-group kills and pdeathsig cover a dying agent, but an agent that was
// SIGKILLed leaves containers the daemon happily keeps running. On boot we find
// them by label and remove them. Without this, every agent restart permanently
// leaks a slice of the node's capacity while the control plane's ledger believes
// it is free.
func (e *DockerExecutor) ReapOrphans(ctx context.Context) (int, error) {
	out, err := e.output(ctx, 60*time.Second, "ps", "--all", "--quiet",
		"--filter", "label=nexus.managed=true")
	if err != nil {
		return 0, fmt.Errorf("%w: list orphaned containers: %v", ErrRuntimeUnavailable, err)
	}
	ids := strings.Fields(strings.TrimSpace(out))
	if len(ids) == 0 {
		return 0, nil
	}
	args := append([]string{"rm", "--force", "--volumes"}, ids...)
	if err := e.run(ctx, 2*time.Minute, args...); err != nil {
		return 0, fmt.Errorf("remove orphaned containers: %w", err)
	}
	e.logger().Warn("reaped orphaned containers left by a previous agent session", "count", len(ids))
	return len(ids), nil
}

// dockerLimitArgs translates the resource vector into container limits.
//
// These are ENFORCEMENT ceilings (`limit`), never the scheduler's `request`:
// limit >= request is what permits opportunistic bursting, and applying the
// request as a cap would throttle every task that was deliberately given
// headroom.
func dockerLimitArgs(limit *nexusv1.ResourceVector) []string {
	var args []string
	if limit == nil {
		return args
	}
	if m := limit.GetMemoryBytes(); m > 0 {
		args = append(args, "--memory", strconv.FormatInt(m, 10))
		// Equal --memory-swap disables swap for the container. Without it the
		// kernel lets an over-limit task swap instead of being OOM-killed, so
		// the task does not fail — it just runs a hundred times slower and
		// drags every other task on the node down with it. A hard failure we
		// can classify and retry is strictly better than that.
		args = append(args, "--memory-swap", strconv.FormatInt(m, 10))
	}
	if c := limit.GetCpuMillis(); c > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(float64(c)/1000.0, 'f', 3, 64))
	}
	if g := limit.GetGpuCount(); g > 0 {
		// Requires the NVIDIA container toolkit. If it is absent docker run
		// fails, which is the correct outcome: silently running a GPU task
		// without GPUs produces a confusing application-level failure hours
		// later instead of an immediate, legible one.
		args = append(args, "--gpus", strconv.Itoa(int(g)))
	}
	if d := limit.GetDiskBytes(); d > 0 {
		// Only honoured on storage drivers that support it (overlay2 on xfs
		// with pquota); docker rejects it otherwise, which again surfaces the
		// gap rather than hiding it.
		args = append(args, "--storage-opt", "size="+strconv.FormatInt(d, 10))
	}
	return args
}

func dockerEnv(spec TaskSpec, containerWD string) []string {
	// The container does NOT inherit the agent's environment: the agent's env
	// holds control-plane credentials and result-store keys that a task has no
	// business reading. Only the declared task env plus the platform variables
	// cross the boundary.
	var env []string
	for k, v := range spec.Runtime.GetEnv() {
		env = append(env, k+"="+v)
	}
	env = append(env,
		EnvAttemptID+"="+spec.AttemptID,
		EnvRunID+"="+spec.RunID,
		EnvNodeID+"="+spec.NodeID,
		EnvIdempotencyKey+"="+spec.IdempotencyKey,
		EnvWorkDir+"="+containerWD,
		EnvInputDir+"="+containerWD+"/"+InputDirName,
		EnvOutputFile+"="+containerOutputFile(spec, containerWD),
		EnvOutputDir+"="+containerWD+"/"+OutputDirName,
		EnvOutputNames+"="+strings.Join(spec.DeclaredOutputs, " "),
	)
	if !spec.StartToCloseDeadline.IsZero() {
		env = append(env, EnvDeadlineUnixMs+"="+strconv.FormatInt(spec.StartToCloseDeadline.UnixMilli(), 10))
	}
	return env
}

func (e *DockerExecutor) run(ctx context.Context, timeout time.Duration, args ...string) error {
	_, err := e.output(ctx, timeout, args...)
	return err
}

func (e *DockerExecutor) output(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, e.bin(), args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.String(), err
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cmdReadCloser ties a log stream's lifetime to the CLI process behind it, so
// closing the stream reaps the child instead of leaking a `docker logs
// --follow` process per task — at scale, a very effective way to exhaust the
// node's pid limit.
type cmdReadCloser struct {
	r   io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *cmdReadCloser) Close() error {
	err := c.r.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return err
}

// containerOutputFile mirrors OutputFilePath in the CONTAINER's path namespace.
//
// It cannot reuse OutputFilePath: that builds a host path with filepath, which on
// a Windows or Darwin agent would produce separators the Linux container cannot
// resolve. The convention must be identical from the task's point of view on
// every substrate, so the rule is shared and only the path rendering differs.
func containerOutputFile(spec TaskSpec, containerWD string) string {
	if len(spec.DeclaredOutputs) == 1 {
		return containerWD + "/" + OutputDirName + "/" + sanitizeID(spec.DeclaredOutputs[0])
	}
	return containerWD + "/" + OutputFileName
}
