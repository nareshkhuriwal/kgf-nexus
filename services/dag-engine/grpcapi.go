// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otel/attribute"

	"github.com/khuriwalgroup/kgf-nexus/internal/tracing"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/dag"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"google.golang.org/protobuf/encoding/protojson"
)

// ── gRPC surface ──────────────────────────────────────────────────────────────
//
// The frozen proto/ defines only WorkerService (CONTRACT.md forbids editing
// proto/ or gen/), so there is no generated stub for a DagEngineService. Rather
// than add an unapproved .proto -- which would be a silent divergence of exactly
// the kind CONTRACT.md warns about -- the service registers hand-written handlers
// through grpc.ServiceDesc with JSON codecs on the wire.
//
// The method set and names below are what a generated DagEngineService would
// expose, so adopting a real .proto later is a drop-in replacement for this file
// and nothing else. The gap is reported in the handover notes.

// MaterializeRunRequest asks the engine to compile a run's DAG.
type MaterializeRunRequest struct {
	RunID string `json:"run_id"`
}

// MaterializeRunResponse reports what was materialized.
type MaterializeRunResponse struct {
	RunID          string   `json:"run_id"`
	NodesInserted  int64    `json:"nodes_inserted"`
	EdgesInserted  int64    `json:"edges_inserted"`
	Frontier       []string `json:"frontier"`
	AttemptIDs     []string `json:"attempt_ids"`
	AlreadyExisted bool     `json:"already_existed"`
}

// ReconcileRunRequest asks the engine to recompute a run.
type ReconcileRunRequest struct {
	RunID string `json:"run_id"`
	// NodeID is advisory: the node whose attempt just went terminal. The engine
	// recomputes the whole run regardless, because a correct reconcile must not
	// depend on being told the right place to look -- a lost notification would
	// otherwise strand the run.
	NodeID string `json:"node_id,omitempty"`
}

// ReconcileRunResponse reports one reconcile pass.
type ReconcileRunResponse struct {
	RunID          string   `json:"run_id"`
	Frontier       []string `json:"frontier"`
	AttemptIDs     []string `json:"attempt_ids"`
	RunState       string   `json:"run_state"`
	RunTerminal    bool     `json:"run_terminal"`
	Halted         bool     `json:"halted"`
	Skipped        []string `json:"skipped,omitempty"`
	UpstreamFailed []string `json:"upstream_failed,omitempty"`
}

// ResetNodesRequest applies an operator retry to a run's graph.
type ResetNodesRequest struct {
	RunID string `json:"run_id"`
	// NodeIDs is the selection. Empty retries the whole run from every node
	// that did not succeed.
	NodeIDs []string `json:"node_ids,omitempty"`
	// Actor attributes the per-node history entries.
	Actor string `json:"actor,omitempty"`
}

// ResetNodesResponse reports what the retry reset.
type ResetNodesResponse struct {
	RunID    string   `json:"run_id"`
	Selected []string `json:"selected"`
	Reset    []string `json:"reset_nodes"`
}

// ValidateSpecRequest carries a spec as protojson, the same encoding used in
// workflow_specs.spec_json, so a caller can validate exactly the bytes it intends
// to store.
type ValidateSpecRequest struct {
	SpecJSON json.RawMessage `json:"spec_json"`
}

// ValidateSpecResponse returns EVERY problem, not the first. See
// dag.ValidationErrors for why.
type ValidateSpecResponse struct {
	Valid  bool                  `json:"valid"`
	Errors []ValidationErrorJSON `json:"errors,omitempty"`
}

// ValidationErrorJSON is one machine-readable validation failure.
type ValidationErrorJSON struct {
	Code   string   `json:"code"`
	NodeID string   `json:"node_id,omitempty"`
	Field  string   `json:"field,omitempty"`
	Msg    string   `json:"message"`
	Path   []string `json:"path,omitempty"`
}

