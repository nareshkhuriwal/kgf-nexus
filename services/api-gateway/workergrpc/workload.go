// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package workergrpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/upstream"
)

// The workload API (ADR-0032). The agent serves each attempt a local socket and
// forwards what the task asks for through these RPCs, adding the lease token.
//
// Every one is fenced here on the lease token first (invariant 3), exactly like
// the other attempt RPCs, and the scheduler fences AGAIN inside its write for
// the ones that store data. A capability the gateway was not configured to
// offer answers Unimplemented -- the same answer a gateway that predates the
// RPC gives -- so the agent has one code path for "not available here".

// WorkloadScheduler is the scheduler's workload-API surface. Optional: a
// Scheduler that does not implement it (an older build, a test fake) makes
// every workload RPC answer Unimplemented.
type WorkloadScheduler interface {
	RecordUsage(ctx context.Context, attemptID, leaseToken string, units map[string]int64) (stale bool, err error)
	AcquirePermits(ctx context.Context, call upstream.PermitCall) (upstream.PermitResult, error)
	SaveCheckpoint(ctx context.Context, call upstream.CheckpointCall) (upstream.CheckpointResult, error)
}

// DefaultMaxCheckpointBytes is advertised when the configuration names none.
const DefaultMaxCheckpointBytes = 64 << 20

// DefaultWorkloadAPI is the full workload API.
func DefaultWorkloadAPI() *nexusv1.WorkloadApiConfig {
	return &nexusv1.WorkloadApiConfig{
		UsageReporting: true, RateLimits: true, Checkpoints: true,
		MaxCheckpointBytes: DefaultMaxCheckpointBytes,
	}
}

// advertisedWorkloadAPI is what Register tells the agent: the configured
// capabilities, or nothing when the scheduler client cannot serve them.
func (s *Server) advertisedWorkloadAPI() *nexusv1.WorkloadApiConfig {
	if _, ok := s.sch.(WorkloadScheduler); !ok || s.cfg.WorkloadAPI == nil {
		return nil
	}
	return s.cfg.WorkloadAPI
}

func (s *Server) workload(capability func(*nexusv1.WorkloadApiConfig) bool) (WorkloadScheduler, error) {
	ws, ok := s.sch.(WorkloadScheduler)
	if !ok || s.cfg.WorkloadAPI == nil || !capability(s.cfg.WorkloadAPI) {
		return nil, status.Error(codes.Unimplemented, "the workload API capability is not offered by this control plane")
	}
	return ws, nil
}

