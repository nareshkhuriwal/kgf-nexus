// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/agentmetrics"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/artifactcache"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/logship"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/resultstore"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/secrets"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/taskapi"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/wal"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"
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

	// UninterruptibleCeiling is the platform's hard cap on how long an
	// external cancellation may be deferred for a task that declared itself
	// uninterruptible (ADR-0015). Zero means DefaultUninterruptibleCeiling. It
	// is operator configuration on the agent -- the component that enforces
	// it -- so no workflow spec can hold this worker longer than the operator
	// allows. Set before the first Run; read-only afterwards.
	UninterruptibleCeiling time.Duration

	// InlineThreshold is the control plane's inline-versus-blob boundary
	// (ADR-0016), used on the no-result-store path to decide what may be
	// inlined at all. Zero means the platform default. Set before the first
	// Run; read-only afterwards.
	InlineThreshold int64

	mu      sync.Mutex
	running map[string]*runningTask

	// WorkerID is this session's identity, handed to executors that label
	// their workloads with it. Set after registration.
	WorkerID string
	// GPUKind and GPUCount are the GPUs this worker advertises, exported to
	// every workload (executor.EnvWorkerGPUKind / EnvWorkerGPUCount). Set with
	// WorkerID.
	GPUKind  string
	GPUCount int32

	// Metrics records task starts, outcomes and durations. Optional: nil
	// records nothing.
	Metrics *agentmetrics.Metrics
	// LogShipping is the control plane's log policy from RegisterResponse
	// (ADR-0020). Nil or disabled ships nothing. Set before the first Run.
	LogShipping *nexusv1.LogShippingConfig
	// LogIndexer is the ReportLogs RPC. Nil ships nothing.
	LogIndexer logship.Indexer
	// LogSpoolDir holds per-attempt spools for log compaction. Empty uses the
	// OS temp directory.
	LogSpoolDir string
	// LogFinalizeTimeout overrides the shipper's finalize budget; zero uses
	// logship.DefaultFinalizeTimeout.
	LogFinalizeTimeout time.Duration
	// SecretValues adds values to redact beyond the attempt's own resolved
	// secrets, which are always redacted (ADR-0025). Optional.
	SecretValues func(a *nexusv1.TaskAssignment) map[string]string
	// Secrets resolves the attempt's SecretRefs with this tenant's providers
	// (ADR-0025). Nil means no providers: a task that declares a secret fails
	// PERMISSION_DENIED instead of running without it. Set before first Run.
	Secrets *secrets.Resolver
	// Namespace prefixes the attempt's log and checkpoint objects; the
	// agent's namespace.
	Namespace string

	// WorkloadAPI is RegisterResponse.workload_api (ADR-0032). Nil offers
	// workloads no usage, rate limits or checkpoints; the socket is still
	// served for progress and declared errors. Set before the first Run.
	WorkloadAPI *nexusv1.WorkloadApiConfig
	// WorkloadUpstream forwards the workload API's calls. Nil disables
	// everything that needs the control plane.
	WorkloadUpstream taskapi.Upstream
	// TaskAPIRoot is where per-attempt sockets are created; empty means
	// taskapi.DefaultRoot.
	TaskAPIRoot string
	// TaskAPIDisabled serves no workload API at all (the root was found
	// unusable at registration).
	TaskAPIDisabled bool
	// CheckpointStore overrides the result store for checkpoint objects. Nil
	// uses the result store; tests set it to an in-memory store.
	CheckpointStore taskapi.ObjectStore
	// Cache is the node-local artifact cache (ADR-0030 §4). Nil disables it:
	// artifacts are then the task's own business, as before.
	Cache *artifactcache.Cache
}

