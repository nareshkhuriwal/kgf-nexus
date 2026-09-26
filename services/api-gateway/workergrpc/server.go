// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package workergrpc implements proto/nexus/v1 WorkerService: the plane contract
// between the control plane and every worker agent (architecture §8.1).
//
// Every RPC here is initiated by the WORKER. The control plane never dials a
// worker (ADR-0005), which is what lets workers live behind NAT and makes
// backpressure automatic — a saturated worker simply stops polling. The cost of
// that inversion is that control-plane→worker commands have nowhere to ride, so
// they ride on the Heartbeat RESPONSE as directives.
package workergrpc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// ResourceManager is the resource-manager's API as this service needs it.
//
// Declared as a narrow interface here rather than importing a client: the gateway
// does NOT own `workers` or `reservations` (ADR-0011), so every mutation of them
// must go through the owner. Making that dependency explicit and small keeps the
// ownership rule visible in the type system instead of relying on reviewer
// vigilance.
type ResourceManager interface {
	// RegisterWorker creates the worker row and returns its PER-SESSION id.
	RegisterWorker(ctx context.Context, req *nexusv1.RegisterRequest, sessionTokenHash string) (workerID string, err error)
	// RecordHeartbeat updates liveness and free capacity.
	RecordHeartbeat(ctx context.Context, workerID string, free *nexusv1.ResourceVector, cpuUtil, memUtil float64) error
	// ReleaseReservation returns leaked capacity to the pool. Called during
	// heartbeat reconciliation for every reservation the worker does not
	// acknowledge.
	ReleaseReservation(ctx context.Context, workerID, attemptID, reason string) error
	// WorkerState reports the worker's lifecycle state, which decides whether a
	// DRAIN directive must be issued.
	WorkerState(ctx context.Context, workerID string) (state string, ok bool, err error)
}

// Scheduler is the scheduler's API as this service needs it.
//
// The gateway does not own task_attempts or leases either. PollTask is therefore
// a thin, long-polling adapter in front of the scheduler's atomic claim (§5.3):
// the claim must stay one transaction inside its owner, and splitting it across a
// service boundary would replace a transaction with a saga on the one path where
// a compensation bug means a double-dispatched task.
type Scheduler interface {
	// ClaimForWorker performs the atomic dispatch claim and returns assignments.
	ClaimForWorker(ctx context.Context, workerID string, free *nexusv1.ResourceVector, queues []string, maxTasks int32) ([]*nexusv1.TaskAssignment, error)
	// MarkStarted transitions DISPATCHED -> RUNNING.
	MarkStarted(ctx context.Context, attemptID string, startedAt time.Time) error
	// RecordProgress stores the worker's latest status line and fraction on the
	// attempt and resets the task heartbeat deadline. The lease token is
	// forwarded so the scheduler can fence again inside its write; stale is true
	// when that second fence rejected the report.
	RecordProgress(ctx context.Context, attemptID, leaseToken, message string, fraction float64) (stale bool, err error)
	// ApplyResult applies a terminal outcome.
	ApplyResult(ctx context.Context, req *nexusv1.ReportResultRequest) error
	// PendingCancellations lists attempts this worker must stop, delivered as
	// heartbeat directives.
	PendingCancellations(ctx context.Context, workerID string) ([]string, error)
}

// Config tunes the worker protocol's timing.
type Config struct {
	// HeartbeatInterval is what the server TELLS workers to use. Server-controlled
	// so the control plane can slow a large fleet under load rather than being
	// overwhelmed by it (ADR-0005).
	HeartbeatInterval time.Duration
	// LeaseTTL is the fencing window. 30s with 5s renewal means six missed
	// renewals before expiry: tolerant of brief network blips, while still
	// detecting a dead worker in well under a minute (ADR-0010).
	LeaseTTL time.Duration
	// MaxPollWait caps a long poll. Bounded because an unbounded hold would pin a
	// connection past any load balancer's idle timeout, and the worker would see a
	// confusing transport error instead of a clean empty response.
	MaxPollWait time.Duration
	// PollInterval is how often a held long poll re-checks for work.
	PollInterval time.Duration
	// CancelGrace is the SIGTERM-to-SIGKILL window sent with a CANCEL directive.
	CancelGrace time.Duration
	// ResultStore is handed to workers at registration so they read and write
	// MinIO directly; task payloads never transit the control plane (invariant 6).
	ResultStore *nexusv1.ResultStoreConfig
	// LogShipping is advertised to workers at registration (ADR-0020). Nil or
	// disabled means agents ship no logs -- the right answer for a gateway that
	// cannot READ them back, since objects nobody can serve are pure cost.
	LogShipping *nexusv1.LogShippingConfig
	// AgentPolicy is the admission gate at Register: a minimum agent_version
	// and capabilities every agent must advertise (docs/COMPATIBILITY.md §4.4).
	// The zero value admits every agent.
	AgentPolicy domain.AgentPolicy
}

