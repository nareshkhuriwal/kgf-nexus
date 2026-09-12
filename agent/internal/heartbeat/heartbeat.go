// Package heartbeat proves worker liveness and carries control-plane
// directives back to the agent.
//
// THE CENTRAL CONSTRAINT: this loop runs on its OWN goroutine and must never
// block on task execution. If a wedged task could delay a beat, the control
// plane would exceed the liveness window, declare the worker DEAD, expire its
// leases, mark its attempts LOST, and dispatch them elsewhere — DUPLICATING work
// that is in fact still running. Every design decision in this file follows from
// that: state is read through a snapshot interface rather than by taking the
// executor's locks, each RPC gets a bounded timeout, and a failed beat is logged
// and retried rather than propagated.
package heartbeat

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
)

// DefaultInterval is the beat period. 5s against a 30s liveness window gives
// six consecutive missed beats before the control plane acts, which tolerates a
// short network blip without tolerating a genuinely dead worker for long.
const DefaultInterval = 5 * time.Second

// RPCTimeout bounds one beat. It MUST be well under the interval: a beat that
// could block for longer than the period would let one slow control-plane
// response stall the next beat and manufacture the false-death scenario this
// package exists to prevent.
const RPCTimeout = 3 * time.Second

// Client is the heartbeat RPC.
type Client interface {
	Heartbeat(ctx context.Context, req *nexusv1.HeartbeatRequest) (*nexusv1.HeartbeatResponse, error)
}

// StateSource provides the fleet's ground truth for one beat.
//
// It returns a SNAPSHOT, taken under the task registry's own short-lived lock,
// rather than giving the heartbeat access to live task state. That indirection is
// deliberate: if the heartbeat had to acquire a lock an executor holds while
// running a task, a wedged task would once again be able to block the beat.
type StateSource interface {
	// RunningAttemptIDs is the GROUND TRUTH the control plane reconciles its
	// reservations against. Anything it holds for this worker that is not in
	// this list is a leak and gets released (§3.2). Reporting it accurately
	// every beat is therefore what keeps the capacity ledger honest.
	RunningAttemptIDs() []string
	// FreeCapacity is advertised capacity minus what this worker has actually
	// committed locally.
	FreeCapacity() *nexusv1.ResourceVector
	// Utilization is observed load, used for placement hints and dashboards.
	Utilization() (cpu, mem float64)
}

// DirectiveHandler acts on control-plane commands. Implementations MUST return
// promptly: a handler that blocks blocks the next beat. Anything slow (draining
// a task, SIGKILLing after a grace period) must be started asynchronously.
type DirectiveHandler interface {
	// OnDrain stops accepting new work, lets current work finish, then exits.
	OnDrain(ctx context.Context)
	// OnCancelAttempt tears an attempt down with SIGTERM, grace, then SIGKILL.
	OnCancelAttempt(ctx context.Context, attemptID string, grace time.Duration)
	// OnReregister signals that this session is invalid and a new registration
	// (and therefore a NEW worker_id) is required. The agent can never reclaim
	// its old leases: identity is per-session, not per-host (§5.5).
	OnReregister(ctx context.Context)
}

// Loop is the heartbeat goroutine.
type Loop struct {
	client   Client
	state    StateSource
	handler  DirectiveHandler
	log      *slog.Logger
	workerID string

	// interval is atomic because the control plane changes it via
	// next_heartbeat_ms on any beat, from the RPC goroutine, while the ticker
	// loop reads it.
	interval atomic.Int64

	// consecutiveFailures drives a warn-then-error escalation so an operator can
	// tell a blip from a partition.
	consecutiveFailures atomic.Int64
	lastSuccess         atomic.Int64
}

// New builds a heartbeat loop. initialInterval comes from
// RegisterResponse.heartbeat_interval_ms; the server owns the cadence so it can
// shed load by slowing the fleet rather than being overwhelmed by it.
func New(workerID string, c Client, s StateSource, h DirectiveHandler, initialInterval time.Duration, log *slog.Logger) *Loop {
	if initialInterval <= 0 {
		initialInterval = DefaultInterval
	}
	l := &Loop{client: c, state: s, handler: h, workerID: workerID,
		log: log.With("worker_id", workerID)}
	l.interval.Store(int64(initialInterval))
	l.lastSuccess.Store(time.Now().UnixMilli())
	return l
}

// Interval reports the current beat period, for tests and logs.
func (l *Loop) Interval() time.Duration { return time.Duration(l.interval.Load()) }

// ConsecutiveFailures reports the current failure streak.
func (l *Loop) ConsecutiveFailures() int { return int(l.consecutiveFailures.Load()) }

