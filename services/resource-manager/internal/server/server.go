// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package server implements the Resource Manager's gRPC API: the capacity
// ledger over workers.capacity (advertised at registration) and
// workers.committed (the sum of active reservations).
//
// The ledger's hardest problem is not arithmetic, it is HONESTY. Distributed
// accounting drifts: a worker restarts and forgets its work, a release RPC is
// lost, a dispatch is rolled back after the reservation was written. The only
// defence is periodic reconciliation against ground truth, and the only party
// holding ground truth is the worker itself, in the running_attempt_ids it
// reports every heartbeat. That is why reconciliation lives on the heartbeat path
// here and not in a nightly job.
package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// DefaultReconcileGrace is how long a reservation is protected from being
// treated as a leak.
//
// It must exceed the worst-case gap between "the scheduler committed the
// reservation" and "the worker acknowledges the attempt in a heartbeat": one
// poll interval plus one heartbeat interval plus slack. Too short and
// reconciliation races dispatch and cancels work the instant it is scheduled —
// the leak detector becomes a leak generator. Too long and real leaks linger.
// 60s comfortably covers a 20s long poll plus a 5s beat.
const DefaultReconcileGrace = 60 * time.Second

// Server is the gRPC service.
type Server struct {
	rmv1.UnimplementedResourceManagerServiceServer

	store *store.RMStore
	log   *slog.Logger

	// ReconcileGrace is the default applied when a caller supplies none.
	ReconcileGrace time.Duration

	// Metrics is optional; nil records nothing.
	Metrics *Metrics
}

// New builds the service over a store.
func New(s *store.RMStore, log *slog.Logger) *Server {
	return &Server{store: s, log: log, ReconcileGrace: DefaultReconcileGrace}
}

// RegisterWorker creates a per-session worker row.
func (s *Server) RegisterWorker(ctx context.Context, req *rmv1.RegisterWorkerRequest) (*rmv1.RegisterWorkerResponse, error) {
	if req.GetHostname() == "" {
		return nil, status.Error(codes.InvalidArgument, "hostname is required")
	}
	if req.GetNamespace() == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace is required")
	}
	if req.GetCapacity() == nil {
		return nil, status.Error(codes.InvalidArgument, "capacity is required: the ledger is built from it")
	}
	queues := req.GetQueues()
	if len(queues) == 0 {
		queues = []string{"default"}
	}

	id, err := s.store.RegisterWorker(ctx, store.Worker{
		Namespace:    req.GetNamespace(),
		Hostname:     req.GetHostname(),
		Queues:       queues,
		Executors:    req.GetExecutors(),
		Labels:       req.GetLabels(),
		Capacity:     req.GetCapacity(),
		AgentVersion: req.GetAgentVersion(),
	}, req.GetSessionTokenHash())
	if err != nil {
		s.log.Error("worker registration failed", "hostname", req.GetHostname(), "error", err)
		return nil, status.Errorf(codes.Internal, "register worker: %v", err)
	}
	s.log.Info("worker registered", "worker_id", id,
		"namespace", req.GetNamespace(), "hostname", req.GetHostname(),
		"queues", queues, "executors", req.GetExecutors(),
		"agent_version", req.GetAgentVersion())
	// A new session from a host with an open quarantine inherits it (see
	// store.InheritHostQuarantine). Best-effort: failing registration over it
	// would take a healthy host out of service on a bookkeeping error, and the
	// detector re-quarantines a genuinely poisoned one within a window.
	if inherited, qerr := s.store.InheritHostQuarantine(ctx, id); qerr != nil {
		s.log.Error("could not check host quarantine for a new worker session",
			"worker_id", id, "hostname", req.GetHostname(), "error", qerr)
	} else if inherited {
		s.log.Warn("new worker session inherited its host's quarantine",
			"worker_id", id, "hostname", req.GetHostname())
	}
	return &rmv1.RegisterWorkerResponse{WorkerId: id}, nil
}