// DefaultConfig is the tuning from ADR-0010 and §5.5.
func DefaultConfig() Config {
	return Config{
		HeartbeatInterval: 5 * time.Second,
		LeaseTTL:          30 * time.Second,
		MaxPollWait:       30 * time.Second,
		PollInterval:      250 * time.Millisecond,
		CancelGrace:       30 * time.Second,
	}
}

// Server implements nexusv1.WorkerServiceServer.
type Server struct {
	nexusv1.UnimplementedWorkerServiceServer

	db  store.GWDB
	rm  ResourceManager
	sch Scheduler
	cfg Config
	log *slog.Logger
}

// NewServer builds the worker-facing gRPC service.
func NewServer(db store.GWDB, rm ResourceManager, sch Scheduler, cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{db: db, rm: rm, sch: sch, cfg: cfg, log: log}
}

// Register issues a PER-SESSION worker identity.
//
// The returned worker_id is fresh on EVERY call and is never derived from the
// hostname. That is the whole point: a restarted agent gets a new id, so it can
// never present itself as its predecessor and reclaim leases the control plane
// has already expired and retried elsewhere. Keying worker identity on hostname
// is a genuinely nasty class of bug — the restarted agent looks like the same
// worker, resurrects stale leases, and two processes end up believing they own
// the same attempt (§5.5, ADR-0010 point 7).
func (s *Server) Register(ctx context.Context, req *nexusv1.RegisterRequest) (*nexusv1.RegisterResponse, error) {
	p, err := s.requirePrincipal(ctx, req.GetNamespace())
	if err != nil {
		return nil, err
	}
	if req.GetHostname() == "" {
		return nil, status.Error(codes.InvalidArgument, "hostname is required")
	}
	if len(req.GetQueues()) == 0 {
		// A worker that serves no queue can never be dispatched to, so accepting
		// the registration would create a permanently idle row that still counts
		// toward fleet capacity in the UI.
		return nil, status.Error(codes.InvalidArgument, "at least one queue is required")
	}
	if req.GetCapacity() == nil {
		return nil, status.Error(codes.InvalidArgument, "advertised capacity is required")
	}
	// The version and capability gate, before any row is written. An agent
	// that is too old, or lacks a behaviour the operator requires, would run
	// tasks with older semantics without anyone noticing (the N-1 hazard,
	// docs/COMPATIBILITY.md §4.3). FAILED_PRECONDITION because the request is
	// well formed and the agent itself must change; the agent's register
	// backoff treats it as permanent and exits with the reason.
	if err := s.cfg.AgentPolicy.Check(req.GetAgentVersion(), req.GetCapabilities()); err != nil {
		s.log.WarnContext(ctx, "refused an incompatible agent",
			"namespace", req.GetNamespace(), "hostname", req.GetHostname(),
			"agent_version", req.GetAgentVersion(), "capabilities", req.GetCapabilities(),
			"principal", p.Name, "reason", err)
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}

	sessionToken, err := newOpaqueToken()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issue session token: %v", err)
	}

	// Only the HASH is stored. The token is a bearer credential for this session,
	// and a metadata-store compromise must not hand an attacker the ability to
	// impersonate live workers.
	workerID, err := s.rm.RegisterWorker(ctx, req, store.HashAPIKey(sessionToken))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register worker: %v", err)
	}

	s.log.InfoContext(ctx, "worker registered",
		"worker_id", workerID,
		"namespace", req.GetNamespace(),
		"hostname", req.GetHostname(),
		"queues", req.GetQueues(),
		"agent_version", req.GetAgentVersion(),
		"principal", p.Name,
	)

	return &nexusv1.RegisterResponse{
		WorkerId:     workerID,
		SessionToken: sessionToken,
		// Milliseconds, from control-plane configuration. The worker does not get
		// to pick its own heartbeat rate.
		HeartbeatIntervalMs: int32(s.cfg.HeartbeatInterval.Milliseconds()),
		LeaseTtlMs:          int32(s.cfg.LeaseTTL.Milliseconds()),
		ResultStore:         s.cfg.ResultStore,
		LogShipping:         s.cfg.LogShipping,
	}, nil
}

