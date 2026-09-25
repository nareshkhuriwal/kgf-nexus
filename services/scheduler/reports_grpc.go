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
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
)

// CONTRACT GAP (reported, not worked around): nexus.v1.SchedulerService defines
// ClaimForWorker and PendingCancellations only. It has no RPC for the other half
// of an attempt's life — ReportStarted, ReportProgress, ReportResult — even though
// ADR-0011 assigns task_attempts and leases to this service and the worker
// protocol in §8.1 requires all three.
//
// Without them the platform cannot execute anything: an attempt reaches
// DISPATCHED and no component in the system is able to move it to RUNNING or to a
// terminal state, so every run stalls after dispatch. The choices were to let the
// gateway write task_attempts directly, which is precisely the ownership
// violation ADR-0011's per-service DB roles exist to make impossible, or to serve
// an RPC the frozen proto does not describe. CONTRACT.md forbids editing the
// proto and asks for the problem to be reported instead, so this uses the same
// hand-written-descriptor-plus-JSON-codec shape services/dag-engine already
// established, and is a drop-in replacement for generated stubs once these three
// RPCs are added to services.proto.

// MarkStartedRequest confirms DISPATCHED -> RUNNING.
type MarkStartedRequest struct {
	AttemptID string `json:"attempt_id"`
}

// RecordProgressRequest is the task-level heartbeat, plus the worker's latest
// status line (e.g. "pod Pending: Unschedulable: insufficient cpu").
//
// LeaseToken is additive (F96). The gateway fences on it before calling; the
// scheduler fences on it AGAIN inside the write, because the lease can be
// reclaimed in between. A gateway that predates the field sends none, and then
// only the heartbeat is honoured, as before: without a token nothing here can
// prove the message is from the current lease holder.
type RecordProgressRequest struct {
	AttemptID  string  `json:"attempt_id"`
	LeaseToken string  `json:"lease_token,omitempty"`
	Message    string  `json:"message,omitempty"`
	Fraction   float64 `json:"fraction,omitempty"`
}

// RecordProgressResponse is additive over the Empty this RPC used to return,
// so an old gateway decodes it as Empty and an old scheduler's Empty decodes
// here as "not stale".
type RecordProgressResponse struct {
	// StaleLease: the token is not the attempt's live lease. Nothing was
	// written, and the gateway answers the worker stale_lease_token.
	StaleLease bool `json:"stale_lease,omitempty"`
	// Stored: the message and/or fraction were recorded on the attempt.
	Stored bool `json:"stored,omitempty"`
}

// ApplyResultRequest carries a terminal outcome.
//
// ResultJSON is the worker's ReportResultRequest re-encoded as protojson rather
// than being destructured into fields here. The outcome, the output refs, the
// error taxonomy and the metrics are all defined in the frozen proto, and
// restating them as hand-written Go structs would create a second definition of
// the message that every future proto change would have to be replicated into.
type ApplyResultRequest struct {
	ResultJSON json.RawMessage `json:"result_json"`
}

// ApplyResultResponse reports what the terminal transition did, so the caller can
// drive the DAG engine's reconcile without re-reading the row.
type ApplyResultResponse struct {
	RunID  string `json:"run_id"`
	NodeID string `json:"node_id"`
	State  string `json:"state"`
	// Applied is false when the report was a duplicate or arrived after the
	// attempt had already been moved. Not an error: at-least-once delivery makes
	// duplicates routine.
	Applied bool `json:"applied"`
	Retry   bool `json:"retry"`
}

// CancelRunAttemptsRequest stops every in-flight attempt of a run.
type CancelRunAttemptsRequest struct {
	RunID string `json:"run_id"`
}

// CancelRunAttemptsResponse reports how many were moved.
type CancelRunAttemptsResponse struct {
	Cancelled int `json:"cancelled"`
}

// Empty is the response for the RPCs that report only success or failure.
type Empty struct{}

type attemptReportService interface {
	MarkStarted(context.Context, *MarkStartedRequest) (*Empty, error)
	RecordProgress(context.Context, *RecordProgressRequest) (*RecordProgressResponse, error)
	ApplyResult(context.Context, *ApplyResultRequest) (*ApplyResultResponse, error)
	CancelRunAttempts(context.Context, *CancelRunAttemptsRequest) (*CancelRunAttemptsResponse, error)
}

// ReportServer implements the attempt-report half.
//
// Every method here is reached only AFTER the gateway has fenced the caller on
// its lease token (invariant 3), so these guard on attempt STATE rather than
// re-checking ownership. The one exception is CancelRunAttempts, which is called
// by workflow-manager rather than by a worker and carries no lease at all.
type ReportServer struct {
	store *store.SchedulerStore
	log   *slog.Logger
	now   func() time.Time
}

// NewReportServer builds the adapter.
func NewReportServer(st *store.SchedulerStore, log *slog.Logger) *ReportServer {
	if log == nil {
		log = slog.Default()
	}
	return &ReportServer{store: st, log: log, now: func() time.Time { return time.Now().UTC() }}
}

// MarkStarted transitions DISPATCHED -> RUNNING.
func (r *ReportServer) MarkStarted(ctx context.Context, req *MarkStartedRequest) (*Empty, error) {
	id, err := uuid.Parse(req.AttemptID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "attempt_id %q is not a uuid", req.AttemptID)
	}
	// The timestamp is the CONTROL PLANE's, never the worker's (invariant 7).
	if err := r.store.MarkStarted(ctx, id, r.now()); err != nil {
		return nil, status.Errorf(codes.Internal, "mark started: %v", err)
	}
	return &Empty{}, nil
}