// HeartbeatWorker records liveness and reconciles the ledger against the
// worker's ground truth in one call.
//
// One call, because the heartbeat is the ONLY moment at which the control plane
// holds ground truth about what a worker is running. Reconciling anywhere else
// means reconciling against the ledger's own guess, which can only ever confirm
// what it already believes.
func (s *Server) HeartbeatWorker(ctx context.Context, req *rmv1.HeartbeatWorkerRequest) (*rmv1.HeartbeatWorkerResponse, error) {
	workerID := req.GetWorkerId()
	if workerID == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	log := s.log.With("worker_id", workerID)

	if err := s.store.TouchHeartbeat(ctx, workerID); err != nil {
		if errors.Is(err, store.ErrWorkerNotFound) {
			// Not an error to the caller: the correct response is a REREGISTER
			// directive, and the worker gateway turns worker_known=false into one.
			// Returning NOT_FOUND would make a routine post-sweep heartbeat look
			// like a fault in every dashboard.
			log.Warn("heartbeat from an unknown or dead worker; re-registration required")
			s.Metrics.heartbeat("unknown_worker")
			return &rmv1.HeartbeatWorkerResponse{WorkerKnown: false}, nil
		}
		s.Metrics.heartbeat("error")
		log.Error("could not record heartbeat", "error", err)
		return nil, status.Errorf(codes.Internal, "touch heartbeat: %v", err)
	}

	if req.GetCachedArtifactsReported() {
		// Placement input only (ADR-0030 §4): a failure here must not fail
		// the heartbeat, which is liveness.
		if err := s.store.RecordCachedArtifacts(ctx, workerID, req.GetCachedArtifacts()); err != nil {
			log.Warn("could not record cached artifacts; cache affinity uses the previous set", "error", err)
		}
	}

	grace := time.Duration(req.GetReconcileGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = s.ReconcileGrace
	}

	leaked, err := s.store.ReconcileWorker(ctx, workerID, req.GetRunningAttemptIds(), grace)
	if err != nil {
		// Liveness already succeeded, so do NOT fail the call: a bookkeeping error
		// must not make a live worker look dead. A dead-looking worker gets its
		// leases expired and its work duplicated, which is far worse than a
		// reservation staying leaked for one more beat — and the next beat retries.
		log.Error("reconciliation failed; the worker is alive and the ledger will be retried next beat", "error", err)
		s.Metrics.heartbeat("ok")
		return &rmv1.HeartbeatWorkerResponse{WorkerKnown: true}, nil
	}
	s.Metrics.heartbeat("ok")
	s.Metrics.leak("heartbeat", len(leaked))

	if len(leaked) > 0 {
		freed := &nexusv1.ResourceVector{}
		for _, r := range leaked {
			freed = domain.AddVec(freed, r.Resources)
			log.Warn("released a leaked reservation: the worker does not acknowledge this attempt",
				"attempt_id", r.AttemptID, "reservation_id", r.ID)
		}
		log.Warn("reclaimed leaked capacity", "reservations", len(leaked),
			"cpu_millis", freed.GetCpuMillis(), "memory_bytes", freed.GetMemoryBytes(),
			"gpu_count", freed.GetGpuCount())
	}

	resp := &rmv1.HeartbeatWorkerResponse{
		WorkerKnown: true,
		Released:    toProtoReservations(leaked),
	}
	if w, err := s.store.GetWorker(ctx, workerID); err == nil {
		resp.Committed = w.Committed
	}
	return resp, nil
}

