// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"google.golang.org/protobuf/encoding/protojson"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// GRPCServer exposes the scheduler's two worker-facing operations over
// nexus.v1.SchedulerService.
//
// It is a thin adapter, on purpose. The §5.3 atomic claim lives in
// internal/store and is shared byte-for-byte with the background dispatch loop;
// this type translates protobuf to store types and back and does no scheduling
// decision of its own. A second implementation of the claim behind an RPC is how
// a no-double-dispatch guarantee quietly stops holding.
type GRPCServer struct {
	nexusv1.UnimplementedSchedulerServiceServer

	store *store.SchedulerStore
	log   *slog.Logger
	// timeNow is injectable so tests can pin control-plane time. Production
	// always uses the real clock: deadlines are computed from CONTROL-PLANE time
	// (invariant 7) and nothing a worker sends may influence it.
	timeNow func() time.Time

	// metrics is optional; nil records nothing.
	metrics *Metrics
}

// WithMetrics attaches the service instruments and returns g.
func (g *GRPCServer) WithMetrics(m *Metrics) *GRPCServer {
	g.metrics = m
	return g
}

// NewGRPCServer builds the adapter.
func NewGRPCServer(st *store.SchedulerStore, log *slog.Logger) *GRPCServer {
	if log == nil {
		log = slog.Default()
	}
	return &GRPCServer{store: st, log: log, timeNow: func() time.Time { return time.Now().UTC() }}
}

// Register wires the service onto a gRPC server.
func Register(srv grpc.ServiceRegistrar, impl *GRPCServer) {
	nexusv1.RegisterSchedulerServiceServer(srv, impl)
}

// ClaimForWorker runs the atomic claim for one polling worker.
//
// The worker_id is taken as GIVEN. The gateway has already authenticated the
// worker's session and substituted the verified id, which is precisely why this
// service is internal and not exposed through the gateway: a worker able to call
// it directly could name a DIFFERENT worker and claim that worker's capacity.
//
// An empty response is returned when nothing fits, never an error. "No work
// available" is the steady state of a healthy fleet — the agents in this system
// poll continuously — and reporting it as an error would make every idle poll
// look like a fault in the logs and drive the agent's failure backoff.
func (g *GRPCServer) ClaimForWorker(
	ctx context.Context, req *nexusv1.ClaimForWorkerRequest,
) (*nexusv1.ClaimForWorkerResponse, error) {
	workerID, err := uuid.Parse(req.GetWorkerId())
	if err != nil {
		// A malformed id IS an error, unlike an empty result: it means the caller
		// is broken rather than the cluster being busy.
		return nil, status.Errorf(codes.InvalidArgument, "worker_id %q is not a uuid", req.GetWorkerId())
	}

	now := g.timeNow()
	claimStart := time.Now()
	assignments, err := g.store.ClaimForWorker(ctx, now, store.WorkerClaimRequest{
		WorkerID:     workerID,
		Queues:       req.GetQueues(),
		FreeCapacity: req.GetFreeCapacity(),
		MaxTasks:     req.GetMaxTasks(),
	})
	outcome := "empty"
	switch {
	case err != nil:
		outcome = "error"
	case len(assignments) > 0:
		outcome = "dispatched"
	}
	g.metrics.observeClaim("poll", outcome, time.Since(claimStart), len(assignments))
	if err != nil {
		// Assignments committed before the error are still returned: each is a
		// real lease on a real attempt, and discarding them here would strand
		// that work until its lease expired and it was retried as LOST.
		g.log.ErrorContext(ctx, "claim for worker failed",
			"worker_id", workerID, "partial_assignments", len(assignments), "err", err)
		if len(assignments) == 0 {
			return nil, status.Errorf(codes.Internal, "claim for worker: %v", err)
		}
	}

	resp := &nexusv1.ClaimForWorkerResponse{}
	for i := range assignments {
		resp.Assignments = append(resp.Assignments, toTaskAssignment(&assignments[i]))
	}
	for _, a := range assignments {
		g.log.InfoContext(ctx, "attempt claimed by polling worker",
			"run_id", a.RunID, "attempt_id", a.AttemptID, "node_id", a.NodeID,
			"worker_id", workerID, "attempt_number", a.AttemptNumber,
			"lease_expires_at", a.LeaseExpiresAt)
	}
	return resp, nil
}