// newShipper starts the attempt's log shipper, or returns nil when shipping is
// off (old gateway, no result store, no indexer) -- the attempt then runs
// exactly as it did before ADR-0020.
func (r *Runner) newShipper(a *nexusv1.TaskAssignment, redactor *logship.Redactor, log *slog.Logger) *logship.Shipper {
	if r.LogShipping == nil || !r.LogShipping.GetEnabled() || r.LogIndexer == nil || r.store == nil || r.Namespace == "" {
		return nil
	}
	return logship.New(logship.Config{
		Namespace:       r.Namespace,
		AttemptID:       a.GetAttemptId(),
		MaxBytes:        r.LogShipping.GetMaxBytesPerAttempt(),
		ChunkBytes:      r.LogShipping.GetChunkBytes(),
		FlushInterval:   time.Duration(r.LogShipping.GetFlushIntervalMs()) * time.Millisecond,
		FinalizeTimeout: r.LogFinalizeTimeout,
		SpoolDir:        r.LogSpoolDir,
		Redactor:        redactor,
		Token:           func() string { return r.currentToken(a) },
		Log:             log,
	}, r.store, r.LogIndexer)
}

// finishLogs finalizes the attempt's log BEFORE its result is reported: the
// final ReportLogs is lease-fenced, and the result report releases the lease.
func (r *Runner) finishLogs(ctx context.Context, ship *logship.Shipper, log *slog.Logger) {
	if ship == nil {
		return
	}
	st := ship.Close(context.WithoutCancel(ctx))
	log.Info("attempt log shipped", "lines", st.Lines, "bytes", st.Bytes, "chunks", st.Chunks,
		"finalized", st.Finalized, "compacted", st.Compacted, "truncated_lines", st.TruncatedLines,
		"dropped_lines", st.DroppedLines, "redactions", st.Redactions, "fenced", st.Fenced,
		"outcome", st.Outcome)
	r.Metrics.LogShipped(st)
}

// DefaultUninterruptibleCeiling bounds a deferred cancellation when the agent is
// not configured otherwise. Ten minutes: long enough for a typical
// non-idempotent critical section (a payment capture, a schema step) to finish,
// short enough that a cancelled run's capacity comes back within one coffee
// break rather than one shift.
const DefaultUninterruptibleCeiling = 10 * time.Minute

// EffectiveUninterruptibleBound is how long a cancellation of a task with this
// policy is deferred: the declared bound, clamped to the ceiling, or the
// ceiling when none was declared. Zero for an interruptible task.
//
// The clamp is here, on the enforcing side, and nowhere else. A second copy in
// the control plane would be a second ceiling that can drift from this one.
func EffectiveUninterruptibleBound(p *nexusv1.CancellationPolicy, ceiling time.Duration) time.Duration {
	if !p.GetUninterruptible() {
		return 0
	}
	if ceiling <= 0 {
		ceiling = DefaultUninterruptibleCeiling
	}
	declared := time.Duration(p.GetMaxUninterruptibleMs()) * time.Millisecond
	if declared <= 0 || declared > ceiling {
		return ceiling
	}
	return declared
}

