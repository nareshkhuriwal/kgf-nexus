// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
)

// The workload API's upstream half (ADR-0032): the scheduler owns attempt_usage,
// rate_limit_buckets and task_checkpoints, so every write goes there. These
// mirror services/scheduler/workload_grpc.go; every added field is optional on
// the wire, so an older scheduler decodes what it knows. A scheduler that
// predates the methods answers Unimplemented, which the gateway passes on.

type schedRecordUsageRequest struct {
	AttemptID  string           `json:"attempt_id"`
	LeaseToken string           `json:"lease_token"`
	Units      map[string]int64 `json:"units"`
}

type schedRecordUsageResponse struct {
	StaleLease bool `json:"stale_lease,omitempty"`
}

// PermitCall is one AcquirePermits forwarded to the scheduler.
type PermitCall struct {
	AttemptID  string   `json:"attempt_id"`
	LeaseToken string   `json:"lease_token"`
	Keys       []string `json:"keys"`
	Requests   int64    `json:"requests,omitempty"`
	Tokens     int64    `json:"tokens,omitempty"`
	Settle     bool     `json:"settle,omitempty"`
}

// PermitResult is the scheduler's answer.
type PermitResult struct {
	StaleLease    bool     `json:"stale_lease,omitempty"`
	Granted       bool     `json:"granted"`
	RetryAfterMs  int64    `json:"retry_after_ms,omitempty"`
	UnlimitedKeys []string `json:"unlimited_keys,omitempty"`
}

// CheckpointCall registers an already-written checkpoint object.
type CheckpointCall struct {
	AttemptID  string `json:"attempt_id"`
	LeaseToken string `json:"lease_token"`
	Seq        int64  `json:"seq"`
	Key        string `json:"key"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
}

// CheckpointResult is the scheduler's answer.
type CheckpointResult struct {
	StaleLease bool `json:"stale_lease,omitempty"`
	Superseded bool `json:"superseded,omitempty"`
}

func (s *schedGRPC) invokeJSON(ctx context.Context, method string, in, out any) error {
	ctx, cancel := withCallTimeout(ctx)
	defer cancel()
	return s.conn.Invoke(ctx, "/nexus.v1.AttemptReportService/"+method, in, out,
		grpc.ForceCodec(schedJSONCodec{}))
}

// RecordUsage forwards the attempt's cumulative usage units. stale is true when
// the scheduler's own lease fence refused them.
func (c *SchedulerClient) RecordUsage(ctx context.Context, attemptID, leaseToken string, units map[string]int64) (bool, error) {
	if c.grpc == nil {
		return false, fmt.Errorf("record usage: %w (scheduler owns attempt_usage)", ErrUpstreamNotConfigured)
	}
	var resp schedRecordUsageResponse
	if err := c.grpc.invokeJSON(ctx, "RecordUsage",
		&schedRecordUsageRequest{AttemptID: attemptID, LeaseToken: leaseToken, Units: units}, &resp); err != nil {
		return false, fmt.Errorf("record usage for attempt %s: %w", attemptID, err)
	}
	return resp.StaleLease, nil
}

// AcquirePermits forwards a permit acquisition or settlement.
func (c *SchedulerClient) AcquirePermits(ctx context.Context, call PermitCall) (PermitResult, error) {
	if c.grpc == nil {
		return PermitResult{}, fmt.Errorf("acquire permits: %w (scheduler owns rate_limit_buckets)", ErrUpstreamNotConfigured)
	}
	var resp PermitResult
	if err := c.grpc.invokeJSON(ctx, "AcquirePermits", &call, &resp); err != nil {
		return PermitResult{}, fmt.Errorf("acquire permits for attempt %s: %w", call.AttemptID, err)
	}
	return resp, nil
}

// SaveCheckpoint forwards a checkpoint registration.
func (c *SchedulerClient) SaveCheckpoint(ctx context.Context, call CheckpointCall) (CheckpointResult, error) {
	if c.grpc == nil {
		return CheckpointResult{}, fmt.Errorf("save checkpoint: %w (scheduler owns task_checkpoints)", ErrUpstreamNotConfigured)
	}
	var resp CheckpointResult
	if err := c.grpc.invokeJSON(ctx, "SaveCheckpoint", &call, &resp); err != nil {
		return CheckpointResult{}, fmt.Errorf("save checkpoint for attempt %s: %w", call.AttemptID, err)
	}
	return resp, nil
}
