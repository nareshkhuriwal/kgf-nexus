// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package grpcapi

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/khuriwalgroup/kgf-nexus/services/workflow-manager/runs"
)

// CreateTriggeredRunRequest creates a run on behalf of an external trigger
// (ADR-0028). Unlike the protobuf CreateRun it carries typed params, a trigger
// detail and a pinned spec version, which is why it is a JSON RPC here.
type CreateTriggeredRunRequest struct {
	Namespace      string         `json:"namespace"`
	SpecName       string         `json:"spec_name"`
	SpecVersion    *int32         `json:"spec_version"`
	IdempotencyKey string         `json:"idempotency_key"`
	TriggerKind    string         `json:"trigger_kind"`
	TriggerDetail  map[string]any `json:"trigger_detail"`
	Params         map[string]any `json:"params"`
	Priority       int32          `json:"priority"`
}

// CreateTriggeredRunResponse reports the run and whether it is new.
type CreateTriggeredRunResponse struct {
	RunID   string `json:"run_id"`
	Created bool   `json:"created"`
}

// allowedTriggerKinds are the kinds this RPC may record. Manual, schedule and
// backfill runs have their own paths; letting a caller claim them here would
// let a webhook masquerade as a schedule in the run history.
var allowedTriggerKinds = map[string]bool{"webhook": true, "sensor": true}

// CreateTriggeredRun creates (or deduplicates) a triggered run.
func (s *SpecServer) CreateTriggeredRun(ctx context.Context, req *CreateTriggeredRunRequest) (*CreateTriggeredRunResponse, error) {
	if req.Namespace == "" || req.SpecName == "" {
		return nil, status.Error(codes.InvalidArgument, "namespace and spec_name are required")
	}
	if !allowedTriggerKinds[req.TriggerKind] {
		return nil, status.Errorf(codes.InvalidArgument, "trigger_kind %q is not a triggered-run kind", req.TriggerKind)
	}
	if !strings.HasPrefix(req.IdempotencyKey, req.TriggerKind+":") {
		// Namespaced keys keep a trigger from colliding with (or hijacking) a
		// run created under someone else's idempotency key.
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key must start with %q", req.TriggerKind+":")
	}
	res, err := s.runs.Create(ctx, runs.CreateRequest{
		Namespace: req.Namespace, SpecName: req.SpecName, SpecVersion: req.SpecVersion,
		IdempotencyKey: req.IdempotencyKey, TriggerKind: req.TriggerKind, TriggerDetail: req.TriggerDetail,
		Params: req.Params, Priority: req.Priority,
	})
	if err != nil {
		return nil, runError("create triggered run", err)
	}
	s.log.InfoContext(ctx, "triggered run", "namespace", req.Namespace, "workflow", req.SpecName,
		"trigger", req.TriggerKind, "run_id", res.Run.ID, "created", res.Created)
	return &CreateTriggeredRunResponse{RunID: res.Run.ID, Created: res.Created}, nil
}

func createTriggeredRunHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(CreateTriggeredRunRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(specService).CreateTriggeredRun(ctx, in)
	}
	return interceptor(ctx, in, &grpc.UnaryServerInfo{
		Server: srv, FullMethod: "/nexus.v1.WorkflowSpecService/CreateTriggeredRun",
	}, func(ctx context.Context, req any) (any, error) {
		return srv.(specService).CreateTriggeredRun(ctx, req.(*CreateTriggeredRunRequest))
	})
}
