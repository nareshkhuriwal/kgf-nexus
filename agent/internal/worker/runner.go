package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// LegacyOutputName is the output name given to a result produced under the
// single-output convention (<workdir>/output.json).
//
// "result" and not the task's id: it is what the committed example workflows'
// edges already bind (output_to_input: {data: result}), and renaming it would
// break every spec written before named outputs existed.
const LegacyOutputName = "result"

// LeaseTracker is the slice of the lease keeper the runner needs.
type LeaseTracker interface {
	Track(attemptID, token string, expiresAt time.Time)
	Untrack(attemptID string)
	Token(attemptID string) (string, bool)
}

// Runner executes one attempt at a time on its own goroutine, under the WAL.
type Runner struct {
	executors map[string]executor.Executor
	registry  *Registry
	leases    LeaseTracker
	reporter  *Reporter
	wal       *wal.WAL
	store     *resultstore.Client
	log       *slog.Logger

	// OutputInlineThreshold mirrors the result store's threshold so the runner
	// knows when an output needs a blob write at all.
	mu      sync.Mutex
	running map[string]*runningTask
}

type runningTask struct {
	cancel context.CancelFunc
	// grace is the cancel directive's grace period, applied on teardown.
	grace time.Duration
	exec  executor.Executor
	// handle is set once Start succeeds; a cancel before then only needs the
	// context cancellation.
	handle   *executor.Handle
	handleMu sync.Mutex
}

// NewRunner builds a runner over the executors this worker advertises.
func NewRunner(execs map[string]executor.Executor, reg *Registry, leases LeaseTracker,
	rep *Reporter, w *wal.WAL, store *resultstore.Client, log *slog.Logger) *Runner {
	return &Runner{
		executors: execs, registry: reg, leases: leases, reporter: rep,
		wal: w, store: store, log: log,
		running: make(map[string]*runningTask),
	}
}

// Executors reports the advertised executor kinds, used at registration so the
// scheduler can treat executor support as a placement constraint (ADR-0007).
func (r *Runner) Executors() []string {
	out := make([]string, 0, len(r.executors))
	for k := range r.executors {
		out = append(out, k)
	}
	return out
}

