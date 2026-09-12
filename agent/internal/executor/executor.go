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

	// IdempotencyKey is handed to the workload as NEXUS_IDEMPOTENCY_KEY. It is
	// stable across retries by construction, which is the mechanism that lets
	// a handler deduplicate its own side effects.
	IdempotencyKey string

	Runtime *nexusv1.RuntimeSpec

	// Limit is the ENFORCEMENT ceiling (cgroup / container limit), which is
	// deliberately distinct from the scheduler's `request`: limit >= request
	// permits opportunistic bursting.
	Limit *nexusv1.ResourceVector

	// Inputs are already-resolved references. Staging them is the executor's
	// job because the payload must never transit the control plane (ADR-0008).
	Inputs map[string]*nexusv1.ResultRef

	// StartToCloseDeadline is absolute and computed from CONTROL-PLANE time.
	// Worker clocks are never authoritative, so the executor treats this as
	// given rather than deriving it from a local duration.
	StartToCloseDeadline time.Time
	// GraceAfterTerm is how long SIGTERM is given before SIGKILL follows.
	GraceAfterTerm time.Duration
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

	// Output is the workload's declared result, read from the conventional
	// output file in WorkDir. Raw bytes: the executor does not interpret it.
	Output      []byte
	ContentType string

	// Err is an executor-level failure (could not start, runtime broke), NOT a
	// workload failure. A non-zero ExitCode is a successful execution of a
	// failing task and leaves this nil.
	Err error
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
	EnvWorkDir        = "NEXUS_WORK_DIR"
	EnvDeadlineUnixMs = "NEXUS_DEADLINE_UNIX_MS"
)
