// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/rest"
)

type wfmCreateTriggeredRunRequest struct {
	Namespace      string         `json:"namespace"`
	SpecName       string         `json:"spec_name"`
	SpecVersion    *int32         `json:"spec_version"`
	IdempotencyKey string         `json:"idempotency_key"`
	TriggerKind    string         `json:"trigger_kind"`
	TriggerDetail  map[string]any `json:"trigger_detail"`
	Params         map[string]any `json:"params"`
	Priority       int32          `json:"priority"`
}

type wfmCreateTriggeredRunResponse struct {
	RunID   string `json:"run_id"`
	Created bool   `json:"created"`
}

// CreateTriggeredRun creates a webhook-triggered run through its owner
// (ADR-0028). The gRPC status is returned unwrapped-compatible so the gateway
// can map NotFound / InvalidArgument / ResourceExhausted onto HTTP.
func (c *WorkflowManagerClient) CreateTriggeredRun(ctx context.Context, req rest.TriggeredRunRequest) (string, bool, error) {
	if c.grpc == nil {
		return "", false, fmt.Errorf("create triggered run: %w (workflow-manager owns runs)", ErrUpstreamNotConfigured)
	}
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	var resp wfmCreateTriggeredRunResponse
	if err := c.grpc.conn.Invoke(ctx, "/nexus.v1.WorkflowSpecService/CreateTriggeredRun",
		&wfmCreateTriggeredRunRequest{
			Namespace: req.Namespace, SpecName: req.SpecName, SpecVersion: req.SpecVersion,
			IdempotencyKey: req.IdempotencyKey, TriggerKind: req.TriggerKind, TriggerDetail: req.TriggerDetail,
			Params: req.Params, Priority: req.Priority,
		}, &resp, grpc.ForceCodec(wfmJSONCodec{})); err != nil {
		return "", false, err
	}
	return resp.RunID, resp.Created, nil
}