// Reserve commits capacity outside the dispatch transaction.
func (s *Server) Reserve(ctx context.Context, req *rmv1.ReserveRequest) (*rmv1.ReserveResponse, error) {
	if req.GetWorkerId() == "" || req.GetAttemptId() == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id and attempt_id are required")
	}
	log := s.log.With("worker_id", req.GetWorkerId(), "attempt_id", req.GetAttemptId())

	start := time.Now()
	res, err := s.store.Reserve(ctx, req.GetWorkerId(), req.GetAttemptId(), req.GetRequest())
	if err != nil {
		var insufficient *store.ErrInsufficientCapacity
		if errors.As(err, &insufficient) {
			s.Metrics.reserve("refused", start)
			// A refusal is a normal scheduling outcome, not a fault. Returning it
			// as granted=false with the shortfall lets the caller record an
			// operator-legible pending_reason instead of retrying blindly.
			reason := domain.ShortfallReason(insufficient.Shortfall)
			log.Info("reservation refused", "reason", reason)
			resp := &rmv1.ReserveResponse{Granted: false, ShortfallReason: reason}
			if w, err := s.store.GetWorker(ctx, req.GetWorkerId()); err == nil {
				resp.Free = domain.SubVec(w.Capacity, w.Committed)
			}
			return resp, nil
		}
		if errors.Is(err, store.ErrWorkerNotFound) {
			s.Metrics.reserve("not_found", start)
			return nil, status.Errorf(codes.NotFound, "worker %s not found", req.GetWorkerId())
		}
		s.Metrics.reserve("error", start)
		log.Error("reservation failed", "error", err)
		return nil, status.Errorf(codes.Internal, "reserve: %v", err)
	}

	s.Metrics.reserve("granted", start)
	log.Info("capacity reserved", "reservation_id", res.ID,
		"cpu_millis", res.Resources.GetCpuMillis(),
		"memory_bytes", res.Resources.GetMemoryBytes(),
		"gpu_count", res.Resources.GetGpuCount())

	resp := &rmv1.ReserveResponse{Granted: true, Reservation: toProtoReservation(res)}
	if w, err := s.store.GetWorker(ctx, req.GetWorkerId()); err == nil {
		resp.Free = domain.SubVec(w.Capacity, w.Committed)
	}
	return resp, nil
}

// Release frees a reservation. Releasing an absent reservation is SUCCESS with
// released=false, not an error: this is the main non-dispatch mutation path and
// it is driven by at-least-once completion reports, so a duplicate call is
// expected traffic rather than a fault.
func (s *Server) Release(ctx context.Context, req *rmv1.ReleaseRequest) (*rmv1.ReleaseResponse, error) {
	if req.GetAttemptId() == "" {
		return nil, status.Error(codes.InvalidArgument, "attempt_id is required")
	}
	res, err := s.store.Release(ctx, req.GetAttemptId())
	if errors.Is(err, store.ErrReservationNotFound) {
		s.log.Debug("release found nothing to release (already released)", "attempt_id", req.GetAttemptId())
		s.Metrics.release("absent")
		return &rmv1.ReleaseResponse{Released: false}, nil
	}
	if err != nil {
		s.Metrics.release("error")
		s.log.Error("release failed", "attempt_id", req.GetAttemptId(), "error", err)
		return nil, status.Errorf(codes.Internal, "release: %v", err)
	}
	s.Metrics.release("released")
	s.log.Info("capacity released", "worker_id", res.WorkerID, "attempt_id", res.AttemptID)
	return &rmv1.ReleaseResponse{Released: true, Reservation: toProtoReservation(res)}, nil
}

// Reconcile is the explicit form of the heartbeat's reconciliation, for the
// periodic sweep and for operator repair.
func (s *Server) Reconcile(ctx context.Context, req *rmv1.ReconcileRequest) (*rmv1.ReconcileResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	grace := time.Duration(req.GetGraceMs()) * time.Millisecond
	if grace <= 0 {
		grace = s.ReconcileGrace
	}
	leaked, err := s.store.ReconcileWorker(ctx, req.GetWorkerId(), req.GetRunningAttemptIds(), grace)
	if err != nil {
		if errors.Is(err, store.ErrWorkerNotFound) {
			return nil, status.Errorf(codes.NotFound, "worker %s not found", req.GetWorkerId())
		}
		return nil, status.Errorf(codes.Internal, "reconcile: %v", err)
	}
	s.Metrics.leak("reconcile", len(leaked))
	resp := &rmv1.ReconcileResponse{Released: toProtoReservations(leaked)}
	if w, err := s.store.GetWorker(ctx, req.GetWorkerId()); err == nil {
		resp.Committed = w.Committed
	}
	return resp, nil
}

// ReleaseAllForWorker frees a dead worker's entire committed vector (§5.5).
func (s *Server) ReleaseAllForWorker(ctx context.Context, req *rmv1.ReleaseAllForWorkerRequest) (*rmv1.ReconcileResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	leaked, err := s.store.ReleaseAllForWorker(ctx, req.GetWorkerId())
	if err != nil {
		if errors.Is(err, store.ErrWorkerNotFound) {
			return nil, status.Errorf(codes.NotFound, "worker %s not found", req.GetWorkerId())
		}
		return nil, status.Errorf(codes.Internal, "release all: %v", err)
	}
	s.Metrics.leak("dead_worker", len(leaked))
	s.log.Warn("released all reservations for a worker", "worker_id", req.GetWorkerId(), "reservations", len(leaked))
	return &rmv1.ReconcileResponse{Released: toProtoReservations(leaked)}, nil
}