// InvalidateCacheRequest records a memo invalidation (§6.4). The gateway has
// already authenticated and authorized the caller; Actor is its attribution.
type InvalidateCacheRequest struct {
	Namespace   string `json:"namespace"`
	TaskRef     string `json:"task_ref,omitempty"`
	TaskVersion *int32 `json:"task_version,omitempty"`
	CacheKey    string `json:"cache_key,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Actor       string `json:"actor"`
}

// InvalidateCacheResponse identifies the recorded cutoff.
type InvalidateCacheResponse struct {
	InvalidationID int64     `json:"invalidation_id"`
	InvalidatedAt  time.Time `json:"invalidated_at"`
}

// GRPCServer adapts Engine to the gRPC surface.
type GRPCServer struct {
	engine *Engine
	log    *slog.Logger
}

// NewGRPCServer wires the adapter.
func NewGRPCServer(e *Engine, log *slog.Logger) *GRPCServer {
	if log == nil {
		log = slog.Default()
	}
	return &GRPCServer{engine: e, log: log}
}

// MaterializeRun compiles a run's DAG and creates its initial frontier.
func (s *GRPCServer) MaterializeRun(ctx context.Context, req *MaterializeRunRequest) (*MaterializeRunResponse, error) {
	if req.GetRunID() == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	ctx, span := tracing.Start(ctx, "run.materialize", trace.WithAttributes(tracing.AttrRunID.String(req.RunID)))
	defer span.End()
	res, err := s.engine.Materialize(ctx, req.RunID)
	if err != nil {
		tracing.End(span, err)
		return nil, toStatus(err)
	}
	span.SetAttributes(attribute.Int64("nexus.nodes", res.NodesInserted),
		attribute.Int("nexus.attempts_created", len(res.AttemptsCreated)))
	out := &MaterializeRunResponse{
		RunID:          res.RunID,
		NodesInserted:  res.NodesInserted,
		EdgesInserted:  res.EdgesInserted,
		Frontier:       res.Frontier,
		AlreadyExisted: res.AlreadyExisted,
	}
	for _, a := range res.AttemptsCreated {
		out.AttemptIDs = append(out.AttemptIDs, a.AttemptID)
	}
	return out, nil
}

// ReconcileRun recomputes a run after an attempt reached a terminal state.
//
// It is the FAST path of run advancement, not the durable one (F160): the
// request it answers is already queued in run_reconcile_requests, written with
// the fact that needed it, so a call that times out or never arrives delays
// the run by one consumer poll instead of stranding it. Concurrent calls for
// one run are coalesced (reconcileFlights); the answer always comes from a
// pass that began after the call arrived.
func (s *GRPCServer) ReconcileRun(ctx context.Context, req *ReconcileRunRequest) (*ReconcileRunResponse, error) {
	if req.GetRunID() == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	ctx, span := tracing.Start(ctx, "run.reconcile", trace.WithAttributes(tracing.AttrRunID.String(req.RunID)))
	defer span.End()
	res, err := s.engine.ReconcileDurably(ctx, req.RunID)
	if err != nil {
		tracing.End(span, err)
		return nil, toStatus(err)
	}
	span.SetAttributes(attribute.String("nexus.run_state", string(res.RunState)),
		attribute.Bool("nexus.run_terminal", res.RunTerminal),
		attribute.Int("nexus.attempts_created", len(res.AttemptsCreated)))
	out := &ReconcileRunResponse{
		RunID:       res.RunID,
		Frontier:    res.Frontier,
		RunState:    string(res.RunState),
		RunTerminal: res.RunTerminal,
		Halted:      res.Halted,
	}
	for _, a := range res.AttemptsCreated {
		out.AttemptIDs = append(out.AttemptIDs, a.AttemptID)
	}
	// Surfacing these separately is the point of keeping the states distinct:
	// a caller can show "what actually broke" without walking the graph.
	for _, t := range res.Transitions {
		switch t.To {
		case dag.NodeSkipped:
			out.Skipped = append(out.Skipped, t.NodeID)
		case dag.NodeUpstreamFailed:
			out.UpstreamFailed = append(out.UpstreamFailed, t.NodeID)
		}
	}
	return out, nil
}

// ResetNodes resets a failed run's selected nodes and their dependents for an
// operator retry. It does not reconcile; see Engine.ResetNodes.
func (s *GRPCServer) ResetNodes(ctx context.Context, req *ResetNodesRequest) (*ResetNodesResponse, error) {
	if req.GetRunID() == "" {
		return nil, status.Error(codes.InvalidArgument, "run_id is required")
	}
	res, err := s.engine.ResetNodes(ctx, req.RunID, req.NodeIDs, req.Actor)
	if err != nil {
		return nil, toStatus(err)
	}
	return &ResetNodesResponse{RunID: res.RunID, Selected: res.Selected, Reset: res.Reset}, nil
}

// ValidateSpec runs submit-time validation without touching the database, so the
// gateway can reject a bad spec before creating anything.
func (s *GRPCServer) ValidateSpec(ctx context.Context, req *ValidateSpecRequest) (*ValidateSpecResponse, error) {
	if len(req.GetSpecJSON()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "spec_json is required")
	}
	var spec nexusv1.WorkflowSpec
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(req.SpecJSON, &spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec_json is not a valid WorkflowSpec: %v", err)
	}
	_, errs := dag.Validate(&spec, s.engine.validation)
	out := &ValidateSpecResponse{Valid: len(errs) == 0}
	for _, e := range errs {
		out.Errors = append(out.Errors, ValidationErrorJSON{
			Code: string(e.Code), NodeID: e.NodeID, Field: e.Field, Msg: e.Msg, Path: e.Path,
		})
	}
	return out, nil
}

// InvalidateCache records an operator's memo invalidation.
func (s *GRPCServer) InvalidateCache(ctx context.Context, req *InvalidateCacheRequest) (*InvalidateCacheResponse, error) {
	if req == nil || req.Namespace == "" || req.Actor == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace and actor are required")
	}
	if req.TaskRef == "" && req.CacheKey == "" {
		return nil, status.Error(codes.InvalidArgument, "task_ref or cache_key is required")
	}
	if req.TaskVersion != nil && req.TaskRef == "" {
		return nil, status.Error(codes.InvalidArgument, "task_version needs a task_ref")
	}
	id, at, err := s.engine.InvalidateCache(ctx, store.MemoInvalidation{
		Namespace: req.Namespace, TaskRef: req.TaskRef, TaskVersion: req.TaskVersion,
		CacheKey: req.CacheKey, Reason: req.Reason, Actor: req.Actor,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &InvalidateCacheResponse{InvalidationID: id, InvalidatedAt: at}, nil
}

// GetRunID accessors keep nil requests from panicking in the handler, which a
// malformed client frame can otherwise cause.
func (r *MaterializeRunRequest) GetRunID() string {
	if r == nil {
		return ""
	}
	return r.RunID
}

func (r *ReconcileRunRequest) GetRunID() string {
	if r == nil {
		return ""
	}
	return r.RunID
}

func (r *ResetNodesRequest) GetRunID() string {
	if r == nil {
		return ""
	}
	return r.RunID
}

func (r *ValidateSpecRequest) GetSpecJSON() json.RawMessage {
	if r == nil {
		return nil
	}
	return r.SpecJSON
}

// toStatus maps engine errors onto gRPC codes. Distinguishing NOT_FOUND and
// INVALID_ARGUMENT from INTERNAL matters operationally: the first two are the
// caller's problem and must not page anyone, while INTERNAL should.
func toStatus(err error) error {
	switch {
	case err == nil:
		return nil
	case errorsIs(err, context.DeadlineExceeded):
		// A coalesced caller that stopped waiting; the pass it waited for
		// carries on, and the durable queue covers the run either way.
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errorsIs(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errorsIs(err, store.ErrDAGEngineRunNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errorsIs(err, ErrNotMaterialized):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errorsIs(err, ErrRunNotRetryable):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errorsIs(err, store.ErrNodeRetryConflict):
		// ABORTED, not FAILED_PRECONDITION: the request was sound and a
		// concurrent writer won the race. Re-reading and asking again may
		// succeed; the same request against the same state never would.
		return status.Error(codes.Aborted, err.Error())
	default:
		// A refused retry selection: unknown node ids are a malformed request;
		// nodes in the wrong state are a precondition of the run as it is now.
		if refused, badRequest := isRetryRefusal(err); refused {
			if badRequest {
				return status.Error(codes.InvalidArgument, err.Error())
			}
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		if isValidationError(err) {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
}

// DagEngineServiceDesc is the hand-written service descriptor. See the note at
// the top of this file for why it is hand-written rather than generated.
var DagEngineServiceDesc = grpc.ServiceDesc{
	ServiceName: "nexus.v1.DagEngineService",
	HandlerType: (*dagEngineService)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "MaterializeRun",
			Handler:    materializeRunHandler,
		},
		{
			MethodName: "ReconcileRun",
			Handler:    reconcileRunHandler,
		},
		{
			MethodName: "ValidateSpec",
			Handler:    validateSpecHandler,
		},
		{
			MethodName: "InvalidateCache",
			Handler:    invalidateCacheHandler,
		},
		{
			MethodName: "ResetNodes",
			Handler:    resetNodesHandler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "services/dag-engine/grpcapi.go",
}

type dagEngineService interface {
	MaterializeRun(context.Context, *MaterializeRunRequest) (*MaterializeRunResponse, error)
	ReconcileRun(context.Context, *ReconcileRunRequest) (*ReconcileRunResponse, error)
	ValidateSpec(context.Context, *ValidateSpecRequest) (*ValidateSpecResponse, error)
	InvalidateCache(context.Context, *InvalidateCacheRequest) (*InvalidateCacheResponse, error)
	ResetNodes(context.Context, *ResetNodesRequest) (*ResetNodesResponse, error)
}

func resetNodesHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ResetNodesRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(dagEngineService).ResetNodes(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.DagEngineService/ResetNodes",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(dagEngineService).ResetNodes(ctx, req.(*ResetNodesRequest))
	})
}

func invalidateCacheHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(InvalidateCacheRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(dagEngineService).InvalidateCache(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.DagEngineService/InvalidateCache",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(dagEngineService).InvalidateCache(ctx, req.(*InvalidateCacheRequest))
	})
}

func materializeRunHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(MaterializeRunRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(dagEngineService).MaterializeRun(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.DagEngineService/MaterializeRun",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(dagEngineService).MaterializeRun(ctx, req.(*MaterializeRunRequest))
	})
}

func reconcileRunHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ReconcileRunRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(dagEngineService).ReconcileRun(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.DagEngineService/ReconcileRun",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(dagEngineService).ReconcileRun(ctx, req.(*ReconcileRunRequest))
	})
}

func validateSpecHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(ValidateSpecRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(dagEngineService).ValidateSpec(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.DagEngineService/ValidateSpec",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(dagEngineService).ValidateSpec(ctx, req.(*ValidateSpecRequest))
	})
}

// RegisterDagEngineService registers the hand-written descriptor.
func RegisterDagEngineService(s grpc.ServiceRegistrar, impl *GRPCServer) {
	s.RegisterService(&DagEngineServiceDesc, impl)
}

// jsonCodec carries the hand-written request/response types on the wire. A
// generated .proto would replace both this and the descriptor above.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal grpc message: %w", err)
	}
	return b, nil
}

func (jsonCodec) Unmarshal(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("unmarshal grpc message: %w", err)
	}
	return nil
}

func (jsonCodec) Name() string { return "json" }