type runningTask struct {
	// policy is the task's declared cancellation policy (ADR-0015).
	policy *nexusv1.CancellationPolicy
	// deferring is set, under Runner.mu, once a cancellation of an
	// uninterruptible task has started its bounded wait. The control plane
	// repeats CANCEL_ATTEMPT on every heartbeat and a revoked lease cancels
	// again. The first wait's timer already bounds the task, so a repeat must
	// not start another: over a ten-minute ceiling that would be ~120 parked
	// goroutines, each firing its own redundant SIGKILL and WARN line.
	deferring bool
	// done is closed when Run returns, which is how a deferred cancellation
	// learns the task finished on its own and no SIGKILL is needed.
	done   chan struct{}
	cancel context.CancelFunc
	// grace is the cancel directive's grace period, applied on teardown.
	grace time.Duration
	exec  executor.Executor
	// handle is set once Start succeeds; a cancel before then only needs the
	// context cancellation.
	handle   *executor.Handle
	handleMu sync.Mutex

	// redactor replaces the attempt's secret values in everything that leaves
	// the runner as text: shipped logs, the agent-log copy, progress notes
	// and the reported error message. Set once, before the workload starts;
	// nil redacts nothing.
	redactor *logship.Redactor

	// kind and started feed the task metrics. Written by the attempt's own
	// goroutine before any report, and read by that goroutine only.
	kind    string
	started time.Time

	// cancelRequested is set once the agent has decided to stop the workload
	// (Cancel, a revoked lease, an uninterruptible bound expiring). A
	// workload that then dies of our signal did not fail for the reason it
	// may have declared through the workload API.
	cancelRequested atomic.Bool
	// preempted is set by Preempt: the node is being reclaimed, and a
	// non-successful outcome is reported as PREEMPTED (ADR-0030 §2).
	preempted atomic.Bool
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

	// Continue the run's trace from the dispatch span the scheduler recorded
	// (ADR-0026). The attempt's whole life on this worker -- inputs, workload,
	// logs, outputs, report -- is one span with phase children, and the log
	// lines of this attempt carry its trace_id.
	ctx, span := tracing.Start(tracing.Extract(ctx, a.GetTraceContext()), "attempt.execute",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			tracing.AttrRunID.String(a.GetRunId()),
			tracing.AttrNodeID.String(a.GetNodeId()),
			tracing.AttrAttemptID.String(attemptID),
			tracing.AttrAttemptNumber.Int(int(a.GetAttemptNumber())),
			tracing.AttrWorkerID.String(r.WorkerID),
			tracing.AttrExecutor.String(a.GetSpec().GetRuntime().GetExecutor())))
	defer span.End()

	log := tracing.WithTrace(ctx, r.log.With("attempt_id", attemptID, "run_id", a.GetRunId(),
		"node_id", a.GetNodeId(), "attempt_number", a.GetAttemptNumber()))

	// A per-attempt cancellable context, so a CANCEL_ATTEMPT directive or a lease
	// revocation can tear down THIS attempt without touching its siblings.
	taskCtx, cancel := context.WithCancel(ctx)
	rt := &runningTask{cancel: cancel, policy: a.GetSpec().GetCancellation(), done: make(chan struct{})}

	r.mu.Lock()
	r.running[attemptID] = rt
	r.mu.Unlock()

	defer func() {
		cancel()
		close(rt.done)
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
	if _, known := r.executors[kind]; !known {
		// Bounded label: an executor this worker does not run is reported as
		// "unsupported", not by the (task-author-controlled) name it asked for.
		rt.kind = "unsupported"
	} else {
		rt.kind = kind
	}
	rt.started = time.Now()
	r.Metrics.TaskStarted(rt.kind)
	defer r.Metrics.TaskFinished(rt.kind)
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

	// ── Secrets: resolved BEFORE anything is prepared (ADR-0025) ─────────────
	// A task either gets every secret it declared or never starts. Values live
	// in this goroutine's memory and the executor's injection only; they are
	// wiped when the attempt ends.
	secCtx, secSpan := tracing.Start(taskCtx, "attempt.resolve_secrets",
		trace.WithAttributes(attribute.Int("nexus.secrets", len(a.GetSpec().GetRuntime().GetSecrets()))))
	resolved, serr := r.resolveSecrets(secCtx, a)
	// The error text names the reference and the provider, never a value.
	tracing.End(secSpan, serr)
	if serr != nil {
		span.SetStatus(otelcodes.Error, "secrets could not be resolved")
		c := secretFailure(serr)
		log.Error("could not resolve the task's secrets; the workload was not started",
			"error", c.Message, "class", c.Class.String(), "retryable", c.Retryable)
		r.reportFailure(taskCtx, a, c, executor.Outcome{})
		return
	}
	defer secrets.Wipe(resolved)
	rt.redactor = r.buildRedactor(a, resolved, log)

	// Cached artifacts are pinned for the whole attempt, so LRU eviction can
	// never pull a model out from under a running task (ADR-0030 §4).
	artifacts, releaseArtifacts, aerr := r.acquireArtifacts(taskCtx, a, kind, log)
	defer releaseArtifacts()
	if aerr != nil {
		span.SetStatus(otelcodes.Error, "a declared artifact could not be provided")
		c := classifyArtifactError(aerr)
		log.Error("could not provide a declared artifact", "error", aerr, "class", c.Class.String())
		r.reportFailure(taskCtx, a, c, executor.Outcome{})
		return
	}

	inCtx, inSpan := tracing.Start(taskCtx, "attempt.prepare_inputs",
		trace.WithAttributes(attribute.Int("nexus.inputs", len(a.GetInputs()))))
	spec, err := r.buildExecutorSpec(inCtx, a)
	tracing.End(inSpan, err)
	if err != nil {
		span.SetStatus(otelcodes.Error, "inputs could not be prepared")
		c := executor.ClassifyError(err)
		log.Error("could not prepare the task specification", "error", err, "class", c.Class.String())
		r.reportFailure(taskCtx, a, c, executor.Outcome{})
		return
	}
	spec.Secrets = resolved
	if rt.redactor != nil && spec.OnPhase != nil {
		onPhase := spec.OnPhase
		spec.OnPhase = func(phase, detail string) { onPhase(phase, redactString(rt.redactor, detail)) }
	}

	spec.Artifacts = artifacts

	// The workload API exists before Prepare, because the executor bakes the
	// socket into the workload's environment and mounts, and is torn down on
	// every exit path, so nothing can act for this attempt once Run returns.
	api := r.startTaskAPI(taskCtx, a, exec, log)
	if api != nil {
		defer api.Close()
		spec.TaskAPISocket = api.SocketPath()
		spec.TaskAPIToken = api.Token()
	}

	ship := r.newShipper(a, rt.redactor, log)
	if ship != nil {
		spec.LogSink = ship
	}
	runCtx, runSpan := tracing.Start(taskCtx, "attempt.run",
		trace.WithAttributes(tracing.AttrExecutor.String(exec.Kind())))
	// The workload's own spans (TRACEPARENT) are children of attempt.run.
	spec.TraceEnv = tracing.TaskEnv(runCtx)
	outcome, prepErr := r.execute(runCtx, exec, rt, spec, log)
	runSpan.SetAttributes(attribute.Int("nexus.exit_code", int(outcome.ExitCode)))
	tracing.End(runSpan, prepErr)

	// Both are lease-fenced and both must land before the result report,
	// which releases the lease.
	logCtx, logSpan := tracing.Start(ctx, "attempt.finalize_logs")
	facts := r.finishTaskAPI(logCtx, api, rt, log)
	r.finishLogs(logCtx, ship, log)
	logSpan.End()
	if prepErr != nil {
		span.SetStatus(otelcodes.Error, "execution failed")
		c := executor.ClassifyError(prepErr)
		c.Message = redactString(rt.redactor, c.Message)
		log.Error("execution failed before or during the workload",
			"error", redactString(rt.redactor, prepErr.Error()), "class", c.Class.String())
		r.reportFailure(taskCtx, a, c, outcome)
		return
	}

	r.reportOutcome(ctx, a, outcome, log, facts)
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

	var followDone chan struct{}
	if spec.LogSink != nil {
		followDone = r.followIntoSink(ctx, exec, handle, spec.LogSink, log)
	} else {
		r.streamLogs(ctx, exec, handle, rt.redactor, log)
	}

	outcome, err := exec.Wait(ctx, handle)
	if followDone != nil {
		// The follower ends at the workload's EOF; bounded, because a
		// substrate that never closes the stream must not hold the result.
		select {
		case <-followDone:
		case <-time.After(10 * time.Second):
			log.Warn("log follower did not reach EOF after the workload exited; finalizing without it")
		}
	}
	if err != nil {
		return outcome, fmt.Errorf("wait: %w", err)
	}
	return outcome, nil
}

