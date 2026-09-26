// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package executor is the pluggable execution substrate inside the worker
// agent (ADR-0007).
//
// The abstraction lives HERE, in the agent, and not in the control plane. If
// executor choice were a control-plane concern the control plane would have to
// understand container runtimes and Kubernetes APIs and their failure modes —
// exactly the coupling that plane separation exists to prevent. The control
// plane only ever sees a string in `runtime.executor` and treats it as a
// placement constraint, like a label.
package executor

import (
	"context"
	"io"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// Signal is the executor-neutral signal vocabulary. Only the two signals the
// contract actually requires are modelled: widening this set would invite
// executors to diverge on signals no conformance test covers.
type Signal string

const (
	// SignalTerm asks the workload to shut down cleanly. The grace period
	// after it is what lets a task flush a checkpoint instead of losing an
	// hour of GPU time.
	SignalTerm Signal = "TERM"
	// SignalKill is unconditional and un-catchable.
	SignalKill Signal = "KILL"
)

// TaskSpec is everything an executor needs to run one attempt. It is a
// narrowed projection of nexusv1.TaskAssignment rather than the proto itself,
// so the executor contract does not drift every time the wire schema grows a
// field that execution does not care about.
type TaskSpec struct {
	AttemptID string
	RunID     string
	NodeID    string

	// TraceEnv is the attempt's W3C trace context as process environment
	// (TRACEPARENT, and TRACESTATE when set), following the OpenTelemetry
	// environment-carrier convention (ADR-0026). Every executor passes it to
	// the workload with the platform variables, so an instrumented task (the
	// Python SDK with its opentelemetry extra, or any program honouring the
	// convention) continues the attempt's trace. Empty when untraced.
	TraceEnv map[string]string

	// IdempotencyKey is handed to the workload as NEXUS_IDEMPOTENCY_KEY. It is
	// stable across retries by construction, which is the mechanism that lets
	// a handler deduplicate its own side effects.
	IdempotencyKey string

	Runtime *nexusv1.RuntimeSpec

	// Limit is the ENFORCEMENT ceiling (cgroup / container limit), which is
	// deliberately distinct from the scheduler's `request`: limit >= request
	// permits opportunistic bursting.
	Limit *nexusv1.ResourceVector

	// GPUDevices are the device indices this attempt holds on the worker,
	// assigned by the worker's registry at admission. Executors expose exactly
	// these (CUDA_VISIBLE_DEVICES, docker --gpus device=...) so concurrent GPU
	// attempts land on different devices. Nil means none were assigned.
	GPUDevices []int

	// Inputs are already-resolved references. Staging them is the executor's
	// job because the payload must never transit the control plane (ADR-0008).
	Inputs map[string]*nexusv1.ResultRef

	// DeclaredOutputs are the output names from TaskSpec.outputs, sorted. The
	// executor needs them because the output convention is name-driven: it
	// exports NEXUS_OUTPUT_FILE for a single-output task and collects
	// $NEXUS_OUTPUT_DIR/<name> for every declared name. Undeclared files in the
	// output directory are IGNORED rather than guessed at, so a task cannot
	// smuggle an output past the DAG's static type validation.
	DeclaredOutputs []string

	// StdoutOutput names the declared output that receives stdout, or is empty
	// to capture nothing (RuntimeSpec.stdout_output).
	StdoutOutput string

	// InlineThresholdBytes is the control plane's inline-versus-blob boundary
	// (ADR-0016), already clamped by the agent. Exported to the workload as
	// NEXUS_INLINE_THRESHOLD_BYTES when positive; zero exports nothing.
	InlineThresholdBytes int64

	// StartToCloseDeadline is absolute and computed from CONTROL-PLANE time.
	// Worker clocks are never authoritative, so the executor treats this as
	// given rather than deriving it from a local duration.
	StartToCloseDeadline time.Time
	// GraceAfterTerm is how long SIGTERM is given before SIGKILL follows.
	GraceAfterTerm time.Duration

	// WorkerID is the agent's per-session identity. Informational only (the
	// kubernetes executor labels its Jobs with it so `kubectl get jobs -l`
	// answers "what is this worker running"); nothing keys correctness on it,
	// because it changes on every re-registration.
	WorkerID string

	// OnPhase, when set, is called on every observed change in the workload's
	// substrate-level lifecycle (for kubernetes: pod phase and the reason
	// behind it). It exists so the control plane can see WHY an attempt that
	// it considers RUNNING has not started — "0/12 nodes available:
	// insufficient nvidia.com/gpu" — which is ADR-0007's mitigation for nested
	// scheduling. It MUST NOT block: it is called from the executor's wait loop.
	// Executors whose workload has no such lifecycle never call it.
	OnPhase func(phase, detail string)

	// LogSink, when set, receives the workload's output for durable shipping
	// (ADR-0020). Executors that implement LogCapturer write to it directly,
	// tagged by stream; for the others the runner follows Logs() into it. Nil
	// leaves every executor's capture exactly as it was before log shipping.
	LogSink LogSink
}

// Prepared is an executor's staged, not-yet-running workload: a materialised
// input directory, a pulled image, a written command line.
//
// Prepare is separate from Start because preparation can fail in ways that
// classify very differently (a missing image is NOT_FOUND and terminal; a
// registry timeout is TRANSIENT and retryable) and because it can be slow. A
// combined Start would make those failures indistinguishable from a workload
// crash.
type Prepared struct {
	AttemptID string
	// WorkDir is the attempt's scratch directory: staged inputs, the output
	// drop, captured logs.
	WorkDir string
	// Opaque per-executor state (resolved image ref, argv, env).
	State any
}

// Handle identifies a running workload. It must carry enough to signal and
// clean up the workload after an agent RESTART in principle — hence the
// exported, serialisable identity fields rather than a live process object.
type Handle struct {
	AttemptID string
	// ExecutorKind is recorded so orphan cleanup after a restart knows which
	// implementation owns the workload.
	ExecutorKind string
	// ID is the OS pid, container id, or pod name.
	ID string
	// WorkDir mirrors Prepared.WorkDir so Cleanup needs nothing else.
	WorkDir   string
	StartedAt time.Time
	// Opaque live state (the *exec.Cmd, the docker client handle).
	State any
}

// Outcome is the result of waiting on a workload. It is intentionally a
// STATEMENT OF FACT about the process, not a verdict about the attempt: the
// mapping from facts to nexusv1 outcomes belongs to the caller, which also
// knows about leases and deadlines the executor does not.
type Outcome struct {
	ExitCode int
	// OOMKilled is a first-class field rather than an inferred exit code
	// because retrying an OOM on a bigger worker can succeed, while retrying
	// an application error cannot. Conflating them wastes retry budget and
	// hides the real fix.
	OOMKilled bool
	// TimedOut means the executor itself enforced the start_to_close deadline.
	TimedOut bool
	// Signalled means the workload died from a signal rather than exiting.
	Signalled bool
	Signal    string

	PeakMemoryBytes int64
	CPUSeconds      float64
	WallMs          int64

	// Output is the workload's result under the SINGLE-output convention
	// (output.json in WorkDir). Raw bytes: the executor does not interpret it.
	// Retained because it predates named outputs and a task that writes only
	// output.json must keep working.
	Output      []byte
	ContentType string

	// Outputs are the workload's DECLARED, named results: one entry per name in
	// TaskSpec.outputs that the workload actually produced. Keyed by output name
	// because that is what an Edge binds (Edge.output_to_input) and what
	// task_attempts.output_ref is keyed by — a positional or single-valued
	// result could not express a task with two outputs at all.
	//
	// Absent names are simply absent, not empty: "the task produced no metrics"
	// and "the task produced empty metrics" are different facts, and only the
	// first should leave a downstream input unresolved.
	Outputs map[string]NamedOutput

	// Err is an executor-level failure (could not start, runtime broke), NOT a
	// workload failure. A non-zero ExitCode is a successful execution of a
	// failing task and leaves this nil.
	Err error
}

// NamedOutput is one declared output's bytes as the workload produced them.
//
// Bytes rather than a path: the caller (which owns the result-store client)
// decides inline-versus-blob and must hash the content, and the executor's work
// directory is deleted by Cleanup before the caller could read a path.
type NamedOutput struct {
	Bytes       []byte
	ContentType string
	// FromStdout records that this came from the stdout opt-in rather than from a
	// file, which is worth logging: a task that accidentally prints diagnostics
	// to stdout gets them recorded as its result, and the operator needs to be
	// able to see why.
	FromStdout bool
}

// Succeeded is the single definition of workload success, so subprocess and
// docker cannot drift on it.
func (o Outcome) Succeeded() bool {
	return o.Err == nil && o.ExitCode == 0 && !o.OOMKilled && !o.TimedOut
}

// Executor is the contract from ADR-0007, unchanged in shape.
//
// Every method takes a context because every one of them can block on I/O that
// must be abandonable when the lease is revoked: a revoked lease means another
// attempt may already be running, so continuing to pull an image or stage
// inputs is pure waste.
type Executor interface {
	// Kind is the identifier advertised at registration and matched against
	// `runtime.executor`.
	Kind() string

	Prepare(ctx context.Context, spec TaskSpec) (Prepared, error)
	Start(ctx context.Context, p Prepared) (Handle, error)
	Wait(ctx context.Context, h Handle) (Outcome, error)
	Signal(ctx context.Context, h Handle, sig Signal) error
	Logs(ctx context.Context, h Handle) (io.ReadCloser, error)

	// Cleanup MUST be idempotent and MUST leave no orphaned workload. It is
	// called on every path — success, failure, cancel, panic — so a Cleanup
	// that errors on an already-cleaned handle would turn a normal shutdown
	// into a noisy one, and a Cleanup that leaves the container running would
	// leak the node's capacity for as long as the node lives.
	Cleanup(ctx context.Context, h Handle) error
}

// Conventional filenames inside WorkDir. Fixed by convention rather than
// configuration so the same handler code runs unchanged under every executor.
const (
	// OutputFileName is where a workload writes its result. Absent means "no
	// output", which is not an error: plenty of tasks are pure side effect.
	OutputFileName = "output.json"
	// OutputDirName holds DECLARED outputs, one file per output name. This is
	// the general convention: it is the only one that can express a task with
	// several outputs, and the only one that can carry binary data safely.
	OutputDirName = "outputs"
	// InputDirName holds staged inputs, one file per named input.
	InputDirName = "inputs"
	// LogFileName is the combined stdout+stderr capture. Combined rather than
	// split because interleaved ordering is what an operator actually reads.
	LogFileName = "combined.log"
)

// Environment variable names handed to every workload. Part of the executor
// contract: a handler written against these works on any substrate.
const (
	EnvAttemptID      = "NEXUS_ATTEMPT_ID"
	EnvRunID          = "NEXUS_RUN_ID"
	EnvNodeID         = "NEXUS_NODE_ID"
	EnvIdempotencyKey = "NEXUS_IDEMPOTENCY_KEY"
	EnvInputDir       = "NEXUS_INPUT_DIR"
	EnvOutputFile     = "NEXUS_OUTPUT_FILE"
	// EnvOutputDir points at the directory in which a workload writes one file
	// per declared output name. Exported ALWAYS, even for a task that declares
	// no outputs, so a handler can write unconditionally without probing.
	EnvOutputDir = "NEXUS_OUTPUT_DIR"
	// EnvOutputNames is the space-separated list of declared output names, so a
	// shell task can loop over exactly what the platform will collect instead of
	// discovering silently-dropped files at runtime.
	EnvOutputNames    = "NEXUS_OUTPUT_NAMES"
	EnvWorkDir        = "NEXUS_WORK_DIR"
	EnvDeadlineUnixMs = "NEXUS_DEADLINE_UNIX_MS"
	// EnvInlineThreshold is the control plane's configured inline-versus-blob
	// boundary in bytes (ADR-0016), so an in-task result store -- the Python
	// SDK's ctx.store -- inlines exactly what the agent itself would, instead
	// of a compiled-in 64 KiB. Exported only when the agent knows the value.
	EnvInlineThreshold = "NEXUS_INLINE_THRESHOLD_BYTES"
)

// traceEnvList renders TraceEnv as KEY=VALUE pairs in a fixed order. Only the
// two convention keys are passed, whatever the map holds: the trace context
// arrives from the control plane, and nothing else from it belongs in a task's
// environment.
func traceEnvList(spec TaskSpec) []string {
	var out []string
	for _, k := range []string{"TRACEPARENT", "TRACESTATE"} {
		if v := spec.TraceEnv[k]; v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}