// Heartbeat is liveness, capacity reconciliation, and command delivery in one RPC.
//
// running_attempt_ids is treated as GROUND TRUTH. The control plane compares its
// own reservations against what the worker says it is actually doing and releases
// the difference. This is what keeps the capacity ledger from drifting when a
// worker restarts and forgets its work: without it, the ledger would believe
// capacity is committed forever and that worker's GPUs would never be schedulable
// again. Periodic reconciliation of a ledger against ground truth is the only way
// to keep distributed accounting honest (§3.2).
//
// The reconciliation is deliberately ONE-DIRECTIONAL. We release what the worker
// does not claim, but we do NOT trust the worker's claim to create or extend
// anything: an attempt the worker reports running that we have no lease for is
// logged and then commanded to stop via a CANCEL directive, never adopted.
// Trusting a worker's word to establish ownership would hand a buggy or
// compromised agent the ability to fabricate work.
func (s *Server) Heartbeat(ctx context.Context, req *nexusv1.HeartbeatRequest) (*nexusv1.HeartbeatResponse, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	workerID := req.GetWorkerId()
	if workerID == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	state, exists, err := s.rm.WorkerState(ctx, workerID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read worker state: %v", err)
	}
	// DEAD is checked alongside "row is gone" because the liveness sweeper marks
	// the row DEAD rather than deleting it, and a DEAD row is a session that can
	// never come back: its leases have been expired and its work re-dispatched
	// elsewhere, and the resource-manager's heartbeat update is deliberately
	// scoped to ACTIVE/DRAINING so a resurrected zombie cannot reclaim them.
	//
	// Without this branch a worker that missed its liveness window — which a
	// control-plane restart alone is enough to cause — heartbeats forever against
	// a row that will never be updated, is never told to re-register, and never
	// runs another task. The host looks healthy and is permanently idle, which is
	// the worst shape of failure: invisible.
	if !exists || state == string(domain.WorkerDead) {
		// REREGISTER rather than an error: the worker recovers on its own, and a
		// hard error would leave a healthy host idle until someone restarted it by
		// hand. Re-registration mints a NEW session id, so it cannot inherit the
		// dead session's leases (§5.5).
		s.log.WarnContext(ctx, "heartbeat from a session that is gone or DEAD; ordering re-register",
			"worker_id", workerID, "state", state, "exists", exists)
		return &nexusv1.HeartbeatResponse{
			Directives:      []*nexusv1.Directive{{Kind: nexusv1.DirectiveKind_DIRECTIVE_KIND_REREGISTER}},
			NextHeartbeatMs: int32(s.cfg.HeartbeatInterval.Milliseconds()),
		}, nil
	}

	if err := s.rm.RecordHeartbeat(ctx, workerID, req.GetFreeCapacity(), req.GetCpuUtilization(), req.GetMemoryUtilization()); err != nil {
		return nil, status.Errorf(codes.Internal, "record heartbeat: %v", err)
	}

	directives, err := s.reconcileAndBuildDirectives(ctx, workerID, state, req.GetRunningAttemptIds())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reconcile worker: %v", err)
	}

	return &nexusv1.HeartbeatResponse{
		Directives:      directives,
		NextHeartbeatMs: int32(s.cfg.HeartbeatInterval.Milliseconds()),
	}, nil
}