// followIntoSink feeds executors that do not capture into the sink themselves
// (kubernetes) by following Logs(), tagged "combined" because the substrate
// has already interleaved the streams. Returns nil when the executor captures
// on its own.
func (r *Runner) followIntoSink(ctx context.Context, exec executor.Executor, h executor.Handle,
	sink executor.LogSink, log *slog.Logger) chan struct{} {
	if c, ok := exec.(executor.LogCapturer); ok && c.CapturesLogs() {
		return nil
	}
	rc, err := exec.Logs(context.WithoutCancel(ctx), h)
	if err != nil {
		log.Warn("could not open the workload log stream; the attempt log will be empty", "error", err)
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rc.Close()
		_, _ = io.Copy(sink.Writer(executor.LogStreamCombined), rc)
	}()
	return done
}

// streamLogs tails the workload's output into the agent's log, on its own
// goroutine.
//
// Separate goroutine, and a bounded reader: a task that emits gigabytes of logs
// must not be able to block the runner or exhaust the agent's memory. The log
// stream is a diagnostic, not a result — losing the tail of a pathological log is
// strictly better than losing the task.
//
// Read by LINE, not by fixed-size chunk, so the redactor -- which matches per
// line (ADR-0020) -- sees every secret whole rather than split across two
// reads. A line longer than the scanner's cap ends the copy: this is a
// diagnostic, and a truncated one is better than an unredacted one.
func (r *Runner) streamLogs(ctx context.Context, exec executor.Executor, h executor.Handle,
	redactor *logship.Redactor, log *slog.Logger) {
	rc, err := exec.Logs(ctx, h)
	if err != nil {
		log.Warn("could not open the workload log stream", "error", err)
		return
	}
	go func() {
		defer rc.Close()
		const maxLogBytes = 16 << 20 // 16 MiB per attempt
		sc := bufio.NewScanner(io.LimitReader(rc, maxLogBytes))
		sc.Buffer(make([]byte, 32<<10), 256<<10)
		for sc.Scan() {
			line, _ := redactor.Apply(sc.Text())
			log.Info("task log", "output", line)
		}
		if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			log.Debug("log stream ended", "error", err)
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
		AttemptID:      a.GetAttemptId(),
		RunID:          a.GetRunId(),
		NodeID:         a.GetNodeId(),
		IdempotencyKey: a.GetIdempotencyKey(),
		AttemptNumber:  a.GetAttemptNumber(),
		Runtime:        spec.GetRuntime(),
		Limit:          effectiveLimit(spec.GetResources()),
		// The devices the registry named at admission, so concurrent GPU
		// attempts on this worker never share one.
		GPUDevices:           r.registry.GPUDevices(a.GetAttemptId()),
		GPU:                  r.registry.GPUAssignment(a.GetAttemptId()),
		Inputs:               inputs,
		DeclaredOutputs:      declaredOutputNames(spec),
		StdoutOutput:         spec.GetRuntime().GetStdoutOutput(),
		InlineThresholdBytes: domain.ClampInlineThreshold(r.InlineThreshold),
		StartToCloseDeadline: deadline,
		GraceAfterTerm:       executor.DefaultGraceAfterTerm,
		WorkerID:             r.WorkerID,
		WorkerGPUKind:        r.GPUKind,
		WorkerGPUCount:       r.GPUCount,
		OnPhase:              r.phaseReporter(ctx, a),
	}, nil
}