// LastSuccess reports when the last beat was acked. The agent uses this to
// decide it has been partitioned long enough that its leases must have expired,
// at which point continuing to run the work is waste.
func (l *Loop) LastSuccess() time.Time {
	return time.UnixMilli(l.lastSuccess.Load())
}

// Run beats until ctx is cancelled. It is intended to be the whole body of a
// dedicated goroutine.
//
// A beat failure is NEVER fatal here. The control plane is entitled to be
// briefly unreachable; tearing down running tasks the moment one beat fails
// would turn a load-balancer restart into a fleet-wide outage. The control plane
// already has the authoritative mechanism for a truly dead worker — the liveness
// window — so the agent's correct behaviour is to keep trying.
func (l *Loop) Run(ctx context.Context) error {
	// Beat immediately: the gap between registration and the first beat is a
	// window in which the control plane has a worker it has never heard from.
	l.beat(ctx)

	t := time.NewTimer(l.Interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.log.Info("heartbeat loop stopping", "reason", ctx.Err())
			return nil
		case <-t.C:
			l.beat(ctx)
			// Re-read the interval every tick: the control plane may have
			// changed it on the beat we just completed.
			t.Reset(l.Interval())
		}
	}
}

// beat sends one heartbeat and applies whatever comes back.
func (l *Loop) beat(ctx context.Context) {
	running := l.state.RunningAttemptIDs()
	cpu, mem := l.state.Utilization()
	req := &nexusv1.HeartbeatRequest{
		WorkerId:          l.workerID,
		RunningAttemptIds: running,
		FreeCapacity:      l.state.FreeCapacity(),
		CpuUtilization:    cpu,
		MemoryUtilization: mem,
	}

	// A per-beat timeout derived from the parent context, so shutdown cancels a
	// beat in flight but a hung control plane cannot outlast one period.
	rpcCtx, cancel := context.WithTimeout(ctx, RPCTimeout)
	resp, err := l.client.Heartbeat(rpcCtx, req)
	cancel()

	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return // shutting down; not a failure worth counting
		}
		n := l.consecutiveFailures.Add(1)
		// Escalate to ERROR only once the streak approaches the liveness
		// window, so a single blip does not page anyone but a real partition
		// does.
		lg := l.log.Warn
		if n >= 3 {
			lg = l.log.Error
		}
		lg("heartbeat failed", "consecutive_failures", n,
			"running_attempts", len(running), "error", err)
		return
	}

	if n := l.consecutiveFailures.Swap(0); n > 0 {
		l.log.Info("heartbeat recovered", "missed_beats", n)
	}
	l.lastSuccess.Store(time.Now().UnixMilli())

	if ms := resp.GetNextHeartbeatMs(); ms > 0 {
		next := time.Duration(ms) * time.Millisecond
		if next != l.Interval() {
			l.log.Info("heartbeat interval changed by control plane", "next_heartbeat_ms", ms)
			l.interval.Store(int64(next))
		}
	}
	l.applyDirectives(ctx, resp.GetDirectives())
}

// applyDirectives dispatches control-plane commands.
//
// Directives ride the heartbeat RESPONSE, which is the trick that keeps the
// design fully pull-based (ADR-0005) while retaining the ability to command the
// fleet: at a 5s beat, worst-case command latency is 5s, which is fine for drain
// and cancel and requires no inbound connectivity to the worker at all.
func (l *Loop) applyDirectives(ctx context.Context, ds []*nexusv1.Directive) {
	for _, d := range ds {
		switch d.GetKind() {
		case nexusv1.DirectiveKind_DIRECTIVE_KIND_DRAIN:
			l.log.Info("received DRAIN directive")
			l.handler.OnDrain(ctx)

		case nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT:
			id := d.GetAttemptId()
			if id == "" {
				l.log.Warn("CANCEL_ATTEMPT directive with no attempt_id; ignoring")
				continue
			}
			grace := time.Duration(d.GetGraceMs()) * time.Millisecond
			l.log.Info("received CANCEL_ATTEMPT directive", "attempt_id", id, "grace_ms", d.GetGraceMs())
			l.handler.OnCancelAttempt(ctx, id, grace)

		case nexusv1.DirectiveKind_DIRECTIVE_KIND_REREGISTER:
			l.log.Warn("received REREGISTER directive; this session is no longer valid")
			l.handler.OnReregister(ctx)

		default:
			// An unknown directive from a newer control plane must be ignored,
			// not fatal: a rolling upgrade adds directives before every agent
			// understands them, and an agent that died on the unknown would turn
			// a routine deploy into a fleet outage.
			l.log.Warn("ignoring unknown heartbeat directive", "kind", d.GetKind().String())
		}
	}
}