// reconcileAndBuildDirectives performs the ledger reconciliation and assembles the
// commands this worker must obey before its next heartbeat.
func (s *Server) reconcileAndBuildDirectives(ctx context.Context, workerID, workerState string, acknowledged []string) ([]*nexusv1.Directive, error) {
	ack := make(map[string]bool, len(acknowledged))
	for _, id := range acknowledged {
		ack[id] = true
	}

	reserved, err := store.GatewayReservedAttempts(ctx, s.db, workerID)
	if err != nil {
		return nil, fmt.Errorf("list reservations: %w", err)
	}

	for _, attemptID := range reserved {
		if ack[attemptID] {
			continue
		}
		// A reservation the worker does not acknowledge is a LEAK. Releasing it
		// through the resource-manager's API rather than deleting the row here
		// respects table ownership (ADR-0011) — the gateway is not the owner, and
		// the release has to update workers.committed atomically with the
		// reservation delete, which only the owner can do correctly.
		if err := s.rm.ReleaseReservation(ctx, workerID, attemptID, "heartbeat_reconciliation"); err != nil {
			// One stuck release must not abort the whole heartbeat: the worker
			// still needs its directives, and the next heartbeat retries this.
			s.log.ErrorContext(ctx, "failed to release leaked reservation",
				"worker_id", workerID, "attempt_id", attemptID, "error", err)
			continue
		}
		s.log.WarnContext(ctx, "released leaked reservation: worker does not acknowledge the attempt",
			"worker_id", workerID, "attempt_id", attemptID)
	}

	var directives []*nexusv1.Directive

	// DRAIN rides the heartbeat because the control plane cannot dial the worker.
	// At a 5s heartbeat, worst-case drain latency is 5s, which is entirely
	// adequate for a deploy or a scale-in (ADR-0005).
	// QUARANTINED deliberately gets NO drain directive. The agent treats DRAIN as
	// permanent (it never re-admits work), so draining a quarantined worker would
	// make release meaningless. It needs none: the claim admits only ACTIVE
	// workers, so a quarantined one is simply never handed new work while it
	// finishes what it holds, and resumes the moment it is released.
	if workerState == "DRAINING" {
		directives = append(directives, &nexusv1.Directive{
			Kind:    nexusv1.DirectiveKind_DIRECTIVE_KIND_DRAIN,
			GraceMs: int32(s.cfg.CancelGrace.Milliseconds()),
		})
	}

	cancels, err := s.sch.PendingCancellations(ctx, workerID)
	if err != nil {
		return nil, fmt.Errorf("list pending cancellations: %w", err)
	}
	wanted := make(map[string]bool, len(cancels))
	for _, attemptID := range cancels {
		wanted[attemptID] = true
		// Only tell the worker to cancel what it says it is running. Repeating a
		// cancel for work it has already stopped is harmless but noisy, and noise
		// on this path is read by operators during incidents.
		if ack[attemptID] {
			directives = append(directives, &nexusv1.Directive{
				Kind:      nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT,
				AttemptId: attemptID,
				GraceMs:   int32(s.cfg.CancelGrace.Milliseconds()),
			})
		}
	}

	// The other half of reconciliation: work the worker claims that we have no
	// live lease for. We never adopt it — that would let an agent fabricate
	// ownership — so we order it stopped.
	active, err := store.GatewayWorkerActiveAttempts(ctx, s.db, workerID)
	if err != nil {
		return nil, fmt.Errorf("list active attempts: %w", err)
	}
	known := make(map[string]bool, len(active))
	for _, id := range active {
		known[id] = true
	}
	for _, claimed := range acknowledged {
		if known[claimed] || wanted[claimed] {
			continue
		}
		s.log.WarnContext(ctx, "worker reports an attempt the control plane does not own; ordering cancel",
			"worker_id", workerID, "attempt_id", claimed)
		directives = append(directives, &nexusv1.Directive{
			Kind:      nexusv1.DirectiveKind_DIRECTIVE_KIND_CANCEL_ATTEMPT,
			AttemptId: claimed,
			GraceMs:   int32(s.cfg.CancelGrace.Milliseconds()),
		})
	}

	return directives, nil
}

// PollTask hands out work, holding the request open until work appears.
//
// Long polling is what keeps a pull-based design's latency low without push
// (ADR-0005): the worker's request is parked server-side and returns the instant
// the scheduler has something for it, instead of waiting out a fixed poll
// interval. The loop polls the scheduler rather than waiting on a condition
// variable because the work may be created by ANOTHER replica's transaction, which
// this process has no in-memory signal for.
//
// The hold always ends. It ends on work, on wait_ms elapsing, or on the client
// going away — and an empty response is a normal, correct answer, not an error.
func (s *Server) PollTask(ctx context.Context, req *nexusv1.PollTaskRequest) (*nexusv1.PollTaskResponse, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	workerID := req.GetWorkerId()
	if workerID == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	maxTasks := req.GetMaxTasks()
	if maxTasks <= 0 {
		maxTasks = 1
	}

	wait := time.Duration(req.GetWaitMs()) * time.Millisecond
	if wait > s.cfg.MaxPollWait {
		wait = s.cfg.MaxPollWait
	}

	deadline := time.Now().Add(wait)
	for {
		assignments, err := s.sch.ClaimForWorker(ctx, workerID, req.GetFreeCapacity(), req.GetQueues(), maxTasks)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "claim work: %v", err)
		}
		if len(assignments) > 0 {
			s.log.DebugContext(ctx, "dispatched assignments to worker",
				"worker_id", workerID, "count", len(assignments))
			return &nexusv1.PollTaskResponse{Assignments: assignments}, nil
		}
		if !time.Now().Before(deadline) {
			break
		}

		sleep := s.cfg.PollInterval
		if remaining := time.Until(deadline); remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			// The worker hung up or the deadline passed. Returning empty rather
			// than an error keeps the worker's loop simple: an empty poll is
			// already the common case it handles.
			return &nexusv1.PollTaskResponse{
				PollAgainAfterMs: int32(s.cfg.PollInterval.Milliseconds()),
			}, nil
		case <-time.After(sleep):
		}
	}

	return &nexusv1.PollTaskResponse{
		// Advisory backoff, so an idle fleet does not hammer the gateway.
		PollAgainAfterMs: int32(s.cfg.PollInterval.Milliseconds()),
	}, nil
}

