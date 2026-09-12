// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
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
	// conn is non-nil once an address is configured. The scheduler serves its
	// attempt-report half over a hand-written JSON codec (the frozen proto
	// declares no such RPCs), so the wire types mirror
	// services/scheduler/reports_grpc.go rather than coming from generated stubs.
	conn *schedConn
}

// NewSchedulerClient creates the client.
func NewSchedulerClient(addr string, log *slog.Logger) *SchedulerClient {
	c := &SchedulerClient{addr: addr, log: log}
	if addr == "" {
		log.Warn("scheduler address is unset; attempt-scoped timers and run cancellation cannot take effect",
			"remedy", "set NEXUS_SCHEDULER_ADDR")
		return c
	}
	conn, err := dialScheduler(addr, log)
	if err != nil {
		log.Error("failed to dial scheduler; run cancellation will fail loudly rather than silently drop",
			"addr", addr, "error", err)
		return c
	}
	c.conn = conn
	return c
}

// CancelRunAttempts stops every in-flight attempt of a run.
//
// Called by cancel and by the run deadline, and NEVER by pause: pause must leave
// in-flight work running to completion (§5.2), and calling this from the pause path
// would silently convert every pause into a cancel and throw away hours of compute.
func (c *SchedulerClient) CancelRunAttempts(ctx context.Context, runID string) error {
	if c.conn == nil {
		return fmt.Errorf("cancel attempts of run %s: %w (scheduler owns task_attempts)", runID, ErrPeerNotConfigured)
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var resp schedCancelRunAttemptsResponse
	if err := c.conn.invoke(ctx, "/nexus.v1.AttemptReportService/CancelRunAttempts",
		&schedCancelRunAttemptsRequest{RunID: runID}, &resp); err != nil {
		return fmt.Errorf("cancel attempts of run %s: %w", runID, err)
	}
	c.log.DebugContext(ctx, "cancelled in-flight attempts of a run",
		"run_id", runID, "cancelled", resp.Cancelled)
	return nil
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
//
// HTTP, not gRPC, because that is what the service speaks: event-service serves a
// plain JSON REST surface (POST /v1/runs/{runID}/events) and registers no gRPC
// service at all. The earlier stub here reported that an "event-service gRPC
// service definition is still pending", which sent everyone looking for a proto
// that was never the plan.
type EventClient struct {
	addr string
	log  *slog.Logger
	http *http.Client
}

// NewEventClient creates the client.
func NewEventClient(addr string, log *slog.Logger) *EventClient {
	if addr == "" {
		log.Warn("event-service address is unset; run history will not be recorded",
			"remedy", "set NEXUS_EVENT_SERVICE_ADDR")
	}
	return &EventClient{
		addr: eventServiceHTTPAddr(addr),
		log:  log,
		// One client, reused: a fresh http.Client per call leaks a connection pool
		// and defeats keep-alive on a path that fires on every run transition.
		http: &http.Client{Timeout: peerTimeout},
	}
}

// eventBody mirrors event-service's Event for the fields a publisher supplies.
//
// run_id is omitted because the handler takes it from the path and overwrites
// whatever the body carried; seq, id and occurred_at are assigned by the owner,
// and seq in particular MUST be (it is allocated from workflow_runs.event_seq, so
// a client-chosen value would race — see CONTRACT.md's EVENT SEQ rule).
type eventBody struct {
	NodeID    *string `json:"node_id,omitempty"`
	AttemptID *string `json:"attempt_id,omitempty"`
	Kind      string  `json:"kind"`
	Payload   any     `json:"payload,omitempty"`
	Actor     string  `json:"actor"`
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

	body, err := json.Marshal(eventBody{
		NodeID: nodeID, AttemptID: attemptID, Kind: kind, Payload: payload, Actor: actor,
	})
	if err != nil {
		return fmt.Errorf("encode %s event for run %s: %w", kind, runID, err)
	}

	url := fmt.Sprintf("http://%s/v1/runs/%s/events", c.addr, runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build publish request for run %s: %w", runID, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("publish %s for run %s: %w", kind, runID, err)
	}
	defer func() {
		// Drain before close so the connection returns to the pool rather than
		// being torn down after every event.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode == http.StatusCreated {
		return nil
	}
	// The body carries the owner's reason; 4xx here means this publisher sent
	// something wrong and retrying it will not help, so it is reported as-is
	// rather than dressed up as an outage.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("publish %s for run %s: event-service returned %s: %s",
		kind, runID, resp.Status, bytes.TrimSpace(detail))
}

// eventServiceHTTPAddr adapts a configured address to event-service's HTTP port.
//
// Every service is handed peers as host:9090 because that is the gRPC convention
// across the fleet, but event-service is the one peer that serves HTTP and listens
// on 8080 only — so the configured address pointed at a port with nothing behind
// it and every publish failed with "connection refused". Rewriting the well-known
// wrong port here keeps the existing deployment config working; an address given
// with any other port is respected as-is.
func eventServiceHTTPAddr(addr string) string {
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// No port at all: event-service's own default is what it listens on.
		return net.JoinHostPort(addr, "8080")
	}
	if port == "9090" {
		return net.JoinHostPort(host, "8080")
	}
	return addr
}

// errNotWired marks a method whose peer gRPC service definition does not exist yet.
func errNotWired(service, method string) error {
	return fmt.Errorf("%s.%s is not wired yet; the %s gRPC service definition is still pending", service, method, service)
}