// upstreamWorkloadError maps a scheduler failure onto the worker-facing status:
// InvalidArgument passes through (resending cannot help), transient failures
// become Unavailable (the agent retries), anything else Internal.
func upstreamWorkloadError(op string, err error) error {
	// status.FromError finds a status wrapped by the upstream client's %w.
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument:
			return status.Errorf(codes.InvalidArgument, "%s: %s", op, st.Message())
		case codes.Unimplemented:
			return status.Errorf(codes.Unimplemented, "%s: the scheduler does not serve it yet", op)
		}
	}
	if retryableUpstream(err) {
		return status.Errorf(codes.Unavailable, "%s: %v", op, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", op, err)
}

// ReportUsage stores the attempt's cumulative usage units.
func (s *Server) ReportUsage(ctx context.Context, req *nexusv1.ReportUsageRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	ws, err := s.workload(func(c *nexusv1.WorkloadApiConfig) bool { return c.GetUsageReporting() })
	if err != nil {
		return nil, err
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "ReportUsage"); ack != nil {
		return ack, nil
	}
	stale, err := ws.RecordUsage(ctx, req.GetAttemptId(), req.GetLeaseToken(), req.GetUnits())
	if err != nil {
		st := upstreamWorkloadError("record usage", err)
		if status.Code(st) == codes.InvalidArgument {
			// A protocol outcome: the agent stops sending a report that can
			// never be accepted instead of retrying it.
			s.log.WarnContext(ctx, "usage report refused", "attempt_id", req.GetAttemptId(), "error", err)
			return &nexusv1.Ack{Accepted: false, Reason: "invalid_usage"}, nil
		}
		return nil, st
	}
	if stale {
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

// AcquirePermits takes or settles shared rate-limit permits.
func (s *Server) AcquirePermits(ctx context.Context, req *nexusv1.AcquirePermitsRequest) (*nexusv1.AcquirePermitsResponse, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	ws, err := s.workload(func(c *nexusv1.WorkloadApiConfig) bool { return c.GetRateLimits() })
	if err != nil {
		return nil, err
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "AcquirePermits"); ack != nil {
		return &nexusv1.AcquirePermitsResponse{Accepted: false, Reason: ack.GetReason()}, nil
	}
	res, err := ws.AcquirePermits(ctx, upstream.PermitCall{
		AttemptID: req.GetAttemptId(), LeaseToken: req.GetLeaseToken(), Keys: req.GetKeys(),
		Requests: req.GetRequests(), Tokens: req.GetTokens(), Settle: req.GetSettle(),
	})
	if err != nil {
		return nil, upstreamWorkloadError("acquire permits", err)
	}
	if res.StaleLease {
		return &nexusv1.AcquirePermitsResponse{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
	}
	return &nexusv1.AcquirePermitsResponse{
		Accepted: true, Granted: res.Granted, RetryAfterMs: res.RetryAfterMs, UnlimitedKeys: res.UnlimitedKeys,
	}, nil
}

// SaveCheckpoint registers a checkpoint the agent has already written.
func (s *Server) SaveCheckpoint(ctx context.Context, req *nexusv1.SaveCheckpointRequest) (*nexusv1.Ack, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	ws, err := s.workload(func(c *nexusv1.WorkloadApiConfig) bool { return c.GetCheckpoints() })
	if err != nil {
		return nil, err
	}
	if max := s.cfg.WorkloadAPI.GetMaxCheckpointBytes(); max > 0 && req.GetSizeBytes() > max {
		return &nexusv1.Ack{Accepted: false, Reason: "invalid_checkpoint"}, nil
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "SaveCheckpoint"); ack != nil {
		return ack, nil
	}
	res, err := ws.SaveCheckpoint(ctx, upstream.CheckpointCall{
		AttemptID: req.GetAttemptId(), LeaseToken: req.GetLeaseToken(), Seq: req.GetSeq(),
		Key: req.GetKey(), SHA256: req.GetSha256(), SizeBytes: req.GetSizeBytes(),
	})
	if err != nil {
		st := upstreamWorkloadError("save checkpoint", err)
		if status.Code(st) == codes.InvalidArgument {
			s.log.WarnContext(ctx, "checkpoint refused", "attempt_id", req.GetAttemptId(), "error", err)
			return &nexusv1.Ack{Accepted: false, Reason: "invalid_checkpoint"}, nil
		}
		return nil, st
	}
	switch {
	case res.StaleLease:
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleLeaseToken}, nil
	case res.Superseded:
		return &nexusv1.Ack{Accepted: false, Reason: store.ReasonStaleCheckpoint}, nil
	}
	return &nexusv1.Ack{Accepted: true}, nil
}

// GetCheckpoint returns the latest checkpoint for the attempt's idempotency
// key. A read, served from the gateway's own connection after the fence.
func (s *Server) GetCheckpoint(ctx context.Context, req *nexusv1.GetCheckpointRequest) (*nexusv1.GetCheckpointResponse, error) {
	if _, err := s.requirePrincipal(ctx, ""); err != nil {
		return nil, err
	}
	if _, err := s.workload(func(c *nexusv1.WorkloadApiConfig) bool { return c.GetCheckpoints() }); err != nil {
		return nil, err
	}
	if ack := s.fence(ctx, req.GetAttemptId(), req.GetLeaseToken(), "GetCheckpoint"); ack != nil {
		return &nexusv1.GetCheckpointResponse{Accepted: false, Reason: ack.GetReason()}, nil
	}
	c, err := store.GatewayGetCheckpoint(ctx, s.db, req.GetAttemptId())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &nexusv1.GetCheckpointResponse{Accepted: true, Found: false}, nil
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "get checkpoint: %v", err)
	}
	return &nexusv1.GetCheckpointResponse{
		Accepted: true, Found: true, Key: c.Key, Sha256: c.SHA256, SizeBytes: c.SizeBytes,
		Seq: c.Seq, AttemptNumber: c.AttemptNumber, SavedAtUnixMs: c.SavedAt.UnixMilli(),
		Bucket: s.cfg.ResultStore.GetBucket(),
	}, nil
}