// PendingCancellations reports the attempts this worker should stop.
func (g *GRPCServer) PendingCancellations(
	ctx context.Context, req *nexusv1.PendingCancellationsRequest,
) (*nexusv1.PendingCancellationsResponse, error) {
	workerID, err := uuid.Parse(req.GetWorkerId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "worker_id %q is not a uuid", req.GetWorkerId())
	}

	ids, err := g.store.PendingCancellations(ctx, workerID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "pending cancellations: %v", err)
	}

	out := &nexusv1.PendingCancellationsResponse{
		// The grace period is the scheduler's lease TTL rather than a separate
		// knob: a worker given longer than its lease to shut down would be
		// finishing work whose lease had already expired and been reclaimed,
		// which is the duplicate execution the fencing exists to prevent.
		GraceMs: int32(g.store.Config().LeaseTTL.Milliseconds()),
	}
	for _, id := range ids {
		out.AttemptIds = append(out.AttemptIds, id.String())
	}
	return out, nil
}

// toTaskAssignment renders one claimed attempt onto the wire.
//
// The TaskSpec is decoded from task_nodes.spec_json with protojson rather than
// being rebuilt field by field: the DAG engine owns that column's shape, and a
// hand-written translation here would silently drop any field the scheduler had
// not been taught about — which for a runtime or timeout field means the worker
// runs the task differently than the author wrote it.
func toTaskAssignment(a *store.WorkerAssignment) *nexusv1.TaskAssignment {
	out := &nexusv1.TaskAssignment{
		AttemptId:     a.AttemptID.String(),
		RunId:         a.RunID.String(),
		NodeId:        a.NodeID,
		AttemptNumber: a.AttemptNumber,
		// Read from the attempt row, where it was derived WITHOUT attempt_number
		// (invariant 1). Every retry of this logical work carries the same key.
		IdempotencyKey: a.IdempotencyKey,
		LeaseToken:     a.LeaseToken,
		// Absolute instants from control-plane time (invariant 7).
		LeaseExpiresAtUnixMs: a.LeaseExpiresAt.UnixMilli(),
	}
	if !a.StartToCloseDeadline.IsZero() {
		out.StartToCloseDeadlineUnixMs = a.StartToCloseDeadline.UnixMilli()
	}
	// The reservation as committed, so the agent uses the shared GPU device
	// the ledger charged (ADR-0029 §2).
	out.Reserved = a.Reserved

	if len(a.SpecJSON) > 0 {
		spec := &nexusv1.TaskSpec{}
		// DiscardUnknown: an additive spec change by the DAG engine must not make
		// every dispatch fail (ADR-0011 rule 5). A spec we cannot parse at all
		// still ships with the ids and the lease token, so the worker can fail
		// the attempt with a real error instead of the control plane wedging.
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(a.SpecJSON, spec); err == nil {
			out.Spec = spec
		}
	}

	if len(a.InputRefsJSON) > 0 {
		// input_refs is a map of input name to ResultRef, written by the DAG
		// engine when it resolved the node's upstreams. Decoded leniently for the
		// same reason as the spec.
		var refs map[string]json.RawMessage
		if err := json.Unmarshal(a.InputRefsJSON, &refs); err == nil && len(refs) > 0 {
			out.Inputs = map[string]*nexusv1.ResultRef{}
			for name, raw := range refs {
				ref := &nexusv1.ResultRef{}
				if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, ref); err == nil {
					out.Inputs[name] = ref
				}
			}
		}
	}
	return out
}