// Run executes one assignment to completion. It is the whole body of a
// per-attempt goroutine, and it OWNS the terminal cleanup: the registry release,
// the lease untrack and the executor cleanup all happen on every exit path,
// including a panic in task-adjacent code.
func (r *Runner) Run(ctx context.Context, a *nexusv1.TaskAssignment) {
	attemptID := a.GetAttemptId()
	log := r.log.With("attempt_id", attemptID, "run_id", a.GetRunId(),
		"node_id", a.GetNodeId(), "attempt_number", a.GetAttemptNumber())

	// A per-attempt cancellable context, so a CANCEL_ATTEMPT directive or a lease
	// revocation can tear down THIS attempt without touching its siblings.
	taskCtx, cancel := context.WithCancel(ctx)
	rt := &runningTask{cancel: cancel}

	r.mu.Lock()
	r.running[attemptID] = rt
	r.mu.Unlock()

	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.running, attemptID)
		r.mu.Unlock()
		// Release local capacity and stop renewing BEFORE returning, so a
		// finished attempt cannot hold a reservation or a lease. A leaked local
		// reservation shrinks this worker until restart; a leaked lease renewal
		// keeps a finished attempt alive in the control plane's ledger.
		r.leases.Untrack(attemptID)
		r.registry.Release(attemptID)

		if p := recover(); p != nil {
			log.Error("panic while running attempt; reporting INFRASTRUCTURE failure", "panic", p)
			r.reportFailure(context.WithoutCancel(ctx), a,
				executor.Classified{
					Class:     nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE,
					Type:      "AgentPanic",
					Message:   fmt.Sprintf("agent panicked while running this attempt: %v", p),
					Retryable: true,
				}, executor.Outcome{})
		}
	}()

	kind := a.GetSpec().GetRuntime().GetExecutor()
	if kind == "" {
		kind = "subprocess"
	}
	exec, ok := r.executors[kind]
	if !ok {
		// The scheduler should never place a task here whose executor we did not
		// advertise, so this is a control-plane bug rather than a task bug. Still
		// classify it INFRASTRUCTURE and retryable: a different worker may well
		// support it, and failing the user's task for our routing mistake is the
		// wrong direction to be wrong in.
		log.Error("assigned an executor this worker does not support", "executor", kind)
		r.reportFailure(taskCtx, a, executor.Classified{
			Class:     nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE,
			Type:      "UnsupportedExecutor",
			Message:   fmt.Sprintf("worker does not support executor %q", kind),
			Retryable: true,
		}, executor.Outcome{})
		return
	}
	rt.exec = exec

	// ── WAL: record the INTENT before anything runs ───────────────────────────
	// This must be durable BEFORE Start. If we crash after this and before the
	// completion record, replay reports INDETERMINATE — honest, and the control
	// plane's §6.5 policy (which knows the task's idempotency mode) decides
	// whether a retry is safe. If we crashed with no record at all, the attempt
	// would still become LOST via lease expiry, so the record costs nothing and
	// buys the ability to say "we started this".
	entry := wal.Entry{
		AttemptID:  attemptID,
		LeaseToken: a.GetLeaseToken(),
		RunID:      a.GetRunId(),
		NodeID:     a.GetNodeId(),
	}
	if err := r.wal.RecordStarting(entry); err != nil {
		// A WAL we cannot write means the next crash loses this result. Refuse the
		// work rather than run it blind: INFRASTRUCTURE + retryable sends it to a
		// healthy worker, which is strictly better than running it here and
		// possibly losing a completed result.
		log.Error("could not record STARTING in the WAL; refusing the attempt", "error", err)
		r.reportFailure(taskCtx, a, executor.Classified{
			Class:     nexusv1.ErrorClass_ERROR_CLASS_INFRASTRUCTURE,
			Type:      "WALWriteFailed",
			Message:   fmt.Sprintf("agent could not make the attempt durable: %v", err),
			Retryable: true,
		}, executor.Outcome{})
		return
	}

	token := a.GetLeaseToken()
	r.reporter.ReportStarted(taskCtx, attemptID, token)

	spec, err := r.buildExecutorSpec(taskCtx, a)
	if err != nil {
		c := executor.ClassifyError(err)
		log.Error("could not prepare the task specification", "error", err, "class", c.Class.String())
		r.reportFailure(taskCtx, a, c, executor.Outcome{})
		return
	}

	outcome, prepErr := r.execute(taskCtx, exec, rt, spec, log)
	if prepErr != nil {
		c := executor.ClassifyError(prepErr)
		log.Error("execution failed before or during the workload", "error", prepErr, "class", c.Class.String())
		r.reportFailure(taskCtx, a, c, outcome)
		return
	}

	r.reportOutcome(ctx, a, outcome, log)
}

// execute drives Prepare → Start → Wait → Cleanup. Cleanup runs on EVERY path,
// which is what makes "a killed executor leaves no orphaned workload" hold for
// the normal failure paths as well as the happy one.
func (r *Runner) execute(ctx context.Context, exec executor.Executor, rt *runningTask,
	spec executor.TaskSpec, log *slog.Logger) (executor.Outcome, error) {

	prepared, err := exec.Prepare(ctx, spec)
	if err != nil {
		return executor.Outcome{}, fmt.Errorf("prepare: %w", err)
	}

	handle, err := exec.Start(ctx, prepared)
	if err != nil {
		// Clean up the prepared state: a work directory or a pulled image left
		// behind by a failed start accumulates until the disk fills.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		_ = exec.Cleanup(cleanupCtx, executor.Handle{
			AttemptID: prepared.AttemptID, ExecutorKind: exec.Kind(), WorkDir: prepared.WorkDir,
		})
		cancel()
		return executor.Outcome{}, fmt.Errorf("start: %w", err)
	}

	rt.handleMu.Lock()
	rt.handle = &handle
	rt.handleMu.Unlock()

	// Cleanup uses a context detached from the task's: when a task is cancelled
	// or its lease revoked, the task context is ALREADY cancelled, and a cleanup
	// bound to it would be cancelled before it could remove the container — the
	// exact case in which an orphan is most likely.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := exec.Cleanup(cleanupCtx, handle); err != nil {
			log.Error("executor cleanup failed; a workload or work directory may have leaked",
				"executor", exec.Kind(), "handle", handle.ID, "error", err)
		}
	}()

	r.streamLogs(ctx, exec, handle, log)

	outcome, err := exec.Wait(ctx, handle)
	if err != nil {
		return outcome, fmt.Errorf("wait: %w", err)
	}
	return outcome, nil
}