// phaseReporter turns an executor's substrate-phase changes (a Kubernetes pod
// going Pending → Running, or stuck on "Unschedulable: insufficient cpu") into
// task-level ReportProgress calls.
//
// That makes the TASK heartbeat reflect whether the workload actually exists
// and is progressing through its lifecycle, which is ADR-0007's mitigation for
// nested scheduling. Only changes are reported, so a pod stuck Pending does
// NOT keep refreshing the heartbeat: the task-level heartbeat timeout still
// catches an attempt the cluster never starts.
//
// Delivery is on one goroutine per attempt through a small buffer: the
// executor's wait loop must never block on an RPC, and a single sender keeps
// "Pending" from overtaking "Running" on the wire. A full buffer drops the
// update — a progress note is advisory, and a lost one is corrected by the next.
func (r *Runner) phaseReporter(ctx context.Context, a *nexusv1.TaskAssignment) func(phase, detail string) {
	if r.reporter == nil {
		return nil
	}
	ch := make(chan string, 8)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				r.reporter.ReportProgress(rctx, a.GetAttemptId(), r.currentToken(a), msg, 0)
				cancel()
			}
		}
	}()
	return func(phase, detail string) {
		msg := "pod " + phase
		if detail != "" {
			msg += ": " + detail
		}
		select {
		case ch <- msg:
		default:
		}
	}
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
// through the reporter (which owns the WAL ordering). facts carries what the
// workload API learned (a declared failure, an agent-initiated cancel); the
// zero value classifies from the Outcome alone.
func (r *Runner) reportOutcome(ctx context.Context, a *nexusv1.TaskAssignment,
	o executor.Outcome, log *slog.Logger, facts attemptFacts) {

	// Detached context: the result MUST be reported even when the parent context
	// is being cancelled by shutdown. A completed task whose report we abandoned
	// because we were shutting down is precisely the waste the WAL exists to
	// prevent, and reporting now is cheaper than replaying at next boot.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	reportCtx, span := tracing.Start(reportCtx, "attempt.report_result")
	defer span.End()

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
		outCtx, outSpan := tracing.Start(reportCtx, "attempt.store_outputs")
		refs, err := r.storeOutputs(outCtx, a, o, log)
		outSpan.SetAttributes(attribute.Int("nexus.outputs", len(refs)))
		tracing.End(outSpan, err)
		switch {
		case errors.Is(err, executor.ErrOutputTooLarge):
			// The workload broke its own declared bound: terminal VALIDATION,
			// and nothing was uploaded (checkOutputBounds runs first).
			log.Error("a task output exceeds its declared max_bytes; failing the attempt", "error", err)
			req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
			req.Error = executor.ClassifyError(err).ToProto()
		case err != nil:
			// We cannot persist an output, so we cannot honestly claim success: a
			// downstream task would read a reference to nothing. INFRASTRUCTURE +
			// retryable is the truthful report.
			log.Error("could not store the task outputs; reporting INFRASTRUCTURE failure", "error", err)
			req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
			req.Error = executor.ClassifyError(fmt.Errorf("%w: %v", executor.ErrResultStore, err)).ToProto()
		case len(refs) > 0:
			req.Outputs = refs
		}
	case o.TimedOut:
		req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_TIMED_OUT
		req.Error = executor.ClassifiedFromOutcome(o, "").ToProto()
	default:
		req.Outcome = nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_FAILED
		req.Error = executor.ClassifiedFromOutcome(o, "").ToProto()
		if te, ok := declaredTaskError(o, facts); ok {
			log.Info("classifying the failure as the workload declared it",
				"class", te.GetClass().String(), "type", te.GetType(), "retry_after_ms", te.GetRetryAfterMs(),
				"observed_class", req.GetError().GetClass().String())
			req.Error = te
		}
	}

	// An announced eviction overrides whatever the workload declared: the
	// node going away is why the attempt ended (ADR-0030 §2).
	if req.Outcome != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED && r.preempted(a.GetAttemptId()) {
		asPreempted(req)
	}
	if req.Error != nil {
		// A kubernetes failure message can carry the pod's log tail
		// (FallbackToLogsOnError); it must not carry a secret into the
		// control plane's attempt history.
		req.Error.Message = r.redactAttempt(a.GetAttemptId(), req.Error.GetMessage())
	}
	entry := wal.Entry{
		AttemptID: a.GetAttemptId(), LeaseToken: token,
		RunID: a.GetRunId(), NodeID: a.GetNodeId(),
	}
	span.SetAttributes(tracing.AttrOutcome.String(req.GetOutcome().String()))
	if req.GetOutcome() != nexusv1.AttemptOutcome_ATTEMPT_OUTCOME_SUCCEEDED {
		span.SetStatus(otelcodes.Error, req.GetError().GetType())
	}
	err := r.reporter.ReportResult(reportCtx, req, entry)
	r.observeReport(a.GetAttemptId(), req, err)
	if err != nil {
		span.RecordError(err)
	}
	if err != nil {
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

	// Every bound is checked before ANY output is written, so an attempt that
	// breaks one leaves no object behind for the orphan sweep.
	if err := checkOutputBounds(a.GetSpec(), produced); err != nil {
		return nil, err
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

// checkOutputBounds enforces each output's declared ValueSpec.max_bytes
// against the bytes that would be stored: the inline payload, or the blob the
// store would upload (F125).
//
// Here and not in an executor: every executor's outputs converge on
// storeOutputs, so a container that writes its own files, a non-Python
// handler and a Python handler not started through the SDK entrypoint are all
// held to the bound the gateway's submit-time fan-in proof (dag/validate.go)
// assumed. Names are checked in sorted order so the error is deterministic
// when several outputs are oversized. 0 means undeclared, and an output the
// spec does not declare has no bound.
func checkOutputBounds(spec *nexusv1.TaskSpec, produced map[string]executor.NamedOutput) error {
	decl := spec.GetOutputs()
	if len(decl) == 0 {
		return nil
	}
	names := make([]string, 0, len(produced))
	for name := range produced {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		limit := decl[name].GetMaxBytes()
		if size := int64(len(produced[name].Bytes)); limit > 0 && size > limit {
			return fmt.Errorf("%w: output %q is %d bytes, over its declared max_bytes=%d",
				executor.ErrOutputTooLarge, name, size, limit)
		}
	}
	return nil
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
		//
		// The CONFIGURED threshold, not the compiled-in default: an operator
		// who lowered it must not have a store-less worker quietly inline past
		// it (ADR-0016).
		if int64(len(out.Bytes)) > domain.ClampInlineThreshold(r.InlineThreshold) {
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
	c.Message = r.redactAttempt(a.GetAttemptId(), c.Message)
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
	if r.preempted(a.GetAttemptId()) {
		asPreempted(req)
	}
	entry := wal.Entry{AttemptID: a.GetAttemptId(), LeaseToken: token,
		RunID: a.GetRunId(), NodeID: a.GetNodeId()}
	err := r.reporter.ReportResult(reportCtx, req, entry)
	r.observeReport(a.GetAttemptId(), req, err)
	if err != nil {
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
	if rt.policy.GetUninterruptible() {
		r.cancelUninterruptible(rt, log)
		return
	}
	log.Warn("cancelling attempt", "grace", grace)
	rt.cancelRequested.Store(true)

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

// cancelUninterruptible is Cancel for a task that declared itself
// uninterruptible (ADR-0015): no SIGTERM at all, a bounded wait for the task to
// finish on its own, then SIGKILL.
//
// No SIGTERM because the declaration says the task cannot be safely asked to
// stop part way; a TERM it cannot honour would only invite a half-finished side
// effect. SIGKILL at the bound because the platform's promise to reclaim a
// worker outranks the task's wish to finish -- that is what "bounded" means.
//
// The control plane's grace_ms is deliberately ignored here: it is one number
// for every attempt on the heartbeat, and the task's own declaration, clamped by
// this agent's ceiling, is the more specific statement.
func (r *Runner) cancelUninterruptible(rt *runningTask, log *slog.Logger) {
	r.mu.Lock()
	if rt.deferring {
		r.mu.Unlock()
		// A repeat (next heartbeat's CANCEL_ATTEMPT, a lease revocation). The
		// first cancellation's clock is the one that counts.
		return
	}
	rt.deferring = true
	r.mu.Unlock()

	bound := EffectiveUninterruptibleBound(rt.policy, r.UninterruptibleCeiling)
	log.Warn("cancellation deferred: task declared uninterruptible; waiting for it to finish, then SIGKILL",
		"bound", bound, "declared_ms", rt.policy.GetMaxUninterruptibleMs(),
		"ceiling", r.UninterruptibleCeiling)

	go func() {
		rt.handleMu.Lock()
		started := rt.handle != nil
		rt.handleMu.Unlock()
		if !started {
			// Nothing is executing yet, so there is no critical section to
			// protect: abandon Prepare/Start now rather than wait out the bound.
			rt.cancel()
			return
		}

		t := time.NewTimer(bound)
		defer t.Stop()
		select {
		case <-rt.done:
			log.Info("uninterruptible task finished within its cancellation bound")
			return
		case <-t.C:
		}

		rt.handleMu.Lock()
		h, exec := rt.handle, rt.exec
		rt.handleMu.Unlock()
		log.Warn("uninterruptible cancellation bound expired; SIGKILL", "bound", bound)
		rt.cancelRequested.Store(true)
		if h != nil && exec != nil {
			if err := exec.Signal(context.Background(), *h, executor.SignalKill); err != nil {
				log.Warn("SIGKILL delivery failed", "error", err)
			}
		}
		rt.cancel()
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

// observeReport records a result report in the task metrics. The attempt's
// executor and start time come from its running entry; on the panic path the
// entry is already gone, and the report is counted without a duration.
func (r *Runner) observeReport(attemptID string, req *nexusv1.ReportResultRequest, err error) {
	if r.Metrics == nil {
		return
	}
	kind, started := "unknown", time.Time{}
	r.mu.Lock()
	if rt, ok := r.running[attemptID]; ok {
		kind, started = rt.kind, rt.started
	}
	r.mu.Unlock()
	r.Metrics.TaskReported(kind, req.GetOutcome(), req.GetError().GetClass(), started, err)
}
