// Package grpcapi is the workflow-manager's gRPC surface plus its clients for the
// peer services it must call.
//
// The workflow-manager owns workflow_specs, workflow_runs, and timers (ADR-0011).
// It owns NEITHER task_nodes/task_edges (dag-engine) NOR task_attempts/leases
// (scheduler) NOR run_events (event-service), so materializing a DAG, cancelling
// attempts, and recording history are all calls, not writes. This file is where
// those boundaries live.
package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrPeerNotConfigured means the owning service's address was not supplied.
//
// Loud rather than a silent no-op: a no-op would let a run be created with no nodes
// (so it could never progress) or an operator's cancel to be accepted and dropped.
// Both would look like success and fail invisibly.
var ErrPeerNotConfigured = errors.New("peer service is not configured")

// peerTimeout bounds every call out of this service. ADR-0011 names this as a cost
// of the decomposition: calls that were function calls are now network hops, and
// every one needs a timeout.
const peerTimeout = 5 * time.Second

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, peerTimeout)
}

// DAGEngineClient calls dag-engine, which owns task_nodes and task_edges.
type DAGEngineClient struct {
	addr string
	log  *slog.Logger
	// conn is non-nil once an address is configured. dag-engine speaks a JSON
	// codec over a hand-written descriptor, so the wire types and the codec live
	// in dagengine_grpc.go rather than coming from generated stubs.
	conn *deConn
}

// NewDAGEngineClient creates the client.
func NewDAGEngineClient(addr string, log *slog.Logger) *DAGEngineClient {
	c := &DAGEngineClient{addr: addr, log: log}
	if addr == "" {
		log.Warn("dag-engine address is unset; run creation will fail rather than create a run with no nodes",
			"remedy", "set NEXUS_DAG_ENGINE_ADDR")
		return c
	}
	conn, err := dialDAGEngine(addr, log)
	if err != nil {
		// A dial failure here is a configuration error, not a peer outage
		// (grpc.NewClient does not connect eagerly), so it is logged and the
		// client degrades to the not-configured path rather than killing a
		// process that could still serve health and lifecycle reads.
		log.Error("cannot construct dag-engine client", "addr", addr, "err", err)
		return c
	}
	c.conn = conn
	return c
}

// Close releases the connection.
func (c *DAGEngineClient) Close() error {
	if c.conn != nil {
		return c.conn.close()
	}
	return nil
}