// streamLogs tails the workload's output into the agent's log, on its own
// goroutine.
//
// Separate goroutine, and a bounded reader: a task that emits gigabytes of logs
// must not be able to block the runner or exhaust the agent's memory. The log
// stream is a diagnostic, not a result — losing the tail of a pathological log is
// strictly better than losing the task.
func (r *Runner) streamLogs(ctx context.Context, exec executor.Executor, h executor.Handle, log *slog.Logger) {
	rc, err := exec.Logs(ctx, h)
	if err != nil {
		log.Warn("could not open the workload log stream", "error", err)
		return
	}
	go func() {
		defer rc.Close()
		const maxLogBytes = 16 << 20 // 16 MiB per attempt
		lr := io.LimitReader(rc, maxLogBytes)
		buf := make([]byte, 32<<10)
		for {
			n, err := lr.Read(buf)
			if n > 0 {
				log.Info("task log", "output", string(buf[:n]))
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && ctx.Err() == nil {
					log.Debug("log stream ended", "error", err)
				}
				return
			}
		}
	}()
}

// buildExecutorSpec resolves the assignment into an executor spec, FETCHING blob
// inputs from the result store directly.
//
// Directly: the payload never transits the control plane (ADR-0008, invariant 6).
// A large input routed through the API gateway would contend with heartbeats, and
// a starved heartbeat is duplicated work — a data-path decision with a
// correctness consequence.
func (r *Runner) buildExecutorSpec(ctx context.Context, a *nexusv1.TaskAssignment) (executor.TaskSpec, error) {
	spec := a.GetSpec()
	inputs := make(map[string]*nexusv1.ResultRef, len(a.GetInputs()))
	for name, ref := range a.GetInputs() {
		if ref == nil {
			continue
		}
		if _, isBlob := ref.GetKind().(*nexusv1.ResultRef_Blob); !isBlob {
			inputs[name] = ref
			continue
		}
		if r.store == nil {
			return executor.TaskSpec{}, fmt.Errorf("%w: input %q is a blob ref but no result store is configured",
				executor.ErrResultStore, name)
		}
		b, err := r.store.Get(ctx, ref)
		if err != nil {
			return executor.TaskSpec{}, fmt.Errorf("%w: fetch input %q: %v", executor.ErrResultStore, name, err)
		}
		// Hand the executor inline bytes: the executor packages stay free of any
		// object-store dependency, which also means the conformance suite runs
		// with no MinIO in the loop.
		inputs[name] = &nexusv1.ResultRef{
			Kind:        &nexusv1.ResultRef_Inline{Inline: b},
			ContentType: ref.GetContentType(),
			SizeBytes:   int64(len(b)),
		}
	}

	var deadline time.Time
	if ms := a.GetStartToCloseDeadlineUnixMs(); ms > 0 {
		// Absolute, from CONTROL-PLANE time. We do not recompute it from a local
		// duration: worker clocks are never authoritative (invariant 7), and a
		// worker whose clock is fast would cut its own tasks short.
		deadline = time.UnixMilli(ms)
	} else if d := spec.GetTimeouts().GetStartToCloseMs(); d > 0 {
		deadline = time.Now().Add(time.Duration(d) * time.Millisecond)
	}

	return executor.TaskSpec{
		AttemptID:            a.GetAttemptId(),
		RunID:                a.GetRunId(),
		NodeID:               a.GetNodeId(),
		IdempotencyKey:       a.GetIdempotencyKey(),
		Runtime:              spec.GetRuntime(),
		Limit:                effectiveLimit(spec.GetResources()),
		Inputs:               inputs,
		DeclaredOutputs:      declaredOutputNames(spec),
		StdoutOutput:         spec.GetRuntime().GetStdoutOutput(),
		StartToCloseDeadline: deadline,
		GraceAfterTerm:       executor.DefaultGraceAfterTerm,
	}, nil
}

