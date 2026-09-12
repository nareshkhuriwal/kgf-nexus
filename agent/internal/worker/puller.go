// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"log/slog"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// PollClient is the task-acquisition RPC.
type PollClient interface {
	PollTask(ctx context.Context, req *nexusv1.PollTaskRequest) (*nexusv1.PollTaskResponse, error)
}

// Puller acquires work.
//
// PULL, NEVER PUSH (ADR-0005). The control plane never opens a connection to a
// worker, so workers can live behind NAT, in another VPC, or on a laptop; the
// control plane needs no model of worker reachability; and backpressure is
// automatic, because a saturated worker simply stops polling. Phase 1 is
// poll-only — the NATS dispatch notification is deferred — so LONG POLLING
// carries the latency: the server holds the request open until work appears
// instead of making us choose between a tight poll loop and seconds of delay.
type Puller struct {
	client   PollClient
	registry *Registry
	leases   LeaseTracker
	runner   *Runner
	log      *slog.Logger

	workerID string
	queues   []string

	// LongPollWait is how long the server may hold a request open. 20s balances
	// dispatch latency against connection churn and stays inside typical L7
	// idle timeouts.
	LongPollWait time.Duration
	// MaxTasks caps assignments per call. Batching amortises the round-trip when
	// a queue is backed up.
	MaxTasks int32
	// IdleBackoff is the pause when the server has nothing, overridable per-call
	// by poll_again_after_ms so the control plane can shed load.
	IdleBackoff time.Duration
	// ErrorBackoff is the pause after a failed poll, so an unreachable control
	// plane is not hammered by the whole fleet.
	ErrorBackoff time.Duration
}

// NewPuller builds a puller with defaults suited to Phase 1's poll-only path.
func NewPuller(workerID string, queues []string, c PollClient, reg *Registry,
	leases LeaseTracker, runner *Runner, log *slog.Logger) *Puller {
	return &Puller{
		client: c, registry: reg, leases: leases, runner: runner,
		log:          log.With("worker_id", workerID),
		workerID:     workerID,
		queues:       queues,
		LongPollWait: 20 * time.Second,
		MaxTasks:     4,
		IdleBackoff:  time.Second,
		ErrorBackoff: 2 * time.Second,
	}
}

// Run polls until ctx is cancelled or the worker is draining and idle.
func (p *Puller) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		// A draining worker stops polling entirely. This is the DRAINING state
		// from §5.5: no new tasks, finish current, then DEAD.
		if p.registry.Draining() {
			p.log.Info("puller stopping: worker is draining")
			return nil
		}

		wait := p.pollOnce(ctx)
		if wait <= 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// pollOnce performs one long poll and dispatches whatever comes back. It returns
// how long to wait before polling again.
func (p *Puller) pollOnce(ctx context.Context) time.Duration {
	free := p.registry.FreeCapacity()

	// A worker with nothing free must not long-poll: it would hold a connection
	// open for 20s only to reject whatever it was handed, and the control plane
	// would have reserved capacity against it in the meantime.
	if p.registry.Count() > 0 && isExhausted(free) {
		return p.IdleBackoff
	}

	// The RPC outlives the long-poll window by a margin so the SERVER, not the
	// client, decides when an empty poll ends. A client timeout shorter than the
	// server's wait would silently convert every long poll into a failed one.
	rpcCtx, cancel := context.WithTimeout(ctx, p.LongPollWait+10*time.Second)
	resp, err := p.client.PollTask(rpcCtx, &nexusv1.PollTaskRequest{
		WorkerId:     p.workerID,
		FreeCapacity: free,
		Queues:       p.queues,
		WaitMs:       int32(p.LongPollWait / time.Millisecond),
		MaxTasks:     p.MaxTasks,
	})
	cancel()

	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		p.log.Warn("PollTask failed", "error", err)
		return p.ErrorBackoff
	}

	assignments := resp.GetAssignments()
	if len(assignments) == 0 {
		if ms := resp.GetPollAgainAfterMs(); ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
		// A long poll that returned empty already consumed its wait; poll again
		// immediately rather than adding latency on top of the server's window.
		return 0
	}

	for _, a := range assignments {
		p.dispatch(ctx, a)
	}
	// Work is flowing: poll again at once rather than sleeping through a backlog.
	return 0
}

// dispatch runs the LOCAL admission check and starts the attempt.
//
// Both sides verify capacity on purpose. The control plane's ledger can be
// momentarily stale — a completion it has not processed, a reconciliation in
// flight — and a worker that accepts more than it can hold OOMs the node, taking
// every sibling attempt down with it. The local re-check catches exactly that
// staleness window.
func (p *Puller) dispatch(ctx context.Context, a *nexusv1.TaskAssignment) {
	attemptID := a.GetAttemptId()
	log := p.log.With("attempt_id", attemptID, "run_id", a.GetRunId(), "node_id", a.GetNodeId())

	request := a.GetSpec().GetResources().GetRequest()
	if ok, reason := p.registry.Admit(attemptID, request); !ok {
		// Rejecting means NOT tracking the lease: letting it expire is how the
		// control plane learns the placement failed. Reporting a failure instead
		// would consume one of the task's retry attempts for a scheduling
		// mismatch that is not the task's fault.
		log.Warn("rejecting assignment: local admission failed", "reason", reason)
		return
	}

	// Track the lease BEFORE execution starts. An assignment we hold but do not
	// renew expires while we are still working on it, and the control plane
	// re-dispatches work we are actively running.
	p.leases.Track(attemptID, a.GetLeaseToken(),
		time.UnixMilli(a.GetLeaseExpiresAtUnixMs()))

	log.Info("attempt admitted", "executor", a.GetSpec().GetRuntime().GetExecutor(),
		"attempt_number", a.GetAttemptNumber())

	// One goroutine per attempt. Execution must never block the puller, and it
	// must certainly never block the heartbeat.
	go p.runner.Run(ctx, a)
}

// isExhausted reports whether a vector has no room left on any dimension it
// tracks. Used only as a cheap short-circuit; the authoritative check is
// Registry.Admit, which compares against the actual request.
func isExhausted(free *nexusv1.ResourceVector) bool {
	return free.GetCpuMillis() <= 0 && free.GetMemoryBytes() <= 0 && free.GetGpuCount() <= 0
}