// MaterializeRun compiles the pinned spec into nodes and edges.
//
// This must SUCCEED for a run to be usable: a run whose DAG was never materialized
// has no frontier, so it would sit in PENDING forever with no error anywhere. That
// is why the failure propagates out of run creation rather than being logged and
// swallowed.
func (c *DAGEngineClient) MaterializeRun(ctx context.Context, runID, specID string, specJSON []byte) error {
	if c.conn == nil {
		return fmt.Errorf("materialize run %s: %w (dag-engine owns task_nodes)", runID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	// specID and specJSON are deliberately NOT sent. The engine re-reads the run's
	// pinned spec from workflow_specs itself, so sending a copy would create a
	// second source of truth for what a run executes — and if the two ever
	// disagreed, the run would be materialized from bytes no one can reproduce
	// from the run's own record (ADR-0004).
	var resp deMaterializeRunResponse
	if err := c.conn.invoke(ctx, "/nexus.v1.DagEngineService/MaterializeRun",
		&deMaterializeRunRequest{RunID: runID}, &resp); err != nil {
		return fmt.Errorf("materialize run %s: %w", runID, err)
	}
	c.log.InfoContext(ctx, "run materialized",
		"run_id", runID, "nodes", resp.NodesInserted, "edges", resp.EdgesInserted,
		"frontier", resp.Frontier, "already_existed", resp.AlreadyExisted)
	return nil
}

// ReconcileRun asks the engine to recompute a run's frontier after an attempt
// reached a terminal state.
//
// This is what turns one finished task into the next one starting. Without it a
// run's first node succeeds and the run then sits forever: nothing else in the
// system computes the frontier, because only the engine owns the graph.
func (c *DAGEngineClient) ReconcileRun(ctx context.Context, runID, nodeID string) (frontier []string, runState string, terminal bool, err error) {
	if c.conn == nil {
		return nil, "", false, fmt.Errorf("reconcile run %s: %w (dag-engine owns task_nodes)", runID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()

	var resp deReconcileRunResponse
	if err := c.conn.invoke(ctx, "/nexus.v1.DagEngineService/ReconcileRun",
		&deReconcileRunRequest{RunID: runID, NodeID: nodeID}, &resp); err != nil {
		return nil, "", false, fmt.Errorf("reconcile run %s: %w", runID, err)
	}
	return resp.Frontier, resp.RunState, resp.RunTerminal, nil
}

// ResetFailedNodes prepares a retry-from-failed.
//
// Nodes that already SUCCEEDED are deliberately left alone, so a 50-node pipeline
// that failed at node 49 does not redo 48 nodes of work.
func (c *DAGEngineClient) ResetFailedNodes(ctx context.Context, runID string) ([]string, error) {
	if c.addr == "" {
		return nil, fmt.Errorf("reset failed nodes for %s: %w (dag-engine owns task_nodes)", runID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_ = ctx
	return nil, fmt.Errorf("reset failed nodes for %s: %w", runID, errNotWired("dag-engine", "ResetFailedNodes"))
}

// SchedulerClient calls scheduler, which owns task_attempts, leases, and
// concurrency_slots.
type SchedulerClient struct {
	addr string
	log  *slog.Logger
}

// NewSchedulerClient creates the client.
func NewSchedulerClient(addr string, log *slog.Logger) *SchedulerClient {
	if addr == "" {
		log.Warn("scheduler address is unset; attempt-scoped timers and run cancellation cannot take effect",
			"remedy", "set NEXUS_SCHEDULER_ADDR")
	}
	return &SchedulerClient{addr: addr, log: log}
}

// CancelRunAttempts stops every in-flight attempt of a run.
//
// Called by cancel and by the run deadline, and NEVER by pause: pause must leave
// in-flight work running to completion (§5.2), and calling this from the pause path
// would silently convert every pause into a cancel and throw away hours of compute.
func (c *SchedulerClient) CancelRunAttempts(ctx context.Context, runID string) error {
	if c.addr == "" {
		return fmt.Errorf("cancel attempts of run %s: %w (scheduler owns task_attempts)", runID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_ = ctx
	return fmt.Errorf("cancel attempts of run %s: %w", runID, errNotWired("scheduler", "CancelRunAttempts"))
}

// The attempt-scoped timer effects. The reconciler lives in workflow-manager
// because `timers` is its table, but each effect mutates task_attempts or leases,
// which the scheduler owns — so the timer FIRES here and is APPLIED there.
//
// Each is a distinct method rather than one generic "expire" call because the four
// timeouts mean genuinely different things (§4.3): schedule_to_start means the
// cluster lacks capacity, start_to_close means the code ran too long, heartbeat
// means it went silent, and lease_expiry means the WORKER is gone. Collapsing them
// into one call would erase exactly the signal the four-timeout model exists to
// preserve, and the resulting attempt state differs too — TIMED_OUT for a timeout,
// LOST for a lease expiry, and LOST is not FAILED (invariant 2).

// OnScheduleToStart handles an attempt that waited too long for capacity.
func (c *SchedulerClient) OnScheduleToStart(ctx context.Context, attemptID string) error {
	return c.attemptTimeout(ctx, "OnScheduleToStart", attemptID)
}

// OnStartToClose handles a single attempt exceeding its wall-clock budget.
func (c *SchedulerClient) OnStartToClose(ctx context.Context, attemptID string) error {
	return c.attemptTimeout(ctx, "OnStartToClose", attemptID)
}

// OnScheduleToClose handles the end-to-end SLA being blown across ALL retries.
func (c *SchedulerClient) OnScheduleToClose(ctx context.Context, attemptID string) error {
	return c.attemptTimeout(ctx, "OnScheduleToClose", attemptID)
}

// OnHeartbeatTimeout handles a task that went silent while its worker stayed alive.
func (c *SchedulerClient) OnHeartbeatTimeout(ctx context.Context, attemptID string) error {
	return c.attemptTimeout(ctx, "OnHeartbeatTimeout", attemptID)
}

// OnRetryBackoff releases a backed-off retry into PENDING.
func (c *SchedulerClient) OnRetryBackoff(ctx context.Context, attemptID string) error {
	return c.attemptTimeout(ctx, "OnRetryBackoff", attemptID)
}

// OnLeaseExpiry handles a lease that was not renewed: the attempt becomes LOST, not
// FAILED, because we do not know whether it ran (invariant 2).
func (c *SchedulerClient) OnLeaseExpiry(ctx context.Context, attemptID string) error {
	return c.attemptTimeout(ctx, "OnLeaseExpiry", attemptID)
}

func (c *SchedulerClient) attemptTimeout(ctx context.Context, op, attemptID string) error {
	if c.addr == "" {
		return fmt.Errorf("%s for %s: %w (scheduler owns task_attempts)", op, attemptID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_ = ctx
	return fmt.Errorf("%s for %s: %w", op, attemptID, errNotWired("scheduler", op))
}

// EventClient calls event-service, which owns the append-only run_events log.
type EventClient struct {
	addr string
	log  *slog.Logger
}

// NewEventClient creates the client.
func NewEventClient(addr string, log *slog.Logger) *EventClient {
	if addr == "" {
		log.Warn("event-service address is unset; run history will not be recorded",
			"remedy", "set NEXUS_EVENT_SERVICE_ADDR")
	}
	return &EventClient{addr: addr, log: log}
}

// Publish appends one event.
//
// The run service logs a failure here and CONTINUES, deliberately: the state change
// is already durably committed, and refusing an operator's cancel because the audit
// log was briefly unavailable would be the wrong trade.
func (c *EventClient) Publish(ctx context.Context, runID string, nodeID, attemptID *string, kind string, payload any, actor string) error {
	if c.addr == "" {
		return fmt.Errorf("publish %s for run %s: %w (event-service owns run_events)", kind, runID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	_ = ctx
	return fmt.Errorf("publish %s for run %s: %w", kind, runID, errNotWired("event-service", "Publish"))
}

// errNotWired marks a method whose peer gRPC service definition does not exist yet.
func errNotWired(service, method string) error {
	return fmt.Errorf("%s.%s is not wired yet; the %s gRPC service definition is still pending", service, method, service)
}