// declaredOutputNames lists TaskSpec.outputs in a STABLE order.
//
// Sorted because Go map iteration is randomized and the order reaches the
// workload through NEXUS_OUTPUT_NAMES: a shell task that loops over that
// variable would otherwise produce its outputs in a different order on every
// attempt, which makes a task's own logs non-comparable between retries for no
// reason.
func declaredOutputNames(spec *nexusv1.TaskSpec) []string {
	outs := spec.GetOutputs()
	if len(outs) == 0 {
		return nil
	}
	names := make([]string, 0, len(outs))
	for name := range outs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// effectiveLimit picks the enforcement ceiling.
//
// `limit` when present, else `request`. The two are deliberately distinct
// (common.proto): request is what the scheduler subtracts from capacity, limit is
// what the executor enforces, and limit >= request is what permits opportunistic
// bursting. Falling back to request when no limit is declared gives predictable
// performance rather than an unbounded task.
func effectiveLimit(rr *nexusv1.ResourceRequest) *nexusv1.ResourceVector {
	if l := rr.GetLimit(); l != nil {
		return l
	}
	return rr.GetRequest()
}

// reportOutcome converts an executor Outcome into a ReportResult and delivers it
// through the reporter (which owns the WAL ordering).
func (r *Runner) reportOutcome(ctx context.Context, a *nexusv1.TaskAssignment,
	o executor.Outcome, log *slog.Logger) {

	// Detached context: the result MUST be reported even when the parent context
	// is being cancelled by shutdown. A completed task whose report we abandoned
	// because we were shutting down is precisely the waste the WAL exists to
	// prevent, and reporting now is cheaper than replaying at next boot.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	token := r.currentToken(a)
	req := &nexusv1.ReportResultRequest{
		AttemptId:     a.GetAttemptId(),
		LeaseToken:    token,
		EndedAtUnixMs: time.Now().UnixMilli(),
		Metrics: &nexusv1.ExecutionMetrics{
			PeakMemoryBytes: o.PeakMemoryBytes,
			CpuSeconds:      o.CPUSeconds,
			WallMs:          o.WallMs,
			OomKilled:       boolToInt32(o.OOMKilled),
			ExitCode:        int32(o.ExitCode),
		},
	}

	switch {
	case o.Succeeded():
		req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED
		refs, err := r.storeOutputs(reportCtx, a, o, log)
		if err != nil {
			// We cannot persist an output, so we cannot honestly claim success: a
			// downstream task would read a reference to nothing. INFRASTRUCTURE +
			// retryable is the truthful report.
			log.Error("could not store the task outputs; reporting INFRASTRUCTURE failure", "error", err)
			req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
			req.Error = executor.ClassifyError(fmt.Errorf("%w: %v", executor.ErrResultStore, err)).ToProto()
		} else if len(refs) > 0 {
			req.Outputs = refs
		}
	case o.TimedOut:
		req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT
		req.Error = executor.ClassifiedFromOutcome(o, "").ToProto()
	default:
		req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
		req.Error = executor.ClassifiedFromOutcome(o, "").ToProto()
	}

	entry := wal.Entry{
		AttemptID: a.GetAttemptId(), LeaseToken: token,
		RunID: a.GetRunId(), NodeID: a.GetNodeId(),
	}
	if err := r.reporter.ReportResult(reportCtx, req, entry); err != nil {
		log.Error("failed to report the attempt result", "error", err)
	}
}

// storeOutputs writes every output the workload produced and returns the refs
// that become task_attempts.output_ref.
//
// The upload goes DIRECTLY from this agent to object storage (ADR-0008,
// invariant 6). Only the resulting ResultRef — a bucket, a key and a digest —
// travels to the control plane, so a 300 MiB result costs the control plane a
// few hundred bytes. Routing the payload through the API gateway instead would
// contend with heartbeats on the same path, and a starved heartbeat is a
// wrongly-declared-dead worker, which is duplicated work: a data-path shortcut
// with a correctness consequence.
//
// The named outputs take precedence over the legacy single output. A task that
// declares outputs and writes them by name gets exactly those names; a task that
// declares none and writes the legacy output.json still gets "result", which is
// the name the pre-existing edges in examples/ bind.
func (r *Runner) storeOutputs(ctx context.Context, a *nexusv1.TaskAssignment,
	o executor.Outcome, log *slog.Logger) (map[string]*nexusv1.ResultRef, error) {

	produced := o.Outputs
	if len(produced) == 0 && len(o.Output) > 0 {
		produced = map[string]executor.NamedOutput{
			LegacyOutputName: {Bytes: o.Output, ContentType: o.ContentType},
		}
	}
	if len(produced) == 0 {
		// No outputs is not a failure. Plenty of tasks are pure side effect, and
		// failing them would make the whole class unrunnable.
		return nil, nil
	}

	refs := make(map[string]*nexusv1.ResultRef, len(produced))
	for name, out := range produced {
		ref, err := r.putOutput(ctx, out)
		if err != nil {
			return nil, fmt.Errorf("store output %q: %w", name, err)
		}
		refs[name] = ref
		if _, isBlob := ref.GetKind().(*nexusv1.ResultRef_Blob); isBlob {
			log.Info("stored task output as a blob",
				"output", name, "size_bytes", ref.GetSizeBytes(),
				"bucket", ref.GetBlob().GetBucket(), "key", ref.GetBlob().GetKey(),
				"from_stdout", out.FromStdout)
		} else {
			log.Debug("stored task output inline",
				"output", name, "size_bytes", ref.GetSizeBytes(), "from_stdout", out.FromStdout)
		}
	}
	return refs, nil
}

// putOutput inlines a small payload and writes a large one to object storage.
func (r *Runner) putOutput(ctx context.Context, out executor.NamedOutput) (*nexusv1.ResultRef, error) {
	ct := out.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	if r.store == nil {
		// No store configured (a development agent). Inline regardless: it is
		// better than failing a task that produced a small, valid result. A
		// payload above the threshold is NOT inlined here — that would push a
		// large payload through the control plane, which is the one thing
		// invariant 6 forbids.
		if int64(len(out.Bytes)) > resultstore.DefaultInlineThresholdBytes {
			return nil, fmt.Errorf("%w: output is %d bytes and no result store is configured, "+
				"so it cannot be stored without routing the payload through the control plane",
				executor.ErrResultStore, len(out.Bytes))
		}
		return &nexusv1.ResultRef{
			Kind:        &nexusv1.ResultRef_Inline{Inline: out.Bytes},
			ContentType: ct,
			SizeBytes:   int64(len(out.Bytes)),
		}, nil
	}
	return r.store.Put(ctx, out.Bytes, ct)
}

func (r *Runner) reportFailure(ctx context.Context, a *nexusv1.TaskAssignment,
	c executor.Classified, o executor.Outcome) {

	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	token := r.currentToken(a)
	req := &nexusv1.ReportResultRequest{
		AttemptId:     a.GetAttemptId(),
		LeaseToken:    token,
		Outcome:       nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED,
		Error:         c.ToProto(),
		EndedAtUnixMs: time.Now().UnixMilli(),
		Metrics: &nexusv1.ExecutionMetrics{
			PeakMemoryBytes: o.PeakMemoryBytes, CpuSeconds: o.CPUSeconds,
			WallMs: o.WallMs, OomKilled: boolToInt32(o.OOMKilled), ExitCode: int32(o.ExitCode),
		},
	}
	if c.Class == nexusv1.ErrorClass_ERROR_CLASS_TIMEOUT {
		req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT
	}
	entry := wal.Entry{AttemptID: a.GetAttemptId(), LeaseToken: token,
		RunID: a.GetRunId(), NodeID: a.GetNodeId()}
	if err := r.reporter.ReportResult(reportCtx, req, entry); err != nil {
		r.log.Error("failed to report a failed attempt", "attempt_id", a.GetAttemptId(), "error", err)
	}
}

// currentToken prefers the lease keeper's token over the assignment's, because a
// renewal can bump the epoch and only the current token will be accepted.
func (r *Runner) currentToken(a *nexusv1.TaskAssignment) string {
	if t, ok := r.leases.Token(a.GetAttemptId()); ok && t != "" {
		return t
	}
	return a.GetLeaseToken()
}

// Cancel tears an attempt down on the SIGTERM → grace → SIGKILL contract, driven
// by a CANCEL_ATTEMPT directive or a lease revocation.
//
// It returns immediately and does the work asynchronously: it is called from the
// heartbeat and lease goroutines, and blocking either of them for the grace
// period would delay the next beat or stall renewal for every sibling attempt.
func (r *Runner) Cancel(attemptID string, grace time.Duration, reason string) {
	r.mu.Lock()
	rt, ok := r.running[attemptID]
	r.mu.Unlock()
	if !ok {
		return // already finished; nothing to cancel
	}
	log := r.log.With("attempt_id", attemptID, "reason", reason)
	log.Warn("cancelling attempt", "grace", grace)

	go func() {
		rt.handleMu.Lock()
		h, exec := rt.handle, rt.exec
		rt.handleMu.Unlock()

		if h == nil || exec == nil {
			// Not started yet: cancelling the context is sufficient and Prepare
			// or Start will abandon.
			rt.cancel()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), grace+time.Minute)
		defer cancel()

		if err := exec.Signal(ctx, *h, executor.SignalTerm); err != nil {
			log.Warn("SIGTERM delivery failed; escalating", "error", err)
		}
		if grace <= 0 {
			grace = executor.DefaultGraceAfterTerm
		}
		// Cancelling the task context AFTER the grace timer, not before, is what
		// honours the grace period: Wait's own cancellation path escalates to
		// SIGKILL, so cancelling early would skip the grace the operator asked
		// for and lose whatever the task was flushing.
		time.AfterFunc(grace, func() {
			rt.handleMu.Lock()
			h2 := rt.handle
			rt.handleMu.Unlock()
			if h2 != nil {
				if err := exec.Signal(context.Background(), *h2, executor.SignalKill); err != nil {
					log.Warn("SIGKILL delivery failed", "error", err)
				}
			}
			rt.cancel()
		})
	}()
}

// OnLeaseRevoked implements lease.RevocationHandler. A revoked lease means the
// work is already someone else's (or cancelled), so any result we produce will be
// rejected on token mismatch — continuing is guaranteed waste of capacity that
// could be running work that counts.
func (r *Runner) OnLeaseRevoked(attemptID, reason string) {
	// A short grace rather than an immediate kill: the task may be mid-flush, and
	// a few seconds costs nothing once we have already decided to discard the
	// outcome.
	r.Cancel(attemptID, 5*time.Second, "lease_revoked:"+reason)
}

// RunningCount reports how many attempts are executing.
func (r *Runner) RunningCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}

func boolToInt32(b bool) int32 {
	if b {
		return 1
	}
	return 0
}
