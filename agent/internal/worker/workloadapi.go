// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/khuriwalgroup/kgf-nexus/agent/internal/executor"
	"github.com/khuriwalgroup/kgf-nexus/agent/internal/taskapi"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// attemptFacts is what the agent learned about an attempt, beyond the
// executor's Outcome, that bears on how its failure is classified.
type attemptFacts struct {
	// Declared is the failure the workload declared through the workload API,
	// or nil.
	Declared *taskapi.DeclaredError
	// CancelledByAgent is set when the agent itself signalled the workload to
	// stop (a CANCEL_ATTEMPT directive, a revoked lease). A workload dying of
	// our signal did not fail for the reason it may have declared earlier.
	CancelledByAgent bool
}

// declaredTaskError decides whether a workload's declared failure replaces
// the agent's own classification of a failed attempt, and renders it.
//
// The declaration is honoured only when the agent observed nothing more
// specific (ADR-0032 §3):
//
//   - the workload exited NON-ZERO on its own (a zero exit is a success, and
//     an executor error means the workload may never have run);
//   - it was not OOM-killed and did not time out -- those are the agent's
//     observations, and they drive different retry and sizing decisions;
//   - the agent did not signal it to stop.
//
// The declarable classes are already restricted by taskapi (no WORKER_LOST,
// OOM, TIMEOUT or INFRASTRUCTURE), so a workload can describe its own failure
// but never impersonate the platform's.
func declaredTaskError(o executor.Outcome, f attemptFacts) (*nexusv1.TaskError, bool) {
	if f.Declared == nil || f.CancelledByAgent {
		return nil, false
	}
	if o.Err != nil || o.ExitCode == 0 || o.OOMKilled || o.TimedOut {
		return nil, false
	}
	d := *f.Declared
	typ := d.Type
	if typ == "" {
		typ = "WorkloadDeclared"
	}
	class := strings.ToLower(strings.TrimPrefix(d.Class.String(), "ERROR_CLASS_"))
	msg := d.Message
	if msg == "" {
		msg = fmt.Sprintf("the workload declared a %s failure", class)
	}
	msg = fmt.Sprintf("%s (exit code %d)", msg, o.ExitCode)
	return &nexusv1.TaskError{
		Class:        d.Class,
		Type:         typ,
		Message:      msg,
		Retryable:    d.Retryable(),
		RetryAfterMs: d.RetryAfterMs,
	}, true
}

// startTaskAPI serves the attempt's workload API, or returns nil when the
// attempt gets none: a kubernetes attempt on an agent with no network
// listener, an unusable socket root, or a socket that could not be created.
//
// A kubernetes pod runs on another node and cannot reach a socket here, so
// its API is served on the agent's TLS listener instead (TaskAPINetwork,
// F148), routed by the attempt's token.
//
// A failure here degrades rather than fails the attempt. The SDK keeps usage,
// permits and checkpoints in-process without a workload API, so the task still
// runs correctly; what is lost is the usage record and the shared limit, which
// is why it is logged at ERROR.
func (r *Runner) startTaskAPI(ctx context.Context, a *nexusv1.TaskAssignment,
	exec executor.Executor, log *slog.Logger) *taskapi.Server {
	_, remote := exec.(*executor.KubernetesExecutor)
	switch {
	case remote && r.TaskAPINetwork == nil:
		return nil
	case !remote && r.TaskAPIDisabled:
		return nil
	}
	var network *taskapi.Network
	if remote {
		network = r.TaskAPINetwork
	}
	var store taskapi.ObjectStore
	switch {
	case r.CheckpointStore != nil:
		store = r.CheckpointStore
	case r.store != nil:
		// Assigned only when non-nil: a nil *resultstore.Client in the
		// interface would read as "a store is configured".
		store = r.store
	}
	api, err := taskapi.Start(taskapi.Config{
		AttemptID:      a.GetAttemptId(),
		IdempotencyKey: a.GetIdempotencyKey(),
		Namespace:      r.Namespace,
		Root:           r.TaskAPIRoot,
		Network:        network,
		Workload:       r.WorkloadAPI,
		Upstream:       r.WorkloadUpstream,
		Store:          store,
		LeaseToken:     func() string { return r.currentToken(a) },
		Progress:       r.progressForwarder(ctx, a),
		Metrics:        r.Metrics,
		Log:            log,
	})
	if err != nil {
		log.Error("could not start the attempt's workload API; the task runs without it "+
			"(usage units it reports are not recorded)", "error", err)
		return nil
	}
	return api
}

// finishTaskAPI closes the attempt's workload API and delivers its final usage
// snapshot, returning what the workload declared. Called after the workload
// has exited and BEFORE the result is reported: the control plane records
// usage in the terminal transaction, so a snapshot that arrives after the
// result is refused as stale (ADR-0032 §2). Closing first means no increment
// can arrive after that snapshot. Bounded by the flush budget, so a control
// plane that is unreachable delays the report, never blocks it.
func (r *Runner) finishTaskAPI(ctx context.Context, api *taskapi.Server, rt *runningTask,
	log *slog.Logger) attemptFacts {
	f := attemptFacts{CancelledByAgent: rt.cancelRequested.Load()}
	if api == nil {
		return f
	}
	api.Close()
	if err := api.FinalFlush(context.WithoutCancel(ctx)); err != nil {
		log.Error("the attempt's final usage snapshot was not delivered; its usage units are missing from the ledger",
			"error", err, "keys", len(api.UsageTotals()))
	}
	if d, ok := api.Declared(); ok {
		f.Declared = &d
	}
	return f
}

// progressForwarder turns workload progress notes into ReportProgress calls,
// on one goroutine per attempt, LATEST-WINS.
//
// The HTTP handler must never wait on an RPC, and a burst of notes must not
// queue behind a slow control plane: only the newest pending note matters, so
// a newer one replaces an unsent older one. The goroutine ends with the
// attempt's context.
func (r *Runner) progressForwarder(ctx context.Context, a *nexusv1.TaskAssignment) func(string, float64) {
	if r.reporter == nil {
		return nil
	}
	type note struct {
		msg  string
		frac float64
	}
	var (
		mu      sync.Mutex
		pending *note
		wake    = make(chan struct{}, 1)
	)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			mu.Lock()
			n := pending
			pending = nil
			mu.Unlock()
			if n != nil {
				r.reporter.ReportProgress(ctx, a.GetAttemptId(), r.currentToken(a), n.msg, n.frac)
			}
		}
	}()
	return func(msg string, frac float64) {
		mu.Lock()
		pending = &note{msg: msg, frac: frac}
		mu.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}