// SetWorkerState transitions a worker through the §5.5 lifecycle.
//
// A transition to DEAD also releases every reservation the worker held, in that
// order: state first, then capacity. If capacity were released first, a dispatch
// racing us could place new work on a worker we are about to declare dead, and
// that work would be LOST immediately.
func (s *Server) SetWorkerState(ctx context.Context, req *rmv1.SetWorkerStateRequest) (*rmv1.SetWorkerStateResponse, error) {
	workerID := req.GetWorkerId()
	if workerID == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}
	target, err := fromProtoState(req.GetState())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	log := s.log.With("worker_id", workerID, "target_state", string(target), "reason", req.GetReason())

	// Quarantine and release go through the episode-recording paths, so every
	// entry to and exit from QUARANTINED is attributable and has a reason.
	if target == store.WorkerQuarantined || target == store.WorkerActive {
		return s.setQuarantine(ctx, req, target, log)
	}

	applied, err := s.store.SetWorkerState(ctx, workerID, target)
	if err != nil {
		log.Error("worker state transition failed", "error", err)
		return nil, status.Errorf(codes.Internal, "set worker state: %v", err)
	}
	if !applied {
		// A losing race with a concurrent sweep is normal. Report the current
		// state rather than an error so the caller can decide, rather than retry.
		log.Info("worker state transition not applied: illegal from the current state")
	}

	resp := &rmv1.SetWorkerStateResponse{Applied: applied}
	if w, err := s.store.GetWorker(ctx, workerID); err == nil {
		resp.Current = toProtoState(w.State)
	}

	if applied && target == store.WorkerDead {
		leaked, err := s.store.ReleaseAllForWorker(ctx, workerID)
		if err != nil {
			// The worker IS dead and that fact is recorded. Leaving its capacity
			// committed is a leak the heartbeat reconciler cannot fix (a dead
			// worker sends no heartbeats), so this is a genuine error worth
			// surfacing — but the state change stands.
			log.Error("worker marked DEAD but its reservations could not be released; capacity is leaked until the sweep repairs it", "error", err)
		} else {
			resp.Released = toProtoReservations(leaked)
			log.Warn("worker DEAD: all reservations released, capacity returned to the pool",
				"reservations", len(leaked))
		}
	}
	return resp, nil
}

// setQuarantine implements manual quarantine (QUARANTINED) and release
// (ACTIVE from QUARANTINED).
func (s *Server) setQuarantine(
	ctx context.Context, req *rmv1.SetWorkerStateRequest, target store.WorkerState, log *slog.Logger,
) (*rmv1.SetWorkerStateResponse, error) {
	actor := req.GetActor()
	if actor == "" {
		actor = "unknown"
	}
	reason := req.GetReason()
	var applied bool
	var err error
	if target == store.WorkerQuarantined {
		if reason == "" {
			return nil, status.Error(codes.InvalidArgument, "a quarantine needs a reason")
		}
		if req.GetQuarantineMs() < 0 {
			return nil, status.Error(codes.InvalidArgument, "quarantine_ms must not be negative")
		}
		var expires *time.Time
		if ms := req.GetQuarantineMs(); ms > 0 {
			t := time.Now().Add(time.Duration(ms) * time.Millisecond)
			expires = &t
		}
		applied, err = s.store.QuarantineWorker(ctx, req.GetWorkerId(), store.QuarantineManual, reason, actor, expires, nil)
	} else {
		if reason == "" {
			reason = "released by operator"
		}
		applied, err = s.store.ReleaseQuarantine(ctx, req.GetWorkerId(), actor, reason)
	}
	if err != nil {
		log.Error("quarantine transition failed", "error", err)
		return nil, status.Errorf(codes.Internal, "set worker state: %v", err)
	}
	if applied {
		log.Warn("worker quarantine changed by operator", "actor", actor)
	} else {
		log.Info("quarantine transition not applied: illegal from the current state")
	}
	resp := &rmv1.SetWorkerStateResponse{Applied: applied}
	w, gerr := s.store.GetWorker(ctx, req.GetWorkerId())
	if errors.Is(gerr, store.ErrWorkerNotFound) {
		return nil, status.Errorf(codes.NotFound, "worker %s not found", req.GetWorkerId())
	}
	if gerr == nil {
		resp.Current = toProtoState(w.State)
	}
	return resp, nil
}