// ReportStarted confirms DISPATCHED -> RUNNING.
//
// The worker's own started_at_unix_ms is accepted but NOT used for any deadline.
// Worker clocks are never authoritative (invariant 7): a worker whose clock is
// ten minutes fast would otherwise appear to have blown its start_to_close budget
// the instant it began. The reported value is recorded as the worker's claim; the
// control plane's own clock sets the deadline.
func (s *Server) ReportStarted(ctx context.Context, req *nexusv1.ReportStartedRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "ReportStarted"); ack != nil {
		return ack, nil
	}
	if err := s.sch.MarkStarted(ctx, req.GetAttemptId(), time.Now().UTC()); err != nil {
		return nil, status.Errorf(codes.Internal, "mark attempt started: %v", err)
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

// ExtendLease renews EVERY lease this worker holds in one RPC.
//
// Batching is the design, not an optimization (ADR-0010): at 10k concurrent tasks
// with 5s renewal, per-attempt renewal is 2k RPS of pure overhead, and building it
// per-attempt first would mean retrofitting later under load.
//
// revoked_attempt_ids is the load-bearing half of the response. An attempt whose
// renewal did not land has had its lease expired and reclaimed, and a REPLACEMENT
// ATTEMPT MAY ALREADY BE RUNNING. The worker must stop: anything it reports later
// will be rejected on token mismatch anyway, and continuing to burn CPU on work
// that can no longer be recorded is pure waste.
func (s *Server) ExtendLease(ctx context.Context, req *nexusv1.ExtendLeaseRequest) (*nexusv1.ExtendLeaseResponse, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	workerID := req.GetWorkerId()
	if workerID == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required")
	}

	renewals := make(map[string]string, len(req.GetRenewals()))
	for _, r := range req.GetRenewals() {
		if r.GetAttemptId() == "" {
			continue
		}
		renewals[r.GetAttemptId()] = r.GetLeaseToken()
	}

	grants, revoked, err := store.GatewayRenewLeases(ctx, s.db, workerID, renewals, s.cfg.LeaseTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "renew leases: %v", err)
	}

	resp := &nexusv1.ExtendLeaseResponse{RevokedAttemptIds: revoked}
	for _, g := range grants {
		resp.Grants = append(resp.Grants, &nexusv1.LeaseGrant{
			AttemptId: g.AttemptID,
			// CONTROL-PLANE absolute time, computed by the database. The worker
			// does not convert a duration against its own clock, so clock skew
			// cannot make it believe it holds a lease the control plane considers
			// expired (invariant 7).
			ExpiresAtUnixMs: g.ExpiresAt.UnixMilli(),
			Epoch:           g.Epoch,
		})
	}
	if len(revoked) > 0 {
		s.log.WarnContext(ctx, "revoked leases on renewal; worker must stop this work",
			"worker_id", workerID, "revoked", revoked)
	}
	return resp, nil
}