// RecordProgress stores the worker's latest status for the attempt and pushes
// out the task heartbeat deadline.
//
// The message and fraction are untrusted and are sanitized and validated in the
// store (domain.SanitizeProgressMessage, NormalizeProgressFraction). A bad
// fraction is dropped and logged, never an error: failing the RPC would also
// drop the heartbeat, and a task would then be timed out for a formatting
// mistake in its progress reporting.
func (r *ReportServer) RecordProgress(ctx context.Context, req *RecordProgressRequest) (*RecordProgressResponse, error) {
	id, err := uuid.Parse(req.AttemptID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "attempt_id %q is not a uuid", req.AttemptID)
	}
	out, err := r.store.RecordProgress(ctx, store.ProgressReport{
		AttemptID: id, LeaseToken: req.LeaseToken, Message: req.Message, Fraction: req.Fraction,
	}, r.now())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record progress: %v", err)
	}
	if out.StaleLease {
		// No token material in the log line: the token is a credential.
		r.log.WarnContext(ctx, "progress rejected on stale lease", "attempt_id", req.AttemptID)
	}
	if out.FractionRejected {
		r.log.WarnContext(ctx, "progress fraction outside [0,1] ignored",
			"attempt_id", req.AttemptID, "fraction", req.Fraction)
	}
	if out.Truncated {
		r.log.DebugContext(ctx, "progress message truncated",
			"attempt_id", req.AttemptID, "bytes", len(req.Message))
	}
	return &RecordProgressResponse{StaleLease: out.StaleLease, Stored: out.Stored}, nil
}

// ApplyResult applies a terminal outcome.
func (r *ReportServer) ApplyResult(ctx context.Context, req *ApplyResultRequest) (*ApplyResultResponse, error) {
	inner := &nexusv1.ReportResultRequest{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(req.ResultJSON, inner); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "decode result: %v", err)
	}

	applied, err := r.store.ApplyResult(ctx, r.now(), inner)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "apply result: %v", err)
	}
	if !applied.Applied {
		// A duplicate or late report. Logged at debug because it is expected
		// under at-least-once delivery and would otherwise be constant noise.
		r.log.DebugContext(ctx, "attempt result ignored as already terminal",
			"attempt_id", inner.GetAttemptId())
		return &ApplyResultResponse{Applied: false}, nil
	}

	r.log.InfoContext(ctx, "attempt result applied",
		"run_id", applied.RunID, "attempt_id", inner.GetAttemptId(),
		"node_id", applied.NodeID, "state", applied.State, "retry_scheduled", applied.Retry)

	return &ApplyResultResponse{
		RunID: applied.RunID.String(), NodeID: applied.NodeID,
		State: applied.State, Applied: true, Retry: applied.Retry,
	}, nil
}

// CancelRunAttempts stops every in-flight attempt of a run.
func (r *ReportServer) CancelRunAttempts(ctx context.Context, req *CancelRunAttemptsRequest) (*CancelRunAttemptsResponse, error) {
	runID, err := uuid.Parse(req.RunID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "run_id %q is not a uuid", req.RunID)
	}
	n, err := r.store.CancelRunAttempts(ctx, runID, r.now())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cancel run attempts: %v", err)
	}
	return &CancelRunAttemptsResponse{Cancelled: n}, nil
}

// AttemptReportServiceDesc is the hand-written descriptor.
var AttemptReportServiceDesc = grpc.ServiceDesc{
	ServiceName: "nexus.v1.AttemptReportService",
	HandlerType: (*attemptReportService)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "MarkStarted", Handler: markStartedHandler},
		{MethodName: "RecordProgress", Handler: recordProgressHandler},
		{MethodName: "ApplyResult", Handler: applyResultHandler},
		{MethodName: "CancelRunAttempts", Handler: cancelRunAttemptsHandler},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "services/scheduler/reports_grpc.go",
}

func markStartedHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(MarkStartedRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(attemptReportService).MarkStarted(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/MarkStarted"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(attemptReportService).MarkStarted(ctx, req.(*MarkStartedRequest))
		})
}

func recordProgressHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(RecordProgressRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(attemptReportService).RecordProgress(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/RecordProgress"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(attemptReportService).RecordProgress(ctx, req.(*RecordProgressRequest))
		})
}

func applyResultHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(ApplyResultRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(attemptReportService).ApplyResult(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/ApplyResult"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(attemptReportService).ApplyResult(ctx, req.(*ApplyResultRequest))
		})
}

func cancelRunAttemptsHandler(srv any, ctx context.Context, dec func(any) error, ic grpc.UnaryServerInterceptor) (any, error) {
	in := new(CancelRunAttemptsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if ic == nil {
		return srv.(attemptReportService).CancelRunAttempts(ctx, in)
	}
	return ic(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/nexus.v1.AttemptReportService/CancelRunAttempts"},
		func(ctx context.Context, req any) (any, error) {
			return srv.(attemptReportService).CancelRunAttempts(ctx, req.(*CancelRunAttemptsRequest))
		})
}

// schedJSONCodec is the server-side codec for the hand-written half.
//
// Registering it process-globally is safe HERE and would not be in a client:
// this process serves exactly one JSON service and dials none, so there is no
// protobuf connection for a global "json" codec to interfere with.
type schedJSONCodec struct{}

func (schedJSONCodec) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal grpc message: %v", err)
	}
	return b, nil
}

func (schedJSONCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return status.Errorf(codes.InvalidArgument, "unmarshal grpc message: %v", err)
	}
	return nil
}

func (schedJSONCodec) Name() string { return "json" }

// RegisterReports wires the attempt-report half onto a gRPC server.
func RegisterReports(srv *grpc.Server, impl *ReportServer) {
	encoding.RegisterCodec(schedJSONCodec{})
	srv.RegisterService(&AttemptReportServiceDesc, impl)
}