// GetWorker returns one worker's ledger row.
func (s *Server) GetWorker(ctx context.Context, req *rmv1.GetWorkerRequest) (*rmv1.GetWorkerResponse, error) {
	w, err := s.store.GetWorker(ctx, req.GetWorkerId())
	if errors.Is(err, store.ErrWorkerNotFound) {
		return nil, status.Errorf(codes.NotFound, "worker %s not found", req.GetWorkerId())
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get worker: %v", err)
	}
	return &rmv1.GetWorkerResponse{Worker: toProtoWorker(w)}, nil
}

// ListWorkers returns live workers for a queue, for the scheduler's placement
// scan.
func (s *Server) ListWorkers(ctx context.Context, req *rmv1.ListWorkersRequest) (*rmv1.ListWorkersResponse, error) {
	queue := req.GetQueue()
	if queue == "" {
		queue = "default"
	}
	ws, err := s.store.ListWorkersForQueue(ctx, req.GetNamespace(), queue)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list workers: %v", err)
	}
	out := make([]*rmv1.WorkerInfo, 0, len(ws))
	for _, w := range ws {
		out = append(out, toProtoWorker(w))
	}
	return &rmv1.ListWorkersResponse{Workers: out}, nil
}

func toProtoWorker(w *store.Worker) *rmv1.WorkerInfo {
	return &rmv1.WorkerInfo{
		Id:                    w.ID,
		Namespace:             w.Namespace,
		Hostname:              w.Hostname,
		Queues:                w.Queues,
		Executors:             w.Executors,
		Labels:                w.Labels,
		Capacity:              w.Capacity,
		Committed:             w.Committed,
		Free:                  domain.SubVec(w.Capacity, w.Committed),
		State:                 toProtoState(w.State),
		AgentVersion:          w.AgentVersion,
		RegisteredAtUnixMs:    w.RegisteredAt.UnixMilli(),
		LastHeartbeatAtUnixMs: w.LastHeartbeat.UnixMilli(),
	}
}

func toProtoReservation(r *store.Reservation) *rmv1.Reservation {
	if r == nil {
		return nil
	}
	return &rmv1.Reservation{
		Id: r.ID, WorkerId: r.WorkerID, AttemptId: r.AttemptID,
		Resources: r.Resources, CreatedAtUnixMs: r.CreatedAt.UnixMilli(),
	}
}

func toProtoReservations(rs []*store.Reservation) []*rmv1.Reservation {
	out := make([]*rmv1.Reservation, 0, len(rs))
	for _, r := range rs {
		out = append(out, toProtoReservation(r))
	}
	return out
}

func toProtoState(s store.WorkerState) rmv1.WorkerState {
	switch s {
	case store.WorkerActive:
		return rmv1.WorkerState_WORKER_STATE_ACTIVE
	case store.WorkerDraining:
		return rmv1.WorkerState_WORKER_STATE_DRAINING
	case store.WorkerQuarantined:
		return rmv1.WorkerState_WORKER_STATE_QUARANTINED
	case store.WorkerDead:
		return rmv1.WorkerState_WORKER_STATE_DEAD
	default:
		return rmv1.WorkerState_WORKER_STATE_UNSPECIFIED
	}
}

func fromProtoState(s rmv1.WorkerState) (store.WorkerState, error) {
	switch s {
	case rmv1.WorkerState_WORKER_STATE_ACTIVE:
		return store.WorkerActive, nil
	case rmv1.WorkerState_WORKER_STATE_DRAINING:
		return store.WorkerDraining, nil
	case rmv1.WorkerState_WORKER_STATE_QUARANTINED:
		return store.WorkerQuarantined, nil
	case rmv1.WorkerState_WORKER_STATE_DEAD:
		return store.WorkerDead, nil
	default:
		return "", errors.New("state must be one of ACTIVE, DRAINING, QUARANTINED, DEAD")
	}
}