// ReportProgress is the TASK-level heartbeat, distinct from worker liveness.
//
// The distinction matters operationally: a worker can be perfectly alive while one
// task is wedged. Keeping the signals separate lets us time out the TASK rather
// than declaring the worker dead and losing every other task it is running.
//
// It also carries the executor's status line (F96): the Kubernetes executor
// sends pod phase changes such as "pod Pending: Unschedulable: insufficient
// cpu", and the scheduler stores the latest one on the attempt so the console
// can show why a RUNNING attempt is not making progress. The message is
// forwarded untouched; the scheduler, which stores it, is where it is
// sanitized, so there is exactly one place that decides what a stored message
// may contain.
func (s *Server) ReportProgress(ctx context.Context, req *nexusv1.ReportProgressRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "ReportProgress"); ack != nil {
		return ack, nil
	}
	stale, err := s.sch.RecordProgress(ctx, req.GetAttemptId(), req.GetLeaseToken(), req.GetMessage(), req.GetFractionComplete())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record progress: %v", err)
	}
	if stale {
		// The lease ended between the fence above and the scheduler's write.
		// Same answer as the fence would now give, for the same reason.
		s.log.WarnContext(ctx, "rejected attempt RPC on stale lease token",
			"rpc", "ReportProgress", "attempt_id", req.GetAttemptId())
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

// ReportResult applies a terminal outcome, and is the RPC where fencing matters
// most.
//
// A partitioned worker whose lease we already expired may reconnect and report
// success for an attempt we gave up on and retried. Accepting that report would
// corrupt state: it could mark SUCCEEDED an attempt whose replacement is still
// running, and release capacity twice. The token check rejects it instead.
func (s *Server) ReportResult(ctx context.Context, req *nexusv1.ReportResultRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "ReportResult"); ack != nil {
		return ack, nil
	}
	if err := s.sch.ApplyResult(ctx, req); err != nil {
		if errors.Is(err, store.ErrStaleLeaseToken) {
			// The lease reconciler or liveness sweep reclaimed the attempt
			// after the fence above passed, and the scheduler ignored the
			// result (F104). The same answer the fence gives: the agent drops
			// the result, which is right because a retry owns the work now. A
			// duplicate of a result that did land is not stale and falls
			// through to Accepted below.
			s.log.WarnContext(ctx, "attempt result arrived after its lease was reclaimed; rejected as stale",
				"attempt_id", req.GetAttemptId())
			return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
		}
		// Never Ack{accepted:true} and never Ack{accepted:false} here: the
		// result was not applied, and both Acks make the agent drop it from its
		// WAL. A transport error keeps it there to be resent. A scheduler that
		// could not take the attempt row in time (CONTRACT-FINDINGS F104) or
		// that was unreachable answers Unavailable, which is the status that
		// says so honestly; anything else stays Internal.
		if retryableUpstream(err) {
			s.log.WarnContext(ctx, "attempt result not applied yet; the worker will resend",
				"attempt_id", req.GetAttemptId(), "err", err)
			return nil, status.Errorf(codes.Unavailable, "apply result: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "apply result: %v", err)
	}
	s.log.InfoContext(ctx, "attempt result applied",
		"attempt_id", req.GetAttemptId(), "outcome", req.GetOutcome().String())
	return &nexusv1.Ack{Accepted: true}, nil
}

// retryableUpstream reports whether an upstream failure is transient: the
// scheduler asked for a resend (Unavailable), timed out, or shed load.
func retryableUpstream(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.Aborted:
		return true
	}
	return false
}

// ReportLogs indexes log chunks the agent has already written to the result
// store (ADR-0020).
//
// Fenced first, like every attempt RPC: a stale attempt -- one whose lease was
// reclaimed and whose replacement may already be running -- cannot add a line
// to what the API serves as this attempt's log. Its uploaded objects stay
// unreferenced and artifact-gc collects them.
//
// Idempotent by (attempt_id, seq): the agent retries a report whose ack it did
// not see, and the retry is a no-op. Validation failures are protocol outcomes
// (Ack{accepted:false}) rather than transport errors, so the agent stops
// shipping instead of retrying a report that can never be accepted.
func (s *Server) ReportLogs(ctx context.Context, req *nexusv1.ReportLogsRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	if s.cfg.LogShipping == nil || !s.cfg.LogShipping.GetEnabled() {
		// Only an agent that ignored RegisterResponse gets here. Refusing keeps
		// the index from filling on a gateway that cannot serve it.
		return &nexusv1.Ack{Accepted: false, Reason: "log_shipping_disabled"}, nil
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "ReportLogs"); ack != nil {
		return ack, nil
	}
	chunks := make([]store.LogChunkRow, 0, len(req.GetChunks()))
	for _, c := range req.GetChunks() {
		chunks = append(chunks, store.LogChunkRow{
			Seq:        c.GetSeq(),
			ObjectKey:  c.GetKey(),
			FirstLine:  c.GetFirstLine(),
			LineCount:  c.GetLineCount(),
			ByteOffset: c.GetByteOffset(),
			SizeBytes:  c.GetSizeBytes(),
			SHA256:     c.GetSha256(),
			FirstTS:    unixMsPtr(c.GetFirstTsUnixMs()),
			LastTS:     unixMsPtr(c.GetLastTsUnixMs()),
		})
	}
	var final *store.LogFinalRow
	if f := req.GetFinal(); f != nil {
		final = &store.LogFinalRow{
			CompactedKey:    f.GetCompactedKey(),
			CompactedSHA256: f.GetCompactedSha256(),
			TotalLines:      f.GetTotalLines(),
			TotalBytes:      f.GetTotalBytes(),
			TruncatedLines:  f.GetTruncatedLines(),
			TruncatedBytes:  f.GetTruncatedBytes(),
			DroppedLines:    f.GetDroppedLines(),
			ChunkCount:      f.GetChunkCount(),
		}
	}
	res, err := store.GatewayAppendLogChunks(ctx, s.db, req.GetAttemptId(), chunks, final)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrLogFinalized):
		return &nexusv1.Ack{Accepted: false, Reason: store.ErrLogFinalized.Error()}, nil
	case errors.Is(err, store.ErrInvalidLogChunk):
		s.log.WarnContext(ctx, "rejected an invalid log report", "attempt_id", req.GetAttemptId(), "error", err)
		return &nexusv1.Ack{Accepted: false, Reason: store.ErrInvalidLogChunk.Error()}, nil
	case errors.Is(err, store.ErrNotFound):
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
	default:
		return nil, status.Errorf(codes.Internal, "index log chunks: %v", err)
	}
	if res.Finalized {
		s.log.DebugContext(ctx, "attempt log finalized", "attempt_id", req.GetAttemptId(),
			"inserted", res.Inserted, "duplicates", res.Duplicates)
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

func unixMsPtr(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

// fence is THE gate in front of every attempt RPC (invariant 3).
//
// It returns a non-nil Ack when the caller must be REJECTED, and nil when the
// call may proceed. Returning a rejection Ack rather than a gRPC error is required
// by the contract: `Ack{accepted:false, reason:"stale_lease_token"}` is a normal
// protocol outcome the worker handles by abandoning the attempt, not a transport
// failure it should retry.
//
// Both "no lease at all" and "token does not match" collapse to the same reason,
// because they mean the same thing to the worker: you are not the owner, stop.
func (s *Server) fence(ctx context.Context, attemptID, token, rpc string) *nexusv1.Ack {
	if attemptID == "" || token == "" {
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}
	}
	_, err := store.GatewayVerifyLease(ctx, s.db, attemptID, token)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrStaleLeaseToken), errors.Is(err, store.ErrLeaseNotFound):
		// Logged at WARN with no token material: the lease token is a credential,
		// and a log line is the easiest place to leak one.
		s.log.WarnContext(ctx, "rejected attempt RPC on stale lease token",
			"rpc", rpc, "attempt_id", attemptID)
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}
	default:
		// A database failure is NOT a stale token. Saying "stale_lease_token" here
		// would tell a healthy worker to abandon work it legitimately owns, and
		// every worker in the fleet would do so at once during a database blip.
		s.log.ErrorContext(ctx, "lease verification failed", "rpc", rpc, "attempt_id", attemptID, "error", err)
		return &nexusv1.Ack{Accepted: false, Reason: "lease_verification_unavailable"}
	}
}

// requirePrincipal enforces that the caller holds a worker-capable credential.
func (s *Server) requirePrincipal(ctx context.Context, namespace string) (authn.Principal, error) {
	p, ok := authn.FromContext(ctx)
	if !ok {
		return authn.Principal{}, status.Error(codes.Unauthenticated, "missing credentials")
	}
	if err := p.Authorize(namespace, authn.ActionWorkerRPC); err != nil {
		return authn.Principal{}, status.Error(codes.PermissionDenied, "not permitted")
	}
	return p, nil
}

// newOpaqueToken mints an unguessable bearer token.
//
// crypto/rand, 256 bits, and no structure. The token is opaque by design: nothing
// in the system parses it, so it carries no information an attacker could exploit
// and no format that could constrain a future change.
func newOpaqueToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
